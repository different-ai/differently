// Package server implements the Differently HTTP API, the built-in dashboard
// and the node-agent registration endpoint.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	apispec "github.com/different-ai/differently/api"
	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/dashboard"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/metrics"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/pkg/api"
)

// Server is the API server.
type Server struct {
	Config       config.Config
	Store        store.Store
	Pools        *pools.Manager
	Scheduler    *scheduler.Scheduler
	Audit        *auditlog.Recorder
	Log          *slog.Logger
	Installation string
	AgentToken   string

	limiter *limiter
	now     func() time.Time
}

// New wires a server.
func New(cfg config.Config, st store.Store, pm *pools.Manager, sch *scheduler.Scheduler, rec *auditlog.Recorder, log *slog.Logger, installation, agentToken string) *Server {
	return &Server{
		Config: cfg, Store: st, Pools: pm, Scheduler: sch, Audit: rec, Log: log, Installation: installation, AgentToken: agentToken,
		limiter: newLimiter(cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Bootstrap creates the operator key from the bootstrap token and loads
// config-declared pools. In dev mode it also creates a default organization
// and project and writes a local operator token file.
func (s *Server) Bootstrap(ctx context.Context) error {
	token := os.Getenv(s.Config.Auth.BootstrapTokenEnv)
	if token == "" && s.Config.Dev {
		var err error
		if token, err = devToken(s.Config.DataDir); err != nil {
			return err
		}
	}
	if token == "" {
		s.Log.Warn("no bootstrap token configured; set " + s.Config.Auth.BootstrapTokenEnv + " to create the first operator key")
	}
	if token != "" {
		keyID, secret, ok := auth.ParseToken(token)
		if !ok {
			return fmt.Errorf("%s is not a valid Differently token (want dly_<id>_<secret>)", s.Config.Auth.BootstrapTokenEnv)
		}
		existing, err := s.Store.GetKey(ctx, keyID)
		switch {
		case api.IsCode(err, api.CodeNotFound):
			err = s.Store.CreateKey(ctx, store.KeyRecord{
				APIKey: api.APIKey{ID: keyID, Name: "bootstrap operator", Scopes: []string{auth.ScopeAdmin}, CreatedBy: "bootstrap", CreatedAt: s.now()},
				Hash:   auth.Hash(secret),
			})
			if err == nil {
				s.Audit.Record(ctx, api.AuditEvent{Actor: "bootstrap", Action: "key.create", Resource: keyID, Details: map[string]string{"scopes": auth.ScopeAdmin}})
			}
		case err == nil && existing.Hash != auth.Hash(secret):
			// A revoked bootstrap key stays revoked even if the token changes.
			_, err = s.Store.UpdateKey(ctx, keyID, func(k *store.KeyRecord) error { k.Hash = auth.Hash(secret); return nil })
			s.Audit.Record(ctx, api.AuditEvent{Actor: "bootstrap", Action: "key.rotate", Resource: keyID, Details: map[string]string{"revoked": itoa64(map[bool]int64{true: 1}[existing.Revoked])}})
		}
		if err != nil {
			return err
		}
	}
	for _, pc := range s.Config.Pools {
		p := pc.ToPool()
		if old, err := s.Store.GetPool(ctx, p.Name); err == nil && old.Source == "config" {
			p.Draining = old.Draining // drain state is operational, keep it across restarts
		}
		if err := s.Store.UpsertPool(ctx, p); err != nil {
			return err
		}
	}
	if s.Config.Dev {
		if _, err := s.Store.GetOrg(ctx, "org_default"); api.IsCode(err, api.CodeNotFound) {
			_ = s.Store.CreateOrg(ctx, api.Organization{ID: "org_default", Name: "Default", Quota: s.Config.Defaults.OrgQuota, CreatedAt: s.now()})
			_ = s.Store.CreateProject(ctx, api.Project{ID: "prj_default", OrgID: "org_default", Name: "default", Quota: s.Config.Defaults.ProjectQuota, CreatedAt: s.now()})
		}
	}
	return s.SyncPools(ctx)
}

// SyncPools rebuilds adapters from the stored pool registry.
func (s *Server) SyncPools(ctx context.Context) error {
	list, err := s.Store.ListPools(ctx)
	if err != nil {
		return err
	}
	s.Pools.Sync(list)
	return nil
}

func devToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	token := auth.TokenPrefix + "devoperator_" + id.Secret(40)
	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// handlerFunc is an authenticated endpoint.
type handlerFunc func(w http.ResponseWriter, r *http.Request, p auth.Principal) error

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	public := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.observe(pattern, h)) }
	authed := func(pattern string, h handlerFunc) { mux.Handle(pattern, s.observe(pattern, s.authenticate(h))) }

	public("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	public("GET /readyz", s.ready)
	public("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, api.VersionInfo{Version: version.Version, Commit: version.Commit, APIVersion: "v1", Installation: s.Installation})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	public("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(apispec.OpenAPI)
	})
	if s.Config.Dashboard.On() {
		mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", dashboard.Handler()))
		mux.Handle("GET /{$}", http.RedirectHandler("/dashboard/", http.StatusFound))
	}
	public("POST /internal/v1/agents/heartbeat", s.agentHeartbeat)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, api.Errorf(api.CodeNotFound, "no route for %s %s", r.Method, r.URL.Path))
	})

	authed("GET /v1/me", s.me)
	s.sandboxRoutes(authed)
	s.tenancyRoutes(authed)
	s.adminRoutes(authed)
	return s.recover(mux)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.Store.Installation(ctx); err != nil {
		writeErr(w, r, api.Errorf(api.CodeProviderUnavailable, "store unavailable: %v", err))
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type ctxKey int

const requestIDKey ctxKey = 1

func requestID(r *http.Request) string {
	v, _ := r.Context().Value(requestIDKey).(string)
	return v
}

// observe adds a request ID, structured access logs and metrics.
func (s *Server) observe(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 {
			rid = id.New("req")
		}
		w.Header().Set("X-Request-Id", rid)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, rid))
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = 200
		}
		metrics.Requests.WithLabelValues(route, strconv.Itoa(sw.status)).Inc()
		metrics.RequestSeconds.WithLabelValues(route).Observe(time.Since(start).Seconds())
		if route != "GET /healthz" && route != "GET /readyz" {
			s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(), "requestId", rid)
		}
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				writeErr(w, r, api.Errorf(api.CodeInternal, "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the bearer token to a principal and rate-limits it.
func (s *Server) authenticate(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get("X-Api-Key")
		}
		keyID, secret, ok := auth.ParseToken(token)
		if !ok {
			writeErr(w, r, api.Errorf(api.CodeUnauthenticated, "missing or malformed API key; send Authorization: Bearer dly_..."))
			return
		}
		k, err := s.Store.GetKey(r.Context(), keyID)
		if err == nil {
			err = auth.Verify(k, secret, s.now())
		} else if api.IsCode(err, api.CodeNotFound) {
			err = api.Errorf(api.CodeUnauthenticated, "invalid API key")
		}
		if err != nil {
			writeErr(w, r, err)
			return
		}
		if !s.limiter.allow(keyID) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, r, api.Errorf(api.CodeRateLimited, "rate limit exceeded for this key"))
			return
		}
		if k.LastUsedAt == nil || s.now().Sub(*k.LastUsedAt) > time.Minute {
			now := s.now()
			_, _ = s.Store.UpdateKey(r.Context(), keyID, func(rec *store.KeyRecord) error { rec.LastUsedAt = &now; return nil })
		}
		p := auth.Principal{KeyID: k.ID, OrgID: k.OrgID, ProjectID: k.ProjectID, Scopes: k.Scopes}
		if p.OrgID != "" {
			if org, err := s.Store.GetOrg(r.Context(), p.OrgID); err == nil && org.Suspended && r.Method != http.MethodGet {
				writeErr(w, r, api.Errorf(api.CodeSuspended, "organization %s is suspended", org.ID))
				return
			}
		}
		if err := h(w, r, p); err != nil {
			writeErr(w, r, err)
		}
	})
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, p auth.Principal) error {
	writeJSON(w, 200, map[string]any{"keyId": p.KeyID, "orgId": p.OrgID, "projectId": p.ProjectID, "scopes": p.Scopes, "operator": p.Operator()})
	return nil
}

// audit records an event attributed to p.
func (s *Server) audit(r *http.Request, p auth.Principal, action, resource, orgID, projectID string, details map[string]string) {
	s.Audit.Record(r.Context(), api.AuditEvent{Actor: p.Actor(), OrgID: orgID, ProjectID: projectID, Action: action, Resource: resource, Details: details, RequestID: requestID(r)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	e := api.AsError(err)
	if e.Code == "insufficient_capacity" {
		e = &api.Error{Code: api.CodeProviderUnavailable, Message: e.Message, Details: e.Details}
	}
	out := *e
	out.RequestID = requestID(r)
	writeJSON(w, out.HTTPStatus(), api.ErrorEnvelope{Error: &out})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return api.Errorf(api.CodeInvalidRequest, "invalid JSON body: %v", err)
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}

// limiter is a per-key token bucket. Limits are per API replica.
type limiter struct {
	rate    float64
	burst   float64
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
