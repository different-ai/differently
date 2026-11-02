# Capacity: slots, paused sandboxes, preemption and lifecycle limits

| | |
|---|---|
| Status | Describes the code as of 2026-10-29 |
| Code | `internal/scheduler/capacity.go`, `internal/store/store.go` (`HoldsPoolSlot`, `ReclaimSlot`), `internal/capsim` (the model) |
| Tests | `TestPausedSandboxGivesItsSlotBack`, `TestPreemptionMakesRoom`, `TestLifecycleLimits` (server), `TestVictimsOrder` (scheduler), `ReleasedSlotsAndReclaim` (store conformance, memory and Postgres), `TestStrategiesCompare` (model) |

A pool's `maxConcurrency` is a number of **slots**. This page defines who
holds them, how a slot moves from one sandbox to another, and what that buys,
with numbers from a model of real usage.

## 1. Who holds a slot

| Status | Holds a slot |
|---|---|
| provisioning, running, stopped, terminating | yes |
| paused, on a pool whose pause keeps the sandbox on the host (OpenSandbox Docker, Daytona) | yes |
| paused, on a pool that **checkpoints** on pause (`lifecycle.pause-checkpoint`: OpenSandbox Fast Sandbox, Freestyle) | **no**: the pause wrote memory and disk out and left the host (`slotReleased: true`) |
| queued, terminated, failed | no |

Before this change every paused sandbox held its slot. On a checkpointing
pool that is waste: a paused Fast Sandbox microVM uses no CPU or memory on the
node, only artifact storage, yet four of them filled a four-slot pool for the
14 days OpenWork keeps a computer.

The slot is free when the pause **finishes**, not when it is asked for. A
Fast Sandbox checkpoint of a 4 GiB desktop took a median 219 s with templates
as Fast Sandbox 1.2.0 caches them and 182 s with sparse templates, on an
Azure test host ([research](../research/fast-sandbox-pause-time.md)).

## 2. Taking a slot back: resume and the resume queue

Resuming a sandbox that released its slot takes one again, atomically with
the pool's concurrency check (`ReclaimSlot`, under the same pool lock as
activation):

- **A free slot:** the resume proceeds as before (200).
- **A full pool:** the resume is queued (`resumeRequestedAt`) and the API
  answers **202** with the sandbox still paused and a reason (`waiting for a
  free slot on pool …`). Clients wait with
  `GET /v1/sandboxes/{id}/wait?status=running`.
- **A call that wakes an idled sandbox** (exec, files, ports, keepalive)
  queues the same way and waits up to 60 s; after that it answers
  `insufficient_capacity` (503) and the resume stays queued.

Queued resumes are served every tick: higher `priority` first, then oldest
request. A queued resume cannot overflow to another pool: its memory is on the
pool that took the checkpoint. Creates still overflow to approved external
pools under `policy.external: overflow`, as before.

A backend can wake a sandbox on its own (Fast Sandbox's gateway wakes a
microVM on a request to its port URL, without asking Differently). The
reconciler follows it back to running, which takes a slot; a full pool then
shows as **overcommitted** (`differently_pool_overcommitted`) until a pause or
preemption frees one.

## 3. Making room: preemption

A pool can let the scheduler pause running sandboxes when work waits for one
of its slots: queued resumes pinned to it, and queued creates whose placement
found it eligible but at its limit with no pool ready.

```yaml
pools:
  - name: opensandbox-microvm
    maxConcurrency: 4
    preemption:
      mode: idle          # off (default) | idle | lru
      idleSeconds: 60     # mode idle: no exec, file, port or keepalive call for this long
      minRunSeconds: 600  # mode lru: never a sandbox that ran less than this
```

Also `PATCH /v1/admin/pools/{name}` with `{"preemption": {...}}`.

| Mode | Candidates | Order |
|---|---|---|
| `off` | none | |
| `idle` (recommended) | running, preemptible, idle for `idleSeconds` | least recently active, then lowest priority, then longest running |
| `lru` | running, preemptible, ran for `minRunSeconds`, busy or not | same |

Rules:

- One victim per waiting sandbox, counting victims still pausing, so a
  three-minute checkpoint is not answered with a second preemption.
- Only pools that checkpoint on pause preempt: elsewhere a pause frees nothing.
- A sandbox created with `"preemptible": false` (or patched so) is never a
  victim.
- The victim is paused with its memory and marked idled: its next call
  resumes it, through the queue if the pool is still full. Its reason says
  why (`paused to make room on pool …`); the audit event is
  `sandbox.preempted`.

