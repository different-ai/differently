# End-to-end tests on real Firecracker microVMs

This suite runs the real stack — Postgres, `differently-server` and a
`differently-agent` driving Firecracker — on a Docker host that exposes
`/dev/kvm`, and exercises it through the public API with the Go SDK. Nothing
is faked: sandboxes are microVMs with their own kernel, disk and network.

## Requirements

- A Docker host with `/dev/kvm` and `/dev/net/tun`:
  - **macOS, Apple Silicon M3 or newer, macOS 15 or newer:** a Colima VM with
    nested virtualization (keeps your default Colima profile untouched):

    ```bash
    colima start differently --vm-type vz --nested-virtualization --cpu 4 --memory 8 --disk 20
    docker context use colima   # optional: keep your usual context as the default
    export DOCKER_CONTEXT=colima-differently
    ```

  - **Linux:** a host or VM with KVM (bare metal, or a cloud instance with
    nested virtualization). Run as a user that can use Docker.
- Go 1.26+ and `curl` on the machine running the tests.
- About 3 GiB of disk for images and the 1 GiB guest rootfs.

## Run

```bash
make e2e-prepare   # images, guest kernel and rootfs (test/e2e/.artifacts/)
make e2e-up        # docker compose up: Postgres, server (:18090), metrics (:19091), agent
make e2e           # go test -tags e2e ./test/e2e/
make e2e-down      # remove the stack and its volumes
```

`e2e-prepare` downloads Firecracker's own CI guest kernel (6.1 series) from
its public artifact bucket, as Firecracker's getting-started guide does; set
`KERNEL_URL` to use another kernel. The rootfs is Debian bookworm with the
guest agent, built by `deploy/images/build-rootfs.sh`.

## What it checks

| Test | Proves |
|---|---|
| `TestMicroVMBootsWithRequestedResources` | A sandbox boots as a Firecracker microVM with the requested vCPUs, memory and disk |
| `TestExecSemantics` | Env (sandbox and per command), stdin, exit codes, separate stderr, timeouts, streamed output |
| `TestFiles` | Binary file upload, download and listing through vsock |
| `TestNetworkIsolation` | Internet egress and DNS work; cloud metadata, the node agent, the control plane and other sandboxes are unreachable |
| `TestStopStartKeepsDisk` | Stop releases the VM and keeps its disk; start boots from it |
| `TestPauseResume` | Pause freezes the VM with memory intact |
| `TestSnapshotAndFork` | A fork boots from the parent's disk snapshot and is independent |
| `TestQuotaQueuesAndReleases` | A tenant at its limit queues; the queued sandbox starts when capacity frees |
| `TestAgentRestartRecovers` | After an agent restart the agent keeps its identity, sandboxes are reported stopped and boot again from disk |
| `TestDeleteCleansUpNode` | Deleting a sandbox ends its Firecracker process |
| `TestAuditTrustAndMetrics` | Audit chain intact with fail-closed request records; trust report has no secrets; metrics only on their own port |

## Verified on

| Date | Host | Firecracker | Guest kernel | Result |
|---|---|---|---|---|
| 2026-10-28 | Apple M4 Max, macOS 26.5, Colima 0.10.3 (`vz`, nested virtualization), arm64 | v1.17.0 | 6.1.188 (Firecracker CI) | 11/11 passed, twice; sandbox create-to-running 3.5–4.4 s |

Not yet covered: x86_64 hosts, bare-metal or cloud KVM, multi-node pools,
the jailer, and external providers.
