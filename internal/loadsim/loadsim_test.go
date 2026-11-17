package loadsim

import (
	"context"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// fast is a world whose users act in milliseconds, so a test sees many
// sessions in a second of real time.
func fast(cfg Config) Config {
	cfg.ThinkStep, cfg.Heartbeat, cfg.Retry = 5*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	return cfg
}

func run(t *testing.T, cfg Config, load Load, d time.Duration) (*World, Snapshot) {
	t.Helper()
	w, err := New(fast(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.Start(ctx)
	w.SetLoad(load)
	w.Spike(1)
	deadline := time.Now().Add(d)
	var snap Snapshot
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		s := w.Snapshot()
		s.Events = append(snap.Events, s.Events...)
		snap = s
	}
	return w, snap
}

func held(s Snapshot, pool string) int {
	n := 0
	for _, b := range s.Boxes {
		if b.Pool == pool && !b.SlotReleased && b.Status != string(api.StatusQueued) {
			n++
		}
	}
	return n
}

// Six users on two private slots: two computers land on the private pool and
// the rest overflow, through the real API and scheduler.
func TestOverflowsPastThePrivatePool(t *testing.T) {
	_, s := run(t, Config{Slots: 2, Overflow: true}, Load{Users: 6, SessionsPerHour: 3600, SessionMinutes: 1}, 1500*time.Millisecond)
	if s.Stats.Sessions == 0 {
		t.Fatalf("no session was served: %+v", s)
	}
	if got := held(s, PrivatePool); got != 2 {
		t.Fatalf("the private pool should hold its 2 slots, holds %d: %+v", got, s.Boxes)
	}
	if got := held(s, ExternalPool); got != 4 || s.Stats.Overflowed != 4 {
		t.Fatalf("4 computers should overflow, got %d (counted %d)", got, s.Stats.Overflowed)
	}
	if len(s.Events) == 0 {
		t.Fatal("the audit log should have told the page what happened")
	}
}

// Without overflow the same users queue: nothing leaves the private pool.
func TestPrivateOnlyQueues(t *testing.T) {
	_, s := run(t, Config{Slots: 2}, Load{Users: 6, SessionsPerHour: 3600, SessionMinutes: 1}, time.Second)
	if got := held(s, ExternalPool); got != 0 {
		t.Fatalf("policy external: never must keep everything private, %d ran outside", got)
	}
	if s.Waiting != 4 {
		t.Fatalf("4 users should be waiting for a slot, got %d", s.Waiting)
	}
}

// An offline private backend sends new computers to the overflow pool, and
// the pool reports unhealthy.
func TestOutageOverflows(t *testing.T) {
	w, err := New(fast(Config{Slots: 4, Overflow: true}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.SetOutage(true)
	w.SetLoad(Load{Users: 2, SessionsPerHour: 3600, SessionMinutes: 1})
	w.Spike(1)
	time.Sleep(time.Second)
	s := w.Snapshot()
	if !s.Outage || s.Pools[0].Name != PrivatePool || s.Pools[0].Healthy {
		t.Fatalf("the private pool should show as unhealthy: %+v", s.Pools)
	}
	if got := held(s, ExternalPool); got != 2 {
		t.Fatalf("both computers should run on the overflow pool, got %d: %+v", got, s.Boxes)
	}
}
