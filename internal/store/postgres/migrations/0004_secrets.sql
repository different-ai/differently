-- Provider credentials stored in Differently, sealed with AES-256-GCM under a
-- key-encryption key that never enters the database (internal/secrets).
CREATE TABLE IF NOT EXISTS secrets (
    name text PRIMARY KEY,
    doc  jsonb NOT NULL
);
