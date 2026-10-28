package policy

import (
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
