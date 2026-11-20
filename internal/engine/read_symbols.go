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
// (entity_note, gotcha, invariant), within ~maxTokens (default 4000).
//
// The budget is spent whole symbol by whole symbol, in the order asked. A
// symbol that does not fit is never silently dropped: it is listed at the end
// with its file:line range so it can be asked for on its own (or with a larger
// max_tokens). Only a first symbol bigger than the whole budget is cut, and
// says so with the line to resume from.
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
	budget := maxTokens * 4 // chars, same estimate as TrimToTokenBudget

	var b strings.Builder
	var omitted []string
	for _, raw := range ids {
		section, loc := e.readSymbolSection(dir, raw)
		if b.Len() > 0 && b.Len()+1+len(section) > budget {
			omitted = append(omitted, loc)
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if b.Len() == 0 && len(section) > budget {
			section = trimSymbolSection(section, budget)
		}
		b.WriteString(section)
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n… not shown, over the ~%d token budget (ask for them separately or raise max_tokens):\n", maxTokens)
		for _, loc := range omitted {
			b.WriteString("  " + loc + "\n")
		}
	}
	return b.String(), nil
}

// readSymbolSection renders one symbol's section and returns it with a short
// "name (file:start-end)" locator for the not-shown list.
func (e *Engine) readSymbolSection(dir, raw string) (string, string) {
	var b strings.Builder
	n, err := e.resolveTraceNode(raw)
	if err != nil {
		fmt.Fprintf(&b, "## %s\n(error: %v)\n", raw, err)
		return b.String(), raw
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
	fullEnd := le
	capped := false
	if le-ls+1 > maxReadSymbolLines {
		le, capped = ls+maxReadSymbolLines-1, true
	}
	loc := fmt.Sprintf("%s (%s:%d-%d)", n.Name, file, ls, fullEnd)

	fmt.Fprintf(&b, "## %s [%s] (%s:%d-%d)\n", n.Name, n.Type, file, ls, fullEnd)
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
		return b.String(), loc
	}
	lines, err := readLineRange(filepath.Join(dir, filepath.FromSlash(file)), ls, le)
	if err != nil {
		fmt.Fprintf(&b, "(could not read %s: %v)\n", file, err)
		return b.String(), loc
	}
	b.WriteString("```\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if capped {
		fmt.Fprintf(&b, "… (cut at the %d-line cap; the symbol continues at %s:%d-%d)\n", maxReadSymbolLines, file, le+1, fullEnd)
	}
	b.WriteString("```\n")
	return b.String(), loc
}

// trimSymbolSection cuts one oversized section at a line boundary within
// budget chars, closing the code fence and naming the line to resume from.
func trimSymbolSection(section string, budget int) string {
	cut := strings.LastIndex(section[:budget], "\n")
	if cut <= 0 {
		cut = budget
	}
	head := section[:cut]
	resume := ""
	// Lines are rendered "%4d: text": the next line number is the resume point.
	if rest := strings.TrimLeft(section[cut:], "\n"); rest != "" {
		if colon := strings.Index(rest, ":"); colon > 0 {
			if ln, err := strconv.Atoi(strings.TrimSpace(rest[:colon])); err == nil {
				resume = fmt.Sprintf(" from line %d", ln)
			}
		}
	}
	if strings.Count(head, "```")%2 == 1 {
		head += "\n```"
	}
	return head + fmt.Sprintf("\n… (trimmed to the token budget; continue%s, or raise max_tokens)\n", resume)
}

// readLineRange returns lines start..end (1-based, inclusive) rendered as
// "%4d: text". A file shorter than end just yields fewer lines.
func readLineRange(absPath string, start, end int) ([]string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for ln := 1; sc.Scan(); ln++ {
		if ln < start {
			continue
		}
		if ln > end {
			break
		}
		out = append(out, fmt.Sprintf("%4d: %s", ln, sc.Text()))
	}
	return out, sc.Err()
}
