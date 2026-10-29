# Feature parity

What Differently does today, what comparable products do, and what each
Differently backend supports. Evidence date: **2026-10-28**. Competitor facts
come from their public documentation and repositories (sources linked in
[competitive-landscape.md](competitive-landscape.md) and below); they change
often, so re-check before relying on a cell.

**Status legend for Differently**

| Mark | Meaning |
|---|---|
| ✅ | Implemented and covered by automated tests (unit or in-process integration). |
| ✅ e2e | Also passes the [end-to-end suite](../test/e2e/README.md) against a real OpenSandbox server (Docker runtime). |
| 🧪 | Implemented against published documentation; verified only with fake servers, not live accounts or hardware. |
| 🟡 | Partially implemented; the gap is stated. |
| 📋 | Planned; tracked in [backlog.md](backlog.md). |
| — | Not planned, or not applicable to a self-hosted platform. |

## 1. Ownership and trust model

The deciding question for Differently's audience is who runs the control plane
and where workloads may go.

| | Differently | Boat | Daytona | Freestyle | E2B | OpenSandbox |
|---|---|---|---|---|---|---|
| License | Apache-2.0 | Proprietary | Archived AGPL-3.0 repo (v0.190.0); current core private | Proprietary (SDK MIT) | Apache-2.0 (runtime incl. control plane) | Apache-2.0 |
| Control plane you run, no vendor in the loop | ✅ | No | No maintained path | No | Single node officially ("Embed"); multi-node is Enterprise | Yes |
| BYOC (your compute, vendor control plane) | n/a — you own both | No | Yes ("custom regions" / runners) | No (colocation offered) | Enterprise | n/a |
| Air-gapped / no egress operation | 🟡 designed for it; documented in [deploy/air-gapped.md](deploy/air-gapped.md), not yet tested | No | No | No | UNVERIFIED | UNVERIFIED |
| Telemetry | None (no code path) | Vendor service | Vendor service | Vendor service | UNVERIFIED for self-host | UNVERIFIED |
| Policy-driven placement across own + external backends | ✅ | No | No | No | No | Chooses among its own backends only |
| Fail-closed external placement (explicit approval required) | ✅ | n/a | n/a | n/a | n/a | n/a |
| Data classification routing (e.g. `private-only`) | ✅ | No | No | No | No | No |
| Placement explanation per sandbox | ✅ `GET /v1/sandboxes/{id}/placement` | No | No | No | No | No |
| Trust report (endpoints, credentials by ref, external placements, limits) | ✅ `GET /v1/admin/trust-report` | No | No | No | No | No |
| Tamper-evident audit log | ✅ hash chain + verify endpoint + JSON-lines export | Audit/OTel export (UNVERIFIED scope) | UNVERIFIED | UNVERIFIED | UNVERIFIED | UNVERIFIED |
| Multi-tenant orgs/projects with quotas | ✅ | Orgs, seats, wallets | Orgs, tiers | Account plans | Teams | UNVERIFIED |
| Provider credentials never readable from the DB alone | ✅ env/file references, or stored sealed (AES-256-GCM) under a key that never enters the database ([ADR 0021](adr/0021-provider-credentials-stored-encrypted.md)) | n/a | n/a | n/a | n/a | n/a |

