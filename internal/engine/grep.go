package engine

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"graphallthethings/internal/connector/codebase"
	"graphallthethings/internal/graph"
)

// maxGrepFileSize skips files larger than this — matches the connector's own
// content-hash cap (internal/connector/codebase/codebase.go's contentHash),
// so grep and extraction agree on what counts as "too big to read".
const maxGrepFileSize = 2 << 20

// GrepHit is one matching line.
type GrepHit struct {
	Path string
	Line int
	Text string
}

// Grep is an exhaustive literal (or regex) search over every tracked/
// non-ignored text file in the codebase root — independent of the indexed
// extension set, so it also covers files the connector doesn't parse (config
// files, etc). Unlike Find (semantic/top-N), the walk visits every such file
// using the same exclusion rules as extraction (graph.SkipDir, plus the
// repo's own .gitignore via codebase.GitFileSet when it's a git checkout),
// so a zero-result answer is a reliable proof of absence — over the actual
// codebase, not diluted by vendored/generated/build-output noise.
func (e *Engine) Grep(pattern string, useRegex bool, limit int) (string, error) {
	if !e.IsCodebase() {
		return "", fmt.Errorf("grep needs a codebase graph")
	}
	if limit <= 0 {
		limit = 50
	}
	hits, total, filesScanned, err := e.grepScan(pattern, useRegex, limit)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "exhaustive scan: %d file(s) scanned, %d match(es)\n", filesScanned, total)
	if total == 0 {
		b.WriteString("no matches — this string/pattern does not occur anywhere in the codebase\n")
		return b.String(), nil
	}
	if total > len(hits) {
		fmt.Fprintf(&b, "(showing first %d of %d)\n", len(hits), total)
	}
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, h.Text)
	}
	return b.String(), nil
}

// grepScan is Grep's exhaustive walk-and-match core, split out so a caller
// that only needs a count (coverageNote, below) doesn't have to pay for
// building and parsing the formatted report Grep returns. Returns up to
// limit sample hits, the true total match count (uncapped), and how many
// files were scanned.
func (e *Engine) grepScan(pattern string, useRegex bool, limit int) ([]GrepHit, int, int, error) {
	if !e.IsCodebase() {
		return nil, 0, 0, fmt.Errorf("grep needs a codebase graph")
	}
	if limit <= 0 {
		limit = 50
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	gitFiles, gitOK := codebase.GitFileSet(dir)

	var re *regexp.Regexp
	var needle string
	if useRegex {
		compiled, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("invalid regex: %w", err)
		}
		re = compiled
	} else {
		needle = strings.ToLower(pattern)
	}

	var hits []GrepHit
	filesScanned, total := 0, 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if graph.SkipDir(d.Name(), path == dir) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if gitOK && !gitFiles[rel] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileSize || info.Size() == 0 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		head := make([]byte, 512)
		n, _ := f.Read(head)
		if isBinary(head[:n]) {
			return nil
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil
		}

		filesScanned++
		lineNo := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			matched := false
			if useRegex {
				matched = re.MatchString(line)
			} else {
				matched = strings.Contains(strings.ToLower(line), needle)
			}
			if !matched {
				continue
			}
			total++
			if len(hits) < limit {
				text := strings.TrimSpace(line)
				if len(text) > 200 {
					text = text[:200]
				}
				hits = append(hits, GrepHit{Path: rel, Line: lineNo, Text: text})
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, total, filesScanned, nil
}

// isBinary applies the same heuristic as most greps: a NUL byte anywhere in
// the first chunk means the file isn't text.
func isBinary(head []byte) bool {
	return slices.Contains(head, 0)
}
