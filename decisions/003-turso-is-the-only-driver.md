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
Measured here — 5 000 episodes at 1 536 dimensions with ~1 KB of text apiece,
keeping the top 5 — the Go loop pulled **35.8 MB** across the driver boundary
and took **144 ms**. The same ranking computed by the database, fetching the
winners by primary key, is **35.8 KB and 34 ms**. That is the cost of the
intersection, in one function, on the Plan phase of every turn.

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
   else stays unused until it earns its place, and most of it does not — the
   survey is below.

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

## The rest of Turso's surface, surveyed

The pinned driver exposes 124 SQL functions mainline SQLite does not. Every one
was checked against a real call site in this tree; four things are worth
recording, and the rest is a list of rejections that exists so nobody has to
run the survey again.

**Adopted.**

- `vector_distance_cos` — recall's ranking, in `internal/learning`. The numbers
  are above. Its three measured semantics are pinned by
  `TestVectorDistanceSemanticsRecallDependsOn` in `internal/store`, because all
  three are invisible in the code that leans on them: it is a DISTANCE and
  equals `1 - cos` (which is what makes recall's `distance <= 1 - floor` mean
  `similarity >= floor`); a NaN or infinity component answers **0**, a false
  perfect match that sorts FIRST; and a width mismatch fails DURING ITERATION
  after rows have already been emitted, which is what makes
  `length(embedding) = ?` load-bearing rather than a narrowing.
- `turso_version()` — one field on the `store_opened` log line. The driver is
  pre-1.0 and pinned, so "what changed when we bumped it" is a real support
  question and this is the only thing in the process that can answer it.

**Rejected, with the reason.**

- **The quantised vector types (`vector8`, `vector1bit`)** — measured, 2 000
  rows × 1 536 dims: `vector8` is 4× smaller on disk with the top-10 ranking
  intact, `vector1bit` 31× smaller and 3/10 — but the READ PATH GETS SLOWER,
  ~40% on the distance scan, which is the exact path the whole choice was made
  for. Paying latency on every turn to save 30 MB of local file per seat is the
  wrong trade, and it would additionally need a Go decoder for a layout
  upstream does not document (the read path decodes embeddings for the
  non-finite guard and for compaction).
- **`uuid4` / `uuid7` / `gen_random_uuid`** — the engine's ids are not random.
  An agent id is `uuid5(namespace, org + ":" + handle)`, derived so a handle
  maps to one id across the fleet, and SQL cannot compute it. The random ones
  are minted behind an injection seam so tests can substitute a deterministic
  minter, and the caller needs the value before AND after the insert, for the
  returned struct and for its log lines. `uuid7`'s time ordering buys nothing:
  episodes order by an explicit `ended_at` with its own index.
- **`percentile` / `percentile_cont` / `percentile_disc` / `median` / `stddev`**
  — no call site. Nothing in the tree computes a percentile over rows. The
  nearest candidate is `learning_health.avg_uses_per_skill`, where a median
  would genuinely be a more honest answer than an average — but that is a
  product decision about a documented threshold, not a dialect one, and its Go
  reader has no caller yet.
- **`regexp` and the `REGEXP` operator** — every one of the 30-odd
  `regexp.MustCompile` sites in this tree matches a Go string in memory: secret
  redaction over text that never reaches the store, config validation that must
  answer before anything is written, frontmatter and `${VAR}` parsing. None of
  them is a column.
- **The whole `time_*` pack, `make_date`, `to_timestamp`, `dur_*`,
  `generate_series`** — every timestamp in this schema is an INTEGER of
  microseconds written by `store.EncodeTime`, and all date arithmetic is Go
  over `time.Time`. `time_now()` returns an opaque 13-byte custom-type blob
  with no Go decoder, so adopting any of it would put values in columns only
  SQL can read, in a store whose entire read path is Go.
- **`nextval` / `currval` / `setval`** — Turso sequences are per-database, and
  this database is per-NODE and exclusively owned. Every monotonic value the
  company shares — the lease fencing epoch, the token counters — lives in the
  coordination KV precisely because N nodes must not keep N counters (d-201,
  and migrations 0010–0011 are what breaking that rule cost).
