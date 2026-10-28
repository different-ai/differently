# ADR 0014: Do not advertise memory snapshots on Firecracker until restore is proven

- Status: Obsolete ([ADR 0020](0020-opensandbox-replaces-node-agent.md)): the Firecracker agent it governed was removed; memory snapshots now depend on the pool's runtime
- Date: 2026-10-28

## Context

Firecracker supports full snapshots of a paused microVM (VM state plus a guest
memory file); diff snapshots are still in developer preview. Restoring
correctly is harder than taking the snapshot. Firecracker's own documentation
notes that:

- network and vsock connections are not preserved across a restore;
- clones need care for network identity and uniqueness (MAC, IP, random
  seeds, machine IDs);
- restore latency is high on cgroup v1;
- snapshot files are trusted input and must be authenticated and encrypted by
  the user;
- the guest memory file must remain available for the life of the restored
  VM, because pages are mapped lazily.

Differently's agent keeps one tap and /30 per VM slot and reaches the guest
over vsock, so a restored clone needs new network identity and a fresh vsock
connection. None of this has been exercised on real KVM.

## Decision

- The node agent does not advertise `snapshots.memory`
  (`internal/agent/agent.go` `Features`), and `Snapshot(kind="memory")` returns
  `unsupported_operation`.
- Disk snapshots (`snapshots.disk`) and disk-based `fork` are offered.
- The Firecracker client already has `CreateSnapshot`
  (`internal/firecracker/client.go`), unused, so the work can start from it.
- Memory snapshots are enabled only after
  [POC 4](../proof-of-concepts.md#poc-4-memory-snapshot-restore-with-network-overrides)
  passes on real hardware, tracked as [backlog E9](../backlog.md#e9-memory-snapshots-on-firecracker).

## Consequences

- Requests for `snapshots.memory` are routed to pools that offer it (Freestyle
  always; Daytona VM class) when policy allows, or fail with
  `unsupported_capability`. They are never silently downgraded.
- `POST /v1/sandboxes/{id}/fork` uses a memory snapshot only when the pool
  advertises one and the source is running; on Firecracker pools it forks from
  a disk snapshot, so the child boots fresh.
- Users who need warm forks today must use an external pool, which may be
  barred by classification.

## Alternatives considered

- **Advertise now and fix restore bugs as they come.** Rejected: a broken
  restore loses user state, and the failure would look like a platform bug.
- **Memory snapshots without network identity changes, same-node only.**
  Possible as an intermediate step for pause-to-disk; still needs the vsock and
  uniqueness work, so it is part of the same POC.
