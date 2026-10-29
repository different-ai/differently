package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/secrets"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const operatorToken = "dly_bootstrap_0123456789abcdefghjkmnpqrstvwxyz"

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	sched *scheduler.Scheduler
	store *store.Memory
	fc    *fake.Adapter
	ext   *fake.Adapter
}

// newHarness runs the real server, scheduler and memory store with two fake
// pools: "fc" (private, 4 CPU) and "burst" (external, unbounded).
func newHarness(t *testing.T, policy api.PlacementPolicy) *harness {
	t.Helper()
	return newHarnessWith(t, policy, nil, nil)
}

// newHarnessWith lets a test adjust the config and wrap the store.
func newHarnessWith(t *testing.T, policy api.PlacementPolicy, tweak func(*config.Config), wrap func(store.Store) store.Store) *harness {
	t.Helper()
	t.Setenv("DIFFERENTLY_BOOTSTRAP_TOKEN", operatorToken)
	cfg := config.Default()
	cfg.Policy = policy
	cfg.Policy.Scope = "global"
	cfg.RateLimit.RequestsPerSecond = 0
	if tweak != nil {
		tweak(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mem := store.NewMemory()
	var st store.Store = mem
	if wrap != nil {
		st = wrap(mem)
	}
	rec, _ := auditlog.New(st, log, "")
	box, _ := secrets.New(make([]byte, secrets.KeySize))
	pm := pools.NewManager(pools.Deps{Store: st, Logger: log, Resolve: server.CredentialResolver(st, box)})
	h := &harness{t: t, store: mem,
		fc:  fake.New(fake.Options{Trust: api.TrustPrivate, TotalCPU: 4, TotalMemoryMiB: 16384}),
		ext: fake.New(fake.Options{Trust: api.TrustExternal, Features: []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeaturePorts}}),
	}
	ctx := context.Background()
	for _, p := range []struct {
		pool api.Pool
		a    adapter.ExecutionAdapter
	}{
		{api.Pool{Name: "fc", Adapter: "fake", Trust: api.TrustPrivate, Enabled: true, Region: "dc1", Source: "config", CostModel: api.CostModel{CPUHour: 0.04}}, h.fc},
		{api.Pool{Name: "burst", Adapter: "fake", Trust: api.TrustExternal, Enabled: true, Region: "us", MaxConcurrency: 10, Source: "config", CostModel: api.CostModel{CPUHour: 0.02}}, h.ext},
	} {
		_ = st.UpsertPool(ctx, p.pool)
		pm.Set(p.pool, p.a)
	}
	inst, _ := st.Installation(ctx)
	h.sched = scheduler.New(st, pm, cfg, inst, rec, log)
	h.sched.ReconcileGrace = 0 // tests drive the reconciler directly
	srv := server.New(cfg, st, pm, h.sched, rec, log, inst)
	srv.Secrets = box
	if err := srv.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	pm.Refresh(ctx)
	h.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

// tick runs scheduler passes until provisioning settles.
func (h *harness) tick() {
	for i := 0; i < 3; i++ {
		h.sched.Tick(context.Background())
		h.sched.Drain()
	}
}

func (h *harness) call(method, path, token string, body any, out any) int {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		if b, ok := body.([]byte); ok {
			r = bytes.NewReader(b)
		} else {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if s, ok := out.(*string); ok {
			*s = string(raw)
		} else if err := json.Unmarshal(raw, out); err != nil && len(raw) > 0 {
			h.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (h *harness) must(want int, method, path, token string, body, out any) {
	h.t.Helper()
	var raw string
	if got := h.call(method, path, token, body, &raw); got != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			h.t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

// tenant creates an organization, project and project key.
func (h *harness) tenant(name string, quota api.Quota) (orgID, projectID, token string) {
	var created struct {
		Organization api.Organization `json:"organization"`
		Project      api.Project      `json:"project"`
	}
	h.must(201, "POST", "/v1/admin/organizations", operatorToken, map[string]any{"name": name, "quota": quota, "defaultProject": "default"}, &created)
	var key api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys", operatorToken, api.CreateAPIKeyRequest{
		Name: "ci", OrgID: created.Organization.ID, ProjectID: created.Project.ID,
		Scopes: []string{"sandboxes:read", "sandboxes:write", "sandboxes:exec", "keys:write", "usage:read"},
	}, &key)
	return created.Organization.ID, created.Project.ID, key.Token
}

func (h *harness) create(token string, req api.CreateSandboxRequest) api.Sandbox {
	var sb api.Sandbox
	h.must(202, "POST", "/v1/sandboxes", token, req, &sb)
	return sb
}

func (h *harness) get(token, id string) api.Sandbox {
	var sb api.Sandbox
	h.must(200, "GET", "/v1/sandboxes/"+id, token, nil, &sb)
	return sb
}

var overflowPolicy = api.PlacementPolicy{External: api.ExternalOverflow, ExternalPools: []string{"burst"}}

// Scenario A: install, admin, key, sandbox lifecycle with no hosted service.
func TestOwnCloudQuickstart(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("acme", api.Quota{})
	sb := h.create(token, api.CreateSandboxRequest{Profile: "small", Env: map[string]string{"SECRET": "x"}})
	if sb.Status != api.StatusQueued || len(sb.EnvKeys) != 1 {
		t.Fatalf("created %+v", sb)
	}
	h.tick()
	sb = h.get(token, sb.ID)
	if sb.Status != api.StatusRunning || sb.Pool != "fc" || sb.Trust != api.TrustPrivate {
		t.Fatalf("not running privately: %+v", sb)
	}
	var res api.ExecResult
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, api.ExecRequest{Command: "node --version"}, &res)
	if !strings.Contains(res.Stdout, "node --version") {
		t.Fatalf("exec %+v", res)
	}
	h.must(204, "PUT", "/v1/sandboxes/"+sb.ID+"/files/content?path=/a.txt", token, []byte("hello"), nil)
	var content string
	h.call("GET", "/v1/sandboxes/"+sb.ID+"/files/content?path=/a.txt", token, nil, &content)
	if content != "hello" {
		t.Fatalf("file %q", content)
	}
	var stream string
	h.call("POST", "/v1/sandboxes/"+sb.ID+"/exec?stream=1", token, api.ExecRequest{Command: "echo hi"}, &stream)
	if !strings.Contains(stream, "event: stdout") || !strings.Contains(stream, "event: exit") {
		t.Fatalf("stream %q", stream)
	}
	partial := h.create(token, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 2}})
	if partial.Resources.CPU != 2 || partial.Resources.MemoryMiB != 1024 {
		t.Fatalf("partial resources should start from the default profile: %+v", partial.Resources)
	}
	h.must(202, "DELETE", "/v1/sandboxes/"+partial.ID, token, nil, nil)
	h.must(202, "DELETE", "/v1/sandboxes/"+sb.ID, token, nil, nil)
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusTerminated || h.fc.Count() != 0 {
		t.Fatalf("not cleaned up: %s, %d instances", got.Status, h.fc.Count())
	}
	var verify struct {
		OK     bool `json:"ok"`
		Events int  `json:"events"`
	}
	h.must(200, "GET", "/v1/admin/audit/verify", operatorToken, nil, &verify)
	if !verify.OK || verify.Events < 6 {
		t.Fatalf("audit chain %+v", verify)
	}
}

// Scenarios D and E: overflow only for eligible work; private-only waits.
func TestHybridOverflowAndPrivateOnly(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	_, _, token := h.tenant("acme", api.Quota{})
	// Fill the private pool (4 CPU).
	first := h.create(token, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 4, MemoryMiB: 4096}})
	h.tick()
	if got := h.get(token, first.ID); got.Pool != "fc" {
		t.Fatalf("first sandbox should be private: %+v", got)
	}
	h.sched.Pools.Refresh(context.Background())

	secret := h.create(token, api.CreateSandboxRequest{Classification: "private-only", Resources: &api.Resources{CPU: 2, MemoryMiB: 2048}})
	general := h.create(token, api.CreateSandboxRequest{Classification: "internal", Resources: &api.Resources{CPU: 2, MemoryMiB: 2048}})
	h.tick()
	if got := h.get(token, secret.ID); got.Status != api.StatusQueued || !strings.Contains(got.StatusReason, "waiting") {
		t.Fatalf("private-only must stay queued, got %+v", got)
	}
	if got := h.get(token, general.ID); got.Pool != "burst" || got.Trust != api.TrustExternal {
		t.Fatalf("eligible work should overflow: %+v", got)
	}
	// Opting out per request keeps work private.
	optOut := h.create(token, api.CreateSandboxRequest{Placement: &api.Placement{External: api.ExternalNever}})
	h.tick()
	if got := h.get(token, optOut.ID); got.Status != api.StatusQueued {
		t.Fatalf("opt-out escaped: %+v", got)
	}
	// Scenario L: the operator can see why.
	var explain map[string]any
	h.must(200, "GET", "/v1/sandboxes/"+general.ID+"/placement", token, nil, &explain)
	if explain["trust"] != "external" || explain["decision"] == nil {
		t.Fatalf("placement explanation %v", explain)
	}
	var report server.TrustReport
	h.must(200, "GET", "/v1/admin/trust-report", operatorToken, nil, &report)
	if len(report.ExternalRecent) != 1 || report.ExternalRecent[0].SandboxID != general.ID || len(report.Limitations) == 0 || !report.AuditChain.OK {
		t.Fatalf("trust report %+v", report)
	}
}

