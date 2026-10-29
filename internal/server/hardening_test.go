package server_test

import (
	"context"
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
