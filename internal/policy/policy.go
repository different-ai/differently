// Package policy decides where a sandbox may run and where it should run.
//
// Placement is filter-then-rank. Hard constraints are evaluated first and can
// only be narrowed by more specific scopes: classification, external mode,
// allowed/denied pools, regions, required capabilities, resource limits and
// cost ceilings. Only pools that pass every constraint are ranked by strategy.
// Security and residency therefore always win over cost or speed, and an
// external provider receives work only when every level permits it and it is
// explicitly approved — registering a provider authorizes nothing.
package policy

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// PoolView is the scheduler's current knowledge of one pool.
type PoolView struct {
	Pool         api.Pool
	Capabilities adapter.Capabilities
	Capacity     adapter.Capacity
	Healthy      bool
	HealthDetail string
	Active       int
}

// Input is everything placement needs.
type Input struct {
	Sandbox        api.Sandbox
	Policies       []api.PlacementPolicy // global, org, project — any may be absent
	Classification api.Classification
	Pools          []PoolView
	ExcludedPools  []string // pools that already failed this sandbox
	PinnedPool     string   // e.g. the pool holding a snapshot to restore
	// Template is the configured template the sandbox asks for, if any; it
	// limits the pools and names each pool's own image.
	Template *api.Template
	Now      time.Time
}

// Effective is the merged policy for one request.
type Effective struct {
	External       string
	AllowedPools   []string // nil means any
	DeniedPools    []string
	ExternalPools  []string // approved external pools; empty means none
	Regions        []string // nil means any
	Strategy       string
	MaxCostPerHour float64
	MaxQueue       time.Duration
	Sources        []string
}

var externalRank = map[string]int{api.ExternalNever: 0, api.ExternalOverflow: 1, api.ExternalAllowed: 2}

// ValidExternal reports whether s is a known external mode.
func ValidExternal(s string) bool { _, ok := externalRank[s]; return ok }

// ValidStrategy reports whether s is a known strategy.
func ValidStrategy(s string) bool {
	return s == api.StrategyPrivateFirst || s == api.StrategyCost || s == api.StrategySpread
}

func stricter(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" || externalRank[b] < externalRank[a] {
		return b
	}
	return a
}

func intersect(acc []string, next []string) []string {
	if len(next) == 0 {
		return acc
	}
	if acc == nil {
		return slices.Clone(next)
	}
	var out []string
	for _, v := range acc {
		if slices.Contains(next, v) {
			out = append(out, v)
		}
	}
	if out == nil {
		out = []string{} // an empty intersection allows nothing
	}
	return out
}

// Merge combines policies, classification and the request preference.
func Merge(in Input) Effective {
	eff := Effective{Strategy: api.StrategyPrivateFirst}
	// Fail closed: with no global policy, nothing may leave private pools.
	globalExternal := api.ExternalNever
	var externalPools []string
	for _, p := range in.Policies {
		eff.Sources = append(eff.Sources, p.Scope)
		if p.Scope == "global" {
			if p.External != "" {
				globalExternal = p.External
			}
		} else {
			eff.External = stricter(eff.External, p.External)
		}
		eff.AllowedPools = intersect(eff.AllowedPools, p.AllowedPools)
		eff.Regions = intersect(eff.Regions, p.AllowedRegions)
		eff.DeniedPools = append(eff.DeniedPools, p.DeniedPools...)
		externalPools = intersect(externalPools, p.ExternalPools)
		if p.Strategy != "" {
			eff.Strategy = p.Strategy // policies arrive least to most specific
		}
		if p.MaxCostPerHour > 0 && (eff.MaxCostPerHour == 0 || p.MaxCostPerHour < eff.MaxCostPerHour) {
			eff.MaxCostPerHour = p.MaxCostPerHour
		}
		if q := time.Duration(p.MaxQueueSeconds) * time.Second; q > 0 && (eff.MaxQueue == 0 || q < eff.MaxQueue) {
			eff.MaxQueue = q
		}
	}
	eff.External = stricter(globalExternal, eff.External)
	classExternal := in.Classification.External
	if classExternal == "" {
		classExternal = api.ExternalNever
	}
	eff.External = stricter(eff.External, classExternal)
	if pl := in.Sandbox.Placement; pl != nil {
		eff.External = stricter(eff.External, pl.External)
		eff.AllowedPools = intersect(eff.AllowedPools, pl.AllowedPools)
		eff.Regions = intersect(eff.Regions, pl.Regions)
		if pl.Strategy != "" {
			eff.Strategy = pl.Strategy
		}
	}
	eff.ExternalPools = externalPools
	return eff
}

