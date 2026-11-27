package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/internal/connector/docs"
	"github.com/system4us/graphAllTheThings/pkg/graph"
)

func TestDocContextPackWithSQLiteFTS(t *testing.T) {
	dir := t.TempDir()
	md := `# Política de Devoluciones

Las devoluciones deben solicitarse dentro de los 30 días corridos desde la compra.
El reembolso se acredita en la tarjeta original en un plazo de 5 días hábiles.

## Excepciones

Los productos digitales descargados no admiten devolución salvo falla técnica verificada.
`
	if err := os.WriteFile(filepath.Join(dir, "politica.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := docs.New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "graph.db")
	if err := g.Save(dbPath); err != nil {
		t.Fatalf("Save SQLite failed: %v", err)
	}
	loaded, err := graph.Load(dbPath)
	if err != nil {
		t.Fatalf("Load SQLite failed: %v", err)
	}

	e := New(loaded, nil, nil)
	e.FTS = func(q, typ string, limit int) []string {
		return graph.FTSQuery(dbPath, q, typ, limit)
	}

	out, err := e.DocContextPackWithBudget(context.Background(), "plazo reembolso devoluciones", 4, 2000)
	if err != nil {
		t.Fatalf("DocContextPackWithBudget failed: %v", err)
	}
	if !strings.Contains(out, "politica.md") {
		t.Errorf("expected file citation in output:\n%s", out)
	}
	if !strings.Contains(out, "5 días hábiles") {
		t.Errorf("expected matching passage in output:\n%s", out)
	}

	// Verify Render works for document nodes too.
	rendered, err := e.Render("doc:politica.md")
	if err != nil {
		t.Fatalf("Render document failed: %v", err)
	}
	if !strings.Contains(rendered, "Excepciones") {
		t.Errorf("expected section listing in rendered document:\n%s", rendered)
	}
}

func TestStripChunkOverlap(t *testing.T) {
	prev := "The system encrypts all database traffic using TLS 1.3 and rotates keys monthly."
	next := "using TLS 1.3 and rotates keys monthly. Administrators can also trigger manual rotation."
	got := stripChunkOverlap(prev, next)
	want := "Administrators can also trigger manual rotation."
	if got != want {
		t.Fatalf("stripChunkOverlap:\n got: %q\nwant: %q", got, want)
	}
}

func TestDocContextCodebaseFallback(t *testing.T) {
	g := graph.New("codebase:/tmp/myrepo")
	g.AddNode(&graph.Node{
		ID:   "file:docs/DEPLOYMENT.md",
		Type: graph.NodeFile,
		Name: "(doc)",
		Attrs: map[string]string{
			"file":     "docs/DEPLOYMENT.md",
			"doc":      "true",
			"doc_body": "# Deployment\n\nDeploy Intercloak as a reverse proxy.",
		},
	})
	g.AddNode(&graph.Node{
		ID:   "file:HANDOFF.md",
		Type: graph.NodeFile,
		Name: "(doc)",
		Attrs: map[string]string{
			"file":     "HANDOFF.md",
			"doc":      "true",
			"doc_body": "# Handoff\n\nOld audit notes from sprint 1.",
		},
	})
	g.AddNode(&graph.Node{
		ID:   "file:docs/AUDIT.md",
		Type: graph.NodeFile,
		Name: "(doc)",
		Attrs: map[string]string{
			"file":     "docs/AUDIT.md",
			"doc":      "true",
			"doc_body": "# Audit\n\nAudit logs record every masked tool_result with 30 days retention.",
		},
	})
	e := New(g, nil, nil)
	out, err := e.DocContextPackWithBudget(context.Background(), "audit logs retention", 4, 2000)
	if err != nil {
		t.Fatalf("DocContextPackWithBudget failed: %v", err)
	}
	if strings.Contains(out, "(doc)") {
		t.Errorf("expected real file paths instead of (doc), got:\n%s", out)
	}
	if strings.Contains(out, "Summary: true") {
		t.Errorf("expected no Summary: true stub, got:\n%s", out)
	}
	auditIdx := strings.Index(out, "docs/AUDIT.md")
	handoffIdx := strings.Index(out, "HANDOFF.md")
	if auditIdx < 0 || (handoffIdx >= 0 && auditIdx > handoffIdx) {
		t.Errorf("expected docs/AUDIT.md ranked above HANDOFF.md, got:\n%s", out)
	}
}

func TestAnalyzeDocConfidencePartialTermMatch(t *testing.T) {
	g := graph.New("docs:/tmp/corpus")
	g.AddNode(&graph.Node{
		ID:   "chunk:aws.md:1:1",
		Type: graph.NodeChunk,
		Name: "aws.md",
		Attrs: map[string]string{
			"file":    "aws.md",
			"section": "AWS Services",
			"text":    "AWS EC2, S3, and RDS are supported deployment targets.",
		},
	})
	hits := []FindHit{{ID: "chunk:aws.md:1:1", Score: 0.62}}
	terms := docContentTerms("contactos de AWS")
	conf, missing := analyzeDocConfidence(g, hits, terms, "hybrid", 5)
	if conf != "low" {
		t.Fatalf("expected confidence=low when only 'aws' matched and 'contactos' is missing, got %q", conf)
	}
	if len(missing) != 1 || missing[0] != "contactos" {
		t.Fatalf("expected missing=[contactos], got %v", missing)
	}

	// Spanish stopwords ("las", "por") must never false-match inside "class"/"support".
	g2 := graph.New("docs:/tmp/corpus")
	g2.AddNode(&graph.Node{
		ID:   "chunk:cpp.md:1:1",
		Type: graph.NodeChunk,
		Name: "cpp.md",
		Attrs: map[string]string{
			"file":    "cpp.md",
			"section": "Memory",
			"text":    "C++ class memory support and export allocator.",
		},
	})
	conf2, _ := analyzeDocConfidence(g2, []FindHit{{ID: "chunk:cpp.md:1:1", Score: 1.0}}, docContentTerms("las notificaciones no funcionan"), "keyword", 5)
	if conf2 != "low" {
		t.Fatalf("expected confidence=low for unrelated C++ doc on Spanish query, got %q", conf2)
	}
}

func TestDocGuidePackFullCanonicalAndVariants(t *testing.T) {
	dir := t.TempDir()
	tokenDoc := `# Slack Subscriber (token)

## Essence

Only channels are supported, not direct messages.

## Configuration

Step 1: Obtain token.
!clipboard-20210412.png!

Step 2: Configuration > Subscribers > Add Server.
`
	directDoc := `# Slack Subscriber (direct)

Direct webhook setup for Slack.
`
	if err := os.WriteFile(filepath.Join(dir, "slack_token.md"), []byte(tokenDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slack_direct.md"), []byte(directDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := docs.New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	e := New(g, nil, nil)
	out, err := e.DocGuidePackWithBudget(context.Background(), "configure slack token subscriber", 1, 4000)
	if err != nil {
		t.Fatalf("DocGuidePackWithBudget failed: %v", err)
	}
	if !strings.Contains(out, "Canonical Source 1: slack_token.md") {
		t.Errorf("expected slack_token.md as canonical source 1:\n%s", out)
	}
	if !strings.Contains(out, "Only channels are supported") {
		t.Errorf("expected full Essence caveat included in canonical guide:\n%s", out)
	}
	if !strings.Contains(out, "Screenshots referenced: clipboard-20210412.png") {
		t.Errorf("expected screenshot metadata in guide pack:\n%s", out)
	}
	if !strings.Contains(out, "Related / Variant Documents") || !strings.Contains(out, "slack_direct.md") {
		t.Errorf("expected slack_direct.md listed as related variant:\n%s", out)
	}
}

func TestDocGuideLargeManualCenteredWindow(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("# Giant User Guide\n\n")
	for i := 1; i <= 25; i++ {
		if i == 18 {
			sb.WriteString("Section 4.3.2 Creating a Private CA:\nopenssl genrsa -out datasunrise_gui.key 2048\nopenssl req -new -key datasunrise_gui.key -out datasunrise_gui.csr\n\n")
		} else if i == 19 {
			sb.WriteString("Copy the key and certificate to appfirewall.pem:\ncat datasunrise_gui.key > /opt/datasunrise/appfirewall.pem\nchmod 600 /opt/datasunrise/appfirewall.pem\n\n")
		} else {
			sb.WriteString(strings.Repeat("Generic filler chapter text about database monitoring and audit trails. ", 8) + "\n\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manual.md"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AppFirewallCore_Params.md"), []byte("# AppFirewallCore Command Line Parameters\n\nAntlrTrace flag for debugging.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := docs.New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	e := New(g, nil, nil)
	out, err := e.DocGuidePackWithBudget(context.Background(), "appfirewall.pem", 1, 4000)
	if err != nil {
		t.Fatalf("DocGuidePackWithBudget failed: %v", err)
	}
	if !strings.Contains(out, "Canonical Source 1: manual.md") {
		t.Errorf("expected manual.md (containing exact literal appfirewall.pem) as Canonical Source 1:\n%s", out)
	}
	if !strings.Contains(out, "Large document: showing reading-order window") {
		t.Errorf("expected centered window header for large document:\n%s", out)
	}
	if !strings.Contains(out, "openssl genrsa -out datasunrise_gui.key") {
		t.Errorf("expected preceding OpenSSL command included in centered window:\n%s", out)
	}
	if !strings.Contains(out, "chmod 600 /opt/datasunrise/appfirewall.pem") {
		t.Errorf("expected appfirewall.pem command included in centered window:\n%s", out)
	}
}

func TestFilterOffTopicDocHits(t *testing.T) {
	g := graph.New("docs:/tmp/corpus")
	g.AddNode(&graph.Node{
		ID: "chunk:okta.md:1:1", Type: graph.NodeChunk,
		Attrs: map[string]string{"file": "okta.md", "text": "Configure OIDC SSO with Okta Client ID."},
	})
	g.AddNode(&graph.Node{
		ID: "chunk:okta.md:2:1", Type: graph.NodeChunk,
		Attrs: map[string]string{"file": "okta.md", "text": "Step 2: Paste the redirect URI in the admin console."},
	})
	g.AddNode(&graph.Node{
		ID: "chunk:rsa.md:1:1", Type: graph.NodeChunk,
		Attrs: map[string]string{"file": "rsa.md", "text": "Configure OIDC SSO with RSA SecurID token appliance."},
	})
	hits := []FindHit{
		{ID: "chunk:okta.md:1:1", Score: 0.80},
		{ID: "chunk:okta.md:2:1", Score: 0.70},
		{ID: "chunk:rsa.md:1:1", Score: 0.72},
	}
	filtered := filterOffTopicDocHits(g, hits, graph.QueryTerms("configure oidc sso okta"), "hybrid")
	if len(filtered) != 2 || filtered[0].ID != "chunk:okta.md:1:1" || filtered[1].ID != "chunk:okta.md:2:1" {
		t.Fatalf("expected RSA hit dropped and both Okta hits kept, got %+v", filtered)
	}
}
