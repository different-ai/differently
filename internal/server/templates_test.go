package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func (h *harness) templates() []api.Template {
	h.t.Helper()
	var list api.List[api.Template]
	h.must(200, "GET", "/v1/templates", operatorToken, nil, &list)
	return list.Items
}

func (h *harness) refused(wantStatus int, wantCode, method, path string, body any) *api.Error {
	h.t.Helper()
	var env api.ErrorEnvelope
	if got := h.call(method, path, operatorToken, body, &env); got != wantStatus || env.Error == nil || env.Error.Code != wantCode {
		h.t.Fatalf("%s %s: status %d, error %+v; want %d %s", method, path, got, env.Error, wantStatus, wantCode)
	}
	return env.Error
}

// An operator gives a baseline one name across pools on a running server:
// set it, add a pool later, take a pool away, remove it. Each change applies
// to the next sandbox, and a sandbox for the template only ever runs where
// an image is set.
func TestTemplateSetAtRuntime(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	_, _, token := h.tenant("a", api.Quota{})
	if got := h.templates(); len(got) != 0 {
		t.Fatalf("templates before any was set: %+v", got)
	}

	var tpl api.Template
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{
		"description": "Python 3.12 with our tools", "images": map[string]string{"fc": "registry.example.com/python:3.12"},
	}, &tpl)
	if tpl.Name != "python" || tpl.Source != api.TemplateSourceAPI || tpl.UpdatedBy == "" || tpl.UpdatedAt == nil || tpl.Images["fc"] == "" {
		t.Fatalf("created template %+v", tpl)
	}
	first := h.create(token, api.CreateSandboxRequest{Template: "python"})
	h.tick()
	if got := h.get(token, first.ID); got.Pool != "fc" || h.fc.TemplateOf(first.ID) != "registry.example.com/python:3.12" {
		t.Fatalf("want fc booting its own image, got %+v with %q", got, h.fc.TemplateOf(first.ID))
	}

	// A second pool later: the call names only the new one, and fc's stays.
	h.must(200, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"burst": "python-312"}}, &tpl)
	if len(tpl.Images) != 2 || tpl.Images["fc"] != "registry.example.com/python:3.12" || tpl.Description != "Python 3.12 with our tools" {
		t.Fatalf("adding a pool dropped something: %+v", tpl)
	}
	if got := h.templates(); len(got) != 1 || len(got[0].Images) != 2 || got[0].Source != api.TemplateSourceAPI {
		t.Fatalf("listed templates %+v", got)
	}

	// Take fc away: the next sandbox for the template can only go to burst.
	var left api.Template
	h.must(200, "DELETE", "/v1/admin/templates/python/images/fc", operatorToken, nil, &left)
	if len(left.Images) != 1 || left.Images["burst"] != "python-312" {
		t.Fatalf("after removing fc's image: %+v", left)
	}
	second := h.create(token, api.CreateSandboxRequest{Template: "python"})
	h.tick()
	if got := h.get(token, second.ID); got.Pool != "burst" || h.ext.TemplateOf(second.ID) != "python-312" || h.fc.TemplateOf(second.ID) != "" {
		t.Fatalf("want burst booting python-312 and nothing on fc, got %+v", got)
	}

	// Removing the template: the name is passed to the backend as given again.
	h.must(204, "DELETE", "/v1/admin/templates/python", operatorToken, nil, nil)
	if got := h.templates(); len(got) != 0 {
		t.Fatalf("templates after removal: %+v", got)
	}
	plain := h.create(token, api.CreateSandboxRequest{Template: "python"})
	h.tick()
	if h.fc.TemplateOf(plain.ID) != "python" {
		t.Fatalf("an unknown template name is passed as given, got %q on fc", h.fc.TemplateOf(plain.ID))
	}
	h.refused(404, api.CodeNotFound, "DELETE", "/v1/admin/templates/python", nil)

	var events api.List[api.AuditEvent]
	h.must(200, "GET", "/v1/audit-events?resource=python", operatorToken, nil, &events)
	var actions []string
	for _, ev := range events.Items {
		actions = append(actions, ev.Action)
	}
	if got := strings.Join(actions, " "); got != "template.put template.put template.image_delete template.delete" {
		t.Fatalf("audit actions %q", got)
	}
	if d := events.Items[1].Details; d["images"] != "burst=python-312" || events.Items[2].Details["pool"] != "fc" {
		t.Fatalf("audit details %+v %+v", d, events.Items[2].Details)
	}
	var report server.TrustReport
	h.must(200, "GET", "/v1/admin/trust-report", operatorToken, nil, &report)
	found := false
	for _, ev := range report.AdminActions {
		found = found || ev.Action == "template.put"
	}
	if !found {
		t.Fatal("template changes are missing from the trust report's recent admin actions")
	}
}

