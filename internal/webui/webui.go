// Package webui embeds the built web app into the gatt binary.
// `make ui` copies web/dist into internal/webui/dist before building Go.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built web app rooted at index.html and its assets.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler serves the embedded SPA. Static assets are served directly;
// unknown non-API paths fall back to index.html for client-side navigation.
// If the UI has not been built yet (only README exists), it returns a helpful
// placeholder page.
func Handler() http.Handler {
	sub := FS()
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if _, err := fs.Stat(sub, "index.html"); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<!doctype html><html><body style="font-family:system-ui,sans-serif;padding:2rem;background:#0b1020;color:#e5e7eb">
<h2>gatt Web UI not built</h2>
<p>Build the frontend with <code>make ui</code> (or <code>cd web &amp;&amp; npm install &amp;&amp; npm run build</code>) and rebuild <code>gatt</code>.</p>
</body></html>`))
			return
		}

		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean == "" || clean == "." {
			serveIndex(w, r, sub)
			return
		}
		if f, err := sub.Open(clean); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				if strings.HasPrefix(clean, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, sub)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
