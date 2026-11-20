// Package docs extracts structured semantic graphs from document collections
// (PDF, DOCX, Markdown, and plain text). Each file becomes a document node
// containing section nodes and retrieval-sized chunk nodes linked in reading
// order (HAS_SECTION, HAS_CHUNK, NEXT), ready for hybrid search (Qdrant +
// ONNX embeddings + SQLite FTS5) and graph-aware context expansion.
package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"graphallthethings/internal/graph"
)

const (
	// defaultChunkRunes targets ~100-120 tokens so gatt-embed's 128-token ONNX
	// window (paraphrase-multilingual-MiniLM-L12-v2) embeds the full chunk
	// without truncating its tail.
	defaultChunkRunes   = 480
	defaultOverlapRunes = 80
)

// ParsedSection is a logical section (heading or page) inside a document.
type ParsedSection struct {
	Title     string
	Level     int
	PageStart int
	PageEnd   int
	Body      string
}

// ParsedDoc is the normalized output of any document format parser.
type ParsedDoc struct {
	Title    string
	Format   string
	Pages    int
	Sections []ParsedSection
}

type Connector struct {
	Root         string
	ChunkRunes   int
	OverlapRunes int
}

func New(root string) *Connector {
	return &Connector{
		Root:         root,
		ChunkRunes:   defaultChunkRunes,
		OverlapRunes: defaultOverlapRunes,
	}
}

func (c *Connector) Name() string { return "docs" }

func isSupportedExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".pdf", ".docx", ".md", ".markdown", ".txt":
		return true
	}
	return false
}

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	absRoot, err := filepath.Abs(c.Root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}

	var files []string
	var baseDir string
	if !info.IsDir() {
		if !isSupportedExt(filepath.Ext(absRoot)) {
			return nil, fmt.Errorf("unsupported document format %q (supported: .pdf, .docx, .md, .txt)", filepath.Ext(absRoot))
		}
		baseDir = filepath.Dir(absRoot)
		files = []string{absRoot}
	} else {
		baseDir = absRoot
		ignorePatterns := loadGattIgnore(absRoot)
		err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rel, _ := filepath.Rel(absRoot, path)
			relSlash := filepath.ToSlash(rel)
			if d.IsDir() {
				if graph.SkipDir(d.Name(), path == absRoot) {
					return filepath.SkipDir
				}
				if path != absRoot && matchesIgnore(relSlash, d.Name(), true, ignorePatterns) {
					return filepath.SkipDir
				}
				return nil
			}
			if matchesIgnore(relSlash, d.Name(), false, ignorePatterns) {
				return nil
			}
			if isSupportedExt(filepath.Ext(d.Name())) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
	}

	g := graph.New("docs:" + absRoot)
	chunkSize := c.ChunkRunes
	if chunkSize <= 0 {
		chunkSize = defaultChunkRunes
	}
	overlap := c.OverlapRunes
	if overlap < 0 || overlap >= chunkSize {
		overlap = defaultOverlapRunes
	}

	for _, path := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		rel = filepath.ToSlash(rel)

		parsed, err := parseFile(ctx, path)
		if err != nil {
			// Skip unreadable/corrupt files with a warning instead of failing
			// the entire collection extraction.
			fmt.Fprintf(os.Stderr, "docs: skipping %s: %v\n", rel, err)
			continue
		}
		if len(parsed.Sections) == 0 {
			continue
		}

		st, _ := os.Stat(path)
		hash := fileSHA256(path)
		title := strings.TrimSpace(parsed.Title)
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		}

		docID := "doc:" + rel
		docAttrs := map[string]string{
			"file":   rel,
			"title":  title,
			"format": parsed.Format,
			"sha256": hash,
		}
		if parsed.Pages > 0 {
			docAttrs["pages"] = strconv.Itoa(parsed.Pages)
		}
		if st != nil {
			docAttrs["mtime"] = strconv.FormatInt(st.ModTime().Unix(), 10)
		}
		var docImages []string
		seenImages := map[string]bool{}
		collectImages := func(raw string) {
			for _, img := range extractImageNames(raw) {
				if !seenImages[img] {
					seenImages[img] = true
					docImages = append(docImages, img)
				}
			}
		}
		g.AddNode(&graph.Node{
			ID:    docID,
			Type:  graph.NodeDocument,
			Name:  rel,
			Attrs: docAttrs,
		})

		var prevSecID, prevChunkID string
		chunkTotal := 0
		for si, sec := range parsed.Sections {
			collectImages(sec.Body)
			body := normalizeWhitespace(cleanTextileInline(sec.Body))
			if body == "" && strings.TrimSpace(sec.Title) == "" {
				continue
			}
			secTitle := strings.TrimSpace(sec.Title)
			if secTitle == "" {
				if sec.PageStart > 0 {
					secTitle = fmt.Sprintf("Page %d", sec.PageStart)
				} else {
					secTitle = title
				}
			}

			secID := fmt.Sprintf("sec:%s:%d", rel, si+1)
			secAttrs := map[string]string{
				"file":    rel,
				"title":   title,
				"section": secTitle,
				"format":  parsed.Format,
				"level":   strconv.Itoa(max(sec.Level, 1)),
			}
			if sec.PageStart > 0 {
				secAttrs["page_start"] = strconv.Itoa(sec.PageStart)
			}
			if sec.PageEnd > 0 {
				secAttrs["page_end"] = strconv.Itoa(sec.PageEnd)
			}
			// Store a short preview on the section node so section-level search works too.
			if body != "" {
				secAttrs["text"] = truncateRunes(body, 280)
			}
			g.AddNode(&graph.Node{
				ID:    secID,
				Type:  graph.NodeSection,
				Name:  secTitle,
				Attrs: secAttrs,
			})
			g.AddEdge(docID, secID, graph.EdgeHasSection, nil)
			if prevSecID != "" {
				g.AddEdge(prevSecID, secID, graph.EdgeNext, nil)
			}
			prevSecID = secID

			chunks := splitChunks(body, chunkSize, overlap)
			for ci, ch := range chunks {
				if isLowValueChunk(ch) {
					continue
				}
				chunkTotal++
				chunkID := fmt.Sprintf("chunk:%s:%d:%d", rel, si+1, ci+1)
				chunkAttrs := map[string]string{
					"file":        rel,
					"title":       title,
					"section":     secTitle,
					"format":      parsed.Format,
					"chunk_index": strconv.Itoa(chunkTotal),
					"text":        ch,
				}
				if sec.PageStart > 0 {
					chunkAttrs["page_start"] = strconv.Itoa(sec.PageStart)
				}
				if sec.PageEnd > 0 {
					chunkAttrs["page_end"] = strconv.Itoa(sec.PageEnd)
				}
				g.AddNode(&graph.Node{
					ID:    chunkID,
					Type:  graph.NodeChunk,
					Name:  fmt.Sprintf("%s § %s #%d", filepath.Base(rel), secTitle, ci+1),
					Attrs: chunkAttrs,
				})
				g.AddEdge(secID, chunkID, graph.EdgeHasChunk, nil)
				if prevChunkID != "" {
					g.AddEdge(prevChunkID, chunkID, graph.EdgeNext, nil)
				}
				prevChunkID = chunkID
			}
		}
		docAttrs["chunks"] = strconv.Itoa(chunkTotal)
		if len(docImages) > 0 {
			docAttrs["images"] = strings.Join(docImages, ", ")
		}
	}

	return g, nil
}

