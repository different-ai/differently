# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased] — on `pre-release`

### Added (self-hosted runtimes)

- `agent-sandbox` pools: Sandboxes from kubernetes-sigs/agent-sandbox, or
  SandboxClaims on a warm pool, through the Kubernetes API with no client-go
  dependency; exec and files over WebSocket `pods/exec` (stdin and
  streaming included); stop/start with a per-sandbox volume. Checked live on
  kind with agent-sandbox v1.0.8.
- `cubesandbox` pools: CubeSandbox's KVM microVMs through its E2B-compatible
  CubeAPI and envd: exec (streamed), files, pause/resume, memory snapshots
  and fork. Tested with a fake server; not run against CubeSandbox yet.
- Provider pages `docs/providers/agent-sandbox.md`, `cubesandbox.md` and
  `microsandbox.md` (not supported: no self-hosted server API since its
  rewrite).

### Added (idle auto-stop)

- `idleTimeoutSeconds` on sandboxes (create and `PATCH`) and
  `defaults.idleTimeoutSeconds`: the scheduler leader pauses (pools that keep
  memory) or stops (disk kept) a sandbox with no exec, file, port or
  keepalive call for that long, and the next such call wakes it. A sandbox a
  caller stopped or paused is never woken this way. New
  `POST /v1/sandboxes/{id}/keepalive`, sandbox fields `lastActivityAt` and
  `idledAt`, SDK and CLI (`--idle-timeout`) support, and audit event
  `sandbox.idled` / `sandbox.woke`.

### Added (OpenSandbox parity with Freestyle)

- `runtime: kubernetes` for OpenSandbox pools. With the controller's snapshot
  registry, the pool offers stop/start with the disk kept (OpenSandbox's
  pause commits the root filesystem and frees the pod), disk snapshots and
  fork, and refuses a memory pause it cannot keep.
- `ports: signed`: port URLs through OpenSandbox's ingress gateway (route
  mode `uri`, Secure Access), signed and time-limited, with no header or key.
- `egress: none` or `allowlist` (`egressAllow`), enforced by OpenSandbox's
  egress sidecar; the pool advertises `network.egress-control`.
- Exec stdin on OpenSandbox pools, staged in a file the command reads.
- Helm values `opensandbox.runtime`, `ports`, `portScheme`,
  `portURLSeconds`, `egress` and `egressAllow`; the e2e test `TestPortURL`;
  the recording `kubernetes-registry-1.2.0`.

### Fixed (found against real OpenSandbox runtimes)

- A sandbox whose image cannot be pulled no longer marks its pool unhealthy
  for every other tenant (which could move their work to another pool or
  external provider). The Docker runtime's failed pull fails the sandbox
  after one attempt; the Kubernetes runtime's pod-ready timeout is retried
  as this sandbox's failure (new adapter error `sandbox_failed`), not a
  provider outage.
- Pause on a runtime that accepts it but never starts it (the Kubernetes
  runtime without a snapshot registry) returns `unsupported_operation` after
  30 s instead of `provider_unavailable` after 3 minutes; snapshots without a
  registry say so instead of reporting an outage.
- A sandbox being deleted on the Kubernetes runtime is treated as gone
  instead of as pending.
- The Helm chart's default `opensandbox.apiURL` and NetworkPolicy now match
  OpenSandbox's own chart (namespace `opensandbox-system`, port 80).

### Changed

- OpenSandbox pools no longer advertise `ports.expose` by default: their
  direct endpoints are runtime-internal addresses (`host.docker.internal`,
  pod IPs).
- The end-to-end suite adapts to the pool's capabilities, adds
  `TestUnpullableImageKeepsPoolHealthy`, and runs against any deployment
  (`OPENSANDBOX_E2E_INTERNAL_URL`, `DIFFERENTLY_E2E_POOL`).

### Verified

- OpenSandbox's Kubernetes runtime on single-node k3s (Azure, x86_64),
  installed with OpenSandbox's charts and Differently's Helm chart: the
  end-to-end suite passes 12/12 (pause and snapshots checked as refusals).
  The Docker runtime passes 12/12 too.
- Real runs of both runtimes are recorded under
  `internal/adapters/opensandbox/testdata/recordings` and replayed by
  `TestReplayRecordings` in every `go test`.

## [0.1.0-alpha.1] — 2026-10-28

