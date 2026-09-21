package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE SCOPE INDEX IS THE SAME ROWS AT EVERY CHUNK WIDTH.
//
// [tables.retain] writes one row per term of a record's scope, and a record's
// scope is data-driven: the paths NEST, so a bulk edit over a project states
// the project and every task under it. It was one statement per path until
// [store.InsertRows] existed.
//
// The hazard chunking introduces is not the count, it is COLLISION INSIDE ONE
// STATEMENT: two identical paths in one record used to arrive as two separate
// inserts, where the second hit `ON CONFLICT (position, path) DO NOTHING` on a
// row the first had already committed. Batched, they land in the same
// statement. So the assertion is that the rows are byte-identical across
// widths that split the collection differently — including a width of 1, which
// is the pre-conversion shape, so the old behaviour is the control.
func TestTheDeferredScopeIsTheSameRowsAtEveryChunkWidth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	paths := make([]string, 0, 64)
	for i := range 40 {
		paths = append(paths, fmt.Sprintf("project/ENG/task/%03d", i))
	}
	// A DUPLICATE AND AN ANCESTOR, because both are what a real scope
	// carries and both are what the conflict clause is there for.
	paths = append(paths, "project/ENG", "project/ENG/task/000", "project/ENG")

	rec := Record{
		Envelope: Envelope{
			Subject: Subject{Kind: "task", ID: "T-1"},
			OpID:    "op-1",
			Scope:   ScopeSet{Paths: paths},
		},
		Position: Position{Stream: "TEST", Generation: 1, Seq: 7},
		Payload:  []byte(`{"body":"x"}`),
		StoredAt: time.Unix(1_700_000_000, 0).UTC(),
	}

	// Each limit splits the 41 normalised paths differently. 0 is the unset
	// field — RowsPerInsert reads it as one row per statement, which is
	// exactly the pre-conversion shape and therefore the control; 2 and 4
	// and 14 straddle the collection at one, two and seven rows a
	// statement; 2000 is the real probed limit, which takes it in one.
	want := map[int][]string{}
	for _, limit := range []int{0, 2, 4, 14, 2000} {
		got := scopeRowsAt(ctx, t, rec, limit)
		if len(got) == 0 {
			t.Fatalf("at a limit of %d the scope index is empty, so this test "+
				"would pass on a writer that wrote nothing", limit)
		}
		want[limit] = got
	}
	// AN ABSOLUTE EXPECTATION AS WELL AS A RELATIVE ONE. Comparing the
	// widths against each other cannot catch a bug that is uniform across
	// them — binding every row's path from paths[0] writes one row at every
	// limit and agrees with itself — so the rows are also compared against
	// the record's own normalised scope.
	expect := slices.Clone(rec.Scope.Normalised().Paths)
	slices.Sort(expect)
	if got := want[2000]; !slices.Equal(got, expect) {
		t.Errorf("the scope index holds %v, want the record's normalised scope %v",
			got, expect)
	}

	base := want[0]
	for limit, got := range want {
		if len(got) != len(base) {
			t.Errorf("a limit of %d wrote %d scope rows, a limit of 0 wrote %d",
				limit, len(got), len(base))
			continue
		}
		for i := range got {
			if got[i] != base[i] {
				t.Errorf("a limit of %d wrote scope row %d as %q, a limit of 0 "+
					"wrote %q — chunking changed what the deferral probe reads",
					limit, i, got[i], base[i])
				break
			}
		}
	}
	t.Logf("%d normalised scope paths, identical at limits [0 2 4 14 2000]", len(base))
}

// scopeRowsAt retains one record into a fresh estate at one chunk width and
// reads its scope index back in path order.
func scopeRowsAt(ctx context.Context, t *testing.T, rec Record, limit int) []string {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "scope.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rep := db.Replicated()

	tbl := tables{stream: "TEST", deferred: "probe_deferred", scope: "probe_scope"}
	if err := rep.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE probe_deferred (
				position INTEGER NOT NULL PRIMARY KEY,
				subject TEXT NOT NULL, subject_kind TEXT NOT NULL,
				subject_id TEXT NOT NULL, version INTEGER NOT NULL,
				payload BLOB NOT NULL, stored_at INTEGER NOT NULL)`,
			`CREATE TABLE probe_scope (
				position INTEGER NOT NULL, path TEXT NOT NULL,
				PRIMARY KEY (position, path))`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create the probe tables: %v", err)
	}

	if err := rep.Tx(ctx, func(tx *sql.Tx) error {
		return tbl.retain(ctx, tx, rec, false, limit)
	}); err != nil {
		t.Fatalf("retain at a limit of %d: %v", limit, err)
	}

	var out []string
	if err := rep.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT path FROM probe_scope ORDER BY path`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the scope index back: %v", err)
	}
	return out
}

// A SCOPE WITH HUNDREDS OF ROOTS IS PROBED, NOT REFUSED.
//
// # The refusal this replaced
//
// The probe used to state each root as two chained terms —
// `... OR s.path = ? OR s.path LIKE ? ...` — and a chained `OR` parses
// LEFT-DEEP, so the expression tree's depth grew with the list. Turso refuses
// one past a hundred with `Parse error: Expression tree is too large (maximum
// depth 100)`.
//
// A scope's roots are one per OBJECT an operation writes, so an import carries
// as many as its batch has members. That put the ceiling at about fifty
// objects — measured as a company file with sixty seats failing its own boot
// seed, the whole chart lost, and the node serving a company of nobody. It is
// the worst shape a limit can have: invisible on every fixture small enough to
// write by hand, and reached by the first real company.
//
// # What this asserts
//
// Five hundred roots, which is the largest batch the org chart accepts, and a
// thousand, which is past anything this engine publishes. Both have to return
// an ANSWER rather than an error — the answer itself is what the cases above
// are about.
func TestAScopeWithHundredsOfRootsIsProbedRatherThanRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rep := db.Replicated()
	tbl := tables{stream: "TEST", deferred: "probe_deferred", scope: "probe_scope"}
	if err := rep.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE probe_deferred (
				position INTEGER NOT NULL PRIMARY KEY,
				subject TEXT NOT NULL, subject_kind TEXT NOT NULL,
				subject_id TEXT NOT NULL, version INTEGER NOT NULL,
				payload BLOB NOT NULL, stored_at INTEGER NOT NULL)`,
			`CREATE TABLE probe_scope (
				position INTEGER NOT NULL, path TEXT NOT NULL,
				PRIMARY KEY (position, path))`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create the probe tables: %v", err)
	}

	for _, roots := range []int{500, 1000} {
		scope := ScopeSet{}
		for i := range roots {
			scope.Paths = append(scope.Paths, fmt.Sprintf("g/u/team/s/%04d", i))
		}
		if err := rep.Read(ctx, func(tx *sql.Tx) error {
			_, _, err := tbl.deferredIn(ctx, tx, scope)
			return err
		}); err != nil {
			t.Errorf("a scope of %d roots was refused rather than probed: %v — "+
				"an import of that many objects cannot be published at all",
				roots, err)
		}
	}
}