// Only an operator key changes templates; any key can list them.
func TestTemplateChangesNeedAnOperatorKey(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	body := map[string]any{"images": map[string]string{"fc": "img"}}
	for _, c := range []struct{ method, path string }{
		{"PUT", "/v1/admin/templates/python"}, {"DELETE", "/v1/admin/templates/python"}, {"DELETE", "/v1/admin/templates/python/images/fc"},
	} {
		var env api.ErrorEnvelope
		if code := h.call(c.method, c.path, token, body, &env); code != 403 || env.Error.Code != api.CodePermissionDenied {
			t.Errorf("%s %s with a tenant key: status %d %+v", c.method, c.path, code, env.Error)
		}
	}
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, body, nil)
	var list api.List[api.Template]
	h.must(200, "GET", "/v1/templates", token, nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("a tenant key lists templates: %+v", list.Items)
	}
}

// A template set through the API is held to the config file's rules, and an
// image for a pool nobody registered is refused with the pool named.
func TestTemplateValidation(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	put := func(name string, body any) *api.Error {
		t.Helper()
		return h.refused(400, api.CodeInvalidRequest, "PUT", "/v1/admin/templates/"+name, body)
	}
	if e := put("python", map[string]any{"images": map[string]string{"daytona": "python-312"}}); !strings.Contains(e.Message, "pool daytona, which does not exist") || e.Details["pool"] != "daytona" || !strings.Contains(e.Message, "burst, fc") {
		t.Errorf("unknown pool: %+v", e)
	}
	if e := put("python", map[string]any{"images": map[string]string{"fc": "  "}}); !strings.Contains(e.Message, "images.fc is empty") {
		t.Errorf("empty image: %+v", e)
	}
	if e := put("python", map[string]any{"pools": []string{"fc"}, "images": map[string]string{"fc": "a", "burst": "b"}}); !strings.Contains(e.Message, "images names pool burst, which its pools list leaves out") {
		t.Errorf("image outside pools: %+v", e)
	}
	if e := put("python", map[string]any{"pools": []string{"ghost"}}); !strings.Contains(e.Message, "pools names pool ghost") {
		t.Errorf("unknown pool in pools: %+v", e)
	}
	if e := put("python", map[string]any{}); !strings.Contains(e.Message, "nothing to set") {
		t.Errorf("empty request: %+v", e)
	}
	if e := put("-bad", map[string]any{"images": map[string]string{"fc": "a"}}); !strings.Contains(e.Message, "template names are") {
		t.Errorf("bad name: %+v", e)
	}
	if e := put("python", []byte(`{"image":"x"}`)); !strings.Contains(e.Message, "unknown field") {
		t.Errorf("unknown field: %+v", e)
	}
	if got := h.templates(); len(got) != 0 {
		t.Fatalf("a refused request stored a template: %+v", got)
	}
	// Narrowing the pools list under an existing image is refused too, rather
	// than dropping the image.
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "a", "burst": "b"}}, nil)
	if e := put("python", map[string]any{"pools": []string{"fc"}}); !strings.Contains(e.Message, "images names pool burst") {
		t.Errorf("pools list that leaves out a pool with an image: %+v", e)
	}
	if got := h.templates(); len(got[0].Images) != 2 || len(got[0].Pools) != 0 {
		t.Fatalf("a refused change was stored: %+v", got[0])
	}
}

// The config file wins: a template it declares cannot be changed or removed
// through the API, and a stored template with the same name is ignored.
func TestConfigTemplateWinsOverTheAPI(t *testing.T) {
	h := newHarnessWith(t, overflowPolicy, func(c *config.Config) {
		c.Templates = []api.Template{{Name: "workbot", Description: "from the file", Images: map[string]string{"fc": "workbot-image-v3"}}}
	}, nil)
	_, _, token := h.tenant("a", api.Quota{})
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/v1/admin/templates/workbot", map[string]any{"images": map[string]string{"burst": "elsewhere"}}},
		{"DELETE", "/v1/admin/templates/workbot/images/fc", nil},
		{"DELETE", "/v1/admin/templates/workbot", nil},
	} {
		if e := h.refused(409, api.CodeConflict, c.method, c.path, c.body); !strings.Contains(e.Message, "declared in the config file") {
			t.Errorf("%s %s: %+v", c.method, c.path, e)
		}
	}
	// A stored template with the name (set before the file declared it, say)
	// stays out of the list and out of placement.
	if _, err := h.store.UpdateTemplate(context.Background(), "workbot", func(tpl *api.Template, _ bool) error {
		tpl.Source, tpl.Images = api.TemplateSourceAPI, map[string]string{"burst": "elsewhere"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "py"}}, nil)
	got := h.templates()
	if len(got) != 2 || got[0].Name != "workbot" || got[0].Source != api.TemplateSourceConfig || got[0].Images["fc"] != "workbot-image-v3" || got[0].Images["burst"] != "" ||
		got[1].Name != "python" || got[1].Source != api.TemplateSourceAPI {
		t.Fatalf("want the file's workbot, then the API's python: %+v", got)
	}
	sb := h.create(token, api.CreateSandboxRequest{Template: "workbot"})
	h.tick()
	if got := h.get(token, sb.ID); got.Pool != "fc" || h.fc.TemplateOf(sb.ID) != "workbot-image-v3" || h.ext.Count() != 0 {
		t.Fatalf("the stored template shadowed by the file was used: %+v", got)
	}
}

