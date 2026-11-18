-- Templates set through the admin API: one name mapped to each pool's own
-- image or snapshot. Templates declared in the config file are not stored
-- here; the server merges the two and the config file wins a shared name.
CREATE TABLE IF NOT EXISTS templates (
    name text PRIMARY KEY,
    doc  jsonb NOT NULL
);
