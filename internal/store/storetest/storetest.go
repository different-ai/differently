// Package storetest is a conformance suite every Store implementation runs.
package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/audit"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// Run executes the suite. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("ConcurrentAdmissionRespectsQueueLimit", func(t *testing.T) { concurrentAdmission(t, newStore(t)) })
	t.Run("ConcurrentActivationRespectsQuota", func(t *testing.T) { concurrentActivation(t, newStore(t)) })
	t.Run("PoolConcurrencyCap", func(t *testing.T) { poolCap(t, newStore(t)) })
	t.Run("Idempotency", func(t *testing.T) { idempotency(t, newStore(t)) })
	t.Run("FairOrderAcrossOrgs", func(t *testing.T) { fairOrder(t, newStore(t)) })
	t.Run("AuditChain", func(t *testing.T) { auditChain(t, newStore(t)) })
	t.Run("ProjectDeleteGuard", func(t *testing.T) { projectGuard(t, newStore(t)) })
	t.Run("ConcurrentStartRespectsCPUQuota", func(t *testing.T) { concurrentStart(t, newStore(t)) })
}

func seed(t *testing.T, s store.Store, org string) (api.Organization, api.Project) {
	t.Helper()
	ctx := context.Background()
	o := api.Organization{ID: org, Name: org, CreatedAt: time.Now().UTC()}
	if err := s.CreateOrg(ctx, o); err != nil {
		t.Fatal(err)
	}
	p := api.Project{ID: org + "-proj", OrgID: org, Name: "default", CreatedAt: time.Now().UTC()}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	return o, p
}

func sandbox(p api.Project, cpu int) store.SandboxRecord {
	now := time.Now().UTC()
	return store.SandboxRecord{Sandbox: api.Sandbox{
		ID: id.New("sbx"), OrgID: p.OrgID, ProjectID: p.ID, Resources: api.Resources{CPU: cpu, MemoryMiB: 512, DiskGiB: 1},
		Classification: "internal", CreatedAt: now, UpdatedAt: now,
	}}
}

func concurrentAdmission(t *testing.T, s store.Store) {
	_, p := seed(t, s, "org-a")
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.AdmitSandbox(context.Background(), sandbox(p, 1), store.Admission{OrgQuota: api.Quota{MaxQueued: 10}}); err == nil {
				ok.Add(1)
			} else if !api.IsCode(err, api.CodeQuotaExceeded) {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("admitted %d, want exactly 10", ok.Load())
	}
}

func concurrentActivation(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-b")
	var ids []string
	for i := 0; i < 20; i++ {
		rec, _, err := s.AdmitSandbox(ctx, sandbox(p, 2), store.Admission{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	quota := api.Quota{MaxActiveSandboxes: 5, MaxCPU: 6}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for _, sid := range ids {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			_, err := s.ActivateSandbox(ctx, sid, store.Activation{Pool: "p1", OrgQuota: quota, Now: time.Now().UTC()})
			if err == nil {
				ok.Add(1)
			}
		}(sid)
	}
	wg.Wait()
	// CPU 6 / 2 per sandbox = 3, tighter than the active limit of 5.
	if ok.Load() != 3 {
		t.Fatalf("activated %d, want exactly 3", ok.Load())
	}
	u, err := s.Usage(ctx, p.OrgID, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.CPU != 6 || u.Queued != 17 || u.ActiveSandboxes != 3 {
		t.Fatalf("usage %+v", u)
	}
	// Terminating releases quota with no counter to fix up.
	var active string
	recs, _ := s.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusProvisioning}})
	active = recs[0].ID
	if _, err := s.UpdateSandbox(ctx, active, func(r *store.SandboxRecord) error { r.Status = api.StatusTerminated; return nil }); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Usage(ctx, p.OrgID, ""); u.CPU != 4 {
		t.Fatalf("cpu after release %d", u.CPU)
	}
}

func poolCap(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-c")
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		rec, _, err := s.AdmitSandbox(ctx, sandbox(p, 1), store.Admission{})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			_, err := s.ActivateSandbox(ctx, sid, store.Activation{Pool: "small", MaxConcurrency: 2, Now: time.Now().UTC()})
			if err == nil {
				ok.Add(1)
			} else if !api.IsCode(err, "insufficient_capacity") {
				t.Errorf("unexpected %v", err)
			}
		}(rec.ID)
	}
	wg.Wait()
	if ok.Load() != 2 {
		t.Fatalf("pool admitted %d, want 2", ok.Load())
	}
	counts, _ := s.PoolActive(ctx)
	if counts["small"] != 2 {
		t.Fatalf("pool active %v", counts)
	}
}

func idempotency(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-d")
	a := sandbox(p, 1)
	a.IdempotencyKey = "req-1"
	first, existed, err := s.AdmitSandbox(ctx, a, store.Admission{})
	if err != nil || existed {
		t.Fatal(err, existed)
	}
	b := sandbox(p, 1)
	b.IdempotencyKey = "req-1"
	second, existed, err := s.AdmitSandbox(ctx, b, store.Admission{})
	if err != nil || !existed || second.ID != first.ID {
		t.Fatalf("idempotent replay returned %s existed=%v err=%v", second.ID, existed, err)
	}
}

func fairOrder(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, big := seed(t, s, "org-big")
	_, small := seed(t, s, "org-small")
	for i := 0; i < 6; i++ {
		if _, _, err := s.AdmitSandbox(ctx, sandbox(big, 1), store.Admission{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AdmitSandbox(ctx, sandbox(small, 1), store.Admission{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueuedCandidates(ctx, time.Now().UTC().Add(time.Second), 3)
	if err != nil {
		t.Fatal(err)
	}
	seenSmall := false
	for _, r := range got {
		seenSmall = seenSmall || r.OrgID == "org-small"
	}
	if !seenSmall {
		t.Fatalf("small org starved behind big org's burst: %v", got)
	}
}

func auditChain(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.AppendAudit(ctx, api.AuditEvent{Actor: "key_x", Action: "sandbox.create", Resource: "sbx", Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if res := audit.Verify(events); !res.OK || res.Events != 5 {
		t.Fatalf("chain %+v", res)
	}
}

func concurrentStart(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-f")
	var ids []string
	for i := 0; i < 6; i++ {
		rec, _, err := s.AdmitSandbox(ctx, sandbox(p, 2), store.Admission{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ActivateSandbox(ctx, rec.ID, store.Activation{Pool: "p", Now: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error { r.Status = api.StatusStopped; return nil }); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for _, sid := range ids {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			if _, err := s.ReserveCompute(ctx, sid, api.StatusStopped, api.StatusRunning, api.Quota{MaxCPU: 6}, api.Quota{}, time.Now().UTC()); err == nil {
				ok.Add(1)
			} else if !api.IsCode(err, api.CodeQuotaExceeded) {
				t.Errorf("unexpected %v", err)
			}
		}(sid)
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("started %d stopped sandboxes under a 6-CPU quota, want 3", ok.Load())
	}
}

func projectGuard(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-e")
	if _, _, err := s.AdmitSandbox(ctx, sandbox(p, 1), store.Admission{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}