func parseFile(ctx context.Context, path string) (*ParsedDoc, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return parsePDF(ctx, path)
	case ".docx":
		return parseDOCX(path)
	case ".md", ".markdown", ".txt":
		return parseMarkdown(path)
	default:
		return nil, fmt.Errorf("unsupported extension %q", filepath.Ext(path))
	}
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func normalizeWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	var out []string
	blankRun := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			blankRun++
			if blankRun <= 1 && len(out) > 0 {
				out = append(out, "")
			}
			continue
		}
		blankRun = 0
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func truncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return strings.TrimSpace(string(r[:maxLen])) + "…"
}

// splitChunks breaks text into overlapping rune-bounded chunks, preferring
// paragraph, newline, sentence, or whitespace boundaries so UTF-8 characters
// and words are never split mid-token.
func splitChunks(text string, maxRunes, overlapRunes int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}

	var chunks []string
	start := 0
	for start < len(runes) {
		end := start + maxRunes
		if end >= len(runes) {
			if seg := strings.TrimSpace(string(runes[start:])); seg != "" {
				chunks = append(chunks, seg)
			}
			break
		}

		cut := findBestCut(runes, start, end)
		seg := strings.TrimSpace(string(runes[start:cut]))
		if seg != "" {
			chunks = append(chunks, seg)
		}

		next := cut - overlapRunes
		if next <= start {
			next = cut
		}
		// Advance next to the start of a word so overlap doesn't begin mid-word.
		if next > start && next < len(runes) {
			for next < cut && !unicode.IsSpace(runes[next-1]) {
				next++
			}
			for next < len(runes) && unicode.IsSpace(runes[next]) {
				next++
			}
		}
		if next <= start {
			next = cut
		}
		start = next
	}
	return chunks
}

func findBestCut(runes []rune, start, end int) int {
	minCut := start + (end-start)/2
	// 1. Prefer paragraph break ("\n\n")
	for i := end - 1; i > minCut; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			return i
		}
	}
	// 2. Prefer sentence boundary ('.', '!', '?') followed by whitespace
	for i := end - 1; i > minCut; i-- {
		if (runes[i-1] == '.' || runes[i-1] == '!' || runes[i-1] == '?') && unicode.IsSpace(runes[i]) {
			return i
		}
	}
	// 3. Prefer newline
	for i := end - 1; i > minCut; i-- {
		if runes[i] == '\n' {
			return i
		}
	}
	// 4. Prefer any whitespace
	for i := end - 1; i > minCut; i-- {
		if unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return end
}

