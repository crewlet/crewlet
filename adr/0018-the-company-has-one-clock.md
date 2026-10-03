# ADR-0018 — The company has one clock

- **Status:** accepted
- **Authority:** `internal/config`
- **Enforced-by:** `internal/config.TestTheRetiredClocksAreRefused`
- **Measured:** a company on `America/Los_Angeles` spent 7 hours of every day (8 in winter) with its "today" on two dates at once; one on `Europe/Berlin`, 1 to 2 hours
- **Cost-when-tried:** three clocks, and every one of them was right about something. `tracker.native.timezone` was read by a seat's own work tools and nothing else. `scheduling.default_timezone` was read by the scheduler and nothing else. Everything else cut its day on UTC: the dashboard's board (its surface handed the tracker `time.UTC`), a person's own day and the workload (both said "a compound answer has no caller-supplied zone") and the operator's MCP surface, which was given no zone at all. So for the last seven hours of a Los Angeles day the board banded a task "today" while the same row in its assignee's day was marked overdue, an assistant's `due=friday` meant a different Friday from a seat's, and the example company — Berlin tracker, `default_timezone: UTC` — held its 09:00 standups at 10:00 or 11:00 Berlin time. The scheduler's zone was also captured when its loop was armed, so correcting it did nothing until something re-armed the loop.
- **Tag-status:** unreleased

## The decision

A company has **one clock**: the top-level `timezone` of its Tier B
document, an IANA name, UTC when absent, read through
`config.Company.Location`. Every calendar edge the engine cuts is cut on it —
where "today", "this week" and "this month" begin for a `due=` filter, a due
band, an overdue mark, a person's own day and the workload; what a relative or
all-day date resolves to; and the wall clock a schedule that names no zone of
its own fires on. The anonymous `org` projection serves it resolved, so a
dashboard cuts its days where the engine does.

`tracker.native.timezone` and `scheduling.default_timezone` are refused by
name. A schedule may still name its own `timezone` — that is one piece of
work's wall clock, the Tokyo team's 09:30 standup, and nothing else is cut on
it. Every zone name, the company's and a schedule's, is read through
`period.LoadZone`, which refuses the two names that mean whichever host reads
them (`Local` and `localtime`).

The surfaces that outlive an epoch — the scheduler's loop and the operator's
MCP surface — read the clock at every use (`engine.Engine.Zone`), so an apply
that moves it moves them. A seat's own tools take the clock of the epoch their
turn pinned.

## Why the obvious alternative is wrong

The obvious alternative is the shape this replaced: a clock where it is used,
owned by the subsystem that uses it. Each one is locally reasonable — the
tracker's dates are the tracker's business, a standup's hour is the
scheduler's — and together they make "today" a question with one answer per
subsystem. Nobody asks one subsystem at a time. A founder reads the board, a
person's day and a standup's output on one screen, and the only way those
agree is for there to be one calendar underneath all of them.

Defaulting everything to UTC instead is the second obvious alternative, and it
is how most of the engine behaved: it is a clock every node agrees on, which is
necessary, and it is nobody's working day, which is the whole of what a due
date means. The third is a per-VIEWER clock — the browser's own zone — which
agrees with nothing the engine computed and puts one task under two days for
two people looking at the same board.

## What this does not decide

It does not make any DURATION a calendar quantity. A lease, a timeout, a
retention horizon and a backoff are measured in elapsed time and never against
this clock, because a duration measured on a wall clock changes length twice a
year.

It does not decide how a viewer DISPLAYS an instant. A dashboard may still
print times in the reader's own zone; what it may not do is decide which DAY
something falls on by any clock but this one.

It does not decide where a week begins or which weeks exist. That is
`internal/period`'s — the ISO week from Monday, the same for every company —
and this record only names the location those windows are cut in.
