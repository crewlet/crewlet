# q — the `scheduled_runs` SQL lives in `internal/schedule/sqlledger`, not in `internal/store`

Status: **decided one way and recorded for the store owner to overrule** —
nothing in either package changes if it moves. Raised implementing
`go/internal/schedule/`.

## The collision

`internal/store` owns the schema. `schema/0004_runtime.sql` creates
`scheduled_runs`, with the composite PRIMARY KEY that IS the scheduler's
at-most-once guarantee and the `fired_at DESC` index the dashboard reads
through. Every other table in that file is read and written by code inside
`internal/store` — `EventLog` is the pattern.

The scheduler's ledger is the exception: `sqlledger.Ledger` sits in
`internal/schedule/sqlledger` and issues three statements against a table
another package created.

## Why it was put there

Three reasons, in the order they mattered.

**The interface is consumer-defined.** d-000 says to declare an interface where
it is USED, listing only what that caller needs. `schedule.Claimer` is one
method; `schedule.Ledger` is three. Putting the implementation next to the
declaration keeps the contract, the twin, the SQL backend and the suite that
certifies both in one place a reader can hold at once — and the suite is the
part that matters, because it is what stops the twin and the real store
diverging the way the Python pair did.

**It keeps `internal/schedule` free of a database driver.** The SQL backend is
a subpackage, so the scheduler itself imports stdlib, `org`, `events`,
`queue/topics`, `coord` and `logging` and nothing heavier. A unit test of the
tick loop does not link `modernc.org/sqlite` and `turso.tech/database/tursogo`.

**Ownership, honestly.** Several agents share this checkout and
`internal/store` is not mine to edit. That is a real reason and it is not a
good one on its own, which is why it is third.

## What it costs

The statements and the DDL are in different packages with nothing linking
them, so a renamed column fails at run time in whichever test happens to touch
it, with a driver error that names the column and not the reason. That is
mitigated rather than removed: `TestTheStatementsNameTheirColumns` reads the
live table's shape and compares it to the set the statements name, so a column
that moved produces one failure that says what moved.

It also means `internal/store`'s package doc, which describes the store as the
home of the engine's persistence, is now slightly incomplete: one table is
written from outside it.

## What would change if the store owner wants it moved

Move `sqlledger` under `internal/store` and have it satisfy
`schedule.Ledger` from there. Nothing in `internal/schedule` changes — the
scheduler holds an interface — and `scheduletest.Run` is called with a
different constructor. The column-name guard becomes unnecessary at that point,
since the SQL and the DDL would be in one package.

The one thing that must NOT change either way: both backends keep running the
same `scheduletest` suite. A backend the suite has not certified does not
exist.
