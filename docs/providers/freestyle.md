# Freestyle

| | |
|---|---|
| Status | **Verified live** (2026-10-29): the adapter's live test and a Differently server, against a Freestyle account on the platform's default snapshot (Ubuntu 24.04, x86_64) |
| Adapter | `freestyle` (`internal/adapters/freestyle`) |
| Trust | external (provider-managed; Freestyle is hosted only) |
| Isolation | hardware-virtualized microVMs (Freestyle-managed) |
| API checked | Freestyle's published OpenAPI spec (`/v5`) and the live API, 2026-10-29 |

## Capabilities

| Capability | Support |
|---|---|
| exec | yes; separate stdout and stderr; at most 5 minutes inline; a timeout is exit code 124. Commands run as the image's default user (uid 1000, `ubuntu` on the default snapshot, with passwordless `sudo`) |
| exec.stream | no (buffered fallback) |
| files | yes |
| stdin | yes |
| lifecycle.stop-start | yes: stop runs `poweroff` as root in the guest (Freestyle has no stop endpoint), and VMs are created with Freestyle's automatic restart off so they stay stopped |
| lifecycle.pause-resume | yes; memory and processes kept |
| snapshots.disk | yes (always memory and disk) |
| snapshots.memory | yes |
| fork | yes (memory): running processes carry over |
| ports.expose | yes: a **public** TLS host `<port>-<slug>.<domain>`; anyone with the URL can connect. Works with `egress: none` |
| network.egress-control | yes: no outbound traffic unless `egress: public` |
| Max size | 32 vCPU / 64 GiB / 256 GiB (plan-dependent); the default snapshot boots at 4 vCPU / 8 GiB / 32 GiB |
| Provider TTL backstop | `ttlSeconds` |

## Configure

```sh
differently admin secret put freestyle-key --from-file ./freestyle-api-key.txt
differently admin pool add freestyle --adapter freestyle --trust external \
  --config egress=none --max-concurrency 20 --secret freestyle-key
```

or in the server config:

```yaml
pools:
  - name: freestyle
    adapter: freestyle
    trust: external                # always external
    maxConcurrency: 20
    config:
      apiURL: https://api.freestyle.sh   # default
      egress: none                 # none (default, deny-all) or public
      # snapshot: my-snapshot      # base snapshot for new VMs (id or slug)
      # domain: style.dev          # default domain for exposed ports
    credentials:
      secret: freestyle-key
```

Only the defaults ran live: the platform's default snapshot and the
`style.dev` domain. Leave `egress: none` unless the workload needs the
internet; ports still work without it.

With Helm, set `providers.freestyle.enabled: true` and its `pool.config`.
An external pool receives work only when policy allows it
([approve external providers](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- Create takes no size or env: a VM boots at its snapshot's size and is grown
  with `/resize` (never shrunk). The default snapshot boots at 4 vCPU / 8 GiB
  / 32 GiB, so a smaller request, such as 2 vCPU / 4 GiB / 8 GiB, gets that
  size. Freestyle bills the VM it runs; Differently's quota and cost
  estimates use the requested size.
- Sandbox env is written to a 0600 file owned by the exec user and sourced
  by every exec.
- Files written through Differently are owned by the exec user; directories
  the adapter creates for them are owned by root.
- `/tmp` is emptied when a stopped VM boots again; keep data that must
  survive stop/start elsewhere (`/var/tmp`, the home directory).
- Freestyle boots a stopped VM when it receives a command for it.
- Snapshots always capture memory and disk, and report `kind: memory`.
- Exposed ports are public; use them only for content that may be public.
  Each exposed port takes a `style.dev` name that stays with the Freestyle
  account after the VM is deleted (its routing rule is deleted with the VM).
- Billed by Freestyle under your account's plan.

## How it was tested

- **Fake server** (`freestyle_test.go`, every `go test`): create with resize
  and the env file, idempotent create, exec (cwd, stdin, timeout), files,
  stop, start, pause, resume, snapshots and repeatable snapshot delete, port
  exposure, `List` and repeatable delete. The fake behaves as the live API
  did: it reads the file mode as a decimal number, replaces an existing file
  with one owned by root, refuses `poweroff` from the default user, and boots
  a powered-off VM again while automatic restart is on.
- **Live** (2026-10-29, `FREESTYLE_API_KEY=… go test -tags live -run
  TestLiveFreestyle ./internal/adapters/freestyle/`): three VMs at most at a
  time, each with a provider TTL; every VM and snapshot deleted. Passed:
  health; capabilities; create (running in 0.3–0.5 s, at 4 vCPU / 8 GiB /
  32 GiB); idempotent create; exec with separate stdout and stderr, exit code
  3, argument quoting, sandbox and per-command env, cwd, stdin, and a 2 s
  timeout (exit 124 after 2 s); the env file 0600 and owned by the exec user;
  a rewritten file still readable by commands; file write, read and list, and
  `not_found` for a missing file; `egress: none` blocking an HTTPS request and
  `egress: public` allowing it; a page served on an exposed port reachable at
  its public URL in about 1.2 s, and exposing it again; `List` by
  installation (and nothing for another installation); pause and resume in
  about 0.5 s with a process kept; stop (still stopped 10 s later) and start
  in about 3.5 s together, with a file kept and processes gone; a memory snapshot
  in about 0.3 s; a fork in about 0.5 s with the parent's file, process and
  env; repeatable snapshot delete; repeatable delete and `not_found` after
  it. About 30 s in all.
- **Through Differently** (2026-10-29): a `differently-server` built from
  this branch (memory store) with a Freestyle pool (`trust: external`,
  `egress: none`) that policy allows, driven with the CLI: create to running
  in about 1 s, placement on the pool, exec with sandbox env and exit code 3,
  file upload and download, stop and start with the file kept, pause and
  resume, a port URL serving a page, a memory snapshot, a fork with the
  parent's file, and delete. Nothing was left on the account.
- **Fixed by these runs:** the file mode was sent as `0600`, which Freestyle
  reads as decimal, so the env file was unreadable; rewriting an existing
  file made it root-owned, so a replayed create broke the env file; the
  default user may not power off, so stop timed out; and automatic restart
  booted a stopped VM again about 5 s later.
- **Not run:** `/resize` (the default size was already larger than every
  request), a base snapshot other than the default, a custom domain, plan
  limits and 429s, exec over 5 minutes, and the end-to-end suite.
