package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// An operator registers an OpenWork deployment by its service URLs and sees
// each service's health, readiness and version; tenants cannot.
func TestOperatorWatchesAnOpenWorkStack(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	var ready atomic.Bool
	ready.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"service":"den-api","version":"0.9.0"}`))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	openwork := httptest.NewServer(mux)
	t.Cleanup(openwork.Close)

	body := map[string]any{"kind": "openwork", "components": []map[string]any{
		{"role": "den-api", "url": openwork.URL},
		{"role": "workbot", "url": openwork.URL},
		{"name": "runner", "role": "headless-runner", "url": "http://127.0.0.1:1", "optional": true},
	}}
	var st api.Stack
	h.must(201, "PUT", "/v1/admin/stacks/prod", operatorToken, body, &st)
	if st.Health == nil || st.Health.Status != api.HealthHealthy {
		t.Fatalf("stack health right after registering: %+v", st.Health)
	}
	if c := st.Components[0]; c.Name != "den-api" || c.ReadyPath != "/ready" || c.Health.Status != api.HealthHealthy || c.Health.Version != "0.9.0" {
		t.Fatalf("den-api: %+v %+v", c, c.Health)
	}
	if c := st.Components[1]; c.HealthPath != "/healthz" || c.Health.Status != api.HealthHealthy {
		t.Fatalf("workbot: %+v %+v", c, c.Health)
	}
	if c := st.Components[2]; c.Health.Status != api.HealthUnavailable {
		t.Fatalf("unreachable runner: %+v", c.Health)
	}

	ready.Store(false)
	h.must(200, "POST", "/v1/admin/stacks/prod/check", operatorToken, nil, &st)
	if st.Health.Status != api.HealthDegraded || !strings.Contains(st.Health.Message, "den-api") {
		t.Fatalf("stack with Den not ready: %+v", st.Health)
	}
	h.must(200, "POST", "/v1/admin/stacks/prod/check", operatorToken, nil, nil)
	var raw string
	h.call("GET", "/v1/audit-events", operatorToken, nil, &raw)
	if !strings.Contains(raw, "stack.put") || !strings.Contains(raw, "stack.health") {
		t.Fatalf("audit log lacks the stack events: %s", raw)
	}

	var list api.List[api.Stack]
	h.must(200, "GET", "/v1/admin/stacks", operatorToken, nil, &list)
	if len(list.Items) != 1 || list.Items[0].Health.Status != api.HealthDegraded {
		t.Fatalf("list: %+v", list.Items)
	}
	// Replacing keeps who created it.
	h.must(200, "PUT", "/v1/admin/stacks/prod", operatorToken, body, &st)

	_, _, tenant := h.tenant("acme", api.Quota{})
	h.must(403, "GET", "/v1/admin/stacks", tenant, nil, nil)
	h.must(403, "PUT", "/v1/admin/stacks/mine", tenant, body, nil)

	h.must(400, "PUT", "/v1/admin/stacks/Bad_Name", operatorToken, body, nil)
	h.must(400, "PUT", "/v1/admin/stacks/empty", operatorToken, map[string]any{"components": []any{}}, nil)
	h.must(400, "PUT", "/v1/admin/stacks/ref", operatorToken, map[string]any{"components": []map[string]any{{"name": "a", "url": openwork.URL, "credentialRef": "secret:missing"}}}, nil)

	h.must(204, "DELETE", "/v1/admin/stacks/prod", operatorToken, nil, nil)
	h.must(404, "GET", "/v1/admin/stacks/prod", operatorToken, nil, nil)
	h.must(404, "POST", "/v1/admin/stacks/prod/check", operatorToken, nil, nil)
}