- **The `array_*` / `struct_*` / `union_*` pack — not actually available.**
  This is the correction worth carrying forward: they are present in
  `pragma_function_list` and they do not work.
  `array_length(string_to_array('a,b,c', ','))` fails with *"Array features
  require --experimental-custom-types flag"*, and the Go driver offers no way
  to pass a flag. The same gate hides generated columns
  (`--experimental-generated-columns`) and `VACUUM` (`--experimental-vacuum`).
  "Turso has array functions" is true and misleading at the same time, so the
  honest surface for this project is smaller than the function list suggests.

**Still absent**, and still the reason `Capabilities` exists: no full-text
index (`fts5` is not a registered module and `fts()` is a parse error in
`CREATE INDEX`) and no ANN vector index (`libsql_vector_idx` likewise). Turso
registers no virtual-table module at all — `pragma_module_list` is empty.

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

- **NOT the static binary.** This is worth stating because it is the first
  thing the release-tooling note (d-901) invites a reader to assume. Measured
  on one machine, `CGO_ENABLED=0` throughout:

  | build | linkage |
  |---|---|
  | `main` before this change, both drivers | **dynamic** — `NEEDED libdl.so.2, libpthread.so.0, libc.so.6` |
  | this change, Turso alone | **dynamic** — identical |
  | a program linking ONLY `modernc.org/sqlite` | **statically linked**, zero `NEEDED` |

  Dropping the fallback cost nothing here: Turso was already the DEFAULT
  driver, so purego and its `dlopen` were already linked in and the artifact
  was already dynamic. Row 3 is the honest counterfactual, and it is about the
  other direction — mainline SQLite is pure Go with no shared object to load,
  so an engine that had dropped *Turso* instead would ship a static binary.

  That is a real trade and it was made earlier, in d-002, when Turso became the
  database: a static binary in exchange for the vector functions the learning
  subsystem's recall reads through. This decision only removes a fallback that
  could not serve a database with rows in it. Anyone reopening the trade is
  reopening d-002, not this one.

## What this does not change

- **Windows is dropped in the same change but for a different reason.** The
  Turso driver's platform library ships for `windows/amd64` and NOT for
  `windows/arm64`, so half the released Windows matrix could never open a
  store — the arm64 binary failed at its first query unless the operator knew
  to set `CREWLET_STORE_DRIVER=sqlite`, which is the fallback this decision
  removes. Rather than ship one Windows architecture and drop the other, the
  target goes: see `.goreleaser.yaml` and `RELEASING.md`. Nobody should read
  this as "Turso has no Windows build" — it has one, for one architecture.

  `internal/store/platform.go` is the general form of that lesson: the
  constraint names the four supported pairs rather than excluding windows,
  because `GOOS=windows GOARCH=arm64 go build` exits 0 against an empty
  `embed.FS` and that is exactly how the broken archive got published. A
  platform with no library now fails at build time, whichever one it is.

- **There is no musl build either, and the reason is not the one you would
  guess.** The obvious fix for Alpine is a second archive built with
  `-tags musl`, which is the tag upstream selects its musl `.so` with. It was
  written and then measured out: the tag does not change the BINARY's linkage.
  purego declares its `dlopen` imports with `//go:cgo_import_dynamic`, so
  `CGO_ENABLED=0 go build` emits `interpreter /lib64/ld-linux-x86-64.so.2`
  with `NEEDED libc.so.6` — identical with and without the tag, while a
  hello-world at `CGO_ENABLED=0` is genuinely static. So a `_musl` archive
  built that way fails at `execve` on Alpine: a signed artifact promising a
  platform it cannot run on, which is the windows/arm64 defect again.

  What ships instead is the truth, in the places an operator reads before
  choosing a base image: the linux binaries require glibc. A real musl
  artifact needs a musl toolchain to build on and a musl host to test on, and
  neither was available here — that is worth doing, and it is not worth
  guessing at. The `cross` CI job asserts the linkage on every run, so the day
  a dependency makes the binary static again is a build failure naming the
  statements to re-check.
- **d-002 §2.** The nullable `work_key` and the plain unique index behind it
  are unchanged, and the measurement that decided them is still asserted by
  `TestPartialIndexConflictTarget`.
