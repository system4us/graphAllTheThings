package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"graphallthethings/internal/graph"
)

// DocContextPack answers a natural-language question over a document graph
// (PDF, DOCX, Markdown, TXT) using hybrid retrieval (Qdrant/local vectors +
// SQLite FTS5 + RRF via Engine.Find) and graph-aware neighbor expansion (NEXT).
func (e *Engine) DocContextPack(ctx context.Context, question string, limit int) (string, error) {
	return e.DocContextPackWithBudget(ctx, question, limit, 0)
}

// DocContextPackWithBudget is DocContextPack with an optional approximate
// token budget (0 = default unbudgeted pack).
func (e *Engine) DocContextPackWithBudget(ctx context.Context, question string, limit, maxTokens int) (string, error) {
	if limit <= 0 {
		limit = 6
	}
	fetchLimit := limit * 3
	if fetchLimit < 15 {
		fetchLimit = 15
	}

	docsCount := len(e.G.NodesByType(graph.NodeDocument))
	chunksCount := len(e.G.NodesByType(graph.NodeChunk))
	if docsCount == 0 && chunksCount == 0 {
		return e.codebaseDocsFallback(ctx, question, limit, maxTokens)
	}

	res, err := e.Find(ctx, question, graph.NodeChunk, fetchLimit)
	if err != nil {
		return "", err
	}
	// Fall back to sections or documents if no chunks matched.
	if len(res.Hits) == 0 {
		res, err = e.Find(ctx, question, graph.NodeSection, limit)
		if err != nil {
			return "", err
		}
	}
	if len(res.Hits) == 0 {
		res, err = e.Find(ctx, question, graph.NodeDocument, limit)
		if err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Document Context: %q\n", question)
	if f := e.G.Freshness(time.Now()); f != "" {
		fmt.Fprintf(&b, "Source: %s\n", f)
	}
	method := res.Method
	if method == "" {
		method = "keyword"
	}
	fmt.Fprintf(&b, "Index: %d documents, %d chunks | Search: %s\n", docsCount, chunksCount, method)
	if e.VectorsStale {
		b.WriteString("Note: local vector index is stale; run `gatt index` to refresh semantic search.\n")
	}
	if len(res.Hits) == 0 {
		b.WriteString("\nNo matching document passages found.\n")
		return b.String(), nil
	}

	// Filter out off-topic trailing hits whose score drops sharply relative to
	// the top hit and that lack query-term overlap (prevents stray unrelated
	// product docs from mixing into focused answers).
	qTerms := docContentTerms(question)
	hits := filterOffTopicDocHits(e.G, res.Hits, qTerms, method)

	// Detect low-confidence results: require real multi-term whole-word
	// coverage (filtering ES/EN stopwords so "las" never matches "class" and
	// "por" never matches "support") and check semantic cosine score separately
	// from keyword counts. Return 0 passages on low confidence.
	confidence, missing := analyzeDocConfidence(e.G, hits, qTerms, method, limit)
	fmt.Fprintf(&b, "Confidence: %s\n", confidence)
	if confidence == "low" {
		if len(missing) > 0 {
			fmt.Fprintf(&b, "Low confidence: no passage matched all key terms (missing: %s). Do not infer or fabricate missing facts; state clearly if the documentation does not contain this information.\n",
				strings.Join(missing, ", "))
		} else {
			b.WriteString("Low confidence: weak match to the query — state clearly that the documentation does not contain a reliable answer.\n")
		}
		b.WriteString("\nNo passages returned (below confidence threshold).\n")
		return b.String(), nil
	}

	// Select top-limit hits, then order chunks belonging to the same document
	// by reading order (chunk_index) so multi-step guides stay sequential.
	selected := make([]FindHit, 0, limit)
	picked := map[string]bool{}
	for _, h := range hits {
		if len(selected) >= limit {
			break
		}
		if picked[h.ID] || e.G.Nodes[h.ID] == nil {
			continue
		}
		picked[h.ID] = true
		selected = append(selected, h)
	}
	selected = orderHitsByReadingOrder(e.G, selected)

	seen := map[string]bool{}
	shown := 0
	for _, h := range selected {
		if seen[h.ID] {
			continue
		}
		n := e.G.Nodes[h.ID]
		if n == nil {
			continue
		}
		seen[h.ID] = true
		shown++

		file := n.Attrs["file"]
		if file == "" {
			file = n.Name
		}
		sec := n.Attrs["section"]
		pageRef := formatPageRef(n.Attrs["page_start"], n.Attrs["page_end"])

		var header strings.Builder
		fmt.Fprintf(&header, "\n## %d. %s", shown, file)
		if pageRef != "" {
			fmt.Fprintf(&header, " (%s)", pageRef)
		}
		if sec != "" && sec != file {
			fmt.Fprintf(&header, " — %s", sec)
		}
		header.WriteByte('\n')
		b.WriteString(header.String())

		if title := n.Attrs["title"]; title != "" && title != file && title != sec {
			fmt.Fprintf(&b, "Title: %s | ID: %s\n", title, n.ID)
		} else {
			fmt.Fprintf(&b, "ID: %s\n", n.ID)
		}

		body := strings.TrimSpace(n.Attrs["text"])
		if body != "" {
			b.WriteString(body)
			b.WriteByte('\n')
		}

		// Graph expansion: if this is a chunk, append the non-overlapping tail
		// of its immediate NEXT neighbor in the same section so answers aren't
		// clipped at a chunk boundary or repeated.
		if n.Type == graph.NodeChunk {
			if next := e.nextSameSectionChunk(n); next != nil && !seen[next.ID] {
				if nextTxt := strings.TrimSpace(next.Attrs["text"]); nextTxt != "" {
					seen[next.ID] = true
					tail := stripChunkOverlap(body, nextTxt)
					if tail != "" {
						b.WriteString(tail)
						b.WriteByte('\n')
					}
				}
			}
		}
	}

	return TrimToTokenBudget(b.String(), maxTokens), nil
}

// codebaseDocsFallback answers doc_context on a codebase graph by searching
// indexed Markdown/prose files (NodeFile nodes carrying doc_body), boosting
// filename/title matches, down-ranking legacy meta docs (HANDOFF, DECISIONS,
// CHANGELOG) unless asked for, and extracting query-centered excerpts from the
// full file on disk when available.
func (e *Engine) codebaseDocsFallback(ctx context.Context, question string, limit, maxTokens int) (string, error) {
	res, err := e.Find(ctx, question, graph.NodeFile, limit*4)
	if err != nil {
		return "", err
	}

	qTerms := graph.QueryTerms(question)
	repoRoot := ""
	if strings.HasPrefix(e.G.Source, "codebase:") {
		repoRoot = strings.TrimPrefix(e.G.Source, "codebase:")
	}

	type docCand struct {
		node     *graph.Node
		filePath string
		rank     float64
		matched  bool
	}
	var cands []docCand
	seenFiles := map[string]bool{}

	for i, h := range res.Hits {
		n := e.G.Nodes[h.ID]
		if n == nil {
			continue
		}
		fp := codebaseDocPath(n)
		if fp == "" || seenFiles[fp] {
			continue
		}
		body := strings.TrimSpace(n.Attrs["doc_body"])
		if body == "" && !isProseDoc(fp) {
			continue
		}
		seenFiles[fp] = true

		// Base score from retrieval rank, boosted when filename matches query
		// terms, and penalized for legacy/meta docs unless explicitly queried.
		score := 1.0 / float64(10+i)
		lowPath := strings.ToLower(fp)
		baseName := strings.ToLower(strings.TrimSuffix(filepath.Base(fp), filepath.Ext(fp)))
		matched := false
		for _, t := range qTerms {
			if strings.Contains(baseName, t) {
				score += 0.25
				matched = true
			} else if strings.Contains(lowPath, t) {
				score += 0.12
				matched = true
			}
		}
		if isLegacyDoc(baseName) && !queryMentions(qTerms, baseName) {
			score *= 0.45
		}
		cands = append(cands, docCand{node: n, filePath: fp, rank: score, matched: matched})
	}

	// Also include any prose file whose filename directly matches a query term
	// even if it missed the top-N vector/FTS cutoff (e.g. docs/AUDIT.md or docs/CONFIG.md).
	for _, n := range e.G.NodesByType(graph.NodeFile) {
		fp := codebaseDocPath(n)
		if fp == "" || seenFiles[fp] || !isProseDoc(fp) {
			continue
		}
		baseName := strings.ToLower(strings.TrimSuffix(filepath.Base(fp), filepath.Ext(fp)))
		hits := 0
		for _, t := range qTerms {
			if strings.Contains(baseName, t) {
				hits++
			}
		}
		if hits > 0 {
			seenFiles[fp] = true
			score := 0.20*float64(hits) + 0.05
			if isLegacyDoc(baseName) && !queryMentions(qTerms, baseName) {
				score *= 0.45
			}
			cands = append(cands, docCand{node: n, filePath: fp, rank: score, matched: true})
		}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].rank > cands[j].rank
	})

	var b strings.Builder
	fmt.Fprintf(&b, "# Document Context (codebase docs): %q\n", question)
	if f := e.G.Freshness(time.Now()); f != "" {
		fmt.Fprintf(&b, "Source: %s\n", f)
	}
	method := res.Method
	if method == "" {
		method = "keyword"
	}
	fmt.Fprintf(&b, "Mode: codebase markdown fallback | Search: %s\n", method)
	b.WriteString("Note: this is a codebase repo (not a `docs` collection); showing matching markdown/prose files.\n")

	shown := 0
	anyTermMatch := false
	for _, c := range cands {
		if shown >= limit {
			break
		}
		fullText := strings.TrimSpace(c.node.Attrs["doc_body"])
		if repoRoot != "" {
			if raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(c.filePath))); err == nil && len(raw) > 0 {
				fullText = strings.TrimSpace(string(raw))
			}
		}
		excerpt, termHits := extractRelevantExcerpt(fullText, qTerms, 1200)
		if termHits > 0 || c.matched {
			anyTermMatch = true
		}

		shown++
		fmt.Fprintf(&b, "\n## %d. %s\n", shown, c.filePath)
		if doc := strings.TrimSpace(c.node.Attrs["doc"]); doc != "" && doc != "true" && doc != "false" {
			fmt.Fprintf(&b, "Summary: %s\n", doc)
		}
		if excerpt != "" {
			b.WriteString(excerpt)
			b.WriteByte('\n')
		}
	}
	if shown == 0 {
		b.WriteString("\nNo matching documentation files found in this codebase.\n")
	} else if len(qTerms) > 0 && !anyTermMatch {
		b.WriteString("\nLow confidence: none of the returned markdown files directly matched the query terms.\n")
	}
	return TrimToTokenBudget(b.String(), maxTokens), nil
}

