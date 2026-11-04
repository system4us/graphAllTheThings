package codebase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"graphallthethings/internal/graph"
)

// scanFiles (drift detection) and parseFiles (extraction) must agree on which
// files exist, or HasDrift reports a change on every single query.
func TestDriftScanAgreesWithExtraction(t *testing.T) {
	tmp := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("a.go", "package a\nfunc A() {}\n")
	w("notes.txt", "plain text\n")
	w("tpl.vue", "<template></template>\n<script>export function f() {}</script>\n")
	w("data.json", "{}\n")
	w("dir/real/inner.go", "package real\nfunc B() {}\n")
	mustLink := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(tmp, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	mustLink("notes.txt", "link_to_file")       // resolves to a regular file: readable
	mustLink("dir/real", "link_to_dir")         // a directory: unreadable as a file
	mustLink("does/not/exist", "dangling_link") // broken target

	c := New(tmp)
	g, err := c.Extract(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cur := c.scanFiles()
	prev := graph.FileMtimes(g)
	for rel := range cur {
		if _, ok := prev[rel]; !ok {
			t.Errorf("scanFiles counts %q but extraction recorded no node for it (perpetual false drift)", rel)
		}
	}
	for rel := range prev {
		if _, ok := cur[rel]; !ok {
			t.Errorf("extraction recorded %q but scanFiles does not see it (perpetual false drift)", rel)
		}
	}
	if c.HasDrift(prev) {
		t.Error("HasDrift is true right after a fresh extraction")
	}
	if _, ok := cur["link_to_file"]; !ok {
		t.Error("a symlink to a regular file should still be indexed")
	}
}
