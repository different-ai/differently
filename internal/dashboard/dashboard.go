// Package dashboard serves the built-in operator and tenant web console. It
// is a static single-page app that calls the public API with the viewer's own
// API key, so it can do nothing the key cannot.
package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the dashboard assets.
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	fs := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		fs.ServeHTTP(w, r)
	})
}
