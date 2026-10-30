# Fast Sandbox pause time (2026-10-30)

Question: where do the three minutes of a Workbot desktop's pause on Fast
Sandbox go, and how much of it can we remove? On a pool that checkpoints, a
paused sandbox gives its slot back only when the pause finishes
([capacity.md](../scheduling/capacity.md)), so pause time is capacity.

**Result.** Every phase of the pause is limited by the state-root disk
(about 175 MB/s, reads and writes together) and, once the disk is spared, by
the artifact store on one connection. Leaving the template's zero runs as
holes, a Fast Sandbox fix this page carries as a patch and an operator can
apply by hand meanwhile, cut the median pause from 215.6 s to 179.6 s over
eight cycles (−17%; the first pause after a create −45%) with resumes as
fast and the desktop's state intact. Halving every pause needs Fast Sandbox
changes: two more are written as patches or proposals with expected gains
[below](#remaining-ideas-that-need-an-upstream-change).

Legend: **Measured** = run on the test host below; **Read** = from Fast
Sandbox's source at `main` 3d804c0, the code OpenSandbox 1.2.0 ships for
these paths; **Estimated** = derived from measured rates, not run.

## Setup

| | |
|---|---|
| Host | Azure `Standard_D8s_v6` (8 vCPU, 32 GiB), single-node k3s v1.36.5, nested KVM |
| State root | XFS (reflink) on a Premium SSD **P6** (64 GiB): 50 MB/s, bursting to 170 MB/s on credits. Production's KVM node uses a P15 (125 MB/s, bursting to 170 MB/s) and saw 174–186 s pauses ([azure-vms.md](../deploy/azure-vms.md#verify)) |
| Runtime | OpenSandbox 1.2.0, Fast Sandbox (Firecracker v1.16.1, jailed), Garage v2.4.1 on the same disk (zstd level 1, 1 MiB blocks, replication 1) |
| Template | Workbot's desktop image: 2 vCPU, 4 GiB RAM, 9 GiB root filesystem; no balloon device |
| Control plane | Differently `pre-release` at bce0398, pool with `lifecycle.pause-checkpoint` |
| Harness | [test/pause](../../test/pause/README.md): pause and resume through Differently's API, phases from the controller, fastlet, runtime agent and Garage logs, disk and CPU sampled every second, guest state compared before and after |

An **idle** desktop is paused as created. An **in-use** one runs Xvfb,
LibreOffice, Chromium and a process holding 384 MiB of random data, has
300 MiB of random data and a copy of `/usr/share/doc` on disk and most of
`/usr` in its page cache, and gets 64 MiB of new files before every later
pause.

## How a pause works (Read)

1. The fastlet reflinks the sandbox's root filesystem into a staging
   directory, pauses the VM and asks Firecracker for a full snapshot: device
   state plus every page of guest memory, written to the state root and
   fsynced. Then it **resumes the VM**.
2. It hashes every staged file (SHA-256) to write `SHA256SUMS` and the
   checkpoint manifest. The manifest's digest names the upload's directory.
3. The node agent uploads `rootfs.ext4`, `vmstate.snap`, `memory.snap`, the
   egress state, `SHA256SUMS` and the manifest, one streamed PUT each.
4. The controller records the checkpoint, deletes the (still running)
   runtime and marks the sandbox Paused. Only now does Differently see the
   pause finish.

A checkpoint is the full logical size every time: 13.96 GB for this
template (9 GiB disk, 4 GiB memory), whatever the guest changed. Garage
deduplicates it to what changed (34 MB for an idle desktop, about 2 GB in
use), but the node still reads, hashes and streams all of it.

## Before: where the time goes (Measured)

Five pauses with the disk on burst credits, each on a fresh or just-resumed
desktop: an idle one paused three times, an in-use one twice (its third ran
out of burst credits and is listed apart). Medians, with the range:

| Phase | Idle (3) | In use (2) | Disk traffic (median) |
|---|---:|---:|---|
| Request to the node | < 0.1 s | < 0.1 s | |
| Root filesystem reflink, VM paused | < 0.1 s | 1.0 s | |
| Memory dump (Firecracker, fsynced) | 43.4 s (25.6–49.0) | 27.8 s (26.0–29.7) | 4.3 GB written, plus 0.5–3.2 GB of guest pages faulted in |
| Checksum and manifest | 80.6 s (80.5–80.8) | 80.9 s (80.6–81.2) | 14.0 GB read at 175 MB/s |
| Upload (rootfs / memory) | 81.7 s (68 / 14) | 106.7 s (82 / 25) | 9.1–9.7 GB read again; Garage 131–145 CPU s |
| Metadata commit to `paused` | 1.4 s | 1.8 s | |
| **Pause, as Differently sees it** | **207.3 s** (189.8–230.6) | **218.6 s** (215.6–221.5) | |
| Garage's new data | 10 MB | 0.6–2.0 GB | of 13.96 GB logical |

Resume took 1.67 s (median) and the first command answered 0.03 s later.
The in-use pause that ran out of burst credits took **340 s**: its upload
read at 50 MB/s. Yesterday's five desktop pauses on the same host, read
from the Fast Sandbox logs, split the same way (dump 25–27 s, checksum
about 80 s, upload 71–92 s), and production's P15 node reported 174–186 s.

What limits each phase:

- **Memory dump (25–49 s): disk writes.** 4.3 GB at the disk's limit. It
  is slower when the guest's untouched pages are no longer in the host's
  page cache: a restored VM maps its template's (or last checkpoint's)
  memory file, which is dense on disk, so the dump faults gigabytes of zero
  pages back in from the same disk.
