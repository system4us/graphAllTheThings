// Package search provides pure-Go hybrid search (FTS5 / keyword + semantic
// vectors fused with Reciprocal Rank Fusion) over a graph.Graph, with per-node
// access-control filtering (AllowNode) and optional graph-neighborhood context
// expansion.
//
// Consumers can plug in any Embedder (such as pkg/embed.Client, OpenAI, Gemini,
// Ollama, or a local hash embedder) and any VectorStore (including the built-in
// MemoryStore or an external database).
//
// This package is MIT-licensed (see ../LICENSE) and builds with CGO_ENABLED=0.
package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// Embedder turns one or more texts into float32 embedding vectors.
// *embed.Client from pkg/embed satisfies this interface directly.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// VectorPoint is one embedded graph node stored in a VectorStore.
type VectorPoint struct {
	NodeID string
	Type   string
	Name   string
	Text   string
	Hash   string
	Vector []float32
}

// VectorHit is one semantic nearest-neighbor match returned by a VectorStore.
type VectorHit struct {
	NodeID string
	Score  float32
	Type   string
	Name   string
}

// VectorStore indexes and searches node embeddings.
type VectorStore interface {
	Upsert(ctx context.Context, points []VectorPoint) error
	Search(ctx context.Context, vector []float32, limit int, nodeType string) ([]VectorHit, error)
}

// Options configures a hybrid graph search call.
type Options struct {
	// NodeType optionally restricts hits to a single node type (e.g. "chunk", "document").
	NodeType string
	// Limit caps the number of returned hits (default 10).
	Limit int
	// FTSPath optionally points to a SQLite graph.db to accelerate keyword search via FTS5.
	FTSPath string
	// Embedder and Vectors enable semantic + hybrid RRF search when both are non-nil.
	Embedder Embedder
	Vectors  VectorStore
	// AllowNode filters which nodes may appear in results or expanded context
	// (e.g. folder-scoped RBAC). Nil allows all nodes.
	AllowNode func(*graph.Node) bool
	// ExpandHops, when > 0, attaches an RBAC-filtered N-hop Subgraph around each hit.
	ExpandHops int
	// ExpandEdgeTypes optionally restricts ExpandHops to specific edge types.
	ExpandEdgeTypes []string
}

// Hit is one ranked search result.
type Hit struct {
	Node           *graph.Node
	Score          float32
	SemanticScore  float32
	KeywordMatches int
	Text           string
	Context        *graph.Graph // populated when Options.ExpandHops > 0
}

// Result is the output of Query.
type Result struct {
	// Method is "hybrid", "semantic", "keyword-fts", or "keyword".
	Method string
	// Terms are the normalized search terms extracted from the query.
	Terms []string
	Hits  []Hit
}

