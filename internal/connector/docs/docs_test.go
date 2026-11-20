package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"graphallthethings/internal/graph"
)

// writeTestDOCX builds a valid minimal .docx archive with headings, paragraphs, and a table.
func writeTestDOCX(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	coreXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"
  xmlns:dc="http://purl.org/dc/elements/1.1/">
  <dc:title>Política de Facturación</dc:title>
</cp:coreProperties>`
	f1, err := zw.Create("docProps/core.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f1.Write([]byte(coreXML))

	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
      <w:r><w:t>Reglas de Retención</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Las facturas deben emitirse dentro de los 5 días hábiles posteriores al cierre del servicio.</w:t></w:r>
    </w:p>
    <w:tbl>
      <w:tr>
        <w:tc><w:p><w:r><w:t>Plan</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>Plazo</w:t></w:r></w:p></w:tc>
      </w:tr>
      <w:tr>
        <w:tc><w:p><w:r><w:t>Enterprise</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>30 días</w:t></w:r></w:p></w:tc>
      </w:tr>
    </w:tbl>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading2"/></w:pPr>
      <w:r><w:t>Penalidades</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>El retraso mayor a 15 días genera un recargo del 2% mensual.</w:t></w:r>
    </w:p>
  </w:body>
</w:document>`
	f2, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f2.Write([]byte(docXML))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTestPDF writes a valid minimal 2-page PDF readable by both pdftotext and github.com/dslipak/pdf.
func writeTestPDF(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	offsets := make([]int, 0, 8)
	writeObj := func(num int, body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}

	buf.WriteString("%PDF-1.4\n")
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 6 0 R >>")
	writeObj(4, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 7 0 R >>")
	writeObj(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	s1 := "BT /F1 12 Tf 72 720 Td (1. SLA OVERVIEW) Tj 0 -20 Td (Uptime target is 99.9 percent monthly.) Tj ET"
	writeObj(6, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s1), s1))

	s2 := "BT /F1 12 Tf 72 720 Td (2. INCIDENT RESPONSE) Tj 0 -20 Td (Critical incidents require response within 15 minutes.) Tj ET"
	writeObj(7, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s2), s2))

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractDocsMixedFormats(t *testing.T) {
	dir := t.TempDir()
	writeTestDOCX(t, filepath.Join(dir, "billing.docx"))
	writeTestPDF(t, filepath.Join(dir, "sla.pdf"))
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Architecture\n\nUses Qdrant and ONNX.\n\n## Security\n\nAll traffic is loopback only.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	if !g.IsDocs() {
		t.Fatalf("expected IsDocs()=true, source=%q", g.Source)
	}

	docs := g.NodesByType(graph.NodeDocument)
	if len(docs) != 3 {
		t.Fatalf("expected 3 document nodes, got %d", len(docs))
	}

	// Verify DOCX title, table extraction, and sections.
	docxNode := g.Nodes["doc:billing.docx"]
	if docxNode == nil {
		t.Fatal("missing doc:billing.docx")
	}
	if docxNode.Attrs["title"] != "Política de Facturación" {
		t.Errorf("unexpected docx title: %q", docxNode.Attrs["title"])
	}
	chunks := g.NodesByType(graph.NodeChunk)
	var foundTable, foundSLA, foundMD bool
	for _, ch := range chunks {
		txt := ch.Attrs["text"]
		if strings.Contains(txt, "Enterprise | 30 días") {
			foundTable = true
		}
		if strings.Contains(txt, "99.9 percent") && ch.Attrs["page_start"] == "1" {
			foundSLA = true
		}
		if strings.Contains(txt, "Qdrant and ONNX") {
			foundMD = true
		}
	}
	if !foundTable {
		t.Error("expected DOCX table content in chunks")
	}
	if !foundSLA {
		t.Error("expected PDF page 1 content with page_start=1 in chunks")
	}
	if !foundMD {
		t.Error("expected Markdown content in chunks")
	}

	// Verify NEXT edges exist between sequential chunks.
	nextEdges := 0
	for _, e := range g.Edges {
		if e.Type == graph.EdgeNext {
			nextEdges++
		}
	}
	if nextEdges == 0 {
		t.Error("expected NEXT edges between sections/chunks")
	}
}

func TestSplitChunksUTF8Safe(t *testing.T) {
	// Spanish text with accented characters and ñ to ensure rune-safe chunking.
	para := strings.Repeat("Información técnica sobre facturación electrónica y retención de impuestos en España. ", 15)
	chunks := splitChunks(para, 200, 40)
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c)) > 210 {
			t.Errorf("chunk %d exceeded max runes: %d", i, len([]rune(c)))
		}
	}
}

func TestParseTextileHeadingsInMarkdown(t *testing.T) {
	dir := t.TempDir()
	content := "h1. Redmine Wiki Guide\n\nIntro text.\n\nh2. High Availability Setup\n\nDeploy across two nodes.\n"
	if err := os.WriteFile(filepath.Join(dir, "wiki.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	secs := g.NodesByType(graph.NodeSection)
	if len(secs) != 2 {
		t.Fatalf("expected 2 textile sections, got %d", len(secs))
	}
	if g.Nodes["doc:wiki.md"].Attrs["title"] != "Redmine Wiki Guide" {
		t.Errorf("unexpected title: %q", g.Nodes["doc:wiki.md"].Attrs["title"])
	}
}

func TestIsLowValueChunk(t *testing.T) {
	cases := []struct {
		text string
		low  bool
	}{
		{"![diagram](img/arch.png)\n---", true},
		{"| !{height: 1299px; width: 908px;}maskMethods.jpg! |", true},
		{"{{TOC}}", true},
		{"https://redmine.example.com/issues/12345", true},
		{"- [Intro](#intro)\n- [Setup](#setup)\n- [FAQ](#faq)", true},
		{"> **Source:** https://example.com\n> **Component:** Wiki\n---", true},
		{"Dynamic Data Masking hides sensitive columns in real time without altering stored data.", false},
	}
	for _, tc := range cases {
		if got := isLowValueChunk(tc.text); got != tc.low {
			t.Errorf("isLowValueChunk(%q) = %v, want %v", tc.text, got, tc.low)
		}
	}
}

func TestGattIgnoreExcludesSensitiveDocs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gattignore"), []byte("# Sensitive docs\nAWS_Accounts.md\nprivate/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AWS_Accounts.md"), []byte("# AWS\n\nSecret keys here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "passwords.md"), []byte("# Passwords\n\nAdmin password.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public.md"), []byte("# Okta Guide\n\nConfigure OIDC SSO with Okta client ID and secret.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := New(dir).Extract(context.Background())
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	docs := g.NodesByType(graph.NodeDocument)
	if len(docs) != 1 || docs[0].Name != "public.md" {
		t.Fatalf("expected only public.md, got %d docs: %+v", len(docs), docs)
	}
}
