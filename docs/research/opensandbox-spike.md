# OpenSandbox spike (2026-10-28)

Question: can [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
serve as Differently's sandbox/VM foundation, with Differently focused on
orchestration across providers (adapters, policy-gated placement, quotas,
audit, trust report)? This records what was read and what was run. It was the
main input to the decision to make OpenSandbox Differently's runtime.

Legend: **Observed** = run on our Colima setup today; **Read** = from the
published specs or source at release 1.2.0; **Unverified** = neither.

## Project facts (Read)

| | |
|---|---|
| License | Apache-2.0 |
| Latest release | `release-1.2.0` (2026-10-27); ~16k GitHub stars |
| Languages | Lifecycle server in Python (FastAPI on uvicorn); in-sandbox agent `execd`, ingress and other components in Go; SDKs in Python, Go, Kotlin, TypeScript, C# |
| Runtimes behind the server | Docker, Kubernetes, and Fast Sandbox (Firecracker microVMs managed by a per-node DaemonSet agent) |
| Contracts | `specs/sandbox-lifecycle.yml` (OpenAPI, version 0.1.0), `specs/execd-api.yaml` (1.0.0), plus diagnostics and egress specs; the specs directory asks maintainers to prefer additive changes; larger changes go through OSEPs (`oseps/`) |

## Auth and tenancy (Read)

- One server-wide API key (`server.api_key`, header `OPEN-SANDBOX-API-KEY`).
  If it is unset the server refuses to start unless insecure mode is
  acknowledged explicitly.
- The lifecycle spec has no organization, project or quota model; filtering
  is by free-form `metadata`.
- Data-plane requests to a sandbox go either directly to the sandbox (with
  per-sandbox headers returned by the endpoints API, such as
  `X-EXECD-ACCESS-TOKEN`) or through the server's proxy, authenticated with
  the same API key.

**Implication:** OpenSandbox is a single-trust-domain runtime. Tenancy,
quotas, scoped keys, placement policy and audit stay Differently's job, which
matches the intended split. The API key is a pool credential and must never
reach tenants or sandboxes; the adapter refuses to hand out port endpoints
that would need it.

## API shape (Read, then Observed)

| Need | OpenSandbox |
|---|---|
| Create | `POST /v1/sandboxes` with `image.uri` + `entrypoint`, or `snapshotId`, or `templateId`; `resourceLimits` as strings (`"cpu": "2"`, `"memory": "1024Mi"`); `timeout` ≥ 60 s; `env`; `metadata`. Returns 202 and `Pending`. |
| Readiness | Poll `GET /v1/sandboxes/{id}` until `status.state` is `Running`, then ping `execd` |
| Exec | `execd` `POST /command` with `command`, `cwd`, `envs`, `timeout` (ms); output is a stream of typed events; success is `execution_complete`, non-zero exit is an `error` event named `CommandExecError` with `exit_code` |
| Files | `execd` `GET /files/download`, `POST /files/upload` (multipart), `GET /directories/list` |
| Pause / resume | `POST /v1/sandboxes/{id}/pause` and `/resume` (async, 202) |
| Snapshots | `POST /v1/sandboxes/{id}/snapshots`, poll `GET /v1/snapshots/{id}` until `Ready`; restore with `snapshotId` on create |
| Ports | `GET /v1/sandboxes/{id}/endpoints/{port}` |
| Stop / start | None. There is pause/resume and delete only. |
| Stdin on exec | None in `POST /command` (interactive use goes through sessions and PTYs) |

## Running it on our Colima setup (Observed)

`test/opensandbox/compose.yaml` runs `opensandbox/server:release-1.2.0` with
the Docker runtime and `opensandbox/execd:v1.1.0`, on the same
`colima-differently` VM used for the Firecracker e2e suite.

- `TestLiveOpenSandbox` (build tag `live`) drives the real adapter against
  it: create (0.5–0.9 s to running, Docker runtime, arm64), idempotent create,
  exec with sandbox and per-command env, separate stderr, exit codes,
  argument lists, cwd, multi-line output, binary upload and download,
  listing, list by metadata, pause and resume with state kept, snapshot, and
  delete. It passes.
- Through Differently itself, with an `opensandbox` pool in the server
  config: the pool is healthy; private-first placement selects it; exec,
  pause and resume work through the CLI; `stop` is refused as unsupported;
  the trust report shows the endpoint and the credential as a reference
  only; delete cleans up in OpenSandbox; the API key never appears in the
  server log.

### Differences between the specs and the deployed images (Observed)

Each one would have broken a spec-only adapter. All are handled and covered
by the fake server in `opensandbox_test.go`.

1. `execd` v1.1.0 (the image the 1.2.0 server's example config deploys)
   rejects the spec's `argv` field and requires `command`. The adapter sends
   argument lists as a shell-quoted command.
2. Upload metadata must be a JSON **file part** named `metadata`, as the
   official SDKs send it; a plain form field is rejected (`INVALID_FILE_METADATA`).
3. Exec events arrive as newline-delimited JSON, not `data:`-prefixed SSE.
   The adapter accepts both.
4. `execd` v1.1.0 strips line terminators (one event per line); the 1.2
   spec says terminators are kept. The adapter restores a newline on any
   event without one, so a final fragment without a newline gains one.

**Implication for protocol stability:** the published spec runs ahead of
the images the server deploys by default. Pin the server and `execd`
versions per pool and run the live test on every upgrade.

## Fast Sandbox (Read, not attempted)

Fast Sandbox runs Firecracker microVMs through a per-node runtime agent (the
job Differently's own node agent did before the move to OpenSandbox), and adds memory-preserving pause and
resume, templates, and OCI-published snapshots. Its documented host
requirements: Linux ≥ 5.10, `/dev/kvm`, `/dev/net/tun`, cgroup v2, an XFS
filesystem with reflink for images, and an enlarged `/dev/shm`. Our Colima
VM has KVM (nested), tun and cgroup v2 but an ext4 root, so it needs an XFS
volume before Fast Sandbox can be tried there. **Unverified:** boot time,
density, memory snapshot restore, multi-node behaviour, and egress control
under Fast Sandbox.

## What this means

- **As a backend:** yes, now. The `opensandbox` adapter
  (`internal/adapters/opensandbox`) puts OpenSandbox behind Differently's
  contract like Daytona and Freestyle. The difference is that operators
  declare its trust, because it can be self-hosted.
- **As the foundation** (replacing Differently's own Firecracker node agent):
  decided ahead of the evidence below. OpenSandbox already solves per-node sandbox
  management with more lifecycle features. The evidence still owed:
  - Fast Sandbox on our supported KVM targets;
  - memory snapshot restore;
  - per-sandbox egress;
  - multi-node operation;
  - its operational footprint, including which services its features need.

  These were the adoption criteria; they are now follow-up verification,
  not a gate.

## Next spike steps

1. Run Fast Sandbox on the Colima VM with an XFS volume, and run the e2e
   suite (`test/e2e`, which already passes on the Docker runtime) against
   it.
2. Measure boot time and density on the same host; the removed node agent's
   figures (3.5–4.4 s create-to-running on arm64 under nested
   virtualization) are a reference point.
3. Check Kubernetes runtime behaviour and multi-node scheduling.
4. Review the Credential Vault and network-policy APIs as possible
   replacements for Differently's planned per-sandbox egress and secrets
   work.
