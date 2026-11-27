package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// The coverage note must point at the missed caller, not at lookalikes:
// InsertEventParams is not InsertEvent, the definition and the wired caller
// are already accounted for, and a call-shaped production line outranks a
// test helper or a comment.
func TestCoverageNoteWholeWordAccountedAndCallFirst(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"db/events.go":        "package db\n\n// InsertEvent writes one row.\nfunc (q *Queries) InsertEvent(p InsertEventParams) error {\n\treturn nil\n}\n",
		"audit/chain.go":      "package audit\n\nfunc chainVersionOf(p db.InsertEventParams) string { return \"\" }\n",
		"audit/wired.go":      "package audit\n\nfunc wired(q *db.Queries) {\n\tq.InsertEvent(db.InsertEventParams{})\n}\n",
		"audit/audit.go":      "package audit\n\nfunc insert(q *db.Queries) error {\n\tp := db.InsertEventParams{}\n\tif err := q.InsertEvent(p); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n",
		"audit/audit_test.go": "package audit\n\nvar name = \"InsertEvent\"\n",
		"README.md":           "Call InsertEvent to write.\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New("codebase:" + dir)
	target := &graph.Node{ID: "func:db/events.go:InsertEvent:4", Type: graph.NodeFunction, Name: "InsertEvent",
		Attrs: map[string]string{"file": "db/events.go", "line_start": "4", "line_end": "6"}}
	caller := &graph.Node{ID: "func:audit/wired.go:wired:3", Type: graph.NodeFunction, Name: "wired",
		Attrs: map[string]string{"file": "audit/wired.go", "line_start": "3", "line_end": "5"}}
	g.AddNode(target)
	g.AddNode(caller)
	e := New(g, nil, nil)

	note := e.coverageNote(target, []*graph.Node{caller})
	if !strings.Contains(note, "appears in 2 file(s)") {
		t.Fatalf("want 2 unaccounted files (audit.go call, the test string), got:\n%s", note)
	}
	lines := strings.Split(strings.TrimSpace(note), "\n")
	if first := strings.TrimSpace(lines[1]); !strings.HasPrefix(first, "audit/audit.go:5:") {
		t.Errorf("the missed call (audit/audit.go:5) must be listed first, got %q", first)
	}
	for _, bad := range []string{"chain.go", "wired.go", "events.go", "README.md"} {
		if strings.Contains(note, bad) {
			t.Errorf("%s is accounted for or a lookalike, must not be sampled:\n%s", bad, note)
		}
	}

	// Everything accounted for: no note at all.
	insertNode := &graph.Node{ID: "func:audit/audit.go:insert:3", Type: graph.NodeFunction, Name: "insert",
		Attrs: map[string]string{"file": "audit/audit.go", "line_start": "3", "line_end": "9"}}
	testFile := &graph.Node{ID: "file:" + dir + "/audit/audit_test.go", Type: graph.NodeFile, Name: "audit/audit_test.go"}
	if note := e.coverageNote(target, []*graph.Node{caller, insertNode, testFile}); note != "" {
		t.Errorf("all mentions accounted for, want no note, got:\n%s", note)
	}
}
