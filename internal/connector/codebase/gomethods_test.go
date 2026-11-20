package codebase

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"graphallthethings/internal/graph"
)

// A value produced by a call (sqlc's s.q.WithTx(tx), atomic.Pointer.Load())
// hides its type from the receiver index; the Go method fallback wires the
// call when exactly one method of that name exists in the caller's package or
// the packages it imports — and stays silent in every ambiguous shape.
func TestGoValueMethodFallback(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"db/db.go": `package db

type Queries struct{}

func New() *Queries { return &Queries{} }

func (q *Queries) WithTx(tx int) *Queries { return q }

func (q *Queries) InsertEvent(x int) error { return nil }

func (q *Queries) Stats() int { return 0 }
`,
		"adapter/adapter.go": `package adapter

type X struct{}

func (x *X) PathEscape(s string) string { return s }

func (x *X) Close() error { return nil }

func (x *X) Stats() int { return 0 }
`,
		"adapter/adapter_test.go": `package adapter

type fakeConn struct{}

func (f *fakeConn) Drain() {}
`,
		"other/other.go": `package other

type O struct{}

func (o *O) Frobnicate() {}
`,
		"store/store.go": `package store

import (
	"net/url"

	"example.com/m/adapter"
	"example.com/m/db"
)

type Store struct{ q *db.Queries }

var _ = adapter.X{}

func (s *Store) Insert() error {
	q := s.q.WithTx(1)
	return q.InsertEvent(1)
}

func escape() string { return url.PathEscape("a b") }

func closeIt(f interface{ Close() error }) { f.Close() }

func drain(c interface{ Drain() }) { c.Drain() }

func frob(z interface{ Frobnicate() }) { z.Frobnicate() }

func stats(v interface{ Stats() int }) int { return v.Stats() }
`,
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	edge := func(from, to string) bool {
		for _, e := range g.Edges {
			if e.Type != graph.EdgeCalls {
				continue
			}
			fn, tn := g.Nodes[e.From], g.Nodes[e.To]
			if fn != nil && tn != nil && fn.Name == from && tn.Name == to {
				return true
			}
		}
		return false
	}
	for _, r := range [][2]string{{"Insert", "WithTx"}, {"Insert", "InsertEvent"}} {
		if !edge(r[0], r[1]) {
			t.Errorf("missing CALLS %s -> %s (sqlc value method)", r[0], r[1])
		}
	}
	wrong := []struct{ from, to, why string }{
		{"escape", "PathEscape", "url.PathEscape is a package call, not adapter.X's method"},
		{"closeIt", "Close", "Close is a method every stdlib type has"},
		{"drain", "Drain", "a method in another package's _test.go is not reachable"},
		{"frob", "Frobnicate", "package other is not imported by the caller"},
		{"stats", "Stats", "two imported packages declare Stats"},
	}
	for _, w := range wrong {
		if edge(w.from, w.to) {
			t.Errorf("unexpected CALLS %s -> %s: %s", w.from, w.to, w.why)
		}
	}
}
