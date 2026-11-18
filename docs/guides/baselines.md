# One baseline on every provider

**The question:** "If my sandboxes can land on different providers, don't I
need my image or snapshot on each one?"

**The answer:** yes. Every backend boots its own images, and nothing copies
a snapshot from one provider to another. What Differently adds is that you
do this once per provider and never again per application: you give those
baselines **one name**, your code asks for the name, and Differently sends
the request only to pools that have it, handing each backend its own image.

```mermaid
flowchart LR
  A["your code<br/>template: python"] --> D[Differently]
  D -->|registry.example.com/python:3.12| P1[OpenSandbox<br/>on your hosts]
  D -->|snapshot python-312| P2[Daytona]
  D -.->|no baseline: never chosen| P3[Freestyle]
```

## What is shared and what is not

| | Shared across providers? | What Differently does |
|---|---|---|
| The **name** your code uses for a baseline | Yes | One template name maps to each pool's own image or snapshot |
| The **baseline itself** (image, snapshot) | No. You build it on each provider you want to use | Routes only to pools that have it, and says which do not |
| A **running sandbox** | No. It stays on the pool it was created on | Decides the pool once, at create, and records why |
| A **snapshot** of a running sandbox | No. It stays with the backend that took it | Remembers which pool holds it and sends every restore and fork there |
| What a provider **can do** (memory snapshots, fork, stop and start) | No. Backends differ | Shows the differences side by side, and places a request that needs a feature only where it exists |

So the rule is: **Differently routes when a sandbox is created, and the
sandbox then stays where it is.** That is why a missing baseline can never
surprise a running workload. It can only make one pool ineligible for new
ones.

## Set it up

### 1. Build the baseline on each backend

Use each backend's own form. This is the part nothing can do for you.

| Backend | A baseline is | The value to register |
|---|---|---|
| OpenSandbox, Docker or Kubernetes runtime | A container image in a registry the runtime can pull from | The image, such as `registry.example.com/python:3.12` |
| OpenSandbox, Fast Sandbox microVMs | A Fast Sandbox template | The template ID (the pool's `templateId`) |
| Daytona | A Daytona snapshot | The snapshot's name |
| Freestyle | A Freestyle snapshot | The snapshot's slug |

### 2. Give them one name

In the server configuration (with the Helm chart, under `config:`):

```yaml
templates:
  - name: python
    description: Python 3.12 with our tools
    images:                                    # keyed by pool name
      opensandbox: registry.example.com/python:3.12
      daytona: python-312
```

A template with `images` runs only on the pools it lists. Add a provider
later by adding one line. The server reads this at start, so apply it with a
restart (`helm upgrade` does that).

### 3. Ask for the name

```bash
differently run --template python -- python3 --version
```

```ts
const sandbox = await differently.sandboxes.create({ template: "python" });
```

Nothing in the request names a provider. Operators can move work between
pools, or add one, without a change to this code.

## See what each pool has

```bash
differently templates
```

```text
TEMPLATE  opensandbox                       daytona     freestyle  DESCRIPTION
python    registry.example.com/python:3.12  python-312  -          Python 3.12 with our tools
```

A dash means that pool has no baseline for the template and will not be
chosen for it. For a sandbox that exists, `differently sandbox placement
<id>` shows every pool that was considered, with the reason each one was
skipped, for example `template python has no image for this pool`.

If no pool you are allowed to use has the template, the request fails at
once with the reason, instead of a sandbox that never starts:

```text
error [no_eligible_pool]: no permitted pool has template python
  classification: internal
  template: python
```

## Snapshots of work in progress

A baseline is a starting point everyone shares. A snapshot is a copy of one
sandbox at one moment: after `pip install`, after a build, in the middle of
a task.

```bash
differently sandbox create --template python --wait      # prints the sandbox ID
differently sandbox exec <id> -- pip install pandas
differently sandbox snapshot <id>                        # prints a snapshot ID, snp_...
differently run --from-snapshot <snp-id> -- python3 -c 'import pandas'
differently sandbox fork <id>                            # a second sandbox from this one, now
```

Starting from a snapshot skips the setup, on self-hosted backends as much as
on hosted ones. The snapshot stays with the backend that took it, so a
sandbox started from it runs on the same pool. You do not have to keep track
of that: the snapshot's record names its pool, and Differently sends the
restore there. If that pool is busy the new sandbox waits for it. If the
request cannot run there at all, it is refused with the reason:

```text
error [no_eligible_pool]: a sandbox started from a snapshot runs on the pool that holds it, onprem, and that pool cannot take this one
  classification: internal
  pool: onprem
```

To have the same prepared state on a second provider, build it there too and
register it as a baseline (steps 1 and 2). To move the *contents* of one
sandbox elsewhere, copy the files out through the API and into a new
sandbox.

## What each provider can do

Snapshot kinds, fork, stop and start, and isolation differ by backend. Put
them side by side:

```bash
differently capabilities
```

```text
                      opensandbox  daytona   freestyle
adapter               opensandbox  daytona   freestyle
trust                 private      external  external
status                healthy      healthy   healthy
fork                  yes          -         yes
lifecycle.stop-start  -            yes       yes
snapshots.disk        yes          yes       yes
snapshots.memory      -            -         yes
...
```

The output above is an illustration (OpenSandbox on its Docker runtime,
Daytona's container class): run the command for what your pools report, and
see the [provider catalog](../providers/README.md) for what has
been tested against each provider. When code needs a feature, say so, and
only pools that have it are considered:

```bash
differently sandbox create --capability snapshots.memory --wait
```

## Try the routing on a laptop

Dev mode can run two pools, which is enough to watch one name go to
different places. Sandboxes here are directories on your machine, so the
"images" are only labels, but the routing is the real thing. Stop any dev
server you have running, save this as `two-pools.yaml`:

```yaml
dev: true
pools:
  - {name: onprem, adapter: devhost, trust: private}
  - {name: burst, adapter: devhost, trust: private}
templates:
  - name: python
    images:
      onprem: registry.example.com/python:3.12
  - name: node
    images:
      onprem: registry.example.com/node:22
      burst: node-22-snapshot
```

```bash
differently-server --config two-pools.yaml --data-dir ~/.differently/dev
```

and in a second terminal:

```bash
differently templates                                         # who has what
differently sandbox create --template python --wait           # always onprem
differently sandbox create --template node --pool burst --wait
differently sandbox create --template python --pool burst     # refused: burst has no python
```

## What this does not do yet

- **Templates are set in the server configuration.** Changing one needs a
  restart. There is no command to add a template at runtime.
- **A snapshot taken through Differently cannot be named as a template's
  image.** A baseline is registered by the backend's own name for it.
- **Nothing copies an image or a snapshot between providers**, and a running
  sandbox does not move. [Storage and lifecycle](../storage-and-lifecycle.md#8-what-is-not-portable)
  has the full list and the reasons.

More questions of this kind are answered in [Questions people ask](../faq.md).
