package demo

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/api"
)

// The presenter's talk tracks are claims about the real control plane. This
// runs it in process on the demo configuration, the way --demo starts it,
// and checks each claim with requests the generator would send.
type plane struct {
	t     *testing.T
	gen   *Generator
	sched *scheduler.Scheduler
	pools map[string]config.PoolConfig
}

func startPlane(t *testing.T) *plane {
	t.Helper()
	t.Setenv("DIFFERENTLY_BOOTSTRAP_TOKEN", "") // dev mode writes its own token
	dir := t.TempDir()
	cfg := Config(dir)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	rec, err := auditlog.New(st, log, "")
	if err != nil {
		t.Fatal(err)
	}
	wiring.Register()
	pm := pools.NewManager(pools.Deps{Store: st, Dev: true, DataDir: dir, Logger: log})
	inst, err := st.Installation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(st, pm, cfg, inst, rec, log)
	srv := server.New(cfg, st, pm, sched, rec, log, inst)
	if err := srv.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	pm.Refresh(ctx)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	p := &plane{t: t, gen: NewGenerator(ts.URL, strings.TrimSpace(string(token)), log), sched: sched, pools: map[string]config.PoolConfig{}}
	for _, pc := range cfg.Pools {
		p.pools[pc.Name] = pc
	}
	return p
}

