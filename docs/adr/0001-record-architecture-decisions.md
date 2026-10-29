# ADR 0001: Record architecture decisions

- Status: Accepted
- Date: 2026-10-28

## Context

Differently makes several choices that are easy to second-guess later: driving
Firecracker directly rather than through Kata, keeping Kubernetes out of the
API, depending only on PostgreSQL, and refusing to send work to external
providers by default. New contributors and operators evaluating the project
need to see why each choice was made and what was given up, without
reconstructing it from commit history. The repository has no remote history to
mine, so the reasoning has to live in the tree.

## Decision

Keep architecture decision records in `docs/adr/`, numbered `NNNN-short-title.md`
and listed in [README.md](README.md). Each record has:

- a header with **Status** (Proposed, Accepted, Deferred, Superseded by NNNN)
  and **Date**;
- **Context**: the forces and facts at the time, with code paths where the
  decision is visible;
- **Decision**: what we do, stated so it can be checked against the code;
- **Consequences**: what becomes easier, what becomes harder, and any gap the
  decision leaves open;
- **Alternatives considered**: what was rejected and why.

An accepted record is not rewritten. A change of mind is a new record that
supersedes the old one, and the old record's status is updated to point at it.

## Consequences

- Reviews of design changes can point at the record they change.
- Records must be kept honest about status. Where a decision rests on an
  assumption that has not been tested on real infrastructure, the record says
  so and links the proof of concept that would test it.
- Records cost a little time per decision; trivial or easily reversed choices
  do not need one.

## Alternatives considered

- **Design notes in a wiki.** Rejected: drifts from the code, and is not
  available to people who only have the repository.
- **Long comments in code.** Kept for local rationale (for example
  `internal/policy/policy.go` explains fail-closed merging), but comments do
  not capture rejected alternatives.
