package codebase

// Selector-level CSS linkage: which template/JSX/stylesheet file uses which
// selectors of which repo stylesheet. Three cheap passes, no CSS grammar:
//
//  1. stylesheet side (extractCSSSelectors): `.class`, `#id` and `[data-*]`
//     selectors are collected from *selector position only* — text runs
//     that end at a `{`, cleared at `;`/`}` — so url(img.png), font sizes
//     (.5em), hex colors and declaration values never produce fake tokens;
//     custom-property definitions (`--brand-color:`) are mined from the
//     whole text. Stored as a css_selectors attr on the stylesheet's file
//     node (survives incremental refresh). Tag/global/pseudo selectors are
//     excluded by design: every file uses `div`, and `.btn:hover` already
//     links through `.btn`.
//  2. usage side (scanStyleUses): class="a b" / className / classList,
//     id="x" / getElementById / '#x' query strings, data-* attributes /
//     dataset.camelCase / setAttribute('data-*'), and var(--x) references —
//     scanned from JSX/TSX/JS/TS sources, template files (.vue/.html/
//     .cshtml — raw content, before masking) and other stylesheets (design
//     tokens make stylesheet→stylesheet edges).
//  3. wireStyles: usage file → stylesheet USES_STYLE edges, matched tokens
//     (in CSS syntax: `.btn,#cart,[data-role],--brand`) on the edge's
//     selectors attr. Only tokens *defined in the repo's own stylesheets*
//     link — Tailwind/Bootstrap utilities aren't defined here, so utility
//     soup produces zero edges; tokens defined in >3 repo stylesheets are
//     treated as ambiguous and skipped.

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"graphallthethings/internal/graph"
)

const maxCSSSelectorAttr = 6 * 1024 // cap on the css_selectors attr

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)
	cssClassRe   = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_-]*)`)
	cssIDRe      = regexp.MustCompile(`#([A-Za-z_][A-Za-z0-9_-]*)`)
	cssDataRe    = regexp.MustCompile(`\[\s*(data-[A-Za-z][\w-]*)`)
	cssVarDefRe  = regexp.MustCompile(`(--[A-Za-z_][\w-]*)\s*:`)

	classAttrRe   = regexp.MustCompile(`(?i)\b(?:class|className)\s*[:=]\s*["'{]?\s*["']?([^"'<>{}]+?)["'}]`)
	classListRe   = regexp.MustCompile(`classList\.(?:add|remove|toggle|replace)\(\s*["']([A-Za-z_][\w-]*)["']`)
	idAttrRe      = regexp.MustCompile(`\bid\s*=\s*["']([A-Za-z_][\w-]*)["']`)
	idByRe        = regexp.MustCompile(`getElementById\(\s*["']([A-Za-z_][\w-]*)["']`)
	idHashRe      = regexp.MustCompile(`["']#([A-Za-z_][\w-]*)["']`) // querySelector('#x'), $('#x')
	dataAttrUseRe = regexp.MustCompile(`\b(data-[A-Za-z][\w-]*)\s*=`)
	dataSetRe     = regexp.MustCompile(`\.dataset\.([A-Za-z]\w*)`)
	setDataAttrRe = regexp.MustCompile(`setAttribute\(\s*["'](data-[A-Za-z][\w-]*)["']`)
	cssVarUseRe   = regexp.MustCompile(`var\(\s*(--[A-Za-z_][\w-]*)`)

	classTokenRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	styleUseCutoff = 3 // token defined in more repo stylesheets than this = ambiguous
)

// styleUse is one scanned file's class-token set, wired by wireStyles.
type styleUse struct {
	file    string
	classes []string
}

