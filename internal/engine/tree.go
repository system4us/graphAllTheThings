package engine

import (
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

// treeEntry is one file under the synthesized tree, with the one-line
// annotation Tree prints next to it.
type treeEntry struct {
	relPath string
	summary string
}

// Tree renders a directory tree of the codebase, annotated with a one-line
// summary per file — the file's leading doc comment (attrs["file_doc"]), a
// markdown doc's title, or the doc of its earliest top-level function/type —
// so an agent can scan repo structure without an ls+Read round-trip per
// file. The graph has no directory nodes: the tree is synthesized purely
// from file node relative paths, the same way `tree`/`find` fold them.
// pathPrefix filters to files under that relative path ("" = whole repo);
// depth limits how many path segments deep to print (0 = unlimited).
func (e *Engine) Tree(pathPrefix string, depth int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("tree needs a codebase graph")
	}
	pathPrefix = strings.Trim(pathPrefix, "/")

	// parseMarkdown emits two NodeFile nodes per .md: the real file node
	// (Name = relPath, attrs["path"] set) and a synthetic "doc:" title
	// carrier (attrs["file"] set, no "path"). Index the latter by relPath so
	// the loop below can use it as an annotation for the former.
	docTitles := map[string]string{}
	for id, n := range e.G.Nodes {
		if n.Type == graph.NodeFile && strings.HasPrefix(id, "doc:") {
			docTitles[n.Attrs["file"]] = strings.TrimSuffix(n.Name, " (doc)")
		}
	}

	// Earliest-function doc per file, used as a fallback annotation when a
	// file has no leading comment or markdown title of its own.
	type funcDoc struct {
		doc  string
		line int
	}
	firstFuncDoc := map[string]funcDoc{}
	for _, n := range e.G.NodesByType(graph.NodeFunction) {
		if n.Attrs["external"] == "true" || n.Attrs["doc"] == "" {
			continue
		}
		f := n.Attrs["file"]
		if f == "" {
			continue
		}
		var line int
		fmt.Sscanf(n.Attrs["line_start"], "%d", &line)
		if cur, ok := firstFuncDoc[f]; !ok || line < cur.line {
			firstFuncDoc[f] = funcDoc{n.Attrs["doc"], line}
		}
	}

	var entries []treeEntry
	for _, n := range e.G.NodesByType(graph.NodeFile) {
		if n.Attrs["path"] == "" {
			continue // the synthetic doc-title node, not a real file
		}
		rel := n.Name
		if pathPrefix != "" && rel != pathPrefix && !strings.HasPrefix(rel, pathPrefix+"/") {
			continue
		}
		summary := n.Attrs["file_doc"]
		if summary == "" {
			summary = docTitles[rel]
		}
		if summary == "" {
			if d, ok := firstFuncDoc[rel]; ok {
				summary = d.doc
			}
		}
		entries = append(entries, treeEntry{relPath: rel, summary: summary})
	}
	if len(entries) == 0 {
		if pathPrefix == "" {
			return "no files in this codebase graph\n", nil
		}
		return fmt.Sprintf("no files under %q\n", pathPrefix), nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })

	var b strings.Builder
	truncatedDirs := map[string]bool{}
	for _, en := range entries {
		rel := strings.TrimPrefix(strings.TrimPrefix(en.relPath, pathPrefix), "/")
		segs := strings.Split(rel, "/")
		if depth > 0 && len(segs) > depth {
			// Beyond the depth cutoff: note the containing directory once
			// instead of silently dropping it (a dir with only deep files
			// would otherwise vanish from the tree entirely).
			dir := strings.Join(segs[:depth], "/")
			if !truncatedDirs[dir] {
				truncatedDirs[dir] = true
				fmt.Fprintf(&b, "%s/...\n", dir)
			}
			continue
		}
		indent := strings.Repeat("  ", len(segs)-1)
		line := indent + segs[len(segs)-1]
		if en.summary != "" {
			line += "  — " + truncate(en.summary, 100)
		}
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
