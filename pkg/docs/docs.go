// Package docs extracts structured sections and retrieval-sized chunks from
// documents (PDF, DOCX, Markdown, Textile, and plain text) and inserts them
// into a graph.Graph as document -> section -> chunk nodes connected by
// HAS_SECTION, HAS_CHUNK, and NEXT edges.
//
// All parsers operate directly on in-memory bytes (ParseBytes) so remote
// sources such as OneDrive, Google Drive, S3, or IMAP attachments can be
// ingested without writing temporary files to disk.
//
// This package is MIT-licensed (see ../LICENSE) and builds with CGO_ENABLED=0.
package docs

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

const (
	// DefaultChunkRunes targets ~100-120 tokens so small-window embedding models
	// (e.g. 128-token MiniLM) embed the full chunk without truncating its tail.
	DefaultChunkRunes   = 480
	DefaultOverlapRunes = 80
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

// DocOptions configures how AddToGraph inserts a ParsedDoc into a Graph.
type DocOptions struct {
	// ID overrides the document node ID. Defaults to "doc:" + Name.
	ID string
	// Name is the logical path or display name (e.g. "IT/Compras 2021/facturas.pdf").
	// Defaults to parsed.Title.
	Name string
	// Attrs are additional attributes attached to the document node (e.g. web_url,
	// etag, onedrive_id, sha256, mtime).
	Attrs map[string]string
	// InheritAttrs are attributes copied onto the document node AND every section
	// and chunk node (e.g. {"folder": "it", "year": "2021", "department": "IT"})
	// so RBAC filters (AllowNode) and search work uniformly on chunks.
	InheritAttrs map[string]string
	// ChunkRunes overrides DefaultChunkRunes when > 0.
	ChunkRunes int
	// OverlapRunes overrides DefaultOverlapRunes when >= 0 and < ChunkRunes.
	OverlapRunes int
	// KeepLowValue disables filtering of boilerplate/TOC/image-only chunks.
	KeepLowValue bool
}

// IsSupportedExt reports whether a file extension is handled by ParseBytes.
func IsSupportedExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".pdf", ".docx", ".md", ".markdown", ".txt":
		return true
	}
	return false
}

// ParseFile reads path from disk and delegates to ParseBytes.
func ParseFile(ctx context.Context, path string) (*ParsedDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBytes(ctx, filepath.Base(path), "", data)
}

// ParseBytes parses raw document bytes in memory without touching disk.
// Format is detected from filename extension first, falling back to contentType
// (e.g. "application/pdf", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
// "text/markdown", "text/plain").
func ParseBytes(ctx context.Context, filename, contentType string, data []byte) (*ParsedDoc, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))
	ct := strings.ToLower(strings.TrimSpace(contentType))

	switch {
	case ext == ".pdf" || strings.Contains(ct, "application/pdf"):
		return parsePDFBytes(ctx, filename, data)
	case ext == ".docx" || strings.Contains(ct, "wordprocessingml"):
		return parseDOCXBytes(filename, data)
	case ext == ".md" || ext == ".markdown" || strings.Contains(ct, "text/markdown"):
		return parseMarkdownBytes(filename, "md", data)
	case ext == ".txt" || strings.HasPrefix(ct, "text/"):
		return parseMarkdownBytes(filename, "txt", data)
	default:
		return nil, fmt.Errorf("unsupported document format (filename=%q, contentType=%q)", filename, contentType)
	}
}

