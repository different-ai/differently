package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// attest records attestations on a harness pool, as a config file would.
func (h *harness) attest(pool string, held map[string]api.Attestation) {
	h.t.Helper()
	ctx := context.Background()
	p, err := h.store.GetPool(ctx, pool)
	if err != nil {
		h.t.Fatal(err)
	}
	p.Attestations = held
	if err := h.store.UpsertPool(ctx, p); err != nil {
		h.t.Fatal(err)
	}
	if err := h.api.SyncPools(ctx); err != nil {
		h.t.Fatal(err)
	}
}

// Data of a class that requires an attestation runs only on a pool that
// holds it. With no such pool the request is refused up front and by name;
// an expired attestation stops counting.
func TestAttestationsDecideWhereRegulatedWorkRuns(t *testing.T) {
	policy := api.PlacementPolicy{External: api.ExternalAllowed, ExternalPools: []string{"burst"}}
	h := newHarnessWith(t, policy, func(c *config.Config) {
		c.Classifications = append(c.Classifications, api.Classification{Name: "regulated", External: api.ExternalAllowed, RequiredAttestations: []string{"soc2-type2"}})
	}, nil)
	_, _, token := h.tenant("acme", api.Quota{})
	regulated := api.CreateSandboxRequest{Classification: "regulated", Resources: &api.Resources{CPU: 1, MemoryMiB: 1024}}

	refused := func(req api.CreateSandboxRequest, wantInBody ...string) {
		t.Helper()
		var raw string
		if got := h.call("POST", "/v1/sandboxes", token, req, &raw); got != 422 {
			t.Fatalf("status %d, want 422: %s", got, raw)
		}
		for _, want := range append([]string{api.CodeNoEligiblePool}, wantInBody...) {
			if !strings.Contains(raw, want) {
				t.Fatalf("refusal does not mention %q: %s", want, raw)
			}
		}
	}
	// Neither pool holds the report yet, the private one included.
	refused(regulated, "soc2-type2", "attestations")

	h.attest("burst", map[string]api.Attestation{"soc2-type2": {Evidence: "https://trust.example.com/report"}})
	sb := h.create(token, regulated)
	h.tick()
	got := h.get(token, sb.ID)
	if got.Pool != "burst" || got.Decision == nil {
		t.Fatalf("regulated work should run on the attested pool: %+v", got)
	}
	if req := got.Decision.RequiredAttestations; len(req) != 1 || req[0] != "soc2-type2" {
		t.Fatalf("decision does not record the requirement: %v", req)
	}
	var why string
	for _, c := range got.Decision.Candidates {
		if c.Pool == "fc" {
			why = strings.Join(c.Reasons, "; ")
		}
	}
	if why != "missing attestations: soc2-type2" {
		t.Fatalf("explanation for the private pool: %q", why)
	}
	// Work with no requirement still prefers the private pool.
	plain := h.create(token, api.CreateSandboxRequest{Resources: &api.Resources{CPU: 1, MemoryMiB: 1024}})
	h.tick()
	if got := h.get(token, plain.ID); got.Pool != "fc" {
		t.Fatalf("unregulated work moved: %+v", got)
	}

	// A request adds to the class's requirements; it cannot drop one.
	more := regulated
	more.Placement = &api.Placement{RequiredAttestations: []string{"hipaa-baa"}}
	refused(more, "hipaa-baa,soc2-type2")
	var raw string
	more.Placement = &api.Placement{RequiredAttestations: []string{"Not A Name"}}
	if code := h.call("POST", "/v1/sandboxes", token, more, &raw); code != 400 || !strings.Contains(raw, "requiredAttestations") {
		t.Fatalf("malformed attestation name: %d %s", code, raw)
	}

	// Tenants see which attestations a pool holds, never the evidence.
	h.call("GET", "/v1/capabilities", token, nil, &raw)
	if !strings.Contains(raw, `"attestations":["soc2-type2"]`) || strings.Contains(raw, "trust.example.com") {
		t.Fatalf("capabilities view: %s", raw)
	}

	// Once the report lapses, new regulated work is refused again.
	lapsed := time.Now().Add(-time.Hour)
	h.attest("burst", map[string]api.Attestation{"soc2-type2": {Evidence: "https://trust.example.com/report", ValidUntil: &lapsed}})
	refused(regulated, "soc2-type2")
	h.call("GET", "/v1/capabilities", token, nil, &raw)
	if strings.Contains(raw, "soc2-type2") {
		t.Fatalf("an expired attestation is still shown as held: %s", raw)
	}
}

