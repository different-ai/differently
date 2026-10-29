# Maintaining and improving Differently

A working guide for coding agents and maintainers: where to change what, how
to prove a change at the right cost, how to keep the recorded runtime
behaviour current, and where to look when making Differently faster and
easier to self-host. [AGENTS.md](../AGENTS.md) holds the short rules; this
page holds the reasoning and the commands.

Differently is a control plane. It owns tenancy, keys, quotas, fair queueing,
placement policy, reconciliation, audit and the trust report. It runs no
sandboxes itself: OpenSandbox (the default private runtime), Daytona,
Freestyle and `remote` plugins do, behind the adapter contract. Most bugs worth
fixing sit where the two meet: what a backend really returns, and what the
control plane does with it.

## Where to change what

| Change | Files | Tests to add or keep green |
|---|---|---|
| Public API (a route, a field, an error) | `pkg/api`, `internal/server`, `api/openapi.yaml`, the SDKs | `internal/server` tests (the OpenAPI test keeps the router and spec in sync); SDK tests |
| Placement, policy, classification | `internal/policy`, `internal/scheduler` | `internal/server/server_test.go` (acceptance scenarios A–L) |
| Quotas, admission, activation | `internal/store` (memory and Postgres) | `internal/store/storetest` through both stores; `make test-postgres` |
| Reconciliation and recovery | `internal/scheduler` (`Reconcile`, `retry`) | Server tests using the fake backend; the e2e drift test |
| A backend's behaviour | `internal/adapters/<kind>` | Its fake-server tests; for OpenSandbox also the recordings ([below](#recordings-real-backends-replayed-in-go-test)) |
| The adapter contract | `pkg/adapter`, `pkg/adapter/adapterhttp`, `internal/adapters/fake` | `adapterhttp` round-trip tests; [adapter specification](adapters/specification.md) |
| Stored credentials and keys | `internal/secrets`, `internal/server` | [keys-and-providers.md](guides/keys-and-providers.md) lists what is verified |
| Install and operations | `deploy/helm/differently` (values, schema, templates, NOTES), `cmd/differently-server` (`preflight`, `migrate`) | `make helm-lint`; the ci value sets; the deploy guides |
| The OpenWork integration | This repository's API and [docs/guides/openwork.md](guides/openwork.md); the client lives in OpenWork (`packages/sandbox-differently`) | OpenWork's provider tests against a running Differently |

Docs that must stay truthful with the code: [feature-parity.md](feature-parity.md),
[roadmap.md](roadmap.md), [acceptance-scenarios.md](acceptance-scenarios.md),
the [trust model](security/trust-model.md) and the `limitations()` the trust
report returns.

## Prove a change at the right cost

Climb only as far as the claim needs. Each rung is slower and more real than
the one before; repeating a rung for reassurance adds nothing.

