# ADR 0006: Filter-then-rank placement; external providers fail closed

- Status: Accepted
- Date: 2026-10-28

## Context

Placement mixes two kinds of input:

- **hard constraints**: data classification, whether work may leave private
  infrastructure, which pools and regions are allowed, required capabilities,
  resource limits, cost ceilings;
- **preferences**: private first, cheapest, or most free capacity.

If constraints and preferences are scored together, a large enough cost saving
can outweigh a residency rule. Constraints also come from several owners: the
operator (global policy and classifications), organization admins, project
admins and the request itself. A lower level must never be able to widen what a
higher level allows. Finally, registering a provider (for example to test
credentials) must not by itself make it a destination.

## Decision

`internal/policy/policy.go` implements placement as two stages:

1. **Filter.** Every pool is evaluated against every hard constraint and gets
   an explanation. Only pools that pass all of them are candidates.
2. **Rank.** Candidates with capacity are ordered by the strategy.

Merge rules (`Merge`):

- External mode: the global policy sets the ceiling, and **no global setting
  means `never`**. Organization, project, classification and request can only
  make it stricter (`never` < `overflow` < `allowed`). A classification with no
  external mode means `never`.
- Allowed pools and regions intersect across levels; an empty intersection
  allows nothing. Denied pools are the union.
- An external pool must also be named in `externalPools`; the lists intersect
  across levels and an unnamed pool is never approved.
- Strategy: the most specific non-empty value wins (global, organization,
  project, request).
- Cost ceiling and maximum queue time: the minimum across levels.

`overflow` holds external candidates back while any eligible private pool can
take the sandbox now. Outcomes are `placed`, `waiting` (eligible pools exist,
none has room) or `rejected` (`unsupported_capability` when only capabilities
rule pools out, `no_eligible_pool` otherwise). The full decision, including
every pool's verdict, is stored on the sandbox and served by
`GET /v1/sandboxes/{id}/placement`.

## Consequences

- Security and residency always win over cost and speed.
- Fail-closed: a fresh install places nothing externally
  (`internal/config/config.go` defaults the global policy to `never`), and
  registering a Daytona pool authorizes nothing (`TestUnapprovedExternalPoolIsNeverUsed`).
- Tenants can tighten placement for their own work with policies or the
  request's `placement`, but never loosen it.
- Requests are rejected early when nothing could ever take them, instead of
  queueing forever.
- Overflow is decided per sandbox per tick: if no private pool is *eligible*
  (for example none has a required capability), eligible external pools are
  used even when private pools have spare capacity.
- Ranking is simple and deterministic (ties break on pool name); there is no
  bin-packing or anti-affinity beyond the strategies.
- Pools that already failed a sandbox are meant to be excluded on retry
  (`Input.ExcludedPools`), but the scheduler never records them; retries rely
  on the pool being marked unhealthy until its next probe. Known issue,
  [backlog E0](../backlog.md#e0-correctness-and-security-fixes-from-the-code-review).

Details and worked examples: [placement-policy.md](../scheduling/placement-policy.md).

## Alternatives considered

- **Weighted scoring of all factors.** Rejected: makes residency negotiable.
- **Most specific policy wins outright.** Rejected: a project admin could widen
  an operator's restriction.
- **Implicit approval of registered external pools.** Rejected: registering a
  provider is an operational act, not an authorization.
- **Kueue-style cohorts and borrowing.** Considered for later batch work; not
  needed for interactive placement.
