# Live check against a real OpenSandbox server

`compose.yaml` runs OpenSandbox's lifecycle server (release 1.2.0) with the
Docker runtime on any Docker host. `TestLiveOpenSandbox` then drives the
`opensandbox` adapter against it through the scenario in
`internal/adapters/opensandbox/recording_test.go`: create and idempotency,
listing, exec (exact output, argv, timeouts, streaming), files, pause and
resume, snapshot and fork, port endpoints, expiry, delete, and an image that
cannot be pulled. Each step checks the outcome expected for the runtime.

```bash
docker compose -f test/opensandbox/compose.yaml up -d
OPENSANDBOX_URL=http://localhost:8090/v1 go test -tags live -v -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
docker compose -f test/opensandbox/compose.yaml down
```

For a server with the Kubernetes runtime, set its `OPENSANDBOX_API_KEY` and
`OPENSANDBOX_RUNTIME=kubernetes`, or `kubernetes-registry` when the
controller has a snapshot registry. `OPENSANDBOX_PORTS=signed` (with
`OPENSANDBOX_PORT_SCHEME`) checks signed gateway URLs, fetching one for real,
and `OPENSANDBOX_EGRESS_ALLOW=pypi.org` checks an egress allowlist. For
Fast Sandbox set `OPENSANDBOX_RUNTIME=fastsandbox` with
`OPENSANDBOX_TEMPLATE`, `OPENSANDBOX_POOL_CPU` and `OPENSANDBOX_POOL_MEMORY`
(the SandboxPool's size), `OPENSANDBOX_PORT_SCHEME=https` when its gateway
is behind TLS, and `OPENSANDBOX_GATEWAY_HOST` (the gateway's host:port,
which the recording replaces like the server's address).

## Recordings, replayed without a server

Set `OPENSANDBOX_RECORD=<name>` and the run is saved to
`internal/adapters/opensandbox/testdata/recordings/<name>.json`: every
request path and every response body, with the API key and the server's
address removed. `TestReplayRecordings` serves those responses back from a
local listener and runs the same scenario on every `go test`, so the adapter
is checked against what real runtimes send, including how they fail, with no
server or cloud. Record again after an OpenSandbox upgrade:

```bash
OPENSANDBOX_URL=http://localhost:8090/v1 OPENSANDBOX_RECORD=docker-1.2.0 \
  OPENSANDBOX_HOST="Docker in Colima on Apple Silicon" \
  go test -tags live -count=1 -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
```

| Recording | Runtime | Recorded on |
|---|---|---|
| `docker-1.2.0` | Docker, `execd` release-1.2.0 | Colima 0.10.3, arm64, 2026-10-28 |
| `kubernetes-1.2.0` | Kubernetes (BatchSandbox), `execd` release-1.2.0, no snapshot registry | k3s v1.36.5 on Azure, x86_64, 2026-10-29 |
| `kubernetes-registry-1.2.0` | Kubernetes with a snapshot registry (`runtime: kubernetes`: stop/start, snapshots, fork), signed gateway port URLs and an egress allowlist | k3s v1.36.5 on Azure, x86_64, 2026-10-29 |
| `fastsandbox-1.2.0` | Fast Sandbox (Firecracker microVMs, template mode): pause and resume with memory, snapshot and fork, route-scope port URLs over HTTPS | k3s on Azure Standard_D8s_v6, nested KVM, 2026-10-29 |

The API key in `config.toml` is a test value. Sandboxes are Docker
containers on the same host, so this checks the API contract, not
isolation.

Verified 2026-10-28 on Colima (`colima-differently`, arm64) with server
1.2.0 and `execd` v1.1.0: passes; sandbox creation 0.5–0.9 s. Re-verified
2026-10-28 with `execd` release-1.2.0 and `lineEndings: kept`: byte-exact
output and `renew-expiration`.
