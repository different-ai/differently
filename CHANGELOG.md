# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased] — on `pre-release`

### Added

- Control plane (`differently-server`):
  - v1 API, API keys with scopes, organizations and projects;
  - atomic quota admission and activation, fair queueing;
  - policy-driven placement with fail-closed external providers;
  - scheduler and reconciler;
  - tamper-evident audit chain, trust report, usage estimates;
  - Prometheus metrics, built-in dashboard;
  - memory and Postgres stores.
- Adapters:
  - `opensandbox`, the default private runtime: OpenSandbox servers as
    pools with operator-declared trust and isolation, covering create, exec
    and streamed exec, files, pause/resume, snapshots, fork and port
    endpoints;
  - Daytona, Freestyle;
  - `remote` (plugin protocol);
  - `devhost` (dev only).
- CLI (`differently`), Go, TypeScript and Python SDKs, OpenAPI 3.1 spec.
- Helm chart for the control plane, with an `opensandbox` block that
  registers an existing OpenSandbox server as the default pool; AWS
  Terraform module.
- End-to-end suite (`make e2e`): Postgres, the server and an OpenSandbox
  server in Docker, driven through the public API.
- Templates can map one name to each pool's own image (`templates[].images`),
  so a template routes to OpenSandbox, Daytona or Freestyle; placement only
  considers pools the template lists.
- `opensandbox` pools: `lineEndings: kept` gives byte-exact exec output with
  `execd` release-1.2.0 or later, and a TTL change moves OpenSandbox's own
  expiry (`renew-expiration`).
- [OpenWork on Differently](docs/guides/openwork.md): OpenWork's sandbox
  contract mapped onto this API.
- Documentation: architecture RFC, ADRs, security model, adapter spec,
  placement policy, feature parity, deployment guides, roadmap and backlog.

### Changed

- OpenSandbox replaces Differently's own Firecracker node agent as the
  private runtime ([ADR 0020](docs/adr/0020-opensandbox-replaces-node-agent.md)).
  The following were removed before any release:
  - `differently-agent`, `differently-guest` and the `firecracker` adapter;
  - agent registration, heartbeats and per-agent secrets;
  - the guest image scripts and systemd units.

  The server's config parser and the Helm schema reject the removed
  settings (`auth.agentTokenEnv`, `scheduler.agentTimeoutSeconds`,
  `agent.*`). Postgres migration `0002` drops the `agents` table.
- The trust report shows each pool's declared isolation and states that
  isolation and egress come from the backend runtime, not from Differently.

- An `Idempotency-Key` names a live sandbox: once that sandbox is terminated
  or failed, the same key creates a new one (Postgres migration `0003`).

### Fixed (found against real OpenSandbox runtimes)

- A sandbox whose image cannot be pulled no longer marks its pool unhealthy
  for every other tenant (which could move their work to another pool or
  external provider). The Docker runtime's failed pull fails the sandbox
  after one attempt; the Kubernetes runtime's pod-ready timeout is retried
  as this sandbox's failure, not a provider outage.
- Pause on a runtime that accepts it but never starts it (the Kubernetes
  runtime without a snapshot registry) returns `unsupported_operation` after
  30 s instead of `provider_unavailable` after 3 minutes; snapshots without a
  registry say so instead of reporting an outage.
- A sandbox being deleted on the Kubernetes runtime is treated as gone
  instead of as pending.
- The Helm chart's default `opensandbox.apiURL` and NetworkPolicy now match
  OpenSandbox's own chart (namespace `opensandbox-system`, port 80).

### Changed (OpenSandbox pools)

- `ports.expose` is no longer advertised by default: OpenSandbox's direct
  endpoints are runtime-internal addresses.

### Security

- `audit.failClosed` refuses state-changing requests that cannot be audited;
  `differently_audit_append_failures_total` counts audit failures.
- `metrics.listen` serves `/metrics` on its own port; the Helm chart does this
  by default (port 9091), keeping metrics off the ingress.

### Fixed (found by the end-to-end suite)

- `start` refused every request after the first one: the lifecycle handler
  reused state between requests.
- The reconciler could correct a sandbox in the middle of a change. It now
  waits 30 seconds after any change, and follows the backend's state: a
  sandbox that stopped behind the control plane's back is reported `stopped`.
- `differently sandbox list -o json` printed an object instead of an array
  for a single sandbox.

### Verified

- OpenSandbox's Kubernetes runtime on single-node k3s (Azure, x86_64),
  installed with OpenSandbox's charts and Differently's Helm chart: the
  end-to-end suite passes 11/11. Real runs of both runtimes are recorded
  and replayed in `go test` (`internal/adapters/opensandbox/testdata`).
- End-to-end suite against OpenSandbox 1.2.0 (Docker runtime) on arm64
  Colima: 11/11. Not yet verified:
  - OpenSandbox's Fast Sandbox runtime, and pause and snapshots on the
    Kubernetes runtime (they need a snapshot registry);
  - multi-node clusters;
  - live Daytona and Freestyle accounts.
