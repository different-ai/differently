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

## What it checks

| Test | Proves |
|---|---|
| `TestSandboxRunsOnOpenSandboxWithLimits` | A sandbox placed on the pool runs in OpenSandbox, and its CPU and memory envelope reaches the runtime as cgroup limits |
| `TestExecSemantics` | Env (sandbox and per command), exit codes, separate stderr, timeouts and streamed output; stdin is refused with `unsupported_operation` |
| `TestFiles` | Binary file upload, download and listing |
| `TestPauseResumeAndUnsupportedStop` | Pause and resume keep state; `stop` returns `unsupported_operation` because OpenSandbox has none |
| `TestSnapshotAndFork` | A fork starts from the parent's snapshot and is independent of it |
| `TestCapabilityMismatchIsPrecise` | A request needing stop/start is rejected up front (422) with the missing capability named |
| `TestQuotaQueuesAndReleases` | A tenant at its limit queues; the queued sandbox starts when capacity frees |
| `TestDeleteCleansUpBackend` | Deleting a sandbox removes it from OpenSandbox |
| `TestReconcilesDriftInTheBackend` | An orphan carrying this installation's metadata is deleted; a sandbox deleted behind Differently's back is failed and its quota released |
| `TestAuditTrustAndMetrics` | Audit chain intact with fail-closed request records; the trust report shows the pool's isolation and no secrets; metrics only on their own port |

## Verified on

| Date | Host | OpenSandbox | Result |
|---|---|---|---|
| 2026-10-28 | Apple M4 Max, macOS 26.5, Colima 0.10.3 (`colima-differently`), arm64 | server 1.2.0, `execd` v1.1.0, Docker runtime | 10/10 passed, twice; sandboxes running in about 0.3–0.8 s |

Not covered yet:

- OpenSandbox's Kubernetes and Fast Sandbox (Firecracker microVM) runtimes;
- x86_64 hosts;
- a Helm install on a real cluster;
- external providers.