// Scenario B: with external disabled nothing reaches the external pool.
func TestFullyPrivateDeployment(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{External: api.ExternalNever})
	_, _, token := h.tenant("acme", api.Quota{})
	for i := 0; i < 4; i++ {
		h.create(token, api.CreateSandboxRequest{Classification: "public", Resources: &api.Resources{CPU: 2, MemoryMiB: 1024}})
	}
	h.tick()
	if h.ext.Count() != 0 {
		t.Fatalf("%d sandboxes reached the external provider", h.ext.Count())
	}
}

// Scenario C: per-tenant concurrency, fair queueing and cleanup.
func TestMultiTenantQuotas(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, a := h.tenant("a", api.Quota{MaxActiveSandboxes: 1, MaxQueued: 2})
	_, _, b := h.tenant("b", api.Quota{MaxActiveSandboxes: 2})
	a1 := h.create(a, api.CreateSandboxRequest{})
	h.tick()
	a2 := h.create(a, api.CreateSandboxRequest{})
	h.create(a, api.CreateSandboxRequest{})
	var e api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", a, api.CreateSandboxRequest{}, &e); code != 429 || e.Error.Code != api.CodeQuotaExceeded {
		t.Fatalf("queue limit not enforced: %d %+v", code, e.Error)
	}
	b1 := h.create(b, api.CreateSandboxRequest{})
	h.tick()
	if h.get(a, a1.ID).Status != api.StatusRunning || h.get(a, a2.ID).Status != api.StatusQueued || h.get(b, b1.ID).Status != api.StatusRunning {
		t.Fatal("concurrency limit or fairness broken")
	}
	if code := h.call("POST", "/v1/sandboxes", a, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 64, MemoryMiB: 1024}}, &e); code != 422 {
		t.Fatalf("oversized request should be rejected up front, got %d %+v", code, e.Error)
	}
	// Stopping releases CPU; starting must re-check quota (a1 holds the only slot).
	for i := 0; i < 2; i++ { // the second cycle catches state leaking between requests
		h.must(200, "POST", "/v1/sandboxes/"+a1.ID+"/stop", a, nil, nil)
		h.must(200, "POST", "/v1/sandboxes/"+a1.ID+"/start", a, nil, nil)
	}
	h.must(202, "DELETE", "/v1/sandboxes/"+a1.ID, a, nil, nil)
	h.tick()
	h.tick()
	if got := h.get(a, a2.ID).Status; got != api.StatusRunning {
		t.Fatalf("queued sandbox did not start after release: %s", got)
	}
	var q api.QuotaReport
	h.must(200, "GET", "/v1/quotas", a, nil, &q)
	if q.OrgUsage.ActiveSandboxes != 1 || q.OrgUsage.Queued != 1 {
		t.Fatalf("usage %+v", q.OrgUsage)
	}
}

