# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased] — on `pre-release`

### Fixed (Daytona and Freestyle, run live)

The `daytona` and `freestyle` adapters ran against live accounts for the
first time (`go test -tags live`, and a Differently server). Daytona's VM
class has not run.

- Freestyle: the env file was written with a mode Freestyle reads as
  decimal, and rewriting a file left it owned by root, so commands could not
  read the sandbox env; stop's `poweroff` ran as a user who may not power
  off; and a stopped VM was booted again by Freestyle's automatic restart.
- Daytona: stop and start returned while still in progress; a command
  timeout was reported as `provider_unavailable` (now exit code 124); a
  repeated delete, or one during a stop, failed with `invalid_state`;
  snapshot delete sent the name where Daytona needs the id; and a snapshot
  was returned before it could be used.

### Fixed (Helm chart)

- A pool under `config.pools` can use a provider key stored in Differently
  (`credentials: {secret: name}`), as the server already allowed; the
  values schema refused it.
- An external pool declared under `config.pools` now opens server egress to
  `providerCIDRs` on 443, like `providers.*` does.
- The `preflight` init container gets `server.extraVolumeMounts` too, so a
  `file:` credential on a mounted volume (such as a Key Vault CSI mount)
  resolves there as it does in the server.

## [0.1.0-alpha.2] — 2026-10-29

Second alpha, published as a GitHub pre-release. Not for production.
OpenSandbox pools come close to Freestyle on the Kubernetes runtime, Fast
Sandbox microVMs run live, idle sandboxes stop and wake, and 15 more
provider adapters join OpenSandbox. The managed-provider adapters are tested
only against fakes of each provider's API, and multi-node and managed
clusters are not verified yet (see the provider catalog and
`docs/roadmap.md`).

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

### Added (Fast Sandbox runtime)

- `runtime: fastsandbox` for OpenSandbox pools: Firecracker microVMs created
  from a Fast Sandbox template, pause/resume with memory kept, disk snapshots
  and fork. A sandbox's idle timeout becomes OpenSandbox's own idle policy
  (pause on idle, wake on access); the scheduler leaves those pools to it and
  the reconciler follows the backend's pauses and gateway wakes. New optional
  adapter interface `adapter.NativeIdler`.

### Fixed (Fast Sandbox, run live)

- `portScheme` now applies to every gateway endpoint OpenSandbox returns
  without a scheme, not only signed ones, so a Fast Sandbox gateway behind
  TLS in route mode `uri` gives `https://` port URLs and execd traffic.
  Before, those came back as `http://` on a TLS-only port.
- Fork on Fast Sandbox: a restore from a snapshot sends the pool's size
  (new pool config `poolCPU` and `poolMemory`), which OpenSandbox requires.
- Fast Sandbox ran live on one node (nested KVM): a `fastsandbox-1.2.0`
  recording replays in every `go test`, and the pools guide lists measured
  timings with Workbot's desktop and the gaps found, each marked as an
  OpenSandbox limitation or not built yet.

### Added (idle auto-stop)

- `idleTimeoutSeconds` on sandboxes (create and `PATCH`) and
  `defaults.idleTimeoutSeconds`: the scheduler leader pauses (pools that keep
  memory) or stops (disk kept) a sandbox with no exec, file, port or
  keepalive call for that long, and the next such call wakes it. A sandbox a
  caller stopped or paused is never woken this way. New
  `POST /v1/sandboxes/{id}/keepalive`, sandbox fields `lastActivityAt` and
  `idledAt`, SDK and CLI (`--idle-timeout`) support, and audit event
  `sandbox.idled` / `sandbox.woke`.

### Added (provider catalog)

- `docs/providers/`: 26 providers, each with a tested status (verified live,
  tested with a fake server, documented, or not supported), capabilities,
  configuration and exactly what was tested, and a guide to adding one.
- A new adapter kind can no longer become private by omission: only
  `remote`, `devhost` and `fake` default to private, and a test fails until
  every kind is in exactly one trust list.
- `vercel` (Vercel Sandbox) and `fly` (Fly.io Machines) adapters, always
  external; pages for Modal, Cloudflare and Sprites (documented: a `remote`
  plugin). Tested with fake servers, not live accounts.
- `boat`, `morph` (always external) and `e2b` (declared trust: it is
  self-hostable) adapters; a page for CodeSandbox (documented). Tested with
  fake servers, not live accounts.

### Added (cloud providers' own sandboxes)

