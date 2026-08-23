# d-501 — The scheduler: a claim per tick, a ledger under it, and a horizon sized to the grammar

Status: **decided** · Phase: 5 · Spec: `src/crewlet/schedule/` (`cron.py`,
`scheduler.py`, `store.py`), `docs/concepts/scheduling.md` ·
Implementation: `go/internal/schedule/`, `go/internal/schedule/sqlledger/`,
`go/internal/schedule/scheduletest/`

## What is unchanged, because it was never idiom

Three things carry the correctness, and all three are ported verbatim in
meaning:

1. **At-most-once lives in the LEDGER, not in the tick.** Every fire is
   claimed before it is published, on an identity whose fire label is the
   LOCAL wall-clock minute. That is what makes the dedupe DST-correct: a
   fall-back day maps one local cron minute onto two UTC instants, both render
   to one label, and the schedule fires once.
2. **A tick that did not evaluate its window does not close it.** A node that
   is shedding on config posture, or that does not hold the fleet duty, leaves
   `lastTick` alone — so it stays on its FIRST tick, and if it later starts
   evaluating, the catchup pass covers what it skipped rather than a window
   stretching back to boot.
3. **Failure is per-fire.** One unparseable cron, one decommissioned runner,
   one broker refusal must not stop the other nineteen schedules.

Two more that read as details and are not: the runner is resolved from the ORG
and never from a local agent pool (the node whose tick wins the duty is rarely
the node that owns the seat), and the identity is the column TUPLE rather than
a delimiter-joined string.

## The four things Go changed

### 1. The persistence seam is an interface with two certified backends

Python had `ScheduledRunStoreProtocol`, a Postgres store, and a memory twin
whose divergences were nobody's failing test — the twin's `purge` dropped the
record and kept the claim key for a while, which is a permanently silent
refusal.

Here there is `schedule.Ledger`, an in-memory twin, a SQL backend over
`scheduled_runs`, and ONE contract suite (`scheduletest`) that both run. The
suite enumerates what it SENDS on two axes — the values that go into an
identity, and the point in a ledger's lifecycle at which each operation is
sent — because no mutation can reveal an input the suite never sends.

The scheduler itself depends on `Claimer`, a one-method interface: it claims
and nothing else, and a dispatcher that could also read history and delete
rows is a dispatcher whose blast radius is not visible from its type.

`Purge` takes an INSTANT rather than an age. Python's `purge(older_than_seconds)`
made "drop everything" a negative number that the twin then clamped to zero, so
the two backends' boundary behaviour differed for the one call a test actually
makes.

### 2. The fleet duty is a claim per tick on `coord.WorkerResource("scheduler")`

Not a leader election, and the difference is the point: a claim needs no term,
no quorum, no failure detector and no step-down protocol. A node that dies
between ticks releases the duty by letting its lease lapse, and the next node
to tick takes it — nothing has to notice the death, because nothing was
waiting for the dead node to say anything.

The claim is `Ungated`, for the reason `coord`'s own doc gives: a duty record
left at an older protocol by a build predating the gate would block every seat
claim fleet-wide the moment the version moved.

`(false, err)` from the backend is UNKNOWN and the tick fails CLOSED on it. An
unreadable lease store says nothing about who holds the duty, and firing on
that basis puts every node back to racing the ledger claim — the exact
duplicated work the duty exists to remove.

### 3. The cron horizon is sized to the grammar

`cron.py` scanned 400 days before reporting "no next fire". The longest gap
between two fires of a valid 5-field expression is `0 0 29 2 *` across a
century that is not a leap year: 2096-02-29 to 2104-02-29 is 2921 days,
because 2100 is divisible by 100 and not by 400.

So the Python evaluator reported "never" for a legitimate quadrennial schedule
in three years out of four, and across the century gap in seven out of eight.
Consequences: the dashboard drew no next run, and `_catchup_window` fell back
to its minimum clamp instead of the schedule's period. `Horizon` here is 2925
days, and `TestTheHorizonReachesTheRarestLegalFire` holds it there.

The cost lands only on an expression that never matches at all (February 30th),
because every reachable one terminates at its next fire. Measured:
`BenchmarkNextUnreachable` walks the whole horizon in 143 ms;
`BenchmarkNextDaily` finds an ordinary weekday fire in 25 µs.

The scan stays minute-by-minute over UTC instants, matching their LOCAL
projection, and that is deliberate rather than unexamined. It is the whole DST
design: every UTC instant has exactly one local time, so a repeated local hour
yields two fire instants and a vanished one yields none, with nothing to
resolve. Constructing local times instead — the obvious way to skip whole days
— has to invent an answer in both cases, and Go's `time.Date` explicitly does
not promise which one it picks inside a gap. Skipping by whole hours is
unavailable too: `Asia/Kathmandu` is +05:45 and `Pacific/Chatham` +12:45, so an
hour-granular jump lands on the wrong minute in exactly the zones nobody tests.

