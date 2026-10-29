# ADR 0015: Defer Firecracker jailer integration; state the gap

- Status: Obsolete ([ADR 0020](0020-opensandbox-replaces-node-agent.md)): Differently no longer runs Firecracker; sandboxing the VMM is the runtime's job
- Date: 2026-10-28

## Context

Firecracker's jailer wraps each VMM process in a chroot, its own namespaces
and cgroups, and drops privileges to an unprivileged UID/GID before starting
Firecracker. It is Firecracker's recommended production setup and limits the
damage of a VMM escape. It requires a statically linked (musl) Firecracker of
the same version, a per-VM chroot layout with the kernel, rootfs and sockets
hard-linked or bind-mounted inside, and correct device node permissions.

The MVP needed a working lifecycle first, and the agent has only been tested
with a fake launcher.

## Decision

- The agent launches Firecracker directly
  (`internal/agent/agent.go` `ExecLauncher.Launch`:
  `firecracker --api-sock <vmdir>/fc.sock --id <sandbox>` in a new session,
  logs to `firecracker.log`). It runs with the agent's privileges: root, and
  in the default Helm values a privileged container.
- Firecracker's built-in seccomp filters stay at their defaults (the agent
  passes no `--no-seccomp` or custom filter).
- The gap is stated in the trust model. Jailer integration is
  [backlog E4](../backlog.md#e4-firecracker-jailer), preceded by
  [POC 3](../proof-of-concepts.md#poc-3-jailer-integration).

## Consequences

- A guest that escapes the VMM process gets the agent's privileges on the node,
  which in the default Helm configuration is a privileged container with the
  host's `/dev/kvm`, `/dev/net/tun` and the state directory. Hardware
  virtualization remains the primary isolation boundary.
- Operators who require the jailer cannot run Differently in production yet.
- **Misleading switch:** the Helm chart has `agent.jailer.enabled`, which
  passes `--jailer=<path>` to the agent. The agent accepts the flag "for
  forward compatibility" and `doctor` checks that the binary exists, but
  microVMs still run without the jailer. Turning the value on changes nothing
  about isolation.
- The `Launcher` interface isolates the change: a jailer launcher can replace
  `ExecLauncher` without touching lifecycle code, but paths (`APISocket`,
  `VsockPath`, `Rootfs`) must become chroot-relative.

## Alternatives considered

- **Block the MVP on the jailer.** Rejected: nothing has run on real KVM yet,
  and jailer work is best done together with that validation.
- **Run the agent unprivileged with device plugins instead.** The chart
  supports an unprivileged mode with `NET_ADMIN` and device resources; it
  reduces the agent's privileges but is not a substitute for per-VM jails.
