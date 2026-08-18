# Scheduling

The **Scheduler** (`crewlet.schedule`) lets an agent — or a whole unit —
own **recurring work** without an external cron emitting webhooks into
the engine. A QA Engineer can run a smoke-test pipeline every morning, a
team can hold an async standup at 9:30, a Knowledge-Base agent can audit
Confluence weekly — all declared in the YAML org config.

Schedules are a first-class part of the org model: they hang off a
**Role** (`Role.schedules`) or an **OrgUnit** (`OrgUnit.schedules`), share
one [`Schedule`](#the-schedule-model) model, and hot-reload with the rest
of the org via `engine.reload_config()`.

---

## How it works

A single asyncio loop ticks on a short interval (default 10s). Each tick:

1. Enumerates every `Schedule` across the **live** org (all roles and all
   units — read fresh each tick, so hot-reload is free).
2. Works out which schedules are **due** since the previous tick.
3. Resolves the **runner(s)** for each due fire.
4. Claims the fire in the `scheduled_runs` ledger (at-most-once) and
   publishes a `TaskAssigned` to each runner's inbox topic
   (`crewlet.agent.{handle}.inbox`).

Because it reuses the existing `TaskAssigned` event, the agent runtime
path is **unchanged** — a scheduled turn runs Plan → Execute → Review like
any other, and the **full learning loop** (episodes, diary, reflection)
runs on it. Periodic work is *more* worth learning from, not less.

```
Scheduler loop (every tick_seconds)
   │  read live org via org_provider
   ├── role.schedules ───────────────► run as that role
   └── unit.schedules ──► target ──┬── each  → every direct agent member (default)
                                   └── lead  → the unit's effective lead
        │
        ▼
   claim scheduled_runs (composite PK dedup)  ── already claimed? skip
        │ claimed
        ▼
   publish TaskAssigned → crewlet.agent.{handle}.inbox  (+ ScheduledTaskFired)
        │
        ▼
   normal Plan → Execute → Review turn (with a hard wall-clock cap)
```

---

## The `Schedule` model

```yaml
schedules:
  - name: morning-smoke        # unique within the role/unit; part of the idempotency key
    cron: "0 9 * * 1-5"        # standard 5-field cron (see below)
    timezone: Europe/Amsterdam # IANA tz; falls back to scheduling.default_timezone
    task: "Run the smoke-test pipeline and triage failures"
    target: each               # unit schedules only — each | lead
    enabled: true              # set false to keep it in config without firing
    timeout_seconds: 180       # hard wall-clock cap on the turn (default 180 s —
                               #   roomy for a multi-tool turn, stops runaway loops)
    catchup: true              # fire one recent missed tick on restart (see Catchup)
```

| Field | Default | Meaning |
|-------|---------|---------|
| `name` | — (required) | Identifier, unique within the role/unit. Renaming lets a same-minute fire re-run once. |
| `cron` | — (required) | 5-field cron expression, evaluated in `timezone`. |
| `task` | — (required) | The task prompt handed to the runner agent. |
| `timezone` | `scheduling.default_timezone` | IANA timezone the cron is evaluated in. |
| `target` | `each` | **Unit schedules only.** Who runs it (see [Delivery](#delivery-who-runs-it)). Ignored for role schedules. [Human seats](humans-in-the-org.md) never run schedules: `each` fans out to direct agent roles only, an enabled `lead` schedule under a (possibly inherited) human lead is a config error, and human seats cannot define role schedules. |
| `enabled` | `true` | `false` keeps the schedule in config but never fires it. |
| `timeout_seconds` | `180` | Hard wall-clock cap on the scheduled turn. |
| `catchup` | `true` | Whether to fire a recent missed tick on (re)start. |

Cron / timezone / target validity is checked at **config load**, so a bad
expression fails `crewlet validate` rather than silently at 9am.

### Cron syntax

Standard five fields — `minute hour day-of-month month day-of-week` —
with `*`, ranges (`1-5`), steps (`*/15`, `0-30/10`), lists (`9,17`), and
three-letter month / day names (`JAN`, `MON`). Day-of-week accepts `0-7`
where both `0` and `7` are Sunday. **Vixie semantics:** when *both*
day-of-month and day-of-week are restricted, a day matches if **either**
matches.

```
"30 9 * * 1-5"   09:30, Monday–Friday
"0 9,17 * * *"   09:00 and 17:00 every day
"*/15 * * * *"   every 15 minutes
"0 14 * * 5"     14:00 every Friday
"0 2 1 * *"      02:00 on the 1st of each month
```

---

## Role vs unit schedules

```yaml
units:
  - name: Backend
    type: team
    lead: Backend Lead
    slack_channel: C_BACKEND
    schedules:
      # target defaults to `each` → every direct member posts their own update
      - name: daily-standup
        cron: "30 9 * * 1-5"
        timezone: Europe/Amsterdam
        task: "Post your standup: shipped yesterday / on today / blockers."
      # opt-in lead-coordinated variant (gather + summarize)
      - name: weekly-report
        cron: "0 16 * * 5"
        timezone: Europe/Amsterdam
        target: lead
        task: "Collect the week's progress from your reports and post a summary to the team channel."
    roles:
      - name: Backend Lead
      - name: Backend Dev
        schedules:                       # role-scoped — runs as this role
          - name: morning-smoke
            cron: "0 9 * * 1-5"
            task: "Run the smoke-test pipeline and triage failures"
```

A **role** schedule always runs as that role (its `target` is ignored). A
**unit** schedule resolves its runner(s) from `target`.

Runners are resolved from the **org**, never from the agents running in
the ticking process. A fire is addressed to the runner seat's inbox and
consumed by whichever node owns that seat — which is rarely the node
whose tick won the ledger claim. The seat's agent id comes from
`Organization.agent_id_for`, the same `uuid5` over `(org name, handle)`
every node derives, so the `TaskAssigned` a scheduler publishes names
exactly the identity the turn will run under.

> **Schedules are not inherited.** Unlike `lead` and `slack_channel`, a
> schedule on a department does **not** cascade to child units — that
> would silently multiply a standup across every squad. Declare schedules
> explicitly on each unit that should run them.

### Delivery: who runs it

| `target` | Runner(s) | Use it for |
|----------|-----------|------------|
| `each` (**default**) | every **direct** role of the unit | async standups, "everyone files their own status" |
| `lead` | the unit's effective lead (`get_effective_lead`) | gather-and-post standups, weekly reports, "review overnight Jira for the team" |

These are the only two unit-schedule targets. There is intentionally no
"specific role" target: a static role pin is exactly what a **role
schedule** is, so to run a recurring task as one person, put a schedule
on that role. `lead` is kept because it's *dynamic* — it follows whoever
leads the unit (including inherited leads), so the schedule survives a
leadership change untouched.

`each` fans out **one independent task per member** — each with its own
dedup identity — so a slow or failing member never blocks the others.
`each` resolves **direct** roles only, never descendant units.

A standup is just a scheduled task: the runner does any fan-in with the
colleague tools it already has (`a2a_ask`, Slack reads, the roster). There
is no dedicated "standup" primitive — the same shape covers retros, sprint
kickoffs, demo-prep, on-call handoffs, and [manager 1:1s](one-on-ones.md)
(an `each` schedule where each report opens a private A2A review with its
manager).

---

## Guarantees

### At-most-once

Every fire is claimed in the `scheduled_runs` table before publishing.
The dedup identity is a **composite PRIMARY KEY** and the claim is
`INSERT … ON CONFLICT DO NOTHING`, so a restart, a slow tick, or a
re-evaluated minute can never fire the same run twice.

```
PRIMARY KEY (scope_type, scope_id, schedule_name, fire_label, target_handle)
```

`fire_label` is the schedule's **local wall-clock** stamp
(`YYYYmmddTHHMM` in its timezone), not the UTC instant — so the dedup is
DST-correct (see [DST & cron edge cases](#dst--cron-edge-cases)) and a
`:` in any name/handle can't collide two fires (the identity is the
column tuple, not a joined string).

`scheduled_runs` is a **dispatch ledger**, not a turn-outcome store:
`outcome` is `fired` or `skipped_catchup`. The downstream turn result
(done / failed / timed-out) lives in the normal turn telemetry
(`TaskStarted` / `TaskCompleted` / `TaskFailed`, `TurnGuardBreach`) keyed
by the same trace.

> Scheduling **requires a database** — the ledger is what makes
> at-most-once delivery survive restarts. With no database configured the
> scheduler logs a disabled notice and does not start.

### Missed-tick catchup

Catchup is evaluated only on the **first tick after (re)start**. If the
engine was down across a scheduled fire, the **single most-recent** missed
fire is run if it falls inside the catchup window — *half the schedule's
period, clamped to `[catchup_min_seconds, catchup_max_seconds]`* (default
120s–7200s). Older misses are never backfilled; a missed fire outside the
window is recorded as `skipped_catchup` for audit. Set `catchup: false` on
a schedule to opt out entirely.

### Hard wall-clock timeout

Each scheduled turn carries `timeout_seconds` (default 180). The turn
engine wraps the run in that cap; on breach the turn terminates as
`failed` with a `scheduled_timeout` `TurnGuardBreach` and a `TaskFailed`,
so a runaway loop can't monopolise the runner. A failing or timed-out run
never blocks the next tick.

---

## Configuration

System-level knobs live under a top-level `scheduling:` block:

```yaml
scheduling:
  enabled: true              # master switch
  tick_seconds: 10           # scheduler poll interval
  default_timezone: UTC      # used by any Schedule without its own timezone
  jitter_seconds: 0          # max per-schedule deterministic spread (see below)
  catchup_min_seconds: 120   # lower clamp on the catchup window
  catchup_max_seconds: 7200  # upper clamp on the catchup window
```

The scheduler **auto-enables** when `enabled` is true, a database is
configured, and the org actually declares at least one schedule — orgs
with no schedules never spin up the tick loop.

### Thundering herd (jitter)

When many schedules share a popular minute (everyone writes
`0 9 * * *`), they all become due at once. The `ConcurrencyController`
already queues the burst fairly, so this is a smoothing concern, not a
correctness one. Set `scheduling.jitter_seconds` to a non-zero value to
spread firing: each schedule gets a **deterministic** offset in
`[0, jitter_seconds]` derived from its scope + name, so the 9am wave is
fanned out across that window. The canonical fire minute still forms the
idempotency key, so dedup is unaffected. Default `0` fires exactly on the
minute.

---

## Observability

- **Dashboard.** The **Schedules** view lists every configured schedule
  (scope, cron, timezone, target → resolved runners, next run) and the
  recent dispatch ledger. It's backed by `GET /schedules`, which serves
  the resolved schedule list (computed once at startup), per-request
  next-run times, and the most recent `scheduled_runs` rows. On the
  embedded-API path the view refreshes live when a `ScheduledTaskFired`
  event arrives.
- **`ScheduledTaskFired`** event (`crewlet.events.scheduled_task_fired`) is
  emitted per dispatch with `scope_type`, `scope_id`, `schedule_name`,
  `target_handle`, and `scheduled_at` — surfaced in the dashboard / event
  store.
- The `scheduled_runs` table is the durable dispatch ledger.
- Structured logs: `scheduler_enabled`, `schedule_fired`,
  `schedule_catchup_skipped`, `schedule_no_runners`,
  `scheduled_task_timeout`.

---

## DST & cron edge cases

- **Fall-back (clocks repeat an hour):** a wall-clock cron time in the
  repeated hour maps to two UTC instants, but both share one local
  fire-label, so the run **fires once**, not twice. (The `scheduled_runs`
  dedup identity uses the local wall-clock minute, not the UTC instant.)
- **Spring-forward (clocks skip an hour):** a cron time in the skipped
  hour has no instant that day, so it **does not run** that day — and
  nothing was "missed" from the window's view, so there's no
  `skipped_catchup` row. If a daily run is critical, avoid the local
  02:00–03:00 window or use `timezone: UTC`.
- **Day-of-week ranges don't wrap:** `sat-sun` is rejected as a
  descending range (Sunday is `0`). Write `6-7`, `sat,sun`, or `0,6`
  for weekends.
- **Catchup window** is a bounded heuristic (half the period, clamped to
  `[catchup_min, catchup_max]`); for irregular cadences (e.g. `0 9 * * 1,5`)
  it's an approximation of the inter-fire interval, not an exact value.

## Out of scope

- **Multi-platform delivery.** The runner already owns its outbound
  surfaces (Slack channel, Jira project), so delivery routing is the
  agent's job, not the scheduler's.
- **Script-only monitors.** A cheap monitor runs as a low-budget role; the
  scheduler always dispatches to an agent turn.
