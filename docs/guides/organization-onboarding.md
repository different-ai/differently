# Deploying Differently for an organization

This guide covers the two decisions every adopter makes and then walks
through onboarding a team end to end:

1. **Installations:** one Differently per organization, or one shared
   installation serving several organizations as tenants?
2. **Tenancy inside an installation:** organizations → projects → scoped keys,
   with quotas and placement policies.

## 1. One installation per organization, or a shared one?

| | One installation per organization | Shared installation, many tenant orgs |
|---|---|---|
| Who it suits | A company deploying into its own cloud or data center; a vendor shipping into each customer's environment | A platform team serving many internal business units, or a provider hosting tenants |
| Trust boundary | Strongest: separate database, credentials, sandbox runtime and operators | Shared control plane, runtime and operators; tenants isolated by API scoping and quotas, and from each other's sandboxes only by the runtime's isolation (containers share a host kernel; Fast Sandbox microVMs have run on Azure, but their isolation has not been probed) |
| Who operates it | That organization's infrastructure team | One central operator team |
| Data and residency | Entirely inside that organization's boundary | Per-tenant policies (pools, regions, classifications) enforce it |
| Cost | One Postgres + control plane per org | Amortized |

**Recommendation:** when an organization's reason for adopting Differently is
ownership (the usual case), give it its **own installation** inside its own
infrastructure. Use tenant organizations within one installation for internal
divisions that already share an infrastructure operator.

Nothing in Differently calls home, so a per-organization installation needs no
coordination with OpenWork or anyone else.

### Rolling out to a different organization (per-organization install)

Repeat for each organization; every step happens in *their* environment.

1. **Pick the target** — see the [support matrix](../deploy/README.md):
   existing Kubernetes, AWS, GCP, Azure, bare metal or air-gapped.
2. **Mirror artifacts** if the environment is restricted: container images,
   the Helm chart, and OpenSandbox's images and sandbox images
   ([air-gapped.md](../deploy/air-gapped.md)).