Honest positioning: a self-hosted, open-source sandbox runtime is **not**
unique (OpenSandbox, Tencent CubeSandbox and E2B's runtime exist), so
Differently no longer ships one. OpenSandbox is its default private runtime
([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)). The rows nobody
else fills are the policy, classification, placement-explanation and
trust-report rows: that is what Differently adds on top of the runtimes it
places work on.

## 2. Developer API: Differently vs Boat, endpoint by endpoint

Boat is the developer-experience reference. Boat paths are relative to
`https://boat.dev/api/v1` ([API reference](https://docs.boat.dev/api/v1.md)).
"Mapping" names the Differently endpoint that covers the same need.

### 2.1 Sandboxes

| Capability | Boat | Differently | Status | Notes |
|---|---|---|---|---|
| Create | `POST /sandboxes` (202, poll) | `POST /v1/sandboxes` (202) | ✅ | Differently admits against quota first and rejects impossible requests up front with `no_eligible_pool` / `unsupported_capability` (422). |
| Idempotent create | `Idempotency-Key` | `Idempotency-Key` | ✅ | Replay returns the live sandbox (200); once it has ended, the key creates a new one. SDKs send a key per call. |
| Batch create (up to 1000 from a snapshot) | `count` + `from` | — | 📋 | Loop in the SDK today. |
| Readiness | Poll `GET /sandboxes/{id}`; webhooks | `GET /v1/sandboxes/{id}/wait` long-poll; SDK `create()` waits by default | ✅ | Boat has no wait endpoint. |
| Get / list with labels, cursor | Yes | `GET /v1/sandboxes`, `?label=k=v`, `cursor`, `status` | ✅ | |
| Update (rename, TTL, subdomain) | `PATCH` | `PATCH /v1/sandboxes/{id}` (labels, TTL) | 🟡 | No rename or subdomain. |
| Delete | `DELETE` with confirm header, async | `DELETE /v1/sandboxes/{id}` (202, idempotent) | ✅ | No confirm header; SDK/CLI are explicit. |
| TTL / auto-stop | `ttlSeconds` (default 1 h, max 30 d) | `ttlSeconds`, and `idleTimeoutSeconds` (operator default `defaults.idleTimeoutSeconds`) | ✅ e2e | Expiry terminates. Idle auto-stop pauses (pools that keep memory) or stops (disk kept) a sandbox after its idle timeout with no exec, file, port or `keepalive` call, and the next such call wakes it: about 1.7 s on OpenSandbox's Kubernetes runtime, under 0.1 s on Docker. Traffic straight to a port URL is not seen; viewers call `keepalive`. |
| Sizes | 4 fixed types | Operator-defined profiles + explicit `resources` | ✅ | |
| Recover (power-cycle) | `POST /recover` | — | 📋 | Stop/start or pause/resume, where the backend has them. |
| Share with org members | `POST /share` | Project-scoped keys | — | Different tenancy model. |
| Per-sandbox usage | `GET /usage` | `GET /v1/usage?groupBy=…` | 🟡 | Aggregated by pool/project/org/trust, estimates only. |

### 2.2 Commands and processes

| Capability | Boat | Differently | Status | Notes |
|---|---|---|---|---|
| Synchronous exec | `POST /commands` (≤600 s) | `POST /v1/sandboxes/{id}/exec` | ✅ e2e | Shell string or argv (OpenSandbox receives argv as a quoted shell command); `timeoutSeconds` is enforced by the backend. |
| Streamed output | NDJSON | Server-sent events (`stdout`, `stderr`, `exit`, `error`) | ✅ e2e | |
| Per-command env | No | `env` on exec | ✅ e2e | |
| stdin | No | `stdin` on exec | ✅ e2e | OpenSandbox (both runtimes; staged in a file the command reads, since `execd` takes no stdin) and Freestyle (🧪); Daytona returns `unsupported_operation`. |
| Detached processes, attach by offset, kill | Yes | — | 📋 | Backlog: process sessions. |
| Command history | Yes | Audit log records each exec (program name by default) | 🟡 | Output is not retained by design. |
| PTY / interactive shell | SSH only | — | 📋 | SSH gateway / PTY over WebSocket in backlog. |
| Process list | Yes | — | 📋 | Achievable with exec today. |

### 2.3 Files

| Capability | Boat | Differently | Status |
|---|---|---|---|
| Read / write a file | `GET/PUT /files` (JSON, utf8/base64) | `GET/PUT /v1/sandboxes/{id}/files/content` (raw bytes, ≤100 MiB) | ✅ e2e |
| List a directory | `GET /files/list` | `GET /v1/sandboxes/{id}/files` | ✅ e2e |
| Delete, move, chmod, mkdir | Yes | — (use exec) | 📋 |
| Search / grep / replace | Yes | — (use exec) | 📋 |
| Signed upload/download URLs for large files | Yes | — | 📋 |
| Path restriction | `/home/user`, `/tmp` only | Whole sandbox filesystem | ✅ (by design) |

### 2.4 Access and networking

| Capability | Boat | Differently | Status | Notes |
|---|---|---|---|---|
| HTTPS port URLs | `POST /host` (token or public) | `POST /v1/sandboxes/{id}/ports` | ✅ e2e | OpenSandbox with `ports: signed`: signed, time-limited URLs through its ingress gateway (route mode `uri`, Secure Access), with no header or key. Checked over public DNS and TLS on Azure, including WebSocket upgrades (wss) with the path after the signed prefix and the Origin header passed through. Daytona (signed URL) and Freestyle (public `*.style.dev` host) 🧪. |
| SSH | `POST /sshkey` | — | 📋 | SSH gateway in backlog. |
| Desktop / browser stream | `POST /desktop` | — | 📋 (Phase 4) | |
| Egress policy | No API | Pool `egress: none`, `public` or `allowlist` | ✅ live (Kubernetes runtime) | OpenSandbox enforces it per sandbox with its egress sidecar (deny-all and a host allowlist checked live); Freestyle firewall 🧪. Per-request allowlists 📋. |
| Sandbox-to-node and sandbox-to-sandbox isolation | Platform-managed | Provided by the backend runtime; each pool declares its isolation and the trust report shows it | — | Differently does not enforce isolation itself ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)). |

### 2.5 Lifecycle, persistence and snapshots

| Capability | Boat | Differently | Status | Notes |
|---|---|---|---|---|
| Stop keeping disk / start | Stop then resume on a new machine | `POST …/stop`, `POST …/start` | ✅ e2e | OpenSandbox's Kubernetes runtime with a snapshot registry: the root filesystem is committed and the pod freed (stop about 3.7 s, start about 2.7 s on Azure). Daytona and Freestyle 🧪. OpenSandbox's Docker runtime has no stop. |
| Pause / resume (memory kept) | No | `POST …/pause`, `POST …/resume` | ✅ e2e | OpenSandbox's Docker runtime; Freestyle and Daytona VM class 🧪. OpenSandbox's Kubernetes runtime keeps no memory, so it offers stop/start instead. |
| Disk snapshot | Automatic every minute | `POST …/snapshots {kind: disk}` | ✅ e2e | OpenSandbox on both runtimes (Kubernetes: an OCI image in the snapshot registry, ready in about 7 s). |
| Memory snapshot | No | `kind: memory` | 🟡 | Freestyle (always memory+disk), Daytona VM class. OpenSandbox: not on the Docker or Kubernetes runtime (root filesystem only); Fast Sandbox and its QEMU VMState mode are documented to capture memory, unverified. |
| Fork | Disk copy | `POST …/fork` | ✅ e2e | Same pool as the source: snapshots are not portable. |
| Saved / named snapshots, restore by name | Yes | `fromSnapshot` on create; `GET /v1/snapshots` | ✅ | |
| Point-in-time restore | No | No | — | |
| Volumes / persistent disks independent of a sandbox | No | — | 📋 | |
| Cross-provider move of a running sandbox | No | No | — | Not promised; see [storage-and-lifecycle.md](storage-and-lifecycle.md). |

### 2.6 Platform and account

| Capability | Boat | Differently | Status |
|---|---|---|---|
| Scoped API keys | Yes | `POST /v1/api-keys`; scopes, project-bound, expiry; no-escalation rule; rotation with a grace period of up to 7 days (`POST /v1/api-keys/{id}/rotate`) | ✅ |
| Provider credentials managed in the product | n/a | `PUT /v1/admin/secrets/{name}`, used by pools as `secret:name`; replacing a value takes effect without a restart. Not yet tried against live Daytona or Freestyle accounts ([guide](guides/keys-and-providers.md)) | ✅ |
| Organizations / projects | Orgs, seats | Operator-managed orgs, tenant-managed projects | ✅ |
| Quotas (active, queued, CPU, memory, disk, TTL, snapshots) | Plan limits | Per org and project, enforced atomically | ✅ e2e |
| Fair queueing across tenants | n/a | Round-robin by least active CPU | ✅ |
| Suspend a tenant | n/a | `POST /v1/admin/organizations/{id}/suspend` (optionally terminate) | ✅ |
| Drain a pool | n/a | `POST /v1/admin/pools/{name}/drain` | ✅ |
| Switch which provider new sandboxes go to | n/a | `POST /v1/admin/pools/{name}/use` drains every other pool; running sandboxes stay; external pools need approval; every replica picks it up within about 5 s | ✅ |
| Webhooks | 6 lifecycle events, HMAC | — | 📋 |
| OpenTelemetry | Metrics + logs | Prometheus `/metrics`, structured JSON logs | 🟡 OTel traces 📋 |
| Integrated coding agents (prompt, steer, events) | Yes | — | — (out of scope: Differently is the substrate agents run on) |
| CI runners | GitHub Actions runners | — | — |
| Web console | Yes | Built-in dashboard (operator + tenant views, same API) | ✅ |
| CLI | `boat` (~80 commands) | `differently` | ✅ core commands |
| SDKs | Python, TypeScript | TypeScript, Python, Go | ✅ |
| OpenAPI spec | Public | [api/openapi.yaml](../api/openapi.yaml), served at `/openapi.yaml` | ✅ |

## 3. Backend capability matrix

What each Differently adapter advertises today (from the adapters' code). A
request that needs a capability is only placed on pools that advertise it; an
operation a backend lacks returns `unsupported_operation` (501).

| Capability | `opensandbox` (default private runtime) | `daytona` | `freestyle` | `remote` (plugin) | `devhost` (dev only) |
|---|---|---|---|---|---|
| Trust | **declared by the operator**: `private` when you run it | external | external | declared | private |
| Isolation | The OpenSandbox runtime's: containers (Docker, Kubernetes; shared host kernel) or Firecracker microVMs (Fast Sandbox); the pool declares which | Container, or VM with `class: linux-vm` | Hardware-virtualized microVM | Declared by the plugin | **None** |
| `exec` | ✅ e2e; stdin staged in a file | 🧪 combined stdout/stderr, no stdin | 🧪 separate streams, ≤5 min inline | As advertised | ✅ |
| `exec.stream` | ✅ e2e | — (falls back to buffered) | — (falls back to buffered) | As advertised | ✅ |
| `files` | ✅ e2e | 🧪 | 🧪 | As advertised | ✅ |
| `lifecycle.stop-start` / `persistence.disk` | ✅ e2e with `runtime: kubernetes` and a snapshot registry (root filesystem kept); not on Docker | 🧪 | 🧪 (stop = in-guest poweroff) | As advertised | ✅ |
| `lifecycle.pause-resume` | ✅ e2e (Docker runtime; memory kept); not on Kubernetes, which keeps no memory | 🧪 VM class only | 🧪 | As advertised | — |
| `snapshots.disk` / `fork` | ✅ e2e (Docker; Kubernetes with a snapshot registry) | 🧪 | 🧪 | As advertised | ✅ |
| `snapshots.memory` | 🧪 opt-in per pool (Fast Sandbox) | 🧪 VM class only | 🧪 always memory+disk | As advertised | — |
| `ports.expose` | ✅ e2e with `ports: signed` (ingress gateway, signed time-limited URLs, no header); off otherwise | 🧪 signed preview URL | 🧪 public TLS host | As advertised | — |
| `network.egress-control` | ✅ live with `egress: none` or `allowlist` (Kubernetes runtime) | — | 🧪 firewall (deny by default; `egress: public` to allow) | As advertised | — |
| Max size | Server and runtime limits | 4 vCPU / 8 GiB / 10 GiB (provider default) | 32 vCPU / 64 GiB / 256 GiB (plan-dependent) | Plugin limits | Host |
| Idempotent create | Metadata lookup | Label lookup | Slug derived from sandbox ID | Required by the contract (sandbox ID is the key) | Directory per sandbox ID |
| Provider-side TTL backstop | `timeout` (≥ 60 s) | `ttlMinutes` | `ttlSeconds` | `ttlSeconds` is passed; plugin's choice | n/a |

"e2e" for `opensandbox` means the [end-to-end suite](../test/e2e/README.md)
and the [live adapter check](../test/opensandbox/README.md) pass against a
real OpenSandbox 1.2.0 server with the Docker runtime. Its Kubernetes and Fast
Sandbox runtimes are not verified yet, so microVM isolation through
Differently is unverified.

The reconciler works through each adapter's `List` and the sandbox-ID label,
so it applies to every backend: orphans in the backend are deleted and
sandboxes that disappeared are failed with their quota released. The e2e
suite checks this against OpenSandbox. The reconciler's own tests use the
fake backend, and the Daytona, Freestyle and devhost `List` implementations
are unit-tested against fakes of their APIs.

Known unverified provider details (default exec timeouts, pause semantics,
rate limits) are listed in [proof-of-concepts.md](proof-of-concepts.md) for
live-account verification.

## 4. Remaining OpenSandbox gaps: limitation or not built yet

Against what Freestyle gives Workbot computers, as of 2026-10-29:

| Gap | Kind | Why, and what would close it |
|---|---|---|
| Pause that keeps memory, on the Kubernetes runtime | **Limitation** of OpenSandbox's Kubernetes runtime | Its pause commits the root filesystem and frees the pod; memory and processes are gone. Differently offers stop/start there. Fast Sandbox (Firecracker) and OpenSandbox's QEMU VMState mode are documented to keep memory: not built or run with Differently yet. |
| Memory snapshots | **Limitation** on Docker and Kubernetes; **not run** on Fast Sandbox | Same reason. Running Fast Sandbox needs KVM nodes ([E18](backlog.md#e18-fast-sandbox-spike)). |
| Idle auto-stop seeing port traffic | **Not built yet** | Differently sees exec, file and port calls, not requests sent straight to a port URL, so viewers call `keepalive`. OpenSandbox's gateway records that traffic in Redis (`gateway.activity`) and could feed Differently's idle clock. |
| Wake on a port request | **Limitation** for the Kubernetes and Docker runtimes in OpenSandbox 1.2.0 | OpenSandbox's gateway wake-on-access covers Fast Sandbox only. A stopped sandbox wakes on the next API call (or `keepalive`), not on a hit to its URL. |
| Per-request port URL lifetime | **Not built yet** | URLs last the pool's `portURLSeconds`; the ports API takes no `ttlSeconds`. |
| Egress on the Docker runtime | **Not checked** | The setting is sent on both runtimes; enforcement was checked on Kubernetes only. |
| Detached processes, PTY, SSH | **Not built yet** | OpenSandbox's execd offers background commands; not wired into the adapter. |

## 5. Boat-compatible facade: decision

**Not in the MVP.** Differently's own vendor-neutral API is the contract.
Reasons:

1. The portable core (sandbox CRUD, stop/resume/fork, exec sync and streamed,
   files, ports, snapshots, idempotency) maps cleanly onto Differently and is
   already available under its own paths.
2. Much of Boat's surface is product-specific (integrated agents, AI credential
   proxy, environments and repositories, CI runners, seats and wallets, desktop
   streaming) and would be expensive to mimic without adding value to a
   self-hosted substrate.
3. The license of Boat's published OpenAPI document is not stated; it must not
   be vendored into an Apache-2.0 project until that is clarified.

**If demand appears**, a facade would live at `/compat/boat/v1`, implement the
portable core only, and document every semantic difference. Mapping sketch:

| Boat | Differently | Semantic difference to document |
|---|---|---|
| `POST /sandboxes` (`type`, `ttlSeconds`, `labels`, `env`) | `POST /v1/sandboxes` (`profile`, `ttlSeconds`, `labels`, `env`) | Boat `type` → profile name; Differently may queue behind quota. |
| `GET /sandboxes/{id}` `state` | `status` | Boat `archived` ≈ `stopped`; Differently has `paused` and `queued`. |
| `POST /sandboxes/{id}/stop` / `resume` | `…/stop` / `…/start`, or `…/pause` / `…/resume` | Boat resumes on a new machine and IP; on Differently it depends on the backend (OpenSandbox has pause/resume only). |
| `POST /sandboxes/{id}/fork` | `…/fork` | Disk-based on OpenSandbox; memory-based on Freestyle. |
| `POST /sandboxes/{id}/commands` (`stream`) | `…/exec` (`?stream=1`) | NDJSON vs SSE framing; Boat has detached processes. |
| `GET/PUT /sandboxes/{id}/files` | `…/files/content` | JSON with encoding vs raw bytes; path restrictions differ. |
| `POST /sandboxes/{id}/host` | `…/ports` | Availability depends on the backend. |
| `X-Ascii-Confirm-Delete` | not required | Facade would accept and ignore it. |

## 6. Biggest gaps, in priority order

1. Run OpenSandbox's Kubernetes and Fast Sandbox runtimes under
   Differently on real infrastructure, so microVM isolation through
   Differently is verified. Verify the Daytona and Freestyle adapters
   against live accounts.
2. Wire OpenSandbox's network policy into placement, then per-sandbox egress
   allow-lists.
3. Port URLs on OpenSandbox pools and an SSH gateway.
4. Detached processes and PTY sessions.
5. Webhooks and OpenTelemetry traces.
6. Memory snapshots with restore through OpenSandbox (Fast Sandbox).
7. mTLS between the control plane and its backends.

Each has an epic with acceptance criteria in [backlog.md](backlog.md).
