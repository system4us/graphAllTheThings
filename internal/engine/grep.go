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

// GrepOptions configures GrepWithOptions.
type GrepOptions struct {
	Regex            bool
	Limit            int      // >0 caps displayed hits; 0 = unlimited; <0 = default (50)
	Include          []string // glob patterns: when non-empty, only matching paths are scanned
	Exclude          []string // glob patterns: matching paths are skipped
	IncludeLockfiles bool     // false (default) skips common dependency lockfiles
}

// defaultLockfiles are skipped by grep unless IncludeLockfiles is true: they
// are huge generated files that drown real audit hits.
var defaultLockfiles = map[string]bool{
	"package-lock.json":    true,
	"npm-shrinkwrap.json":  true,
	"shrinkwrap.json":      true,
	"pnpm-lock.yaml":       true,
	"yarn.lock":            true,
	"bun.lock":             true,
	"bun.lockb":            true,
	"go.sum":               true,
	"cargo.lock":           true,
	"poetry.lock":          true,
	"pipfile.lock":         true,
	"uv.lock":              true,
	"composer.lock":        true,
	"gemfile.lock":         true,
	"flake.lock":           true,
	"pubspec.lock":         true,
	"mix.lock":             true,
	"pods.lock":            true,
	"podfile.lock":         true,
	"packages.lock.json":   true,
	"paket.lock":           true,
	"gradle.lockfile":      true,
}

func isLockfile(rel string) bool {
	return defaultLockfiles[strings.ToLower(filepath.Base(rel))]
}

// matchGlob reports whether slash-normalized path rel matches pattern.
// Supports filepath.Match against either the full relative path or basename,
// plus simple "**" directory globs (e.g. "docs/**", "**/*_test.go", "a/**/b.ts").
func matchGlob(rel, pattern string) bool {
	rel = filepath.ToSlash(rel)
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "**") {
		if ok, _ := filepath.Match(pattern, rel); ok {
			return true
		}
		if !strings.Contains(pattern, "/") {
			if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
				return true
			}
		}
		return false
	}
	// Convert glob with ** into an anchored regex.
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		if i+1 < len(pattern) && pattern[i] == '*' && pattern[i+1] == '*' {
			if i+2 < len(pattern) && pattern[i+2] == '/' {
				re.WriteString("(?:.*/)?")
				i += 2
			} else {
				re.WriteString(".*")
				i++
			}
			continue
		}
		switch pattern[i] {
		case '*':
			re.WriteString("[^/]*")
		case '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	re.WriteString("$")
	matched, _ := regexp.MatchString(re.String(), rel)
	return matched
}

func matchesAnyGlob(rel string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlob(rel, p) {
			return true
		}
	}
	return false
}

// GrepResult is the structured result of GrepWithOptions.
type GrepResult struct {
	Text         string
	Hits         []GrepHit
	Total        int
	Shown        int
	FilesScanned int
	Truncated    bool
}

// Grep is an exhaustive literal (or regex) search over every tracked/
// non-ignored text file in the codebase root — independent of the indexed
// extension set, so it also covers files the connector doesn't parse (config
// files, etc). Unlike Find (semantic/top-N), the walk visits every such file
// using the same exclusion rules as extraction (graph.SkipDir, plus the
// repo's own .gitignore via codebase.GitFileSet when it's a git checkout),
// so a zero-result answer is a reliable proof of absence — over the actual
// codebase, not diluted by vendored/generated/build-output noise.
//
// Pass limit == 0 for unlimited results; negative limit defaults to 50.
func (e *Engine) Grep(pattern string, useRegex bool, limit int) (string, error) {
	res, err := e.GrepWithOptions(pattern, GrepOptions{Regex: useRegex, Limit: limit})
	return res.Text, err
}

// GrepWithOptions runs an exhaustive literal/regex scan with optional path
// include/exclude globs and lockfile filtering.
func (e *Engine) GrepWithOptions(pattern string, opts GrepOptions) (GrepResult, error) {
	if !e.IsCodebase() {
		return GrepResult{}, fmt.Errorf("grep needs a codebase graph")
	}
	if opts.Limit < 0 {
		opts.Limit = 50
	}
	hits, total, filesScanned, err := e.grepScan(pattern, opts)
	if err != nil {
		return GrepResult{}, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "exhaustive scan: %d file(s) scanned, %d match(es)\n", filesScanned, total)
	if total == 0 {
		b.WriteString("no matches — this string/pattern does not occur anywhere in the codebase\n")
		return GrepResult{Text: b.String(), Total: 0, Shown: 0, FilesScanned: filesScanned}, nil
	}
	truncated := total > len(hits)
	if truncated {
		fmt.Fprintf(&b, "(showing first %d of %d — pass --limit 0 for all)\n", len(hits), total)
	}
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, h.Text)
	}
	return GrepResult{
		Text:         b.String(),
		Hits:         hits,
		Total:        total,
		Shown:        len(hits),
		FilesScanned: filesScanned,
		Truncated:    truncated,
	}, nil
}

// grepScan is Grep's exhaustive walk-and-match core, split out so a caller
// that only needs a count (coverageNote, below) doesn't have to pay for
// building and parsing the formatted report Grep returns. Returns up to
// opts.Limit sample hits (0 = unlimited), the true total match count, and how
// many files were scanned.
func (e *Engine) grepScan(pattern string, opts GrepOptions) ([]GrepHit, int, int, error) {
	if !e.IsCodebase() {
		return nil, 0, 0, fmt.Errorf("grep needs a codebase graph")
	}
	dir := strings.TrimPrefix(e.G.Source, "codebase:")
	gitFiles, gitOK := codebase.GitFileSet(dir)

	var re *regexp.Regexp
	var needle string
	if opts.Regex {
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
		if !opts.IncludeLockfiles && isLockfile(rel) {
			return nil
		}
		if len(opts.Include) > 0 && !matchesAnyGlob(rel, opts.Include) {
			return nil
		}
		if len(opts.Exclude) > 0 && matchesAnyGlob(rel, opts.Exclude) {
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
			if opts.Regex {
				matched = re.MatchString(line)
			} else {
				matched = strings.Contains(strings.ToLower(line), needle)
			}
			if !matched {
				continue
			}
			total++
			if opts.Limit == 0 || len(hits) < opts.Limit {
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
