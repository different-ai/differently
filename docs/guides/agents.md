# Use Differently with a coding agent

This page has two readers: a person who wants their coding agent to set
Differently up, and the agent doing it. The same text is served as plain
text at <https://different-ai.github.io/differently/agents.txt>, and
<https://different-ai.github.io/differently/llms.txt> lists every other page
as Markdown.

## If you are a person

Paste this into your agent (Claude Code, Codex, Cursor or any other that can
run commands):

```text
Read https://different-ai.github.io/differently/agents.txt and follow it:
install Differently on this machine, start it in dev mode, run one sandbox to
prove it works, then show me how to create a sandbox from my code.
```

The agent needs no account, key, cluster or database for this. It installs
two programs into `~/.local/bin`, starts a local server and runs `echo hello`
through it. Dev mode runs commands directly on your machine with no
isolation, so it is for learning the API, not for code you do not trust.

Other things you can ask for the same way: "add Differently to this
project", "deploy Differently on this VM", or "explain what Differently
would change for us". Concerns such as snapshots across providers are
answered in [questions people ask](../faq.md).

Prefer to do it yourself? The [quickstart](quickstart.md) is three commands.

## If you are an agent

Do the one task below that matches what you were asked. Run the commands as
written. Every step has a check: if a check fails, stop and report the
command and its output instead of guessing a fix.

Rules that always apply:

- Dev mode has no isolation. Never use it to run code the person does not
  trust.
- The dev token is for this machine only. Never send it to another host,
  commit it, or print it.
- Leave placement alone. Do not add external providers or change policies
  unless the person asked for exactly that.
- Ask the person only for the things listed under "Ask first" in a task.
- Say what you did not verify.

<!-- Remove this note in the release that ships `differently run` (the first after v0.1.0-alpha.5); do not bump its version. -->
Version note: `differently run`, and the SDKs finding a local server by
themselves, are newer than `v0.1.0-alpha.5`. If `differently --version`
prints that version or an older one:

- instead of `differently run -- echo hello`, use
  `differently sandbox create --wait`, copy the ID it prints, then
  `differently sandbox exec <id> -- echo hello` and
  `differently sandbox delete <id>`;
- give the SDKs the key and the project yourself: set
  `DIFFERENTLY_API_KEY` to the contents of `~/.differently/dev/token` and
  pass `projectId: "prj_default"` (Python: `project_id="prj_default"`) to
  `sandboxes.create`;
- `differently templates` and `--from-snapshot` do not exist, and
  `differently capabilities` prints JSON instead of a table. Read
  `GET /v1/templates`, and start from a snapshot with `fromSnapshot` in the
  API or an SDK.

### Task A: install it and run a first sandbox

Nothing to ask first. Works on macOS and Linux (on Windows, use WSL).

1. Install the CLI and the server from the newest release:

   ```bash
   curl -fsSL https://different-ai.github.io/differently/install.sh | sh
   export PATH="$HOME/.local/bin:$PATH"
   differently --version
   ```

   Check: the last command prints a version.

   The script downloads one archive from the project's GitHub releases,
   verifies it against the release's `checksums.txt`, and writes two files
   into `~/.local/bin`. It does not use `sudo`. If you may not pipe a script
   into a shell, read it first at the same address, or build from source
   instead (needs Go 1.26 or newer): `git clone
   https://github.com/different-ai/differently && cd differently && make
   build`, then use `./bin/differently` and `./bin/differently-server`.

2. Start a local server in the background and wait until it answers:

   ```bash
   differently-server --dev > differently-dev.log 2>&1 &
   until curl -fsS http://localhost:8080/healthz > /dev/null; do sleep 0.2; done
   differently doctor
   ```

   Check: `doctor` prints three `[ok  ]` lines: the server, the key and the
   pool `local`.

   If port 8080 is taken, add `--listen 127.0.0.1:18080` and set
   `DIFFERENTLY_URL=http://127.0.0.1:18080` for every later command.

3. Run a command in a sandbox:

   ```bash
   differently run -- echo hello
   ```

   Check: it prints `hello` and exits 0. One sandbox was created, used and
   deleted.

4. Tell the person what is running and how to stop it: the server is the
   background process you started (`kill %1`, or `pkill differently-server`),
   its log is `differently-dev.log`, the dashboard is
   <http://localhost:8080/dashboard/>, and nothing is kept after it stops.

### Task B: call it from code

Ask first: which language, and whether a Differently server already exists
(its URL and a key) or the local dev server from Task A is enough.

Against a local dev server the CLI and the SDKs need no configuration: on
this machine they read the dev token from `~/.differently/dev/token` and use
project `prj_default`. Against any other server, set `DIFFERENTLY_URL` and
`DIFFERENTLY_API_KEY`, and `DIFFERENTLY_PROJECT` when the key is not limited
to one project.

TypeScript (Node.js 20 or newer). The package is not on npm yet, so install
it from a checkout:

```bash
git clone https://github.com/different-ai/differently.git
(cd differently/sdk/typescript && npm ci && npm run build)
npm install ./differently/sdk/typescript
```

```ts
import { Differently } from "@differently/sdk";

const differently = new Differently();
const sandbox = await differently.sandboxes.create();
console.log((await sandbox.exec("uname -sm")).stdout);
await sandbox.delete();
```

