# Morph Cloud

| | |
|---|---|
| Status | **Tested with a fake server** (2026-10-28): implemented from Morph Cloud's published API, not run against Morph Cloud |
| Adapter | `morph` (`internal/adapters/morph`) |
| Trust | external (provider-managed; forced) |
| Isolation | Virtual machine per instance ("MorphVM"); the hypervisor is not documented |
| API checked | OpenAPI [cloud.morph.so/api/openapi.json](https://cloud.morph.so/api/openapi.json), [docs](https://cloud.morph.so/docs/developers) and the Python SDK ([morphcloud/api.py](https://github.com/morph-labs/morph-python-sdk/blob/ac0b6454130fd3a2cad2fae4880dfbb0b3498f43/morphcloud/api.py)), read 2026-10-28 |

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `POST /instance/{id}/exec`; env and cwd are written into the command; a timeout runs it under `timeout` (exit 124) |
| exec.stream | yes | `POST /instance/{id}/exec/sse`; accepts both the spec's named events and the SDKs' JSON events |
| files | yes, through exec | Morph has no HTTP files API (its SDKs use SFTP): files move as base64 through exec, 48 KiB per call |
| stdin | no | The exec API has no stdin |
| lifecycle.stop-start | no | Deleting is Morph's only stop |
| lifecycle.pause-resume | yes | Pause snapshots memory and disk and releases compute; billed for storage only |
| snapshots.disk | yes | Captured with memory |
| snapshots.memory | yes | `POST /instance/{id}/snapshot` |
| fork | yes | Through a snapshot and a new instance from it |
| ports.expose | yes | `POST /instance/{id}/http`; public by default (`portAuth: none`) |
| network.egress-control | no | Morph's API has no egress controls |
| max size | not documented | The Developer plan's organization quota is 64 vCPU, 256 GB RAM, 1 TB disk |
| provider TTL backstop | yes | `ttl_seconds` with `ttl_action: stop` (deletes); moved with `/ttl` |

## Configure

```yaml
pools:
  - name: morph
    adapter: morph
    trust: external          # forced; the adapter is always external
    maxConcurrency: 20
    config:
      apiURL: https://cloud.morph.so/api   # default
      snapshot: ""           # optional: the snapshot every instance starts from
      image: morphvm-minimal # default; used for the base snapshot when there is no snapshot
      portAuth: none         # none (default, public URL) or api_key
    credentials: {secret: morph-key}
```

```sh
differently admin secret put morph-key --from-file ./morph-api-key.txt
differently admin pool add morph --adapter morph --trust external \
  --max-concurrency 20 --secret morph-key
```

The Helm chart's pool schema takes `credentials: {env: MORPH_API_KEY}` with
the key in `server.extraEnv`, or register the pool at runtime as above.
Morph Cloud is not placed until policy approves external pools
([keys-and-providers.md](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- **Snapshots, not images.** Instances start from a snapshot. With no
  `snapshot` or template, the adapter creates a base snapshot of `image` at
  the requested vCPU, memory (MB) and disk (MB), keyed by a digest such as
  `differently-morphvm-minimal-2c-4096m-20480d`, and reuses it for every
  sandbox of that shape. An unknown snapshot or image is `invalid_request`.
- **Exec** takes no env, cwd, stdin or timeout. The adapter exports env and
  changes directory inside the command, and runs commands with a timeout
  under coreutils' `timeout`. Stdin is unsupported.
- **Files** run as shell commands and need `base64`, `dirname` and GNU
  `find` in the image (`find -printf` for listings). Large files take one
  exec call per 48 KiB.
- **No stop.** Pause keeps memory and disk and is billed as storage; there
  is no stopped-and-kept state, so `lifecycle.stop-start` is not offered.
- **Exec on a paused instance** is refused by Morph (`412`, reported as
  `invalid_state`); resume first.
- **Ports.** `portAuth: none` gives a public URL: anyone with it can connect.
  `api_key` requires callers to send the pool's Morph API key, which tenants
  should not have.
- **Idempotent create** lists instances by the `differently-sandbox-id`
  metadata first; Morph has no idempotency key or unique name. A failed
  instance is replaced on the retry.
- **Errors.** Morph's FastAPI error bodies (`detail`, `error_code`) are kept
  in messages. 422 is `invalid_request`, 409 and 412 `invalid_state`, 429
  `insufficient_capacity`, 401, 402, 403 and 5xx `provider_unavailable`.
  Morph documents no rate limits or regions.
- **Pricing:** metered in MCU (1 vCPU-hour + 4 GB RAM-hours + 16 GB
  disk-hours); paused instances pay storage only.

## How it was tested

`go test ./internal/adapters/morph/` against a fake server built from the
OpenAPI spec (2026-10-28). The fake runs exec commands through a local
`/bin/sh`, as Morph runs them in the VM, so the adapter's env, cwd and file
scripts run for real: create through a shared base snapshot, idempotent
create, replacing a failed instance; exec with env, cwd, argv and exit codes;
both SSE formats; a 144 KiB binary file through chunked writes and reads,
missing paths and directories; pause, exec while paused, resume; `SetExpiry`;
snapshot, restore and delete; port exposure; paged `List`; idempotent
delete; error mapping for 401, 402, 409, 412, 422, 429 and 502. Listings
(GNU `find`) and the timeout (`timeout`) were also run in a Debian
container. Nothing ran against Morph Cloud itself: no account or key was
available.
