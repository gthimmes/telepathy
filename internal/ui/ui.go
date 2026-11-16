// Package ui embeds the built web app (web/ → internal/ui/dist) into the binary.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

const notBuilt = `<!doctype html><meta charset="utf-8"><title>Telepathy</title>
<body style="font-family:system-ui;background:#0d0d0d;color:#f0efec;padding:48px">
<h1>Telepathy is running</h1>
<p>The API and OTLP ingest are up, but the web UI has not been built into this binary.</p>
<pre>cd web &amp;&amp; npm ci &amp;&amp; npm run build</pre>
<p>then rebuild the server.</p></body>`

// Handler serves the single-page app, falling back to index.html.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			index, err := fs.ReadFile(sub, "index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err != nil {
				w.Write([]byte(notBuilt))
				return
			}
			w.Write(index)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