// PolicyReasons says why the effective policy keeps work off pool p: denied,
// not allowed, external without approval, or outside the allowed regions.
// Empty means the policy lets work go there. Capacity, capabilities,
// templates and cost are not considered.
func PolicyReasons(eff Effective, p api.Pool) []string {
	var out []string
	if slices.Contains(eff.DeniedPools, p.Name) {
		out = append(out, "denied by policy")
	}
	if eff.AllowedPools != nil && !slices.Contains(eff.AllowedPools, p.Name) {
		out = append(out, "not in allowed pools")
	}
	if p.Trust != api.TrustPrivate {
		switch {
		case eff.External == api.ExternalNever:
			out = append(out, fmt.Sprintf("external placement not permitted (effective mode %q)", eff.External))
		case !slices.Contains(eff.ExternalPools, p.Name):
			out = append(out, "external pool not approved by policy")
		}
	}
	if eff.Regions != nil && !slices.Contains(eff.Regions, p.Region) {
		out = append(out, fmt.Sprintf("region %q not allowed", p.Region))
	}
	return out
}

// Result is a placement decision plus the ranked pools to try in order.
type Result struct {
	Decision *api.PlacementDecision
	Ranked   []string
	// Err is set when no pool can ever satisfy the request.
	Err *api.Error
}