// Query searches g for query using hybrid retrieval (semantic vectors + FTS5 /
// accent-insensitive keyword matching fused via RRF), enforcing opts.AllowNode
// and optionally expanding graph context around each hit.
func Query(ctx context.Context, g *graph.Graph, query string, opts Options) (Result, error) {
	if g == nil {
		return Result{}, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	terms := SearchTerms(query)
	if len(terms) == 0 && strings.TrimSpace(query) == "" {
		return Result{Method: "keyword"}, nil
	}

	// Over-fetch when filtering by AllowNode so RBAC exclusions do not starve top-K.
	fetchLimit := limit * 3
	if fetchLimit < 24 {
		fetchLimit = 24
	}
	if opts.AllowNode != nil && fetchLimit < limit*6 {
		fetchLimit = limit * 6
	}

	allow := func(n *graph.Node) bool {
		if n == nil {
			return false
		}
		if opts.NodeType != "" && n.Type != opts.NodeType {
			return false
		}
		return opts.AllowNode == nil || opts.AllowNode(n)
	}

	// 1. Semantic search (optional).
	var semHits []VectorHit
	semOK := false
	if opts.Embedder != nil && opts.Vectors != nil {
		vecs, err := opts.Embedder.Embed(ctx, []string{query})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			if sh, serr := opts.Vectors.Search(ctx, vecs[0], fetchLimit, opts.NodeType); serr == nil {
				for _, h := range sh {
					if allow(g.Nodes[h.NodeID]) {
						semHits = append(semHits, h)
					}
				}
				semOK = len(semHits) > 0
			}
		}
	}

	// 2. Keyword search (FTS5 when FTSPath is set, plus accent-insensitive in-memory scan).
	kwHits, kwMethod := keywordSearch(g, terms, opts, fetchLimit, allow)

	// 3. Fuse via Reciprocal Rank Fusion (RRF).
	type fused struct {
		node     *graph.Node
		text     string
		rrf      float32
		semScore float32
		kwCount  int
	}
	byID := map[string]*fused{}
	const rrfK = 60.0

	for rank, sh := range semHits {
		n := g.Nodes[sh.NodeID]
		if n == nil {
			continue
		}
		txt := g.NodeText(n.ID)
		byID[n.ID] = &fused{
			node:     n,
			text:     txt,
			rrf:      float32(1.0 / (rrfK + float64(rank+1))),
			semScore: sh.Score,
			kwCount:  countTermMatches(FoldAccents(strings.ToLower(n.Name+" "+txt)), terms),
		}
	}

	for rank, kh := range kwHits {
		if f, ok := byID[kh.node.ID]; ok {
			f.rrf += float32(1.0 / (rrfK + float64(rank+1)))
			if kh.matches > f.kwCount {
				f.kwCount = kh.matches
			}
		} else {
			weight := 1.0
			if semOK {
				weight = 0.85
			}
			byID[kh.node.ID] = &fused{
				node:    kh.node,
				text:    kh.text,
				rrf:     float32(weight / (rrfK + float64(rank+1))),
				kwCount: kh.matches,
			}
		}
	}

	ranked := make([]*fused, 0, len(byID))
	for _, f := range byID {
		// Reward covering multiple query terms (e.g. matching all of "compras", "2021", "it").
		if len(terms) > 0 && f.kwCount > 0 {
			coverage := float32(f.kwCount) / float32(len(terms))
			f.rrf += coverage * 0.015
		}
		ranked = append(ranked, f)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].rrf != ranked[j].rrf {
			return ranked[i].rrf > ranked[j].rrf
		}
		if ranked[i].semScore != ranked[j].semScore {
			return ranked[i].semScore > ranked[j].semScore
		}
		return ranked[i].node.ID < ranked[j].node.ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	method := kwMethod
	switch {
	case semOK && len(kwHits) > 0:
		method = "hybrid"
	case semOK:
		method = "semantic"
	}

	hits := make([]Hit, 0, len(ranked))
	for _, f := range ranked {
		h := Hit{
			Node:           f.node,
			Score:          f.rrf,
			SemanticScore:  f.semScore,
			KeywordMatches: f.kwCount,
			Text:           f.text,
		}
		if opts.ExpandHops > 0 {
			h.Context = g.Subgraph(f.node.ID, graph.TraverseOptions{
				EdgeTypes: opts.ExpandEdgeTypes,
				Direction: graph.DirectionBoth,
				MaxDepth:  opts.ExpandHops,
				AllowNode: opts.AllowNode,
			})
		}
		hits = append(hits, h)
	}
	return Result{Method: method, Terms: terms, Hits: hits}, nil
}

type kwHit struct {
	node    *graph.Node
	text    string
	matches int
	score   float32
}

