# Sandbox acid test

| | |
|---|---|
| Status | **Design, nothing implemented.** No test below has run. This page is the proposal to build, argue with and change. |
| Goal | One open, reproducible test that any sandbox provider can be run through, getting harder level by level, so we can publish real numbers, file precise issues upstream, and help customers pick a provider for what they actually need. |
| Builds on | The [adapter contract](adapters/specification.md) (`pkg/adapter`), the conformance checklist in [§11](adapters/specification.md#11-adapter-conformance-checklist) and backlog [E0.34](backlog.md#e0-correctness-and-security-fixes-from-the-code-review), [E2.4](backlog.md) (budget-guarded live tests) and the benchmark plan in [reliability-and-performance.md §4](reliability-and-performance.md#4-benchmark-plan) |

## Why an acid test

The browser Acid tests worked because they were public, cumulative and hard
to argue with: one page, one score, every failure a concrete bug a vendor
could fix. Sandbox providers have no equivalent. Each publishes its own boot
time on its own terms, feature tables say "pause" without saying whether
memory survives, and nobody checks that a declared feature works.

Differently already talks to every provider through one contract, so it is
in the right place to run the same test against all of them:

- **For providers:** each failure is a reproducible case with a one-line
  command, ready to become an issue or a pull request.
- **For customers:** a matrix of what each provider really does, plus
  numbers measured the same way, filtered by their workload.
- **For Differently:** the conformance suite E0.34 asks for, live evidence
  for [feature-parity.md](feature-parity.md), and inputs for placement
  (which pool really supports fork, how long a resume takes).

## Principles

1. **Same test, any provider.** The harness drives the
   `adapter.ExecutionAdapter` contract, so every built-in adapter runs it,
   and any other backend runs it behind the [`remote`](providers/remote.md)
   HTTP protocol. Nothing in a test is specific to one provider.
2. **Incremental.** Levels 0–8 go from "does the API answer" to "does it
   survive a day of abuse". A provider's level is the highest level it
   passes completely; partial progress still shows as a score.
3. **Declared versus real.** Every feature check has four outcomes, not two:

   | Outcome | Meaning | Counts as |
   |---|---|---|
   | ✅ pass | Declared and works | Points |
   | ➖ unsupported | Not declared, and the call answers `unsupported_operation` | No points, no penalty |
   | ❌ fail | Declared, and does not work | Failure |
   | ⚠️ dishonest | Not declared but silently does something else, or "succeeds" without doing it (a pause that loses memory, an egress block that blocks nothing) | Failure, flagged in the report |

   The last row is the one customers get hurt by, so it is reported on its
   own.
4. **Capability, speed and cost stay separate.** Pass/fail answers "can it";
   measurements answer "how fast"; cost is an estimate from public prices.
   None is folded into another, so a fast provider cannot buy points with
   speed.
5. **Reproducible or not published.** Pinned suite version, pinned probe,
   recorded environment, raw JSON for every run, at least five repetitions
   for every number, median and p95 published with the spread.
6. **Fair to providers.** Providers see their results and a reproduction
   before publication, can reply, and their reply is published next to the
   result. Isolation failures are disclosed privately first (see
   [Publishing](#publishing-and-working-with-providers)).

## Levels

Each test has an ID (`L3.04`), the features it needs, what passing means,
and what it measures. A test whose feature the adapter does not declare is
checked for the ➖ path instead: the operation must answer
`unsupported_operation`.

### Level 0 — Contract

Does the provider behave like an API a scheduler can rely on? This is the
§11 checklist, made executable.

| ID | Test | Passes when |
|---|---|---|
| L0.01 | Health | `Health` succeeds with valid credentials and fails, as `provider_unavailable`, with bad ones |
| L0.02 | Create and get | A created instance reaches `running`; `Get` reports it |
| L0.03 | Idempotent create | Creating the same ID twice returns the same ref, including when the first response is dropped (the harness drops it in the transport) |
| L0.04 | List ownership | `List(installation)` returns exactly this run's instances, each with its `sandboxId` |
| L0.05 | Idempotent delete | Deleting twice succeeds; `Get` afterwards answers `not_found` |
| L0.06 | Error classes | A bad request is `invalid_request`, an oversized one is refused before creation, and no message contains the credential |
| L0.07 | Honest features | Every undeclared optional operation answers `unsupported_operation` |

**Measures:** time to `running` (cold), API call latency per operation.

### Level 1 — Run a command

| ID | Test | Passes when |
|---|---|---|
| L1.01 | Exit codes | `exit 3` returns exit code 3 as a result, not an error |
| L1.02 | Separate streams | stdout and stderr arrive separately and in full |
| L1.03 | cwd and env | `cwd` and `env` are honoured |
| L1.04 | Timeout | A command past its timeout is stopped and reported as timed out; the sandbox stays usable |
| L1.05 | Bytes, not text | Arbitrary bytes and UTF-8 in output survive unchanged |
| L1.06 | Large output | 10 MiB of output arrives complete (or is cut at a documented limit and says so) |
| L1.07 | Long command | A 10-minute command completes and returns its exit code |
| L1.08 | stdin | Input passed to a command reaches it, or the call answers `unsupported_operation` |

**Measures:** exec round trip for `true` (p50, p95), time to first exec after
create.

### Level 2 — Files

| ID | Test | Passes when |
|---|---|---|
| L2.01 | Round trip | A file written through the API reads back byte-identical (binary included) |
| L2.02 | Seen from inside | A file written through the API is visible to `exec`, and the reverse |
| L2.03 | Awkward paths | Spaces, UTF-8 and nested directories work |
| L2.04 | Listing | `ListFiles` reports names, sizes and directories correctly |
| L2.05 | Large file | A 100 MiB file round-trips, or is refused at a documented limit |
| L2.06 | Missing file | Reading a missing path answers `not_found`, not empty content |

**Measures:** upload and download throughput.

### Level 3 — A real dev box

What a coding agent does in its first minute.

| ID | Test | Passes when |
|---|---|---|
| L3.01 | Requested size | `nproc` and the cgroup memory limit match the requested CPU and memory (or the provider documents its rounding) |
| L3.02 | Disk | The requested disk can be filled to 90 % and written back |
| L3.03 | DNS and egress | DNS resolves and HTTPS to a public host works (where egress is allowed) |
| L3.04 | Install a package | A package manager install completes (apt, pip or npm, whichever the image has) |
| L3.05 | Clone and build | A pinned small repository clones, builds and runs its tests |
| L3.06 | Out of memory | A process that exceeds memory is killed; the sandbox and its API stay up |
| L3.07 | Clock | The clock is within 1 s of the runner's NTP time |

**Measures:** a fixed CPU workload's time per requested vCPU, disk write and
read MB/s, time to clone and build.

### Level 4 — Services

| ID | Test | Passes when |
|---|---|---|
| L4.01 | Streamed exec | Output arrives while the command runs (first byte well before the command ends) |
| L4.02 | Background process | A process started with `nohup … &` is still running after the exec returns |
| L4.03 | Exposed port | An HTTP server inside is reachable from the runner through `ExposePort` |
| L4.04 | WebSocket | A WebSocket through the exposed port stays open for 5 minutes |
| L4.05 | Port access control | A port URL marked private is refused without its token |
| L4.06 | PTY | An interactive shell over a PTY echoes input and handles resize |

**Measures:** time to first streamed byte, exposed-port request latency.

### Level 5 — State

Where providers differ most, and where feature tables say least.

| ID | Test | Passes when |
|---|---|---|
| L5.01 | Stop and start | A file written before stop is there after start |
| L5.02 | Pause keeps memory | A process counting in RAM continues from where it was after resume, with the same PID |
| L5.03 | Pause frees the host | A paused sandbox is not billed or not counted as running, as the provider claims (`lifecycle.pause-checkpoint`) |
| L5.04 | Disk snapshot | A sandbox created from a snapshot has the snapshot's files |
| L5.05 | Memory snapshot | A sandbox created from a memory snapshot has the running process |
| L5.06 | Fork | Five forks of one sandbox start with its state and then diverge independently |
| L5.07 | TTL | A sandbox past its TTL is gone; extending the TTL (`SetExpiry`) keeps it |
| L5.08 | Idle | A sandbox with an idle timeout pauses or stops on its own and wakes on access, where declared |

**Measures:** stop, start, pause and resume time; snapshot size and time;
restore time; fork time for 1 and 5 children.

### Level 6 — Isolation

Run only on sandboxes the tester owns, on accounts the tester pays for. These
probe the boundary from inside; they do not attack the provider.

| ID | Test | Passes when |
|---|---|---|
| L6.01 | Egress deny | With egress denied, outbound connections fail; allowed hosts still work |
| L6.02 | Metadata endpoint | Cloud metadata addresses (`169.254.169.254` and friends) are unreachable |
| L6.03 | Neighbours | Two sandboxes of the run cannot reach each other unless configured to |
| L6.04 | Isolation claim | Evidence matches the declared isolation: a microVM claim shows its own kernel and boot ID per sandbox; a container claim is reported as such |
| L6.05 | No host leaks | No host paths, Docker socket or host credentials are visible inside |
| L6.06 | Process limits | A fork bomb is contained; a neighbour sandbox's exec latency stays within 2× its baseline |
| L6.07 | Noisy neighbour | A neighbour saturating CPU and disk moves the measured sandbox's CPU score by less than 20 % |

**Measures:** neighbour latency and CPU score under load.

### Level 7 — Scale

| ID | Test | Passes when |
|---|---|---|
| L7.01 | Burst | 50 concurrent creates all reach `running`; failures are classified (`insufficient_capacity`, never a generic error) |
| L7.02 | Big burst | 200 concurrent creates: report success rate and time to all running |
| L7.03 | Churn | 1 000 create/exec/delete cycles over an hour leave `List` empty: nothing leaks |
| L7.04 | Rate limits | Throttling answers a classified error and recovers after backing off |
| L7.05 | Capacity honesty | `Capacity`, where reported, predicts refusals |

**Measures:** time to N running for N = 1, 10, 50, 200; failure rate; cost
of the hour.

### Level 8 — Endurance and chaos

| ID | Test | Passes when |
|---|---|---|
| L8.01 | A day long | One sandbox answers exec every minute for 24 h |
| L8.02 | Delete while creating | Deleting during create leaves no instance behind |
| L8.03 | Exec during pause | Exec on a paused sandbox fails cleanly or resumes it, as documented |
| L8.04 | Dropped responses | With 10 % of responses dropped by the transport, retries never create duplicates |
| L8.05 | Repeated pause | 100 pause/resume cycles keep memory every time |

## Scores

- **Level:** the highest level whose every applicable test passes (➖ counts
  as passed for the level, since not offering a feature is honest). Shown as
  "Acid level 5".
- **Score:** points out of the tests in the suite, so "Level 5 · 61/78" says
  both what is solid and how much exists.
- **Profiles** for customers, as weighted subsets of tests:

  | Profile | Weighs |
  |---|---|
  | Coding agent | L1–L3, L4.01–L4.03, L5.01–L5.02 |
  | Untrusted code | L6 above everything else, L1.04, L3.06 |
  | Browser and desktop agent | L4, L5.02, L5.08 |
  | CI runner | L3, L7 |
  | Agent swarms | L5.04–L5.06, L7 |

Speed and cost are published next to the score and never inside it.

## How it runs

```text
differently-acid run --config pools.yaml --pool e2b --max-level 5 \
  --budget-usd 5 --repeat 5 --out results/e2b-2026-11-19.json
differently-acid report results/*.json --format html
differently-acid repro L5.02 --pool e2b      # one test, verbose, for an issue
```

- **One harness, any pool.** `cmd/differently-acid` loads the same pool
  configuration as the server and builds adapters through `internal/wiring`,
  so every provider Differently supports runs with the credentials operators
  already have (`env:`, `file:` or `secret:` references; never values on the
  command line).
- **Probe binary.** Measurements inside the sandbox use one static
  `acid-probe` binary (linux/amd64 and linux/arm64, Go standard library
  only), uploaded with `WriteFile` or, without files, in base64 chunks through
  `exec`. Results then do not depend on what the provider's image ships.
  Package and build tests (L3.04–L3.05) use whatever the image has and record
  it.
- **Image.** Each run states its image: the provider's default ("as
  shipped"), and where custom images are supported, a pinned public
  `differently/acid` image so providers compare on equal footing.
- **Tests are Go.** `internal/acid` holds a registry of tests
  (`ID`, `Level`, `Needs []string`, `Run(ctx, *acid.T)`), like `storetest`.
  `acid.T` creates sandboxes with a run label, records timings and evidence,
  and always deletes; a final sweep lists and deletes anything still carrying
  the run label.
- **Budget guard.** Caps on concurrent sandboxes, sandbox-minutes and
  estimated spend (from a price table in the repository); the run stops and
  cleans up when it would exceed one. Levels 7 and 8 need an explicit flag.
- **Checked against itself.** In CI the suite runs against `fake`, the
  provider fakes and the recorded OpenSandbox runtimes, so a test that is
  wrong fails here before it embarrasses anyone in public. Levels 0–2 against
  the fakes are E0.34's acceptance.
- **Versioned.** The suite is `acid/1`; once results are published its tests
  are frozen. New and harder tests go into `acid/2`, as Acid2 followed Acid1.

### Result format

One JSON document per run, schema `acid/1`:

```json
{
  "suite": "acid/1", "commit": "…", "startedAt": "2026-11-19T10:00:00Z",
  "provider": {"kind": "e2b", "adapterVersion": "…", "region": "…", "image": "…"},
  "runner": {"location": "…", "network": "…"},
  "features": {"declared": ["exec", "files", "lifecycle.pause-resume"]},
  "tests": [
    {"id": "L5.02", "outcome": "pass", "attempts": 5,
     "evidence": "counter 1840 → 1841, pid 412 → 412",
     "metrics": {"pauseSeconds": [1.2, 1.1, 1.3, 1.2, 1.4]}}
  ],
  "cost": {"estimateUSD": 0.42, "priceTable": "2026-11-01"}
}
```

Results live in `acid/results/<provider>/` in the repository, and the
website renders them on an `/acid/` page: the matrix, the profiles, the
measurements with their spread, and for each failure the repro command and
a link to the provider's issue if one is open.

## Publishing and working with providers

1. Run the suite at least twice on different days before publishing.
2. Send the provider its results, the raw JSON and the repro commands, and
   wait 14 days before publishing. Publish their reply next to the result.
3. **Isolation failures (level 6) go to the provider's security contact
   privately**, and are published only after a fix or after 90 days.
4. Every other failure becomes a public issue on the provider's tracker,
   written from `differently-acid repro` output: what was expected, what
   happened, one command to reproduce. Where we can fix it, a pull request.
5. Check each provider's terms for benchmarking clauses before the first
   publication.
6. Rerun on every suite release and when a provider ships a fix; the page
   shows history, so improvement is visible.
7. Providers can run the suite themselves and submit results, published as
   "provider-run" until we reproduce them.

## Plan

| Phase | Delivers | Proves |
|---|---|---|
| 1 | `internal/acid`, `cmd/differently-acid`, levels 0–2, run against fakes and the OpenSandbox recordings in CI | Closes E0.34 |
| 2 | `acid-probe`, levels 3–5, budget guard, JSON results; live runs on OpenSandbox, Daytona and Freestyle (the default build) | First real numbers, kept private |
| 3 | Levels 6–7, `report`, the `/acid/` page; experimental adapters (E2B, Vercel, Morph, Fly, Blaxel, …) | First publication, after the provider review window |
| 4 | Level 8, scheduled reruns on release candidates (E2.4), provider-submitted results | A living leaderboard |

## Open questions

- **Through the adapter or through Differently?** Testing the adapter
  measures the provider plus our adapter; a bug in our adapter would look
  like the provider's. Proposal: test the adapter, and before filing any
  issue upstream, reproduce it with the provider's own SDK or API (the repro
  command prints the raw calls).
- **Adapters that do not expose a provider feature yet** (stdin, PTY, egress
  rules on some providers) would show ➖ for a feature the provider has.
  Proposal: mark these "not wired in Differently" separately, and treat
  them as our backlog, not the provider's gap.
- **Runner location** dominates latency numbers. Proposal: publish latency
  only from runners in a stated cloud region near each provider's region,
  and say so on the page.
- **Who pays** for live runs across 15+ providers, and whether providers
  will grant credits for it.