- **Checksum (81 s): disk reads.** The whole 13.96 GB is read back at
  173 MB/s, the disk's burst ceiling. Even the memory just dumped usually
  comes from disk: Firecracker runs inside the fastlet's memory cgroup
  (8448 Mi for two desktops), whose limit pushes the dump out of the page
  cache.
- **Upload (81–108 s): Garage and the disk.** Garage spends about 130 CPU
  seconds per pause on one connection (about 1.6 cores); whatever the
  checksum pass's page cache no longer holds is read from disk a second
  time.
- **Metadata commit (1–3 s)** and the hop from Differently to the node
  (under 0.1 s) do not matter.

Reads and writes share one limit: every second of the run moved about
175 MB through the disk, whatever the mix. On a P6 that is the burst
ceiling; once the burst credits are gone it drops to 50 MB/s, and these
phases would take three times as long.

## Candidates (ranked by expected gain and risk)

Expected gains are against the in-use baseline (about 220 s). "Safe" means
a resumed sandbox gets the same memory and disk, and a crash at any point
leaves either the old running sandbox or a complete checkpoint.

| # | Candidate | Lives in | Expected gain | Safe? | Done here |
|---:|---|---|---|---|---|
| 1 | **Sparse templates**: write a pulled template's zero runs as holes, so reflinked roots, staged checkpoints and the memory a VM maps are sparse | Fast Sandbox (node agent pull); stand-in: punch the holes into the cached template | **Measured:** median pause −22% idle, −17% in use; the first pause after a create −45% | Yes: bytes and digests unchanged, the pull still verifies every file | [Patch](fast-sandbox-sparse-pull.patch) with tests; stand-in measured below |
| 2 | **Parallel multipart upload** | Fast Sandbox (node agent) | Upload about 2× where the store is the limit: Garage took 285 MB/s in one PUT, 644 MB/s as four parallel parts and 693 MB/s as eight (from RAM) | Yes: same object; failed uploads are aborted, so no orphaned parts | [Patch](fast-sandbox-multipart-upload.patch) with tests; store measured, not deployed |
| 3 | **Hash while uploading**: one read of the checkpoint instead of two | Fast Sandbox (fastlet and agent) | Removes the checksum pass as a separate read: the remaining checksum phase after #1 | Yes, if checkpoint uploads stop being named by the manifest's digest; the pull already resolves files next to the manifest it is given | Proposal |
| 4 | **Finish the pause at the local commit** and upload in the background | Fast Sandbox (controller and agent) | Pause ≈ dump + manifest: about 30 s, a few seconds with a RAM spill | Changes durability: until the upload ends only the node holds the checkpoint (on these installs the store is on the same disk anyway); a resume on another node must wait for the upload | Proposal |
| 5 | **Incremental checkpoints** against the template or the last checkpoint | Fast Sandbox and Firecracker (diff snapshots, reflink-aware disk diffs) | Bytes in proportion to what changed: an idle desktop's pause would write little more than its dirty pages | Needs chains and garbage collection | Proposal (Fast Sandbox issue #56 tracks diff snapshots) |
| 6 | **Faster state root** | Infrastructure | Every disk-bound phase scales with throughput: a disk at 600 MB/s would cut the dump and checksum about 3.5× | Yes | Not done: new spend |
| 7 | **Smaller guest**: 3 GiB memory, 6 GiB disk | Template (a product decision for Workbot) | Streamed bytes −25% (13.96 → about 10.5 GB); less once #1 makes unused disk free | Yes; less room for the desktop's apps and files | Not done |
| 8 | **Dump into a RAM spill area** (`FAST_SANDBOX_SNAPSHOT_SPILL_DIR`) | Fast Sandbox configuration | Dump 26–49 s → 1.6 s **measured**, but the pause total did not move (210 s): the memory still reaches the disk while the checksum pass reads | Yes, with the fastlet's memory overhead raised by one dump (below) | **Measured**, then reverted |
| 9 | **Drop the guest page cache or inflate a balloon** before the dump | Differently (a command through execd) or the template | None on time: the dump writes all of guest RAM whatever it holds, and this template has no balloon device, so freed pages keep their contents | Yes, but the desktop resumes with a cold cache | Not done (reasoned from the dump path) |
| 10 | **Skip zero and free pages** in the dump | Firecracker / Fast Sandbox | Small after #1: the dump's writes are the full RAM; with #1 the reads already skip zeros | Yes | Not done |
| 11 | **zstd level, parallel compression or none** in Garage | Garage configuration | Small: Garage compresses only blocks it does not hold, yet spent about the same CPU on an idle pause (34 MB new) as an in-use one (2 GB new). Its time goes to hashing every byte on one connection, which #2 spreads | Yes | Not done (measured as not the limit) |