func codebaseDocPath(n *graph.Node) string {
	if f := strings.TrimSpace(n.Attrs["file"]); f != "" {
		return f
	}
	if strings.HasPrefix(n.ID, "file:") {
		if f := strings.TrimPrefix(n.ID, "file:"); f != "" {
			return f
		}
	}
	if n.Name != "" && n.Name != "(doc)" {
		return n.Name
	}
	return ""
}

func isLegacyDoc(baseName string) bool {
	switch baseName {
	case "handoff", "decisions", "changelog", "history", "todo", "notes":
		return true
	}
	return false
}

func queryMentions(terms []string, baseName string) bool {
	for _, t := range terms {
		if strings.Contains(baseName, t) || strings.Contains(t, baseName) {
			return true
		}
	}
	return false
}

// extractRelevantExcerpt picks the section/window of text with the highest
// density of query terms and trims cleanly at a sentence or paragraph boundary.
func extractRelevantExcerpt(text string, terms []string, maxRunes int) (string, int) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return "", 0
	}
	runes := []rune(text)
	totalHits := countTermHits(strings.ToLower(text), terms)
	if len(runes) <= maxRunes {
		return text, totalHits
	}

	// Split into markdown sections/paragraphs and pick the highest-scoring start offset.
	bestStart := 0
	bestScore := -1
	if len(terms) > 0 {
		step := maxRunes / 3
		if step < 200 {
			step = 200
		}
		for pos := 0; pos < len(runes); pos += step {
			end := min(pos+maxRunes, len(runes))
			win := strings.ToLower(string(runes[pos:end]))
			sc := countTermHits(win, terms)
			if sc > bestScore {
				bestScore = sc
				bestStart = pos
			}
			if end == len(runes) {
				break
			}
		}
	}

	// Align bestStart forward to a line/paragraph start when mid-file.
	prefix := ""
	if bestStart > 0 {
		prefix = "…\n"
		if nl := indexRuneFrom(runes, '\n', bestStart, min(bestStart+120, len(runes))); nl >= 0 && nl+1 < len(runes) {
			bestStart = nl + 1
		}
	}

	end := min(bestStart+maxRunes, len(runes))
	truncated := end < len(runes)
	if truncated {
		cut := cleanExcerptCut(runes, bestStart, end)
		end = cut
	}
	out := strings.TrimSpace(string(runes[bestStart:end]))
	if truncated {
		out += "\n… (excerpt truncated)"
	}
	return prefix + out, totalHits
}

