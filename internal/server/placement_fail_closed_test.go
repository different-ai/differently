package server_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// flakyPolicies is a store whose policy reads fail while failing is set, as a
// database blip would make them.
type flakyPolicies struct {
	store.Store
	failing atomic.Bool
}

func (f *flakyPolicies) GetPolicy(ctx context.Context, scope string) (api.PlacementPolicy, bool, error) {
	if f.failing.Load() && scope != "global" {
		return api.PlacementPolicy{}, false, errors.New("connection reset")
	}
	return f.Store.GetPolicy(ctx, scope)
}

// A policy that cannot be read must never be treated as absent: with the
// organization's "external: never" unreadable, queued work stays queued
// instead of overflowing to the external pool the global policy allows.
func TestPlacementFailsClosedWhenPolicyUnreadable(t *testing.T) {
	var flaky *flakyPolicies
	h := newHarnessWith(t, overflowPolicy, nil, func(st store.Store) store.Store {
		flaky = &flakyPolicies{Store: st}
		return flaky
	})
	orgID, _, token := h.tenant("acme", api.Quota{})
	h.must(200, "PUT", "/v1/policies/org:"+orgID, operatorToken, api.PlacementPolicy{External: api.ExternalNever}, nil)
	h.create(token, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 4, MemoryMiB: 4096}}) // fills the private pool
	h.tick()
	h.sched.Pools.Refresh(context.Background())
	waiting := h.create(token, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 2, MemoryMiB: 2048}})

	flaky.failing.Store(true)
	h.tick()
	if got := h.get(token, waiting.ID); got.Status != api.StatusQueued || got.Pool != "" {
		t.Fatalf("sandbox was placed while its organization policy was unreadable: %+v", got)
	}
	if h.ext.Count() != 0 {
		t.Fatalf("external pool received %d instances", h.ext.Count())
	}
	var env api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{}, &env); code != 503 || env.Error == nil || env.Error.Code != api.CodeProviderUnavailable {
		t.Fatalf("create with unreadable policy: status %d, body %+v", code, env.Error)
	}
	if code := h.call("GET", "/v1/capabilities", token, nil, nil); code != 503 {
		t.Fatalf("capabilities with unreadable policy: status %d", code)
	}

	flaky.failing.Store(false)
	h.tick()
	if got := h.get(token, waiting.ID); got.Status != api.StatusQueued || got.Trust == api.TrustExternal {
		t.Fatalf("sandbox left private pools after the policy was readable again: %+v", got)
	}
}
