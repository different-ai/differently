# End-to-end tests against a real OpenSandbox server

This suite runs the real stack in Docker:

- Postgres;
- `differently-server`, configured with `audit.failClosed` and a separate
  metrics listener;
- an OpenSandbox lifecycle server (release 1.2.0, Docker runtime) as its only
  pool, with `trust: private`.

The tests drive it through the public API with the Go SDK. Some also call
OpenSandbox directly to plant drift and to check what the backend really
did. Nothing is faked: every sandbox is a real OpenSandbox sandbox.

The Docker runtime runs sandboxes as containers that share the host kernel.
This suite therefore proves orchestration, not microVM isolation. That
includes the API, quotas, placement, lifecycle, reconciliation, audit and
the trust report.

## Requirements

- Any Docker host. On macOS, a separate Colima profile keeps your default
  one untouched:

  ```bash
  colima start differently --cpu 4 --memory 8 --disk 20
  export DOCKER_CONTEXT=colima-differently
  ```

- Go 1.26+ on the machine running the tests.
- About 1 GiB of disk for images.

## Run

```bash
make e2e-prepare   # build differently-server:e2e, pull execd and the sandbox image
make e2e-up        # Postgres, OpenSandbox (:18091), server (:18090), metrics (:19091)
make e2e           # go test -tags e2e ./test/e2e/
make e2e-down      # remove the stack and its volumes
```

OpenSandbox starts sandboxes as sibling containers through the mounted
Docker socket. Its configuration and test API key are in
[`../opensandbox/config.toml`](../opensandbox/config.toml). Every credential
in `compose.yaml` is a test value for this throwaway stack.

Colima shares only your home folder with its VM, so run the stack from a
checkout under `$HOME`; bind mounts from elsewhere (such as `/tmp`) fail.

## Run against another deployment

The suite only needs HTTP access to a Differently server, its metrics port
and the OpenSandbox server behind it, so it also runs against a real
install, such as a Helm release on Kubernetes reached through `kubectl
port-forward` or an SSH tunnel:

```bash
DIFFERENTLY_E2E_URL=http://localhost:28080 \
DIFFERENTLY_E2E_TOKEN="$(cat operator-token)" \
DIFFERENTLY_E2E_METRICS_URL=http://localhost:29091 \
OPENSANDBOX_E2E_URL=http://localhost:28091/v1 \
OPENSANDBOX_E2E_KEY="$(cat opensandbox-api-key)" \
OPENSANDBOX_E2E_INTERNAL_URL=http://opensandbox-server.opensandbox-system.svc/v1 \
go test -tags e2e -count=1 -v ./test/e2e/
```

`OPENSANDBOX_E2E_INTERNAL_URL` is OpenSandbox as the server reaches it, for
the pool `TestStoredProviderKeyAndSwitching` registers, and
`DIFFERENTLY_E2E_POOL` names the config's private pool (`opensandbox`). The
server's config must match [`config.yaml`](config.yaml) where the tests
depend on it: the `small` and `medium` profiles, `audit.failClosed: true`
and a short `scheduler.reconcileIntervalSeconds`. On a Fast Sandbox pool, sandboxes boot templates, not images:
`DIFFERENTLY_E2E_TEMPLATE` and `DIFFERENTLY_E2E_PYTHON_TEMPLATE` name them,
and the pool's `templateId` must be that template. Tests adapt to what the
pool advertises: where pause or snapshots are not offered (the Kubernetes
runtime without a snapshot registry), they check that Differently refuses
them precisely instead.

## What it checks

