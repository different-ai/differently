# ADR 0004: PostgreSQL is the only production dependency; memory store for development

- Status: Accepted
- Date: 2026-10-28

## Context

Self-hosted software is judged by how many moving parts the operator must run.
The control plane needs durable state, transactions for quota, a queue,
leader election for the scheduler and an append-only audit log. Each could be a
separate system (etcd, Redis, a message broker, an object store). Developers
also need a zero-setup local mode.

## Decision

Use PostgreSQL for everything durable in production
(`internal/store/postgres/postgres.go`):

- resources as JSONB documents with promoted, indexed columns;
- the queue as rows in `sandboxes` with `status = 'queued'` and
  `next_attempt_at`;
- quota checks as transactions with row locks;
- scheduler leadership as a session advisory lock
  ([ADR 0016](0016-single-leader-scheduler-advisory-lock.md));
- the audit chain as a table appended under a transaction advisory lock.

Provide an in-memory store (`internal/store/memory.go`) with the same
contract for `--dev` and tests. Both implementations must pass the shared
conformance suite in `internal/store/storetest/storetest.go`.

## Consequences

- An install needs one database; CloudNativePG or a managed Postgres is
  sufficient. Backups, high availability and encryption at rest are Postgres
  concerns the operator already knows.
- Migrations are embedded and applied under an advisory lock, so several
  replicas can start at once.
- The memory store cannot be shared between processes: run one replica (the
  chart switches to a `Recreate` strategy for it). Its `TryLeader` always
  returns true.
- Throughput is bounded by Postgres. Known hot spots in the current code:
  admission and activation recompute usage by reading every non-terminal
  sandbox document of the organization inside the org lock; audit appends are
  serialized globally. See [reliability-and-performance.md](../reliability-and-performance.md).
- The Postgres conformance run requires `DIFFERENTLY_TEST_POSTGRES_URL`. CI has
  a job for it (`.github/workflows/ci.yml`, Postgres 17), but with no remote it
  has never run, and the local run for this document skipped it; parity
  between the two stores is asserted by the suite's design, not yet by a
  recorded run.

## Alternatives considered

- **etcd.** Good for leader election and small keys; weak for ad-hoc queries
  (usage reports, audit filters) and adds a second database if Postgres is
  needed anyway.
- **Redis or a broker for the queue.** Rejected: the queue must be consistent
  with quota usage in one transaction. Two systems would need an outbox.
- **SQLite for small installs.** Not chosen for the MVP; the memory store
  covers development, and SQLite does not provide multi-replica HA.
- **Kubernetes API objects as the store.** Rejected with
  [ADR 0003](0003-kubernetes-as-substrate-not-api.md).
