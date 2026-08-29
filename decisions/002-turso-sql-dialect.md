# d-002 — The Turso SQL dialect we may actually use

Status: **decided**, amended by [d-003](003-turso-is-the-only-driver.md)

> **What d-003 changed.** §1 (write in the intersection of two dialects) and §3's
> Go-side fallback are retired: `modernc.org/sqlite` is gone, Turso is the only
> driver, and recall's distance arithmetic runs in the database. §2 (NULL for
> "unconstrained" rather than a partial-index `ON CONFLICT` target), §4 (FTS is
> not a dependency) and §5 (pin and re-probe) are unchanged and still
> load-bearing — the measurements below re-run identically at the pinned
> version, which is why the capability probe survived the drop.

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

1. ~~**Write in the intersection dialect.**~~ *Retired by d-003.* All SQL had to
   parse on Turso *and* on mainline SQLite (`modernc.org/sqlite`, the certified
   fallback driver), enforced by the dual-driver CI job. There is one driver
   now, the job is gone with it, and the intersection turned out to cost 124 SQL
   functions the engine is on Turso for — see d-003 for the accounting.

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

   *Amended by d-003.* The intent held and the outcome did not: the Go loop ran on
   BOTH drivers unconditionally, because it had to exist for the driver without the
   functions and nothing ever called the other path. The probe reported the
   capability and no code read it. With one driver the SQL path is the only path —
   `vector_distance_cos` in an `ORDER BY`, still a scan behind the per-agent index
   because there is still no ANN index. The sentence that mattered is the one about
   the workload, and it is unchanged.

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

> *d-003:* the middle clause did not survive contact. The fallback driver was real
> and turned out not to be valuable — it could not serve a database with rows in it,
> because it has no vector functions and recall degraded to nothing without saying
> so. The rest of this paragraph stands, and the last sentence is now the whole
> strategy rather than half of it.
