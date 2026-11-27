-- The same tables as the postgres driver: one row per record, the record as
-- JSON in doc, and beside it only the columns queries filter or lock on.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE organizations (
    id  TEXT PRIMARY KEY,
    doc TEXT NOT NULL
);
CREATE TABLE projects (
    id     TEXT PRIMARY KEY,
    org_id TEXT NOT NULL REFERENCES organizations (id),
    doc    TEXT NOT NULL
);
CREATE INDEX projects_org ON projects (org_id);
CREATE TABLE api_keys (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    doc        TEXT NOT NULL
);
CREATE INDEX api_keys_org ON api_keys (org_id, project_id);
CREATE TABLE pools (
    name TEXT PRIMARY KEY,
    doc  TEXT NOT NULL
);
CREATE TABLE policies (
    scope TEXT PRIMARY KEY,
    doc   TEXT NOT NULL
);
CREATE TABLE secrets (
    name TEXT PRIMARY KEY,
    doc  TEXT NOT NULL
);
CREATE TABLE stacks (
    name TEXT PRIMARY KEY,
    doc  TEXT NOT NULL
);
CREATE TABLE templates (
    name TEXT PRIMARY KEY,
    doc  TEXT NOT NULL
);
-- next_attempt_at is microseconds since the Unix epoch, UTC.
CREATE TABLE sandboxes (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    pool            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    doc             TEXT NOT NULL
);
CREATE INDEX sandboxes_queue ON sandboxes (status, next_attempt_at) WHERE status = 'queued';
CREATE INDEX sandboxes_org ON sandboxes (org_id, project_id, status);
CREATE INDEX sandboxes_pool ON sandboxes (pool, status);
CREATE UNIQUE INDEX sandboxes_idempotency ON sandboxes (project_id, idempotency_key)
    WHERE idempotency_key <> '' AND status NOT IN ('terminated', 'failed');
CREATE TABLE snapshots (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    project_id TEXT NOT NULL,
    doc        TEXT NOT NULL
);
CREATE INDEX snapshots_org ON snapshots (org_id, project_id);
CREATE TABLE audit_events (
    seq      INTEGER PRIMARY KEY,
    org_id   TEXT NOT NULL DEFAULT '',
    resource TEXT NOT NULL DEFAULT '',
    action   TEXT NOT NULL,
    doc      TEXT NOT NULL
);
CREATE INDEX audit_events_org ON audit_events (org_id, seq);