First alpha, published as a GitHub pre-release for early testing. Not for
production: OpenSandbox's Kubernetes and Fast Sandbox runtimes, real
clusters and live Daytona/Freestyle accounts are not verified yet (see
`docs/roadmap.md`). The end-to-end suite covers the real stack against an
OpenSandbox server on its Docker runtime.

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
- Provider credentials stored in Differently
  ([ADR 0021](docs/adr/0021-provider-credentials-stored-encrypted.md)):
  - `PUT /v1/admin/secrets/{name}` stores a Daytona, Freestyle, OpenSandbox
    or plugin key sealed in Postgres (migration `0004_secrets.sql`);
    `GET /v1/admin/secrets` lists and `DELETE /v1/admin/secrets/{name}`
    removes. Listings show fingerprints and the pools using each
    secret, never values. A secret in use cannot be deleted;
  - pools reference a stored key as `credentials: {secret: name}`, and
    `PATCH /v1/admin/pools/{name}` can move an API-registered pool to another
    `credentialRef`;
  - replacing a stored value, or the file behind a `file:` reference, takes
    effect within about 5 seconds on every replica, without a restart.
    `env:` still needs a restart;
  - config `secrets.keyEnv` (default `DIFFERENTLY_SECRETS_KEY`) and
    `secrets.keyFile`. Without a key, storing fails with `invalid_state`.
- `POST /v1/admin/pools/{name}/use` sends new sandboxes to one pool by
  draining the others; running sandboxes stay. An external pool needs the
  global policy's approval, or `approveExternal`.
- `POST /v1/api-keys/{id}/rotate` replaces a key with a grace period of up to
  7 days, and `GET /v1/api-keys/{id}` reads one key.
- The surfaces for these:
  - CLI: `admin secret put|list|delete`, `admin pool add` (with
    `--api-key-stdin` or `--secret`), `admin pool use`, `key rotate`,
    `key get`, and `key list` as a table;
  - the dashboard's Add provider, Use for new sandboxes, Secrets and Keys
    views;
  - rotation and secrets in the Go, TypeScript and Python SDKs.
- Helm:
  - `secrets.enabled`, `secrets.existingSecret` and `secrets.existingSecretKey`.
    By default the chart generates the key once, in
    `<fullname>-secrets-key`, and keeps it across upgrades and uninstalls;
  - `networkPolicy.server.egress.runtimeProviders` for providers added at
    runtime.
- Guide: [keys and providers](docs/guides/keys-and-providers.md).

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

### Security

- `audit.failClosed` refuses state-changing requests that cannot be audited;
  `differently_audit_append_failures_total` counts audit failures.
- `metrics.listen` serves `/metrics` on its own port; the Helm chart does this
  by default (port 9091), keeping metrics off the ingress.
- Stored provider credentials:
  - Values are sealed with AES-256-GCM, with the secret's name bound in,
    under a 32-byte key-encryption key that never enters the database. A
    database dump alone reveals no stored credential.
  - The key is now critical. Losing it makes every stored credential
    unreadable, and the key plus a database copy opens them all. Back it up
    apart from database backups. It cannot be rotated yet.
  - Storing, replacing and deleting are `admin`-only and audited
    (`secret.put` with the fingerprint, `secret.delete`).
- Rotating an API key requires being able to grant every scope it holds,
  and is audited as `key.rotate`.
- Known gaps, not yet fixed:
  - with more than one server replica, pool changes made through the API
    (`use`, drain, undrain, `PATCH`, new pools) reach the other replicas
    only after a restart (backlog E0.36);
  - the trust report does not list `secret.*` events among recent admin
    actions (E0.37).

### Fixed (found by the end-to-end suite)

- `start` refused every request after the first one: the lifecycle handler
  reused state between requests.
- The reconciler could correct a sandbox in the middle of a change. It now
  waits 30 seconds after any change, and follows the backend's state: a
  sandbox that stopped behind the control plane's back is reported `stopped`.
- `differently sandbox list -o json` printed an object instead of an array
  for a single sandbox.

### Verified

- End-to-end suite against OpenSandbox 1.2.0 (Docker runtime) on arm64
  Colima: 10/10, twice. Not yet verified:
  - OpenSandbox's Kubernetes and Fast Sandbox runtimes;
  - x86_64 hosts and real clusters;
  - live Daytona and Freestyle accounts;
  - stored provider credentials, switching providers and API key rotation
    beyond unit and in-process API tests: no live provider account, real
    cluster or multi-replica run yet.

[Unreleased]: https://github.com/different-ai/differently/compare/v0.1.0-alpha.1...pre-release
[0.1.0-alpha.1]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.1
