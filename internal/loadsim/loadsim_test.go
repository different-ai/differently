package loadsim

import (
	"context"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

const (
	own   = "own-cluster"
	burst = "burst-provider"
	third = "third-provider"
)

// fast is a world whose users act in milliseconds, so a test sees many
// sessions in a second of real time.
func fast(cfg Config) Config {
	cfg.ThinkStep, cfg.Heartbeat, cfg.Retry = 5*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	return cfg
}

// byPolicy is routing by placement policy over the default two pools, with slots
// on the private one.
func byPolicy(slots int, external string) Routing {
	return Routing{Mode: RoutePolicy, External: external, Providers: map[string]ProviderRouting{
		own: {Enabled: true, Slots: slots}, burst: {Enabled: true},
	}}
}

func start(t *testing.T, cfg Config, r Routing, load Load) *World {
	t.Helper()
	w, err := New(fast(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.Start(ctx)
	w.SetLoad(load)
	w.Spike(1)
	return w
}

// watch snapshots the world for d, keeping every event.
func watch(w *World, d time.Duration) Snapshot {
	deadline := time.Now().Add(d)
	var snap Snapshot
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		s := w.Snapshot()
		s.Events = append(snap.Events, s.Events...)
		snap = s
	}
	return snap
}

func pool(t *testing.T, s Snapshot, name string) PoolView {
	t.Helper()
	for _, p := range s.Pools {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no pool %s in %+v", name, s.Pools)
	return PoolView{}
}

var everyone = Load{Users: 6, SessionsPerHour: 3600, SessionMinutes: 1, Keep: 1}

// Six users on two private slots: two computers land on the private pool and
// the rest overflow, through the real API and scheduler.
func TestOverflowsPastThePrivatePool(t *testing.T) {
	s := watch(start(t, Config{}, byPolicy(2, api.ExternalOverflow), everyone), 1500*time.Millisecond)
	if s.Stats.Sessions == 0 {
		t.Fatalf("no session was served: %+v", s)
	}
	if got := pool(t, s, own); got.Held != 2 || got.Slots != 2 {
		t.Fatalf("the private pool should hold its 2 slots: %+v", got)
	}
	if got := pool(t, s, burst); got.Held != 4 || got.Placed != 4 {
		t.Fatalf("4 computers should overflow: %+v", got)
	}
	if len(s.Events) == 0 {
		t.Fatal("the audit log should have told the page what happened")
	}
}

// Without overflow the same users queue: nothing leaves the private pool.
func TestPrivateOnlyQueues(t *testing.T) {
	s := watch(start(t, Config{}, byPolicy(2, api.ExternalNever), everyone), time.Second)
	if got := pool(t, s, burst); got.Held != 0 {
		t.Fatalf("policy external: never must keep everything private: %+v", got)
	}
	if s.Waiting != 4 || s.Queued != 4 {
		t.Fatalf("4 users should be waiting for a slot, got %d waiting, %d queued", s.Waiting, s.Queued)
	}
}

// An offline private backend sends new computers to the overflow pool, and
// the pool reports unhealthy.
func TestOutageOverflows(t *testing.T) {
	w, err := New(fast(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetRouting(byPolicy(4, api.ExternalOverflow)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.SetOutage(own, true)
	w.SetLoad(Load{Users: 2, SessionsPerHour: 3600, SessionMinutes: 1, Keep: 1})
	w.Spike(1)
	s := watch(w, time.Second)
	if got := pool(t, s, own); !got.Offline || got.Healthy {
		t.Fatalf("the private pool should show as unhealthy: %+v", got)
	}
	if got := pool(t, s, burst); got.Held != 2 {
		t.Fatalf("both computers should run on the overflow pool: %+v", got)
	}
}

// In split mode every create is pinned by share: a pool with no share gets
// nothing, and the others take work in proportion.
func TestSplitFollowsShares(t *testing.T) {
	cfg := Config{Seed: 3, Providers: []Provider{
		{Name: own, Trust: api.TrustPrivate},
		{Name: burst, Trust: api.TrustExternal},
		{Name: third, Trust: api.TrustExternal},
	}}
	r := Routing{Mode: RouteSplit, External: api.ExternalAllowed, Providers: map[string]ProviderRouting{
		own: {Enabled: true, Share: 75}, burst: {Enabled: true, Share: 25}, third: {Enabled: true, Share: 0},
	}}
	w := start(t, cfg, r, Load{Users: 60, SessionsPerHour: 3600, SessionMinutes: 1, Keep: 1})
	var s Snapshot
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && s.Stats.Placed < 60; {
		s = watch(w, 200*time.Millisecond)
	}
	a, b, c := pool(t, s, own), pool(t, s, burst), pool(t, s, third)
	if a.Placed+b.Placed != 60 || c.Placed != 0 {
		t.Fatalf("60 computers on the two pools with a share: %d, %d, %d", a.Placed, b.Placed, c.Placed)
	}
	if a.Placed < 36 || a.Placed > 54 {
		t.Fatalf("a 75%% share of 60 should be near 45, got %d", a.Placed)
	}
}

// A create pinned to an external pool is refused while policy keeps work
// private, and the page hears why.
func TestSplitObeysThePolicy(t *testing.T) {
	r := Routing{Mode: RouteSplit, External: api.ExternalNever, Providers: map[string]ProviderRouting{
		own: {Enabled: true, Share: 0}, burst: {Enabled: true, Share: 100},
	}}
	s := watch(start(t, Config{}, r, everyone), 700*time.Millisecond)
	if s.Stats.Refused == 0 || s.Stats.Placed != 0 {
		t.Fatalf("pinned to an external pool under external: never: %d refused, %d placed", s.Stats.Refused, s.Stats.Placed)
	}
	refused := false
	for _, e := range s.Events {
		refused = refused || e.Action == "sandbox.rejected"
	}
	if !refused {
		t.Fatalf("the audit log records each refusal: %+v", s.Events)
	}
}

// Disabling a pool while the world runs stops new work there and leaves what
// runs on it alone; its backend is still the one that holds those sandboxes.
func TestDisableAPoolLive(t *testing.T) {
	w := start(t, Config{}, byPolicy(2, api.ExternalOverflow), everyone)
	if s := watch(w, 700*time.Millisecond); pool(t, s, burst).Held != 4 {
		t.Fatalf("setup: %+v", s.Pools)
	}
	r := byPolicy(2, api.ExternalOverflow)
	r.Providers[burst] = ProviderRouting{Enabled: false}
	if err := w.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	w.SetLoad(Load{Users: 9, SessionsPerHour: 3600, SessionMinutes: 1, Keep: 1})
	w.Spike(1)
	s := watch(w, 700*time.Millisecond)
	if got := pool(t, s, burst); got.Enabled || got.Held != 4 || got.Placed != 4 {
		t.Fatalf("a disabled pool keeps its sandboxes and takes no more: %+v", got)
	}
	if s.Queued != 3 {
		t.Fatalf("the three newcomers should queue, got %d", s.Queued)
	}
	if s.Working < 6 {
		t.Fatalf("the sessions on the disabled pool go on: %d working", s.Working)
	}
}

// Tasks delete their sandbox when the session ends, so sandboxes turn over.
func TestTasksAreDeleted(t *testing.T) {
	load := Load{Users: 4, SessionsPerHour: 3600 * 20, SessionMinutes: 0.0005}
	w, err := New(fast(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetRouting(byPolicy(8, api.ExternalNever)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.SetLoad(load)
	s := watch(w, 2*time.Second)
	if s.Stats.Placed <= 4 || len(s.Boxes) > 4 {
		t.Fatalf("4 users should have run more than 4 sandboxes, never more than 4 at once: %d placed, %d live", s.Stats.Placed, len(s.Boxes))
	}
}

// The page's ranking is the policy's: private first, then the cheaper
// provider; while a private slot is free the providers are held for overflow,
// and a disabled pool is not a candidate.
func TestRankingIsThePolicys(t *testing.T) {
	cfg := Config{Providers: []Provider{
		{Name: own, Trust: api.TrustPrivate},
		{Name: burst, Trust: api.TrustExternal, CostPerHour: 0.08},
		{Name: third, Trust: api.TrustExternal, CostPerHour: 0.03},
	}}
	r := Routing{Mode: RoutePolicy, External: api.ExternalOverflow, Providers: map[string]ProviderRouting{
		own: {Enabled: true, Slots: 2}, burst: {Enabled: true}, third: {Enabled: true},
	}}
	w := start(t, cfg, r, Load{Users: 1, SessionsPerHour: 3600, SessionMinutes: 1, Keep: 1})
	order := func(s Snapshot) (ranked []string, held map[string]Rank) {
		held = map[string]Rank{}
		for _, k := range s.Ranking {
			if k.Rank > 0 {
				ranked = append(ranked, k.Pool)
			} else {
				held[k.Pool] = k
			}
		}
		return ranked, held
	}
	ranked, held := order(watch(w, 600*time.Millisecond))
	if len(ranked) != 1 || ranked[0] != own || len(held[burst].Reasons) == 0 || !held[burst].Eligible {
		t.Fatalf("with a private slot free only the private pool ranks, and the providers say why they wait: %v %+v", ranked, held)
	}
	w.SetLoad(everyone)
	w.Spike(1)
	ranked, _ = order(watch(w, 800*time.Millisecond))
	if len(ranked) != 2 || ranked[0] != third || ranked[1] != burst {
		t.Fatalf("with the private pool full, the cheaper provider ranks first: %v", ranked)
	}
	r.Providers[third] = ProviderRouting{Enabled: false}
	if err := w.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	ranked, held = order(watch(w, 300*time.Millisecond))
	if len(ranked) != 1 || ranked[0] != burst || held[third].Eligible {
		t.Fatalf("a disabled pool is no candidate: %v %+v", ranked, held)
	}
}

// A provider added while the world runs takes work: here the overflow of a
// full private pool, which had been queueing with nowhere to go.
func TestAddAProviderLive(t *testing.T) {
	cfg := Config{Providers: []Provider{{Name: own, Trust: api.TrustPrivate}}}
	r := Routing{Mode: RoutePolicy, External: api.ExternalOverflow, Providers: map[string]ProviderRouting{own: {Enabled: true, Slots: 2}}}
	w := start(t, cfg, r, everyone)
	if s := watch(w, 600*time.Millisecond); s.Queued != 4 {
		t.Fatalf("setup: 4 should queue on a full private pool, got %d", s.Queued)
	}
	r.Providers[third] = ProviderRouting{Enabled: true, Slots: 8, Share: 1}
	if err := w.AddProvider(Provider{Name: third, Trust: api.TrustExternal, Slots: 8, CostPerHour: 0.05}, r); err != nil {
		t.Fatal(err)
	}
	var s Snapshot // queued creates are looked at again after a back-off of seconds
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && (len(s.Pools) < 2 || pool(t, s, third).Held < 4); {
		s = watch(w, 300*time.Millisecond)
	}
	if got := pool(t, s, third); got.Held != 4 || got.Slots != 8 || got.Cost != 0.05 {
		t.Fatalf("the new provider should take the 4 that waited: %+v", got)
	}
	if err := w.AddProvider(Provider{Name: third}, r); err == nil {
		t.Fatal("a second provider of the same name must be refused")
	}
}

// Kept sandboxes hibernate when idle and are woken by their user's next
// session; one left hibernated past its limit is deleted and replaced.
func TestKeptSandboxesHibernateAndWake(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the control plane's one-minute idle timeout")
	}
	cfg := fast(Config{MaxPausedSeconds: 60})
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.sched.Config.Scheduler.ReconcileIntervalSecs = 1
	if err := w.SetRouting(byPolicy(4, api.ExternalNever)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	// One short session, then nothing: the sandbox is kept and goes idle.
	w.SetLoad(Load{Users: 1, SessionsPerHour: 0, SessionMinutes: 0.0005, Keep: 1})
	w.Spike(1)
	var s Snapshot
	for deadline := time.Now().Add(100 * time.Second); time.Now().Before(deadline) && s.Stats.Hibernations == 0; {
		s = watch(w, time.Second)
	}
	if s.Stats.Hibernations != 1 || pool(t, s, own).Checkpointed != 1 {
		t.Fatalf("an idle kept sandbox should hibernate and give its slot back: %+v %+v", s.Stats, s.Pools)
	}
	w.Spike(1)
	if s = watch(w, 2*time.Second); s.Stats.Wakes != 1 || s.Stats.Placed != 1 {
		t.Fatalf("the next session wakes the same sandbox: %+v", s.Stats)
	}
}

// The tenant's quota is the control plane's: over maxActiveSandboxes a
// sandbox stays queued though pools have room, and over maxQueued a create is
// refused. The audit log's chain verifies.
func TestQuotaAndAudit(t *testing.T) {
	w, err := New(fast(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetRouting(byPolicy(8, api.ExternalNever)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetQuota(Quota{MaxActive: 2, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	w.SetLoad(everyone)
	w.Spike(1)
	s := watch(w, time.Second)
	if s.Active != 2 || pool(t, s, own).Held != 2 {
		t.Fatalf("2 sandboxes at once, on a pool with 8 slots: %d active, %+v", s.Active, pool(t, s, own))
	}
	if s.Queued != 1 || s.Stats.OverQuota == 0 {
		t.Fatalf("one may queue and the rest are refused: %d queued, %+v", s.Queued, s.Stats)
	}
	if s.Quota != (Quota{MaxActive: 2, MaxQueued: 1}) {
		t.Fatalf("the snapshot reports the quota: %+v", s.Quota)
	}
	if check := w.VerifyAudit(); !check.OK || check.Events == 0 || check.HeadHash == "" {
		t.Fatalf("the audit chain should verify: %+v", check)
	}
	chained := false
	for _, e := range s.Events {
		chained = chained || (e.Action == "sandbox.placed" && e.Seq > 0 && e.Hash != "")
	}
	if !chained {
		t.Fatalf("a decision carries its place in the chain: %+v", s.Events)
	}
	// With every event shown, the creates and execs that are audited too appear.
	w.SetAuditAll(true)
	if err := w.SetQuota(Quota{}); err != nil {
		t.Fatal(err)
	}
	w.Spike(1)
	seen := false
	for _, e := range watch(w, time.Second).Events {
		seen = seen || e.Action == "sandbox.create"
	}
	if !seen {
		t.Fatal("with every event shown, a create appears")
	}
}
