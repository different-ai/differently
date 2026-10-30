# Maintaining and improving Differently

A working guide for coding agents and maintainers: where to change what, how
to prove a change at the right cost, how to keep the recorded runtime
behaviour current, and where to look when making Differently faster and
easier to self-host. [AGENTS.md](../AGENTS.md) holds the short rules; this
page holds the reasoning and the commands.

Differently is a control plane. It owns tenancy, keys, quotas, fair queueing,
placement policy, reconciliation, audit and the trust report. It runs no
sandboxes itself: OpenSandbox (the default private runtime), the other
[provider adapters](providers/README.md) and `remote` plugins do, behind the
adapter contract. Most bugs worth
fixing sit where the two meet: what a backend really returns, and what the
control plane does with it.

## Where to change what

| Change | Files | Tests to add or keep green |
|---|---|---|
| Public API (a route, a field, an error) | `pkg/api`, `internal/server`, `api/openapi.yaml`, the SDKs | `internal/server` tests (the OpenAPI test keeps the router and spec in sync); SDK tests |
| Placement, policy, classification | `internal/policy`, `internal/scheduler` | `internal/server/server_test.go` (acceptance scenarios A–L) |
| Slots, the resume queue, preemption, lifecycle limits | `internal/scheduler/capacity.go`, `internal/store` (`HoldsPoolSlot`, `ReclaimSlot`), the model in `internal/capsim` | The capacity tests named in [capacity.md](scheduling/capacity.md); store conformance through both stores; rerun `go run ./cmd/differently-capsim` when a strategy changes |
| Quotas, admission, activation | `internal/store` (memory and Postgres) | `internal/store/storetest` through both stores; `make test-postgres` |
| Reconciliation and recovery | `internal/scheduler` (`Reconcile`, `retry`) | Server tests using the fake backend; the e2e drift test |
| A backend's behaviour | `internal/adapters/<kind>` | Its fake-server tests; for OpenSandbox also the recordings ([below](#recordings-real-backends-replayed-in-go-test)) |
| A new provider kind | The adapter, then every registration list in [the provider catalog's checklist](providers/README.md#add-a-built-in-adapter) | `TestEveryAdapterKindHasATrustRule`; its fake-server tests |
| The adapter contract | `pkg/adapter`, `pkg/adapter/adapterhttp`, `internal/adapters/fake` | `adapterhttp` round-trip tests; [adapter specification](adapters/specification.md) |
| Stored credentials and keys | `internal/secrets`, `internal/server` | [keys-and-providers.md](guides/keys-and-providers.md) lists what is verified |
| Install and operations | `deploy/helm/differently` (values, schema, templates, NOTES), `cmd/differently-server` (`preflight`, `migrate`) | `make helm-lint`; the ci value sets; the deploy guides |
| The OpenWork integration | This repository's API and [docs/guides/openwork.md](guides/openwork.md); the client lives in OpenWork (`packages/sandbox-differently`) | OpenWork's provider tests against a running Differently |
| The built-in dashboard | `internal/dashboard/static` | Load it against `differently-server --dev` |
| The admin console | `console/` ([README](../console/README.md)); `src/lib/providers.ts` follows the dashboard's `kinds` | `npm run typecheck`, `npm run build` and `npm run build:server` (CI's `console` job); its pages against dev mode |
| The website | `site/src/pages/index.astro` (landing page), `site/pages-src/features.md`, the sidebar in `site/astro.config.mjs`; every other page syncs from `docs/**` and the READMEs | `npm run build` in `site/` validates every link |

Docs that must stay truthful with the code: [feature-parity.md](feature-parity.md),
[roadmap.md](roadmap.md), [acceptance-scenarios.md](acceptance-scenarios.md),
the [trust model](security/trust-model.md) and the `limitations()` the trust
report returns.

A verified result is stated in several places: the page that recorded it
(a deploy guide, [test/e2e](../test/e2e/README.md), a provider page), then
the README's status list, the roadmap, feature parity, the website's landing
page and its Features page. Update them in the same PR, so no page still
says "not yet" about something another one reports as run.

## Prove a change at the right cost

Climb only as far as the claim needs. Each rung is slower and more real than
the one before; repeating a rung for reassurance adds nothing.