| Test | Proves |
|---|---|
| `TestSandboxRunsOnOpenSandboxWithLimits` | A sandbox placed on the pool runs in OpenSandbox, and its CPU and memory envelope reaches the runtime as cgroup limits |
| `TestExecSemantics` | Env (sandbox and per command), exit codes, separate stderr, timeouts, stdin and streamed output |
| `TestFiles` | Binary file upload, download and listing |
| `TestPauseResumeAndUnsupportedStop` | Where the pool offers stop/start (the Kubernetes runtime with a snapshot registry), stop and start keep the disk; otherwise stop is refused. Pause and resume keep memory where offered (the Docker runtime), and are refused precisely otherwise |
| `TestPortURL` | Where the pool offers port URLs (signed gateway URLs), a page served in the sandbox opens with the URL alone, which carries no key; otherwise the request is refused precisely |
| `TestSnapshotAndFork` | A fork starts from the parent's snapshot and is independent of it, or is refused precisely if the pool has no snapshots |
| `TestUnpullableImageKeepsPoolHealthy` | A sandbox whose image cannot be pulled fails or retries with a reason naming the image, never as a provider outage, and the pool stays healthy throughout |
| `TestIdleAutoStopAndWake` | A sandbox with a 60 s idle timeout is stopped (Kubernetes) or paused (Docker) after a minute unused, and the next exec wakes it with its files intact |
| `TestCapabilityMismatchIsPrecise` | A request needing a capability no pool has (`gpu`) is rejected up front (422) with the missing capability named |
| `TestQuotaQueuesAndReleases` | A tenant at its limit queues; the queued sandbox starts when capacity frees |
| `TestDeleteCleansUpBackend` | Deleting a sandbox removes it from OpenSandbox |
| `TestReconcilesDriftInTheBackend` | An orphan carrying this installation's metadata is deleted; a sandbox deleted behind Differently's back is failed and its quota released |
| `TestAuditTrustAndMetrics` | Audit chain intact with fail-closed request records; the trust report shows the pool's isolation and no secrets; metrics only on their own port |
| `TestStoredProviderKeyAndSwitching` | A provider key stored in Differently drives a second pool; `use` sends new sandboxes there and back; replacing the stored key takes the pool down and brings it back without a restart |
| `TestPauseKeepsMemory` | Where the pool pauses, a background process keeps its PID and 1 MiB in `/dev/shm` keeps its checksum across pause and resume |
| `TestIdlePauseAndWakeThroughPortURL` | On a microVM pool that pauses and exposes ports, an idle sandbox is paused, and asking for its port URL wakes it with the same server process |
| `TestMemorySnapshot` | A memory snapshot is taken where the pool advertises `snapshots.memory`, and refused precisely otherwise |
| `TestDesktopStreams` | With `DIFFERENTLY_E2E_DESKTOP_IMAGE` (the image in [`desktop/`](desktop/), or a Fast Sandbox template built from it), VNC over the port URL's WebSocket completes the RFB 3.8 handshake and returns a full frame, saved as a PNG; where the pool pauses, the same X server streams again after resume |

## Verified on

| Date | Host | OpenSandbox | Result |
|---|---|---|---|
| 2026-10-28 | Apple M4 Max, macOS 26.5, Colima 0.10.3 (`colima-differently`), arm64 | server 1.2.0, `execd` v1.1.0, Docker runtime | 10/10 passed, twice; sandboxes running in about 0.3–0.8 s |
| 2026-10-28 | same | server 1.2.0, `execd` release-1.2.0, `lineEndings: kept` | 10/10 passed, including byte-exact output |
| 2026-10-28 | same | server 1.2.0, `execd` release-1.2.0, `lineEndings: kept` | 11/11 passed, including stored provider keys and switching |
| 2026-10-28 | same | same | 12/12 passed (the combined suite); an unpullable image fails after one attempt in about 1 s |
| 2026-10-29 | same Azure k3s; snapshot registry (`registry:2` on the node), ingress gateway in route mode `uri` with Secure Access; pool `runtime: kubernetes`, `ports: signed` | same | 13/13 passed with stop/start (about 3.7 s / 2.7 s), snapshot and fork, signed port URLs and stdin all exercised. Signed URLs also checked over public TLS (Caddy in front of the gateway): wss upgrades to `/vnc/<token>` and `/screencast/<token>` reach the sandbox with path and Origin intact |
| 2026-10-29 | Apple M4 Max, Colima, arm64 | server 1.2.0, `execd` release-1.2.0, Docker runtime | 13/13 passed |
| 2026-10-29 | Azure k3s and Colima, as above | same | 14/14 on both; an idle sandbox is stopped (Kubernetes) or paused (Docker) after 60 s and woken by the next exec in about 1.7 s (Kubernetes) or under 0.1 s (Docker) |
| 2026-10-28 | k3s v1.36.5 on an Azure `Standard_D8s_v6`, x86_64; Differently from its Helm chart with embedded PostgreSQL | server, controller and `execd` release-1.2.0, Kubernetes runtime, no snapshot registry | 12/12 passed (pause and snapshots checked as refusals); sandboxes running in about 1.4–2.7 s; an unpullable image retries after OpenSandbox's 60 s pod-ready timeout with the pool healthy |
| 2026-10-29 | same Azure VM; pool `opensandbox-microvm` (`runtime: fastsandbox`), template `tpl-1cbcfa94` (1 vCPU, 1 GiB, guest kernel 6.18.36+) | server, Fast Sandbox and `execd` release-1.2.0 | **Partial run**, stopped when the VM was deallocated. Sandboxes running in about 1.3 s; files and pause/resume passed (pause about 52 s for 1 GiB); sandbox env and a template other than the pool's are refused precisely (the tests now expect that). Fork failed with a bare 422 from OpenSandbox on restore, not yet diagnosed. Port URL, idle wake, memory kept and the desktop stream did not run |

Not covered yet:

- the full suite on OpenSandbox's Fast Sandbox (Firecracker microVM) runtime (one partial run, above);
- several nodes and a node drain on the Kubernetes runtime;
- external providers.
