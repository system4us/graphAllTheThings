package engine

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"graphallthethings/internal/graph"
)

// maxSnippetLines caps on-demand disk reads so a single caller/blast list
// can't blow past the token budget these tools exist to save in the first
// place. Nodes with a stored "body" attr (short functions captured at
// extraction time, see codebase.go) skip the disk read entirely.
const maxSnippetLines = 15

// maxSnippetChars caps the same read by size, mirroring the 600-byte cap
// extraction uses when deciding whether to store a function's body inline.
const maxSnippetChars = 800

// snippetFor returns source text for a function node: its stored body if
// extraction captured one, otherwise an on-demand read of its line range
// from disk (capped at maxSnippetLines/maxSnippetChars). Returns "" when no
// content is available — external/synthetic nodes, missing file, or a
// non-codebase graph.
func (e *Engine) snippetFor(n *graph.Node) string {
	if body := n.Attrs["body"]; body != "" {
		return body
	}
	if !e.IsCodebase() || n.Attrs["external"] == "true" {
		return ""
	}
	file := n.Attrs["file"]
	lineStart, err := strconv.Atoi(n.Attrs["line_start"])
	if file == "" || err != nil || lineStart <= 0 {
		return ""
	}
	lineEnd, err := strconv.Atoi(n.Attrs["line_end"])
	if err != nil || lineEnd < lineStart {
		lineEnd = lineStart
	}
	readEnd := lineEnd
	truncated := false
	if readEnd-lineStart+1 > maxSnippetLines {
		readEnd = lineStart + maxSnippetLines - 1
		truncated = true
	}

	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		if ln < lineStart {
			continue
		}
		if ln > readEnd {
			break
		}
		lines = append(lines, sc.Text())
	}
	if len(lines) == 0 {
		return ""
	}
	text := strings.Join(lines, "\n")
	if len(text) > maxSnippetChars {
		text = text[:maxSnippetChars]
		truncated = true
	}
	if truncated {
		text += "\n… (truncated)"
	}
	return text
}

// indentSnippet prefixes every line of s with prefix, for embedding a
// multi-line snippet under a "  name (file:line)" reference line. Returns ""
// for an empty snippet.
func indentSnippet(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
