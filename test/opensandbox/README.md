# Live check against a real OpenSandbox server

`compose.yaml` runs OpenSandbox's lifecycle server (release 1.2.0) with the
Docker runtime on any Docker host. `TestLiveOpenSandbox` then drives the
`opensandbox` adapter against it: create, idempotency, exec, files, listing,
pause/resume, snapshot and delete.

```bash
docker compose -f test/opensandbox/compose.yaml up -d
OPENSANDBOX_URL=http://localhost:8090/v1 go test -tags live -v -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
docker compose -f test/opensandbox/compose.yaml down
```

The API key in `config.toml` is a test value. Sandboxes are Docker
containers on the same host, so this checks the API contract, not
isolation.

Verified 2026-10-28 on Colima (`colima-differently`, arm64) with server
1.2.0 and `execd` v1.1.0: passes; sandbox creation 0.5–0.9 s.