// Decide evaluates every pool and ranks the placeable ones.
func Decide(in Input) Result {
	eff := Merge(in)
	sb := in.Sandbox
	dec := &api.PlacementDecision{Strategy: eff.Strategy, External: eff.External, Policies: eff.Sources, EvaluatedAt: in.Now}
	if in.Classification.Name != "" {
		dec.Policies = append(dec.Policies, "classification:"+in.Classification.Name)
	}

	type cand struct {
		view api.CandidateEvaluation
		pv   PoolView
	}
	var cands []cand
	capabilityOnly := 0
	for _, pv := range in.Pools {
		p := pv.Pool
		ev := api.CandidateEvaluation{Pool: p.Name, Trust: p.Trust, Eligible: true}
		reject := func(format string, args ...any) {
			ev.Eligible = false
			ev.Reasons = append(ev.Reasons, fmt.Sprintf(format, args...))
		}
		if !p.Enabled {
			reject("pool disabled")
		}
		if p.Draining {
			reject("pool draining")
		}
		if slices.Contains(in.ExcludedPools, p.Name) {
			reject("an earlier provisioning attempt on this pool failed")
		}
		if in.PinnedPool != "" && p.Name != in.PinnedPool {
			reject("restore requires pool %s", in.PinnedPool)
		}
		for _, why := range PolicyReasons(eff, p) {
			reject("%s", why)
		}
		missing := pv.Capabilities.Missing(sb.Capabilities)
		policyOK := ev.Eligible
		if len(missing) > 0 {
			reject("missing capabilities: %s", strings.Join(missing, ", "))
		}
		image := sb.Template
		if in.Template != nil {
			if ok, why := in.Template.Offers(p.Name); !ok {
				reject("%s", why)
			}
			image = in.Template.ImageFor(p.Name)
		}
		if ok, why := pv.Capabilities.Accepts(sb.Resources, image); !ok {
			reject("%s", why)
		}
		cost := p.CostModel.HourlyCost(sb.Resources)
		ev.EstimatedCost = cost
		if eff.MaxCostPerHour > 0 && cost > eff.MaxCostPerHour {
			reject("estimated cost %.4f/h exceeds ceiling %.4f/h", cost, eff.MaxCostPerHour)
		}
		if policyOK && !ev.Eligible && len(missing) > 0 && len(ev.Reasons) == 1 {
			capabilityOnly++
		}
		if ev.Eligible {
			ev.HasCapacity = true
			switch {
			case !pv.Healthy:
				ev.HasCapacity = false
				ev.Reasons = append(ev.Reasons, "unhealthy: "+pv.HealthDetail)
			case p.MaxConcurrency > 0 && pv.Active >= p.MaxConcurrency:
				ev.HasCapacity = false
				ev.Reasons = append(ev.Reasons, fmt.Sprintf("at concurrency limit %d", p.MaxConcurrency))
			case !pv.Capacity.Fits(sb.Resources):
				ev.HasCapacity = false
				ev.Reasons = append(ev.Reasons, "insufficient free capacity")
			}
		}
		cands = append(cands, cand{view: ev, pv: pv})
	}

	privateReady := false
	for _, c := range cands {
		if c.view.Eligible && c.view.HasCapacity && c.view.Trust == api.TrustPrivate {
			privateReady = true
		}
	}
	var ready []cand
	for i := range cands {
		c := &cands[i]
		if !c.view.Eligible || !c.view.HasCapacity {
			continue
		}
		if c.view.Trust != api.TrustPrivate && eff.External == api.ExternalOverflow && privateReady {
			c.view.Reasons = append(c.view.Reasons, "held for overflow: a private pool can take this now")
			continue
		}
		ready = append(ready, *c)
	}

	freeRatio := func(pv PoolView) float64 {
		if !pv.Capacity.Bounded || pv.Capacity.TotalCPU == 0 {
			if pv.Pool.MaxConcurrency > 0 {
				return 1 - float64(pv.Active)/float64(pv.Pool.MaxConcurrency)
			}
			return 1
		}
		return float64(pv.Capacity.FreeCPU) / float64(pv.Capacity.TotalCPU)
	}
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		ap, bp := a.view.Trust == api.TrustPrivate, b.view.Trust == api.TrustPrivate
		switch eff.Strategy {
		case api.StrategyCost:
			if a.view.EstimatedCost != b.view.EstimatedCost {
				return a.view.EstimatedCost < b.view.EstimatedCost
			}
			if ap != bp {
				return ap
			}
		case api.StrategySpread:
			if fa, fb := freeRatio(a.pv), freeRatio(b.pv); fa != fb {
				return fa > fb
			}
			if ap != bp {
				return ap
			}
		default: // private-first
			if ap != bp {
				return ap
			}
			if a.view.EstimatedCost != b.view.EstimatedCost {
				return a.view.EstimatedCost < b.view.EstimatedCost
			}
			if fa, fb := freeRatio(a.pv), freeRatio(b.pv); fa != fb {
				return fa > fb
			}
		}
		return a.view.Pool < b.view.Pool
	})

	res := Result{Decision: dec}
	rank := map[string]int{}
	for i, c := range ready {
		res.Ranked = append(res.Ranked, c.view.Pool)
		rank[c.view.Pool] = i + 1
	}
	eligible := 0
	for _, c := range cands {
		c.view.Rank = rank[c.view.Pool]
		if c.view.Eligible {
			eligible++
		}
		dec.Candidates = append(dec.Candidates, c.view)
	}

	switch {
	case len(res.Ranked) > 0:
		dec.Outcome, dec.Pool = "placed", res.Ranked[0]
		dec.Reason = fmt.Sprintf("selected %s by %s strategy", res.Ranked[0], eff.Strategy)
	case eligible > 0:
		dec.Outcome = "waiting"
		dec.Reason = "eligible pools exist but none can take the sandbox now; it stays queued"
	default:
		dec.Outcome = "rejected"
		if capabilityOnly > 0 {
			dec.Reason = "no permitted pool supports the requested capabilities"
			res.Err = api.Errorf(api.CodeUnsupportedCapability, "%s", dec.Reason).With("capabilities", strings.Join(sb.Capabilities, ","))
		} else {
			dec.Reason = "no pool satisfies placement policy for this sandbox"
			res.Err = api.Errorf(api.CodeNoEligiblePool, "%s", dec.Reason).With("classification", sb.Classification)
		}
	}
	return res
}

// QueueExpired reports whether a waiting sandbox exceeded its queue budget.
func QueueExpired(in Input) bool {
	eff := Merge(in)
	return eff.MaxQueue > 0 && in.Now.Sub(in.Sandbox.CreatedAt) > eff.MaxQueue
}
