# ADR 0019: OpenSandbox as a backend first; foundation decision after the Fast Sandbox spike

- Status: Accepted (backend). Proposed (foundation); see criteria
- Date: 2026-10-28
- Related: [ADR 0002](0002-direct-firecracker-node-agent.md), [ADR 0007](0007-adapter-contract-optional-interfaces-http-protocol.md), [spike](../research/opensandbox-spike.md)

## Context

The project direction is for Differently to concentrate on orchestration and
resource management across providers (adapters, policy-gated placement,
quotas, audit, trust report) and to rely on OpenSandbox for running
sandboxes and VMs where possible. OpenSandbox is Apache-2.0, self-hostable,
and already manages Firecracker microVMs per node (Fast Sandbox), alongside
Docker and Kubernetes runtimes. Differently's own node agent
([ADR 0002](0002-direct-firecracker-node-agent.md)) covers the same ground with
fewer features.

The spike confirmed that OpenSandbox's lifecycle and `execd` APIs map onto
the adapter contract, and that it runs on our Colima setup with the Docker
runtime. It also found that the published spec runs ahead of the default
images. Fast Sandbox itself has not been run yet.

## Decision

1. **Now:** add an `opensandbox` execution adapter behind `pkg/adapter`, like
   Daytona and Freestyle. Placement, quota, audit and secret handling are
   unchanged:
   - external placement still fails closed;
   - the API key is a credential reference;
   - no credential reaches tenants, sandboxes or logs.

   Because OpenSandbox can run in or out of the operator's control, the pool
   must declare `trust` explicitly. Configuration without it is rejected.
2. **Keep** Differently's Firecracker node agent as the default private
   backend. ADR 0002 stays in force until the criteria below are met.
3. **Decide the foundation question** with a follow-up ADR that supersedes
   ADR 0002 if, on our supported KVM targets, OpenSandbox's Fast Sandbox:
   - passes the Firecracker e2e scenarios through the adapter (boot,
     isolation, stop or pause semantics, snapshots and fork, recovery);
   - restores memory snapshots, which Differently's agent does not offer yet;
   - offers per-sandbox egress control at least equal to the agent's;
   - runs multi-node with an operational footprint we can support in the
     Helm chart;
   - lets us pin server and `execd` versions and test upgrades.

   If it qualifies, the direct agent becomes optional and is deprecated
   over at least one release.

## Consequences

- Operators can use OpenSandbox today, private or external, through the
  same API and policies as every other pool.
- OpenSandbox has one API key per server and no tenant model, so
  Differently's tenancy, quotas and audit stay essential above it. One
  OpenSandbox server corresponds to one trust domain, that is, one pool.
- Capabilities differ from the Firecracker agent:
  - no stop/start, so no `lifecycle.stop-start` or `persistence.disk`;
  - no exec stdin;
  - snapshot kind and isolation depend on the runtime, which the pool
    declares through `config.isolation` and `config.features`.
- Version skew between spec and images is a standing risk. The live test
  (`go test -tags live`) has to run on every OpenSandbox upgrade.

## Alternatives considered

- **Replace the node agent with OpenSandbox now.** Rejected for the moment:
  Fast Sandbox is unverified on our targets, and the direct agent is the
  only backend proven end to end on real microVMs.
- **Only document OpenSandbox as a competitor.** Rejected: it is more useful
  as a backend, and the adapter costs little.
- **Wrap OpenSandbox's SDK instead of its HTTP API.** Rejected: the adapter
  contract is HTTP-shaped. Calling the API directly keeps the dependency set
  small and makes version skew visible in our own tests.
