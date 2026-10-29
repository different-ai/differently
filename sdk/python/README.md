# differently

Python SDK for Differently, the self-hosted sandbox and microVM
platform. Python 3.9 or newer, standard library only.

Your code asks for a profile, capabilities and a placement policy. Differently
picks the pool; the SDK never needs to know which backend runs the sandbox.

## Install

```sh
pip install differently
```

Until the package is published, install from a checkout: `pip install ./sdk/python`.

## Quickstart

```python
from differently import Differently

# Defaults: DIFFERENTLY_API_KEY and DIFFERENTLY_URL (or http://localhost:8080).
client = Differently()

# Waits until the sandbox is running; pass wait=False to return at once.
sandbox = client.sandboxes.create(
    profile="small",
    placement={"strategy": "private-first", "external": "never"},
)

result = sandbox.exec("uname -sm")  # a string runs via /bin/sh -c
print(result.exit_code, result.stdout)
sandbox.exec(["ls", "-la", "/work"], cwd="/work", timeout_seconds=30)  # a list runs directly

sandbox.files.write("/work/a.txt", "hi")
text = sandbox.files.read_text("/work/a.txt")

sandbox.delete()
```

A sandbox is also a context manager that deletes itself on exit:

```python
with client.sandboxes.create(profile="small") as sandbox:
    print(sandbox.exec("uname -a").stdout)
```

## Streaming output

```python
import sys

for ev in sandbox.exec_stream("make test"):
    if ev.type == "stdout":
        sys.stdout.write(ev.data)
    elif ev.type == "stderr":
        sys.stderr.write(ev.data)
    elif ev.type == "exit":
        print("exit code", ev.exit_code)
    else:
        print("backend error:", ev.error)
```

The command starts when `exec_stream` is called. The iterator ends after
`exit` or `error`. If the connection drops before either arrives, it raises a
`connection_error`.

## Errors

Every failure is a `DifferentlyError` with `code`, `status`, `details` and
`request_id`. Branch on `code`, not on the message.

```python
from differently import DifferentlyError

try:
    client.sandboxes.create(profile="gpu-large")
except DifferentlyError as err:
    if err.code != "quota_exceeded":
        raise
    print("over quota on", err.details.get("dimension"), "request", err.request_id)
```

Server codes include `invalid_request`, `unauthenticated`, `permission_denied`,
`not_found`, `conflict`, `quota_exceeded`, `no_eligible_pool`,
`unsupported_capability`, `unsupported_operation`, `provider_unavailable`,
`invalid_state`, `rate_limited`, `suspended` and `internal`. The SDK adds
`timeout` and `connection_error` (with `status` 0). Waiting raises
`invalid_state` if the sandbox fails or terminates first.

**Retries.** GET, PUT and DELETE requests, and sandbox creation, are retried
up to `max_retries` times (default 3). Retries happen on 503, on 429
`rate_limited` (never `quota_exceeded`) and on connection errors, with
exponential backoff that respects `Retry-After`. Each `create` call sends its
own `Idempotency-Key`, so a retried create never makes a second sandbox. Pass
`idempotency_key` to keep the key stable across process restarts. Exec
and other POSTs are never retried.

## What's covered

| Namespace | Methods |
|---|---|
| `sandboxes` | `create`, `get`, `list`, `iterate`, `update`, `delete`, `wait` |
| `Sandbox` handle | `exec`, `exec_stream`, `files.list/read/read_text/write`, `stop`, `start`, `pause`, `resume`, `snapshot`, `fork`, `expose_port`, `explain_placement`, `refresh`, `update`, `wait`, `delete` |
| `snapshots` | `list`, `get`, `delete` |
| `projects` | `list`, `create`, `get`, `delete` |
| `api_keys` | `list`, `create`, `revoke` |
| `quotas`, `usage` | `get` |
| `audit_events` | `list`, `iterate` |
| `policies` | `list`, `put`, `delete` |
| `capabilities`, `profiles`, `templates`, `classifications` | `list` |
| `admin` | `organizations.{list,create,get,update,suspend,resume}`, `projects.set_quota`, `pools.{list,get,create,update,drain,undrain,delete}`, `agents.list`, `trust_report`, `verify_audit`, `terminate_sandbox`, `reconcile` |
| client | `me`, `version` |

Keyword arguments are snake_case. Records come back as plain dicts keyed exactly
like the API's JSON (camelCase), typed with `TypedDict` in `differently.types`.
`ExecResult`, `ExecEvent` and `Page` are dataclasses.

`list` returns one `Page` (`items`, `next_cursor`); `iterate` walks every page:

```python
for sb in client.sandboxes.iterate(status="running", labels={"team": "ml"}):
    print(sb.id)
```

## Options

```python
Differently(
    api_key="dly_...",       # default: DIFFERENTLY_API_KEY
    base_url="https://...",  # default: DIFFERENTLY_URL, then http://localhost:8080
    timeout=120.0,           # socket timeout in seconds; waits and streams set their own
    max_retries=3,
    headers={},              # extra headers on every request
)
```

Plain `exec` uses the client timeout unless you pass `timeout_seconds`. For long
commands, pass `timeout_seconds` or use `exec_stream`, which has no socket
timeout.

## Development

```sh
python3 -m unittest discover -s tests   # runs against an in-process http.server
```
