package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/graph"
)

// CodeDiff reports the structural diff between the working tree and a git
// ref (default HEAD): added/removed/changed/renamed/moved functions and
// types — detected by matching signatures across two extractions, not a
// textual diff — plus, for anything changed/renamed/moved, its current
// callers pulled from this engine's already-loaded graph ("who needs to
// look at this"). Needs a git checkout.
func (e *Engine) CodeDiff(ctx context.Context, ref string, limit int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("diff needs a codebase graph")
	}
	if ref == "" {
		ref = "HEAD"
	}
	if limit <= 0 {
		limit = 30
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")

	pairs, err := codebase.GitChangedFiles(ctx, dir, ref)
	if err != nil {
		return "", err
	}
	if len(pairs) == 0 {
		return fmt.Sprintf("no file changes since %s\n", ref), nil
	}
	renamed := map[string]string{}
	for _, p := range pairs {
		if p.Old != "" && p.New != "" && p.Old != p.New {
			renamed[p.Old] = p.New
		}
	}

	oldGraph, err := codebase.New(dir).ExtractAt(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("extracting %s: %w", ref, err)
	}

	d := graph.DiffCode(oldGraph, e.G, pairs, renamed)
	if d.Empty() {
		return fmt.Sprintf("%d file(s) touched since %s, no function/type signature or doc changed\n", len(pairs), ref), nil
	}

	total := len(d.Changes)
	shown := d.Changes
	if len(shown) > limit {
		shown = shown[:limit]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "structural diff vs %s: %d change(s)", ref, total)
	if total > len(shown) {
		fmt.Fprintf(&b, " (showing %d)", len(shown))
	}
	b.WriteString("\n")

	if len(d.FileRenames) > 0 {
		var olds []string
		for o := range d.FileRenames {
			olds = append(olds, o)
		}
		sort.Strings(olds)
		for _, o := range olds {
			fmt.Fprintf(&b, "file renamed: %s -> %s\n", o, d.FileRenames[o])
		}
	}
	for _, c := range shown {
		writeCodeChangeLine(&b, c)
		if c.NewID == "" {
			continue
		}
		if callers := e.currentCallers(c.NewID); len(callers) > 0 {
			fmt.Fprintf(&b, "    %d caller(s) may need review: %s\n", len(callers), strings.Join(callers, ", "))
		}
	}
	return b.String(), nil
}

func writeCodeChangeLine(b *strings.Builder, c graph.CodeChange) {
	switch c.Kind {
	case graph.CodeAdded:
		fmt.Fprintf(b, "+ %s %s (%s)\n", c.Type, c.NewName, c.NewFile)
	case graph.CodeRemoved:
		fmt.Fprintf(b, "- %s %s (%s)\n", c.Type, c.OldName, c.OldFile)
	case graph.CodeChanged:
		fmt.Fprintf(b, "~ %s %s (%s)\n", c.Type, c.NewName, c.NewFile)
	case graph.CodeRenamed:
		fmt.Fprintf(b, "-> %s renamed %s to %s (%s)\n", c.Type, c.OldName, c.NewName, c.NewFile)
	case graph.CodeMoved:
		fmt.Fprintf(b, "-> %s %s moved %s to %s\n", c.Type, c.NewName, c.OldFile, c.NewFile)
	}
}

// currentCallers returns up to 8 current callers of a function id, labeled
// with file:line, from this engine's live graph.
func (e *Engine) currentCallers(id string) []string {
	var names []string
	for _, ed := range e.G.EdgesOf(id) {
		if ed.Type != graph.EdgeCalls || ed.To != id {
			continue
		}
		on := e.G.Nodes[ed.From]
		if on == nil {
			continue
		}
		label := on.Name
		if f := on.Attrs["file"]; f != "" && on.Attrs["line_start"] != "" {
			label += fmt.Sprintf(" (%s:%s)", f, on.Attrs["line_start"])
		}
		names = append(names, label)
	}
	sort.Strings(names)
	if len(names) > 8 {
		names = names[:8]
	}
	return names
}