var (
	mdImageRe      = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)
	textileImageRe = regexp.MustCompile(`(?i)!(?:\{[^}]*\})?([^!\s]+\.(?:png|jpg|jpeg|gif|svg|webp))!`)
	tocMarkerRe    = regexp.MustCompile(`(?i)\{\{>?\s*toc\s*\}\}|\[toc\]`)
	mdLinkOnlyRe   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	bareURLRe      = regexp.MustCompile(`https?://\S+`)
	textileBlockRe = regexp.MustCompile(`(?m)^(?:bq|bc|p)\.\s+`)
)

// extractImageNames returns image filenames referenced via Markdown or Textile syntax.
func extractImageNames(s string) []string {
	var out []string
	for _, m := range mdImageRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			out = append(out, filepath.Base(strings.TrimSpace(m[1])))
		}
	}
	for _, m := range textileImageRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			out = append(out, filepath.Base(strings.TrimSpace(m[1])))
		}
	}
	return out
}

// cleanTextileInline strips noisy Textile formatting artifacts (*_, _*, bq.,
// bc., {{toc}}) and normalizes Textile image tags into readable placeholders
// without modifying actual document text or credentials.
func cleanTextileInline(s string) string {
	if s == "" {
		return s
	}
	s = tocMarkerRe.ReplaceAllString(s, "")
	s = textileImageRe.ReplaceAllString(s, "[image: $1]")
	s = textileBlockRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "*_", "")
	s = strings.ReplaceAll(s, "_*", "")
	return s
}

// isLowValueChunk filters out boilerplate/noise chunks that hurt retrieval
// quality: standalone image tags (Markdown or Textile with style attrs),
// horizontal rules, Redmine/wiki TOC blocks, bare ticket URLs, or fragments
// with fewer than 20 meaningful characters of actual prose.
func isLowValueChunk(chunk string) bool {
	var kept []string
	for _, line := range strings.Split(chunk, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// Skip pure horizontal rules (---, ***, ___) or empty table separators (| --- |)
		if strings.Trim(t, "-*_ =|:") == "" {
			continue
		}
		// Skip Redmine/wiki export metadata headers that repeat on every file
		if strings.HasPrefix(t, "> **Source:**") ||
			strings.HasPrefix(t, "> **Component:**") ||
			strings.HasPrefix(t, "> **Internal KB slug:**") {
			continue
		}
		t = mdImageRe.ReplaceAllString(t, "")
		t = textileImageRe.ReplaceAllString(t, "")
		t = tocMarkerRe.ReplaceAllString(t, "")
		if strings.HasPrefix(t, "[image:") && strings.HasSuffix(t, "]") {
			t = ""
		}
		t = strings.TrimSpace(strings.Trim(t, "|"))
		if t == "" {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return true
	}

	// Detect pure Table-of-Contents blocks where most lines are markdown anchor links.
	tocLines := 0
	for _, l := range kept {
		trimmed := strings.TrimLeft(l, "-*+0123456789. \t")
		if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "](#") && strings.HasSuffix(trimmed, ")") {
			tocLines++
		}
	}
	if tocLines > 0 && tocLines*2 >= len(kept) {
		return true
	}

	// Count actual letters/digits after stripping markdown link URLs and bare URLs
	// (so a standalone ticket link like "https://redmine.../issues/1234" is dropped).
	plain := mdLinkOnlyRe.ReplaceAllString(strings.Join(kept, " "), "$1")
	plain = bareURLRe.ReplaceAllString(plain, "")
	alnum := 0
	for _, r := range plain {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alnum++
		}
	}
	return alnum < 15
}

// loadGattIgnore reads optional exclusion patterns from <root>/.gattignore
// (one glob per line; '#' comments and blank lines ignored) so sensitive or
// internal files (e.g. AWS_Accounts.md) can be excluded from indexing.
func loadGattIgnore(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, ".gattignore"))
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(p))
	}
	return patterns
}

func matchesIgnore(relSlash, baseName string, isDir bool, patterns []string) bool {
	for _, p := range patterns {
		dirOnly := strings.HasSuffix(p, "/")
		pat := strings.TrimSuffix(p, "/")
		if dirOnly && !isDir {
			continue
		}
		if strings.Contains(pat, "/") {
			if ok, _ := filepath.Match(pat, relSlash); ok {
				return true
			}
			if isDir && strings.HasPrefix(relSlash+"/", pat+"/") {
				return true
			}
		} else {
			if ok, _ := filepath.Match(pat, baseName); ok {
				return true
			}
		}
	}
	return false
}
