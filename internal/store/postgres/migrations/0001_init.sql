-- Differently control-plane schema. Resources are stored as JSONB documents
-- with the columns needed for locking, filtering and ordering promoted.

CREATE TABLE IF NOT EXISTS meta (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE TABLE IF NOT EXISTS organizations (
    id  text PRIMARY KEY,
    doc jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id     text PRIMARY KEY,
    org_id text NOT NULL REFERENCES organizations (id),
    doc    jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS projects_org ON projects (org_id);

CREATE TABLE IF NOT EXISTS api_keys (
    id         text PRIMARY KEY,
    org_id     text NOT NULL DEFAULT '',
    project_id text NOT NULL DEFAULT '',
    doc        jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS api_keys_org ON api_keys (org_id, project_id);

CREATE TABLE IF NOT EXISTS pools (
    name text PRIMARY KEY,
    doc  jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS policies (
    scope text PRIMARY KEY,
    doc   jsonb NOT NULL
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id              text PRIMARY KEY,
    org_id          text NOT NULL,
    project_id      text NOT NULL,
    pool            text NOT NULL DEFAULT '',
    status          text NOT NULL,
    idempotency_key text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    doc             jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS sandboxes_queue ON sandboxes (status, next_attempt_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS sandboxes_org ON sandboxes (org_id, project_id, status);
CREATE INDEX IF NOT EXISTS sandboxes_pool ON sandboxes (pool, status);
CREATE UNIQUE INDEX IF NOT EXISTS sandboxes_idempotency ON sandboxes (project_id, idempotency_key) WHERE idempotency_key <> '';

CREATE TABLE IF NOT EXISTS snapshots (
    id         text PRIMARY KEY,
    org_id     text NOT NULL,
    project_id text NOT NULL,
    doc        jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS snapshots_org ON snapshots (org_id, project_id);

CREATE TABLE IF NOT EXISTS audit_events (
    seq      bigint PRIMARY KEY,
    org_id   text NOT NULL DEFAULT '',
    resource text NOT NULL DEFAULT '',
    action   text NOT NULL,
    doc      jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_events_org ON audit_events (org_id, seq);

CREATE TABLE IF NOT EXISTS agents (
    id   text PRIMARY KEY,
    pool text NOT NULL,
    doc  jsonb NOT NULL
);
