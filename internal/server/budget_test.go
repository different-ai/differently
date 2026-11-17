package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// budget sets a harness pool's monthly budget, as a config file would.
func (h *harness) budget(pool string, monthly float64) {
	h.t.Helper()
	ctx := context.Background()
	p, err := h.store.GetPool(ctx, pool)
	if err != nil {
		h.t.Fatal(err)
	}
	p.Budget = nil
	if monthly > 0 {
		p.Budget = &api.Budget{Monthly: monthly}
	}
	if err := h.store.UpsertPool(ctx, p); err != nil {
		h.t.Fatal(err)
	}
	if err := h.api.SyncPools(ctx); err != nil {
		h.t.Fatal(err)
	}
}

// Once a pool's estimated spend reaches its monthly budget, new sandboxes go
// to the next permitted pool and the explanation says why; raising the
// budget puts the pool back in use.
func TestMonthlyBudgetMovesNewWorkToTheNextPool(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	h.sched.SpendInterval = time.Nanosecond // always a fresh estimate
	_, _, token := h.tenant("acme", api.Quota{})
	small := api.CreateSandboxRequest{Resources: &api.Resources{CPU: 1, MemoryMiB: 1024}}

	// The private pool costs 0.04 per CPU-hour; the budget is a sliver of that,
	// reached as soon as anything has run there.
	h.budget("fc", 1e-12)
	first := h.create(token, small)
	h.tick()
	if got := h.get(token, first.ID); got.Pool != "fc" {
		t.Fatalf("with nothing spent the budgeted pool should take work: %+v", got)
	}
	time.Sleep(20 * time.Millisecond)

	var pool api.Pool
	h.must(200, "GET", "/v1/admin/pools/fc", operatorToken, nil, &pool)
	if pool.Spend == nil || !pool.Spend.BudgetReached || pool.Spend.Estimated <= 0 || pool.Spend.Month != time.Now().UTC().Format("2006-01") {
		t.Fatalf("pool should report this month's spend and the reached budget: %+v", pool.Spend)
	}

	second := h.create(token, small)
	h.tick()
	got := h.get(token, second.ID)
	if got.Pool != "burst" || got.Decision == nil {
		t.Fatalf("new work should move to the next pool: %+v", got)
	}
	var why string
	for _, c := range got.Decision.Candidates {
		if c.Pool == "fc" {
			why = strings.Join(c.Reasons, "; ")
		}
	}
	if !strings.HasPrefix(why, "monthly budget reached: estimated ") {
		t.Fatalf("explanation for the held pool: %q", why)
	}
	// The sandbox already on the pool is untouched.
	if got := h.get(token, first.ID); got.Status != api.StatusRunning || got.Pool != "fc" {
		t.Fatalf("a reached budget must not disturb running work: %+v", got)
	}
	// private-only work waits for the budget instead of leaving.
	secret := h.create(token, api.CreateSandboxRequest{Classification: "private-only", Resources: small.Resources})
	h.tick()
	if got := h.get(token, secret.ID); got.Status != api.StatusQueued {
		t.Fatalf("private-only work left or failed: %+v", got)
	}

	// Raising the budget lets the pool take work again, the waiting sandbox included.
	h.budget("fc", 1000)
	third := h.create(token, small)
	// The waiting sandbox is due again after the retry delay; make it due now.
	_, _ = h.store.UpdateSandbox(context.Background(), secret.ID, func(r *store.SandboxRecord) error { r.NextAttemptAt = time.Now(); return nil })
	h.tick()
	if got := h.get(token, third.ID); got.Pool != "fc" {
		t.Fatalf("after the budget was raised: %+v", got)
	}
	if got := h.get(token, secret.ID); got.Pool != "fc" {
		t.Fatalf("the waiting private-only sandbox should run once there is budget: %+v", got)
	}
	// A pool without a budget reports no spend.
	var other api.Pool
	h.must(200, "GET", "/v1/admin/pools/burst", operatorToken, nil, &other)
	if other.Budget != nil || other.Spend != nil {
		t.Fatalf("unexpected budget or spend on burst: %+v / %+v", other.Budget, other.Spend)
	}
}

// Operators set a budget through the pool API. It needs prices, is refused
// on a pool declared in the config file, and 0 removes it.
func TestPoolBudgetThroughAPI(t *testing.T) {
	wiring.Register()
	h := newHarness(t, api.PlacementPolicy{})
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapterhttp.Handler(fake.New(fake.Options{}), "").ServeHTTP(w, r)
	}))
	t.Cleanup(plugin.Close)
	body := func(name string, extra map[string]any) map[string]any {
		b := map[string]any{"name": name, "adapter": "remote", "trust": "external", "config": map[string]string{"endpoint": plugin.URL}}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	var raw string
	if code := h.call("POST", "/v1/admin/pools", operatorToken, body("unpriced", map[string]any{"budget": map[string]float64{"monthly": 500}}), &raw); code != 400 || !strings.Contains(raw, "needs a costModel") {
		t.Fatalf("a budget without prices: %d %s", code, raw)
	}
	var pool api.Pool
	h.must(201, "POST", "/v1/admin/pools", operatorToken, body("vendor", map[string]any{
		"costModel": map[string]float64{"cpuHour": 0.0504, "memoryGiBHour": 0.0162}, "budget": map[string]float64{"monthly": 500},
	}), &pool)
	if pool.Budget == nil || pool.Budget.Monthly != 500 {
		t.Fatalf("created pool: %+v", pool.Budget)
	}
	patch := func(want int, name string, change map[string]any) api.Pool {
		t.Helper()
		var updated api.Pool
		var raw string
		if got := h.call("PATCH", "/v1/admin/pools/"+name, operatorToken, change, &raw); got != want {
			t.Fatalf("PATCH %s %v: status %d, want %d: %s", name, change, got, want, raw)
		}
		if want == 200 {
			h.must(200, "GET", "/v1/admin/pools/"+name, operatorToken, nil, &updated)
		}
		return updated
	}
	if got := patch(200, "vendor", map[string]any{"budget": map[string]float64{"monthly": 750}}); got.Budget == nil || got.Budget.Monthly != 750 || got.Spend == nil || got.Spend.BudgetReached {
		t.Fatalf("raised budget: %+v / %+v", got.Budget, got.Spend)
	}
	patch(400, "vendor", map[string]any{"budget": map[string]float64{"monthly": -5}})
	// Dropping the prices under a budget would leave one that is never reached.
	patch(400, "vendor", map[string]any{"costModel": map[string]float64{}})
	if got := patch(200, "vendor", map[string]any{"maxConcurrency": 3}); got.Budget == nil || got.Budget.Monthly != 750 {
		t.Fatalf("an unrelated update changed the budget: %+v", got.Budget)
	}
	if got := patch(200, "vendor", map[string]any{"budget": map[string]float64{"monthly": 0}}); got.Budget != nil || got.Spend != nil {
		t.Fatalf("0 must remove the budget: %+v / %+v", got.Budget, got.Spend)
	}
	patch(409, "fc", map[string]any{"budget": map[string]float64{"monthly": 100}}) // declared in config
}
