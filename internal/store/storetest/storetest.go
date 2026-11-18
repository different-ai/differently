// Package storetest is a conformance suite every Store implementation runs.
package storetest

import (
	"context"
	"slices"
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
	t.Run("ReleasedSlotsAndReclaim", func(t *testing.T) { releasedSlots(t, newStore(t)) })
	t.Run("Idempotency", func(t *testing.T) { idempotency(t, newStore(t)) })
	t.Run("Secrets", func(t *testing.T) { secrets(t, newStore(t)) })
	t.Run("Stacks", func(t *testing.T) { stacks(t, newStore(t)) })
	t.Run("Templates", func(t *testing.T) { templates(t, newStore(t)) })
	t.Run("ConcurrentTemplateUpdatesKeepEveryImage", func(t *testing.T) { concurrentTemplates(t, newStore(t)) })
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

// releasedSlots: a paused sandbox that released its slot does not count
// toward the pool's cap, and two concurrent resumes racing for the one free
// slot get it exactly once.
func releasedSlots(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, p := seed(t, s, "org-rs")
	// The server writes a row for every pool at startup; Postgres serializes
	// a pool's slots by locking it, so without one the reclaims below race.
	if err := s.UpsertPool(ctx, api.Pool{Name: "ck", Adapter: "fake"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var ids []string
	for i := 0; i < 3; i++ {
		rec, _, err := s.AdmitSandbox(ctx, sandbox(p, 1), store.Admission{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	for _, sid := range ids[:2] {
		if _, err := s.ActivateSandbox(ctx, sid, store.Activation{Pool: "ck", MaxConcurrency: 2, Now: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateSandbox(ctx, sid, func(r *store.SandboxRecord) error {
			r.Status, r.SlotReleased = api.StatusPaused, true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ActivateSandbox(ctx, ids[2], store.Activation{Pool: "ck", MaxConcurrency: 2, Now: now}); err != nil {
		t.Fatalf("released slots should not count: %v", err)
	}
	if counts, _ := s.PoolActive(ctx); counts["ck"] != 1 {
		t.Fatalf("pool active %v, want 1", counts)
	}
	// A pause in progress has not given its slot back yet.
	if _, err := s.UpdateSandbox(ctx, ids[2], func(r *store.SandboxRecord) error {
		r.Status = api.StatusPausing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if counts, _ := s.PoolActive(ctx); counts["ck"] != 1 {
		t.Fatalf("pool active %v with one pausing, want 1", counts)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for _, sid := range ids[:2] {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			_, err := s.ReclaimSlot(ctx, sid, 2, now)
			if err == nil {
				ok.Add(1)
			} else if !api.IsCode(err, "insufficient_capacity") {
				t.Errorf("unexpected %v", err)
			}
		}(sid)
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("reclaimed %d slots, want 1", ok.Load())
	}
	if counts, _ := s.PoolActive(ctx); counts["ck"] != 2 {
		t.Fatalf("pool active %v, want 2", counts)
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
	// A key names a live sandbox: once it has ended, the key creates a new one.
	if _, err := s.UpdateSandbox(ctx, first.ID, func(r *store.SandboxRecord) error { r.Status = api.StatusTerminated; return nil }); err != nil {
		t.Fatal(err)
	}
	c := sandbox(p, 1)
	c.IdempotencyKey = "req-1"
	third, existed, err := s.AdmitSandbox(ctx, c, store.Admission{})
	if err != nil || existed || third.ID == first.ID {
		t.Fatalf("a key whose sandbox ended should admit a new one, got %s existed=%v err=%v", third.ID, existed, err)
	}
}

func stacks(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	st := api.Stack{Name: "prod", Kind: api.StackOpenWork, CreatedAt: now, UpdatedAt: now, Components: []api.StackComponent{
		{Name: "den-api", Role: "den-api", URL: "http://den-api:8788", HealthPath: "/health", ReadyPath: "/ready"},
	}}
	if err := s.PutStack(ctx, st); err != nil {
		t.Fatal(err)
	}
	st.Components = append(st.Components, api.StackComponent{Name: "workbot", URL: "http://workbot:3020", HealthPath: "/healthz", Optional: true})
	if err := s.PutStack(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStack(ctx, "prod")
	if err != nil || len(got.Components) != 2 || !got.Components[1].Optional || !got.CreatedAt.Equal(now) {
		t.Fatalf("replaced stack %+v %v", got, err)
	}
	if err := s.PutStack(ctx, api.Stack{Name: "dev", Kind: api.StackCustom}); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListStacks(ctx); err != nil || len(list) != 2 || list[0].Name != "dev" {
		t.Fatalf("list %v %v", list, err)
	}
	if err := s.DeleteStack(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetStack(ctx, "prod"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleted stack still there: %v", err)
	}
	if err := s.DeleteStack(ctx, "prod"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleting a missing stack: %v", err)
	}
}

func templates(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := s.GetTemplate(ctx, "python"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a template nobody set: %v", err)
	}
	if list, err := s.ListTemplates(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty list %v %v", list, err)
	}
	created, err := s.UpdateTemplate(ctx, "python", func(tpl *api.Template, found bool) error {
		if found || tpl.Name != "" || len(tpl.Images) != 0 {
			t.Errorf("a new template starts empty, got found=%v %+v", found, *tpl)
		}
		tpl.Description, tpl.Source = "Python 3.12", api.TemplateSourceAPI
		tpl.Images = map[string]string{"onprem": "registry.example.com/python:3.12"}
		tpl.UpdatedAt, tpl.UpdatedBy = &now, "key:op"
		return nil
	})
	if err != nil || created.Name != "python" || created.Images["onprem"] == "" {
		t.Fatalf("created %+v %v", created, err)
	}
	// A change starts from what is stored, so adding one image keeps the rest.
	merged, err := s.UpdateTemplate(ctx, "python", func(tpl *api.Template, found bool) error {
		if !found || tpl.Name != "python" || tpl.Images["onprem"] != "registry.example.com/python:3.12" {
			t.Errorf("an existing template arrives as stored, got found=%v %+v", found, *tpl)
		}
		tpl.Images["daytona"] = "python-312"
		return nil
	})
	if err != nil || len(merged.Images) != 2 {
		t.Fatalf("merged %+v %v", merged, err)
	}
	got, err := s.GetTemplate(ctx, "python")
	if err != nil || got.Description != "Python 3.12" || got.Source != api.TemplateSourceAPI || len(got.Images) != 2 ||
		got.Images["daytona"] != "python-312" || got.UpdatedAt == nil || !got.UpdatedAt.Equal(now) || got.UpdatedBy != "key:op" {
		t.Fatalf("stored template %+v %v", got, err)
	}
	// An error from fn changes nothing, for an existing template and a new one.
	refused := api.Errorf(api.CodeInvalidRequest, "refused")
	if _, err := s.UpdateTemplate(ctx, "python", func(tpl *api.Template, _ bool) error {
		tpl.Images = nil
		return refused
	}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("fn's error was not returned: %v", err)
	}
	if got, _ := s.GetTemplate(ctx, "python"); len(got.Images) != 2 {
		t.Fatalf("a refused change was stored: %+v", got)
	}
	if _, err := s.UpdateTemplate(ctx, "ghost", func(*api.Template, bool) error { return refused }); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("fn's error was not returned: %v", err)
	}
	if _, err := s.GetTemplate(ctx, "ghost"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a refused create left a template behind: %v", err)
	}
	// The name is the key: fn cannot store a template under another one.
	if renamed, err := s.UpdateTemplate(ctx, "node", func(tpl *api.Template, _ bool) error {
		tpl.Name, tpl.Images = "other", map[string]string{"onprem": "registry.example.com/node:22"}
		return nil
	}); err != nil || renamed.Name != "node" {
		t.Fatalf("renamed %+v %v", renamed, err)
	}
	if list, err := s.ListTemplates(ctx); err != nil || len(list) != 2 || list[0].Name != "node" || list[1].Name != "python" {
		t.Fatalf("list is not every template in name order: %v %v", list, err)
	}
	if err := s.DeleteTemplate(ctx, "python"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTemplate(ctx, "python"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleted template still there: %v", err)
	}
	if err := s.DeleteTemplate(ctx, "python"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleting a missing template: %v", err)
	}
	if list, _ := s.ListTemplates(ctx); len(list) != 1 || list[0].Name != "node" {
		t.Fatalf("list after delete %v", list)
	}
	// Name order is byte order in every store, whatever a database's
	// collation would make of capitals, digits and punctuation.
	for _, name := range []string{"a_b", "Node", "a:b", "B", "a.b", "Z9", "a-b", "9z", "b"} {
		if _, err := s.UpdateTemplate(ctx, name, func(tpl *api.Template, _ bool) error { tpl.Description = name; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tpl := range list {
		names = append(names, tpl.Name)
	}
	if want := []string{"9z", "B", "Node", "Z9", "a-b", "a.b", "a:b", "a_b", "b", "node"}; !slices.Equal(names, want) {
		t.Fatalf("templates are listed as %v, want byte order %v", names, want)
	}
}

// Two operators adding an image for different pools at the same moment both
// keep theirs: an update never starts from a copy another one is changing,
// not even when the template does not exist yet.
func concurrentTemplates(t *testing.T, s store.Store) {
	ctx := context.Background()
	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool := "pool-" + string(rune('a'+i))
			if _, err := s.UpdateTemplate(ctx, "python", func(tpl *api.Template, _ bool) error {
				if tpl.Images == nil {
					tpl.Images = map[string]string{}
				}
				tpl.Images[pool] = "image-for-" + pool
				return nil
			}); err != nil {
				t.Errorf("update for %s: %v", pool, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := s.GetTemplate(ctx, "python")
	if err != nil || len(got.Images) != writers {
		t.Fatalf("%d of %d images survived: %+v %v", len(got.Images), writers, got.Images, err)
	}
}

func secrets(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	rec := store.SecretRecord{Secret: api.Secret{Name: "daytona", Fingerprint: "aaa", CreatedAt: now, UpdatedAt: now}, Sealed: []byte{0, 1, 2, 255}}
	if err := s.PutSecret(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Fingerprint, rec.Sealed = "bbb", []byte{9}
	if err := s.PutSecret(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSecret(ctx, "daytona")
	if err != nil || got.Fingerprint != "bbb" || string(got.Sealed) != string([]byte{9}) {
		t.Fatalf("replaced secret %+v %v", got, err)
	}
	if list, err := s.ListSecrets(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err := s.UpsertPool(ctx, api.Pool{Name: "dt", Adapter: "daytona", CredentialRef: "secret:daytona"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "daytona"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a secret in use was deleted: %v", err)
	}
	if err := s.DeletePool(ctx, "dt"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "daytona"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSecret(ctx, "daytona"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleted secret still there: %v", err)
	}
	if err := s.DeleteSecret(ctx, "daytona"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleting a missing secret: %v", err)
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
	// Newest first: a limit keeps the latest events, and BeforeSeq pages back.
	seqs := func(f store.AuditFilter) (out []int64) {
		t.Helper()
		list, err := s.ListAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range list {
			out = append(out, ev.Seq)
		}
		return out
	}
	if got := seqs(store.AuditFilter{Desc: true, Limit: 2}); !slices.Equal(got, []int64{5, 4}) {
		t.Fatalf("newest two events: %v, want [5 4]", got)
	}
	if got := seqs(store.AuditFilter{Desc: true, Limit: 2, BeforeSeq: 4}); !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("the page before 4: %v, want [3 2]", got)
	}
	if got := seqs(store.AuditFilter{AfterSeq: 1, BeforeSeq: 4}); !slices.Equal(got, []int64{2, 3}) {
		t.Fatalf("between 1 and 4, oldest first: %v, want [2 3]", got)
	}
	if got := seqs(store.AuditFilter{Limit: 2}); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("default order: %v, want [1 2]", got)
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
