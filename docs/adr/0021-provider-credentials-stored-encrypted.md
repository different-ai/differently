# ADR 0021: Provider credentials can also be stored in Differently, encrypted

- Status: Accepted
- Date: 2026-10-28
- Amends: [ADR 0008](0008-secrets-by-reference-only.md). Pool credentials
  may now also be stored sealed in Differently's database. The DSN and the
  bootstrap token stay references only.

## Context

ADR 0008 kept every credential out of the database. A pool's credential
was always a reference (`env:NAME` or `file:/path`) to something on the
server's host. That has a cost for the most common operator task, adding a
provider:

- Adding a Daytona or Freestyle key meant creating a Kubernetes Secret,
  wiring it into the server's environment through Helm values, and
  restarting every replica. Only then could the pool be registered.
- `POST /v1/admin/pools` accepted only references. A pool registered from
  the dashboard was useless until someone with cluster access had done the
  steps above.
- Rotating a provider key also meant changing the Secret and restarting.

Operators asked to add a provider key from Differently itself (dashboard,
CLI or API) without touching Kubernetes Secrets.

## Decision

- **A third credential form, `secret:<name>`.** A pool writes it as
  `credentials: {secret: name}` in config, and the API shows it as
  `credentialRef: secret:name`. The value lives in the `secrets` table
  (migration `0004_secrets.sql`), sealed. `env:` and `file:` keep working
  unchanged.
- **Operator-only endpoints** (`admin` scope; `internal/server/secrets.go`):
  - `PUT /v1/admin/secrets/{name}` with `{value}` stores or replaces a
    value. It answers 201 for a new secret and 200 for a replacement.
    Names match `^[a-z0-9][a-z0-9._-]{0,62}$`. Values are trimmed and at
    most 16 KiB.
  - `GET /v1/admin/secrets` lists each secret's name, fingerprint, who
    created and last changed it, and the pools whose credential refers to
    it. It never returns values.
  - `DELETE /v1/admin/secrets/{name}` answers `409 conflict` while any pool
    refers to the secret.
  - Registering a pool with a `secret:` reference that does not exist fails
    with `400`, so a typo shows up at registration, not at first use.
- **Sealing** (`internal/secrets`). Values are sealed with AES-256-GCM:
  - a random nonce for every seal;
  - the secret's name bound in as associated data, so a sealed value cannot
    be copied to another name;
  - a 32-byte key-encryption key, base64-encoded, read at start-up from
    `secrets.keyEnv` (default `DIFFERENTLY_SECRETS_KEY`) or from the file
    named by `secrets.keyFile` (the file wins when both are set).

  The key never enters the database.
- **Without a key, nothing can be stored.** The server logs that it has no
  secrets key and keeps running. `PUT` answers `409 invalid_state`, and so
  does registering a pool with a `secret:` reference. `env:` and `file:`
  references work as before. `differently-server preflight` checks the key's
  format, and it checks that a key exists when a config-declared pool uses
  `secret:`.
- **A fingerprint identifies a value without revealing it:** the first six
  bytes of its SHA-256, as 12 hex characters. It appears in listings and in
  the `secret.put` audit event.
- **Changed values are picked up live** (`internal/pools`). The pool manager
  fingerprints each pool's resolved credential whenever it refreshes pools,
  which every replica does every 5 seconds. When the fingerprint changes,
  it rebuilds that pool's adapter. The replica that handles a `PUT` rechecks
  at once.
  - This covers `secret:` and `file:` credentials, so a rotated Kubernetes
    Secret volume or Key Vault CSI mount needs no restart either.
  - `env:` values change only when the process restarts.
- **Moving a pool to another credential.** `PATCH /v1/admin/pools/{name}`
  accepts `credentialRef` for pools registered through the API. Pools
  declared in the config file change it there.
- **Audit.** `secret.put` (with the fingerprint) and `secret.delete`.

Not changed by this record:

- The database DSN and the bootstrap token are still references.
- Sandbox environment values are still stored in plaintext, ADR 0008's stated
  exception ([backlog E13](../backlog.md#e13-sandbox-secrets-by-reference)).

## Consequences

- **The key-encryption key is the new crown jewel.**
  - Anyone holding both the key and a copy of the database can read every
    stored credential.
  - Losing the key makes every stored credential unreadable. There is no
    recovery: an operator re-enters each value.
  - Back the key up separately from database backups, for example in Azure
    Key Vault.
- **A database dump alone reveals no stored credential.** It holds names,
  fingerprints, timestamps, the actors who changed each secret, and sealed
  bytes. The fingerprint is an unsalted, truncated hash. It can confirm a
  guessed value but does not reveal one; provider API keys are high-entropy.
- **A compromised server process can read every credential it can use**, as
  it could before with environment variables. Storing credentials does not
  narrow what the control plane holds at run time.
- **Rotation without restarts.** Replacing a stored value, or the file behind
  a `file:` reference, reaches every replica within seconds. Restoring a
  database backup rewinds stored values to their state at backup time.
- **The key itself cannot be rotated yet.** Sealed values carry no key
  version, and there is no re-encryption command. Changing the key makes
  every existing value unopenable until it is stored again.
- **Credentials can now live in two places**, the operator's secret store or
  Differently. The trust report keeps showing references only
  (`secret:daytona-key`, `env:...`), never values.
- **With the memory store**, stored secrets are lost on restart, like all
  other state.

## Alternatives considered

- **References only (ADR 0008 as it was).** It keeps the database free of
  credentials. But every new provider needs cluster access, a Secret, a
  values change and a restart, and the dashboard cannot add one. Kept as
  the default for operators who prefer it: `env:` and `file:` are
  unchanged, and with no key configured nothing can be stored.
- **Writing Kubernetes Secrets from the server.** Rejected for three
  reasons. The server would need RBAC to create and patch Secrets in its
  namespace; today no component calls the Kubernetes API, and the chart
  does not mount a service account token. It would work only on
  Kubernetes. And pods would still restart, or remount volumes, before
  seeing the value.
- **An external vault integration** (Azure Key Vault, HashiCorp Vault, cloud
  secret managers) as the resolver behind `secret:`. Deferred, not rejected:
  `secret:` is the place a vault-backed resolver can plug in later
  ([backlog E23](../backlog.md#e23-provider-credential-follow-ups)). Until
  then, vault-held values reach the server as `file:` mounts through the
  vault's CSI driver.
