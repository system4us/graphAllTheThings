package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

// A "how does X get verified" question must answer with the production code
// that verifies X: question words (how, does, get) never match by name, word
// forms meet (verified ~ Verify), tests come after production code, and a
// token budget keeps functions rather than cutting them off.
func TestCodeContextPackRanking(t *testing.T) {
	g := graph.New("codebase:/tmp/fake")
	fn := func(id, name, file, doc string) {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, Attrs: map[string]string{
			"file": file, "line_start": "1", "line_end": "2", "signature": name + "()", "doc": doc,
			"body": strings.Repeat("// body line\n", 20)}})
	}
	fn("func:audit/verify.go:Verify:10", "Verify", "audit/verify.go", "Verify checks the audit hash chain end to end.")
	fn("func:audit/verify.go:verifyEvents:40", "verifyEvents", "audit/verify.go", "verifyEvents recomputes each event hash of the chain.")
	for i := 0; i < 8; i++ {
		fn(fmt.Sprintf("func:audit/verify_test.go:TestVerifyChain%d:%d", i, i+1), fmt.Sprintf("TestVerifyChain%d", i),
			"audit/verify_test.go", "the audit hash chain is verified")
	}
	g.AddNode(&graph.Node{ID: "def:web/store.test.ts:get", Type: graph.NodeDefinition, Name: "get",
		Attrs: map[string]string{"file": "web/store.test.ts", "line_start": "3", "kind": "const"}})
	e := New(g, nil, nil)

	pack, err := e.CodeContextPack(context.Background(), "how does the audit hash chain get verified", 6)
	if err != nil {
		t.Fatal(err)
	}
	iVerify := strings.Index(pack, "## func Verify(")
	iTest := strings.Index(pack, "## func TestVerifyChain")
	if iVerify < 0 || !strings.Contains(pack, "## func verifyEvents(") {
		t.Fatalf("production verifiers missing:\n%s", pack)
	}
	if iTest >= 0 && iTest < iVerify {
		t.Errorf("tests must come after production code:\n%s", pack)
	}
	if strings.Contains(pack, "const get") {
		t.Errorf("the question word \"get\" must not pull in a const named get:\n%s", pack)
	}

	budgeted, err := e.CodeContextPackWithBudget(context.Background(), "how does the audit hash chain get verified", 6, 150)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(budgeted, "## func Verify(") && !strings.Contains(budgeted, "## func verifyEvents(") {
		t.Errorf("a budget must keep the best functions:\n%s", budgeted)
	}
	if !strings.Contains(budgeted, "left out to fit") {
		t.Errorf("a budgeted pack must say what it left out:\n%s", budgeted)
	}
}

func TestQueryTermsAndStem(t *testing.T) {
	if got := strings.Join(graph.QueryTerms("How does the audit chain get verified?"), " "); got != "audit chain verified" {
		t.Errorf("QueryTerms = %q", got)
	}
	if got := strings.Join(graph.QueryTerms("dónde se bloquea un proveedor"), " "); got != "bloquea proveedor" {
		t.Errorf("QueryTerms (es) = %q", got)
	}
	if got := strings.Join(graph.QueryTerms("get"), " "); got != "get" {
		t.Errorf("all-stopword query must keep its words, got %q", got)
	}
	for in, want := range map[string]string{"verified": "verif", "verify": "verif", "masking": "mask", "key": "key", "policies": "polic"} {
		if got := graph.Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
}
