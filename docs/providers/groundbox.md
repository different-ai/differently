# Groundbox

> **Experimental adapter.** It is not in the default `differently-server`
> build or the published image. It needs a server built with
> `-tags experimental_adapters`: see
> [what the server contains](README.md#what-the-server-contains).

| | |
|---|---|
| Status | **Verified live** (2026-11-18): the adapter's live test and a Differently server, against Groundbox's hosted API on its own image (Ubuntu 24.04, x86_64) and on a Docker image (`python:3.12-slim`). Run by a Groundbox maintainer on a Groundbox team account, not by the Differently maintainers |
| Adapter | `groundbox` (`internal/adapters/groundbox`) |
| Trust | external (provider-managed; Groundbox is hosted only) |
| Isolation | A Firecracker microVM per sandbox, with its own kernel (Groundbox-managed) |
| API checked | [REST API reference](https://groundbox.dev/docs/api), [sandboxes](https://groundbox.dev/docs/sandboxes), [previews](https://groundbox.dev/docs/previews) and [templates](https://groundbox.dev/docs/templates), read 2026-11-18, the API's published contracts, and the live API |

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `POST /sandboxes/{id}/exec`: a command string runs with `sh -c`, an argument list runs as it is with no shell; separate stdout and stderr; env and cwd native; at most 1 hour on this call (sent when the request sets no timeout; Groundbox's own default is 60 s), longer commands go over the stream; a timeout is exit code 124. The call keeps the last 4 MiB of each stream. Commands run as the image's default user (`runner` on Groundbox's image, with passwordless `sudo`) |
| exec.stream | yes | WebSocket `GET /sandboxes/{id}/exec/stream`, started as a resumable command (up to 30 days): when the connection drops the adapter attaches again and reads on from the last output it saw. A caller that goes away ends the command. A stream error carries Groundbox's error code, which the adapter maps as it maps an HTTP error |
| files | yes | `PUT` and `GET /sandboxes/{id}/files`; parent folders are created; at most 100 MB a file |
| stdin | yes | Up to 512 KiB with the buffered call (`stdin_base64`); more over the stream (`data_base64`). Sent as base64, so bytes that are not text arrive as written. Stdin sent on a stream connection that dropped may not have arrived |
| lifecycle.stop-start | yes | `POST /sandboxes/{id}/stop` shuts the VM down and keeps only its disk (`shutting_down`, then `shut_down`); `POST .../start` boots a new VM from it. Processes and `/tmp` do not survive; the sandbox env does. A stopped sandbox is billed for storage only, and Groundbox deletes one stopped for 30 days |
| persistence.disk | yes | The disk survives a stop |
| lifecycle.pause-resume | yes | Pause keeps memory, disk and processes and frees the VM (`lifecycle.pause-checkpoint`): a paused sandbox is billed for storage only and does not count against the account's running sandboxes. Not for a sandbox with volumes |
| idle pause (native) | yes | The adapter is a `NativeIdler`: a sandbox's `idleTimeoutSeconds` goes with the create as Groundbox's `idle_timeout_seconds` (30 s to 30 days) with `wake_on_request`. Groundbox pauses the sandbox (memory kept) once nothing has reached it for that long, and the next exec, stream, file or preview request wakes it. The reconciler follows the pause and the wake |
| snapshots.memory | yes, of a running sandbox | A kept snapshot (`POST /snapshots`): memory, disk and processes, copied off the sandbox so it outlives it. `DeleteSnapshot` deletes it (`DELETE /snapshots/{id}`) |
| snapshots.disk | yes | Captured with memory; reported as `kind: memory` |
| fork | yes | A new sandbox started from a kept snapshot (`POST /sandboxes` with `snapshot_id`): the files and running processes as they were, at the snapshot's size, with the env of the request. The source may be deleted by then |
| ports.expose | yes | A preview at `https://<slug>-<port>.groundbox.page`. With `ports: private` (default) the URL returned is a share link with a token, valid 24 hours and renewed when asked for again; with `ports: public` anyone with the URL can connect. At most 10 ports a sandbox |
| network.egress-control | yes | `egress: none` (an empty allowlist) or `egress: allowlist` with `egressAllow`, and `egressDeny` with any of them. Entries are host names, wildcards (`*.npmjs.org`), IPv4 addresses and networks; a denied entry wins over an allowed one. The sandbox's DNS is answered by Groundbox, which resolves only what the rules let it reach |
| max size | 16 vCPU, 64 GiB, 100 GiB | At most 4 GB of memory per vCPU. A size outside the limits, or above what the account allows, is refused, never rounded. A team runs a few sandboxes of more than 8 vCPU at once |
| provider TTL backstop | yes | `timeout_minutes` (auto-stop, which deletes the sandbox), at most 30 days from when it was last set; moved with `PATCH /sandboxes/{id}` |

## Configure

```sh
differently admin secret put groundbox-key --from-file ./groundbox-api-key.txt
differently admin pool add groundbox --adapter groundbox --trust external \
  --config egress=none --max-concurrency 5 --cost cpuHour=0.04 --secret groundbox-key
```

or in the server config:

```yaml
pools:
  - name: groundbox
    adapter: groundbox
    trust: external                # always external
    maxConcurrency: 5              # at most the account's limit of sandboxes at once
    costModel: {cpuHour: 0.04}
    config:
      apiURL: https://api.groundbox.dev/v1   # default
      egress: public               # public (default), none or allowlist
      # egressAllow: github.com,*.npmjs.org   # with egress: allowlist
      # egressDeny: 169.254.0.0/16             # with any egress setting; wins over an allow
      ports: private               # private (default: share links) or public
      # template: python-ml        # default template (name, name:version or tpl_...)
      # image: python:3.12-slim    # or a Docker image to start from (not with template)
    credentials:
      secret: groundbox-key
```

The key needs the `sandboxes:read` and `sandboxes:write` scopes. Sandboxes,
stop, pause, kept snapshots and previews are on Groundbox's paid plan. The Helm chart's pool
schema takes `credentials: {env: GROUNDBOX_API_KEY}` with the key in
`server.extraEnv`, or register the pool at runtime as above. An external pool
receives work only when policy allows it
([approve external providers](../guides/keys-and-providers.md#approve-external-providers-safely)).
The server needs egress to `api.groundbox.dev` on 443.

## Limits and differences

- **Sizes.** Memory is sent in whole GB, rounded up (1500 MiB asks for
  2 GB). Groundbox refuses a size outside its limits, so 1 vCPU with 8 GiB
  fails the sandbox with `invalid_request` instead of starting at another
  size. The adapter advertises Groundbox's ceiling (16 vCPU, 64 GiB); an
  account may be allowed less, and a size above it is refused the same way.
  A fork has the snapshot's size. The disk size is sent as asked; only the
  vCPU and memory of a new sandbox were read back in the guest.
- **Templates and images.** A pool's `template` or `image`, or a request's
  template, says what a sandbox starts from. A request's template that reads
  as a Groundbox template (`python-ml`, `python-ml:3`, `tpl_...`) is sent as
  one; anything else (`python:3.12-slim`, `ghcr.io/org/app:tag`) as a Docker
  image. A bare Docker Hub name such as `ubuntu` reads as a template: write
  it as `docker.io/library/ubuntu`. The first sandbox from an image waits,
  queued, while Groundbox turns the image into a disk, which the adapter
  reports as `insufficient_capacity` after 90 seconds; the scheduler's retry
  finds the same sandbox.
- **Lifetime.** Every sandbox has an auto-stop time, at most 30 days ahead.
  The adapter sets it to the sandbox's TTL plus 5 minutes, and moves it when
  the TTL changes. A sandbox with no TTL, or one longer than 30 days, is
  deleted by Groundbox 30 days after its create or last TTL change, and
  Differently then reports it gone. Paused and stopped sandboxes do not
  auto-stop; Groundbox deletes one that stays paused or stopped for 30
  days.
- **Snapshots.** A snapshot is taken of a running sandbox only (a paused or
  stopped one is `invalid_state`). It stays until it is deleted, or until
  no sandbox has been started from it for 30 days, when Groundbox deletes
  it. Starting from a deleted snapshot is `invalid_request`. A team keeps
  at most 50 snapshots by default.
- **Create is repeated safely with an `Idempotency-Key`** made from the
  installation and sandbox ID, which Groundbox remembers for 24 hours. When
  the sandbox a key made has ended (a failed attempt being retried), the
  adapter moves to the next key in the series, so a retry gets a new
  sandbox; Groundbox marks a replayed answer with `Idempotent-Replayed:
  true`, which tells the two apart.
- **List** asks for the installation's sandboxes that are not over
  (`live=true` and `metadata[differently.dev/installation]=...`), every
  page, and still checks `differently.dev/installation` and
  `differently.dev/sandbox-id` on each one. Other sandboxes in the account
  are left alone.
- **A deleted sandbox stays readable** in Groundbox as `stopped`; the
  adapter reports it as `not_found`. A sandbox stopped with its disk kept is
  `shut_down` in Groundbox and `stopped` in Differently.
- **Queued sandboxes.** When no machine has room, Groundbox queues a new
  sandbox. The adapter waits 90 seconds, then reports
  `insufficient_capacity` and leaves the sandbox queued: the scheduler's next
  attempt finds the same one. If the sandbox is placed elsewhere meanwhile,
  the reconciler deletes the queued one.
- **Errors.** `429` (`concurrency_limit`, `rate_limited`) and `no_capacity`
  are `insufficient_capacity`; a sandbox that ends before it runs is
  `sandbox_failed`; a bad size, an unknown template or snapshot is
  `invalid_request`; `401`, `403`, `402` (no credit, a spend cap, no paid
  plan) and `5xx` are `provider_unavailable`. Exec on a paused or stopped
  sandbox is `invalid_state`, on the stream as on the buffered call.
- **Exposed ports.** A share link's token is in the URL: treat the URL as a
  secret. A public port is for content that may be public.
- **Idle.** Groundbox pauses an idle sandbox itself, so Differently's own
  idle sweep leaves this pool alone. Activity is any request that reaches
  the VM (exec, a stream, files, a preview's traffic) and a command started
  through the API that still runs; reading the sandbox is not. A sandbox is
  never idle-paused in its first minute after a start or a wake, so a short
  timeout pauses it about a minute after its last request. The idle timeout
  is set at create: a later `PATCH` of `idleTimeoutSeconds` changes
  Differently's record but not the sandbox in Groundbox. A sandbox paused
  through Differently stays paused until it is resumed; only an idle pause
  wakes on request.
- **Not mapped:** volumes, schedules, desktops (agent computers), team
  secrets, terminals (tty), `on_timeout` (the auto-stop always deletes:
  it is Differently's backstop), rewinding a sandbox to a save point, fork
  groups of more than one copy, flex pricing and the automatic timeline.
- **Regions:** Groundbox does not offer a choice of region.
- **Pricing:** $0.04 per vCPU-hour while running, billed by the second,
  with up to 4 GB of memory per vCPU included; paused and stopped sandboxes
  and kept snapshots are $0.05 per GB-month. Sandboxes need the paid plan
  ($20 a month, with $20 of usage included).
  [groundbox.dev/pricing](https://groundbox.dev/pricing), read 2026-11-18.

## How it was tested

- **Fake server** (`groundbox_test.go`, every `go test -tags
  experimental_adapters`): create with size, env, template or image,
  metadata, egress allow and deny lists and the auto-stop time; idempotent
  create, a retry after a failed sandbox, a replay after a config change,
  and a queued sandbox; exec (cwd, env, an argument list sent with no shell,
  timeout, binary stdin, stdin and commands too large or long for the
  buffered call going over the stream); streamed exec over a fake WebSocket
  (output, exit code, stdin, timeout, re-attaching after dropped
  connections, coded errors, a cancelled caller closing with 1000); files;
  stop and start (the disk kept, `/tmp` not, a replayed create of a stopped
  sandbox); pause and resume; the idle timeout sent at create (within 30 s
  to 30 days, also from a snapshot), an idle pause seen in `Get` and `List`,
  an exec waking it, and a pause through the adapter not waking on
  request; `SetExpiry` up to 30 days; a kept snapshot
  that is still being copied, a fork from it at its size without the
  template, a sandbox from it after its source is deleted, repeatable
  snapshot delete, and a deleted, unknown or foreign snapshot; private and
  public ports; `List` with its filters, every page, and a server that
  ignores the filter; repeatable delete; error classification for 400,
  401, 402, 403, 404, 413, 429, 500, 503 and 504.
- **Live** (2026-11-18, `GROUNDBOX_API_KEY=… go test -tags
  live,experimental_adapters -run TestLiveGroundbox
  ./internal/adapters/groundbox/`): four sandboxes at most at a time, each
  1 vCPU / 1 GiB / 5 GiB with an auto-stop time and an installation ID of
  its own; every one deleted. Passed: health, and a wrong key reported as
  `provider_unavailable` without the key; capabilities; create (running in
  about 0.7 s, 1 vCPU and 984 MiB in the guest); idempotent create; exec
  with separate stdout and stderr, exit code 3, an argument list with no
  shell (`$HOME` and `*` arrive as written), sandbox and per-command env,
  cwd, stdin, binary stdin, 1 MiB of stdin over the stream, and a 2 s
  timeout (exit 124 after 2.3 s); streamed exec (output as it was produced,
  2 s apart, stderr, exit code 5, the timeout) and a cancelled stream ending
  its command; file write, read and list; `egress: none` blocking an HTTPS
  request, `egress: public` allowing it, and an allowlist with a wildcard
  and a deny list (a listed host and one under the wildcard reachable; a
  listed host that is also denied, and an unlisted one, not); a page on a
  private port reachable with its share link and not without the token, and
  on a public port without one; `List` by installation (and nothing for
  another); pause and resume with a process kept (pause 1.8 s and resume
  0.7 s in one run, 7.4 s and 23.5 s in the next), and exec refused while paused, buffered and streamed; stop in about
  0.7 s and start in about 1.5 s with a file kept, a process gone and the
  sandbox env kept, the stopped state in `Get` and `List`, exec refused
  while stopped, and a replayed create answered with the stopped sandbox;
  the auto-stop time moved 26 hours ahead; a kept snapshot (363 MiB) in
  about 1.6 s; a fork from it ready in about 0.7 s with the file as it was
  at the snapshot, the process, and the sandbox env, and the same fork on a
  replayed create; delete; a sandbox started from the snapshot after its
  source was deleted, with the file and the process; repeatable snapshot
  delete and `invalid_request` from a deleted one; with
  `GROUNDBOX_IMAGE=python:3.12-slim`, a sandbox from that image (Debian 13,
  Python 3.12) answering a buffered and a streamed exec; a sandbox with a
  30 s idle timeout paused by Groundbox with `pause_reason: idle` about a
  minute after its last exec, then woken by an exec (0.7 s) with a process
  kept; an empty list at the end. About 106 s in all, every step passing.
- **Through Differently** (2026-11-18): a `differently-server --dev` built
  from this branch with `GO_TAGS=experimental_adapters` and a Groundbox
  pool (`trust: external`, `egress=none`, `--credential
  env:GROUNDBOX_API_KEY`) approved with `admin pool use groundbox
  --approve-external`, driven with the CLI: create with the `small` profile
  to running in about 0.6 s on the pool; exec with sandbox env and exit
  code 3; a streamed exec with lines a second apart; file upload, list and
  download; pause, exec refused as `invalid_state`, resume with a process
  and the file kept; stop, exec refused, start with the disk file and the
  env kept and the process gone; a TTL change (Groundbox's auto-stop moved
  to 5 minutes after it); a port's share link serving a page, and 401
  without its token; a memory snapshot; a fork with the parent's file,
  process and env; the source deleted, then a sandbox from the snapshot
  through the API with the file and process; both snapshots deleted through
  the server (and gone from Groundbox); a reconcile pass; delete of
  everything, with nothing left live in Groundbox.
- **Not run:** a size above 1 vCPU / 1 GiB (16 vCPU is mapped from
  Groundbox's published limit, not run); a template other than Groundbox's
  own image and `python:3.12-slim`; an idle pause through a Differently
  server (the adapter's live test only); the disk size read in the
  guest; a dropped stream connection and the re-attach (fake only); a
  queued sandbox and the capacity and plan errors (fake only); commands
  longer than a few seconds; files near the 100 MB limit; a snapshot left
  to expire; and the end-to-end suite.
