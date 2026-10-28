-- Node agents were removed: OpenSandbox (or another backend) now runs
-- sandboxes, so the control plane no longer tracks per-node agents.
DROP TABLE IF EXISTS agents;
