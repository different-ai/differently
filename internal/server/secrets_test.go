package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// A provider key stored in Differently reaches the backend, is never returned,
// and a rotation takes effect without a restart. The backend here is a remote
// plugin, whose credential is the bearer token it checks.
func TestStoredProviderSecretReachesBackendAndRotatesLive(t *testing.T) {
	wiring.Register() // adapter factories, for a pool added through the API
	h := newHarness(t, api.PlacementPolicy{})
	ctx := context.Background()
	var accepted atomic.Value
	accepted.Store("key-v1")
	backend := fake.New(fake.Options{})
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapterhttp.Handler(backend, accepted.Load().(string)).ServeHTTP(w, r)
	}))
	t.Cleanup(plugin.Close)

	var sec api.Secret
	h.must(201, "PUT", "/v1/admin/secrets/plugin-key", operatorToken, map[string]string{"value": "key-v1"}, &sec)
	if sec.Fingerprint == "" {
		t.Fatalf("no fingerprint: %+v", sec)
	}
	var raw string
	h.call("GET", "/v1/admin/secrets", operatorToken, nil, &raw)
	if strings.Contains(raw, "key-v1") || !strings.Contains(raw, sec.Fingerprint) {
		t.Fatalf("listing leaked the value or lost the fingerprint: %s", raw)
	}
	pool := func(secret string) map[string]any {
		return map[string]any{"name": "plugin", "adapter": "remote", "trust": "private", "config": map[string]string{"endpoint": plugin.URL}, "credentials": map[string]string{"secret": secret}}
	}
	h.must(400, "POST", "/v1/admin/pools", operatorToken, pool("missing"), nil)
	h.must(201, "POST", "/v1/admin/pools", operatorToken, pool("plugin-key"), nil)
	health := func() string {
		h.sched.Pools.Refresh(ctx)
		var p api.Pool
		h.must(200, "GET", "/v1/admin/pools/plugin", operatorToken, nil, &p)
		return p.Health.Status
	}
	if got := health(); got != "healthy" {
		t.Fatalf("pool with the stored key: %s", got)
	}
	accepted.Store("key-v2") // the provider rotated its key
	if got := health(); got == "healthy" {
		t.Fatal("the old key was still accepted")
	}
	h.must(200, "PUT", "/v1/admin/secrets/plugin-key", operatorToken, map[string]string{"value": "key-v2"}, nil)
	if got := health(); got != "healthy" {
		t.Fatalf("pool after rotating the stored key: %s", got)
	}
	h.must(409, "DELETE", "/v1/admin/secrets/plugin-key", operatorToken, nil, nil)

	// Moving the pool to another stored credential.
	accepted.Store("key-b")
	h.must(201, "PUT", "/v1/admin/secrets/plugin-key-b", operatorToken, map[string]string{"value": "key-b"}, nil)
	h.must(400, "PATCH", "/v1/admin/pools/plugin", operatorToken, map[string]string{"credentialRef": "plugin-key-b"}, nil)
	h.must(400, "PATCH", "/v1/admin/pools/plugin", operatorToken, map[string]string{"credentialRef": "secret:missing"}, nil)
	h.must(409, "PATCH", "/v1/admin/pools/fc", operatorToken, map[string]string{"credentialRef": "secret:plugin-key-b"}, nil) // declared in config
	h.must(200, "PATCH", "/v1/admin/pools/plugin", operatorToken, map[string]string{"credentialRef": "secret:plugin-key-b"}, nil)
	if got := health(); got != "healthy" {
		t.Fatalf("pool after moving to another credential: %s", got)
	}
	h.must(204, "DELETE", "/v1/admin/secrets/plugin-key", operatorToken, nil, nil) // no longer used
	_, _, tenant := h.tenant("a", api.Quota{})
	h.must(403, "GET", "/v1/admin/secrets", tenant, nil, nil)
	h.must(403, "PUT", "/v1/admin/secrets/plugin-key", tenant, map[string]string{"value": "x"}, nil)
	h.must(400, "PUT", "/v1/admin/secrets/Bad Name", operatorToken, map[string]string{"value": "x"}, nil)
}

