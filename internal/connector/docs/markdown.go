package docs

import (
	"os"
	"path/filepath"
	"strings"
)

// parseMarkdown parses .md, .markdown, and .txt files into heading-delimited sections.
func parseMarkdown(path string) (*ParsedDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if ext == "markdown" {
		ext = "md"
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var sections []ParsedSection
	var cur *ParsedSection
	var docTitle string
	inFence := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence && ext == "md" {
			if lvl, heading, ok := parseMarkdownOrTextileHeading(trimmed); ok {
				if docTitle == "" && lvl == 1 {
					docTitle = heading
				}
				sections = append(sections, ParsedSection{Title: heading, Level: lvl})
				cur = &sections[len(sections)-1]
				continue
			}
		}
		if cur == nil {
			if trimmed == "" {
				continue
			}
			sections = append(sections, ParsedSection{
				Title: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
				Level: 1,
			})
			cur = &sections[len(sections)-1]
		}
		if cur.Body != "" {
			cur.Body += "\n"
		}
		cur.Body += line
	}

	if docTitle == "" {
		docTitle = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &ParsedDoc{
		Title:    docTitle,
		Format:   ext,
		Sections: sections,
	}, nil
}

// parseMarkdownOrTextileHeading recognizes ATX Markdown headings ("# Title" ..
// "###### Title") as well as Textile headings ("h1. Title" .. "h6. Title"),
// commonly produced by Redmine/Confluence wiki exports.
func parseMarkdownOrTextileHeading(trimmed string) (int, string, bool) {
	if strings.HasPrefix(trimmed, "#") {
		lvl := 0
		for lvl < len(trimmed) && lvl < 6 && trimmed[lvl] == '#' {
			lvl++
		}
		if lvl > 0 && lvl < len(trimmed) && trimmed[lvl] == ' ' {
			if h := strings.TrimSpace(trimmed[lvl+1:]); h != "" {
				return lvl, h, true
			}
		}
	}
	if len(trimmed) >= 4 && (trimmed[0] == 'h' || trimmed[0] == 'H') &&
		trimmed[1] >= '1' && trimmed[1] <= '6' && trimmed[2] == '.' && trimmed[3] == ' ' {
		if h := strings.TrimSpace(trimmed[4:]); h != "" {
			return int(trimmed[1] - '0'), h, true
		}
	}
	return 0, "", false
}
