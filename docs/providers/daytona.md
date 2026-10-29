# Daytona

| | |
|---|---|
| Status | **Verified live** for `class: container` (2026-10-29): the adapter's live test and a Differently server, against a Daytona account in region `us`. `class: linux-vm` has not run: the account had no VM snapshot available in its only region |
| Adapter | `daytona` (`internal/adapters/daytona`) |
| Trust | external (provider-managed) |
| Isolation | Daytona-managed containers (`class: container`, the default) or VMs (`class: linux-vm`, from a VM snapshot) |
| API checked | Daytona's published OpenAPI specs for the control plane and the toolbox, 2026-10-28, and the live API, 2026-10-29 |

## Capabilities

| Capability | Support |
|---|---|
| exec | yes; stdout and stderr come back combined; a timeout is exit code 124 with no output. Commands run as `daytona` on the default snapshot |
| exec.stream | no (buffered fallback) |
| files | yes |
| stdin | no (`unsupported_operation`) |
| lifecycle.stop-start | yes; the disk is kept |
| lifecycle.pause-resume | `class: linux-vm` only (not run live; containers answer `invalid_request`) |
| snapshots.disk | yes; restore by creating from it |
| snapshots.memory | `class: linux-vm` only (not run live) |
| fork | no memory fork; Differently's fork, which creates from a disk snapshot, works |
| ports.expose | yes (signed preview URL, valid 1 h) |
| network.egress-control | no |
| Max size | 4 vCPU / 8 GiB / 10 GiB (Daytona's default maximum); the size comes from the snapshot |
| Provider TTL backstop | `ttlMinutes`, which Daytona turns into its delete deadline; Daytona's own auto-stop is off, so Differently owns stop |

## Configure

```sh
differently admin secret put daytona-key --from-file ./daytona-api-key.txt
differently admin pool add daytona --adapter daytona --trust external \
  --config target=us --config class=container \
  --region us --max-concurrency 20 --secret daytona-key
```

or in the server config:

```yaml
pools:
  - name: daytona
    adapter: daytona
    trust: external                # always external
    region: us
    maxConcurrency: 20
    config:
      apiURL: https://app.daytona.io/api   # default
      target: us                   # Daytona region; also the pool's region
      class: container             # or linux-vm, with a VM snapshot
      # snapshot: daytona-medium   # base snapshot; it sets the size
    credentials:
      secret: daytona-key
```

The snapshot decides the sandbox's size and class. Daytona's shared
snapshots include `daytona-small` (1 vCPU / 1 GiB / 3 GiB), `daytona-medium`
(2 vCPU / 4 GiB / 8 GiB) and `daytona-large` (4 vCPU / 8 GiB / 10 GiB);
without one, Daytona's default snapshot gives 1 vCPU / 1 GiB / 3 GiB.
`class: linux-vm` only changes what the pool advertises: set `snapshot` to a
VM snapshot (such as `daytona-vm-small`) that is available in `target`, or
sandboxes are containers that refuse pause.

With Helm, set `providers.daytona.enabled: true` and its `pool.config`. An
external pool receives work only when policy allows external placement and
lists it in `externalPools`
([approve external providers](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- Requested CPU, memory and disk are not sent, because Daytona refuses them
  with a snapshot ("Cannot specify Sandbox resources when using a
  snapshot"): a sandbox gets its snapshot's size. Quota and cost still use
  the requested size.
- Containers enforce the size as cgroup limits; `nproc` and `free` inside
  show the host's.
- Exec has no stdin and no streaming, and timeouts default to 600 s. A
  command that times out returns exit code 124 without its output.
- Stop and start return once Daytona has finished. A delete during a stop or
  start waits for it, and a repeated delete succeeds.
- Daytona's list can show a deleted sandbox for a few seconds. The
  reconciler may then delete it again, which is harmless.
- A snapshot is an image in your Daytona organization and counts against its
  snapshot quota until deleted.
- Billed by Daytona under your account's plan.

## How it was tested

- **Fake server** (`daytona_test.go`, every `go test`): create and
  idempotent create, `List` for the reconciler, exec (stdin refused, a
  timeout as exit 124), files, stop and start returning once settled, pause
  refused on a container, a disk snapshot waited on until active and deleted
  by id (twice), delete during a stop and repeated delete, health,
  capabilities per class, and error classification (429 as
  `insufficient_capacity`, outages as `provider_unavailable`). The fake
  behaves as the live API did: stop, start and delete answer while still in
  progress, a delete during a change is a 409, a command timeout is a 408,
  and snapshots are deleted by id only.
- **Live** (2026-10-29, `DAYTONA_API_KEY=… go test -tags live -run
  TestLiveDaytona ./internal/adapters/daytona/`): three sandboxes at most at
  a time, each with a provider TTL; every sandbox and snapshot deleted.
  Passed: health; capabilities; create on the default snapshot (running in
  about 2 s); idempotent create; exec with output, exit code 3, argument
  quoting, sandbox and per-command env, cwd, and a 2 s timeout (exit 124
  after 2.1 s); stdin refused; file write, read and list (with size, mode and
  time), and `not_found` for a missing file; a page served on port 8080
  reachable at its signed URL in about 1.5 s; `List` by installation (and
  nothing for another installation); stop and start together in 4–14 s with
  a file kept; a disk snapshot (9–22 s until active); a new sandbox created from
  it in 3–4 s with the file; repeatable snapshot delete; a `daytona-medium`
  sandbox with cgroup limits of 2 vCPU and 4 GiB and an 8 GiB disk;
  repeatable delete and `not_found` after it; the list empty 1–3 s after the
  delete. About 45 s in all. `class: linux-vm` was refused
  ("Snapshot daytona-vm-small is not available in region us"), so pause,
  resume and memory snapshots did not run.
- **Through Differently** (2026-10-29): a `differently-server` built from
  this branch (memory store) with a Daytona pool (`trust: external`, target
  `us`) that policy allows, driven with the CLI: create to running in about
  2 s, placement on the pool, exec with sandbox env and exit code 3, file
  upload and download, stop and start with the file kept, a port URL serving
  a page, a disk snapshot, a fork created from it with the parent's file,
  and delete. Nothing was left on the account.
- **Fixed by these runs:** stop and start returned while the sandbox was
  still stopping or starting; a command timeout was reported as
  `provider_unavailable`, which counts against the pool's health; a repeated
  delete, or one during a stop, failed with `invalid_state`; snapshot delete
  sent the name where Daytona needs the id; and a snapshot was handed back
  before it could be created from.
- **Not run:** `class: linux-vm` (pause, resume, memory snapshots), regions
  other than `us`, the `archived` state, plan limits and 429s, and the
  end-to-end suite.
