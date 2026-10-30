# Pause runs: where a Fast Sandbox pause spends its time

Scripts that pause and resume a sandbox through Differently over and over,
split every pause into its phases from the Fast Sandbox components' logs,
trace the state-root disk and check that the resumed guest kept its state.
They produced the numbers in
[docs/research/fast-sandbox-pause-time.md](../../docs/research/fast-sandbox-pause-time.md).

Run them on a **test** install only: they fill pool slots, write a full
checkpoint per pause and can drain a burstable disk's credits.

## Requirements

- A single-node (or KVM-node) k3s install with Fast Sandbox, as in
  [docs/deploy/azure-vms.md](../../docs/deploy/azure-vms.md); the scripts run
  on the KVM node with `sudo` (for `k3s kubectl logs` and `/proc`).
- A Differently pool on that runtime, and a project key that can use it.
- Python 3 (standard library only).

```bash
export DIFFERENTLY_URL=http://<server address>:8080
export DIFFERENTLY_API_KEY=...            # a project key; never write it to a file here
export DIFFERENTLY_POOL=opensandbox-microvm
export STATE_ROOT_DEVICE=nvme0n1          # the disk under /var/lib/fast-sandbox
python3 runset.py before idle:3 in-use:5  # fresh sandbox per scenario, N cycles each
python3 summarize.py runs/before-*.jsonl  # Markdown tables: median and p95
```

`idle` pauses a desktop as created. `in-use` first starts Xvfb, LibreOffice,
Chromium and a process holding 384 MiB of random data, writes 300 MiB of
random data and a copy of `/usr/share/doc` to disk, reads most of `/usr`
into the page cache, then adds 64 MiB of new files before every later pause
(`guest-use.sh`).

## What a cycle records

| Field | Source |
|---|---|
| `pause_s`, `resume_s` | Differently's API: from the call until the sandbox reads `paused` / `running` |
| `resume_to_exec_s` | the resume plus the first `exec` that answers |
| `node.phases.quiesce_and_disk_clone` | fastlet: `dump starting` to `paused for dump` (root filesystem reflink, then the VM's vCPUs stop) |
| `node.phases.memory_dump` | fastlet: Firecracker's `dumpAPI` (memory and device state written) |
| `node.phases.checksum_and_manifest` | fastlet: `resumed after dump` to `manifest assembled` (SHA-256 of every staged file, SHA256SUMS, manifest) |
| `node.phases.upload` | runtime agent: `publish started` to `publish completed`; per file from Garage's request log |
| `node.phases.metadata_commit` | fastlet `snapshot published` to the controller's `runtime released` |
| `node.disk.<phase>` | `sampler.py`: MB read and written on the state-root disk, busy share, CPU seconds of Garage, the agent, the fastlet and Firecracker |
| `garage_stored_mb` | growth of Garage's block directory |
| `state_kept`, `procs_kept`, `holder_ok` | `guest-state.sh` before and after: long-lived processes with their start times, their open files, a checksum of the memory-held buffer, a tmpfs file and a disk file, the boot ID |

Compression happens inside Garage (zstd on blocks it does not already
hold), so it shows as Garage CPU during the upload, not as its own phase.
`s3bench.py` uploads a file the way the agent does (one PUT,
`UNSIGNED-PAYLOAD`) or as a parallel multipart upload, to measure the store
alone.

## Traps

- **The page cache decides the upload.** The checksum pass reads the whole
  checkpoint; whether the upload reads it again from disk depends on free
  memory. Keep other memory users (tmpfs files included) off the node while
  measuring, and compare runs on the same host.
- **Burstable disks.** Premium SSD P6 to P20 burst to 170 MB/s on credits;
  a long run can drain them. `dd ... iflag=direct` before and after shows
  whether the disk still bursts.
- **Do not delete a checkpoint a live sandbox came from.** A resumed
  sandbox needs its checkpoint's manifest to take the next one; Fast Sandbox
  1.2.0 then retries the failed pause forever, dumping memory each time.
  `drop-checkpoints.sh` (and `PAUSE_DROP_CHECKPOINTS=1`) is for test hosts,
  with no sandbox alive.
- `punch.py` turns a file's all-zero 64 KiB runs into holes, in place: a
  stand-in for the sparse-pull patch on cached templates. Run it only with no
  sandbox alive.
