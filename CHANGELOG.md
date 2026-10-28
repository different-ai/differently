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