// A pool change made through one replica (here a drain) reaches another
// replica sharing the store on its next refresh, without a restart.
func TestReplicasPickUpPoolChangesFromTheStore(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	ctx := context.Background()
	other := pools.NewManager(pools.Deps{Store: h.store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	stored, _ := h.store.ListPools(ctx)
	for _, p := range stored {
		other.Set(p, map[string]*fake.Adapter{"fc": h.fc, "burst": h.ext}[p.Name])
	}
	h.must(200, "POST", "/v1/admin/pools/fc/drain", operatorToken, nil, nil)
	other.Refresh(ctx)
	for _, v := range other.Views(nil) {
		if v.Pool.Name == "fc" && !v.Pool.Draining {
			t.Fatal("the other replica still places work on the drained pool")
		}
	}
}

// Rotating a key issues a replacement with the same name, scope and project;
// the old key keeps working through the grace period, or stops at once.
func TestRotateAPIKey(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, admin := h.tenant("a", api.Quota{})
	var runner api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys", admin, api.CreateAPIKeyRequest{Name: "workbot-runner", Scopes: []string{"sandboxes:read", "sandboxes:exec"}, TTLHours: 24}, &runner)

	var rotated api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys/"+runner.ID+"/rotate", admin, api.RotateAPIKeyRequest{GraceSeconds: 600}, &rotated)
	if rotated.ID == runner.ID || rotated.Name != "workbot-runner" || rotated.ProjectID != runner.ProjectID || len(rotated.Scopes) != 2 || rotated.ExpiresAt == nil {
		t.Fatalf("replacement %+v", rotated.APIKey)
	}
	if rotated.Replaced == nil || rotated.Replaced.ExpiresAt == nil || rotated.Replaced.Revoked {
		t.Fatalf("old key should expire after the grace period: %+v", rotated.Replaced)
	}
	h.must(200, "GET", "/v1/sandboxes", rotated.Token, nil, nil)
	h.must(200, "GET", "/v1/sandboxes", runner.Token, nil, nil)               // still in its grace period
	h.must(409, "POST", "/v1/api-keys/"+runner.ID+"/rotate", admin, nil, nil) // already replaced: rotate the replacement

	var again api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys/"+rotated.ID+"/rotate", admin, nil, &again)
	h.must(401, "GET", "/v1/sandboxes", rotated.Token, nil, nil) // no grace: revoked at once
	h.must(200, "GET", "/v1/sandboxes", again.Token, nil, nil)
	h.must(409, "POST", "/v1/api-keys/"+rotated.ID+"/rotate", admin, nil, nil)

	var got api.APIKey
	h.must(200, "GET", "/v1/api-keys/"+again.ID, admin, nil, &got)
	if got.ID != again.ID || got.Revoked {
		t.Fatalf("lookup %+v", got)
	}
	// A key may not rotate a key holding scopes it lacks, nor see another organization's keys.
	var narrow api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys", admin, api.CreateAPIKeyRequest{Name: "key-admin", Scopes: []string{"keys:write"}}, &narrow)
	h.must(403, "POST", "/v1/api-keys/"+again.ID+"/rotate", narrow.Token, nil, nil)
	_, _, other := h.tenant("b", api.Quota{})
	h.must(404, "POST", "/v1/api-keys/"+again.ID+"/rotate", other, nil, nil)
	h.must(400, "POST", "/v1/api-keys/"+again.ID+"/rotate", admin, api.RotateAPIKeyRequest{GraceSeconds: 8 * 24 * 3600}, nil)
}

// Switching providers sends new sandboxes to the chosen pool; an external pool
// needs explicit approval, and private-only work still never leaves.
func TestUsePoolSwitchesWhereNewSandboxesGo(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{External: api.ExternalNever})
	_, _, token := h.tenant("a", api.Quota{})
	h.must(409, "POST", "/v1/admin/pools/burst/use", operatorToken, nil, nil)

	var out api.UsePoolResponse
	h.must(200, "POST", "/v1/admin/pools/burst/use", operatorToken, api.UsePoolRequest{ApproveExternal: true}, &out)
	if len(out.Drained) != 1 || out.Drained[0] != "fc" || out.Policy == nil || out.Policy.External != api.ExternalOverflow {
		t.Fatalf("switch to burst: %+v", out)
	}
	h.sched.Pools.Refresh(context.Background())
	sb := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if got := h.get(token, sb.ID); got.Pool != "burst" {
		t.Fatalf("new sandbox placed on %q, want burst", got.Pool)
	}
	// Private-only work never leaves private pools: with them all drained it is refused up front.
	h.must(422, "POST", "/v1/sandboxes", token, api.CreateSandboxRequest{Classification: "private-only"}, nil)

	out = api.UsePoolResponse{}
	h.must(200, "POST", "/v1/admin/pools/fc/use", operatorToken, nil, &out)
	if len(out.Drained) != 1 || out.Drained[0] != "burst" || out.Policy != nil {
		t.Fatalf("switch back to fc: %+v", out)
	}
	h.sched.Pools.Refresh(context.Background())
	back := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if got := h.get(token, back.ID); got.Pool != "fc" {
		t.Fatalf("after switching back, placed on %q", got.Pool)
	}
	if got := h.get(token, sb.ID); got.Pool != "burst" || got.Status != api.StatusRunning {
		t.Fatalf("a running sandbox moved or stopped: %+v", got)
	}
}