// Scenario H: a project key reaches only its own resources.
func TestScopedKeyIsolation(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, a := h.tenant("a", api.Quota{})
	_, projB, b := h.tenant("b", api.Quota{})
	sbB := h.create(b, api.CreateSandboxRequest{})
	if code := h.call("GET", "/v1/sandboxes/"+sbB.ID, a, nil, nil); code != 404 {
		t.Fatalf("cross-tenant read returned %d", code)
	}
	if code := h.call("POST", "/v1/sandboxes", a, api.CreateSandboxRequest{ProjectID: projB}, nil); code != 403 {
		t.Fatalf("cross-project create returned %d", code)
	}
	if code := h.call("POST", "/v1/api-keys", a, api.CreateAPIKeyRequest{Name: "x", Scopes: []string{"admin"}}, nil); code != 403 {
		t.Fatalf("escalation returned %d", code)
	}
	if code := h.call("GET", "/v1/admin/pools", a, nil, nil); code != 403 {
		t.Fatalf("admin API returned %d", code)
	}
	orgA := ""
	var me map[string]any
	h.must(200, "GET", "/v1/me", a, nil, &me)
	orgA, _ = me["orgId"].(string)
	var adminA api.CreateAPIKeyResponse
	h.must(201, "POST", "/v1/api-keys", operatorToken, api.CreateAPIKeyRequest{Name: "a-admin", OrgID: orgA, Scopes: []string{"org:admin"}}, &adminA)
	h.must(200, "PUT", "/v1/policies/project:"+projB, operatorToken, api.PlacementPolicy{External: api.ExternalNever}, nil)
	if code := h.call("PUT", "/v1/policies/project:"+projB, adminA.Token, api.PlacementPolicy{}, nil); code != 404 {
		t.Fatalf("cross-tenant policy update returned %d", code)
	}
	if code := h.call("DELETE", "/v1/policies/project:"+projB, adminA.Token, nil, nil); code != 404 {
		t.Fatalf("cross-tenant policy delete returned %d", code)
	}
	var pools map[string]any
	h.must(200, "GET", "/v1/capabilities", a, nil, &pools)
	if strings.Contains(mustJSON(pools), "credential") {
		t.Fatal("tenant view leaks credential references")
	}
	if code := h.call("GET", "/v1/sandboxes", "dly_nope_0123456789abcdefghjkmnpqrstvwxyz", nil, nil); code != 401 {
		t.Fatalf("bad key returned %d", code)
	}
}

