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
// [tables.retain] writes one row per scope term through [store.InsertRows], and
// chunking's hazard is COLLISION INSIDE ONE STATEMENT: duplicate paths of one
// record land together under `ON CONFLICT (position, path) DO NOTHING`. So the
// rows must be byte-identical at widths that split the collection differently,
// one row per statement being the control.
func TestTheDeferredScopeIsTheSameRowsAtEveryChunkWidth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	paths := make([]string, 0, 64)
	for i := range 40 {
		paths = append(paths, fmt.Sprintf("project/ENG/task/%03d", i))
	}
	// A DUPLICATE AND AN ANCESTOR: a real scope carries both, and the
	// conflict clause is there for them.
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

	// Each limit splits the 41 normalised paths differently: 0, unset,
	// RowsPerInsert reads as one row per statement (the control); 2, 4 and
	// 14 give one, two and seven rows a statement; 2000 takes all 41 in
	// one, as the probed limit (32 766) does.
	want := map[int][]string{}
	for _, limit := range []int{0, 2, 4, 14, 2000} {
		got := scopeRowsAt(ctx, t, rec, limit)
		if len(got) == 0 {
			t.Fatalf("at a limit of %d the scope index is empty, so this test "+
				"would pass on a writer that wrote nothing", limit)
		}
		want[limit] = got
	}
	// AN ABSOLUTE EXPECTATION TOO: comparing widths misses a bug uniform
	// across them (binding every row from paths[0] agrees with itself), so
	// the rows are also held to the record's normalised scope.
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
	rep := openProbeEstate(ctx, t)

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
// A scope has one root per object an operation writes, so an import carries as
// many as its batch has members, and a probe whose depth grows with the roots
// hits Turso's `Expression tree is too large (maximum depth 100)` near fifty
// ([tables.deferredBelow]), losing the whole batch. Five hundred and a thousand
// roots must each return an answer rather than an error; what the answer is
// belongs to the other cases.
func TestAScopeWithHundredsOfRootsIsProbedRatherThanRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	rep := openProbeEstate(ctx, t)
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

// THE COVERAGE PROBE ANSWERS IN BOTH DIRECTIONS, EXACTLY.
//
// A retained record and a probe intersect when one CONTAINS the other, either
// way round (a record on a task beneath the project a write rewrites, or on the
// project above the task a write edits), and only then. Each probe below fails
// when the bindings do not follow the statement's text ([tables.deferredBelow]):
// no descendant matches, and LIKE runs case-blind with `_` as a wildcard.
func TestTheCoverageProbeFindsBothDirectionsAndNothingElse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rep := openProbeEstate(ctx, t)
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
			`INSERT INTO probe_deferred VALUES (7, 's', 'task', 'T-1', 9, x'00', 0)`,
			`INSERT INTO probe_scope VALUES (7, 'project/ENG_OPS/task/1')`,
			`INSERT INTO probe_deferred VALUES (8, 's', 'team', 'teamxa', 9, x'00', 0)`,
			`INSERT INTO probe_scope VALUES (8, 'teamxa')`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create the probe tables: %v", err)
	}

	// at is the position the probe must find, and zero is a probe that must
	// find nothing.
	for _, tc := range []struct {
		name  string
		probe string
		at    uint64
	}{
		{"the object itself", "project/ENG_OPS/task/1", 7},
		{"the container above it", "project/ENG_OPS", 7},
		{"the root above that", "project", 7},
		{"something inside it", "project/ENG_OPS/task/1/comment/3", 7},
		{"the other record, below its own root", "teamxa/member/1", 8},
		{"a neighbour", "project/ENG_OPS/task/2", 0},
		{"another container", "project/OPS", 0},
		{"an underscore in a stored path is a character", "project/ENGXOPS", 0},
		{"a path is case-sensitive", "PROJECT/ENG_OPS/task/1", 0},
		{"a prefix is a whole segment", "project/ENG_OP", 0},
		{"an underscore in the probe is not a wildcard", "team_a/member/1", 0},
		{"a probe's case is its own", "TEAMXA/member/1", 0},
	} {
		if err := rep.Read(ctx, func(tx *sql.Tx) error {
			d, hit, err := tbl.deferredIn(ctx, tx, ScopeSet{Paths: []string{tc.probe}})
			if err != nil {
				return err
			}
			var got uint64
			if hit {
				got = d.Position.Seq
			}
			if got != tc.at {
				t.Errorf("%s: a probe over %q found the record at %d (hit %v), "+
					"want %d", tc.name, tc.probe, got, hit, tc.at)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: probe: %v", tc.name, err)
		}
	}
}

// openProbeEstate is a fresh node with its replicated estate open, where a
// domain's deferral tables live, so the probe tables are made there.
func openProbeEstate(ctx context.Context, t *testing.T) store.ReplicatedHandle {
	t.Helper()
	db, err := store.OpenNode(ctx, filepath.Join(t.TempDir(), "probe.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.OpenReplicated(ctx, 1); err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	return db.Replicated()
}