func indexRuneFrom(r []rune, target rune, from, to int) int {
	for i := from; i < to; i++ {
		if r[i] == target {
			return i
		}
	}
	return -1
}

func cleanExcerptCut(runes []rune, start, end int) int {
	minCut := start + (end-start)/2
	for i := end - 1; i > minCut; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			return i
		}
	}
	for i := end - 1; i > minCut; i-- {
		if (runes[i-1] == '.' || runes[i-1] == '!' || runes[i-1] == '?') && unicode.IsSpace(runes[i]) {
			return i
		}
	}
	for i := end - 1; i > minCut; i-- {
		if runes[i] == '\n' {
			return i
		}
	}
	for i := end - 1; i > minCut; i-- {
		if unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return end
}

func countTermHits(lowText string, terms []string) int {
	hits := 0
	for _, t := range terms {
		if strings.Contains(lowText, t) {
			hits++
		}
	}
	return hits
}

func filterOffTopicDocHits(g *graph.Graph, hits []FindHit, terms []string, method string) []FindHit {
	if len(hits) <= 1 {
		return hits
	}
	topScore := hits[0].Score
	if topScore <= 0 {
		return hits
	}

	// Identify files that contain hits covering the maximum number of query
	// terms (e.g. a guide covering all of "configure", "oidc", "sso", "okta").
	// When a strong document matches all terms, drop hits from *other* files
	// that are missing a distinguishing term (e.g. RSA SecurID missing "okta").
	maxTerms := 0
	hitTerms := make([]int, len(hits))
	for i, h := range hits {
		th := hitTermMatches(g, h, terms)
		hitTerms[i] = th
		if th > maxTerms {
			maxTerms = th
		}
	}
	bestFiles := map[string]bool{}
	if len(terms) >= 2 && maxTerms >= 2 {
		for i, h := range hits {
			if hitTerms[i] == maxTerms {
				if n := g.Nodes[h.ID]; n != nil && n.Attrs["file"] != "" {
					bestFiles[n.Attrs["file"]] = true
				}
			}
		}
	}
	strictFileFilter := len(bestFiles) > 0 && maxTerms == len(terms)

	topTermHits := hitTerms[0]
	out := make([]FindHit, 0, len(hits))
	out = append(out, hits[0])

	ratio := float32(0.65)
	if strings.HasPrefix(method, "keyword") {
		ratio = 0.45
	}
	for i := 1; i < len(hits); i++ {
		h := hits[i]
		th := hitTerms[i]
		n := g.Nodes[h.ID]
		file := ""
		if n != nil {
			file = n.Attrs["file"]
		}
		if strictFileFilter && file != "" && !bestFiles[file] && th < maxTerms {
			continue
		}
		// Drop trailing hits that are both significantly weaker than #1 AND
		// have zero lexical overlap when #1 had lexical matches.
		if h.Score < topScore*ratio && (th == 0 || (topTermHits >= 2 && th < topTermHits/2)) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func hitTermMatches(g *graph.Graph, h FindHit, terms []string) int {
	n := g.Nodes[h.ID]
	if n == nil || len(terms) == 0 {
		return 0
	}
	low := strings.ToLower(n.Attrs["text"] + " " + n.Attrs["section"] + " " + n.Attrs["file"])
	return countWordHits(low, terms)
}

var docStopwords = map[string]bool{
	// Spanish
	"las": true, "los": true, "una": true, "uno": true, "unos": true, "unas": true,
	"del": true, "con": true, "por": true, "para": true, "como": true, "cómo": true,
	"que": true, "qué": true, "cual": true, "cuál": true, "cuanto": true, "cuánto": true,
	"donde": true, "dónde": true, "cuando": true, "cuándo": true, "quien": true,
	"esto": true, "esta": true, "este": true, "estos": true, "estas": true,
	"son": true, "hay": true, "sin": true, "sobre": true, "pero": true, "mas": true,
	"más": true, "muy": true, "todo": true, "todos": true, "toda": true, "todas": true,
	"desde": true, "hasta": true, "entre": true, "hacia": true, "segun": true,
	"guia": true, "guía": true, "paso": true, "pasos": true, "usuario": true,
	// English
	"the": true, "and": true, "for": true, "how": true, "what": true, "why": true,
	"who": true, "when": true, "where": true, "which": true, "with": true, "from": true,
	"that": true, "this": true, "these": true, "those": true, "are": true, "was": true,
	"were": true, "not": true, "does": true, "did": true, "can": true, "could": true,
	"should": true, "would": true, "into": true, "about": true, "guide": true, "step": true,
}

// docContentTerms extracts meaningful lowercase query terms, filtering out
// common Spanish and English stopwords so "las" or "por" never produce false
// lexical matches.
func docContentTerms(query string) []string {
	raw := graph.QueryTerms(query)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if len([]rune(t)) < 3 || docStopwords[t] {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return raw
	}
	return out
}

// containsWord reports whether term appears as a whole word (or word prefix of
// length >= 4 for mild inflection like "notific..."/"masking") bounded on the
// left by a non-alphanumeric rune, so "las" never matches "class" and "por"
// never matches "support".
func containsWord(lowText, term string) bool {
	if term == "" {
		return false
	}
	tLen := len(term)
	for i := 0; i <= len(lowText)-tLen; {
		idx := strings.Index(lowText[i:], term)
		if idx < 0 {
			return false
		}
		pos := i + idx
		leftOK := pos == 0 || !isWordChar(rune(lowText[pos-1]))
		endPos := pos + tLen
		rightOK := endPos == len(lowText) || !isWordChar(rune(lowText[endPos]))
		// Allow right-hand suffix inflection only for longer content terms (>= 5 bytes).
		if leftOK && (rightOK || tLen >= 5) {
			return true
		}
		i = pos + 1
	}
	return false
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func countWordHits(lowText string, terms []string) int {
	hits := 0
	for _, t := range terms {
		if containsWord(lowText, t) {
			hits++
		}
	}
	return hits
}

// orderHitsByReadingOrder keeps files in their highest-ranked order while
// sorting multiple chunks from the same file sequentially by chunk_index.
func orderHitsByReadingOrder(g *graph.Graph, hits []FindHit) []FindHit {
	if len(hits) <= 1 {
		return hits
	}
	fileOrder := map[string]int{}
	nextOrder := 0
	for _, h := range hits {
		f := ""
		if n := g.Nodes[h.ID]; n != nil {
			f = n.Attrs["file"]
		}
		if _, ok := fileOrder[f]; !ok {
			fileOrder[f] = nextOrder
			nextOrder++
		}
	}
	out := make([]FindHit, len(hits))
	copy(out, hits)
	sort.SliceStable(out, func(i, j int) bool {
		ni, nj := g.Nodes[out[i].ID], g.Nodes[out[j].ID]
		fi, fj := "", ""
		if ni != nil {
			fi = ni.Attrs["file"]
		}
		if nj != nil {
			fj = nj.Attrs["file"]
		}
		if fileOrder[fi] != fileOrder[fj] {
			return fileOrder[fi] < fileOrder[fj]
		}
		ci, _ := strconv.Atoi(ni.Attrs["chunk_index"])
		cj, _ := strconv.Atoi(nj.Attrs["chunk_index"])
		return ci < cj
	})
	return out
}

// analyzeDocConfidence classifies retrieval confidence as "high", "medium", or
// "low" and lists any key query terms missing across all top passages.
// Semantic cosine scores (0..1) are evaluated separately from keyword counts so
// a single keyword hit (score >= 1.0) never masks missing terms.
func analyzeDocConfidence(g *graph.Graph, hits []FindHit, terms []string, method string, limit int) (confidence string, missing []string) {
	check := min(len(hits), limit)
	if check == 0 {
		return "low", terms
	}
	if len(terms) == 0 {
		return "medium", nil
	}

	foundAny := make(map[string]bool, len(terms))
	maxInOne := 0
	var topCosine float32
	isSemantic := method == "semantic" || method == "hybrid"

	for i := 0; i < check; i++ {
		h := hits[i]
		if isSemantic && h.Score > topCosine && h.Score <= 1.0 {
			topCosine = h.Score
		}
		n := g.Nodes[h.ID]
		if n == nil {
			continue
		}
		lowText := strings.ToLower(n.Attrs["text"] + " " + n.Attrs["section"] + " " + n.Attrs["file"])
		inThis := 0
		for _, t := range terms {
			if containsWord(lowText, t) {
				foundAny[t] = true
				inThis++
			}
		}
		if inThis > maxInOne {
			maxInOne = inThis
		}
	}

	for _, t := range terms {
		if !foundAny[t] {
			missing = append(missing, t)
		}
	}

	// Cross-lingual queries (e.g. Spanish question over English docs) have 0
	// lexical overlap; only trust them when semantic cosine similarity is strong.
	if maxInOne == 0 {
		if isSemantic && topCosine >= 0.72 {
			return "medium", missing
		}
		return "low", missing
	}

	required := 1
	if len(terms) >= 2 {
		required = max(2, (len(terms)+1)/2)
	}
	if maxInOne >= len(terms) || (maxInOne >= required && len(missing) == 0) {
		return "high", nil
	}
	if maxInOne < required && topCosine < 0.74 {
		return "low", missing
	}
	if len(terms) >= 2 && len(missing)*2 >= len(terms) && topCosine < 0.74 {
		return "low", missing
	}
	return "medium", missing
}

// DocGuidePackWithBudget builds a structured source pack optimized for
// authoring user guides: returns the top 1..N canonical documents in full
// reading order (all sections/steps 1->N), lists referenced screenshots and
// modification dates, and separates related/variant documents so the author
// doesn't mix incompatible procedures (e.g. Slack token vs. direct).
func (e *Engine) DocGuidePackWithBudget(ctx context.Context, topic string, maxDocs, maxTokens int) (string, error) {
	if maxDocs <= 0 {
		maxDocs = 2
	}
	if maxTokens <= 0 {
		maxTokens = 6000
	}

	docsCount := len(e.G.NodesByType(graph.NodeDocument))
	chunksCount := len(e.G.NodesByType(graph.NodeChunk))
	if docsCount == 0 && chunksCount == 0 {
		return e.codebaseDocsFallback(ctx, topic, maxDocs, maxTokens)
	}

	res, err := e.Find(ctx, topic, graph.NodeChunk, 24)
	if err != nil {
		return "", err
	}
	method := res.Method
	if method == "" {
		method = "keyword"
	}

	qTerms := docContentTerms(topic)
	hits := filterOffTopicDocHits(e.G, res.Hits, qTerms, method)
	confidence, missing := analyzeDocConfidence(e.G, hits, qTerms, method, 8)

	var b strings.Builder
	fmt.Fprintf(&b, "# Guide Source Pack: %q\n", topic)
	if f := e.G.Freshness(time.Now()); f != "" {
		fmt.Fprintf(&b, "Source: %s\n", f)
	}
	fmt.Fprintf(&b, "Index: %d documents, %d chunks | Search: %s | Confidence: %s\n", docsCount, chunksCount, method, confidence)

	if len(hits) == 0 || confidence == "low" {
		if len(missing) > 0 {
			fmt.Fprintf(&b, "Low confidence: no canonical document matched key terms (missing: %s). Verify whether this topic exists in the corpus before writing a guide.\n",
				strings.Join(missing, ", "))
		} else {
			b.WriteString("Low confidence: no strong canonical document found for this topic.\n")
		}
		return b.String(), nil
	}

	// Score documents by aggregating their top chunk hits + filename/title match.
	type docScore struct {
		file  string
		docID string
		score float64
		hits  int
	}
	byFile := map[string]*docScore{}
	for rank, h := range hits {
		n := e.G.Nodes[h.ID]
		if n == nil {
			continue
		}
		f := n.Attrs["file"]
		if f == "" {
			continue
		}
		ds := byFile[f]
		if ds == nil {
			ds = &docScore{file: f, docID: "doc:" + f}
			byFile[f] = ds
		}
		ds.score += 1.0 / float64(5+rank)
		ds.hits++
	}
	var ranked []*docScore
	for _, ds := range byFile {
		lowFile := strings.ToLower(ds.file)
		for _, t := range qTerms {
			if containsWord(lowFile, t) {
				ds.score += 0.25
			}
		}
		ranked = append(ranked, ds)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].file < ranked[j].file
	})

	canonical := ranked
	var related []*docScore
	if len(canonical) > maxDocs {
		related = canonical[maxDocs:]
		canonical = canonical[:maxDocs]
		if len(related) > 5 {
			related = related[:5]
		}
	}

	for i, ds := range canonical {
		docNode := e.G.Nodes[ds.docID]
		title := ds.file
		if docNode != nil && docNode.Attrs["title"] != "" {
			title = docNode.Attrs["title"]
		}
		fmt.Fprintf(&b, "\n## Canonical Source %d: %s\n", i+1, ds.file)
		fmt.Fprintf(&b, "Title: %s", title)
		if docNode != nil {
			if f := docNode.Attrs["format"]; f != "" {
				fmt.Fprintf(&b, " | Format: %s", f)
			}
			if p := docNode.Attrs["pages"]; p != "" {
				fmt.Fprintf(&b, " | Pages: %s", p)
			}
			if mt := docNode.Attrs["mtime"]; mt != "" {
				if sec, err := strconv.ParseInt(mt, 10, 64); err == nil {
					fmt.Fprintf(&b, " | Modified: %s", time.Unix(sec, 0).Format("2006-01-02"))
				}
			}
		}
		b.WriteByte('\n')
		if docNode != nil && docNode.Attrs["images"] != "" {
			fmt.Fprintf(&b, "Screenshots referenced: %s\n", docNode.Attrs["images"])
		}
		b.WriteByte('\n')
		b.WriteString(e.reconstructDocumentText(ds.docID))
		b.WriteByte('\n')
	}

	if len(related) > 0 {
		b.WriteString("\n## Related / Variant Documents (not expanded)\n")
		for _, r := range related {
			label := r.file
			if dn := e.G.Nodes[r.docID]; dn != nil && dn.Attrs["title"] != "" && dn.Attrs["title"] != r.file {
				label = fmt.Sprintf("%s — %s", r.file, dn.Attrs["title"])
			}
			fmt.Fprintf(&b, "- %s\n", label)
		}
	}

	return TrimToTokenBudget(b.String(), maxTokens), nil
}

// reconstructDocumentText walks document -> HAS_SECTION -> HAS_CHUNK in exact
// reading order, deduplicating chunk overlap so the full canonical document is
// returned cleanly.
func (e *Engine) reconstructDocumentText(docID string) string {
	var sections []*graph.Node
	for _, ed := range e.G.EdgesOf(docID) {
		if ed.Type == graph.EdgeHasSection && ed.From == docID {
			if sn := e.G.Nodes[ed.To]; sn != nil {
				sections = append(sections, sn)
			}
		}
	}
	sort.Slice(sections, func(i, j int) bool {
		return trailingInt(sections[i].ID) < trailingInt(sections[j].ID)
	})

	var b strings.Builder
	for _, sec := range sections {
		if sec.Name != "" {
			pageRef := formatPageRef(sec.Attrs["page_start"], sec.Attrs["page_end"])
			if pageRef != "" {
				fmt.Fprintf(&b, "### %s (%s)\n", sec.Name, pageRef)
			} else {
				fmt.Fprintf(&b, "### %s\n", sec.Name)
			}
		}
		var chunks []*graph.Node
		for _, ed := range e.G.EdgesOf(sec.ID) {
			if ed.Type == graph.EdgeHasChunk && ed.From == sec.ID {
				if cn := e.G.Nodes[ed.To]; cn != nil {
					chunks = append(chunks, cn)
				}
			}
		}
		sort.Slice(chunks, func(i, j int) bool {
			ci, _ := strconv.Atoi(chunks[i].Attrs["chunk_index"])
			cj, _ := strconv.Atoi(chunks[j].Attrs["chunk_index"])
			return ci < cj
		})
		prevTxt := ""
		for _, ch := range chunks {
			txt := strings.TrimSpace(ch.Attrs["text"])
			if txt == "" {
				continue
			}
			if prevTxt != "" {
				txt = stripChunkOverlap(prevTxt, txt)
			}
			if txt != "" {
				b.WriteString(txt)
				b.WriteString("\n\n")
			}
			prevTxt = strings.TrimSpace(ch.Attrs["text"])
		}
	}
	return strings.TrimSpace(b.String())
}

func trailingInt(id string) int {
	if idx := strings.LastIndexByte(id, ':'); idx >= 0 {
		n, _ := strconv.Atoi(id[idx+1:])
		return n
	}
	return 0
}

// stripChunkOverlap removes the overlapping prefix of next that already
// appears at the end of prev (both exact rune match and line-level suffix/prefix
// match), so expanded adjacent chunks read smoothly without repetition.
func stripChunkOverlap(prev, next string) string {
	pr := []rune(prev)
	nr := []rune(next)
	maxCheck := min(len(pr), len(nr))
	if maxCheck > 350 {
		maxCheck = 350
	}
	for k := maxCheck; k >= 15; k-- {
		match := true
		for i := 0; i < k; i++ {
			if pr[len(pr)-k+i] != nr[i] {
				match = false
				break
			}
		}
		if match {
			return strings.TrimSpace(string(nr[k:]))
		}
	}
	// Line-level fallback: drop leading non-empty lines of next that match the
	// trailing non-empty lines of prev (handles minor blank-line differences).
	pLines := nonEmptyLines(prev)
	nLines := strings.Split(next, "\n")
	if len(pLines) > 0 {
		maxLines := min(len(pLines), 5)
		for k := maxLines; k >= 1; k-- {
			matched := 0
			idx := 0
			for idx < len(nLines) && matched < k {
				t := strings.TrimSpace(nLines[idx])
				idx++
				if t == "" {
					continue
				}
				if t != pLines[len(pLines)-k+matched] {
					matched = -1
					break
				}
				matched++
			}
			if matched == k {
				return strings.TrimSpace(strings.Join(nLines[idx:], "\n"))
			}
		}
	}
	return next
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) nextSameSectionChunk(n *graph.Node) *graph.Node {
	for _, ed := range e.G.EdgesOf(n.ID) {
		if ed.Type == graph.EdgeNext && ed.From == n.ID {
			other := e.G.Nodes[ed.To]
			if other != nil && other.Type == graph.NodeChunk &&
				other.Attrs["file"] == n.Attrs["file"] &&
				other.Attrs["section"] == n.Attrs["section"] {
				return other
			}
		}
	}
	return nil
}

func formatPageRef(start, end string) string {
	if start == "" {
		return ""
	}
	if end != "" && end != start {
		return fmt.Sprintf("pp. %s-%s", start, end)
	}
	return fmt.Sprintf("p. %s", start)
}

// RenderDocument renders a document, section, or chunk node in compact text form.
func (e *Engine) RenderDocument(id string) (string, error) {
	d, err := e.Describe(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s)\n", d.Name, d.Type)
	if f := d.Attrs["file"]; f != "" {
		fmt.Fprintf(&b, "file: %s", f)
		if fmtStr := d.Attrs["format"]; fmtStr != "" {
			fmt.Fprintf(&b, " (%s)", fmtStr)
		}
		if p := d.Attrs["pages"]; p != "" {
			fmt.Fprintf(&b, ", %s pages", p)
		}
		if pr := formatPageRef(d.Attrs["page_start"], d.Attrs["page_end"]); pr != "" {
			fmt.Fprintf(&b, ", %s", pr)
		}
		b.WriteByte('\n')
	}
	if t := d.Attrs["title"]; t != "" && t != d.Name {
		fmt.Fprintf(&b, "title: %s\n", t)
	}
	if s := d.Attrs["section"]; s != "" && s != d.Name {
		fmt.Fprintf(&b, "section: %s\n", s)
	}
	if en := d.Attrs["entity_note"]; en != "" {
		fmt.Fprintf(&b, "note: %s\n", en)
	}
	if txt := strings.TrimSpace(d.Attrs["text"]); txt != "" {
		fmt.Fprintf(&b, "\n%s\n", txt)
	}
	if d.Type == graph.NodeDocument {
		var secs []string
		for _, ed := range d.Edges {
			if ed.Type == graph.EdgeHasSection && ed.Dir == "out" {
				if sn := e.G.Nodes[ed.Other]; sn != nil {
					label := sn.Name
					if pr := formatPageRef(sn.Attrs["page_start"], sn.Attrs["page_end"]); pr != "" {
						label += " (" + pr + ")"
					}
					secs = append(secs, label)
				}
			}
		}
		if len(secs) > 0 {
			b.WriteString("\nsections:\n")
			for _, s := range secs {
				b.WriteString("- " + s + "\n")
			}
		}
	}
	return b.String(), nil
}