// Scenario I: unsupported capabilities fail precisely or route elsewhere.
func TestCapabilityMismatch(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	var e api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{Capabilities: []string{"gpu"}}, &e); code != 422 || e.Error.Code != api.CodeUnsupportedCapability {
		t.Fatalf("want unsupported_capability, got %d %+v", code, e.Error)
	}
	h2 := newHarness(t, overflowPolicy)
	_, _, token2 := h2.tenant("a", api.Quota{})
	sb := h2.create(token2, api.CreateSandboxRequest{Capabilities: []string{"ports.expose"}})
	h2.tick()
	if got := h2.get(token2, sb.ID); got.Pool != "fc" {
		// The fake private pool supports ports too, so private-first keeps it home.
		t.Fatalf("placed on %s", got.Pool)
	}
	// Lifecycle operations the backend lacks return unsupported_operation.
	plain := h2.create(token2, api.CreateSandboxRequest{Placement: &api.Placement{AllowedPools: []string{"burst"}}})
	h2.tick()
	e = api.ErrorEnvelope{}
	if code := h2.call("POST", "/v1/sandboxes/"+plain.ID+"/pause", token2, nil, &e); code != 501 || e.Error.Code != api.CodeUnsupportedOperation {
		t.Fatalf("want unsupported_operation, got %d %+v", code, e.Error)
	}
}

// Scenario F: an operator policy change moves new work without app changes.
func TestProviderSubstitution(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	_, _, token := h.tenant("a", api.Quota{})
	before := h.create(token, api.CreateSandboxRequest{})
	h.must(200, "PUT", "/v1/policies/global", operatorToken, api.PlacementPolicy{External: api.ExternalAllowed, ExternalPools: []string{"burst"}, DeniedPools: []string{"fc"}}, nil)
	after := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if h.get(token, before.ID).Pool != "fc" && h.get(token, before.ID).Pool != "burst" {
		t.Fatal("first sandbox not placed")
	}
	if got := h.get(token, after.ID); got.Pool != "burst" {
		t.Fatalf("policy change ignored: %+v", got)
	}
}

