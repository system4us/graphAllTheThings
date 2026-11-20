package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"graphallthethings/internal/graph"
)

func TestMCPPromptsAndResources(t *testing.T) {
	dir := t.TempDir()
	graphPath := filepath.Join(dir, "graph.db")

	g := graph.New("codebase:" + dir)
	g.AddNode(&graph.Node{
		ID:   "func:app.go:Checkout:10",
		Type: graph.NodeFunction,
		Name: "Checkout",
		Attrs: map[string]string{
			"file":      "app.go",
			"invariant": "must run inside DB transaction",
			"gotcha":    "retries must pass idempotency key",
		},
	})
	if err := g.Save(graphPath); err != nil {
		t.Fatalf("Save graph: %v", err)
	}

	srv, err := New(Config{GraphPath: graphPath})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ct, st := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.MCPServer().Run(ctx, st)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	// 1. Prompts
	prompts, err := session.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	gotPrompts := map[string]bool{}
	for _, p := range prompts.Prompts {
		gotPrompts[p.Name] = true
	}
	for _, want := range []string{"plan-change", "debug-flow", "review-diff", "onboard-repo"} {
		if !gotPrompts[want] {
			t.Errorf("missing prompt %q", want)
		}
	}

	pr, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "debug-flow",
		Arguments: map[string]string{"symptom": "missing audit row", "from": "POST /checkout", "to": "events"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(pr.Messages) == 0 {
		t.Fatalf("expected non-empty prompt messages")
	}
	tc, ok := pr.Messages[0].Content.(*mcp.TextContent)
	if !ok || !strings.Contains(tc.Text, `trace_path(from="POST /checkout", to="events")`) {
		t.Errorf("unexpected prompt content: %#v", pr.Messages[0].Content)
	}

	// 2. Resources
	resources, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	gotRes := map[string]bool{}
	for _, r := range resources.Resources {
		gotRes[r.URI] = true
	}
	for _, want := range []string{"gatt://overview", "gatt://hotspots", "gatt://annotations", "gatt://routes", "gatt://models"} {
		if !gotRes[want] {
			t.Errorf("missing resource %q", want)
		}
	}

	ov, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gatt://overview"})
	if err != nil {
		t.Fatalf("ReadResource overview: %v", err)
	}
	if len(ov.Contents) == 0 || !strings.Contains(ov.Contents[0].Text, "function") {
		t.Errorf("unexpected overview resource: %+v", ov.Contents)
	}

	ann, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "gatt://annotations"})
	if err != nil {
		t.Fatalf("ReadResource annotations: %v", err)
	}
	if len(ann.Contents) == 0 || !strings.Contains(ann.Contents[0].Text, "must run inside DB transaction") || !strings.Contains(ann.Contents[0].Text, "idempotency key") {
		t.Errorf("unexpected annotations resource: %+v", ann.Contents)
	}
}
