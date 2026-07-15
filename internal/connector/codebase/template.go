package codebase

// Content-based template handling: any git-tracked text file whose extension
// gatt doesn't parse (.vue, .html, .cshtml, .razor, .svelte, .erb, .php, —
// deliberately no extension list) gets a bare file node, and, when it embeds
// client-side HTTP surface, feeds the same client-call pipeline as real code:
//
//   - <script> blocks are *masked*, not extracted: every byte outside a kept
//     block becomes a space (newlines preserved) and the whole buffer is
//     parsed with the JS/TS grammar — line numbers stay correct for free, and
//     functions/calls inside the blocks become regular graph nodes;
//   - htmx attributes (hx-get="/x", …) and <form action="/x" method="post">
//     are scanned textually and queued as clientCall candidates directly.
//
// Detection is content sniffing (contains "<script" / "hx-" / "<form"), so a
// new template framework needs no code change here.

import (
	"regexp"
	"strings"
)

// maxTemplateBytes bounds what an unknown-extension file may weigh to be
// indexed at all — mirrored by scanFiles so drift checks stay stat-only.
const maxTemplateBytes = 1 << 20

// binaryExts are extensions never worth a file node or a content sniff.
// Everything else that git tracks gets a bare node (mtime-aligned with
// scanFiles) and, if it holds template markup, client-call scanning.
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".avif": true, ".ico": true, ".bmp": true, ".tif": true, ".tiff": true,
	".svg": true, ".psd": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".mp4": true, ".m4a": true, ".webm": true, ".ogg": true,
	".wav": true, ".mov": true, ".avi": true, ".mkv": true, ".flac": true,
	".zip": true, ".gz": true, ".tgz": true, ".tar": true, ".bz2": true,
	".xz": true, ".7z": true, ".rar": true, ".jar": true, ".war": true, ".whl": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true,
	".o": true, ".bin": true, ".dat": true, ".wasm": true, ".class": true,
	".pyc": true, ".pdb": true, ".obj": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".odt": true,
	".sqlite": true, ".db": true, ".map": true,
}

var (
	scriptBlockRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	scriptSrcRe   = regexp.MustCompile(`(?i)\bsrc\s*=`)
	scriptTypeRe  = regexp.MustCompile(`(?i)\btype\s*=\s*["']([^"']+)["']`)
	scriptLangRe  = regexp.MustCompile(`(?i)\blang\s*=\s*["']?(ts|typescript)\b`)

	htmxAttrRe = regexp.MustCompile(`(?i)\bhx-(get|post|put|patch|delete)\s*=\s*["']([^"']+)["']`)
	formTagRe  = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	formAttrRe = regexp.MustCompile(`(?i)\b(action|method)\s*=\s*["']([^"']+)["']`)
)

// maskScriptBlocks returns src with everything outside inline <script>
// bodies blanked to spaces (newlines kept), plus the grammar extension the
// blocks want (".ts" when any block says lang="ts", else ".js"). nil when no
// parseable inline block exists. External (src=) and non-JS (type="…json…")
// blocks stay masked out.
func maskScriptBlocks(src []byte) ([]byte, string) {
	matches := scriptBlockRe.FindAllSubmatchIndex(src, -1)
	if matches == nil {
		return nil, ""
	}
	masked := make([]byte, len(src))
	for i, b := range src {
		if b == '\n' {
			masked[i] = '\n'
		} else {
			masked[i] = ' '
		}
	}
	langExt := ".js"
	kept := false
	for _, m := range matches {
		attrs := string(src[m[2]:m[3]])
		if scriptSrcRe.MatchString(attrs) {
			continue
		}
		if t := scriptTypeRe.FindStringSubmatch(attrs); t != nil {
			v := strings.ToLower(t[1])
			if v != "module" && !strings.Contains(v, "javascript") {
				continue
			}
		}
		if scriptLangRe.MatchString(attrs) {
			langExt = ".ts"
		}
		copy(masked[m[4]:m[5]], src[m[4]:m[5]])
		kept = true
	}
	if !kept {
		return nil, ""
	}
	return masked, langExt
}

// scanTemplateAttrs queues clientCall candidates for markup-level HTTP
// surface: htmx verb attributes and <form action(+method)> tags. The edge
// source falls back to the file node (wireClientCalls) unless the attribute
// sits inside a function's line range, which markup never does.
func (c *Connector) scanTemplateAttrs(src []byte, relPath string) {
	lineOf := func(off int) int {
		n := 1
		for _, b := range src[:off] {
			if b == '\n' {
				n++
			}
		}
		return n
	}
	for _, m := range htmxAttrRe.FindAllSubmatchIndex(src, -1) {
		method := strings.ToUpper(string(src[m[2]:m[3]]))
		path := normalizeClientPath(`"` + string(src[m[4]:m[5]]) + `"`)
		if !isRootedPath(path) {
			continue
		}
		c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
			method: method, path: path, file: relPath, line: lineOf(m[0]),
		})
	}
	for _, f := range formTagRe.FindAllIndex(src, -1) {
		tag := src[f[0]:f[1]]
		method, path := "GET", "" // HTML default method
		for _, m := range formAttrRe.FindAllSubmatch(tag, -1) {
			switch strings.ToLower(string(m[1])) {
			case "action":
				path = normalizeClientPath(`"` + string(m[2]) + `"`)
			case "method":
				if v, ok := clientHTTPVerbs[strings.ToLower(string(m[2]))]; ok {
					method = v
				}
			}
		}
		if !isRootedPath(path) {
			continue
		}
		c.pendingClientCalls = append(c.pendingClientCalls, clientCall{
			method: method, path: path, file: relPath, line: lineOf(f[0]),
		})
	}
}
