# One Docker host

The real stack on any machine that runs Docker, from published images: an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server with
its Docker runtime, and the Differently control plane. You download one
file. There is no clone, no Go, no build and no database to run: Differently
keeps its records in a Docker volume. Every sandbox is a real container on
that host.

It is the step after the [quickstart](../guides/quickstart.md)'s dev mode,
which has no isolation, and before a server for a team
([one Linux VM](single-vm.md) or [Kubernetes](kubernetes.md)).

> **What has run.** CI starts this file on every pull request (the
> `docker host stack` jobs in `.github/workflows/ci.yml`) on a GitHub-hosted
> Ubuntu runner, follows the steps below and creates, uses and deletes one
> sandbox: once with the server image built from the pull request and once
> with the published image this file names. First run 2026-11-18, x86_64,
> Docker Engine 29.4.2 and Compose 5.1.3: the start took 17 s including the
> pulls, and creating the first sandbox, running `echo hello` in it and
> deleting it took 9 s. Those times are from the stack as it was through
> `0.1.0-alpha.7`, with a Postgres container; since `0.1.0-alpha.8` it has
> none, and the times have not been taken again. The stack without Postgres
> also ran once on Colima on an Apple-silicon Mac (arm64, Docker Engine
> 29.5.2), on 2026-11-27, with the image built from the checkout and
> `SANDBOX_PUBLISH_HOST=0.0.0.0` ([Docker in a VM](#docker-in-a-vm-colima-docker-desktop)).
> It has not been run on Docker Desktop, on rootless Docker or on Podman.
> CI stops after the first sandbox, a recreation of the containers and the
> backup command: the other sections from
> [Use it from your machine](#use-it-from-your-machine) on describe how
> Compose and the server behave and have not been run on this stack.

Know what you are getting:

- Sandboxes are containers on Docker's default bridge network. They share
  the host's kernel and can reach the internet and each other.
- That includes each other's control port, which has no authentication:
  code in one sandbox could run commands in another, whatever organization
  or project either belongs to. This follows from how OpenSandbox's Docker
  runtime is set up here and has not been tested. Use this stack to try the
  real thing and for code you trust. It is not a boundary for code you do
  not trust; for that, see the microVM runtime in
  [OpenSandbox pools](../guides/opensandbox-pools.md).
- The OpenSandbox container holds the Docker socket, so whoever controls it
  controls this Docker host. Do not run this next to containers that must
  stay out of its reach.

You need a Linux host with Docker Engine 25 or newer and Docker Compose
2.23.1 or newer (`docker compose version`), and about 1 GB of disk for the
four images it pulls. On macOS or Windows, read
[Docker in a VM](#docker-in-a-vm-colima-docker-desktop) first.

## 1. Download the file

```bash
mkdir differently && cd differently
curl -fsSLO https://different-ai.github.io/differently/compose.yaml
```

It is [`deploy/compose/compose.yaml`](../../deploy/compose/compose.yaml) from
the repository. Both config files it needs are inside it.

## 2. Create the secrets

The file contains no credentials, and `docker compose` refuses to start it
until three values exist in a file named `.env` beside it. Create them once.
The commands write nothing if `.env` is already there or if Docker cannot
run the image:

<!-- test/compose/check.sh runs the next block -->
```bash
img=ghcr.io/different-ai/differently-server:0.1.0-alpha.8
if [ -e .env ]; then
  echo "Keeping the .env that is already here: it holds the only copy of this stack's secrets."
elif docker run --rm $img version >/dev/null; then
  (umask 077 && cat > .env <<EOF
DIFFERENTLY_BOOTSTRAP_TOKEN=$(docker run --rm $img token)
DIFFERENTLY_SECRETS_KEY=$(docker run --rm $img secrets-key)
OPENSANDBOX_API_KEY=$(docker run --rm $img token)
EOF
  )
fi
```

| Value | What it is |
|---|---|
| `DIFFERENTLY_BOOTSTRAP_TOKEN` | Your operator key: the CLI, the API and the dashboard accept it |
| `DIFFERENTLY_SECRETS_KEY` | Seals provider credentials you store in Differently later |
| `OPENSANDBOX_API_KEY` | The key Differently uses to call OpenSandbox |

`token` prints a new random value on every run; the OpenSandbox key uses it
too. The file is created readable by you alone, and an `.env` that is
already there is never replaced: a new secrets key would not open the
provider credentials sealed with the first one. Keep `.env` with the stack
and out of version control.

## 3. Start it

```bash
docker compose up -d --wait
```

The first start pulls one more image, OpenSandbox; step 2 already pulled
the server's. The command returns when both are healthy.

## 4. Run a first sandbox

The server image contains the CLI, and the container already has the
operator key, so nothing needs installing:

```bash
docker compose exec server differently doctor
```

```text
[ok  ] server http://localhost:8080 reachable (version ..., installation inst_...)
[ok  ] authenticated as key... (scopes [admin])
[ok  ] pool opensandbox (private): healthy
```

Create an organization. It comes with a project, and the answer shows the
project's `id` (`prj_...`):

```bash
docker compose exec server differently admin org create my-team
```

Create a sandbox in that project, run a command in it, and delete it. The
first one takes longer, because OpenSandbox pulls the sandbox image
(`debian:bookworm-slim`) and its `execd` image:

```bash
docker compose exec server differently --project <project-id> sandbox create --wait
docker compose exec server differently sandbox exec <sandbox-id> -- echo hello
docker compose exec server differently sandbox delete <sandbox-id>
```

While a sandbox runs, `docker ps` shows it as a container of its own.

## Use it from your machine

The API is at <http://localhost:8080> and the dashboard at
<http://localhost:8080/dashboard/>; sign in with the token in `.env`. With
the CLI [installed](../guides/quickstart.md#1-install):

```bash
export DIFFERENTLY_URL=http://localhost:8080
export DIFFERENTLY_API_KEY="$(sed -n 's/^DIFFERENTLY_BOOTSTRAP_TOKEN=//p' .env)"
export DIFFERENTLY_PROJECT=<project-id>
differently doctor
differently sandbox create --wait
```

The [quickstart's longer tour](../guides/quickstart.md#a-longer-tour) and the
SDKs work the same against this stack. The operator key can do everything,
so give applications a key limited to one project:
[organization onboarding](../guides/organization-onboarding.md). A key
limits what its holder can ask the API for. On this stack it does not keep
sandboxes apart: as said at the top, code in one sandbox could run commands
in a sandbox of another project.

From another machine, tunnel to it (`ssh -L 8080:localhost:8080 <host>`)
instead of publishing the port. To serve a team, put a TLS proxy in front of
it, as in [One Linux VM](single-vm.md#5-optional-https-for-your-team).

## What the host exposes

| What | Where | Why |
|---|---|---|
| The API and dashboard | `127.0.0.1:8080` | Every request needs a key, but nothing outside the host needs it yet |
| The OpenSandbox API, `/metrics` (port 9091 in the `server` container) | Not published | Only the stack's own containers use them |
| Each sandbox's control ports, two per sandbox from 40000 to 60000 | The gateway address of Docker's default bridge, `172.17.0.1` | OpenSandbox reaches sandboxes through them. They have no authentication of their own, so this file does not publish them on every interface |

OpenSandbox's own default publishes sandbox ports on every interface; this
file narrows that to the bridge gateway. That address is not loopback.
Whether another machine can reach those ports through it depends on the
host's forwarding and firewall rules, which has not been tested, so keep
ports 40000 to 60000 closed to other machines yourself.

If your Docker's default bridge has another address,
Docker cannot publish on `172.17.0.1` and sandboxes fail to start. Find the
right one and add it to `.env`:

```bash
docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
# then, in .env:  SANDBOX_PUBLISH_HOST=<that address>
```

### Docker in a VM (Colima, Docker Desktop)

It has run once on Colima (2026-11-27, with the setting below) and not on
Docker Desktop. OpenSandbox reaches a sandbox at
`host.docker.internal`, which on a Linux host is the bridge gateway. A
Docker that runs in a VM may point that name at your Mac or PC instead,
where a port published on `172.17.0.1` does not exist; the first sandbox
would then never become ready.

`SANDBOX_PUBLISH_HOST=0.0.0.0` in `.env` publishes sandbox ports on every
interface of the Docker host, which is OpenSandbox's default and how the
[end-to-end suite](../../test/e2e/README.md) has run on Colima. Use it only
where no network you distrust can reach those interfaces, such as a VM on
your own laptop, never on a server.

## Settings

Add these to `.env`, then run `docker compose up -d --force-recreate`:

| Variable | Default | Meaning |
|---|---|---|
| `DIFFERENTLY_VERSION` | `0.1.0-alpha.8` | Image tag of the control plane: a [release](https://github.com/different-ai/differently/releases)'s version without its leading `v` |
| `DIFFERENTLY_IMAGE` | `ghcr.io/different-ai/differently-server` | Image repository, for a mirror or your own build |
| `DIFFERENTLY_BIND` | `127.0.0.1` | Host address the API is published on |
| `DIFFERENTLY_PORT` | `8080` | Host port of the API |
| `SANDBOX_PUBLISH_HOST` | `172.17.0.1` | Host address sandbox ports are published on |

A variable exported in your shell wins over the same name in `.env`. The
[install script](../guides/quickstart.md#1-install) reads
`DIFFERENTLY_VERSION` too, written with the `v`. If that is still exported,
Compose asks for an image tag that does not exist and the pull fails:
`unset DIFFERENTLY_VERSION` first.

The control plane's config (the pool, the profiles `small` and `medium`, a
one-hour default lifetime, `external: never`) is the `differently` entry
under `configs:` in the file. Edit it there and run
`docker compose up -d --force-recreate`, which recreates both
containers. [OpenSandbox pools](../guides/opensandbox-pools.md) lists the
pool options.

## Keep it running

- **Restart.** Every container restarts with Docker, so the stack comes
  back after a reboot. `docker compose stop` and `docker compose up -d` stop
  and start it by hand.
- **Where the records are.** Organizations, keys, sandbox records and the
  audit chain are files in the `differently-data` volume, written by the
  [file store](../adapters/stores.md#the-file-driver). One server uses them;
  for several replicas, use Postgres on [Kubernetes](kubernetes.md).
- **Upgrade.** Set `DIFFERENTLY_VERSION` in `.env` (or download the file
  again), then `docker compose pull && docker compose up -d`. The volume
  stays. A stack installed from a file older than `0.1.0-alpha.8` keeps its
  records in Postgres: see [From a stack with Postgres](#from-a-stack-with-postgres).
- **Back up.** Stop the server, copy its data directory out, start it again,
  and keep a copy of `.env`:
  `docker compose stop server && docker compose cp server:/var/lib/differently ./differently-backup && docker compose start server`.
- **Remove.** Delete your sandboxes first: they are containers beside the
  stack, not part of it, so `docker compose down` leaves them running.
  Then `docker compose down` keeps the records, and
  `docker compose down -v` deletes them.

### From a stack with Postgres

Through `0.1.0-alpha.7` this file started a Postgres container and kept the
records there. The file store does not read them, and there is no tool yet
that copies records from one store to another. Choose one:

- **Keep the records.** Keep the `compose.yaml` you have and change only the
  version: `DIFFERENTLY_VERSION=0.1.0-alpha.8` in `.env`, then
  `docker compose pull && docker compose up -d`. Postgres remains a store
  the server supports.
- **Start again without Postgres.** Delete your sandboxes, run
  `docker compose down -v` with the old file, download the new file, and
  follow this page from step 2 with a new `.env`. Organizations, keys and
  the audit chain start empty.

Do not start the new file over the old one in the same directory: Compose
would leave the Postgres container running as an orphan and the server would
start on an empty store, with the old records out of sight.

## If something fails

| What you see | Cause |
|---|---|
| `required variable ... is missing a value: create .env first` | Step 2 has not run in this directory, or an `.env` without the three values was there first and step 2 kept it: move it away, run step 2, then add your settings back |
| `docker compose` rejects the file: `start_interval`, or the `content` of a config | Docker Engine older than 25, or Docker Compose older than 2.23.1 |
| The `server` image cannot be pulled because its tag does not exist | `DIFFERENTLY_VERSION` has a leading `v`, usually exported for the install script: `unset DIFFERENTLY_VERSION` |
| The `server` container cannot publish port 8080 | Something else uses it: set `DIFFERENTLY_PORT` |
| A sandbox fails to start with a Docker error about publishing a port | Another bridge address: set `SANDBOX_PUBLISH_HOST` as above |
| The stack is healthy but a sandbox never becomes ready | OpenSandbox cannot reach the sandbox's published ports. On Colima or Docker Desktop, see [Docker in a VM](#docker-in-a-vm-colima-docker-desktop). On Linux, a firewall that denies incoming connections by default, such as ufw, may block the stack's containers from reaching ports 40000 to 60000 on the bridge gateway. Neither case has been tested |
| The `server` container exits and its log says the file store cannot write `/var/lib/differently/store` | The `differently-data` volume belongs to another user, usually because it was created by hand or by an image older than `0.1.0-alpha.8`: remove the volume (`docker compose down -v`) and start again |

`docker compose logs server` and `docker compose logs opensandbox-server`
show the rest.
