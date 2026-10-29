# Keys and providers

This guide is for operators who manage credentials in Differently. It
covers:

- storing provider credentials (Daytona, Freestyle, OpenSandbox, plugin
  keys) in Differently instead of in Kubernetes Secrets;
- switching which provider new sandboxes go to;
- rotating provider keys and Differently's own API keys without downtime;
- the bootstrap operator token, and backing up the key that protects the
  stored credentials.

> **Evidence level.** Unit and in-process API tests cover everything here
> ([What is verified](#what-is-verified)). None of it has run against live
> Daytona or Freestyle accounts, on a real cluster, or with more than one
> server replica. Try it in a staging installation first.

The examples use the CLI with an operator key and the Helm release `dly`
in namespace `differently`:

```sh
export DIFFERENTLY_URL=https://differently.example.com
export DIFFERENTLY_API_KEY=dly_...        # an operator key (admin scope)
```

Every CLI command is a plain API call too
([api/openapi.yaml](../../api/openapi.yaml)). The dashboard
(`$DIFFERENTLY_URL/dashboard/`) calls the same endpoints.

## Several server replicas

Every replica re-reads the pool registry and every pool's credential about
every 5 seconds. A change made through one replica therefore reaches the
others, including the scheduler leader that places sandboxes, without a
restart. That covers adding a pool, `use`, drain, undrain, `PATCH`, delete
and replacing a stored secret. A unit test covers this with two pool
managers sharing one store; a multi-replica cluster has not been tried yet.

## Three kinds of credentials

| Credential | What it is | Who holds it | Where Differently keeps it | How it rotates |
|---|---|---|---|---|
| **Differently API keys** (`dly_<id>_<secret>`) | What clients use to call Differently. The kinds are operator keys (`admin`), organization admin keys (`org:admin`), and project keys for CI or applications, such as the OpenWork Workbot runner. The bootstrap operator token is one of these | Whoever the key was issued to; the token is shown once | A SHA-256 hash of the secret, never the token | [`POST /v1/api-keys/{id}/rotate`](#rotate-differently-api-keys-without-downtime) with a grace period |
| **Provider credentials** | Daytona and Freestyle API keys, an OpenSandbox server's `api_key`, a `remote` plugin's bearer token | Only the control plane. Tenants never see them: `GET /v1/capabilities` omits them and the trust report shows references only | One of three references on the pool: `env:NAME` (an environment variable on the server), `file:/path` (a mounted file), or `secret:name` (a value stored in Differently, sealed) | `secret:` and `file:` are picked up live; `env:` needs a restart ([Rotate a provider key live](#rotate-a-provider-key-live)) |
| **The secrets key** | The 32-byte key-encryption key that seals `secret:` values | Only the server process, and your backup | Never in the database: an environment variable (`DIFFERENTLY_SECRETS_KEY` by default) or a file | It cannot be rotated yet ([Back up the secrets key](#back-up-the-secrets-key)) |

The design is [ADR 0021](../adr/0021-provider-credentials-stored-encrypted.md).
It amends [ADR 0008](../adr/0008-secrets-by-reference-only.md), which kept
every credential as a reference. References still work, and you can mix
the three forms across pools.

## The secrets key

Storing a provider credential needs a secrets key on the server. Without
one:

- `PUT /v1/admin/secrets/{name}` answers `409 invalid_state`;
- registering a pool with a `secret:` credential fails the same way;
- `env:` and `file:` credentials work as before.

**With Helm**, the chart provides the key by default (`secrets.enabled: true`):

- It generates 32 random bytes once and stores them, base64-encoded, in the
  Secret `<fullname>-secrets-key` (`dly-differently-secrets-key` for
  release `dly`) under the key `secrets-key`.
- `lookup` keeps that value across upgrades, and the annotation
  `helm.sh/resource-policy: keep` leaves the Secret in place on
  `helm uninstall`.
- The server receives it as `DIFFERENTLY_SECRETS_KEY`.

To bring your own key, create the Secret and point the chart at it:

```sh
kubectl -n differently create secret generic differently-secrets-key \
  --from-literal=secrets-key="$(openssl rand -base64 32)"
```

```yaml
secrets:
  existingSecret: differently-secrets-key
  existingSecretKey: secrets-key        # the default
```

**GitOps renderers that cannot `lookup`** (`helm template`, Argo CD, some
Flux setups) must use `secrets.existingSecret`. Without it, every render
generates a new key, and a sync that replaces the key makes every stored
credential unreadable. `secrets.enabled: false` gives the server no key, so
nothing can be stored.

**Without Helm**, set the key in the server's environment, or point
`secrets.keyFile` in the config file at a file that holds it:

```sh
export DIFFERENTLY_SECRETS_KEY=$(openssl rand -base64 32)
```

```yaml
secrets:
  keyEnv: DIFFERENTLY_SECRETS_KEY   # the default
  # keyFile: /etc/differently-secrets/secrets-key   # wins over keyEnv when set
```

How to check that the server has a key:

- `differently-server preflight` prints
  `secrets key is valid base64 of 32 bytes (or unset)`.
- A server without a key logs
  `no secrets key; provider credentials can be referenced (env:, file:) but not stored`
  at start-up.

The key is read once, at start-up. Stored credentials need the Postgres
store: the memory store loses them on restart, along with everything else.

## Add Daytona or Freestyle with the key stored in Differently

Registering a pool authorizes nothing. Daytona and Freestyle pools are
always `trust: external`, and they receive no work until the global policy
approves them ([Approve external providers safely](#approve-external-providers-safely)).

**Server egress first.** The chart's NetworkPolicy restricts the server's
egress by default. A provider added at runtime needs
`networkPolicy.server.egress.runtimeProviders: true`. That lets the server
reach `networkPolicy.server.egress.providerCIDRs` on port 443, which
Daytona's and Freestyle's public APIs need. Providers enabled through the
chart's `providers.*` values already open this. Without it, the new pool
shows as unavailable with a connection timeout.

### Dashboard

1. **Pools → Add provider.** Choose the kind (Daytona or Freestyle), a pool
   name, the trust (external), the adapter config (for example `apiURL`) and
   the API key. The dashboard stores the key as a secret, then registers the
   pool with that secret as its credential.
2. Check the new pool's health on the Pools page.
3. **Use for new sandboxes** switches new work to it
   ([below](#switch-new-sandboxes-between-providers)).

The Secrets section lists stored credentials by name and fingerprint, with
the pools using each one. From there you can replace a value or delete an
unused secret.

### CLI

With `--api-key-stdin`, the CLI reads the key from stdin, stores it as the
secret `<pool>-key` (or the name given with `--secret`), and then registers
the pool:

```sh
differently admin pool add daytona --adapter daytona --trust external \
  --config apiURL=https://app.daytona.io/api --config target=us --config class=container \
  --region us --max-concurrency 20 \
  --api-key-stdin < ./daytona-api-key.txt          # stored as secret daytona-key
```

Or store the secret first and reference it:

```sh
differently admin secret put freestyle-key --from-file ./freestyle-api-key.txt
differently admin pool add freestyle --adapter freestyle --trust external \
  --config apiURL=https://api.freestyle.sh --max-concurrency 20 --secret freestyle-key
differently admin secret list        # NAME, FINGERPRINT, USED BY, UPDATED, BY
differently admin pool get freestyle | jq '{name, trust, health: .health.status}'
```

The CLI reads secret values from stdin or a file, never from a flag, so they
stay out of shell history and process listings. If `pool add` fails after
the key was stored, the secret stays stored. Retry with `--secret <name>`,
or remove it with `differently admin secret delete <name>`.

### API

```sh
jq -n --rawfile v ./daytona-api-key.txt '{value: $v}' |
  curl -fsS -X PUT "$DIFFERENTLY_URL/v1/admin/secrets/daytona-key" \
    -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' --data-binary @-
# 201 {"name":"daytona-key","fingerprint":"3f9a0c1d2e4b",...}; 200 when it replaces a value

curl -fsS -X POST "$DIFFERENTLY_URL/v1/admin/pools" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"daytona","adapter":"daytona","trust":"external","region":"us","maxConcurrency":20,
       "config":{"apiURL":"https://app.daytona.io/api","target":"us","class":"container"},
       "credentials":{"secret":"daytona-key"}}'
```

- Secret names are lowercase letters, digits, `.`, `_` and `-`, up to 63
  characters. Values are trimmed and may be up to 16 KiB.
- The API never returns a value: `GET /v1/admin/secrets` gives each
  secret's name, fingerprint, who created and changed it and when, and
  `usedBy`, the pools whose credential refers to it.
- A pool that names a missing secret is refused with `400`
  (`secret daytona-key does not exist; store it first ...`).
- `DELETE /v1/admin/secrets/{name}` is refused with `409` while any pool
  refers to the secret.

### Pools in the config file, and existing pools

A pool in the config file can reference a stored secret too, with
`credentials: {secret: daytona-key}`. `differently-server preflight` then
requires a secrets key. The pool is unavailable until the secret exists,
and it becomes usable within 5 seconds of `differently admin secret put`.

To move an API-registered pool from an `env:` reference to a stored secret,
store the secret, then:

```sh
curl -fsS -X PATCH "$DIFFERENTLY_URL/v1/admin/pools/daytona" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"credentialRef":"secret:daytona-key"}'
```

- `credentialRef` must be `env:NAME`, `file:/path` or `secret:name`.
- A pool declared in the config file answers `409`; change its credentials
  in the config file instead. That includes the chart's `opensandbox` and
  `providers.*` pools.
- Other replicas pick the change up within about 5 seconds ([several replicas](#several-server-replicas)).

## Switch new sandboxes between providers

`use` makes one pool the place new sandboxes go:

```sh
differently admin pool use daytona --approve-external   # to Daytona
differently admin pool use opensandbox                  # back to your private pool
```

```sh
curl -fsS -X POST "$DIFFERENTLY_URL/v1/admin/pools/daytona/use" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"approveExternal":true}'
# {"pool":{...},"drained":["opensandbox"],"policy":{...}}   policy only when this call approved the pool
```

How `use` works:

- It undrains the named pool and drains every other enabled pool. Disabled
  pools are left alone.
- The response lists what it drained. It includes the global policy only
  when the call changed it.
- A disabled target answers `409 invalid_state`; enable it first with
  `PATCH /v1/admin/pools/{name}` `{"enabled": true}`.
- An external target needs approval
  ([next section](#approve-external-providers-safely)).
- Switching back is the same call on the other pool.

What moves and what does not:

| | After `use` |
|---|---|
| New sandboxes | Go to the named pool, if policy and classification allow it there |
| Queued sandboxes not placed yet | Are placed on the named pool on the next scheduler tick. Queued work that cannot go there, such as `private-only` work when every private pool is drained, **fails** instead of waiting |
| Running and paused sandboxes | Stay where they are. Exec, files, pause, resume and delete keep working on drained pools, and those sandboxes keep running (and, at a provider, billing) until their TTL or deletion |
| Snapshots, restores and forks | Snapshots stay on the pool that took them. A restore or fork needs that pool, so it is refused with `422 no_eligible_pool` while the pool is drained |
| Applications | Change nothing. They never name a backend, so an application such as OpenWork keeps calling the same API with the same key |

No sandbox is ever moved between providers.

**Switching versus overflow.** Both send work to external pools, in
different ways:

- *Overflow* is a placement policy (`external: overflow` and the pool in
  `externalPools`; see [hybrid-overflow.md](hybrid-overflow.md)). Every pool
  stays open, private pools are preferred, and an approved external pool
  takes work only when no private pool can take it at that moment.
- *Switching* is an operator decision, carried by drain flags rather than
  policy: all new work goes to one pool. With an external target and every
  private pool drained, overflow sends every eligible sandbox to the
  external pool.

If some work must keep running privately while the rest moves, do not
switch, because `use` drains every private pool. Keep the private pools
open and use overflow instead. To move only some tenants, give them an
organization or project policy whose `allowedPools` names only the
external pool.

Other replicas pick the switch up within about 5 seconds
([several replicas](#several-server-replicas)). Then check where new work lands:

```sh
differently admin pool list | jq '.items[] | {name, trust, draining, active, health: .health.status}'
DIFFERENTLY_API_KEY=$PROJECT_KEY differently sandbox create --profile small --wait   # prints sbx_...
DIFFERENTLY_API_KEY=$PROJECT_KEY differently sandbox placement <sbx_id>              # the pool and every candidate's reasons
```

## Approve external providers safely

An external pool receives work only when all of these hold:

- the global policy's `external` is `overflow` or `allowed`;
- the pool is listed in the global policy's `externalPools`;
- nothing narrower forbids it (see below).

`use` on an external pool that the global policy does not approve answers
`409 invalid_state` (`... pass approveExternal to approve it`). With
`approveExternal` (`--approve-external`), it approves the pool in one step,
recorded as a `policy.update` audit event with `approvedExternalPool`:

- it adds the pool to `externalPools`;
- it raises `external` from `never` to `overflow`, and leaves `overflow`
  and `allowed` as they are;
- it stores the result as the global policy.

Three consequences to plan for:

- **The stored global policy replaces `config.policy`** from then on, so
  later edits to `config.policy` in Helm values have no effect. To return
  to the config file's policy, delete the stored one:
  `curl -X DELETE -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/policies/global"`.
- **Switching back does not withdraw the approval.** The external pool stays
  in `externalPools`, drained. If anyone later undrains it, overflow to it
  resumes. To withdraw the approval, edit the global policy. `PUT` replaces
  the whole policy, so start from the current one:

  ```sh
  differently admin trust-report | jq '.globalPolicy | .externalPools -= ["daytona"]' > global.json
  differently admin policy global global.json
  ```

- **Classifications and narrower policies still apply.** `use` checks only
  the global policy:
  - `private-only` work never leaves private pools. With every private pool
    drained, it is refused up front with `422 no_eligible_pool`.
  - `internal` (the default class) may go to an approved external pool, and
    so may `public`.
  - An organization or project policy with `external: never`, `deniedPools`,
    `allowedPools` or a narrower `externalPools` still applies. So does a
    request's `placement.external: never`. While every private pool is
    drained, the affected sandboxes are refused with `no_eligible_pool`.

A safer order for a first approval:

1. Approve the pool deliberately, by editing the global policy (a reviewable
   change), not as a side effect of a switch.
2. Read the trust report:

   ```sh
   differently admin trust-report | jq '{globalPolicy, scopedPolicies,
     pools: [.pools[] | {name, trust, draining, approvedForExternalPlacement, endpoint, credentialRef}]}'
   ```

3. Set `maxConcurrency` on the external pool. Providers report unbounded
   capacity, so this cap is what limits spend.
4. Switch without `--approve-external`. If the pool is not approved, the call
   fails instead of approving it for you.

Afterwards, the trust report's `recentExternalPlacements` lists each sandbox
that ran outside your pools.

## Rotate a provider key live

**A stored secret (`secret:`).** Replace the value. No restart is needed:

1. Create the new key at the provider. If the provider lets an account hold
   two keys at once, keep the old one for now.
2. Store it:

   ```sh
   differently admin secret put daytona-key --from-file ./daytona-api-key-new.txt
   # secret daytona-key stored (fingerprint 7c21e09ab3f4); pools using it switch to it within seconds
   ```

3. The replica that handled the call rebuilds the pool's adapter at once.
   Every other replica does it at its next pool refresh, within about 5
   seconds. Nothing reports which value each replica is using, so wait
   about 10 seconds, then check the pool is healthy:
   `differently admin pool get daytona | jq .health`. The answer comes from
   whichever replica served the request.
4. Revoke the old key at the provider.

If the provider has only one key at a time, which is the case for an
OpenSandbox server's `api_key`, change it at the provider and store the new
value straight away. Calls to the pool fail between the two steps, and the
pool shows as unavailable.

Rotate within the same provider account. The rebuilt adapter uses the new key
for everything, including sandboxes that already exist. A key for another
account cannot see them: the reconciler would mark them lost, and they would
keep running, and billing, in the old account. To change accounts, register a
new pool and [switch](#switch-new-sandboxes-between-providers) to it.

**A mounted file (`file:`).** Every replica re-reads the file every 5 seconds
and rebuilds the adapter when the value changes. This covers a Kubernetes
Secret volume, or a Key Vault CSI mount on Azure
([azure.md](../deploy/azure.md#provider-credentials-with-azure-key-vault)).
The delay is mostly outside Differently:

- the kubelet updates Secret volumes after its sync period, usually within
  a minute or two;
- `subPath` mounts never update;
- the Key Vault CSI driver updates mounts only with secret rotation turned
  on, at its poll interval.

**An environment variable (`env:`).** The value changes only when the server
restarts:
`kubectl -n differently rollout restart deploy/dly-differently-server`. The
chart's `opensandbox` and `providers.*` pools use `env:` references; day-2
operations describe the
[OpenSandbox key rotation](../deploy/day-2.md#token-and-key-rotation).

Only `secret.put` is audited. A value that changes in a file or the
environment changes outside Differently.

## Rotate Differently API keys without downtime

`POST /v1/api-keys/{id}/rotate` with `{"graceSeconds": N}`:

- **The replacement** has the same name, scopes, organization, project and
  lifetime as the old key. A key with a 24-hour TTL gets a new 24 hours from
  now. The new token is returned once.
- **The old key** stays valid for the grace period, at most 604800 seconds
  (7 days). It keeps an earlier expiry if it already had one. With
  `graceSeconds: 0`, or no body, it is revoked at once.
- **Who can rotate:**
  - the caller needs `keys:write`, and must be able to grant every scope the
    key holds; otherwise the call answers `403`;
  - a key in another tenant answers `404`;
  - a revoked or expired key answers `409 invalid_state`: create a new one.
- **Audit:** one `key.rotate` event on the old key, with `replacement` and
  `graceSeconds`. The replacement's `createdBy` is the key that rotated it.

**Example: the OpenWork Workbot runner's project key.** The runner calls
Differently with a project key in `DIFFERENTLY_API_KEY`. Rotate it with an
organization admin key or an operator key:

```sh
differently key list                                  # NAME, ID, PROJECT, SCOPES, EXPIRES, LAST USED, STATE
OLD=key...                                            # the runner's key ID
NEW=$(differently key rotate "$OLD" --grace 1h -o json)
echo "$NEW" | jq '{id, replaces: .replaced.id, oldKeyValidUntil: .replaced.expiresAt}'
echo "$NEW" | jq -r .token                            # put this in the runner's DIFFERENTLY_API_KEY
```

1. Choose a grace period longer than a runner deploy takes.
2. Update the runner's `DIFFERENTLY_API_KEY` wherever it is configured, and
   redeploy or restart the runner.
3. Check that the switch happened. `lastUsedAt` on the old key stops
   advancing (it is updated at most once a minute), and the new key's starts:

   ```sh
   differently key get "$OLD" | jq '{lastUsedAt, expiresAt}'
   differently key get "$(echo "$NEW" | jq -r .id)" | jq .lastUsedAt
   ```

4. Let the grace period lapse, or revoke the old key early with
   `differently key revoke "$OLD"`.

Nothing changes in OpenWork apart from the key. In the dashboard, the Keys
page has the same actions:

- create a key, showing the token once;
- rotate it, with a grace period;
- revoke it.

Creating keys works as before: `differently key create NAME --scope ...
--key-project PROJECT --ttl-hours N` (`POST /v1/api-keys`). A key can grant
only scopes it holds, within its own tenancy, and `differently key list`
shows each key's state and last use.

## The bootstrap operator token

On every start, the server reads `DIFFERENTLY_BOOTSTRAP_TOKEN`
(`auth.bootstrapTokenEnv`) and makes sure an operator key with that ID and
secret exists:

- The token's ID part is the text between `dly_` and the last `_`.
- If no key has that ID, the server creates one, named
  `bootstrap operator`, with the `admin` scope. Audit event: `key.create`,
  actor `bootstrap`.
- If the key exists and the secret differs, the stored hash is replaced and
  the old token stops working. Audit event: `key.rotate`, actor `bootstrap`.
- A revoked bootstrap key stays revoked, whatever the token says.

The chart generates the token once into `dly-differently-tokens`, key
`bootstrap-token`, unless you set `auth.existingSecret`.

**Stop using it day to day.** Create named keys, then revoke the bootstrap
key:

```sh
export DIFFERENTLY_API_KEY=$(kubectl -n differently get secret dly-differently-tokens \
  -o jsonpath='{.data.bootstrap-token}' | base64 -d)
BOOT_ID=$(differently whoami | jq -r .keyId)

differently key create ops-admin --scope admin -o json | jq -r .token     # store in your secret manager
differently key create acme-admins --org "$ORG_ID" --scope org:admin -o json | jq -r .token

export DIFFERENTLY_API_KEY=<the ops-admin token>
differently whoami                                  # operator: true
differently key revoke "$BOOT_ID"
```

Use the `ops-admin` key for operator work: pools, secrets and the global
policy. Give each organization's administrators an `org:admin` key; it
manages projects, keys and policies inside that organization only.

Unsetting the variable does not revoke the key, which stays valid in the
database. Revoking it does, and the revocation holds even though the chart
supplies the same token on every start.

**Break glass.** If every operator key is lost, put a token with a new ID in
the tokens Secret and restart the server. A new operator key is created.
Revoke it once you have made a named key again.

```sh
differently-server token      # prints dly_<new id>_<secret>
```

**Rotate it, if you keep using it:**

- **Same ID, new secret.** Generate a new secret part, keep `dly_<id>_`,
  update the Secret (or `auth.bootstrapToken`), and restart the server. The
  first replica to start replaces the hash. From then on the old token is
  refused by every replica; there is no grace period.

  ```sh
  echo "dly_${BOOT_ID}_$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 40)"
  ```

- **New ID.** A second operator key is created and the old one stays valid.
  Revoke the old one with `differently key revoke <old-id>`.
- **Through the API**, like any key: `differently key rotate "$BOOT_ID" --grace 0`.
  The replacement is an ordinary operator key, not tied to the variable, and
  the token in the Secret stays dead across restarts.

[Day-2 operations](../deploy/day-2.md#token-and-key-rotation) has the
Kubernetes commands.

## Back up the secrets key

If the key is lost or changed, every stored credential becomes unreadable:

- Pools that use `secret:` credentials show as unavailable with
  `secret <name> cannot be opened with this key`.
- The only recovery is to configure a key and store every value again with
  `differently admin secret put`. A `PUT` overwrites the old sealed value
  without opening it.

Back it up when you install, separately from database backups. For example,
to Azure Key Vault:

```sh
kubectl -n differently get secret dly-differently-secrets-key \
  -o jsonpath='{.data.secrets-key}' | base64 -d > secrets-key.txt
az keyvault secret set --vault-name <vault> --name differently-secrets-key --file secrets-key.txt
shred -u secrets-key.txt          # rm -P on macOS
```

- **A restore needs both the database backup and the key.** A database
  restore also rewinds stored values to the backup's state: store again any
  credential rotated since.
- **Uninstalling keeps the chart's Secret.** `helm uninstall` leaves
  `dly-differently-secrets-key` in place, and a reinstall with the same
  release name and namespace reuses it. Deleting the namespace deletes it.
- **Do not rotate the key.** Sealed values carry no key version and there is
  no re-encryption yet
  ([backlog E23](../backlog.md#e23-provider-credential-follow-ups)). If you
  must change it:
  1. Stop every replica
     (`kubectl -n differently scale deploy/dly-differently-server --replicas=0`),
     so no replica keeps sealing with the old key.
  2. Set the new key and start the server again.
  3. Store every value again.
- **If the key lives in Key Vault** and reaches the cluster through the CSI
  driver, do not create new versions of that Key Vault secret, and pin its
  `objectVersion`. A new version reaching the server at its next restart
  makes every stored credential unreadable.

## Audit events

Every action below is in the audit chain, with actor `key:<id>` unless noted.

| Action | Event | Details |
|---|---|---|
| Store or replace a provider credential | `secret.put` (resource: the secret's name) | `fingerprint` |
| Delete a stored credential | `secret.delete` | |
| Add a pool | `pool.create` | `adapter`, `trust`, `credentialRef` |
| Change a pool, including its `credentialRef` | `pool.update` | None: the new reference is not recorded; read it from `GET /v1/admin/pools/{name}` or the trust report |
| Switch new sandboxes to a pool | `pool.use` | `drained` (comma-separated pool names) |
| Approve an external pool while switching | `policy.update` (resource `global`) | `approvedExternalPool` |
| Drain or undrain one pool | `pool.drain`, `pool.undrain` | |
| Create an API key | `key.create` | `scopes`, `name` |
| Rotate an API key | `key.rotate` (resource: the old key) | `replacement`, `graceSeconds` |
| Revoke an API key | `key.revoke` | |
| Bootstrap token creates or re-hashes the operator key | `key.create`, `key.rotate` (actor `bootstrap`) | `scopes`; `revoked` (`1` if the key stays revoked) |

```sh
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" \
  "$DIFFERENTLY_URL/v1/audit-events?action=secret.put" | jq '.items[] | {time, actor, resource, details}'
```

Not recorded:

- the old key expiring at the end of its grace period;
- a credential changing in a mounted file or an environment variable;
- adapter rebuilds.

The trust report's `recentAdminActions` includes `pool.*`, `policy.*` and
`key.*` events, but not `secret.*` events.

## What is verified

Tests on this branch:

| Test | What it shows |
|---|---|
| `TestStoredProviderSecretReachesBackendAndRotatesLive` (`internal/server/secrets_test.go`) | A key stored in Differently reaches a `remote` plugin backend registered through the API. The value is never returned, while the fingerprint is. A rotated value is picked up without a restart, after the backend stopped accepting the old one. A missing secret is refused at registration, deleting a secret in use is `409`, tenant keys get `403` and a bad name gets `400` |
| `TestRotateAPIKey` | The replacement keeps name, project and scopes. The old key works during the grace period and stops at once with no grace. Rotating a revoked key is `409`. A key lacking a scope gets `403`, and another tenant gets `404`. Grace above 7 days is `400` |
| `TestUsePoolSwitchesWhereNewSandboxesGo` | An unapproved external switch is `409`; `approveExternal` approves the pool and raises `never` to `overflow`. New sandboxes land on the chosen pool, `private-only` is refused with `422`, and switching back works. A running sandbox stays where it is |
| `TestSealOpenBindsName`, `TestLoad` (`internal/secrets`) | A value sealed for one name does not open under another or with another key. Nonces are not reused, short keys are rejected, and keys load from the environment and from files |
| Store conformance `Secrets` (`internal/store/storetest`) | Put, replace, list and delete in the memory and Postgres stores; delete is refused while a pool uses the secret |

Not verified:

- live Daytona and Freestyle accounts (their adapters are tested only
  against fakes);
- `file:` pickup from a real Kubernetes Secret volume or Key Vault CSI mount.
  It uses the same code path as stored secrets but has no test of its own;
- more than one server replica on a real cluster (covered only by a unit
  test with two pool managers sharing a store; see
  [several replicas](#several-server-replicas));
- the chart's generated secrets key and `runtimeProviders` egress on a real
  cluster.
