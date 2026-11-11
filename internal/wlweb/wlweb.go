// Package wlweb holds the page of the whitelist app: HTML, CSS, script, the font, and
// Discord's Embedded App SDK as one bundled file (MIT, built from @discord/embedded-app-sdk
// with tools/sdk, see README there).
package wlweb

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var files embed.FS

// Handler serves the page. Paths are relative to /whitelist.
func Handler() http.Handler {
	sub, _ := fs.Sub(files, "web")
	fsrv := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Discord shows the page in an iframe on discordsays.com; the website shows it itself
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self' https://discord.com wss://*.discordsays.com https://*.discordsays.com; frame-ancestors 'self' https://discord.com https://*.discord.com https://*.discordsays.com")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		switch {
		case r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html"):
			h.Set("Cache-Control", "no-cache")
		case strings.HasSuffix(r.URL.Path, ".woff2"):
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			h.Set("Cache-Control", "public, max-age=300")
		}
		fsrv.ServeHTTP(w, r)
	})
}
