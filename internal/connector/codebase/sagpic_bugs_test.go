package codebase

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

func TestIssue13GoMonorepoCallsChiRoutesAndOpenAPIFetch(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		// Monorepo layout: go.mod lives in api/go.mod (not repo root).
		"api/go.mod": "module sagpic/api\n\ngo 1.25\n",
		"api/internal/localization/timezone.go": `package localization

func ParseDay(s string) string { return s }
`,
		"api/internal/platform/db/db.go": `package db

type DB struct{}

func (d *DB) InTenantMethod(fn func()) { fn() }
func InTenant(fn func()) { fn() }
`,
		"api/internal/tpv/tpv.go": `package tpv

import (
	"sagpic/api/internal/localization"
	database "sagpic/api/internal/platform/db"
)

type Handler struct {
	DB *database.DB
}

func (h *Handler) Routes(r chiRouter) {
	r.Route("/cajas/{id}", func(r chiRouter) {
		r.Get("/turno-actual", h.currentShift)
		r.With(admin).Put("/", h.updateRegister)
	})
	r.Post("/ventas", h.createSale)
	r.Post("/ventas/{id}/anular", h.voidSale)
}

func (h *Handler) UseCrossPkg() {
	_ = localization.ParseDay("2026-01-01")
	database.InTenant(func() {})
	h.DB.InTenantMethod(func() {})
}
`,
		"api/internal/tpv/ventas.go": `package tpv

func (h *Handler) createSale()     {}
func (h *Handler) voidSale()       {}
func (h *Handler) currentShift()   {}
func (h *Handler) updateRegister() {}
`,
		// Test router must NOT be emitted as a production route.
		"api/internal/tpv/tpv_test.go": `package tpv

func TestRoutes() {
	r.Post("/test-only-route", h.createSale)
}
`,
		// Frontend with colliding createSale helper AND openapi-fetch uppercase api.POST.
		"web/src/VerTiquetsModal.tsx": `export function createSale() {}

export async function anularTicket(id: string) {
	await api.POST("/ventas/{id}/anular", { params: { path: { id } } });
}
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g, err := New(root).Extract(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Helper: check if any CALLS edge points to a function matching name & file.
	hasCaller := func(targetFile, targetName, callerName string) bool {
		for _, e := range g.Edges {
			if e.Type != graph.EdgeCalls {
				continue
			}
			from, to := g.Nodes[e.From], g.Nodes[e.To]
			if from != nil && to != nil && from.Name == callerName && to.Name == targetName && to.Attrs["file"] == targetFile {
				return true
			}
		}
		return false
	}

	// Bug 1: cross-package Go calls (localization.ParseDay) + aliased package call (database.InTenant) + struct field method call (h.DB.InTenantMethod).
	if !hasCaller("api/internal/localization/timezone.go", "ParseDay", "UseCrossPkg") {
		t.Error("Bug 1: missing CALLS edge UseCrossPkg -> localization.ParseDay")
	}
	if !hasCaller("api/internal/platform/db/db.go", "InTenant", "UseCrossPkg") {
		t.Error("Bug 1: missing CALLS edge UseCrossPkg -> db.InTenant")
	}
	if !hasCaller("api/internal/platform/db/db.go", "InTenantMethod", "UseCrossPkg") {
		t.Error("Bug 1: missing CALLS edge UseCrossPkg -> (*DB).InTenantMethod via h.DB.InTenantMethod")
	}

	// Collect routes by "METHOD path".
	routesByName := map[string]*graph.Node{}
	for _, n := range g.Nodes {
		if n.Type == graph.NodeRoute {
			routesByName[n.Name] = n
			if strings.HasSuffix(n.Attrs["file"], "_test.go") {
				t.Errorf("Bug 3: test router %q from %s should be skipped", n.Name, n.Attrs["file"])
			}
		}
	}

	// Bug 2: chi r.Route("/cajas/{id}", ...) prefix preserved.
	if routesByName["GET /cajas/:param/turno-actual"] == nil {
		t.Errorf("Bug 2: missing GET /cajas/:param/turno-actual; got routes: %v", routeNames(routesByName))
	}
	if routesByName["PUT /cajas/:param"] == nil {
		t.Errorf("Bug 2: missing PUT /cajas/:param; got routes: %v", routeNames(routesByName))
	}

	// Bug 3: POST /ventas resolves to api/internal/tpv/ventas.go:createSale despite web/src createSale collision.
	postVentas := routesByName["POST /ventas"]
	if postVentas == nil {
		t.Fatalf("missing POST /ventas route")
	}
	handlerFound := false
	for _, e := range g.EdgesOf(postVentas.ID) {
		if e.Type == graph.EdgeHandledBy && e.From == postVentas.ID {
			if hn := g.Nodes[e.To]; hn != nil && hn.Name == "createSale" && hn.Attrs["file"] == "api/internal/tpv/ventas.go" {
				handlerFound = true
			}
		}
	}
	if !handlerFound {
		t.Error("Bug 3: POST /ventas handler not wired to api/internal/tpv/ventas.go:createSale")
	}

	// Bug 4: openapi-fetch uppercase api.POST("/ventas/{id}/anular", ...) wired to POST /ventas/:param/anular.
	postAnular := routesByName["POST /ventas/:param/anular"]
	if postAnular == nil {
		t.Fatalf("missing POST /ventas/:param/anular route")
	}
	clientFound := false
	for _, e := range g.EdgesOf(postAnular.ID) {
		if e.Type == graph.EdgeCallsEndpoint && e.To == postAnular.ID {
			if cn := g.Nodes[e.From]; cn != nil && cn.Name == "anularTicket" {
				clientFound = true
			}
		}
	}
	if !clientFound {
		t.Error("Bug 4: openapi-fetch api.POST('/ventas/{id}/anular') not wired to POST /ventas/:param/anular")
	}
}

func routeNames(m map[string]*graph.Node) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