## 4. Lifecycle limits

Two limits complete the idle timeout. Each takes a request value, a
`PATCH` value or the operator default (`defaults.maxRunSeconds`,
`defaults.maxPausedSeconds`); `-1` turns it off; at least 60 when set.

| Limit | Effect |
|---|---|
| `idleTimeoutSeconds` (existing) | pause (or stop) after this long without activity |
| `maxRunSeconds` | pause (or stop) after running this long without a break, busy or not; the next call resumes it. A long runner gives its slot back periodically |
| `maxPausedSeconds` | delete after staying paused or stopped this long, freeing its storage |
| `ttlSeconds` (existing) | delete after this long, whatever its state |

They run on the reconcile interval. Audit events: `sandbox.max_run`,
`sandbox.expired` (with `reason: maxPausedSeconds`).

## 5. What it buys: the model

`internal/capsim` simulates one pool under four strategies with
OpenWork-Workbot-like users:
- each user owns one long-lived computer, created at their first session;
- about six sessions a day, between 09:00 and 17:00;
- sessions are log-normal, median 12 min;
- computers pause after 5 min idle;
- a pause takes 182 s (measured, see above) and a resume 2 s;
- creates that find no slot overflow to an external pool.

| Strategy | Meaning |
|---|---|
| `hold` | paused computers keep their slot (before this change) |
| `release` | paused computers release their slot; resumes queue |
| `preempt-idle` | release, plus preemption mode `idle` (60 s) |
| `preempt-lru` | release, plus preemption mode `lru` (600 s) |

Four slots, five simulated days, the average of five seeds
(`go run ./cmd/differently-capsim -slots 4 -users 6,8,12,16,24`, pause 182 s):

| users | strategy | users on the pool | external computer-hours | slot utilization | wait p50 | wait p95 | preemptions/day | interrupted sessions/day |
|---:|---|---:|---:|---:|---:|---:|---:|---:|
| 6 | hold | 67% | 17 | 6% | 2 s | 4 s | 0.0 | 0.0 |
| 6 | release | 100% | 0 | 8% | 2 s | 13 s | 0.0 | 0.0 |
| 6 | preempt-idle | 100% | 0 | 8% | 2 s | 13 s | 0.2 | 0.0 |
| 6 | preempt-lru | 100% | 0 | 8% | 2 s | 33 s | 0.2 | 0.2 |
| 8 | hold | 50% | 36 | 6% | 2 s | 2 s | 0.0 | 0.0 |
| 8 | release | 95% | 4 | 10% | 2 s | 56 s | 0.0 | 0.0 |
| 8 | preempt-idle | 95% | 4 | 10% | 2 s | 56 s | 0.8 | 0.0 |
| 8 | preempt-lru | 95% | 4 | 10% | 2 s | 120 s | 1.1 | 0.8 |
| 12 | hold | 33% | 76 | 6% | 2 s | 2 s | 0.0 | 0.0 |
| 12 | release | 87% | 15 | 14% | 2 s | 439 s | 0.0 | 0.0 |
| 12 | preempt-idle | 87% | 15 | 14% | 2 s | 250 s | 6.6 | 0.0 |
| 12 | preempt-lru | 87% | 15 | 14% | 2 s | 344 s | 10.2 | 7.6 |
| 16 | hold | 25% | 120 | 6% | 2 s | 2 s | 0.0 | 0.0 |
| 16 | release | 74% | 42 | 15% | 2 s | 571 s | 0.0 | 0.0 |
| 16 | preempt-idle | 74% | 42 | 15% | 2 s | 374 s | 12.0 | 0.0 |
| 16 | preempt-lru | 74% | 42 | 15% | 4 s | 476 s | 16.9 | 12.8 |
| 24 | hold | 17% | 195 | 6% | 2 s | 2 s | 0.0 | 0.0 |
| 24 | release | 58% | 97 | 17% | 2 s | 650 s | 0.0 | 0.0 |
| 24 | preempt-idle | 58% | 97 | 17% | 2 s | 417 s | 19.7 | 0.0 |
| 24 | preempt-lru | 58% | 97 | 17% | 2 s | 483 s | 26.5 | 19.9 |

"Wait" is from a session's start until its computer runs, over every session.
Utilization counts slot time used by a session in progress.

What it shows:

