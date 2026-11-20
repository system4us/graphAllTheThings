package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"graphallthethings/internal/graph"
)

const (
	defaultReadSymbolsBudget = 4000
	maxReadSymbolLines       = 250
)

// ReadSymbols reads the full source body (with line numbers) of one or more
// codebase symbols in a single batch call, plus any persisted annotations
// (entity_note, gotcha, invariant). Trims cleanly to maxTokens (default 4000).
func (e *Engine) ReadSymbols(ids []string, maxTokens int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("read_symbols requires a codebase graph")
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("ids must contain at least one symbol name or node id")
	}
	if maxTokens <= 0 {
		maxTokens = defaultReadSymbolsBudget
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")

	var b strings.Builder
	for i, raw := range ids {
		if i > 0 {
			b.WriteString("\n")
		}
		n, err := e.resolveTraceNode(raw)
		if err != nil {
			fmt.Fprintf(&b, "## %s\n(error: %v)\n", raw, err)
			continue
		}
		file := n.Attrs["file"]
		if file == "" && n.Type == graph.NodeFile {
			file = n.Name
		}
		ls, _ := strconv.Atoi(n.Attrs["line_start"])
		le, _ := strconv.Atoi(n.Attrs["line_end"])
		if ls <= 0 {
			ls = 1
		}
		if le < ls {
			le = ls + 40
		}
		if le-ls+1 > maxReadSymbolLines {
			le = ls + maxReadSymbolLines - 1
		}

		fmt.Fprintf(&b, "## %s [%s] (%s:%d-%d)\n", n.Name, n.Type, file, ls, le)
		if note := n.Attrs["entity_note"]; note != "" {
			fmt.Fprintf(&b, "note: %s\n", note)
		}
		if inv := n.Attrs["invariant"]; inv != "" {
			fmt.Fprintf(&b, "invariant: %s\n", inv)
		}
		if g := n.Attrs["gotcha"]; g != "" {
			fmt.Fprintf(&b, "gotcha: %s\n", g)
		}
		if file == "" || n.Attrs["external"] == "true" {
			b.WriteString("(external or no source file)\n")
			continue
		}
		lines, truncated, err := readLineRange(filepath.Join(dir, filepath.FromSlash(file)), ls, le)
		if err != nil {
			fmt.Fprintf(&b, "(could not read %s: %v)\n", file, err)
			continue
		}
		b.WriteString("```\n")
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		if truncated {
			b.WriteString("… (symbol truncated at line cap)\n")
		}
		b.WriteString("```\n")
	}
	return TrimToTokenBudget(b.String(), maxTokens), nil
}

func readLineRange(absPath string, start, end int) ([]string, bool, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	truncated := false
	for ln := 1; sc.Scan(); ln++ {
		if ln < start {
			continue
		}
		if ln > end {
			truncated = true
			break
		}
		out = append(out, fmt.Sprintf("%4d: %s", ln, sc.Text()))
	}
	if err := sc.Err(); err != nil {
		return out, truncated, err
	}
	return out, truncated, nil
}