- Adapters for the cloud providers' sandbox services, each tested against a
  fake server built from the provider's published API (not yet live):
  `aws-agentcore` (Amazon Bedrock AgentCore Code Interpreter),
  `azure-sandboxes` (Azure Container Apps Sandboxes), `azure-sessions`
  (Azure Container Apps dynamic sessions, Python code-interpreter pools),
  `gcp-sandboxes` (Google Cloud Agent Platform shell sandboxes) and
  `northflank`. Pools of these kinds must declare `trust` (external, or
  private if the operator's policy treats their own cloud account so).
- Cloud IAM credentials without SDKs: Microsoft Entra ID (client secret or
  workload identity federation), Google service-account keys, and AWS
  Signature Version 4, in `internal/adapters/cloudhttp`.
- Provider pages in `docs/providers/` for these, plus AgentCore Runtime and
  GKE Agent Sandbox (documented, no adapter).

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

### Added (Blaxel provider)

- `blaxel` adapter for Blaxel sandboxes (always `trust: external`): exec
  and streamed exec, files, stdin, token-protected port previews and a TTL
  backstop. Tested against a fake server built from Blaxel's published
  specs, not a live account.
- Provider pages for Blaxel, Runloop (not supported: the service is
  winding down), Beam and Koyeb (documented: a `remote` plugin on their
  SDKs) under `docs/providers/`.

### Added (end-to-end checks for microVM pools)

- `TestPauseKeepsMemory`, `TestIdlePauseAndWakeThroughPortURL`,
  `TestMemorySnapshot` and `TestDesktopStreams` (VNC over a port URL's
  WebSocket), each skipped where the pool lacks the feature. The suite runs
  against Fast Sandbox pools by template (`DIFFERENTLY_E2E_TEMPLATE`), checks
  a microVM guest's size instead of cgroup limits, and expects Fast Sandbox's
  precise refusals of sandbox env and of a template the pool does not serve.

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
- A sandbox whose create outlived the provision timeout stayed
  `provisioning` and was provisioned again on every scheduler tick: the retry
  ran on the expired context, so its PostgreSQL writes failed. Only the
  backend call is bounded by the timeout now.

### Security

- `devhost` refs must match the generated ID pattern; before, `..` reached
  the adapter root, so deleting it removed the root.
- Sandbox file reads from the API and the adapter protocol are served as
  downloads (`Content-Disposition: attachment`, `nosniff` and a sandbox CSP),
  so browsers never render sandbox content on the API origin.
- The SDKs trim trailing slashes from the base URL without a backtracking
  regular expression.

### Changed

- OpenSandbox pools no longer advertise `ports.expose` by default: their
  direct endpoints are runtime-internal addresses (`host.docker.internal`,
  pod IPs).
- The end-to-end suite adapts to the pool's capabilities, adds
  `TestUnpullableImageKeepsPoolHealthy`, and runs against any deployment
  (`OPENSANDBOX_E2E_INTERNAL_URL`, `DIFFERENTLY_E2E_POOL`).
- CI approves a pull request from this repository once every CI job passes,
  which satisfies `main`'s one-approval rule; forks are never approved and a
  new push dismisses the approval.
- Documentation: a maintainer guide (`docs/maintaining.md`), deployment
  guides for any cloud or bare metal with a verified single-VM recipe, the
  provider catalog, and a website and README that describe the project
  plainly. The internal architecture decision records were removed.

### Verified

- OpenSandbox's Docker runtime (arm64, Colima) and Kubernetes runtime
  (single-node k3s on Azure, installed with Helm): the end-to-end suite
  passes 14/14, including stop/start, snapshots and fork through a snapshot
  registry, signed port URLs (WebSockets included), egress policy, stdin, and
  idle auto-stop with wake.
- Fast Sandbox on one node (nested KVM on Azure), with OpenWork's Workbot
  desktop through Differently: create in 1.2 s, the desktop streamed over the
  HTTPS port URL, pause in about 3 minutes and resume in 0.9 s with memory
  kept, and an idle sandbox woken by a port request in 0.67 s. A partial
  end-to-end run passed files and pause/resume; fork failed on a 1 vCPU,
  1 GiB template (not yet diagnosed), and the port URL, idle-wake, memory and
  desktop checks did not run.
- The `agent-sandbox` adapter against agent-sandbox v1.0.8 on a kind cluster.
- OpenWork's Workbot computers through Differently as a sandbox gateway,
  behind a flag.
- Recorded runs of the Docker, Kubernetes (with and without a snapshot
  registry) and Fast Sandbox runtimes replay in every `go test`.
- Not verified: the managed-provider adapters against live accounts (each is
  tested against a fake of its provider's published API), multi-node clusters
  and node drains, and managed EKS, GKE and AKS.

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
- Provider credentials stored in Differently:
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
  private runtime.
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

[Unreleased]: https://github.com/different-ai/differently/compare/v0.1.0-alpha.2...pre-release
[0.1.0-alpha.2]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.2
[0.1.0-alpha.1]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.1