// A template only narrows: giving it an image on a pool never lets work go
// where the placement policy forbids, whatever the template says.
func TestTemplateNeverOpensAPoolPolicyForbids(t *testing.T) {
	// The global policy approves no external pool at all.
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"burst": "python-312"}}, nil)
	var env api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{Template: "python"}, &env); code != 422 || env.Error.Code != api.CodeNoEligiblePool {
		t.Fatalf("a template with an image on a forbidden pool: status %d %+v", code, env.Error)
	}
	h.tick()
	if h.ext.Count() != 0 || h.fc.Count() != 0 {
		t.Fatalf("instances started: burst %d, fc %d", h.ext.Count(), h.fc.Count())
	}

	// With the external pool approved, private-only work still stays private:
	// the template has no image there, so it runs nowhere.
	h = newHarness(t, overflowPolicy)
	_, _, token = h.tenant("a", api.Quota{})
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"burst": "python-312"}}, nil)
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{Template: "python", Classification: "private-only"}, &env); code != 422 || env.Error.Code != api.CodeNoEligiblePool {
		t.Fatalf("private-only work for a template held only by an external pool: status %d %+v", code, env.Error)
	}
	// Adding the private pool's image is what lets it run, and only there.
	h.must(200, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "registry.example.com/python:3.12"}}, nil)
	sb := h.create(token, api.CreateSandboxRequest{Template: "python", Classification: "private-only"})
	h.tick()
	if got := h.get(token, sb.ID); got.Pool != "fc" || got.Trust != api.TrustPrivate || h.ext.Count() != 0 {
		t.Fatalf("private-only work left the private pool: %+v", got)
	}
}

// Removing an image never widens where the template runs: the last one stays
// until the template itself is removed, because a template with no image
// would have its name passed to every pool as given.
func TestTemplateLastImageCannotBeRemoved(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "a", "burst": "b"}}, nil)
	h.refused(404, api.CodeNotFound, "DELETE", "/v1/admin/templates/python/images/ghost", nil)
	h.refused(404, api.CodeNotFound, "DELETE", "/v1/admin/templates/node/images/fc", nil)
	h.must(200, "DELETE", "/v1/admin/templates/python/images/burst", operatorToken, nil, nil)
	if e := h.refused(409, api.CodeInvalidState, "DELETE", "/v1/admin/templates/python/images/fc", nil); !strings.Contains(e.Message, "last image") || !strings.Contains(e.Message, "DELETE /v1/admin/templates/python") {
		t.Fatalf("removing the last image: %+v", e)
	}
	if got := h.templates(); len(got) != 1 || got[0].Images["fc"] != "a" {
		t.Fatalf("the refused removal changed the template: %+v", got)
	}
	if _, err := h.store.GetTemplate(context.Background(), "node"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("removing an image from a template that does not exist created it: %v", err)
	}
}

// flakyTemplates is a store whose template reads fail while failing is set.
type flakyTemplates struct {
	store.Store
	failing atomic.Bool
}

func (f *flakyTemplates) GetTemplate(ctx context.Context, name string) (api.Template, error) {
	if f.failing.Load() {
		return api.Template{}, errors.New("connection reset")
	}
	return f.Store.GetTemplate(ctx, name)
}

func (f *flakyTemplates) ListTemplates(ctx context.Context) ([]api.Template, error) {
	if f.failing.Load() {
		return nil, errors.New("connection reset")
	}
	return f.Store.ListTemplates(ctx)
}