3. **Install OpenSandbox** with its own tooling and choose its runtime:
   containers (Docker or Kubernetes), or Fast Sandbox microVMs, which need
   KVM nodes ([prerequisites.md](../deploy/prerequisites.md)). All three
   have run with Differently; the
   [support matrix](../deploy/README.md#support-matrix) says where, and what
   was not covered. Put its API key in a Kubernetes Secret.
4. **Provide Postgres** (managed service or CloudNativePG) and put its DSN in a
   Kubernetes Secret.
5. **Write `values.yaml`**: the `opensandbox` block (its `apiURL`, `trust`,
   `isolation` and API key Secret), profiles, classifications and a global
   policy. Start fail-closed (`policy.external: never`, no external pools).
6. **Install** with Helm, run `differently-server preflight` (the chart's
   migration/preflight hooks do this), and `helm test` for the smoke test.
7. **Hand over the bootstrap operator token** (stored in a Secret the chart
   generates) to that organization's operators. Have them create named
   operator keys and revoke or rotate the bootstrap token
   ([the bootstrap operator token](keys-and-providers.md#the-bootstrap-operator-token)).
8. **Onboard tenants** as in section 2.
9. **Wire observability**: scrape `/metrics`, ship JSON logs, and mirror the
   audit log (`audit.file`) to write-once storage.

## 2. Onboarding a team inside an installation

The examples use the CLI with an operator key
(`export DIFFERENTLY_URL=… DIFFERENTLY_API_KEY=dly_…`, which the `curl`
examples read too). Every step is a plain API call — see
[api/openapi.yaml](../../api/openapi.yaml).

On a new installation, three commands give the first organization, its first
project and a sandbox in it, with no ID to copy:

```bash
differently login                        # saves the URL and the operator token
differently admin org create my-team     # an organization and its first project
differently run -- echo hello            # a sandbox in that project: created, used, deleted
```

`login` takes the URL and key from the environment or from `--url` and
`--api-key`, and asks for the key when there is none. `admin org create`
prints both IDs and, when the CLI has no default project yet, saves the new
project as the default for that server (it is never used with another one).
It never replaces a default, so the organizations you add later change
nothing for your own commands; the steps below name each organization and
project explicitly.

<!-- Remove this note in the release that ships `differently run` (the first after v0.1.0-alpha.5); do not bump its version. -->
> **Version note.** `differently run`, the saved default project and
> `key create` without flags are newer than `v0.1.0-alpha.5`, the latest
> release. With that release's CLI, pass the project to every command
> (`--project <project-id>`, and `--org` with `--key-project` to
> `key create`) and use `differently sandbox create --wait` instead of
> `run`.

### 2.1 Create the organization with quotas

```bash
differently admin org create "Payments Platform" --default-project ci
ORG=org_...          # the organization ID it printed
```

In a script, `-o json` prints the same response as JSON
(`.organization.id`, `.project.id`).

```bash
curl -sX PATCH "$DIFFERENTLY_URL/v1/admin/organizations/$ORG" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -d '{
    "quota": {
      "maxActiveSandboxes": 50, "maxQueued": 200,
      "maxCPU": 100, "maxMemoryMiB": 204800, "maxDiskGiB": 2000,
      "maxTTLSeconds": 86400, "maxSnapshots": 100
    }}'
```

Quota semantics:

- A request larger than any single limit is rejected at admission (429
  `quota_exceeded`, `details.dimension` says which).
- A tenant at its concurrency, CPU, memory or disk limit **queues** instead of
  failing; the sandbox starts as soon as usage drops.
- `maxQueued` bounds the queue.
- Usage is derived from sandbox state on every check, so it cannot drift after
  crashes.

Project quotas (`PATCH /v1/admin/projects/{id}`) nest inside the organization's.

### 2.2 Give the team its own administrators

```bash
differently key create payments-admins --org $ORG --scope org:admin
```

An `org:admin` key manages projects, keys and organization/project policies
inside that organization only. It cannot read other organizations, the admin
API, or pool configuration and credentials.

### 2.3 Projects and least-privilege keys (as the org admin)

```bash
differently project create nightly-builds        # prints the new project, with its ID
differently key create ci-runner --key-project prj_... \
  --scope sandboxes:read --scope sandboxes:write --scope sandboxes:exec --ttl-hours 720
```

`--key-project` limits the key to that project. Without it and without
`--org`, a key that holds only `sandboxes:` scopes, as this one and the
default do, is limited to the CLI's default project: the one
`differently login` saved (the organization's only project, if it has just
one) or the one named with `--project` or `DIFFERENTLY_PROJECT`. A key with
any other scope, such as `projects:write` or `keys:write`, is made for the
whole organization unless `--key-project` limits it, because a
project-limited key cannot create or delete projects. `--org` alone makes a
key for every project of the organization, as in 2.2.

| Scope | Allows |
|---|---|
| `sandboxes:read` | list, get, wait, placement explanation, snapshots list |
| `sandboxes:write` | create, update, delete, stop/start/pause/resume, snapshot, fork, ports |
| `sandboxes:exec` | exec and file transfer |
| `projects:write` | create and delete projects |
| `keys:write` | create and revoke keys (only with scopes the creator holds) |
| `usage:read` | quotas and usage |
| `audit:read` | the organization's audit events |
| `policies:write` | organization and project placement policies |
| `org:admin` | all of the above within one organization |
| `admin` | operator: every organization and the admin API |

Keys can never grant scopes they do not hold, never reach another
organization, and project keys never reach another project. Tokens are shown
once; only a SHA-256 hash is stored.

### 2.4 Decide where the team's work may run

The operator's global policy is the ceiling. Organization and project
policies can only narrow it.

```bash
# Keep the payments org entirely on private pools in one region.
cat > payments-policy.json <<'JSON'
{ "external": "never", "allowedRegions": ["eu-central"], "strategy": "private-first" }
JSON
differently admin policy org:$ORG payments-policy.json
```

Data classifications (`private-only`, `internal`, `public` by default;
operator-defined in config) cap external placement per workload. A sandbox
created with `--classification private-only` stays on private pools even if
every policy above allows overflow. See
[placement-policy.md](../scheduling/placement-policy.md) for the merge rules.

### 2.5 Hand the team its credentials

Give the team the API URL and their project key. Their applications use the
SDK without naming any backend:

```ts
const differently = new Differently({ baseUrl: process.env.DIFFERENTLY_URL, apiKey: process.env.DIFFERENTLY_API_KEY });
const sb = await differently.sandboxes.create({ profile: "medium", classification: "internal" });
```

### 2.6 Verify the setup

```bash
CI_KEY=dly_...                                    # the ci-runner token from 2.3
differently --api-key $CI_KEY doctor              # connectivity and scopes
differently --api-key $CI_KEY usage quota         # limits and current usage
differently --api-key $CI_KEY sandbox create --wait --profile small
differently --api-key $CI_KEY sandbox placement <id>   # which pool, and why
differently admin trust-report                    # operator: external endpoints, placements, limits
```

### 2.7 Offboarding and incidents

- **Suspend** a tenant (blocks new work; optionally terminates everything):
  `differently admin org suspend $ORG --reason "incident 42" --terminate`
- **Revoke** keys: `differently key revoke <key-id>`
- **Drain** a pool before maintenance: `differently admin pool drain opensandbox`
- Every one of these actions lands in the audit chain
  (`differently admin verify-audit`).
