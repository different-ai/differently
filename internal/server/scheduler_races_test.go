package server_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// racingStore runs afterList right after a sandbox list is read, where a
// request on another replica could change a sandbox the scheduler is about to
// write.
type racingStore struct {
	store.Store
	afterList func(store.SandboxFilter)
}

func (r *racingStore) ListSandboxes(ctx context.Context, f store.SandboxFilter) ([]store.SandboxRecord, error) {
	out, err := r.Store.ListSandboxes(ctx, f)
	if r.afterList != nil {
		r.afterList(f)
	}
	return out, err
}

// The scheduler decides on a list it read a moment ago. A TTL extension or a
// delete that lands in between must win over the stale decision.
func TestSchedulerDoesNotOverwriteConcurrentChanges(t *testing.T) {
	var racing *racingStore
	h := newHarnessWith(t, api.PlacementPolicy{}, nil, func(st store.Store) store.Store {
		racing = &racingStore{Store: st}
		return racing
	})
	ctx := context.Background()
	_, _, token := h.tenant("acme", api.Quota{})
	// once runs change the first time the scheduler reads the list matching when.
	once := func(when func(store.SandboxFilter) bool, change func(*store.SandboxRecord), id string) {
		racing.afterList = func(f store.SandboxFilter) {
			if when(f) {
				racing.afterList = nil
				_, _ = h.store.UpdateSandbox(ctx, id, func(r *store.SandboxRecord) error { change(r); return nil })
			}
		}
	}
	expiryList := func(f store.SandboxFilter) bool {
		return f.Pool == "" && slices.Contains(f.Statuses, api.StatusStopped)
	}
	// The sandboxes run on "fc"; the change must land after that pool's
	// records were read, not while another pool is reconciled first.
	reconcileList := func(f store.SandboxFilter) bool { return f.Pool == "fc" }
	deleted := func(r *store.SandboxRecord) { r.Status, r.StatusReason = api.StatusTerminating, "deleted" }

	// 1. Expired when listed, extended before the write: it keeps running.
	extended := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	past, later := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	_, _ = h.store.UpdateSandbox(ctx, extended.ID, func(r *store.SandboxRecord) error { r.ExpiresAt = &past; return nil })
	once(expiryList, func(r *store.SandboxRecord) { r.ExpiresAt = &later }, extended.ID)
	h.tick()
	if got := h.get(token, extended.ID); got.Status != api.StatusRunning {
		t.Fatalf("a sandbox whose TTL was extended was expired anyway: %s (%s)", got.Status, got.StatusReason)
	}

	// 2. Its instance vanished, and it was deleted before the reconciler
	// wrote "failed": the delete stands.
	lost := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	rec, _ := h.store.GetSandbox(ctx, lost.ID)
	h.fc.Forget(rec.BackendRef)
	once(reconcileList, deleted, lost.ID)
	h.sched.Reconcile(ctx)
	if got := h.get(token, lost.ID); got.Status != api.StatusTerminating {
		t.Fatalf("a deleted sandbox was overwritten as %s (%s)", got.Status, got.StatusReason)
	}

	// 3. Provisioning was interrupted and the instance exists; it was deleted
	// before the reconciler adopted it: it must not come back as running.
	adopted := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	longAgo := time.Now().Add(-time.Hour) // well past the provision timeout
	_, _ = h.store.UpdateSandbox(ctx, adopted.ID, func(r *store.SandboxRecord) error {
		r.Status, r.ProvisionStartedAt = api.StatusProvisioning, &longAgo
		return nil
	})
	once(reconcileList, deleted, adopted.ID)
	h.sched.Reconcile(ctx)
	if got := h.get(token, adopted.ID); got.Status != api.StatusTerminating {
		t.Fatalf("a deleted sandbox came back as %s", got.Status)
	}
	h.tick()
	for _, id := range []string{lost.ID, adopted.ID} {
		if got := h.get(token, id); got.Status != api.StatusTerminated {
			t.Fatalf("deleted sandbox %s ended as %s", id, got.Status)
		}
	}
}