// A template that cannot be read must never be treated as an unknown name:
// that would pass the name to the private pool as given, where the template
// says it has no image.
func TestPlacementFailsClosedWhenTemplateUnreadable(t *testing.T) {
	var flaky *flakyTemplates
	h := newHarnessWith(t, overflowPolicy, nil, func(st store.Store) store.Store {
		flaky = &flakyTemplates{Store: st}
		return flaky
	})
	_, _, token := h.tenant("a", api.Quota{})
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"burst": "python-312"}}, nil)
	waiting := h.create(token, api.CreateSandboxRequest{Template: "python"}) // queued until the scheduler's next pass

	flaky.failing.Store(true)
	h.tick()
	if got := h.get(token, waiting.ID); got.Status != api.StatusQueued || got.Pool != "" || h.fc.Count() != 0 || h.ext.Count() != 0 {
		t.Fatalf("sandbox was placed while its template was unreadable: %+v (fc holds %d, burst %d)", got, h.fc.Count(), h.ext.Count())
	}
	var env api.ErrorEnvelope
	if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{Template: "python"}, &env); code != 503 || env.Error.Code != api.CodeProviderUnavailable {
		t.Fatalf("create with an unreadable template: status %d %+v", code, env.Error)
	}
	if code := h.call("GET", "/v1/templates", token, nil, &env); code != 503 {
		t.Fatalf("listing unreadable templates: status %d", code)
	}

	flaky.failing.Store(false)
	_, _ = h.store.UpdateSandbox(context.Background(), waiting.ID, func(r *store.SandboxRecord) error { r.NextAttemptAt = time.Now(); return nil })
	h.tick()
	if got := h.get(token, waiting.ID); got.Pool != "burst" || h.ext.TemplateOf(waiting.ID) != "python-312" || h.fc.Count() != 0 {
		t.Fatalf("after the template was readable again: %+v", got)
	}
}

// changeAfterActivation runs change once, right after a sandbox was placed on
// a pool and before it is provisioned there.
type changeAfterActivation struct {
	store.Store
	once   sync.Once
	change func(store.Store)
}

func (c *changeAfterActivation) ActivateSandbox(ctx context.Context, id string, act store.Activation) (store.SandboxRecord, error) {
	out, err := c.Store.ActivateSandbox(ctx, id, act)
	if err == nil {
		c.once.Do(func() { c.change(c.Store) })
	}
	return out, err
}

// A template can change between placement and provisioning. A sandbox placed
// on a pool that has just lost its image is placed again instead of booting
// there, and the pool is not blamed.
func TestTemplateChangedBeforeProvisioningIsPlacedAgain(t *testing.T) {
	h := newHarnessWith(t, overflowPolicy, nil, func(st store.Store) store.Store {
		return &changeAfterActivation{Store: st, change: func(st store.Store) {
			_, err := st.UpdateTemplate(context.Background(), "python", func(tpl *api.Template, _ bool) error {
				tpl.Images = map[string]string{"burst": "python-312"}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}}
	})
	_, _, token := h.tenant("a", api.Quota{})
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "registry.example.com/python:3.12"}}, nil)
	sb := h.create(token, api.CreateSandboxRequest{Template: "python"})
	h.tick()
	got := h.get(token, sb.ID)
	if got.Status != api.StatusRunning || got.Pool != "burst" || h.ext.TemplateOf(sb.ID) != "python-312" {
		t.Fatalf("want it placed again on burst, got %+v", got)
	}
	if h.fc.Count() != 0 {
		t.Fatalf("fc booted %d instances for a template it no longer has", h.fc.Count())
	}
	if got.Attempts != 1 {
		t.Errorf("the move counted as a failed attempt: attempts %d", got.Attempts)
	}
	for _, v := range h.sched.Pools.Views(nil) {
		if v.Pool.Name == "fc" && !v.Healthy {
			t.Errorf("fc was marked unhealthy: %s", v.HealthDetail)
		}
	}
}

// Nothing caches templates per replica: a second scheduler on the same store
// (another API replica, or the leader) sees a change on its next read.
func TestTemplateChangeReachesEveryReplica(t *testing.T) {
	h := newHarness(t, overflowPolicy)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec, _ := auditlog.New(h.store, log, "")
	other := scheduler.New(h.store, h.pools, config.Default(), "inst", rec, log)
	ctx := context.Background()
	if _, ok, err := other.Template(ctx, "python"); ok || err != nil {
		t.Fatalf("before it is set: %v %v", ok, err)
	}
	h.must(201, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"fc": "a"}}, nil)
	tpl, ok, err := other.Template(ctx, "python")
	if !ok || err != nil || tpl.Images["fc"] != "a" || tpl.Source != api.TemplateSourceAPI {
		t.Fatalf("the other replica does not see the new template: %+v %v %v", tpl, ok, err)
	}
	h.must(200, "PUT", "/v1/admin/templates/python", operatorToken, map[string]any{"images": map[string]string{"burst": "b"}}, nil)
	in, err := other.PlacementInput(ctx, store.SandboxRecord{Sandbox: api.Sandbox{Template: "python", Classification: "internal"}}, nil)
	if err != nil || in.Template == nil || len(in.Template.Images) != 2 {
		t.Fatalf("the other replica places with a stale template: %+v %v", in.Template, err)
	}
	h.must(204, "DELETE", "/v1/admin/templates/python", operatorToken, nil, nil)
	if list, err := other.Templates(ctx); err != nil || len(list) != 0 {
		t.Fatalf("the other replica still lists a removed template: %+v %v", list, err)
	}
}