// Scenario J: provider failures retry safely, orphans and lost instances are
// reconciled, and quota stays correct.
func TestFailureRecoveryAndReconcile(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	orgID, _, token := h.tenant("a", api.Quota{MaxActiveSandboxes: 5})
	h.fc.FailCreates = 1
	sb := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusQueued || got.Attempts != 1 {
		t.Fatalf("failed attempt should requeue: %+v", got)
	}
	// Retry after backoff; the pool recovers on the next health probe.
	_, _ = h.store.UpdateSandbox(context.Background(), sb.ID, func(r *store.SandboxRecord) error { r.NextAttemptAt = time.Now(); return nil })
	h.sched.Pools.Refresh(context.Background())
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || h.fc.Count() != 1 {
		t.Fatalf("retry did not converge to exactly one instance: %+v (%d)", got, h.fc.Count())
	}
	// A backend instance that stops behind the control plane's back (for
	// example its runtime restarted) makes the sandbox stopped.
	running0, _ := h.store.GetSandbox(context.Background(), sb.ID)
	_ = h.fc.Stop(context.Background(), running0.BackendRef)
	h.sched.Reconcile(context.Background())
	if got := h.get(token, sb.ID); got.Status != api.StatusStopped {
		t.Fatalf("state drift not reconciled: %+v", got)
	}
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/start", token, nil, nil)
	inst, _ := h.store.Installation(context.Background())
	h.fc.Inject("sbx_ghost", inst)
	running, _ := h.store.GetSandbox(context.Background(), sb.ID)
	h.fc.Forget(running.BackendRef)
	h.sched.Reconcile(context.Background())
	if h.fc.Count() != 0 {
		t.Fatal("orphan was not deleted")
	}
	if got := h.get(token, sb.ID); got.Status != api.StatusFailed {
		t.Fatalf("lost instance not detected: %+v", got)
	}
	u, _ := h.store.Usage(context.Background(), orgID, "")
	if u.ActiveSandboxes != 0 || u.CPU != 0 {
		t.Fatalf("quota leaked: %+v", u)
	}
}

// A configured template reaches each backend as that pool's own image, and
// is never placed on a pool without one.
func TestTemplateImagesReachTheBackend(t *testing.T) {
	h := newHarnessWith(t, api.PlacementPolicy{}, func(c *config.Config) {
		c.Templates = []api.Template{{Name: "workbot", Images: map[string]string{"fc": "workbot-image-v3"}}}
	}, nil)
	_, _, token := h.tenant("a", api.Quota{})
	sb := h.create(token, api.CreateSandboxRequest{Template: "workbot"})
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || got.Pool != "fc" || h.fc.TemplateOf(sb.ID) != "workbot-image-v3" {
		t.Fatalf("want fc booting workbot-image-v3, got %+v with %q", got, h.fc.TemplateOf(sb.ID))
	}
	// An unmapped template name still passes through unchanged.
	plain := h.create(token, api.CreateSandboxRequest{Template: "ubuntu:24.04"})
	h.tick()
	if h.fc.TemplateOf(plain.ID) != "ubuntu:24.04" {
		t.Fatalf("unconfigured template changed: %q", h.fc.TemplateOf(plain.ID))
	}
}

// Extending a TTL moves the backend's own deadline too, just behind
// Differently's, so the backend never deletes the sandbox first.
func TestTTLChangeMovesBackendExpiry(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	sb := h.create(token, api.CreateSandboxRequest{TTLSeconds: 600})
	h.tick()
	var out api.Sandbox
	h.must(200, "PATCH", "/v1/sandboxes/"+sb.ID, token, map[string]any{"ttlSeconds": 7200}, &out)
	backend := h.fc.ExpiryOf(sb.ID)
	if out.ExpiresAt == nil || !backend.Equal(out.ExpiresAt.Add(5*time.Minute)) {
		t.Fatalf("backend expiry %v, sandbox expires %v", backend, out.ExpiresAt)
	}
}

