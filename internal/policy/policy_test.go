package policy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

var (
	vmFeatures  = []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureSnapshotDisk, adapter.FeatureMicroVM}
	extFeatures = []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeaturePorts}
)

func pool(name, trust string, free int, features []string, cpuHour float64) PoolView {
	if trust == api.TrustExternal {
		// External providers report unbounded capacity; the pool cap governs.
		return PoolView{
			Pool:         api.Pool{Name: name, Trust: trust, Enabled: true, Region: "us-east", MaxConcurrency: 10, CostModel: api.CostModel{CPUHour: cpuHour}},
			Capabilities: adapter.Capabilities{Features: features},
			Healthy:      true,
		}
	}
	return PoolView{
		Pool:         api.Pool{Name: name, Trust: trust, Enabled: true, Region: "us-east", CostModel: api.CostModel{CPUHour: cpuHour}},
		Capabilities: adapter.Capabilities{Features: features},
		Capacity:     adapter.Capacity{Bounded: true, TotalCPU: 16, FreeCPU: free, TotalMemoryMiB: 65536, FreeMemoryMiB: free * 4096},
		Healthy:      true,
	}
}

func input(class api.Classification, policies []api.PlacementPolicy, pools ...PoolView) Input {
	return Input{
		Sandbox:        api.Sandbox{ID: "sbx_1", Classification: class.Name, Resources: api.Resources{CPU: 2, MemoryMiB: 2048}},
		Policies:       policies,
		Classification: class,
		Pools:          pools,
		Now:            time.Now(),
	}
}

var (
	internal    = api.Classification{Name: "internal", External: api.ExternalAllowed}
	privateOnly = api.Classification{Name: "private-only", External: api.ExternalNever}
	overflow    = []api.PlacementPolicy{{Scope: "global", External: api.ExternalOverflow, ExternalPools: []string{"daytona"}}}
)