**Differently itself** does not sit in the pause path beyond one call and a
0.5 s poll (request to node: under 0.1 s, commit back to `paused`: 1–3 s).
Its share is to measure: `differently_pause_seconds` times every pause it
asks for, so capsim's `PauseTime` can be checked against production.

**The RAM spill needs memory headroom.** Firecracker runs inside the
fastlet's memory cgroup (two 4 GiB desktops plus 256 MiB, 8448 Mi here), and
a dump into a memory-backed volume is charged to it. Raise the Firecracker
binding's `fastletOverhead` in `fast-sandbox-runtime-environments` by one
dump (about 4.6 GiB for a 4 GiB VM; Fast Sandbox runs one dump at a time per
fastlet) and size the volume to match, or two busy VMs and a dump can exceed
the limit.

## After (Measured)

The template's zero runs punched into holes on the node (the stand-in for
the sparse-pull patch, checked against the manifest's SHA-256), nothing else
changed: Fast Sandbox's configuration, the fastlet's 8448 Mi limit and the
dense checkpoints it writes are as in production. Eight pauses on burst
credits:

| | Idle (3) | In use (5) |
|---|---:|---:|
| **Pause median** | **161.6 s** (before 207.3 s, −22%) | **181.8 s** (before 218.6 s, −17%) |
| **Pause p95** | **166.7 s** (before 230.6 s) | **216.1 s** (before 221.5 s) |
| First pause after create | 114.3 s | 180.4 s |
| Memory dump | 31.1 s | 27.6 s |
| Checksum and manifest | 48.6 s | 51.9 s |
| Upload | 79.2 s | 100.1 s |
| Metadata commit | 1.9 s | 1.6 s |
| Disk read, checksum / upload | 7.9 / 3.9 GB (from 14.0 / 9.1) | 8.4 / 4.9 GB (from 14.1 / 9.7) |
| Disk busy during the upload | 40% | 64% |
| **Resume median / p95** | **1.78 / 1.81 s** (before 1.67 / 1.69 s) | **1.66 / 1.72 s** (before 1.68 / 1.69 s) |
| Resume to first command | 1.80 s | 1.69 s |

Over all eight, the median pause went from 215.6 s to 179.6 s (−17%) and the
p95 from 230.6 s to 216.1 s; resumes did not slow down.

**State kept, every cycle:** the same long-lived processes with the same PIDs
and start times (Xvfb, LibreOffice, Chromium, execd, the guest init), a
process's 384 MiB of random data in memory (checksum recomputed after the
resume), a 256 MiB file in tmpfs (guest RAM), a 300 MiB file on disk, and
the boot ID. The open files of those processes matched too, except that
Chromium opened or closed one socket in two in-use cycles: the guest clock
jumps forward at the resume, and its idle-connection timers fire.