// A backend that cannot start one sandbox is retried without being marked
// unhealthy, so one tenant's failing sandbox does not move other tenants'
// work off the pool; a backend that is unavailable still is.
func TestSandboxFailureKeepsPoolHealthy(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	fcHealthy := func() (bool, string) {
		for _, v := range h.sched.Pools.Views(nil) {
			if v.Pool.Name == "fc" {
				return v.Healthy, v.HealthDetail
			}
		}
		t.Fatal("pool fc missing")
		return false, ""
	}
	h.fc.FailCreates, h.fc.FailWith = 1, adapter.SandboxFailed(errors.New("pod not ready within 60s"))
	sb := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusQueued || got.Attempts != 1 {
		t.Fatalf("a failed sandbox should be retried: %+v", got)
	}
	if ok, detail := fcHealthy(); !ok {
		t.Fatalf("one sandbox's failure marked the pool unhealthy: %s", detail)
	}
	h.fc.FailCreates, h.fc.FailWith = 1, nil
	h.create(token, api.CreateSandboxRequest{})
	h.tick()
	if ok, _ := fcHealthy(); ok {
		t.Fatal("an unavailable backend should be marked unhealthy until its next probe")
	}
}

// A backend call that outlives the provision timeout is retried: the
// requeue is recorded even though the call's own context has expired, so the
// sandbox does not stay provisioning.
func TestProvisionTimeoutRequeues(t *testing.T) {
	h := newHarnessWith(t, api.PlacementPolicy{}, func(c *config.Config) { c.Scheduler.ProvisionTimeoutSeconds = 1 },
		func(s store.Store) store.Store { return ctxStore{s} })
	_, _, token := h.tenant("a", api.Quota{})
	h.fc.HangCreates = 1
	sb := h.create(token, api.CreateSandboxRequest{})
	h.sched.Tick(context.Background())
	h.sched.Drain()
	got := h.get(token, sb.ID)
	if got.Status != api.StatusQueued || got.Attempts != 1 || !strings.Contains(got.StatusReason, "deadline exceeded") {
		t.Fatalf("a timed-out create should requeue with its reason: %+v", got)
	}
}

// ctxStore refuses writes on an ended context, as PostgreSQL does.
type ctxStore struct{ store.Store }

func (s ctxStore) UpdateSandbox(ctx context.Context, id string, fn func(*store.SandboxRecord) error) (store.SandboxRecord, error) {
	if err := ctx.Err(); err != nil {
		return store.SandboxRecord{}, err
	}
	return s.Store.UpdateSandbox(ctx, id, fn)
}

// Two pools can front the same backend. Each pool's reconcile sees the
// other's instances and must not delete them; an instance left behind by an
// earlier placement is still an orphan.
func TestReconcileLeavesSharedBackendInstancesAlone(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	ctx := context.Background()
	twin := api.Pool{Name: "fc-twin", Adapter: "fake", Trust: api.TrustPrivate, Enabled: true, Region: "dc1", Source: "config"}
	_ = h.store.UpsertPool(ctx, twin)
	h.sched.Pools.Set(twin, h.fc)
	h.sched.Pools.Refresh(ctx)
	sb := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	h.sched.Reconcile(ctx)
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || h.fc.Count() != 1 {
		t.Fatalf("the twin pool deleted a live instance: %+v (%d)", got, h.fc.Count())
	}
	inst, _ := h.store.Installation(ctx)
	h.fc.Inject(sb.ID, inst) // a leftover from an earlier placement
	h.sched.Reconcile(ctx)
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || h.fc.Count() != 1 {
		t.Fatalf("leftover not deleted, or the live instance went with it: %+v (%d)", got, h.fc.Count())
	}
}