### 4. `Expr` is a comparable value

Bitmasks instead of frozensets, so two expressions that mean the same thing are
`==`. `MON` and `1` are asserted equal in one line rather than field by field,
and a field added later cannot escape that comparison the way a hand-written
equality would. The zero `Expr` matches nothing, which is the safe zero for a
struct field nobody parsed into.

## Constants, and what each is anchored to

| constant | value | anchor |
|---|---|---|
| `Horizon` | 2925 days | the longest gap the grammar allows (Feb 29 across 2100), plus slack |
| `DefaultTick` | 10 s | cron is minute-granular, so any sub-minute tick catches every fire; the value buys how LATE one can be |
| `MaxTick` | 60 s | at or above a minute a tick steps OVER a fire rather than delaying it; mirrors config validation, enforced here because programmatic construction bypasses that |
| `DefaultCatchupMin` | 2 min | keeps a restart from re-firing something that has only just run |
| `DefaultCatchupMax` | 2 h | stops a morning restart replaying the whole night; also the ledger's retention FLOOR |
| catchup window | half the period | a fire more than half a period old is closer to the NEXT one than to the one it would replay |
| `DefaultScheduleTimeout` | 3 min | already in `internal/org`; a scheduled turn is a ritual, not open-ended work |
| `dutyTTLTicks` | 3 | rides out two consecutive slow or failed claims without the duty flapping to a peer — the same rule the seat heartbeat follows |
| `dutyTTLFloor` | 30 s | a very short tick would otherwise mint a very short lease and re-claim it constantly, which is store traffic bought for nothing |
| `scheduletest.budget` | 30 s | a hang must fail as a NAMED case, not as a package timeout attributed to whichever package the deadline lands in |

### The duty TTL's ceiling is a judgment call, and it is stated rather than hidden

When the holder dies the duty is unclaimable for up to one TTL, and the
successor's catchup pass replays the MOST RECENT missed fire, not every one. So
a schedule whose period is shorter than the TTL can lose a fire.

At the default 10 s tick, `DutyTTL` is 30 s — half a cron minute, so a dead
holder costs at most one delayed fire, which catchup then makes up. At the
configured ceiling (`tick_seconds: 59`) it is 177 s, and a per-minute schedule
can lose fires across a duty handover. That is accepted rather than clamped: an
operator setting a tick near `MaxTick` has already accepted minute-scale
dispatch latency, and a TTL that fought the tick would flap instead.

## The non-promise, restated

A publish failure AFTER a successful claim drops that fire permanently. It is
not an oversight — a publish failure is ambiguous (the broker may have
persisted the event and lost the acknowledgement), so releasing the claim to
retry would risk waking the seat twice. At-most-once is the promise; the fire
is dropped, and logged at error with the ledger identity so a burnt claim is
visible rather than inferred.

## What is NOT here

The engine wiring. `internal/schedule` is a library: something above it must
construct the `Scheduler`, gate it on `HasSchedules`, hand it a `Ledger`, hand
it `ClaimDuty(backend, owner, nodeID, tick)` when there is a coordination
store, and run `Run(ctx)` under whatever owns the process's goroutines. The
shape:

```go
s, err := schedule.New(schedule.Options{
    Publisher:       q,                       // queue.EventQueue satisfies it
    Org:             func() *org.Organization { return node.Org() },
    Ledger:          sqlledger.New(db.SQL()),
    DefaultTimezone: cfg.Scheduling.DefaultTimezone,
    Tick:            time.Duration(cfg.Scheduling.TickSeconds) * time.Second,
    Jitter:          time.Duration(cfg.Scheduling.JitterSeconds) * time.Second,
    CatchupMin:      time.Duration(cfg.Scheduling.CatchupMinSeconds) * time.Second,
    CatchupMax:      time.Duration(cfg.Scheduling.CatchupMaxSeconds) * time.Second,
    Admits:          node.AdmitsTriggers,
    Duty:            schedule.ClaimDuty(backend, owner, nodeID, tick),
})
```

Two seams are declared here and satisfied elsewhere when their owners land:

- `Options.Trace` mints the trace context each fire runs under. There is no
  telemetry package in this build, so the default is a W3C-shaped random
  minter — a 32-hex trace id and a 16-hex span id, which is what a real tracer
  will accept when one is wired, so ids already written to the ledger stay
  meaningful across that change.
- The retention sweep. `Ledger.Purge` exists and is certified;
  `Scheduler.RetentionFloor()` reports the shortest retention it may be swept
  with (the catchup ceiling). Nothing calls it yet — which is precisely the
  Python failure mode this records: `purge` existed on both stores and on the
  protocol, and NOTHING called it, so the table grew for the life of the
  deployment.
