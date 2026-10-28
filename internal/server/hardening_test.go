package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func heartbeat(h *harness, id, pool, endpoint, secret string) int {
	h.t.Helper()
	body, _ := json.Marshal(api.Agent{ID: id, Pool: pool, Node: id, Endpoint: endpoint})
	req, _ := http.NewRequest("POST", h.srv.URL+"/internal/v1/agents/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-token")
	if secret != "" {
		req.Header.Set(api.AgentSecretHeader, secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// One node holding the shared registration token cannot take over another
// node's identity, pool or traffic.
func TestAgentIdentityIsPinned(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	ctx := context.Background()
	_ = h.store.UpsertPool(ctx, api.Pool{Name: "metal", Adapter: "firecracker", Trust: api.TrustPrivate, Enabled: true, Source: "config"})
	_ = h.store.UpsertPool(ctx, api.Pool{Name: "metal-2", Adapter: "firecracker", Trust: api.TrustPrivate, Enabled: true, Source: "config"})
	nodeA := strings.Repeat("a", 40)
	attacker := strings.Repeat("x", 40)

	if code := heartbeat(h, "node-a", "metal", "http://10.0.0.1:9090", nodeA); code != 200 {
		t.Fatalf("first registration: %d", code)
	}
	if code := heartbeat(h, "node-a", "metal", "http://10.0.0.1:9090", ""); code != 400 {
		t.Fatalf("heartbeat without a secret: %d", code)
	}
	if code := heartbeat(h, "node-a", "metal", "http://10.6.6.6:9090", attacker); code != 409 {
		t.Fatalf("hijack with another secret: %d", code)
	}
	if code := heartbeat(h, "node-a", "metal-2", "http://10.0.0.1:9090", nodeA); code != 409 {
		t.Fatalf("pool switch: %d", code)
	}
	// The real node may move (pod restarts change IPs).
	if code := heartbeat(h, "node-a", "metal", "http://10.0.0.2:9090", nodeA); code != 200 {
		t.Fatalf("endpoint change by the owner: %d", code)
	}
	rec, _ := h.store.GetAgent(ctx, "node-a")
	if rec.Endpoint != "http://10.0.0.2:9090" || rec.Secret != nodeA {
		t.Fatalf("stored agent %+v", rec.Agent)
	}
	var raw string
	h.must(200, "GET", "/v1/admin/agents", operatorToken, nil, nil)
	h.call("GET", "/v1/admin/agents", operatorToken, nil, &raw)
	if strings.Contains(raw, nodeA) || strings.Contains(raw, "secret") {
		t.Fatal("agent secrets leak through the admin API")
	}
	events, _ := h.store.ListAudit(ctx, store.AuditFilter{Resource: "node-a"})
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action+":"+ev.Outcome)
	}
	if got := strings.Join(actions, ","); !strings.Contains(got, "agent.register:failure") || !strings.Contains(got, "agent.endpoint_change:success") {
		t.Fatalf("audit trail %s", got)
	}
	// An operator can retire a rebuilt node's identity.
	h.must(204, "DELETE", "/v1/admin/agents/node-a", operatorToken, nil, nil)
	if code := heartbeat(h, "node-a", "metal", "http://10.0.0.3:9090", attacker); code != 200 {
		t.Fatalf("re-registration after removal: %d", code)
	}
}

// failingStore fails audit appends on demand.
type failingStore struct {
	store.Store
	fail atomic.Bool
}

func (f *failingStore) AppendAudit(ctx context.Context, ev api.AuditEvent) (api.AuditEvent, error) {
	if f.fail.Load() {
		return ev, errors.New("audit storage offline")
	}
	return f.Store.AppendAudit(ctx, ev)
}

func TestAuditFailClosed(t *testing.T) {
	for _, closed := range []bool{true, false} {
		var fs *failingStore
		h := newHarnessWith(t, api.PlacementPolicy{}, func(c *config.Config) { c.Audit.FailClosed = closed },
			func(s store.Store) store.Store { fs = &failingStore{Store: s}; return fs })
		_, _, token := h.tenant("acme", api.Quota{})
		fs.fail.Store(true)
		var e api.ErrorEnvelope
		code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{}, &e)
		recs, _ := h.store.ListSandboxes(context.Background(), store.SandboxFilter{})
		switch {
		case closed && (code != 503 || len(recs) != 0):
			t.Fatalf("fail-closed: status %d, %d sandboxes admitted (%+v)", code, len(recs), e.Error)
		case !closed && (code != 202 || len(recs) != 1):
			t.Fatalf("fail-open: status %d, %d sandboxes", code, len(recs))
		}
		// Reads keep working either way.
		if code := h.call("GET", "/v1/sandboxes", token, nil, nil); code != 200 {
			t.Fatalf("reads blocked: %d", code)
		}
		fs.fail.Store(false)
		var report server.TrustReport
		h.must(200, "GET", "/v1/admin/trust-report", operatorToken, nil, &report)
		mentionsFailOpen := strings.Contains(strings.Join(report.Limitations, " "), "fail-open")
		if mentionsFailOpen == closed {
			t.Fatalf("trust report fail-open limitation shown=%v with failClosed=%v", mentionsFailOpen, closed)
		}
	}
}

func TestMetricsCanLeaveTheAPIListener(t *testing.T) {
	h := newHarnessWith(t, api.PlacementPolicy{}, func(c *config.Config) { c.Metrics.Listen = ":0" }, nil)
	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/metrics still on the API listener: %d", resp.StatusCode)
	}
	plain := newHarness(t, api.PlacementPolicy{})
	resp, err = http.Get(plain.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("default /metrics: %d", resp.StatusCode)
	}
}
