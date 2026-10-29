# ADR 0020: OpenSandbox replaces Differently's Firecracker node agent

- Status: Accepted
- Date: 2026-10-28
- Supersedes: [ADR 0002](0002-direct-firecracker-node-agent.md); makes
  [ADR 0014](0014-defer-memory-snapshots-until-restore-proven.md),
  [ADR 0015](0015-defer-jailer-integration.md) and
  [ADR 0018](0018-per-agent-secrets-until-mtls.md) obsolete
- Updates: [ADR 0019](0019-opensandbox-backend-first.md) (the foundation question is decided here)

## Context

Differently's value is orchestration and resource management across
providers:

- one API, tenancy and scoped keys;
- quotas and fair queueing;
- policy-gated placement with fail-closed external providers;
- adapters;
- the audit chain, placement explanations and the trust report.

Its own Firecracker node agent (ADR 0002) duplicated what OpenSandbox
already provides and maintains:

- per-node sandbox management;
- Docker, Kubernetes and Firecracker (Fast Sandbox) runtimes;
- memory-preserving pause/resume, templates and snapshots.

ADR 0019 added OpenSandbox as a backend and proposed criteria for making it
the foundation. The maintainer has decided to make OpenSandbox the runtime
now, so that engineering goes into Differently's own deliverables rather
than a second sandbox runtime.

## Decision

- Remove `differently-agent`, `differently-guest`, the `firecracker` pool
  adapter, agent registration and heartbeats, per-agent secrets, the guest
  image build scripts and systemd units.
- OpenSandbox (`opensandbox` adapter) is the default private runtime. A
  private installation runs an OpenSandbox server, using whichever runtime
  fits: Docker, Kubernetes or Fast Sandbox. It registers that server as a
  pool with `trust: private`.
- Daytona, Freestyle and `remote` plugins remain alternative backends under
  the same policies.
- The Helm chart deploys the control plane only. OpenSandbox is installed
  with its own tooling and referenced by URL and API key.

## Consequences

- **Smaller scope:** Differently no longer needs privileged pods, KVM nodes,
  per-node networking or guest images. Those requirements now belong to
  whichever OpenSandbox runtime the operator picks (Fast Sandbox needs KVM,
  among others).
- **Isolation is the runtime's:** the trust report states each pool's
  declared isolation and says that Differently does not enforce isolation
  or egress itself.
- **Accepted ahead of ADR 0019's criteria:** Fast Sandbox has not yet been
  run on our targets. Until it has, microVM isolation through Differently is
  unverified. The Docker runtime is verified end to end, but containers share
  the host kernel.
- **Capability changes:**
  - no stop/start (OpenSandbox has pause/resume and delete);
  - no exec stdin;
  - memory snapshots depend on the runtime;
  - no node-level egress rules from Differently; OpenSandbox's network
    policy is not wired up yet.
- **E2E suite:** it runs the real control plane against a real OpenSandbox
  server.
- **Lost agent work:** the agent-specific fixes and the per-agent identity
  work leave with the agent; their history stays in git and PRs #1 and #3.

## Alternatives considered

- **Keep both runtimes.** Rejected: two runtimes to maintain, test and
  secure, for capability OpenSandbox already has.
- **Wait for the Fast Sandbox spike before deciding.** Rejected by the
  maintainer in favour of focusing now; the spike is still the first
  follow-up.
