# ADR 0012: Pre-release integration branch model

- Status: Accepted
- Date: 2026-10-28

## Context

Several parallel features (adapters, agent hardening, SDKs, deployment) need to
be integrated and tested together on real infrastructure before a release,
while `main` must always be releasable. Release candidates need a stable place
to be cut from, and fixes found during candidate testing must reach `main`
without dragging unfinished work along.

## Decision

- Work happens on short-lived feature branches named
  `<type>/<short-outcome>` (for example `feat/firecracker-jailer`,
  `fix/start-quota-check`).
- Feature branches merge into **`pre-release`**, the integration branch, by
  pull request after CI passes.
- Release candidates are tagged from `pre-release` as `vX.Y.Z-rc.N`. Candidate
  testing (including the real-infrastructure suites in
  [proof-of-concepts.md](../proof-of-concepts.md) once they exist) runs against
  the tag.
- When a candidate is accepted, a release PR from `pre-release` is
  **squash-merged** into **`main`** (the default branch, which the
  organization ruleset protects with linear history, signed commits, code
  scanning and code quality), and the release is tagged from `main` as
  `vX.Y.Z`. The `sync-pre-release` workflow merges `main` back into
  `pre-release` so later release PRs do not conflict with the squash commit. Only release tags are published as
  stable artifacts (images, Helm chart, SDK packages).
- Fixes found during candidate testing go to `pre-release` and produce the next
  `-rc.N`. Urgent fixes to a released version use a `hotfix/<short-outcome>`
  branch from `main`, merge into `main` by pull request, and are merged back
  into `pre-release`.
- `main` accepts pull requests only from `pre-release` or `hotfix/*`. Because
  `main` stays the default branch, contributors and agents pass
  `--base pre-release` explicitly; `pre-release` deliberately carries none of
  `main`'s merge gates so iteration stays fast.

## Consequences

- `main` only moves at releases, so it always matches the latest stable tag.
- Integration problems surface on `pre-release` before they reach users.
- One more branch to keep in sync; contributors must target `pre-release`, not
  `main`, with pull requests.
- The workflows encode the model: `.github/workflows/ci.yml` runs on pull
  requests and pushes to `pre-release` and `main` (including the Postgres
  conformance job) and fails pull requests into `main` whose head is not
  `pre-release` or `hotfix/*`; `.github/workflows/release.yml` publishes
  GitHub pre-releases for `-rc.N` tags and releases for `vX.Y.Z` tags. The
  repository has no remote yet, so these workflows have never run, and branch
  protection rules still have to be configured on the hosting side.

## Alternatives considered

- **Trunk-based development on `main` with release tags.** Simpler, but
  real-infrastructure validation is slow and expensive, so integrating there
  would leave `main` unreleasable for long stretches.
- **Full Git Flow (`develop`, `release/*`, `hotfix/*`).** More ceremony than a
  small project needs; `pre-release` plays the role of `develop` and release
  branches together.
