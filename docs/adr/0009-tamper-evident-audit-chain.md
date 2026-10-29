# ADR 0009: Tamper-evident, not tamper-proof, audit hash chain

- Status: Accepted
- Date: 2026-10-28

## Context

Operators must answer "who did what, and where did work go" for every action,
and need to know whether the log was edited. A truly tamper-proof log needs
write-once storage or an external notary, which a self-hosted product cannot
assume. Overstating the guarantee would be worse than having none.

## Decision

- Every audit event is sealed into a SHA-256 hash chain: the hash covers the
  previous event's hash and the event's canonical JSON encoding
  (`internal/audit/audit.go` `Seal`; `encoding/json` sorts map keys). The
  first event chains to an all-zero genesis hash.
- Events get contiguous sequence numbers. Postgres appends under a transaction
  advisory lock so `seq` and `prevHash` are never interleaved; event times are
  truncated to microseconds so a reload hashes identically.
- `audit.Verify` detects sequence gaps, hash-link mismatches and content edits.
  `GET /v1/admin/audit/verify` verifies the whole chain; the trust report
  includes the result.
- `audit.file` mirrors sealed events as JSON lines for shipping to write-once
  storage or a SIEM (`internal/auditlog/auditlog.go`).
- The guarantee is stated as **tamper-evident**, and the trust report's
  `limitations` says that anyone with database write access can rewrite the
  whole chain.

## Consequences

- Editing, deleting or reordering stored events is detectable; rewriting the
  entire chain is not, unless a head hash was exported earlier.
- Recording is best effort: if appending fails, the error is logged and the
  action that triggered it still completes (`Recorder.Record`). An outage of
  the audit path therefore produces unaudited actions, visible only in logs.
- Exec events record the program name by default; `audit.recordCommands`
  stores the first 512 characters of the command line. Exec events are
  recorded before the command runs, so their outcome is always `success`.
- Global serialization limits audit throughput to one append at a time per
  database.
- Verification reads the entire chain into memory; it will need paging or
  checkpoints at scale.

## Alternatives considered

- **Plain append-only table.** No evidence of edits.
- **Signing events with a server key.** Adds key management and still does not
  stop an attacker with the key; may be layered on later.
- **External anchoring built in (transparency log, object lock).** Deferred:
  depends on the operator's storage; the JSON-lines mirror is the hook.
