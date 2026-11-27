package docs

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// parseDOCXBytes extracts structured sections (headings, paragraphs, and tables)
// from raw .docx bytes in memory using only Go's standard library.
func parseDOCXBytes(filename string, data []byte) (*ParsedDoc, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open docx zip: %w", err)
	}

	var docFile, coreFile *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			docFile = f
		case "docProps/core.xml":
			coreFile = f
		}
	}
	if docFile == nil {
		return nil, fmt.Errorf("word/document.xml not found in docx")
	}

	title := ""
	if coreFile != nil {
		title = readDOCXCoreTitle(coreFile)
	}

	rc, err := docFile.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	sections, inferredTitle, err := decodeDOCXBody(rc)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = inferredTitle
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	}

	return &ParsedDoc{
		Title:    title,
		Format:   "docx",
		Sections: sections,
	}, nil
}

func readDOCXCoreTitle(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()

	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "title" {
			var s string
			if err := dec.DecodeElement(&s, &se); err == nil {
				return strings.TrimSpace(s)
			}
		}
	}
}

func decodeDOCXBody(r io.Reader) ([]ParsedSection, string, error) {
	dec := xml.NewDecoder(r)

	var sections []ParsedSection
	var cur *ParsedSection
	var docTitle string

	flushBody := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if cur == nil {
			sections = append(sections, ParsedSection{Level: 1})
			cur = &sections[len(sections)-1]
		}
		if cur.Body != "" {
			cur.Body += "\n\n"
		}
		cur.Body += line
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "p":
			text, headingLevel, isTitle, err := readDOCXParagraph(dec)
			if err != nil {
				return nil, "", err
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if isTitle && docTitle == "" {
				docTitle = text
				sections = append(sections, ParsedSection{Title: text, Level: 1})
				cur = &sections[len(sections)-1]
				continue
			}
			if headingLevel > 0 {
				sections = append(sections, ParsedSection{Title: text, Level: headingLevel})
				cur = &sections[len(sections)-1]
				continue
			}
			flushBody(text)

		case "tbl":
			tblText, err := readDOCXTable(dec)
			if err != nil {
				return nil, "", err
			}
			flushBody(tblText)
		}
	}

	var out []ParsedSection
	for _, s := range sections {
		if strings.TrimSpace(s.Title) == "" && strings.TrimSpace(s.Body) == "" {
			continue
		}
		out = append(out, s)
	}
	return out, docTitle, nil
}

func readDOCXParagraph(dec *xml.Decoder) (text string, headingLevel int, isTitle bool, err error) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", 0, false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						lvl, title := parseDOCXStyleHeading(a.Value)
						if title {
							isTitle = true
						}
						if lvl > 0 && headingLevel == 0 {
							headingLevel = lvl
						}
					}
				}
			case "outlineLvl":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						if n, convErr := strconv.Atoi(a.Value); convErr == nil && n >= 0 && n <= 5 {
							headingLevel = n + 1
						}
					}
				}
			case "tab":
				b.WriteByte(' ')
			case "br":
				b.WriteByte('\n')
			case "t":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return "", 0, false, err
				}
				depth--
				b.WriteString(s)
			}
		case xml.EndElement:
			depth--
		}
	}
	return b.String(), headingLevel, isTitle, nil
}

func parseDOCXStyleHeading(style string) (level int, isTitle bool) {
	low := strings.ToLower(strings.TrimSpace(style))
	if low == "title" || low == "titulo" || low == "título" {
		return 1, true
	}
	for _, prefix := range []string{"heading", "titulo", "título", "encabezado"} {
		if strings.HasPrefix(low, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(low, prefix))
			if n, err := strconv.Atoi(rest); err == nil && n >= 1 && n <= 6 {
				return n, false
			}
		}
	}
	return 0, false
}

func readDOCXTable(dec *xml.Decoder) (string, error) {
	var rows []string
	var curRow []string
	var curCell strings.Builder
	inCell := false
	depth := 1

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "tr":
				curRow = nil
			case "tc":
				inCell = true
				curCell.Reset()
			case "t":
				if inCell {
					var s string
					if err := dec.DecodeElement(&s, &t); err != nil {
						return "", err
					}
					depth--
					curCell.WriteString(s)
				}
			case "br", "tab":
				if inCell {
					curCell.WriteByte(' ')
				}
			case "p":
				if inCell && curCell.Len() > 0 {
					curCell.WriteByte(' ')
				}
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "tc":
				if inCell {
					curRow = append(curRow, strings.TrimSpace(curCell.String()))
					inCell = false
				}
			case "tr":
				hasContent := false
				for _, c := range curRow {
					if c != "" {
						hasContent = true
						break
					}
				}
				if hasContent {
					rows = append(rows, strings.Join(curRow, " | "))
				}
			}
		}
	}
	return strings.Join(rows, "\n"), nil
}