func kindNamed(t *testing.T, name string) kind {
	t.Helper()
	for _, k := range kinds() {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("no workload %q", name)
	return kind{}
}

// create sends n of a workload's requests, as the generator does, and runs
// the scheduler until they settle.
func (p *plane) create(workload string, n int) []string {
	p.t.Helper()
	k := kindNamed(p.t, workload)
	client := &http.Client{Timeout: 5 * time.Second}
	var ids []string
	for range n {
		req := k.Req
		req.ProjectID = Project
		req.Labels = map[string]string{"workload": k.Name}
		var sb api.Sandbox
		if code := p.gen.do(context.Background(), client, http.MethodPost, "/v1/sandboxes", req, &sb); code != http.StatusAccepted {
			p.t.Fatalf("%s: create answered %d", workload, code)
		}
		ids = append(ids, sb.ID)
	}
	for range 3 {
		p.sched.Tick(context.Background())
		p.sched.Drain()
	}
	return ids
}

func (p *plane) sandbox(id string) api.Sandbox {
	p.t.Helper()
	var sb api.Sandbox
	if code := p.gen.do(context.Background(), http.DefaultClient, http.MethodGet, "/v1/sandboxes/"+id, nil, &sb); code != http.StatusOK {
		p.t.Fatalf("get %s: %d", id, code)
	}
	return sb
}

// each checks every sandbox against a claim about the pool it landed on.
func (p *plane) each(ids []string, claim string, ok func(sb api.Sandbox, pool config.PoolConfig) bool) {
	p.t.Helper()
	for _, id := range ids {
		sb := p.sandbox(id)
		if !ok(sb, p.pools[sb.Pool]) {
			p.t.Errorf("%s: %s is %s on pool %q", claim, id, sb.Status, sb.Pool)
		}
	}
}

func (p *plane) waitEnabled(name string, want bool) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var pool api.Pool
		p.gen.do(context.Background(), http.DefaultClient, http.MethodGet, "/v1/admin/pools/"+name, nil, &pool)
		if pool.Enabled == want {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("pool %s: enabled is %v, want %v", name, pool.Enabled, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isPrivate(_ api.Sandbox, pool config.PoolConfig) bool {
	return pool.Trust == api.TrustPrivate
}

func TestTalkTracksHoldOnTheRealControlPlane(t *testing.T) {
	p := startPlane(t)
	slots := 0
	for _, pc := range p.pools {
		if pc.Trust == api.TrustPrivate {
			slots += pc.MaxConcurrency
		}
	}

	// A normal day: while your own capacity lasts, no vendor sees any work.
	p.each(p.create("code-review-agent", slots), "a normal day stays private", func(sb api.Sandbox, pool config.PoolConfig) bool {
		return sb.Status == api.StatusRunning && pool.Trust == api.TrustPrivate
	})

	// Quarter-end peak: your capacity is full now.
	dealRoom := p.create("deal-room-analysis", 3)
	p.each(dealRoom, "deal-room work waits for your capacity", func(sb api.Sandbox, _ config.PoolConfig) bool {
		return sb.Status == api.StatusQueued && sb.Pool == ""
	})
	p.each(p.create("code-review-agent", 4), "eligible work overflows to an approved vendor", func(sb api.Sandbox, pool config.PoolConfig) bool {
		return sb.Status == api.StatusRunning && pool.Trust == api.TrustExternal
	})

	// Regulated workload: only SOC 2 Type II with a signed BAA.
	p.each(p.create("claims-processing", 4), "claims land only where both attestations are held", func(sb api.Sandbox, pool config.PoolConfig) bool {
		_, soc2 := pool.Attestations["soc2-type2"]
		_, baa := pool.Attestations["hipaa-baa"]
		return sb.Status == api.StatusRunning && soc2 && baa
	})

	// EU data residency: EU regions only, whatever the price.
	p.each(p.create("eu-customer-analytics", 4), "EU analytics stays in EU regions", func(sb api.Sandbox, pool config.PoolConfig) bool {
		return sb.Status == api.StatusRunning && pool.Region == "eu"
	})

	// Vendor incident: the presenter switches two vendors off.
	incident := Scenarios[slices.IndexFunc(Scenarios, func(s Scenario) bool { return s.ID == "incident" })]
	if !p.gen.Set(incident.ID) {
		t.Fatal("no incident scenario")
	}
	for _, name := range incident.Down {
		p.waitEnabled(name, false)
	}
	p.each(p.create("code-review-agent", 4), "work re-routes around the vendors that are off", func(sb api.Sandbox, _ config.PoolConfig) bool {
		return sb.Status == api.StatusRunning && !slices.Contains(incident.Down, sb.Pool)
	})
	p.each(p.create("claims-processing", 2), "claims wait when no vendor with a BAA is left", func(sb api.Sandbox, _ config.PoolConfig) bool {
		return sb.Status == api.StatusQueued && sb.Pool == ""
	})
	p.each(dealRoom, "deal-room work never left", func(sb api.Sandbox, pool config.PoolConfig) bool {
		return sb.Pool == "" || isPrivate(sb, pool)
	})

	// Back to a normal day: every pool is on again.
	p.gen.Set("steady")
	for _, name := range incident.Down {
		p.waitEnabled(name, true)
	}
}

// A normal day's traffic must fit in the private slots even when every
// sandbox lives as long as it can; otherwise its talk track is false.
func TestNormalDayFitsPrivateCapacity(t *testing.T) {
	slots := 0
	for _, pc := range Config(t.TempDir()).Pools {
		if pc.Trust == api.TrustPrivate {
			slots += pc.MaxConcurrency
		}
	}
	steady := Scenarios[0]
	if steady.ID != "steady" {
		t.Fatalf("the first scenario is %s", steady.ID)
	}
	if need := steady.Rate * MaxLife.Seconds(); need > float64(slots) {
		t.Fatalf("a normal day keeps up to %.1f sandboxes running; the private pools have %d slots", need, slots)
	}
}

// The regulated scenario says Freestyle and Northflank stay empty: its mix
// may hold only work that cannot run there.
func TestRegulatedMixStaysOffUnattestedVendors(t *testing.T) {
	c := Config(t.TempDir())
	regulated := Scenarios[slices.IndexFunc(Scenarios, func(s Scenario) bool { return s.ID == "regulated" })]
	for name, weight := range regulated.Mix {
		if weight == 0 {
			continue
		}
		class, _ := c.Classification(kindNamed(t, name).Req.Classification)
		if class.External != api.ExternalNever && len(class.RequiredAttestations) == 0 {
			t.Errorf("%s (%s) could land on any vendor", name, class.Name)
		}
	}
}