func keywordSearch(g *graph.Graph, terms []string, opts Options, limit int, allow func(*graph.Node) bool) ([]kwHit, string) {
	if len(terms) == 0 {
		return nil, "keyword"
	}
	seen := map[string]bool{}
	var hits []kwHit
	method := "keyword"

	if opts.FTSPath != "" {
		if ids := graph.FTSQueryTerms(opts.FTSPath, terms, opts.NodeType, limit); len(ids) > 0 {
			method = "keyword-fts"
			for rank, id := range ids {
				n := g.Nodes[id]
				if !allow(n) || seen[id] {
					continue
				}
				seen[id] = true
				txt := g.NodeText(id)
				folded := FoldAccents(strings.ToLower(n.Name + " " + txt))
				m := countTermMatches(folded, terms)
				hits = append(hits, kwHit{
					node:    n,
					text:    txt,
					matches: m,
					score:   float32(m) + 1.0/float32(10+rank),
				})
			}
		}
	}

	// Complement (or run full scan when FTS5 wasn't used) with accent-insensitive
	// in-memory matching so accented Spanish terms ("adquisición" vs "adquisicion")
	// and un-persisted graphs always match reliably.
	if len(hits) < limit {
		for id, n := range g.Nodes {
			if seen[id] || !allow(n) {
				continue
			}
			txt := g.NodeText(id)
			folded := FoldAccents(strings.ToLower(n.Name + " " + txt))
			m := countTermMatches(folded, terms)
			if m == 0 {
				continue
			}
			nameFolded := FoldAccents(strings.ToLower(n.Name))
			nameMatches := countTermMatches(nameFolded, terms)
			hits = append(hits, kwHit{
				node:    n,
				text:    txt,
				matches: m,
				score:   float32(m) + 0.5*float32(nameMatches),
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].node.ID < hits[j].node.ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, method
}

// searchStopwords intentionally omits short enterprise acronyms like "it", "hr",
// "qa", "bi", "ti", "rh" so queries like "compras 2021 it" keep every signal word.
var searchStopwords = map[string]bool{
	// Spanish
	"el": true, "la": true, "los": true, "las": true, "un": true, "una": true,
	"unos": true, "unas": true, "de": true, "del": true, "en": true, "por": true,
	"para": true, "con": true, "sin": true, "sobre": true, "entre": true,
	"y": true, "o": true, "al": true, "lo": true, "le": true, "se": true,
	"su": true, "sus": true, "que": true, "como": true, "donde": true,
	"cuando": true, "cual": true, "es": true, "son": true, "esta": true, "este": true,
	// English
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "at": true, "by": true, "for": true,
	"from": true, "with": true, "into": true, "about": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "how": true, "what": true,
	"where": true, "when": true, "which": true, "who": true, "why": true,
	"this": true, "that": true, "these": true, "those": true,
}

// SearchTerms splits query into accent-folded lowercase terms, dropping only
// common filler stopwords while keeping domain acronyms ("it", "hr", "qa", "ti")
// and numbers ("2021").
func SearchTerms(query string) []string {
	folded := FoldAccents(strings.ToLower(query))
	fields := strings.FieldsFunc(folded, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		if searchStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return fields
	}
	return out
}

func countTermMatches(foldedText string, terms []string) int {
	matches := 0
	for _, t := range terms {
		if t == "" {
			continue
		}
		if len([]rune(t)) <= 3 {
			if containsWholeWord(foldedText, t) {
				matches++
			}
			continue
		}
		st := graph.Stem(t)
		if strings.Contains(foldedText, t) || (len([]rune(st)) >= 4 && strings.Contains(foldedText, st)) {
			matches++
		}
	}
	return matches
}

func containsWholeWord(text, term string) bool {
	tLen := len(term)
	for i := 0; i <= len(text)-tLen; {
		idx := strings.Index(text[i:], term)
		if idx < 0 {
			return false
		}
		pos := i + idx
		leftOK := pos == 0 || !isAlphaNum(rune(text[pos-1]))
		end := pos + tLen
		rightOK := end == len(text) || !isAlphaNum(rune(text[end]))
		if leftOK && rightOK {
			return true
		}
		i = pos + 1
	}
	return false
}

func isAlphaNum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// FoldAccents maps common Latin diacritics to ASCII equivalents so Spanish/English
// lexical matching treats "adquisición" and "adquisicion" equivalently.
func FoldAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'á', 'à', 'ä', 'â', 'ã':
			b.WriteByte('a')
		case 'é', 'è', 'ë', 'ê':
			b.WriteByte('e')
		case 'í', 'ì', 'ï', 'î':
			b.WriteByte('i')
		case 'ó', 'ò', 'ö', 'ô', 'õ':
			b.WriteByte('o')
		case 'ú', 'ù', 'ü', 'û':
			b.WriteByte('u')
		case 'ñ':
			b.WriteByte('n')
		case 'ç':
			b.WriteByte('c')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MemoryStore is a thread-safe, in-memory cosine-similarity VectorStore.
type MemoryStore struct {
	mu     sync.RWMutex
	points map[string]VectorPoint
}

// NewMemoryStore creates an empty in-memory VectorStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{points: map[string]VectorPoint{}}
}

// Upsert normalizes and stores points by NodeID.
func (m *MemoryStore) Upsert(_ context.Context, points []VectorPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range points {
		vec := make([]float32, len(p.Vector))
		copy(vec, p.Vector)
		normalize(vec)
		p.Vector = vec
		m.points[p.NodeID] = p
	}
	return nil
}

// Delete removes node IDs from the store.
func (m *MemoryStore) Delete(nodeIDs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range nodeIDs {
		delete(m.points, id)
	}
}

// Hash returns the stored text hash for nodeID, or "" if absent.
func (m *MemoryStore) Hash(nodeID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.points[nodeID].Hash
}

// Search returns the top limit nodes by cosine similarity to vector.
func (m *MemoryStore) Search(_ context.Context, vector []float32, limit int, nodeType string) ([]VectorHit, error) {
	if limit <= 0 || len(vector) == 0 {
		return nil, nil
	}
	q := make([]float32, len(vector))
	copy(q, vector)
	normalize(q)

	m.mu.RLock()
	hits := make([]VectorHit, 0, len(m.points))
	for _, p := range m.points {
		if nodeType != "" && p.Type != nodeType {
			continue
		}
		if len(p.Vector) != len(q) {
			continue
		}
		var dot float32
		for i, x := range p.Vector {
			dot += x * q[i]
		}
		hits = append(hits, VectorHit{
			NodeID: p.NodeID,
			Score:  dot,
			Type:   p.Type,
			Name:   p.Name,
		})
	}
	m.mu.RUnlock()

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].NodeID < hits[j].NodeID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// IndexGraph embeds every node in g matching include (nil = all nodes) into vs,
// skipping unchanged nodes when vs is a *MemoryStore. Returns how many nodes
// were newly embedded.
func IndexGraph(ctx context.Context, g *graph.Graph, vs VectorStore, emb Embedder, include func(*graph.Node) bool) (int, error) {
	if g == nil || vs == nil || emb == nil {
		return 0, nil
	}
	mem, _ := vs.(*MemoryStore)

	var todoIDs, todoTexts, todoHashes []string
	for id, n := range g.Nodes {
		if include != nil && !include(n) {
			continue
		}
		txt := g.NodeText(id)
		h := textHash(txt)
		if mem != nil && mem.Hash(id) == h {
			continue
		}
		todoIDs = append(todoIDs, id)
		todoTexts = append(todoTexts, txt)
		todoHashes = append(todoHashes, h)
	}
	if len(todoIDs) == 0 {
		return 0, nil
	}

	const batch = 64
	var points []VectorPoint
	for i := 0; i < len(todoIDs); i += batch {
		end := min(i+batch, len(todoIDs))
		vecs, err := emb.Embed(ctx, todoTexts[i:end])
		if err != nil {
			return 0, err
		}
		for j, v := range vecs {
			id := todoIDs[i+j]
			n := g.Nodes[id]
			points = append(points, VectorPoint{
				NodeID: id,
				Type:   n.Type,
				Name:   n.Name,
				Text:   todoTexts[i+j],
				Hash:   todoHashes[i+j],
				Vector: v,
			})
		}
	}
	if err := vs.Upsert(ctx, points); err != nil {
		return 0, err
	}
	return len(points), nil
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 {
		return
	}
	for i := range v {
		v[i] /= n
	}
}
