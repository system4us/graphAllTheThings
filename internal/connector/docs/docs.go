// Package docs wraps pkg/docs with local filesystem discovery and .gattignore
// filtering for gatt's CLI and MCP server.
package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/system4us/graphAllTheThings/internal/codeutil"
	pkgdocs "github.com/system4us/graphAllTheThings/pkg/docs"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

type Connector struct {
	Root         string
	ChunkRunes   int
	OverlapRunes int
}

func New(root string) *Connector {
	return &Connector{
		Root:         root,
		ChunkRunes:   pkgdocs.DefaultChunkRunes,
		OverlapRunes: pkgdocs.DefaultOverlapRunes,
	}
}

func (c *Connector) Name() string { return "docs" }

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
		if !pkgdocs.IsSupportedExt(filepath.Ext(absRoot)) {
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
				if codeutil.SkipDir(d.Name(), path == absRoot) {
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
			if pkgdocs.IsSupportedExt(filepath.Ext(d.Name())) {
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
	for _, path := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		rel = filepath.ToSlash(rel)

		parsed, err := pkgdocs.ParseFile(ctx, path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "docs: skipping %s: %v\n", rel, err)
			continue
		}
		if len(parsed.Sections) == 0 {
			continue
		}

		attrs := map[string]string{
			"sha256": fileSHA256(path),
		}
		if st, err := os.Stat(path); err == nil {
			attrs["mtime"] = strconv.FormatInt(st.ModTime().Unix(), 10)
		}

		pkgdocs.AddToGraph(g, parsed, pkgdocs.DocOptions{
			Name:         rel,
			Attrs:        attrs,
			ChunkRunes:   c.ChunkRunes,
			OverlapRunes: c.OverlapRunes,
		})
	}

	return g, nil
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

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

// Re-export chunk helpers for internal/connector/docs/docs_test.go.
func splitChunks(text string, maxRunes, overlapRunes int) []string {
	return pkgdocs.SplitChunks(text, maxRunes, overlapRunes)
}

func isLowValueChunk(chunk string) bool {
	return pkgdocs.IsLowValueChunk(chunk)
}
