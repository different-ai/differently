package server

import (
	"net/http"
	"strings"
)

// cors answers browser requests from the origins in cors.allowedOrigins, so
// a console served from another host can call the API. Only exact origins
// match, and only the API, /version and /openapi.yaml are covered. With no
// origins configured it adds nothing.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.Config.CORS.AllowedOrigins {
		allowed[strings.TrimSuffix(o, "/")] = true
	}
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		path := r.URL.Path
		if !allowed[origin] || !(strings.HasPrefix(path, "/v1/") || path == "/version" || path == "/openapi.yaml") {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, Location")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID, X-Request-Id")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
