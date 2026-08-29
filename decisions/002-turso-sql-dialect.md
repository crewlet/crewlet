# d-002 — The Turso SQL dialect we may actually use

Status: **decided**

## What the spike found

Measured against `turso.tech/database/tursogo@v0.8.0-pre.7` (the current official Go
driver path; `github.com/tursodatabase/turso-go@v0.2.2` is the older repo and behaves
the same or worse) on Go 1.27.0:

| Feature | Result |
|---|---|
| Tables, transactions (`BEGIN`/`COMMIT`), `database/sql` round-trip | works |
| Partial unique index *creation* (`CREATE UNIQUE INDEX … WHERE …`) | works |
| `INSERT … ON CONFLICT(col) DO UPDATE … RETURNING` on a **plain** unique/PK | works |
| `INSERT … ON CONFLICT(col) …` targeting a **partial** unique index | **parse error** |
| Conditional `UPDATE … WHERE col = $expected` (the CAS idiom) | works |
| `F32_BLOB(n)` columns, `vector32()`, `vector_distance_cos()` | works |
| `libsql_vector_idx()` ANN index in `CREATE INDEX` | **parse error** |
| Tantivy FTS index (`CREATE INDEX … fts(col)`) | **parse error** |

The vector *functions* ship; the vector *index* and FTS do not, in this driver
version. The expectation that Turso supplies ANN vector search and transactional FTS
is therefore **premature**, not wrong — the column type and distance functions are
present and the rest is announced surface not yet reachable from Go.

## Decisions

1. **Write in the intersection dialect.** All SQL must parse on Turso *and* on
   mainline SQLite (`modernc.org/sqlite`, the certified fallback driver).
   The dual-driver CI job is what enforces this, and it just became the load-bearing
   guard rather than a formality: Turso is the narrower dialect today.

2. **No `ON CONFLICT` against a partial index — use NULL for "unconstrained".**
   The Python design used `UNIQUE(agent_handle, work_key) WHERE work_key <> ''`
   because an empty work key means "legitimately unconstrained". SQL unique indexes
   treat NULLs as distinct, so a **plain** `UNIQUE(agent_handle, work_key)` with
   `work_key = NULL` for the unconstrained case gives identical semantics with a
   plain `ON CONFLICT` target. This is simpler than what it replaces. The store
   layer maps empty-string ⇄ NULL at the boundary so callers keep the Go zero value.

3. **Vector recall is brute-force, behind one query function** — one narrow function
   per Turso-specific feature, so the fallback is a degradation, not a redesign.
   `vector_distance_cos` on Turso; a Go-side cosine loop on the SQLite fallback.
   Both are correct at the actual workload — per-agent diary and compacted episodes,
   thousands of rows, always filtered by agent first. A capability probe at boot
   records which path is live; when Turso's ANN index reaches the Go driver, only
   that one function changes.

4. **FTS is not a v1 dependency.** v1 knowledge search is the external backend
   (Confluence CQL) behind `KnowledgeSearcher`, exactly as today.
   Built-in knowledge search is post-v1 and design-gated anyway; by the time it is
   built, either Turso's FTS is reachable from Go or Bleve fills the slot behind the
   same seam.
   **Nothing in v1 blocks on this.**

5. **Pin and re-probe.** The driver is pinned; a capability probe suite
   (`internal/store/capability_test.go`) re-runs the table above against whatever
   version is pinned, so an upgrade that unlocks ANN/FTS shows up as a passing test
   that was previously skipped, and a regression fails the build.

## Consequence for the database choice

The choice stands — Turso is the database, `database/sql` + store contracts is the
seam, the SQLite fallback driver is real and now demonstrably valuable. What changes
is the *justification*: Turso is chosen for the trajectory, the file-format
compatibility, and MVCC — not for vector/FTS features that are not yet reachable.
Every one of those features is behind a single function, so arrival is an upgrade,
not a migration.
