# Boat

| | |
|---|---|
| Status | **Tested with a fake server** (2026-10-28): implemented from Boat's published API, not run against Boat |
| Adapter | `boat` (`internal/adapters/boat`) |
| Trust | external (provider-managed; forced) |
| Isolation | Virtual machine with its own kernel on dedicated hosts (hypervisor not documented) |
| API checked | [docs.boat.dev/api/v1.md](https://docs.boat.dev/api/v1.md) and the OpenAPI file [docs.boat.dev/openapi/boat-v1.yaml](https://docs.boat.dev/openapi/boat-v1.yaml), read 2026-10-28 |

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | One shell string per command; per-exec env is exported in the command; at most 600 s (Boat's limit); separate stdout and stderr |
| exec.stream | yes | `stream: true` on the command call; NDJSON frames relayed as they arrive |
| files | yes | Only under `/home/user` and `/tmp` (Boat refuses other paths with `invalid_request`); up to 5 MiB per file; parent folders are created |
| stdin | no | Boat's command API has no stdin; requests with stdin answer `unsupported_operation` |
| lifecycle.stop-start | yes | Stop saves the disk and releases the machine; start resumes on a new machine (new IP) with the same disk |
| lifecycle.pause-resume | no | Boat has no pause or hibernate |
| snapshots.disk | yes | Saved snapshots (`POST /saved-snapshots`), taken fresh from a running sandbox; restore by starting from the name |
| snapshots.memory | no | Boat cannot capture memory |
| fork | yes | Through a disk snapshot and a new sandbox from it |
| ports.expose | yes | `https://<subdomain>-<port>.on.boat.dev`; carries an access token unless `portAccess: public` |
| network.egress-control | no | Boat's API has no egress controls |
| max size | 16 vCPU, 32 GB, 251 GB | Fixed sizes: `small` 2/4/51, `default` 4/8/51, `large` 8/16/111, `xlarge` 16/32/251 (vCPU / GB RAM / GB disk) |
| provider TTL backstop | yes, as a stop | `ttlSeconds` auto-stops (keeps the disk); moved with the sandbox's TTL |

## Configure

```yaml
pools:
  - name: boat
    adapter: boat
    trust: external          # forced; the adapter is always external
    region: eu               # Boat runs in the EU and cannot be pinned further
    maxConcurrency: 20
    config:
      apiURL: https://boat.dev/api/v1   # default
      type: small            # optional: the smallest size to use (small|default|large|xlarge)
      snapshot: ""           # optional: a saved snapshot every sandbox starts from
      environment: ""        # optional: a Boat environment (default: Boat's "base")
      org: ""                # optional: the Boat organization billed (team id or name)
      autoStopSeconds: ""    # optional: auto-stop for sandboxes without a TTL (default: off)
      portAccess: token      # token (default) or public
    credentials: {secret: boat-key}
```

Store the key first:

```sh
differently admin secret put boat-key --from-file ./boat-api-key.txt
differently admin pool add boat --adapter boat --trust external --region eu \
  --max-concurrency 20 --secret boat-key
```

The Helm chart's pool schema takes `credentials: {env: BOAT_API_KEY}` with
the key in `server.extraEnv`, or register the pool at runtime as above. Boat
is not placed until policy approves external pools
([keys-and-providers.md](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- **Sizes, not resources.** Boat has four fixed sizes. The adapter picks the
  smallest that fits the requested CPU, memory and disk (from `config.type`
  upward); a request above `xlarge` is refused before placement. `xlarge`
  needs Boat's $100 plan.
- **No images.** Every sandbox runs Boat's Ubuntu 24.04 image. A template
  or `snapshot` is a saved snapshot name; an unknown one fails the sandbox
  with `invalid_request`.
- **TTL stops, it does not delete.** Boat's `ttlSeconds` is an auto-stop.
  With no TTL the adapter turns it off (`null`), which free-trial accounts
  refuse; set `autoStopSeconds` (at most 7200 on the trial) there. A stopped
  sandbox's disk is erased after 14 days, after which it cannot resume.
- **Exec.** No argv, env or stdin fields: argv is shell-quoted, env is
  exported at the start of the command, stdin is unsupported. Commands run
  at most 600 s; a timeout reports exit code 124. Output streams arrive as
  UTF-8 text.
- **Files** are limited to `/home/user` and `/tmp`, 5 MiB each. Boat's
  signed upload and download URLs for larger files are not used.
- **Snapshots** are disk-only and named; each beyond the plan's included
  count costs $0.056 a day, and the free trial has none.
- **Deletes are permanent** and run in the background (`X-Ascii-Confirm-Delete`
  is sent automatically).
- **Errors.** 429 (`rate_limited`, `limit_reached`) and 503
  (`no_ready_machine`, `out_of_capacity`) requeue as `insufficient_capacity`;
  a sandbox that reaches `error` is `sandbox_failed` and is replaced on the
  retry; `cancelled` (no machine) is `insufficient_capacity`; a size the plan
  does not allow is `invalid_request`; 401, 402 and 5xx are
  `provider_unavailable`.
- **Quotas.** Concurrent sandboxes and starts per minute, hour and day depend
  on the plan (trial: 2 concurrent, 5 starts a minute). Create, fork and
  resume each count as a start.
- **Pricing:** per second while running ($0.018 to $0.20 an hour by size);
  monthly plans return as included time; a stopped sandbox costs nothing.

## How it was tested

`go test ./internal/adapters/boat/` against a fake server built from the
OpenAPI file (2026-10-28): create with size selection, label filtering,
env and TTL; idempotent create; replacing a failed sandbox; exec and
streamed exec (env export, cwd, timeout, retryable stream errors, stdin
refused); files (write with parent folder, read, list, path and size
limits); stop and start; `SetExpiry`; saved snapshot, restore and delete;
port exposure; paged `List` that keeps stopped sandboxes; idempotent delete;
error classification for 400, 401, 402, 403, 429, 500, 502, 503 and an
unreachable API. Nothing ran against Boat itself: no account or key was
available.