| Rung | Command | Use it for |
|---|---|---|
| Package tests | `go test ./internal/adapters/opensandbox/` (or the package you changed) | Every change, while iterating |
| CI locally | `make verify` | Before every push: gofmt, vet, tests with `-race`, SDK tests, Helm lint |
| Postgres store | `make test-postgres` (needs Docker) | Changes in `internal/store` |
| Recorded runtimes | part of `go test` (`TestReplayRecordings`) | Any OpenSandbox adapter change: real responses from the Docker and Kubernetes runtimes, no server |
| Live adapter | `OPENSANDBOX_URL=… go test -tags live -run TestLiveOpenSandbox ./internal/adapters/opensandbox/`; for the managed providers `DAYTONA_API_KEY=… go test -tags live -run TestLiveDaytona ./internal/adapters/daytona/` and `FREESTYLE_API_KEY=… go test -tags live -run TestLiveFreestyle ./internal/adapters/freestyle/` (a few small sandboxes, all deleted) | A new OpenSandbox call or version; re-recording; any Daytona or Freestyle adapter change |
| Real stack, local | `make e2e-prepare e2e-up e2e` | Behaviour across the API, scheduler, store and a real OpenSandbox server |
| Real stack, a real cluster | the e2e suite pointed at an install ([test/e2e](../test/e2e/README.md#run-against-another-deployment)) | Helm, NetworkPolicy, the Kubernetes and Fast Sandbox runtimes, upgrades |

CI runs the first four on every PR. The live, e2e and cluster rungs need a
server and are run by hand; record what ran, where and with which versions in
[test/e2e/README.md](../test/e2e/README.md) and
[test/opensandbox/README.md](../test/opensandbox/README.md), and say in the
PR what did not run.

On macOS, run Docker stacks from a checkout under your home folder: Colima
shares only `$HOME` with its VM, so bind mounts from `/tmp` fail.

On a cluster, a Fast Sandbox pause or snapshot of a desktop takes minutes,
so the suite there takes much longer than the Docker stack. Point
`DIFFERENTLY_E2E_METRICS_URL` at the scheduler leader's pod rather than the
Service: with several replicas only the leader counts transitions.

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

The Daytona and Freestyle adapters have live tests but no recordings yet:
their fake servers were changed by hand to match what the live APIs did
([POC 7](proof-of-concepts.md#poc-7-external-provider-live-account-verification)).
Recording them the same way is
[E2.3](backlog.md#e2-external-adapters-on-live-accounts).

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
| Times inside a Fast Sandbox microVM are hours or days behind | The guest clock starts at the time its template or checkpoint was written | The adapter sets it after create and resume; keep that step when changing the adapter |
| A Fast Sandbox pause or snapshot times out | A 4 GiB desktop's checkpoint takes about three minutes, longer than the general ready timeout, and five or more once a burstable state-root disk runs out of credits | The pool's `checkpointTimeoutSeconds` (default 900); [shortening pauses](guides/opensandbox-pools.md#shortening-pauses) |
| Fast Sandbox template builds fail when they report their result | The build pods cannot reach the Kubernetes API (6443) or the source registry | Allow both in the sandbox network policy ([azure-vms.md](deploy/azure-vms.md)) |
| A client shows a date of 31 December, year 1 | The API sends Go's zero time (`0001-01-01T00:00:00Z`) for a time never set | Treat it as unset, as `isSet` in `console/src/lib/ui.ts` does |
| In `console/`, `npm run dev` answers 404 for `/v1/...` | Astro's `trailingSlash: 'always'` rejects paths without a slash before any Vite middleware runs | Leave `trailingSlash` unset in `console/astro.config.mjs` |
| A PR shows no CI run at all | GitHub runs no `pull_request` workflows while a PR has merge conflicts | Merge `pre-release` in, resolve, push |
| CodeQL reports `go/allocation-size-overflow` | `make([]byte, n+len(msg))` with an unbounded length | Check the size against a limit before allocating |
| `gh pr merge --auto` on a `pre-release` PR fails with "unstable status" | `pre-release` has no required checks, so there is nothing for auto-merge to wait for | Check the CI run, then merge |

## Releasing

[CONTRIBUTING.md](../CONTRIBUTING.md#releases) has the steps. In short:
feature PRs target `pre-release`; CI's `approve when CI passes` job gives the
approval `main` needs once every job is green; a release is a squash-merged
`pre-release` → `main` PR followed by a `vX.Y.Z` tag on `main`, which builds
the binaries, the Helm chart and the images (`ghcr.io/different-ai/differently-{server,cli}:X.Y.Z`);
`sync-pre-release` merges `main` back.

What the releases so far have shown:

- Alpha and RC tags are signed annotated tags on `pre-release`
  (`git tag -s vX.Y.Z-alpha.N -m vX.Y.Z-alpha.N <merge commit>`); the release
  workflow takes every version from the tag. `checksums.txt` covers the
  binary archives, not the chart.
- A release prep PR changes the CHANGELOG and the version strings in the
  docs and on the website (`site/src/pages/index.astro`). Merge it and push
  the tag one after the other, so the website never names a release that
  is not published yet.
- `main` follows `pre-release` through a PR titled
  `chore: merge pre-release into main`. Its auto-merge can stay `BLOCKED`
  with every check green and the CI approval present. Merge it with the
  GraphQL `mergePullRequest` mutation, pinned to the head CI checked
  (`expectedHeadOid`), then confirm in
  `GET /repos/different-ai/differently/rulesets/rule-suites?ref=refs/heads/main`
  that every rule passed. A rule that did not pass means the merge used a
  bypass: say so in the PR and fix the cause.
- A ruleset stops `pre-release` from being deleted when a release PR merges
  (the repository deletes merged head branches). If it is ever gone,
  recreate it at the release PR's head and rerun `sync-pre-release`.