func TestIdempotentCreateAndSnapshots(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	body, _ := json.Marshal(api.CreateSandboxRequest{})
	send := func() (int, api.Sandbox) {
		req, _ := http.NewRequest("POST", h.srv.URL+"/v1/sandboxes", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "job-42")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb api.Sandbox
		_ = json.NewDecoder(resp.Body).Decode(&sb)
		return resp.StatusCode, sb
	}
	c1, s1 := send()
	c2, s2 := send()
	if c1 != 202 || c2 != 200 || s1.ID != s2.ID {
		t.Fatalf("replay: %d %s / %d %s", c1, s1.ID, c2, s2.ID)
	}
	h.tick()
	h.must(204, "PUT", "/v1/sandboxes/"+s1.ID+"/files/content?path=/state", token, []byte("v1"), nil)
	var child api.Sandbox
	h.must(202, "POST", "/v1/sandboxes/"+s1.ID+"/fork", token, nil, &child)
	h.tick()
	var data string
	h.call("GET", "/v1/sandboxes/"+child.ID+"/files/content?path=/state", token, nil, &data)
	if child.Labels["forked-from"] != s1.ID || data != "v1" {
		t.Fatalf("fork %+v data %q", child, data)
	}
	var snaps api.List[api.Snapshot]
	h.must(200, "GET", "/v1/snapshots", token, nil, &snaps)
	if len(snaps.Items) != 1 || snaps.Items[0].Pool != "fc" {
		t.Fatalf("snapshots %+v", snaps)
	}
}

func TestSuspendOrganization(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	orgID, _, token := h.tenant("a", api.Quota{})
	sb := h.create(token, api.CreateSandboxRequest{})
	h.tick()
	h.must(200, "POST", "/v1/admin/organizations/"+orgID+"/suspend", operatorToken, map[string]any{"terminate": true, "reason": "abuse"}, nil)
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusTerminated {
		t.Fatalf("suspended tenant's sandbox still %s", got.Status)
	}
	var e api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{}, &e); code != 403 || e.Error.Code != api.CodeSuspended {
		t.Fatalf("suspended org created work: %d %+v", code, e.Error)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Idle auto-stop: a sandbox with an idle timeout is paused once nothing has
// used it for that long, and the next exec wakes it transparently. A
// keepalive resets the clock; a sandbox a caller stopped is not woken.
func TestIdleAutoStopAndWake(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{IdleTimeoutSeconds: 30}, nil); code != 400 {
		t.Fatalf("an idle timeout under 60 s should be refused, got %d", code)
	}
	sb := h.create(token, api.CreateSandboxRequest{IdleTimeoutSeconds: 60})
	h.tick()
	later := func(d time.Duration) { h.sched.Now = func() time.Time { return time.Now().UTC().Add(d) } }

	later(30 * time.Second)
	h.sched.SweepIdle(context.Background())
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning {
		t.Fatalf("swept before its idle timeout: %+v", got.Status)
	}
	later(2 * time.Minute)
	h.sched.SweepIdle(context.Background())
	got := h.get(token, sb.ID)
	if got.Status != api.StatusPaused || got.IdledAt == nil {
		t.Fatalf("an idle sandbox should be paused and marked idled: %s %v", got.Status, got.IdledAt)
	}
	var res api.ExecResult
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/exec", token, api.ExecRequest{Command: "echo hi"}, &res)
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || got.IdledAt != nil || got.LastActivityAt == nil {
		t.Fatalf("exec should wake the sandbox: %+v", got)
	}

	// A keepalive counts as activity.
	h.sched.Now = func() time.Time { return time.Now().UTC() }
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/keepalive", token, nil, nil)
	later(50 * time.Second)
	h.sched.SweepIdle(context.Background())
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning {
		t.Fatalf("swept despite a keepalive 50 s ago: %s", got.Status)
	}

	// A caller's pause is respected: exec does not wake it.
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/pause", token, nil, nil)
	if code := h.call("POST", "/v1/sandboxes/"+sb.ID+"/exec", token, api.ExecRequest{Command: "true"}, nil); code != 409 {
		t.Fatalf("exec on a sandbox the caller paused should be refused, got %d", code)
	}
	var off api.Sandbox
	h.must(200, "PATCH", "/v1/sandboxes/"+sb.ID, token, api.UpdateSandboxRequest{IdleTimeoutSeconds: ptr(-1)}, &off)
	if off.IdleTimeoutSeconds != 0 {
		t.Fatalf("-1 should turn idle auto-stop off: %d", off.IdleTimeoutSeconds)
	}
}

func ptr[T any](v T) *T { return &v }
