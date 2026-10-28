-- An idempotency key names a live sandbox: once that sandbox is terminated or
-- failed, the same key may create a new one.
DROP INDEX IF EXISTS sandboxes_idempotency;
CREATE UNIQUE INDEX IF NOT EXISTS sandboxes_idempotency ON sandboxes (project_id, idempotency_key)
    WHERE idempotency_key <> '' AND status NOT IN ('terminated', 'failed');