| Rung | Command | Use it for |
|---|---|---|
| Package tests | `go test ./internal/adapters/opensandbox/` (or the package you changed) | Every change, while iterating |
| CI locally | `make verify` | Before every push: gofmt, vet, tests with `-race`, SDK tests, Helm lint |
| Postgres store | `make test-postgres` (needs Docker) | Changes in `internal/store` |
| Recorded runtimes | part of `go test` (`TestReplayRecordings`) | Any OpenSandbox adapter change: real responses from the Docker and Kubernetes runtimes, no server |
| Live adapter | `OPENSANDBOX_URL=… go test -tags live -run TestLiveOpenSandbox ./internal/adapters/opensandbox/` | A new OpenSandbox call or version; re-recording |
| Real stack, local | `make e2e-prepare e2e-up e2e` | Behaviour across the API, scheduler, store and a real OpenSandbox server |
| Real stack, a real cluster | the e2e suite pointed at an install ([test/e2e](../test/e2e/README.md#run-against-another-deployment)) | Helm, NetworkPolicy, the Kubernetes runtime, upgrades |

CI runs the first four on every PR. The live, e2e and cluster rungs need a
server and are run by hand; record what ran, where and with which versions in
[test/e2e/README.md](../test/e2e/README.md) and
[test/opensandbox/README.md](../test/opensandbox/README.md), and say in the
PR what did not run.

On macOS, run Docker stacks from a checkout under your home folder: Colima
shares only `$HOME` with its VM, so bind mounts from `/tmp` fail.

## Recordings: real backends, replayed in go test

`internal/adapters/opensandbox/testdata/recordings/*.json` are real
exchanges between the adapter and OpenSandbox, one file per runtime.
`TestReplayRecordings` serves them back and runs the shared `scenario` in
`recording_test.go`, which asserts what Differently should do with each
response, including failures (an image that cannot be pulled, a pause the
Kubernetes runtime never starts, a sandbox being deleted).

- **Re-record** after an OpenSandbox upgrade, when the adapter makes a new
  call, or when a runtime's behaviour is in doubt: run the live test with
  `OPENSANDBOX_RECORD=<runtime>-<version>` (and `OPENSANDBOX_RUNTIME=kubernetes`
  for that runtime). Commands are in
  [test/opensandbox/README.md](../test/opensandbox/README.md).
- **Before committing a recording**, check that it holds no API key or token
  (the recorder replaces the key and the server address, but look), that it
  passes the replay, and that its size stays modest (tens of kilobytes).
- **Add a scenario step** when you fix a behaviour a real backend showed:
  give it a `step(...)` name, run it live on each runtime, and assert the
  per-runtime outcome. Then revert your fix and check that the replay fails;
  a replay that still passes without the fix does not protect it.
- **Replay matching** is per step, method and path; within a step a repeated
  request gets the next recorded response, then the last one again, so
  polling counts may differ from the recording. Fixed IDs
  (`sbx_rec_main`, `inst_rec`) keep paths stable.

The same pattern fits the Daytona and Freestyle adapters once someone has
accounts to record against
([POC 7](proof-of-concepts.md#poc-7-external-provider-live-account-verification)).

## Changing the adapter contract

The contract is what lets operators swap backends without application
changes, so change it deliberately:

- Error codes drive the scheduler: `insufficient_capacity` requeues without
  counting an attempt, `sandbox_failed` retries without marking the pool
  unhealthy, `invalid_request` fails at once, `provider_unavailable` marks the
  pool unhealthy. A wrong code moves other tenants' work, so classify by what
  the backend actually said (its status and its own error code, kept in
  `Details["providerCode"]` by `internal/adapters/httpx`). The table is in the
  [adapter specification](adapters/specification.md#5-error-codes).
- A new optional interface needs: the interface in `pkg/adapter`, a feature
  name, the fake, both sides of `adapterhttp`, the specification, and a
  round-trip test.
- Features are advertised only when they work on the pool's runtime. If a
  runtime accepts an operation it cannot perform, detect it and return
  `unsupported_operation` quickly rather than timing out.

## Making it faster and easier to self-host

Measure before optimising. The code-reading hot spots are in
[reliability-and-performance.md §3](reliability-and-performance.md#3-hot-spots-found-by-reading-the-code)
and the benchmarks to run in §4. Metrics are on the separate metrics port
(`metrics.listen`); start there, then reproduce with the e2e stack before
changing code.

Candidates, roughly by expected payoff for a self-hosted install:

1. **Quota tally** reads every live sandbox of an organization per
   admission ([E5.4](backlog.md#e5-postgres-in-ci-failover-and-scale)). An aggregate
   kept in the same transaction removes the linear cost.
2. **Provisioning concurrency** is unbounded: one goroutine per
   provisioning sandbox. A per-pool limit protects OpenSandbox and providers
   from bursts.
3. **OpenSandbox create** lists by metadata, creates, then polls state and
   `execd`; on the Kubernetes runtime the server itself blocks up to 60 s for
   the pod. Fewer round trips and an earlier `execd` check shorten time to
   running.
4. **Pool refresh** probes every pool from every replica every 5 s, and the
   OpenSandbox and Daytona probes list sandboxes. A cheaper probe, or probing
   from the leader only, scales better with replicas.
5. **Synchronous exec output** is unbounded in memory
   ([E0.35](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
6. **Reconcile cost** grows with live sandboxes per pool (paged lists every
   interval).

For each: write the benchmark or the failing test first, keep behaviour and
audit records identical, and record the before and after numbers in the PR.

Self-hosting checklist for any change an operator will feel:

- Helm: values, `values.schema.json` (the server's parser is strict), NOTES,
  and the ci value sets; defaults must work with OpenSandbox's own chart
  (namespace `opensandbox-system`, port 80).
- `differently-server preflight` catches the new misconfiguration with a
  clear message.
- Upgrades: forward-only migrations in `internal/store/postgres/migrations`;
  removed settings fail validation with a message that names the
  replacement.
- No new egress, telemetry or phone-home path; the
  [air-gapped guide](deploy/air-gapped.md) still holds.
- The deploy guides and the support matrix say what is verified and what is
  not.

## Traps already hit

| Symptom | Cause | Fix |
|---|---|---|
| A bind mount fails under Colima | Colima shares only `$HOME` | Run stacks from a checkout under `$HOME` |
| Helm: "control characters are not allowed"; migrate: `._0001_init.sql` | macOS `tar` added AppleDouble `._*` files, which Helm reads and `go:embed` embeds | `COPYFILE_DISABLE=1 tar …`, or delete `._*` before building |
| `docker build` fails on `$BUILDPLATFORM` on Ubuntu | Ubuntu's `docker.io` has no BuildKit | Install `docker-buildx` |
| OpenSandbox's `base` chart will not install | The namespaces were created first | Let `base` create `opensandbox-system` and `opensandbox-dataplane` |
| OpenSandbox images pull slowly or not at all | Its charts default to a registry in mainland China | Use `docker.io/opensandbox/*:release-1.2.0` |
| Create answers 504 `POD_READY_TIMEOUT` | The pod was not ready within 60 s (slow first pull, or a missing image); OpenSandbox then deletes the sandbox | Pre-pull large images; Differently retries it as `sandbox_failed` |
| Pause hangs, snapshots fail `RegistryNotConfigured` | The Kubernetes runtime needs `controller.snapshot.registry` | Configure the registry, or leave both out of the pool's `features` |
| stderr arrives before earlier stdout | `execd` reads the two pipes separately | Compare each stream on its own |
| `docker pull ghcr.io/different-ai/...` says `denied` on one machine only | A stale `ghcr.io` login in `~/.docker/config.json` | `docker logout ghcr.io`; release images are public, tagged without a leading `v` |

## Releasing

[CONTRIBUTING.md](../CONTRIBUTING.md#releases) has the steps. In short:
feature PRs target `pre-release`; CI's `approve when CI passes` job gives the
approval `main` needs once every job is green; a release is a squash-merged
`pre-release` → `main` PR followed by a `vX.Y.Z` tag on `main`, which builds
the binaries, the Helm chart and the images (`ghcr.io/different-ai/differently-{server,cli}:X.Y.Z`);
`sync-pre-release` merges `main` back.
