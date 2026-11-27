package docs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/dslipak/pdf"
)

// parsePDFBytes extracts page-indexed text from raw PDF bytes in memory.
// When Poppler's `pdftotext` is installed on PATH it is preferred (piped via
// stdin so no temp file is needed); otherwise it falls back seamlessly to the
// pure-Go parser (github.com/dslipak/pdf).
func parsePDFBytes(ctx context.Context, filename string, data []byte) (*ParsedDoc, error) {
	var pages []string
	var err error

	if bin, lookErr := exec.LookPath("pdftotext"); lookErr == nil {
		pages, err = extractPDFWithPoppler(ctx, bin, data)
	}
	if len(pages) == 0 {
		pages, err = extractPDFPureGo(data)
		if err != nil {
			return nil, err
		}
	}

	title := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	sections := buildPDFSections(pages)

	return &ParsedDoc{
		Title:    title,
		Format:   "pdf",
		Pages:    len(pages),
		Sections: sections,
	}, nil
}

func extractPDFWithPoppler(ctx context.Context, bin string, data []byte) ([]string, error) {
	cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	raw := strings.Split(stdout.String(), "\f")
	if len(raw) > 0 && strings.TrimSpace(raw[len(raw)-1]) == "" {
		raw = raw[:len(raw)-1]
	}
	return raw, nil
}

func extractPDFPureGo(data []byte) ([]string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read pdf: %w", err)
	}

	n := r.NumPage()
	pages := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			pages = append(pages, "")
			continue
		}
		txt, err := p.GetPlainText(nil)
		if err != nil {
			pages = append(pages, "")
			continue
		}
		pages = append(pages, txt)
	}
	return pages, nil
}

var pdfNumberedHeadingRe = regexp.MustCompile(`^(?:(?:\d+\.)+\d*|\d+\)|(?:CAP[IÍ]TULO|SECCI[OÓ]N|ART[IÍ]CULO|CHAPTER|SECTION|ARTICLE)\s+[\dIVXLCM]+)\s+\S+`)

func looksLikePDFHeading(line string) bool {
	s := strings.TrimSpace(line)
	runes := []rune(s)
	if len(runes) < 3 || len(runes) > 90 {
		return false
	}
	last := runes[len(runes)-1]
	if last == '.' || last == ',' || last == ';' {
		return false
	}
	if pdfNumberedHeadingRe.MatchString(s) {
		return true
	}
	letters, upper := 0, 0
	for _, r := range runes {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	return letters >= 4 && letters == upper
}

func buildPDFSections(pages []string) []ParsedSection {
	var sections []ParsedSection
	var cur *ParsedSection

	for idx, rawPage := range pages {
		pageNum := idx + 1
		clean := normalizeWhitespace(rawPage)
		if clean == "" {
			continue
		}
		lines := strings.Split(clean, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				if cur != nil && cur.Body != "" && !strings.HasSuffix(cur.Body, "\n\n") {
					cur.Body += "\n\n"
				}
				continue
			}
			if looksLikePDFHeading(trimmed) {
				sections = append(sections, ParsedSection{
					Title:     trimmed,
					Level:     1,
					PageStart: pageNum,
					PageEnd:   pageNum,
				})
				cur = &sections[len(sections)-1]
				continue
			}
			if cur == nil {
				sections = append(sections, ParsedSection{
					Title:     fmt.Sprintf("Page %d", pageNum),
					Level:     1,
					PageStart: pageNum,
					PageEnd:   pageNum,
				})
				cur = &sections[len(sections)-1]
			} else if pageNum > cur.PageEnd {
				if strings.HasPrefix(cur.Title, "Page ") {
					sections = append(sections, ParsedSection{
						Title:     fmt.Sprintf("Page %d", pageNum),
						Level:     1,
						PageStart: pageNum,
						PageEnd:   pageNum,
					})
					cur = &sections[len(sections)-1]
				} else {
					cur.PageEnd = pageNum
				}
			}
			if cur.Body != "" && !strings.HasSuffix(cur.Body, "\n") {
				cur.Body += "\n"
			}
			cur.Body += trimmed
		}
	}
	return sections
}