// AddToGraph inserts parsed as a document node with its section and chunk
// children (linked via HAS_SECTION, HAS_CHUNK, and NEXT) into an existing graph.
// Returns the created document node ID and the slice of created chunk node IDs.
func AddToGraph(g *graph.Graph, parsed *ParsedDoc, opts DocOptions) (string, []string) {
	if g == nil || parsed == nil || len(parsed.Sections) == 0 {
		return "", nil
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = strings.TrimSpace(parsed.Title)
	}
	if name == "" {
		name = "document"
	}
	title := strings.TrimSpace(parsed.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	}
	docID := strings.TrimSpace(opts.ID)
	if docID == "" {
		docID = "doc:" + name
	}

	chunkSize := opts.ChunkRunes
	if chunkSize <= 0 {
		chunkSize = DefaultChunkRunes
	}
	overlap := opts.OverlapRunes
	if opts.ChunkRunes <= 0 && overlap == 0 {
		overlap = DefaultOverlapRunes
	}
	if overlap < 0 || overlap >= chunkSize {
		overlap = DefaultOverlapRunes
		if overlap >= chunkSize {
			overlap = chunkSize / 6
		}
	}

	docAttrs := map[string]string{
		"file":   name,
		"title":  title,
		"format": parsed.Format,
	}
	if parsed.Pages > 0 {
		docAttrs["pages"] = strconv.Itoa(parsed.Pages)
	}
	maps.Copy(docAttrs, opts.InheritAttrs)
	maps.Copy(docAttrs, opts.Attrs)

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
		Name:  name,
		Attrs: docAttrs,
	})

	idStem := strings.TrimPrefix(docID, "doc:")
	var prevSecID, prevChunkID string
	var chunkIDs []string
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

		secID := fmt.Sprintf("sec:%s:%d", idStem, si+1)
		secAttrs := map[string]string{
			"file":    name,
			"title":   title,
			"section": secTitle,
			"format":  parsed.Format,
			"level":   strconv.Itoa(max(sec.Level, 1)),
		}
		maps.Copy(secAttrs, opts.InheritAttrs)
		if sec.PageStart > 0 {
			secAttrs["page_start"] = strconv.Itoa(sec.PageStart)
		}
		if sec.PageEnd > 0 {
			secAttrs["page_end"] = strconv.Itoa(sec.PageEnd)
		}
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

		chunks := SplitChunks(body, chunkSize, overlap)
		for ci, ch := range chunks {
			if !opts.KeepLowValue && IsLowValueChunk(ch) {
				continue
			}
			chunkTotal++
			chunkID := fmt.Sprintf("chunk:%s:%d:%d", idStem, si+1, ci+1)
			chunkAttrs := map[string]string{
				"file":        name,
				"title":       title,
				"section":     secTitle,
				"format":      parsed.Format,
				"chunk_index": strconv.Itoa(chunkTotal),
				"text":        ch,
			}
			maps.Copy(chunkAttrs, opts.InheritAttrs)
			if sec.PageStart > 0 {
				chunkAttrs["page_start"] = strconv.Itoa(sec.PageStart)
			}
			if sec.PageEnd > 0 {
				chunkAttrs["page_end"] = strconv.Itoa(sec.PageEnd)
			}
			g.AddNode(&graph.Node{
				ID:    chunkID,
				Type:  graph.NodeChunk,
				Name:  fmt.Sprintf("%s § %s #%d", filepath.Base(name), secTitle, ci+1),
				Attrs: chunkAttrs,
			})
			g.AddEdge(secID, chunkID, graph.EdgeHasChunk, nil)
			if prevChunkID != "" {
				g.AddEdge(prevChunkID, chunkID, graph.EdgeNext, nil)
			}
			prevChunkID = chunkID
			chunkIDs = append(chunkIDs, chunkID)
		}
	}

	docAttrs["chunks"] = strconv.Itoa(chunkTotal)
	if len(docImages) > 0 {
		docAttrs["images"] = strings.Join(docImages, ", ")
	}
	return docID, chunkIDs
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

// SplitChunks breaks text into overlapping rune-bounded chunks, preferring
// paragraph, sentence, newline, or whitespace boundaries so UTF-8 characters
// and words are never split mid-token.
func SplitChunks(text string, maxRunes, overlapRunes int) []string {
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

var (
	mdImageRe      = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)
	textileImageRe = regexp.MustCompile(`(?i)!(?:\{[^}]*\})?([^!\s]+\.(?:png|jpg|jpeg|gif|svg|webp))!`)
	tocMarkerRe    = regexp.MustCompile(`(?i)\{\{>?\s*toc\s*\}\}|\[toc\]`)
	mdLinkOnlyRe   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	bareURLRe      = regexp.MustCompile(`https?://\S+`)
	textileBlockRe = regexp.MustCompile(`(?m)^(?:bq|bc|p)\.\s+`)
)

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

// IsLowValueChunk reports whether chunk is boilerplate/noise (standalone image
// tags, horizontal rules, wiki TOC blocks, bare URLs, or fewer than 15
// alphanumeric characters of actual prose).
func IsLowValueChunk(chunk string) bool {
	var kept []string
	for _, line := range strings.Split(chunk, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.Trim(t, "-*_ =|:") == "" {
			continue
		}
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