// Operators record attestations through the pool API; the server stamps who
// declared each one, replaces the set on update, and reports them.
func TestPoolAttestationsThroughAPI(t *testing.T) {
	wiring.Register()
	h := newHarness(t, api.PlacementPolicy{})
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapterhttp.Handler(fake.New(fake.Options{}), "").ServeHTTP(w, r)
	}))
	t.Cleanup(plugin.Close)

	body := map[string]any{
		"name": "vendor", "adapter": "remote", "trust": "external", "config": map[string]string{"endpoint": plugin.URL},
		"attestations": map[string]any{"soc2-type2": map[string]string{"evidence": "https://trust.example.com", "validUntil": "2027-06-30T00:00:00Z", "recordedBy": "someone-else"}},
	}
	var pool api.Pool
	h.must(201, "POST", "/v1/admin/pools", operatorToken, body, &pool)
	first := pool.Attestations["soc2-type2"]
	if first.Evidence != "https://trust.example.com" || first.ValidUntil == nil || first.RecordedAt == nil || first.RecordedBy == "" || first.RecordedBy == "someone-else" {
		t.Fatalf("the server must keep the evidence and stamp who recorded it: %+v", first)
	}

	body["name"], body["attestations"] = "bad", map[string]any{"SOC 2": map[string]string{}}
	var raw string
	if code := h.call("POST", "/v1/admin/pools", operatorToken, body, &raw); code != 400 || !strings.Contains(raw, "attestation name") {
		t.Fatalf("malformed name on create: %d %s", code, raw)
	}
	if code := h.call("PATCH", "/v1/admin/pools/vendor", operatorToken, map[string]any{"attestations": map[string]any{"soc2/type2": map[string]string{}}}, &raw); code != 400 {
		t.Fatalf("malformed name on update: %d %s", code, raw)
	}

	// patch decodes into a fresh pool: decoding into a used one would keep
	// map entries the response no longer has.
	patch := func(change map[string]any) map[string]api.Attestation {
		t.Helper()
		var updated api.Pool
		h.must(200, "PATCH", "/v1/admin/pools/vendor", operatorToken, change, &updated)
		return updated.Attestations
	}
	// An update replaces the whole set. An unchanged entry keeps its record.
	held := patch(map[string]any{"attestations": map[string]any{
		"soc2-type2": map[string]string{"evidence": "https://trust.example.com", "validUntil": "2027-06-30T00:00:00Z"},
		"hipaa-baa":  map[string]string{"evidence": "BAA signed 2026-09-01"},
	}})
	if len(held) != 2 || !held["soc2-type2"].RecordedAt.Equal(*first.RecordedAt) || held["hipaa-baa"].RecordedAt == nil {
		t.Fatalf("after adding one: %+v", held)
	}
	held = patch(map[string]any{"attestations": map[string]any{"hipaa-baa": map[string]string{"evidence": "BAA signed 2026-09-01"}}})
	if _, still := held["soc2-type2"]; still || len(held) != 1 {
		t.Fatalf("an update must replace the set: %+v", held)
	}
	// Other updates leave attestations alone.
	if held = patch(map[string]any{"maxConcurrency": 5}); len(held) != 1 {
		t.Fatalf("an unrelated update dropped attestations: %+v", held)
	}
	// A pool declared in the config file takes its attestations from the file.
	if code := h.call("PATCH", "/v1/admin/pools/fc", operatorToken, map[string]any{"attestations": map[string]any{"soc2-type2": map[string]string{}}}, &raw); code != 409 {
		t.Fatalf("config-declared pool: %d %s", code, raw)
	}

	h.call("GET", "/v1/admin/trust-report", operatorToken, nil, &raw)
	if !strings.Contains(raw, "BAA signed 2026-09-01") || !strings.Contains(raw, "does not verify a provider's certification") {
		t.Fatalf("trust report lacks the attestation or its limitation: %s", raw)
	}
	if held = patch(map[string]any{"attestations": map[string]any{}}); len(held) != 0 {
		t.Fatalf("an empty set must clear attestations: %+v", held)
	}
}