Why the gain is smaller than the bytes removed:

- **The first pause after a create gains most** (207 → 114 s idle): the VM
  maps the sparse template, so the dump faults no zeros in and the checksum
  reads 3.6 GB instead of 14 GB. After a resume the VM maps the checkpoint's
  memory file, which Firecracker wrote densely, so later pauses read 4 GB
  more. Writing the dump sparsely is in the list below.
- **The upload is now Garage's.** With the disk 40–64% busy, Garage hashed
  13.96 GB on one connection at about two cores. That is the parallel
  multipart patch's case.
- **An in-use desktop writes its own data** (about 0.3–2 GB of new blocks per
  pause into Garage on the same disk).

The RAM spill area, measured on its own before the sparse templates (one
in-use cycle on burst credits): dump 1.6 s, pause 210.5 s.

## Remaining ideas that need an upstream change

With sparse templates in place, the pause is a fsynced memory dump (about
25–36 s), a checksum pass that reads mostly written data (28–53 s) and an
upload that Garage more than the disk limits (59–125 s; Garage at about two
cores with the disk 20–64% busy). Each idea below needs a change in Fast Sandbox;
gains are Estimated from those phase times.

| Change | Where | Expected gain on the in-use pause | Status |
|---|---|---|---|
| Sparse pull: write pulled artifacts with holes | node agent (`cache.go`) | What the stand-in measured, on every node and after every pull, without an operator step | [Patch](fast-sandbox-sparse-pull.patch), tested |
| Parallel multipart upload | node agent (`s3client.go`, `publish.go`) | Upload about 2.3× faster where the store limits it: −30 to −45 s | [Patch](fast-sandbox-multipart-upload.patch), tested |
| Hash while uploading (one read) | fastlet (`snapshot_driver.go`) and node agent | The checksum pass's read disappears into the upload's: −25 to −45 s | Proposal: upload checkpoint files under a name from the snapshot ID and write `SHA256SUMS` and the manifest last; the pull already resolves files next to the manifest |
| Write the dump sparse, into a spill area | fastlet (`copyFile` in the spill move) | The dump stops waiting on the disk (1.6 s measured with a RAM spill) and the next resume maps a sparse memory file: −20 to −30 s | Proposal; needs the fastlet memory headroom above |
| Finish the pause at the local commit, upload behind it | controller and node agent | Pause ≈ dump + manifest: about 30 s, or a few seconds with the spill | Proposal; a resume on another node waits for the upload, and until then only the node holds the checkpoint |
| Incremental checkpoints | Firecracker diff snapshots, reflink-aware disk diffs | Bytes in proportion to change: seconds for an idle desktop | Proposal (Fast Sandbox issue #56) |
| Keep the VM stopped after a pause's dump | fastlet | Not pause time: frees the sandbox's CPU and memory at the dump instead of after the upload, and nothing the guest does in that window is silently lost | Proposal |
| Cancel a pause when its sandbox is deleted | fastlet | Not pause time: a deleted sandbox's checkpoint kept hashing and uploading for minutes here, holding the disk | Proposal |
| Evict consumed checkpoints before templates | node agent cache GC | Not pause time: a fastlet restart here evicted the desktop template and kept a stale checkpoint, so the next create pulled 14 GB | Proposal |

## Capacity (capsim)

`differently-capsim` now takes `-pause` and `-resume`, and the Workbot
default pause is the measured 182 s. On four slots
([capacity.md](../scheduling/capacity.md#5-what-it-buys-the-model)):

| Users | Pause | `release` wait p95 | `preempt-idle` wait p95 |
|---:|---:|---:|---:|
| 8 | 216 s (before) | 88 s | 89 s |
| 8 | 182 s (after) | 56 s | 56 s |
| 8 | 90 s (with the upstream changes) | 9 s | 9 s |
| 12 | 216 s (before) | 459 s | 277 s |
| 12 | 182 s (after) | 439 s | 250 s |
| 12 | 90 s (with the upstream changes) | 361 s | 158 s |

A shorter pause cuts the wait directly only while the queue is light (eight
users). At twelve users most of the wait is slots held by computers that are
running or not yet idle; halving the pause there takes the p95 down by about
a fifth under `release` and by about 40% with idle preemption, which is the
bigger lever.
