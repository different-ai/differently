# Keep the setup in a file

Put the pools work may run on, the global placement policy and the templates
in one file in your repository, and let two commands keep a server in step
with it:

```bash
differently diff     # what differs between differently.yaml and the server
differently apply    # make the server match
```

<!-- Remove this note in the release that ships `differently apply` (the first after v0.1.0-alpha.8); do not bump its version. -->
> `differently diff` and `differently apply` are newer than `v0.1.0-alpha.8`,
> the latest release. Until the next one, build the CLI from source
> (`make build`).

Applying the same file again changes nothing, so it can run on every merge.
It uses the admin API the other commands use, with an operator key
(`differently login`, or `DIFFERENTLY_URL` and `DIFFERENTLY_API_KEY`).

## The file

`differently.yaml`, or any file named with `-f`:

```yaml
# Where work may run on this installation.
pools:
  - name: opensandbox
    adapter: opensandbox
    trust: private
    maxConcurrency: 20
    config:
      apiURL: http://opensandbox-server:8090/v1
      image: debian:bookworm-slim
    credentials: {env: DIFFERENTLY_OPENSANDBOX_API_KEY}
  - name: daytona
    adapter: daytona
    trust: external
    maxConcurrency: 10
    credentials: {secret: daytona}
    costModel: {currency: USD, cpuHour: 0.05}
    budget: {monthly: 200}
    attestations:
      soc2-type2: {evidence: "https://trust.daytona.io"}

policy:
  external: overflow
  externalPools: [daytona]

templates:
  - name: python
    images:
      opensandbox: python:3.12-slim
      daytona: python-3.12
```

- **`pools`** has the same shape as `pools` in the server's config file, so
  an entry can move from one to the other unchanged
  ([OpenSandbox pools](opensandbox-pools.md), [providers](../providers/README.md)).
- **`policy`** is the global [placement policy](../scheduling/placement-policy.md).
- **`templates`** gives [one name on every pool](baselines.md).

Credentials are never in the file. A pool names an environment variable or
a file on the server, or a secret stored in Differently
(`differently admin secret`, [keys and providers](keys-and-providers.md)).
The server refuses a pool whose credential it cannot open.

An unknown key is an error, with the file left unapplied.

## What "match" means

The file states what it names.

- **A field the file leaves out stays as the server has it.** Taking
  `maxConcurrency` out of the file does not reset it.
- **A pool or template the file does not name is left alone**, and listed.
  `--prune` deletes those that were set through the API.
- **Three things are exact when the file gives them**, because each decides
  where work may run:
  - a pool's `attestations`: one the server holds and the file does not is
    removed (`attestations: {}` removes them all);
  - a template's `images`: an image for a pool the file does not list is
    removed;
  - the `policy`: the file is the whole global policy, not a part of it.
    In it, leaving `strategy` or `external` out means their defaults,
    `private-first` and `never`.

## What cannot be applied

`apply` changes nothing at all when the file asks for something the API
cannot do, and says which:

- **An existing pool's `adapter`, `trust`, `labels` or `config`.** A pool
  with another backend or another trust is another pool: drain and delete it
  (`differently admin pool`), then apply again.
- **Anything the server's own config file declares.** That file is applied
  again at every start and wins. A pool or template named in both must be
  the same in both; change it there, or take it out of one of the two.

## How it applies

In this order: the policy, pools, templates (which name pools), then with
`--prune` the deletions, templates before the pools they name.

These are separate API calls, not one transaction. If the server refuses
one, the ones before it stay, the error says how far it got, and applying
again goes on from there. Between the first call and the last, the server
is partly in the old setup and partly in the new one.

A deletion the server refuses (a pool that still hosts sandboxes, or that a
template can run only on) stops the apply in the same way.

## In CI

```bash
differently diff --exit-code   # fails when the server does not match the file
differently diff -o json       # the same, for a script
```

Run `diff --exit-code` on pull requests to see what a change would do, and
`apply` on merge.

## What it does not cover yet

Organizations, projects and their quotas, policies for one organization or
project, API keys, stored secrets and watched stacks are not in the file:
use their own commands. Nothing here creates infrastructure: a pool's
backend has to exist already.

Tested against the real control plane in one process (first apply, repeat,
changes, exact sets, what cannot be applied, `--prune`, a refusal part-way)
and by hand against a dev-mode server with `fake` and `devhost` pools. Not
run against a real provider's pool or a server on Postgres.
