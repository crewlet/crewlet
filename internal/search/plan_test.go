package search_test

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// EVERY INDEX ON THE VECTOR TABLES SERVES A QUERY THIS PACKAGE ISSUES, and the
// two-stage search reaches `kb_vectors` by its PRIMARY KEY.
//
// # Why this is a gate rather than a comment
//
// Both halves of this were wrong when the tables were created, and both were
// wrong SILENTLY — the answers were correct, the tests passed, and the search
// took a hundred seconds instead of fifty milliseconds. An index is the one
// piece of schema whose mistake looks exactly like a mistake nobody made.
//
// Measured at the pin, 20 000 sources at 3 072 dimensions, warm: the two-stage
// search was 1 min 44 s with the two indexes 0024 created, 50 ms without them,
// against 259 ms for the exact scan they were meant to beat. Two thousand
// times, from two indexes nobody would look at twice.
//
// # What it walks
//
// Every statement this package issues against the vector tables, run through
// EXPLAIN QUERY PLAN on a corpus large enough that the planner has a real
// choice. Then the inverse: every index the schema declares must appear in at
// least one of those plans, or it is a copy of the row order nobody reads and
// a trap the planner can still fall into.
func TestEveryIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db, dim, model := seedVectors(t, 2_000)
	query := randomEmbedding(rand.New(rand.NewPCG(21, 21)), dim)

	// THE REGISTERED QUERIES: every read this package makes of the vector
	// tables. A statement not here is a statement whose plan nothing checks.
	registered := map[string]struct {
		statement string
		args      []any
	}{
		"the two-stage search": {`
			SELECT c.source, c.source_id, c.container,
			       vector_distance_cos(v.embedding, ?) AS distance
			FROM (
				SELECT b.source, b.source_id, b.container
				FROM kb_vectors_bin b
				WHERE b.model = ? AND b.dim = ?
				ORDER BY vector_distance_cos(b.bits, vector1bit(?)),
				         b.source, b.source_id
				LIMIT ?
			) AS c
			JOIN kb_vectors v ON v.source = c.source AND v.source_id = c.source_id
			WHERE length(v.embedding) = ?
			ORDER BY distance, c.source, c.source_id
			LIMIT ?`,
			[]any{query, model, dim, query, 1200, 4 * dim, 150}},
		"the evaluation's exact scan": {`
			SELECT source, source_id FROM kb_vectors
			WHERE model = ? AND dim = ? AND length(embedding) = ?
			ORDER BY vector_distance_cos(embedding, ?), source, source_id
			LIMIT ?`,
			[]any{model, dim, 4 * dim, query, 150}},
		"the embedding spaces an operator reads": {`
			SELECT model, dim, COUNT(*) FROM kb_vectors
			WHERE embedding IS NOT NULL GROUP BY model, dim`, nil},
		"the embed duty's anti-join": {`
			SELECT t.id FROM tracker_tasks t
			LEFT JOIN kb_vectors v ON v.source = 'task' AND v.source_id = t.id
			WHERE t.removed_at IS NULL
			  AND (v.source_id IS NULL OR v.source_rev <> t.version
			       OR v.model <> ? OR v.dim <> ?)
			ORDER BY t.updated_at LIMIT ?`,
			[]any{model, dim, 100}},
	}

	plans := map[string][]string{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		for name, q := range registered {
			steps, err := explain(t, tx, q.statement, q.args...)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			plans[name] = steps
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// THE JOIN REACHES kb_vectors BY ITS PRIMARY KEY. This is the exact
	// failure that cost the hundred seconds: an index leading on `source`
	// alone made every candidate scan half the table.
	joined := strings.Join(plans["the two-stage search"], "\n")
	if !strings.Contains(joined, "sqlite_autoindex_kb_vectors_1") ||
		!strings.Contains(joined, "source_id=?") {
		t.Fatalf("the second stage reaches kb_vectors by something other than "+
			"its primary key:\n%s\n\nA seek on `source` alone scans half the "+
			"company per candidate, through rows twelve kilobytes wide — "+
			"measured at 1 min 44 s against 50 ms", joined)
	}
	// AND THE FIRST STAGE SCANS THE NARROW TABLE. The cost model is
	// N x c_row over 400-byte rows; an index over it is that scan plus a
	// random access per row.
	if !strings.Contains(joined, "SCAN kb_vectors_bin") {
		t.Fatalf("the first stage does not scan kb_vectors_bin:\n%s", joined)
	}

	// THE INVERSE: an index no registered plan reaches.
	var declared []string
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			SELECT name FROM sqlite_master
			WHERE type = 'index' AND tbl_name IN ('kb_vectors', 'kb_vectors_bin')
			  AND name NOT LIKE 'sqlite_autoindex%'`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			declared = append(declared, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(declared) == 0 {
		t.Fatal("no indexes were read back, so this half of the check " +
			"cannot fail and is not asserting anything")
	}
	all := ""
	for _, steps := range plans {
		all += strings.Join(steps, "\n") + "\n"
	}
	for _, index := range declared {
		if !strings.Contains(all, index) {
			t.Errorf("index %s serves no registered query — it is a second "+
				"copy of the row order nobody reads, and a trap the planner "+
				"can still choose over the primary key:\n%s", index, all)
		}
	}
	t.Logf("vector indexes: %v", declared)
}

// explain runs one statement through EXPLAIN QUERY PLAN.
func explain(t *testing.T, tx *sql.Tx, statement string, args ...any) ([]string, error) {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, rows.Err()
}
