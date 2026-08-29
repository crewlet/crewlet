# d-003 — Turso is the only store driver

Status: **decided**

Applies to: `internal/store`, `internal/config` (Tier A `store:`), `.goreleaser.yaml`

Amends [d-002](002-turso-sql-dialect.md), which decided to write in the
*intersection* of two dialects and keep `modernc.org/sqlite` as a certified
fallback. That decision was right for what it knew. This one retires its first
and second clauses; everything d-002 measured is still true, and §2 (NULL for
"unconstrained" instead of a partial-index `ON CONFLICT` target) is unaffected
and still load-bearing.

## What changed since d-002

Nothing about Turso's dialect — the matrix in d-002 re-measures identically at
the pinned `v0.8.0-pre.7`, which is still the newest published version. What
changed is what the second driver was *for*.

d-002 kept mainline SQLite as an escape hatch: a driver an operator could
switch to with `CREWLET_STORE_DRIVER=sqlite` when the Turso driver would not
start. Three things were then discovered about that hatch:

1. **It is not an escape hatch for a database with rows in it.** Only Turso has
   `vector32()` and `vector_distance_cos()`. An operator who flipped the
   variable kept every table and lost recall — the agent-learning subsystem's
   whole read path — and got no error saying so, because recall was written to
   degrade silently. "Fall back to the other driver" was advice that quietly
   traded a company's memory for a start-up.

2. **Nothing selected it but its own test job.** The Tier A `store.driver`
   field, the environment variable and the dual-driver CI matrix existed to
   serve each other. No documented recipe, no deployment and no support path
   ever ran the engine on it.

3. **The intersection is expensive and the bill was invisible.** Writing every
   statement so that it parses on both drivers means never using anything Turso
   has and mainline SQLite does not. Measured on the pinned driver, that is 124
   SQL functions — the vector pack, `uuid7()`, `regexp`, `generate_series`,
   `percentile`/`median`, the whole `time_*` set. The engine paid for a
   database it then declined to use.

The clearest instance is recall. `internal/learning` pulled every embedded row
for a seat across the driver boundary and ran a cosine loop in Go **on both
drivers, unconditionally** — because the loop had to exist for the driver
without the functions, and once it existed nothing called the other path. The
capability probe reported `VectorFunctions: true` on Turso and no code read it.
Measured here, 5 000 rows at 1 536 dimensions: 30.7 MB across the boundary and
81 ms, against 26 ms and one row for the same ranking computed by the database.
That is the cost of the intersection, in one function, on the Plan phase of
every turn.

## Decisions

1. **Turso is the driver.** `modernc.org/sqlite` is removed from `go.mod`,
   along with the `Driver` type, `store.Options.Driver`, `DB.Driver()`,
   `ErrUnknownDriver` and `resolveDriver`. The driver name is an unexported
   constant. `internal/store` asserts that mainline SQLite is not linked into
   the binary — a removed selector with a surviving blank import ships a whole
   storage engine nothing can reach but anything with a raw `sql.Open` can.

2. **The knob is retired, not narrowed.** Tier A's `store.driver` and
   `CREWLET_STORE_DRIVER` both selected between two implementations of which
   one exists, so a field with one legal value would be a knob with no honest
   reason to differ. `store.driver` joins `retiredBootstrapFields`, so a config
   that still carries it — the quickstart and `examples/nimbus.config.yaml`
   both showed it — is answered by name rather than as a misspelling. That
   table is now keyed on the BLOCK (`Store.driver`) rather than on the bare
   word, for the same reason d-001 keyed it per tier: a `driver:` mistyped
   under `stream:` has to read as the ordinary typo it is.

3. **Write in Turso's dialect, deliberately.** The first thing spent is
   `vector_distance_cos` on the recall path, which is where the intersection
   cost the most (see the numbers above, and `internal/learning`). Everything
   else Turso adds stays unused until it earns its place — the survey is in the
   pull request, and the short answer is that `uuid7()` loses to `uuid.UUID`
   generated in Go (the engine's ids are derived, not random) and the
   quantised vector types (`vector8`, `vector1bit`) lose because the Go side
   would need a decoder for a layout upstream does not document.

4. **Keep the capability probe.** It stops being a comparison between two
   drivers and becomes a tripwire on one: `VectorIndex` and `FullTextSearch`
   are both false at the pin, and `capability_test.go` turns each into a test
   that SKIPS today and RUNS the day a driver upgrade lands it. That is the
   property d-002 §5 asked for and it is worth more with one driver, not less.
   A probe still never fails `Open` — recall degrades to empty rather than
   taking a company down with it.

5. **Keep the contract suite.** `storetest` was written to certify two drivers
   against one dialect. What it actually pins is the behaviour the packages
   above the store depend on — keyset paging that does not skip a row, a read
   floor, an idempotent append, a retention sweep that stops where it is told —
   none of which is a property of a driver name. It runs unchanged on a pin
   bump, which is the day it earns its keep.

## What this gives up, honestly

- **A second implementation to cross-check a statement against.** Turso is
  pre-1.0. If it miscompiles a query, nothing here will notice by disagreement
  any more; the contract suite has to catch it by assertion instead.
- **A driver to fall back to when the native library will not load.** The
  remediation in `internal/store/turso.go` used to name one. It now names the
  cache directory to delete and `TURSO_GO_CACHE_DIR`, and says plainly that
  there is nothing to fall back to — which is the honest shape of the same
  message, and makes the library preparation in `turso.go` strictly more
  load-bearing than it was.
- **Not much else.** The file format is unchanged — both drivers wrote a
  SQLite file — so an existing database opens on the remaining driver
  untouched, `sqlite3` still reads it for forensics, and there is no
  migration to run.

## What this does not change

- **Windows is dropped in the same change but for a different reason.** The
  Turso driver's platform library ships for `windows/amd64` and NOT for
  `windows/arm64`, so half the released Windows matrix could never open a
  store — the arm64 binary failed at its first query unless the operator knew
  to set `CREWLET_STORE_DRIVER=sqlite`, which is the fallback this decision
  removes. Rather than ship one Windows architecture and drop the other, the
  target goes: see `.goreleaser.yaml` and `RELEASING.md`. Nobody should read
  this as "Turso has no Windows build" — it has one, for one architecture.
- **d-002 §2.** The nullable `work_key` and the plain unique index behind it
  are unchanged, and the measurement that decided them is still asserted by
  `TestPartialIndexConflictTarget`.
