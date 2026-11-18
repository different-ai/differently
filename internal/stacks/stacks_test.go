package stacks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

type fakeStore struct {
	mu     sync.Mutex
	stacks []api.Stack
	leader bool
}

func (f *fakeStore) ListStacks(context.Context) ([]api.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Stack{}, f.stacks...), nil
}
func (f *fakeStore) TryLeader(context.Context) (bool, error) { return f.leader, nil }

type events struct {
	mu   sync.Mutex
	list []api.AuditEvent
}

func (e *events) Record(_ context.Context, ev api.AuditEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func component(t *testing.T, m *Manager, s api.Stack, name string) api.ComponentHealth {
	t.Helper()
	m.Decorate(&s)
	for _, c := range s.Components {
		if c.Name == name {
			return *c.Health
		}
	}
	t.Fatalf("no component %s", name)
	return api.ComponentHealth{}
}

func TestNormalizeFillsOpenWorkRoutes(t *testing.T) {
	kind, got, err := Normalize(api.StackOpenWork, []api.StackComponent{
		{Role: "den-api", URL: "https://api.example.test/"},
		{Role: "den-gateway", URL: "http://gw:8788"},
		{Role: "workbot", URL: "http://workbot:3020"},
		{Name: "cache", URL: "http://redis-exporter:9121", HealthPath: "/-/healthy"},
	})
	if err != nil || kind != api.StackOpenWork {
		t.Fatal(kind, err)
	}
	want := []api.StackComponent{
		{Name: "den-api", Role: "den-api", URL: "https://api.example.test", HealthPath: "/health", ReadyPath: "/ready"},
		{Name: "den-gateway", Role: "den-gateway", URL: "http://gw:8788", HealthPath: "/__gw/health", ReadyPath: "/__gw/ready"},
		{Name: "workbot", Role: "workbot", URL: "http://workbot:3020", HealthPath: "/healthz"},
		{Name: "cache", URL: "http://redis-exporter:9121", HealthPath: "/-/healthy"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("component %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	// A custom stack knows no roles.
	if _, got, _ := Normalize("", []api.StackComponent{{Name: "workbot", Role: "workbot", URL: "http://w"}}); got[0].HealthPath != "/health" || got[0].ReadyPath != "" {
		t.Errorf("custom stack got OpenWork routes: %+v", got[0])
	}
	for name, c := range map[string]api.StackComponent{
		"no scheme":        {Name: "a", URL: "den-api:8788"},
		"file scheme":      {Name: "a", URL: "file:///etc/passwd"},
		"credentials":      {Name: "a", URL: "http://user:pw@host"},
		"query":            {Name: "a", URL: "http://host/?token=x"},
		"bad name":         {Name: "Den API", URL: "http://host"},
		"relative path":    {Name: "a", URL: "http://host", HealthPath: "health"},
		"literal secret":   {Name: "a", URL: "http://host", CredentialRef: "hunter2"},
		"unnamed, no role": {URL: "http://host"},
	} {
		if _, _, err := Normalize("", []api.StackComponent{c}); !api.IsCode(err, api.CodeInvalidRequest) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, _, err := Normalize("", []api.StackComponent{{Name: "a", URL: "http://h"}, {Name: "a", URL: "http://h"}}); err == nil {
		t.Error("a duplicate component was accepted")
	}
	if _, _, err := Normalize("kubernetes", []api.StackComponent{{Name: "a", URL: "http://h"}}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func TestProbeReportsUpNotReadyAndDown(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	var sawToken atomic.Value
	sawToken.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		sawToken.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true,"service":"den-api","version":"1.4.2"}`))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the probe followed a redirect") }))
	defer elsewhere.Close()
	mux.HandleFunc("GET /moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, elsewhere.URL, http.StatusFound) })
	den := httptest.NewServer(mux)

	store := &fakeStore{leader: true}
	audit := &events{}
	m := New(Deps{Store: store, Audit: audit, Resolve: func(ref string) (string, error) { return "tok-" + ref, nil }})
	s := api.Stack{Name: "prod", Components: []api.StackComponent{
		{Name: "den-api", URL: den.URL, HealthPath: "/health", ReadyPath: "/ready", CredentialRef: "env:PROBE"},
		{Name: "moved", URL: den.URL, HealthPath: "/moved", Optional: true},
	}}
	store.stacks = []api.Stack{s}
	ctx := context.Background()

	if h := component(t, m, s, "den-api"); h.Status != api.HealthUnknown {
		t.Fatalf("before any probe: %+v", h)
	}
	m.Refresh(ctx)
	h := component(t, m, s, "den-api")
	if h.Status != api.HealthHealthy || h.Version != "1.4.2" || h.CheckedAt.IsZero() || h.Since.IsZero() {
		t.Fatalf("healthy probe: %+v", h)
	}
	if got := sawToken.Load(); got != "Bearer tok-env:PROBE" {
		t.Fatalf("probe credential: %q", got)
	}
	if h := component(t, m, s, "moved"); h.Status != api.HealthUnavailable {
		t.Fatalf("a redirecting health route: %+v", h)
	}
	// The optional component is down; the stack is still healthy.
	view := s
	m.Decorate(&view)
	if view.Health.Status != api.HealthHealthy {
		t.Fatalf("stack with only an optional component down: %+v", view.Health)
	}

	ready.Store(false)
	m.Refresh(ctx)
	if h := component(t, m, s, "den-api"); h.Status != api.HealthDegraded {
		t.Fatalf("up but not ready: %+v", h)
	}
	if n := countFor(audit, "prod/den-api"); n != 0 {
		t.Fatalf("one bad probe was written to the audit log (%d events)", n)
	}
	m.Refresh(ctx)
	if n := countFor(audit, "prod/den-api"); n != 1 {
		t.Fatalf("a status that held for two probes: %d audit events", n)
	}
	m.Refresh(ctx)
	if n := countFor(audit, "prod/den-api"); n != 1 {
		t.Fatalf("an unchanged status was recorded again: %d audit events", n)
	}

	den.Close()
	m.Refresh(ctx)
	m.Refresh(ctx)
	h = component(t, m, s, "den-api")
	if h.Status != api.HealthUnavailable || h.Message == "" {
		t.Fatalf("a closed service: %+v", h)
	}
	view = s
	m.Decorate(&view)
	if view.Health.Status != api.HealthUnavailable || view.Health.Message != "den-api is unavailable" {
		t.Fatalf("stack with its API down: %+v", view.Health)
	}
	audit.mu.Lock()
	last := audit.list[len(audit.list)-1]
	audit.mu.Unlock()
	if last.Action != "stack.health" || last.Outcome != "failure" || last.Details["from"] != api.HealthDegraded || last.Details["to"] != api.HealthUnavailable {
		t.Fatalf("audit event: %+v", last)
	}

	// A removed stack is forgotten.
	store.mu.Lock()
	store.stacks = nil
	store.mu.Unlock()
	m.Refresh(ctx)
	if h := component(t, m, s, "den-api"); h.Status != api.HealthUnknown {
		t.Fatalf("a removed stack kept its health: %+v", h)
	}
}

func TestOnlyTheLeaderRecordsHealthChanges(t *testing.T) {
	store := &fakeStore{}
	audit := &events{}
	m := New(Deps{Store: store, Audit: audit})
	store.stacks = []api.Stack{{Name: "prod", Components: []api.StackComponent{{Name: "api", URL: "http://127.0.0.1:1", HealthPath: "/health"}}}}
	m.Refresh(context.Background())
	m.Refresh(context.Background())
	if len(audit.list) != 0 {
		t.Fatalf("a follower recorded %d events", len(audit.list))
	}
}

func countFor(e *events, resource string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.list {
		if ev.Resource == resource {
			n++
		}
	}
	return n
}
