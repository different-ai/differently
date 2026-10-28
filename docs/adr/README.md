# Architecture decision records

Each record states the context, the decision, its consequences and the
alternatives that were rejected. Records are immutable once accepted; a later
record supersedes an earlier one instead of editing it. The format is described
in [ADR 0001](0001-record-architecture-decisions.md). When a later record
changes part of an earlier one, the earlier record's status line says so
("amended by", "superseded by" or "obsolete") and its body stays as history.

| # | Decision | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-direct-firecracker-node-agent.md) | Drive Firecracker directly through a node agent instead of Kata on Kubernetes | Superseded by 0020 |
| [0003](0003-kubernetes-as-substrate-not-api.md) | Kubernetes is a deployment substrate, not the sandbox API | Accepted (amended by 0020) |
| [0004](0004-postgres-only-production-dependency.md) | PostgreSQL is the only production dependency; memory store for development | Accepted |
| [0005](0005-quota-derived-from-state-atomic-activation.md) | Derive quota usage from sandbox state; admit and activate atomically | Accepted |
| [0006](0006-filter-then-rank-placement-fail-closed-external.md) | Filter-then-rank placement; external providers fail closed | Accepted |
| [0007](0007-adapter-contract-optional-interfaces-http-protocol.md) | Small adapter contract with optional interfaces and an HTTP plugin protocol | Accepted (amended by 0020) |
| [0008](0008-secrets-by-reference-only.md) | Control-plane and provider secrets by reference only | Accepted (sandbox env values are a stated exception; amended by 0020 and 0021) |
| [0009](0009-tamper-evident-audit-chain.md) | Tamper-evident, not tamper-proof, audit hash chain | Accepted |
| [0010](0010-apache-2-license.md) | Apache-2.0 licence | Accepted |
| [0011](0011-go-control-plane-ts-python-sdks.md) | Go for control plane and agents; TypeScript and Python SDKs | Accepted (amended by 0020) |
| [0012](0012-pre-release-integration-branch-model.md) | Pre-release integration branch model | Accepted |
| [0013](0013-no-boat-compatible-facade-in-mvp.md) | No Boat-compatible facade in the MVP | Accepted (deferred) |
| [0014](0014-defer-memory-snapshots-until-restore-proven.md) | Do not advertise memory snapshots on Firecracker until restore is proven | Obsolete (ADR 0020) |
| [0015](0015-defer-jailer-integration.md) | Defer Firecracker jailer integration; state the gap | Obsolete (ADR 0020) |
| [0016](0016-single-leader-scheduler-advisory-lock.md) | Single scheduler leader elected with a Postgres advisory lock | Accepted |
| [0017](0017-exec-and-files-proxied-through-control-plane.md) | Proxy exec and file traffic through the control plane | Accepted (amended by 0020) |
| [0018](0018-per-agent-secrets-until-mtls.md) | Per-agent secrets pinned at first registration until mTLS | Obsolete (ADR 0020) |
| [0019](0019-opensandbox-backend-first.md) | OpenSandbox as a backend first; foundation decision after the Fast Sandbox spike | Accepted (backend); foundation decided by 0020 |
| [0020](0020-opensandbox-replaces-node-agent.md) | OpenSandbox replaces Differently's Firecracker node agent | Accepted |
| [0021](0021-provider-credentials-stored-encrypted.md) | Provider credentials can also be stored in Differently, encrypted under a key that stays out of the database | Accepted |
