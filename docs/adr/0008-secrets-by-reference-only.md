# ADR 0008: Control-plane and provider secrets by reference only

- Status: Accepted (amended by [ADR 0020](0020-opensandbox-replaces-node-agent.md) and [ADR 0021](0021-provider-credentials-stored-encrypted.md)). Sandbox environment values are a stated exception. ADR 0020: the agent token and `auth.agentTokenEnv` no longer exist. ADR 0021: pool credentials may also be stored in Differently, sealed under a key-encryption key that stays out of the database
- Date: 2026-10-28

## Context

The control plane needs a database DSN, a bootstrap operator token, the agent
token and provider API keys. Operators want the configuration in Git or a
ConfigMap and the secrets in their own secret store (Kubernetes Secrets, Vault
agent files, cloud secret managers). Any secret written to the Differently
database is one more copy to protect, back up and rotate.

## Decision

- The config file and the database hold **references**, never values:
  - `store.dsnEnv` (default `DIFFERENTLY_DATABASE_URL`),
    `auth.bootstrapTokenEnv` (default `DIFFERENTLY_BOOTSTRAP_TOKEN`),
    `auth.agentTokenEnv` (default `DIFFERENTLY_AGENT_TOKEN`);
  - pool credentials as `env:NAME` or `file:/path`
    (`config.CredentialRef`), resolved when the adapter is built
    (`internal/wiring/wiring.go` `resolve`).
- Pools registered through `POST /v1/admin/pools` accept the same reference
  form; the stored pool has `credentialRef`, never the key
  (`internal/server/admin.go` `createPool`).
- The tenant view of pools (`GET /v1/capabilities`) omits configuration and
  credential references; the operator trust report shows references only.
- API key secrets are stored as SHA-256 hashes and shown once at creation.
- `differently-server preflight` checks that every reference resolves.

## Consequences

- Rotating a provider key means updating the secret and restarting (or
  re-syncing) the server; the database never needs re-encryption.
- A database dump does not contain provider keys, the agent token or API key
  secrets.
- **Exception: sandbox environment values.** `env` passed at sandbox creation
  is stored in the sandbox record (`store.SandboxRecord.Env`, a JSONB document
  in Postgres) in plaintext, and on Firecracker nodes in the VM's `vm.json`
  (mode 0600). The API never returns values, only `envKeys`. Fork copies the
  parent's environment to the child. Encrypting these values or accepting
  references (`secretRef`) for them is
  [backlog E13](../backlog.md#e13-sandbox-secrets-by-reference).
- Exec request environment variables are passed through and not stored.
- The bootstrap key is re-synced on each start: if the environment value
  changes, the stored hash is replaced and the key is un-revoked
  (`internal/server/server.go` `Bootstrap`). To retire it, create another
  operator key and unset the variable.

## Alternatives considered

- **Encrypted secrets in the database with a master key.** Moves the problem to
  the master key and adds a key-management feature; rejected for control-plane
  secrets. Still a candidate for sandbox environment values.
- **Direct Vault or cloud secret manager integrations.** Deferred; file
  references cover agent-injected secrets without a client dependency.