Python (3.9 or newer). The package is not on PyPI yet. Do not run
`pip install differently`: that name belongs to an unrelated project.

```bash
pip install "git+https://github.com/different-ai/differently.git#subdirectory=sdk/python"
```

```python
from differently import Differently

with Differently().sandboxes.create() as sandbox:
    print(sandbox.exec("uname -sm").stdout)
```

Go: `go get github.com/different-ai/differently/pkg/client`, then see the
[Go SDK page](../../pkg/client).

Plain HTTP, for any other language. Every call sends
`Authorization: Bearer <key>`. A key that is not limited to one project,
such as the dev token, must name the project when it creates a sandbox:

```bash
KEY=$(cat ~/.differently/dev/token)
curl -s -X POST http://localhost:8080/v1/sandboxes \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"projectId": "prj_default"}'
# 202 with {"id": "sbx_...", "status": "queued", ...}
curl -s -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/sandboxes/$ID/wait?status=running&timeoutSeconds=60"
curl -s -X POST http://localhost:8080/v1/sandboxes/$ID/exec \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"command": "echo hello"}'
# {"exitCode": 0, "stdout": "hello\n", "stderr": "", ...}
curl -s -X DELETE -H "Authorization: Bearer $KEY" http://localhost:8080/v1/sandboxes/$ID
```

Check, whatever the language: the program prints the command's output and
the sandbox is gone afterwards (`differently sandbox list` shows it
`terminated`).

Write application code so that it:

- never names a backend. Ask for a profile, a template, a capability or a
  data classification, and let the server's policy place the sandbox;
- does not assume a sandbox or a snapshot can move. A sandbox stays on the
  pool it was created on, and a restore (`fromSnapshot`) or fork runs on the
  pool that holds the snapshot. Each provider boots its own images, which a
  template gives one name ([one baseline on every provider](baselines.md));
- deletes sandboxes it creates, or sets `ttlSeconds`;
- branches on the error `code`, not the message. The codes are
  `invalid_request`, `unauthenticated`, `permission_denied`, `not_found`,
  `conflict`, `quota_exceeded`, `no_eligible_pool`, `unsupported_capability`,
  `unsupported_operation`, `provider_unavailable`, `invalid_state`,
  `rate_limited`, `suspended` and `internal`;
- reads its key from the environment, never from source.

The full contract is the
[HTTP API reference](../../api/openapi.yaml), which a running server also
serves at `/openapi.yaml`. Every CLI command is in the
[CLI reference](https://different-ai.github.io/differently/docs/reference/cli/).

### Task C: deploy it for a team

Ask first: where it should run (one Docker host, one Linux VM, or an
existing Kubernetes cluster), whether an OpenSandbox server already exists
there, and which hostname people will use to reach it.

Do not improvise this from the dev-mode steps. A real installation needs a
sandbox runtime (an OpenSandbox server by default) and Postgres, and dev
mode is refused by the Helm chart. Follow one guide from start to finish:

- [One Docker host](../deploy/docker.md): Postgres, OpenSandbox and
  Differently from published images, with one downloaded file and
  `docker compose up`. The quickest real stack, for one machine and code
  the person trusts: sandboxes are containers. CI starts it on every pull
  request on Linux; it has not been run on Docker Desktop or Colima.
- [One Linux VM on any cloud](../deploy/single-vm.md): k3s, OpenSandbox and
  Differently on a single machine. Run end to end on Azure; the same steps
  on other clouds have not been run.
- [Kubernetes (existing cluster)](../deploy/kubernetes.md): the Helm chart
  next to an OpenSandbox server.
- [Prerequisites](../deploy/prerequisites.md) and the
  [deployment overview](../deploy/README.md), which says what is tested,
  documented or unverified.

After the install, [organization onboarding](organization-onboarding.md)
creates the first organization, project and key. Report each step you ran
and each you could not check.

### Task D: change Differently itself

Read [`AGENTS.md`](../../AGENTS.md) in the repository: where to branch, what
to run before pushing, and where each part of the code lives.

## Facts for quick lookup

| Thing | Value |
|---|---|
| Install | `curl -fsSL https://different-ai.github.io/differently/install.sh \| sh` (macOS, Linux; amd64, arm64) |
| Programs | `differently` (CLI) and `differently-server` |
| Local server | `differently-server --dev`, API at `http://localhost:8080` |
| Health check | `GET /healthz` (no key needed) |
| Dev token | `~/.differently/dev/token`, an operator key, valid on this machine's server only |
| Dev organization and project | `org_default`, `prj_default` |
| Dashboard | `http://localhost:8080/dashboard/` |
| Environment variables | `DIFFERENTLY_URL`, `DIFFERENTLY_API_KEY`, `DIFFERENTLY_PROJECT` |
| Default sandbox size | profile `small`: 1 CPU, 1024 MiB memory, 5 GiB disk |
| Default placement | private pools only (`external: never`) |
| What each pool can do | `differently capabilities` (features side by side) |
| What each pool boots for a template | `differently templates` |
| Portability | none after create: a sandbox and its snapshots stay on their pool |
| State in dev mode | in memory; gone when the server stops |
| License and telemetry | Apache-2.0; none |
| Source | <https://github.com/different-ai/differently> |

If something on this page does not match what the software does, the
software is right: report the difference to the person and, if they agree,
[open an issue](https://github.com/different-ai/differently/issues).
