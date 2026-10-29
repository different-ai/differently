# Guidance for coding agents

This file is the entry point for automated contributors (and a quick primer
for humans).

## Where to work

- Branch from `origin/pre-release` as `<type>/<outcome>` and open the PR
  against **`pre-release`**: `gh pr create --base pre-release`. The
  repository's default branch is `main`, so the base must be explicit.
- Never push to or open feature PRs against `main`; releases reach `main` only
  through a release PR (see CONTRIBUTING.md).
- When CI is green you may merge your own PR into `pre-release`
  (`gh pr merge --squash`), or queue it with `--auto`. CI's `approve when CI
  passes` job approves same-repository PRs once every job is green; that
  approval is what `main`'s ruleset needs for release PRs.
- One focused outcome per PR, Conventional Commit title, `git commit -s`.

## Before you push

```bash
make verify
```

It runs gofmt, go vet, Go tests with `-race`, the SDK tests and the Helm lint,
including the replay of recorded OpenSandbox runtimes. For changes touching
`internal/store/postgres`, also run `make test-postgres`. For anything a real
backend or cluster could break, pick the cheapest rung of the verification
ladder in [docs/maintaining.md](docs/maintaining.md#prove-a-change-at-the-right-cost)
that covers it, and say in the PR what did not run.

## Map

- Contracts: `pkg/api` (wire types and error codes), `pkg/adapter`
  (backend contract), `api/openapi.yaml` (HTTP).
- Placement and security decisions: `internal/policy`, `internal/auth`,
  `internal/store` (atomic admission/activation). Treat changes here as
  security-sensitive and keep the tests in `internal/server/server_test.go`
  (acceptance scenarios A–L) green.
- Backends: `internal/adapters/*` (`opensandbox` is the default private
  runtime; Differently runs no sandboxes itself), `internal/wiring` (adapter
  factories), `internal/pools`. The real-stack suite is `test/e2e`
  (`make e2e-prepare e2e-up e2e`, or against any install).
- Recorded runtimes: `internal/adapters/opensandbox/testdata/recordings`,
  replayed by `TestReplayRecordings`; re-record with the live test after an
  OpenSandbox upgrade or a new adapter call.
- Stored credentials: `internal/secrets` (sealed values, `secret:` refs).
- How to change, prove, re-record, optimise and release:
  [docs/maintaining.md](docs/maintaining.md).
- Docs that must stay truthful: `docs/feature-parity.md`, `docs/roadmap.md`,
  `docs/acceptance-scenarios.md`.

## Rules of thumb

- Fail closed: never widen where work may run as a side effect.
- Never log secrets. Credentials are references (`env:`/`file:`) or values
  sealed by `internal/secrets` and referenced as `secret:`; never return a
  stored value through the API.
- Do not claim verification you did not do; mark gaps in docs.
- Prefer the standard library and existing patterns over new dependencies.
- Classify backend failures by what the backend said: one sandbox's failure
  must never mark a shared pool unhealthy.