// extractCSSSelectors returns the linkable tokens a stylesheet defines —
// `.class`, `#id`, `[data-attr]` (selector position) and `--var`
// (custom-property definitions) — comma-joined in CSS syntax for the file
// node's css_selectors attr ("" when none).
func extractCSSSelectors(src []byte) string {
	text := cssCommentRe.ReplaceAllString(string(src), " ")
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	buf := strings.Builder{}
	flush := func(isSelector bool) {
		if isSelector {
			s := buf.String()
			for _, m := range cssClassRe.FindAllStringSubmatch(s, -1) {
				add("." + m[1])
			}
			for _, m := range cssIDRe.FindAllStringSubmatch(s, -1) {
				add("#" + m[1])
			}
			for _, m := range cssDataRe.FindAllStringSubmatch(s, -1) {
				add("[" + m[1] + "]")
			}
		}
		buf.Reset()
	}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{':
			flush(true) // the run ending at { is a selector (works for SCSS nesting too)
		case '}', ';':
			flush(false) // declaration text — discard
		default:
			buf.WriteByte(text[i])
		}
	}
	flush(false)
	// Custom properties are declarations, so they're mined from full text.
	for _, m := range cssVarDefRe.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	sort.Strings(out)
	joined := strings.Join(out, ",")
	if len(joined) > maxCSSSelectorAttr {
		joined = joined[:maxCSSSelectorAttr]
		if i := strings.LastIndexByte(joined, ','); i > 0 {
			joined = joined[:i]
		}
	}
	return joined
}

// camelToKebab maps dataset.userRole to user-role (the DOM's dataset
// camelization, reversed).
func camelToKebab(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('-')
			b.WriteByte(byte(r - 'A' + 'a'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// scanStyleUses collects selector tokens (CSS syntax) referenced by a
// source, template or stylesheet file and queues them for wireStyles.
func (c *Connector) scanStyleUses(src []byte, relPath string) {
	seen := map[string]bool{}
	var tokens []string
	add := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			tokens = append(tokens, tok)
		}
	}
	for _, m := range classAttrRe.FindAllSubmatch(src, -1) {
		for _, tok := range strings.Fields(string(m[1])) {
			if classTokenRe.MatchString(tok) {
				add("." + tok)
			}
		}
	}
	for _, m := range classListRe.FindAllSubmatch(src, -1) {
		add("." + string(m[1]))
	}
	for _, re := range []*regexp.Regexp{idAttrRe, idByRe, idHashRe} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			add("#" + string(m[1]))
		}
	}
	for _, re := range []*regexp.Regexp{dataAttrUseRe, setDataAttrRe} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			add("[" + string(m[1]) + "]")
		}
	}
	for _, m := range dataSetRe.FindAllSubmatch(src, -1) {
		add("[data-" + camelToKebab(string(m[1])) + "]")
	}
	for _, m := range cssVarUseRe.FindAllSubmatch(src, -1) {
		add(string(m[1]))
	}
	if len(tokens) > 0 {
		c.pendingStyleUses = append(c.pendingStyleUses, styleUse{file: relPath, classes: tokens})
	}
}

// wireStyles adds usage-file → stylesheet USES_STYLE edges. The selector
// index is rebuilt from the graph's css_selectors attrs, so incremental
// refreshes wire against unchanged stylesheets too.
func (c *Connector) wireStyles(g *graph.Graph) {
	if len(c.pendingStyleUses) == 0 {
		return
	}
	classIndex := map[string][]string{} // selector token → stylesheet file node ids
	for _, n := range g.Nodes {
		if n.Type != graph.NodeFile || n.Attrs["css_selectors"] == "" {
			continue
		}
		for _, cl := range strings.Split(n.Attrs["css_selectors"], ",") {
			classIndex[cl] = append(classIndex[cl], n.ID)
		}
	}
	if len(classIndex) == 0 {
		c.pendingStyleUses = nil
		return
	}
	existing := map[string]bool{}
	for _, e := range g.Edges {
		if e.Type == graph.EdgeUsesStyle {
			existing[e.From+"\x00"+e.To] = true
		}
	}
	for _, u := range c.pendingStyleUses {
		fromID := "file:" + filepath.Join(c.dir, u.file)
		if g.Nodes[fromID] == nil {
			continue
		}
		matched := map[string][]string{} // stylesheet id → classes
		for _, cl := range u.classes {
			ids := classIndex[cl]
			if len(ids) == 0 || len(ids) > styleUseCutoff {
				continue
			}
			for _, cssID := range ids {
				if cssID != fromID {
					matched[cssID] = append(matched[cssID], cl)
				}
			}
		}
		for cssID, cls := range matched {
			if existing[fromID+"\x00"+cssID] {
				continue
			}
			existing[fromID+"\x00"+cssID] = true
			list := strings.Join(cls, ",")
			if len(list) > 200 {
				list = list[:200] + "…"
			}
			g.AddEdge(fromID, cssID, graph.EdgeUsesStyle, map[string]string{"selectors": list})
		}
	}
	c.pendingStyleUses = nil
}
