# Quickstart

Install Differently, start it on your machine and run a first sandbox. Three
commands, no account, no cluster, no database. New to the project?
[Differently explained](../explainer.md) is the five-minute version of what
it is.

Using a coding agent? Paste this into it and skip the rest of the page:

```text
Read https://different-ai.github.io/differently/agents.txt and follow it:
install Differently on this machine, start it in dev mode, run one sandbox to
prove it works, then show me how to create a sandbox from my code.
```

## 1. Install

```bash
curl -fsSL https://different-ai.github.io/differently/install.sh | sh
```

This puts two programs in `~/.local/bin`: `differently` (the CLI) and
`differently-server`. It works on macOS and Linux (on Windows, use WSL),
checks the download against the release's checksums and does not use `sudo`.
If the script says `~/.local/bin` is not on your `PATH`, run the `export`
line it prints.

Other ways to get them:

- **By hand.** Download the archive for your machine from the
  [releases page](https://github.com/different-ai/differently/releases) and
  put its two programs anywhere on your `PATH`.
- **From source** (needs Go 1.26 or newer):
  `git clone https://github.com/different-ai/differently && cd differently && make build`.
  The programs land in `./bin/`.
- **A specific version or directory:** set `DIFFERENTLY_VERSION` or
  `DIFFERENTLY_INSTALL_DIR` before running the script.

## 2. Start a local server

```bash
differently-server --dev
```

Leave it running. It prints its address, where its token is and the next
command to try. Dev mode needs no configuration: it keeps everything in
memory and creates one organization (`org_default`) with one project
(`prj_default`).

> **Dev mode has no isolation.** Sandboxes are directories on your machine
> and commands run as you. Use it to learn the API, never to run code you do
> not trust.

## 3. Run something

In a second terminal:

```bash
differently run -- echo hello
```

That created a sandbox, ran the command in it, printed its output and deleted
the sandbox. The CLI found the local server and its token by itself.

<!-- Remove this note in the release that ships `differently run` (the first after v0.1.0-alpha.5); do not bump its version. -->
> **Version note.** `differently run`, and the SDKs finding a local server
> by themselves (below), are newer than `v0.1.0-alpha.5`, the latest
> release. With that release, use `differently sandbox create --wait` and
> the commands under [A longer tour](#a-longer-tour) instead of `run`, and
> give the SDKs the key and project yourself:
> `DIFFERENTLY_API_KEY=$(cat ~/.differently/dev/token)` and
> `projectId: "prj_default"` in `sandboxes.create`. `--from-snapshot` is
> newer too, and `differently capabilities` prints JSON there instead of a
> table. Building from source gives you everything on this page.

The dashboard at <http://localhost:8080/dashboard/> shows sandboxes, pools
and the audit trail.

## A longer tour

A sandbox you keep, with files, a fork and the placement decision:

```bash
differently doctor                                   # server, key and pools
differently capabilities                             # what each pool can do, side by side
differently sandbox create --wait                    # prints the sandbox ID
ID=<the ID it printed>
differently sandbox exec $ID -- 'echo "hello from $(uname -sm)"'
echo "a note" > note.txt
differently sandbox files put $ID note.txt /work/note.txt
differently sandbox files cat $ID /work/note.txt
differently sandbox fork $ID                         # a copy, with its own ID
differently sandbox placement $ID                    # where it runs and why
differently sandbox list
differently sandbox delete $ID
differently admin verify-audit                       # the audit chain is intact
```

Ask for something no pool provides and you get a precise error instead of a
sandbox that never starts:

```bash
differently sandbox create --capability gpu
# error [unsupported_capability]: no permitted pool supports the requested capabilities
```

`differently sandbox snapshot $ID` prints a snapshot ID, and
`differently run --from-snapshot <snp-id> -- <command>` starts from it.
Snapshots stay on the pool that took them, and each provider boots its own
images: [one baseline on every provider](baselines.md) explains how one name
covers them all.

Every command is in the [CLI reference](https://different-ai.github.io/differently/docs/reference/cli/).

## From your code

Against the local server the SDKs need no key and no project: on this
machine they read the dev server's token and use `prj_default`, like the
CLI.

```ts
import { Differently } from "@differently/sdk";

const differently = new Differently();
const sandbox = await differently.sandboxes.create();
for await (const ev of sandbox.execStream("uname -sm")) {
  if (ev.type === "stdout") process.stdout.write(ev.data);
}
await sandbox.delete();
```

```python
from differently import Differently

with Differently().sandboxes.create() as sandbox:
    print(sandbox.exec("uname -sm").stdout)
```

The SDKs are not on npm or PyPI yet. Each page says how to install it from
the repository: [TypeScript](../../sdk/typescript/README.md),
[Python](../../sdk/python/README.md), [Go](../../pkg/client/client.go). Any
other language can use the [HTTP API](../../api/openapi.yaml) directly; a
running server serves the contract at `/openapi.yaml`.

Against any other server, set `DIFFERENTLY_URL` and `DIFFERENTLY_API_KEY`
(and `DIFFERENTLY_PROJECT` when the key is not limited to one project), or
run `differently login` once for the CLI.

## Next: a real backend

Dev mode proves the API. To run code in real isolation, Differently needs a
sandbox runtime to place work on. It does not run sandboxes itself: by
default it uses an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server that
you install with its own chart or manifests.

| You have | Follow | What you get |
|---|---|---|
| A Docker host | [One Docker host](../deploy/docker.md): download one file, `docker compose up -d --wait` | Postgres, Differently and OpenSandbox from published images; sandboxes are containers |
| One Linux VM | [One Linux VM on any cloud](../deploy/single-vm.md) | k3s, OpenSandbox and Differently from release artifacts |
| A Kubernetes cluster | [Kubernetes](../deploy/kubernetes.md) | The Helm chart next to an OpenSandbox server |

On Kubernetes the install is, in short:

```bash
kubectl create namespace differently
kubectl -n differently create secret generic opensandbox-api-key \
  --from-literal=api-key='<the OpenSandbox server api_key>'
helm install differently deploy/helm/differently -n differently \
  --set opensandbox.apiURL=http://opensandbox-server.opensandbox-system.svc/v1 \
  --set opensandbox.existingSecret=opensandbox-api-key
kubectl -n differently get secret differently-tokens -o jsonpath='{.data.bootstrap-token}' | base64 -d
```

The chart registers the server as a pool named `opensandbox` with
`trust: private`. Then create a tenant and a scoped key as described in
[organization onboarding](organization-onboarding.md). Pool options and what
OpenSandbox supports are in [OpenSandbox pools](opensandbox-pools.md), and
the [deployment overview](../deploy/README.md) says which paths have been
tested.
