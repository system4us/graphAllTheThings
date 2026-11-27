package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/system4us/graphAllTheThings/pkg/graph"
)

func buildTestDOCXBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	coreXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"
  xmlns:dc="http://purl.org/dc/elements/1.1/">
  <dc:title>Compras IT 2021</dc:title>
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
      <w:r><w:t>Adquisición de Servidores</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>Orden de compra aprobada para infraestructura del departamento de tecnología.</w:t></w:r>
    </w:p>
    <w:tbl>
      <w:tr>
        <w:tc><w:p><w:r><w:t>Proveedor</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>Monto</w:t></w:r></w:p></w:tc>
      </w:tr>
      <w:tr>
        <w:tc><w:p><w:r><w:t>Dell</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>USD 45000</w:t></w:r></w:p></w:tc>
      </w:tr>
    </w:tbl>
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
	return buf.Bytes()
}

func buildTestPDFBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	offsets := make([]int, 0, 8)
	writeObj := func(num int, body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}

	buf.WriteString("%PDF-1.4\n")
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>")
	writeObj(4, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	s1 := "BT /F1 12 Tf 72 720 Td (1. COMPRAS 2021 IT) Tj 0 -20 Td (Renovacion de licencias y hardware de red.) Tj ET"
	writeObj(5, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s1), s1))

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)
	return buf.Bytes()
}

func TestParseBytesInMemoryAndAddToGraph(t *testing.T) {
	ctx := context.Background()
	g := graph.New("onedrive:acme")
	g.AddNode(&graph.Node{ID: "dept:it", Type: "department", Name: "IT", Attrs: map[string]string{"folder": "it"}})

	// 1. Parse DOCX entirely from memory and add with InheritAttrs (folder, year).
	docxParsed, err := ParseBytes(ctx, "compras-2021.docx", "", buildTestDOCXBytes(t))
	if err != nil {
		t.Fatalf("ParseBytes(docx) failed: %v", err)
	}
	if docxParsed.Title != "Compras IT 2021" {
		t.Fatalf("unexpected title: %q", docxParsed.Title)
	}
	docID, chunkIDs := AddToGraph(g, docxParsed, DocOptions{
		ID:   "onedrive:item-123",
		Name: "IT/2021/compras-2021.docx",
		Attrs: map[string]string{
			"web_url": "https://onedrive.example/item-123",
			"etag":    "v1",
		},
		InheritAttrs: map[string]string{
			"folder":     "it",
			"year":       "2021",
			"department": "IT",
		},
	})
	if docID != "onedrive:item-123" || len(chunkIDs) == 0 {
		t.Fatalf("AddToGraph returned docID=%q chunks=%v", docID, chunkIDs)
	}
	g.AddEdge("dept:it", docID, "owns", nil)

	// Inherited attrs must propagate to chunks so RBAC AllowNode works at chunk level.
	ch := g.Nodes[chunkIDs[0]]
	if ch == nil || ch.Attrs["folder"] != "it" || ch.Attrs["year"] != "2021" {
		t.Fatalf("chunk missing inherited attrs: %+v", ch)
	}
	if !strings.Contains(ch.Attrs["text"], "Dell | USD 45000") {
		t.Errorf("expected table text in chunk, got %q", ch.Attrs["text"])
	}

	// 2. Parse PDF entirely from memory (content-type detection even without extension).
	pdfParsed, err := ParseBytes(ctx, "remote-blob", "application/pdf", buildTestPDFBytes(t))
	if err != nil {
		t.Fatalf("ParseBytes(pdf) failed: %v", err)
	}
	if pdfParsed.Pages != 1 || len(pdfParsed.Sections) == 0 {
		t.Fatalf("unexpected pdf result: %+v", pdfParsed)
	}
	pdfDocID, pdfChunks := AddToGraph(g, pdfParsed, DocOptions{
		ID:           "onedrive:item-456",
		Name:         "IT/2021/licencias.pdf",
		InheritAttrs: map[string]string{"folder": "it", "year": "2021"},
	})
	if pdfDocID == "" || len(pdfChunks) == 0 {
		t.Fatalf("AddToGraph(pdf) returned docID=%q chunks=%v", pdfDocID, pdfChunks)
	}
	if !strings.Contains(g.Nodes[pdfChunks[0]].Attrs["text"], "licencias") {
		t.Errorf("expected pdf text in chunk, got %q", g.Nodes[pdfChunks[0]].Attrs["text"])
	}
}
