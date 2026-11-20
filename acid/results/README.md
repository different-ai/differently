# Acid test results

Raw results of the [sandbox acid test](../../docs/acid-test.md), one JSON
file per run (`acid/1` format), under `<provider>/<date>-<setup>.json`.
`differently-acid report acid/results/*/*.json` prints them side by side.

| File | Setup | Result |
|---|---|---|
| [opensandbox/2026-11-20-docker-colima.json](opensandbox/2026-11-20-docker-colima.json) | OpenSandbox server release-1.2.0, Docker runtime, `execd` release-1.2.0, `debian:bookworm-slim`, Colima on Apple Silicon (arm64), runner on the same Mac; full-size run (10-minute command, 100 MiB file) | Level 2 · 21/21 |

A run on a laptop, with the backend on the same machine, says what the
backend does, not how fast it is for anyone else: latency and throughput
here have no network in them. Runs are not yet repeated (the design asks for
five), so the numbers are single samples.
