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
go test -tags e2e -count=1 -v ./test/e2e/
```

The server's config must match [`config.yaml`](config.yaml) where the tests
depend on it: the `small` and `medium` profiles, `audit.failClosed: true`
and a short `scheduler.reconcileIntervalSeconds`. Tests adapt to what the
pool advertises: where pause or snapshots are not offered (the Kubernetes
runtime without a snapshot registry), they check that Differently refuses
them precisely instead.

## What it checks

| Test | Proves |
|---|---|
| `TestSandboxRunsOnOpenSandboxWithLimits` | A sandbox placed on the pool runs in OpenSandbox, and its CPU and memory envelope reaches the runtime as cgroup limits |
| `TestExecSemantics` | Env (sandbox and per command), exit codes, separate stderr, timeouts and streamed output; stdin is refused with `unsupported_operation` |
| `TestFiles` | Binary file upload, download and listing |
| `TestPauseResumeAndUnsupportedStop` | Pause and resume keep state, or, if the pool does not advertise pause, it is refused with `unsupported_operation`; `stop` returns `unsupported_operation` because OpenSandbox has none |
| `TestSnapshotAndFork` | A fork starts from the parent's snapshot and is independent of it, or is refused precisely if the pool has no snapshots |
| `TestUnpullableImageKeepsPoolHealthy` | A sandbox whose image cannot be pulled fails or retries with a reason naming the image, never as a provider outage, and the pool stays healthy throughout |
| `TestCapabilityMismatchIsPrecise` | A request needing stop/start is rejected up front (422) with the missing capability named |
| `TestQuotaQueuesAndReleases` | A tenant at its limit queues; the queued sandbox starts when capacity frees |
| `TestDeleteCleansUpBackend` | Deleting a sandbox removes it from OpenSandbox |
| `TestReconcilesDriftInTheBackend` | An orphan carrying this installation's metadata is deleted; a sandbox deleted behind Differently's back is failed and its quota released |
| `TestAuditTrustAndMetrics` | Audit chain intact with fail-closed request records; the trust report shows the pool's isolation and no secrets; metrics only on their own port |

## Verified on

| Date | Host | OpenSandbox | Result |
|---|---|---|---|
| 2026-10-28 | Apple M4 Max, macOS 26.5, Colima 0.10.3 (`colima-differently`), arm64 | server 1.2.0, `execd` v1.1.0, Docker runtime | 10/10 passed, twice; sandboxes running in about 0.3–0.8 s |
| 2026-10-28 | same | server 1.2.0, `execd` release-1.2.0, `lineEndings: kept` | 10/10 passed, including byte-exact output |
| 2026-10-28 | same | same | 11/11 passed; an unpullable image fails after one attempt in about 1 s |
| 2026-10-28 | k3s v1.36.5 on an Azure `Standard_D8s_v6`, x86_64; Differently from its Helm chart with embedded PostgreSQL | server, controller and `execd` release-1.2.0, Kubernetes runtime, no snapshot registry | 11/11 passed (pause and snapshots checked as refusals); sandboxes running in about 1.4–2.3 s; an unpullable image retries after OpenSandbox's 60 s pod-ready timeout with the pool healthy |

Not covered yet:

- OpenSandbox's Fast Sandbox (Firecracker microVM) runtime;
- pause and snapshots on the Kubernetes runtime (they need OpenSandbox's
  snapshot registry), several nodes, and a node drain;
- external providers.
