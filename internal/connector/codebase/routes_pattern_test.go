package codebase

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"graphallthethings/internal/graph"
)

// Routes registered through a wrapper — a local closure taking
// (pattern, privilege, handler), an in-house TS router helper — were
// invisible because detection keyed on the call name. The "METHOD /path"
// literal itself is the evidence, in any language; test titles and log
// messages that merely mention a route are not registrations.
func TestPatternRoutesThroughWrappers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"admin/admin.go": `package admin

import "net/http"

type Server struct{}

const privAny = 1

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, priv int, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
	}
	handle("GET /api/status", privAny, s.status)
	handle("PUT /api/upstreams/{name}", privAny, s.putUpstream)
	return mux
}

func (s *Server) status(w http.ResponseWriter, r *http.Request)      {}
func (s *Server) putUpstream(w http.ResponseWriter, r *http.Request) {}

func logIt() { println("GET /api/status failed") }
`,
		"web/router.ts": `
export function listUsers(req: any, res: any) {}
export function register(router: any) {
	addRoute("GET /users", listUsers);
}
function addRoute(p: string, h: any) {}
`,
		"web/router.test.ts": `
it("GET /users", () => {});
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
	handlers := map[string]string{} // route name -> handler name
	var routes []string
	for _, n := range g.Nodes {
		if n.Type != graph.NodeRoute {
			continue
		}
		routes = append(routes, n.Name+" @"+n.Attrs["file"])
		for _, e := range g.EdgesOf(n.ID) {
			if e.From == n.ID && e.Type == graph.EdgeUsesMiddleware {
				t.Errorf("route %s: %s must not become middleware", n.Name, g.Nodes[e.To].Name)
			}
			if e.From == n.ID && e.Type == graph.EdgeHandledBy {
				handlers[n.Name] = g.Nodes[e.To].Name
			}
		}
	}
	sort.Strings(routes)
	want := map[string]string{
		"GET /api/status":           "status",
		"PUT /api/upstreams/:param": "putUpstream",
		"GET /users":                "listUsers",
	}
	for route, h := range want {
		if handlers[route] != h {
			t.Errorf("route %s: handler %q, want %q (routes: %v)", route, handlers[route], h, routes)
		}
	}
	if len(routes) != len(want) {
		t.Errorf("want exactly %d routes (no test titles, no log lines), got %v", len(want), routes)
	}
}
