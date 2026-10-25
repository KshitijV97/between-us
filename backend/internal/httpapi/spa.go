package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// newSPAHandler serves the single-page app from ui.
//
//   - Existing files are served as-is. Vite fingerprints everything under
//     /assets/, so those are cached for a year as immutable.
//   - Any other path falls back to index.html, so client-side routes such as
//     /chats/123 survive a page reload.
//   - index.html itself is never cached, so a new deployment is picked up on
//     the next navigation.
//
// When ui has no index.html (the UI was not built), a small placeholder page
// explains how to build it instead of returning a confusing 404.
func newSPAHandler(ui fs.FS) http.Handler {
	if ui == nil {
		return http.HandlerFunc(serveNotBuilt)
	}
	if _, err := fs.Stat(ui, "index.html"); err != nil {
		return http.HandlerFunc(serveNotBuilt)
	}
	files := http.FileServerFS(ui)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "index.html" {
			serveIndex(w, r, ui)
			return
		}
		info, err := fs.Stat(ui, name)
		if err != nil || info.IsDir() {
			// A missing file with an extension is a genuinely missing asset
			// (e.g. a stale chunk); returning index.html would confuse the
			// browser with HTML where JS was expected.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, ui)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// getOnly restricts h to safe methods; the SPA has no write endpoints.
func getOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, ui fs.FS) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, ui, "index.html")
}

const notBuiltPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Between Us</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem">
<h1>Between Us API is running</h1>
<p>The web UI is not embedded in this build. Run <code>make build</code> to bundle it,
or <code>make dev</code> to use the Vite dev server with hot reload.</p>
<p>Health: <a href="/healthz">/healthz</a> · Version: <a href="/version">/version</a></p>
</body></html>`

func serveNotBuilt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(notBuiltPage))
}