- **Releasing paused slots multiplies what a pool serves.** At 12 users the
  same four slots keep 87% of computers private instead of 33%, and overflow
  falls from 76 to 15 computer-hours. Utilization still stays low, because
  people work in bursts; more of the slots' idle time now goes to someone.
- **The price is a queue.** Once the pool is oversubscribed, a returning user
  can wait for a slot, and slots free only after a checkpoint (about three
  minutes). Holding never queues, because it overflows instead, and pays for
  it in external hours.
- **Idle preemption shortens that queue by about 35–45% and never interrupts
  work.** It reclaims slots from computers idle for a minute, four minutes
  before their own idle pause would.
- **LRU preemption is a poor default.** It interrupts 8–20 sessions a day at
  12–24 users for a smaller gain. It is there for pools where `priority` must
  win.

How pause time moves the wait (`-pause`; 216 s and 182 s measured before and
after sparse templates, 90 s and 30 s what the upstream changes in the
[research](../research/fast-sandbox-pause-time.md#remaining-ideas-that-need-an-upstream-change)
could reach):

| users | pause | release: wait p95 | preempt-idle: wait p95 | external computer-hours |
|---:|---:|---:|---:|---:|
| 8 | 216 s | 88 s | 89 s | 4 |
| 8 | 182 s | 56 s | 56 s | 4 |
| 8 | 90 s | 9 s | 9 s | 3 |
| 8 | 30 s | 2 s | 2 s | 3 |
| 12 | 216 s | 459 s | 277 s | 17 |
| 12 | 182 s | 439 s | 250 s | 15 |
| 12 | 90 s | 361 s | 158 s | 12 |
| 12 | 30 s | 329 s | 130 s | 10 |
| 16 | 216 s | 564 s | 396 s | 46 |
| 16 | 182 s | 571 s | 374 s | 42 |
| 16 | 90 s | 586 s | 375 s | 32 |
| 16 | 30 s | 555 s | 339 s | 27 |

- **A faster pause empties a light queue.** At 8 users the p95 wait falls
  from 88 s to 56 s with sparse templates, and to seconds below 90 s.
- **On a busy pool idle preemption matters more.** At 12 users most of the
  wait is slots held by running computers that are not yet idle: halving the
  pause takes `release` from 439 s to 361 s, while preemption takes it to
  250 s, and to 158 s with the shorter pause. Faster pauses also keep more
  users private (external hours 17 → 12 at 12 users, 46 → 32 at 16).

Choosing:

- Size the slots so the p95 wait you accept holds at your user count.
- Use `release` (automatic on checkpointing pools) with preemption `idle`.
- Keep an external overflow pool for creates, so new users never queue.
- Watch `differently_resume_wait_seconds`, `differently_pause_seconds` and
  `differently_pool_slots` in production to check the model against reality.

At six users on four slots, OpenWork's case on Azure today, everything stays
private with a 13 s p95.

Limits of the model:
- one computer per user;
- no time zones;
- pause and resume times are constants (measured on Azure, see
  [opensandbox-pools.md](../guides/opensandbox-pools.md#measured-on-a-workbot-desktop));
  in reality pauses on one node share its disk, so two at once each take
  longer, and a burstable disk that runs out of credits slows every pause
  ([research](../research/fast-sandbox-pause-time.md));
- external pools are unbounded;
- the scheduler's real tick and the backend-woken case are not modeled.

## 6. Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `differently_pool_slots` | pool, state = held, released, waiting_resume, limit | slot use per pool, every tick |
| `differently_pool_overcommitted` | pool | slots held beyond `maxConcurrency` (backend wakes) |
| `differently_resumes_queued_total` | pool | resumes that had to wait |
| `differently_resume_wait_seconds` | pool | how long they waited |
| `differently_pause_seconds` | pool | how long pauses Differently asked for took on the backend: on a checkpointing pool, the time until the slot is free (capsim's `PauseTime`). A backend's own idle pause (Fast Sandbox native idle) is not timed |
| `differently_preemptions_total` | pool, mode | sandboxes paused to make room |
| `differently_lifecycle_actions_total` | action = preempted, max_run, max_paused; pool | the scheduler's own pauses and deletions |

## 7. Compatibility

- Existing sandboxes are not preemptible (the field defaults to true only for
  new ones), and nothing preempts unless a pool sets a policy.
- A client that resumed a sandbox and expected 200 can now get 202 on a full
  checkpointing pool. OpenWork's adapter already waits for `running` after a
  resume.
- Tenant quotas are unchanged: a paused sandbox still counts toward
  `maxActiveSandboxes`.
