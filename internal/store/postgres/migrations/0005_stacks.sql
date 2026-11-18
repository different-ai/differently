-- Application stacks that Differently watches: the services of a deployment
-- (OpenWork's Den, gateway, Workbot and runner, say) and where to probe them.
CREATE TABLE IF NOT EXISTS stacks (
    name text PRIMARY KEY,
    doc  jsonb NOT NULL
);