func TestPrivateFirstPrefersPrivateCapacity(t *testing.T) {
	res := Decide(input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Decision.Pool != "fc" || len(res.Ranked) != 1 {
		t.Fatalf("want fc only, got %+v", res.Ranked)
	}
}

// Scenario D: explicitly authorized overflow goes external when private is full.
func TestOverflowWhenPrivateFull(t *testing.T) {
	res := Decide(input(internal, overflow, pool("fc", api.TrustPrivate, 0, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Decision.Pool != "daytona" {
		t.Fatalf("want overflow to daytona, got %+v", res.Decision)
	}
}

// Registering a provider authorizes nothing: without approval it is never used.
func TestUnapprovedExternalPoolIsNeverUsed(t *testing.T) {
	policies := []api.PlacementPolicy{{Scope: "global", External: api.ExternalAllowed}}
	res := Decide(input(internal, policies, pool("fc", api.TrustPrivate, 0, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Decision.Outcome != "waiting" || len(res.Ranked) != 0 {
		t.Fatalf("unapproved external pool was ranked: %+v", res.Decision)
	}
}

// Scenario E: private-only work stays queued even when only external has room.
func TestPrivateOnlyNeverLeaves(t *testing.T) {
	permissive := []api.PlacementPolicy{{Scope: "global", External: api.ExternalAllowed, ExternalPools: []string{"daytona"}}}
	res := Decide(input(privateOnly, permissive, pool("fc", api.TrustPrivate, 0, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Decision.Outcome != "waiting" || len(res.Ranked) != 0 {
		t.Fatalf("private-only escaped: %+v", res.Decision)
	}
	// With no private pool at all it is rejected rather than routed outside.
	res = Decide(input(privateOnly, permissive, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Err == nil || res.Err.Code != api.CodeNoEligiblePool {
		t.Fatalf("want no_eligible_pool, got %+v", res.Err)
	}
}

// A request or a narrower scope can tighten but never widen external access.
func TestNarrowerScopesOnlyTighten(t *testing.T) {
	policies := append(overflow, api.PlacementPolicy{Scope: "project:p1", External: api.ExternalAllowed})
	in := input(internal, policies, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	if eff := Merge(in); eff.External != api.ExternalOverflow {
		t.Fatalf("project widened external to %q", eff.External)
	}
	in.Sandbox.Placement = &api.Placement{External: api.ExternalNever}
	in.Pools[0].Capacity.FreeCPU = 0
	if res := Decide(in); len(res.Ranked) != 0 {
		t.Fatalf("request opt-out ignored: %v", res.Ranked)
	}
}

// Scenario I: a capability mismatch is a precise error, or another capable
// authorized backend is chosen.
func TestCapabilityMismatch(t *testing.T) {
	in := input(internal, nil, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05))
	in.Sandbox.Capabilities = []string{adapter.FeaturePorts}
	res := Decide(in)
	if res.Err == nil || res.Err.Code != api.CodeUnsupportedCapability {
		t.Fatalf("want unsupported_capability, got %+v", res.Err)
	}
	in = input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Capabilities = []string{adapter.FeaturePorts}
	if res := Decide(in); res.Decision.Pool != "daytona" {
		t.Fatalf("want capable authorized backend daytona, got %+v", res.Decision)
	}
}

func TestCostStrategyAndCeiling(t *testing.T) {
	policies := []api.PlacementPolicy{{Scope: "global", Strategy: api.StrategyCost, MaxCostPerHour: 0.15}}
	res := Decide(input(internal, policies,
		pool("expensive", api.TrustPrivate, 8, vmFeatures, 0.10), // 0.20/h, over ceiling
		pool("cheap", api.TrustPrivate, 8, vmFeatures, 0.02),
		pool("mid", api.TrustPrivate, 8, vmFeatures, 0.05)))
	if len(res.Ranked) != 2 || res.Ranked[0] != "cheap" || res.Ranked[1] != "mid" {
		t.Fatalf("ranked %v", res.Ranked)
	}
}

func TestDrainingAndAllowedPools(t *testing.T) {
	a, b := pool("a", api.TrustPrivate, 8, vmFeatures, 0), pool("b", api.TrustPrivate, 8, vmFeatures, 0)
	a.Pool.Draining = true
	res := Decide(input(internal, []api.PlacementPolicy{{Scope: "org:o1", AllowedPools: []string{"a", "b"}}, {Scope: "project:p1", AllowedPools: []string{"a"}}}, a, b))
	if res.Err == nil {
		t.Fatalf("project narrowed to a (draining) yet got %v", res.Ranked)
	}
}

func TestDecisionExplainsEveryPool(t *testing.T) {
	res := Decide(input(privateOnly, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0), pool("daytona", api.TrustExternal, 8, extFeatures, 0)))
	if len(res.Decision.Candidates) != 2 {
		t.Fatalf("candidates %+v", res.Decision.Candidates)
	}
	for _, c := range res.Decision.Candidates {
		if c.Pool == "daytona" && (c.Eligible || len(c.Reasons) == 0) {
			t.Fatalf("daytona should be ineligible with a reason: %+v", c)
		}
	}
}

// One template name routes to every pool that has its own image for it, and
// to no other pool; each pool's adapter is checked against its own image.
func TestTemplateImagesLimitAndRoute(t *testing.T) {
	tpl := api.Template{Name: "workbot", Images: map[string]string{"daytona": "workbot-daytona-v3"}}
	osb := pool("osb", api.TrustPrivate, 8, vmFeatures, 0.05)
	in := input(internal, overflow, osb, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template, in.Template = "workbot", &tpl
	res := Decide(in)
	if res.Decision.Pool != "daytona" {
		t.Fatalf("only the pool with an image should run the template, got %+v", res.Decision)
	}
	// A pool advertising a fixed template list is checked against the mapped image, not the name.
	osb.Capabilities.Templates = []string{"workbot-osb"}
	tpl.Images["osb"] = "workbot-osb"
	in = input(internal, overflow, osb, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template, in.Template = "workbot", &tpl
	if res := Decide(in); res.Decision.Pool != "osb" {
		t.Fatalf("private pool with a mapped image should win, got %+v", res.Decision)
	}
}

// attested returns a copy of pv whose pool holds the named attestations.
func attested(pv PoolView, held map[string]api.Attestation) PoolView {
	pv.Pool.Attestations = held
	return pv
}

var allowBurst = []api.PlacementPolicy{{Scope: "global", External: api.ExternalAllowed, ExternalPools: []string{"daytona"}}}

// Work that requires an attestation goes only to a pool that holds it, and
// the explanation names what the other pools lack.
func TestRequiredAttestationsFilterPools(t *testing.T) {
	regulated := api.Classification{Name: "regulated", External: api.ExternalAllowed, RequiredAttestations: []string{"soc2-type2"}}
	fc := pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05)
	vendor := attested(pool("daytona", api.TrustExternal, 0, extFeatures, 0.01), map[string]api.Attestation{"soc2-type2": {Evidence: "https://trust.example.com"}})
	res := Decide(input(regulated, allowBurst, fc, vendor))
	if res.Decision.Pool != "daytona" || len(res.Ranked) != 1 {
		t.Fatalf("want only the attested pool, got %+v", res.Ranked)
	}
	if got := res.Decision.RequiredAttestations; len(got) != 1 || got[0] != "soc2-type2" {
		t.Fatalf("decision does not record the requirement: %v", got)
	}
	for _, c := range res.Decision.Candidates {
		if c.Pool == "fc" && (c.Eligible || len(c.Reasons) != 1 || c.Reasons[0] != "missing attestations: soc2-type2") {
			t.Fatalf("fc should be ruled out by name: %+v", c)
		}
	}
	// A private pool is not exempt: it qualifies once the operator records it.
	res = Decide(input(regulated, allowBurst, attested(fc, map[string]api.Attestation{"soc2-type2": {}}), vendor))
	if res.Decision.Pool != "fc" {
		t.Fatalf("attested private pool should win private-first: %+v", res.Decision)
	}
}

// Requirements from every level add up; no level can drop one.
func TestRequiredAttestationsOnlyAccumulate(t *testing.T) {
	regulated := api.Classification{Name: "regulated", External: api.ExternalAllowed, RequiredAttestations: []string{"soc2-type2"}}
	policies := append(allowBurst[:1:1], api.PlacementPolicy{Scope: "org:o1", RequiredAttestations: []string{"iso27001"}}, api.PlacementPolicy{Scope: "project:p1"})
	in := input(regulated, policies)
	in.Sandbox.Placement = &api.Placement{RequiredAttestations: []string{"hipaa-baa", "soc2-type2"}}
	eff := Merge(in)
	if got := strings.Join(eff.RequiredAttestations, ","); got != "hipaa-baa,iso27001,soc2-type2" {
		t.Fatalf("want the union of every level, got %q", got)
	}
	// Holding two of three is not enough.
	partial := attested(pool("daytona", api.TrustExternal, 0, extFeatures, 0.01), map[string]api.Attestation{"soc2-type2": {}, "iso27001": {}})
	in.Pools = []PoolView{partial}
	res := Decide(in)
	if res.Err == nil || res.Err.Code != api.CodeNoEligiblePool {
		t.Fatalf("want no_eligible_pool, got %+v", res.Err)
	}
	if res.Err.Details["requiredAttestations"] != "hipaa-baa,iso27001,soc2-type2" || !strings.Contains(res.Decision.Reason, "attestations") {
		t.Fatalf("the refusal does not name the attestations: %+v / %q", res.Err.Details, res.Decision.Reason)
	}
}

// An attestation past its validUntil no longer counts, and the explanation
// gives the day it expired.
func TestExpiredAttestationDoesNotCount(t *testing.T) {
	regulated := api.Classification{Name: "regulated", External: api.ExternalAllowed, RequiredAttestations: []string{"soc2-type2"}}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	expired, valid := now.Add(-48*time.Hour), now.Add(48*time.Hour)
	in := input(regulated, allowBurst, attested(pool("daytona", api.TrustExternal, 0, extFeatures, 0.01), map[string]api.Attestation{"soc2-type2": {ValidUntil: &expired}}))
	in.Now = now
	res := Decide(in)
	if res.Err == nil || len(res.Ranked) != 0 {
		t.Fatalf("expired attestation was accepted: %+v", res.Decision)
	}
	if got := res.Decision.Candidates[0].Reasons; len(got) != 1 || got[0] != "missing attestations: soc2-type2 (expired 2026-11-14)" {
		t.Fatalf("reasons %v", got)
	}
	// A caller that leaves Now unset must not make expired evidence valid.
	in.Now = time.Time{}
	if res := Decide(in); len(res.Ranked) != 0 {
		t.Fatalf("zero clock accepted an expired attestation: %+v", res.Decision)
	}
	in.Now = now
	in.Pools[0].Pool.Attestations = map[string]api.Attestation{"soc2-type2": {ValidUntil: &valid}}
	if res := Decide(in); res.Decision.Pool != "daytona" {
		t.Fatalf("valid attestation refused: %+v", res.Decision)
	}
}

// Without requirements, attestations change nothing.
func TestNoRequirementsIgnoresAttestations(t *testing.T) {
	res := Decide(input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05)))
	if res.Decision.Pool != "fc" || len(res.Decision.RequiredAttestations) != 0 {
		t.Fatalf("unexpected decision %+v", res.Decision)
	}
}

// budgeted returns a copy of pv with a monthly budget and this month's spend.
func budgeted(pv PoolView, monthly, spent float64) PoolView {
	pv.Pool.Budget = &api.Budget{Monthly: monthly}
	pv.Spent = spent
	return pv
}

// A pool whose estimated spend has reached its monthly budget takes no new
// work; the sandbox goes to the next pool, or waits if there is none.
func TestMonthlyBudgetHoldsAPool(t *testing.T) {
	cheap := pool("cheap", api.TrustPrivate, 8, vmFeatures, 0.01)
	dear := pool("dear", api.TrustPrivate, 8, vmFeatures, 0.05)
	res := Decide(input(internal, nil, budgeted(cheap, 100, 99.99), dear))
	if res.Decision.Pool != "cheap" {
		t.Fatalf("a pool under budget must still take work: %+v", res.Decision)
	}
	res = Decide(input(internal, nil, budgeted(cheap, 100, 100), dear))
	if res.Decision.Pool != "dear" || len(res.Ranked) != 1 {
		t.Fatalf("want the next pool once the budget is reached, got %+v", res.Ranked)
	}
	for _, c := range res.Decision.Candidates {
		if c.Pool == "cheap" && (!c.Eligible || c.HasCapacity || len(c.Reasons) != 1 || c.Reasons[0] != "monthly budget reached: estimated 100.00 of 100.00") {
			t.Fatalf("the held pool should stay eligible and say why it is held: %+v", c)
		}
	}
	// With nowhere else to go the sandbox waits; it is not refused.
	res = Decide(input(internal, nil, budgeted(cheap, 100, 250)))
	if res.Err != nil || res.Decision.Outcome != "waiting" {
		t.Fatalf("want waiting, got %+v / %v", res.Decision, res.Err)
	}
	// A budget of zero is no budget.
	if res := Decide(input(internal, nil, budgeted(cheap, 0, 1e9))); res.Decision.Pool != "cheap" {
		t.Fatalf("zero budget held the pool: %+v", res.Decision)
	}
}

// A private pool at its budget counts as full, so approved overflow may use
// an external pool; an unreadable spend holds the pool rather than pass it.
func TestBudgetReachedOverflowsAndUnknownSpendHolds(t *testing.T) {
	fc := budgeted(pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), 50, 50)
	res := Decide(input(internal, overflow, fc, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01)))
	if res.Decision.Pool != "daytona" {
		t.Fatalf("want overflow once the private budget is reached, got %+v", res.Decision)
	}
	// private-only work still never leaves.
	if res := Decide(input(privateOnly, overflow, fc, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))); len(res.Ranked) != 0 {
		t.Fatalf("a budget moved private-only work outside: %+v", res.Ranked)
	}
	unknown := budgeted(pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), 50, 0)
	unknown.SpendUnknown = true
	res = Decide(input(internal, nil, unknown))
	if len(res.Ranked) != 0 || res.Decision.Outcome != "waiting" || !strings.Contains(res.Decision.Candidates[0].Reasons[0], "could not be read") {
		t.Fatalf("an unreadable spend must hold the pool: %+v", res.Decision)
	}
}

// A template that no permitted pool has is refused with that reason, not a
// general one: each backend boots its own images, so this is the thing an
// operator has to add.
func TestTemplateMissingEverywhereIsNamed(t *testing.T) {
	tpl := api.Template{Name: "python", Images: map[string]string{"fc": "registry.example.com/python:3.12"}}

	// The only pool the request allows has no image for the template.
	in := input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template, in.Template = "python", &tpl
	in.Sandbox.Placement = &api.Placement{AllowedPools: []string{"daytona"}}
	res := Decide(in)
	if res.Err == nil || res.Err.Code != api.CodeNoEligiblePool || res.Err.Message != "no permitted pool has template python" || res.Err.Details["template"] != "python" {
		t.Fatalf("want no_eligible_pool naming the template, got %+v", res.Err)
	}

	// A backend that lists its templates and lacks this one counts the same.
	listed := pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05)
	listed.Capabilities.Templates = []string{"tpl-other"}
	in = input(internal, nil, listed)
	in.Sandbox.Template = "tpl-python"
	if res := Decide(in); res.Err == nil || res.Err.Message != "no permitted pool has template tpl-python" {
		t.Fatalf("want the template named, got %+v", res.Err)
	}

	// When policy is what forbids the only pool with the template, the
	// template is not the reason and is not blamed.
	in = input(privateOnly, overflow, pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template, in.Template = "python", &api.Template{Name: "python", Images: map[string]string{"fc": "x"}}
	if res := Decide(in); res.Err == nil || strings.Contains(res.Err.Message, "template") {
		t.Fatalf("policy forbids the pool, so the template must not be blamed: %+v", res.Err)
	}

	// The pool that has it still gets the work.
	in = input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template, in.Template = "python", &tpl
	if res := Decide(in); res.Err != nil || res.Decision.Pool != "fc" {
		t.Fatalf("want fc, got %+v %+v", res.Decision, res.Err)
	}
}

// A restore or fork that its snapshot's pool cannot take says so: snapshots
// do not move between pools, so no other pool is an answer.
func TestRestoreRefusedNamesTheSnapshotsPool(t *testing.T) {
	in := input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.PinnedPool = "fc"
	in.Sandbox.Placement = &api.Placement{AllowedPools: []string{"daytona"}}
	res := Decide(in)
	if res.Err == nil || res.Err.Code != api.CodeNoEligiblePool || res.Err.Details["pool"] != "fc" || !strings.Contains(res.Err.Message, "the pool that holds it, fc") {
		t.Fatalf("want no_eligible_pool naming the snapshot's pool, got %+v", res.Err)
	}
	// With nothing in the way it goes to that pool and nowhere else.
	in.Sandbox.Placement = nil
	if res := Decide(in); res.Err != nil || res.Decision.Pool != "fc" || len(res.Ranked) != 1 {
		t.Fatalf("want fc only, got %+v %+v", res.Ranked, res.Err)
	}
}

// A sandbox that was limited by a template is not placed anywhere once the
// template is removed: its name is never passed to a backend as given, even
// on pools that could boot it and that policy permits.
func TestRemovedTemplateMakesNoPoolEligible(t *testing.T) {
	in := input(internal, overflow, pool("fc", api.TrustPrivate, 8, vmFeatures, 0.05), pool("daytona", api.TrustExternal, 0, extFeatures, 0.01))
	in.Sandbox.Template = "python"
	if res := Decide(in); res.Err != nil || res.Decision.Pool != "fc" {
		t.Fatalf("a plain name runs where policy permits: %+v %+v", res.Decision, res.Err)
	}
	in.TemplateRemoved = true
	res := Decide(in)
	if res.Err == nil || res.Err.Code != api.CodeNoEligiblePool || res.Err.Details["template"] != "python" || !strings.Contains(res.Err.Message, "template python was removed") {
		t.Fatalf("want no_eligible_pool naming the removed template, got %+v", res.Err)
	}
	if len(res.Ranked) != 0 {
		t.Fatalf("ranked %v for a sandbox whose template was removed", res.Ranked)
	}
	for _, c := range res.Decision.Candidates {
		if c.Eligible || !slices.Contains(c.Reasons, "template python was removed after this sandbox was requested") {
			t.Errorf("pool %s: eligible=%v reasons=%v", c.Pool, c.Eligible, c.Reasons)
		}
	}
	// It outranks every other explanation, such as a missing attestation.
	in.Sandbox.Placement = &api.Placement{RequiredAttestations: []string{"soc2-type2"}}
	if res := Decide(in); res.Err == nil || !strings.Contains(res.Err.Message, "template python was removed") {
		t.Fatalf("want the removed template named first, got %+v", res.Err)
	}
}
