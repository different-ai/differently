package server_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/pkg/api"
)

// stuckHealth is a pool whose backend stops answering: its health probe hangs
// until the probe's own timeout.
type stuckHealth struct {
	*fake.Adapter
	stuck atomic.Bool
}

func (a *stuckHealth) Health(ctx context.Context) error {
	if a.stuck.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// One pool that stops answering must not slow placement on the others: its
// probe hangs for seconds, and used to hold the scheduler loop while it did.
func TestUnreachablePoolDoesNotStallScheduling(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("acme", api.Quota{})
	slow := &stuckHealth{Adapter: fake.New(fake.Options{Trust: api.TrustPrivate, TotalCPU: 4, TotalMemoryMiB: 16384})}
	pool := api.Pool{Name: "slow", Adapter: "fake", Trust: api.TrustPrivate, Enabled: true, Source: "config"}
	_ = h.store.UpsertPool(context.Background(), pool)
	h.pools.Set(pool, slow)

	h.sched.Config.Scheduler.IntervalMs = 10
	h.sched.RefreshInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.sched.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(50 * time.Millisecond) // the first, healthy probe pass is over
	slow.stuck.Store(true)
	time.Sleep(100 * time.Millisecond) // a probe of the stuck pool is now hanging
	sb := h.create(token, api.CreateSandboxRequest{})
	deadline := time.Now().Add(2 * time.Second)
	for h.get(token, sb.ID).Status != api.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatalf("sandbox still %s after 2s while another pool's probe hangs", h.get(token, sb.ID).Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
