package search_test

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
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

// EVERY INDEX ON THE LEXICAL TABLES SERVES A QUERY THIS PACKAGE ISSUES, and
// the posting scan reaches kb_docs by its PRIMARY KEY.
//
// The vector gate above exists because two indexes nobody looked at twice cost
// two thousand times the runtime. This is the same gate over the other estate,
// and it is the estate the shard column arrived in most recently — a bucket
// range is the one predicate a planner can serve from an index, which is
// exactly how it would stop driving on the term.
//
// # The shape that must not change
//
// A term's posting list is the outer loop and kb_docs is reached per posting
// by its primary key. Any plan that drives on kb_docs instead walks a bucket
// range — one sixty-fourth of the corpus, which is far more documents than a
// term appears in — and probes the postings per document.
func TestEveryLexicalIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	// A TERM THAT IS NOT IN EVERY DOCUMENT. If the queried term appeared in
	// the whole corpus, the posting list and the bucket range would be the
	// same size and driving on either would be a defensible plan — which
	// would make this gate agree with a plan that is wrong on a real
	// corpus, where a term is in a small fraction of the documents.
	for i := range 1_000 {
		container := "ENG"
		if i%3 == 0 {
			container = "OPS"
		}
		body := "this page is about retention and the sweep that enforces it"
		if i%20 == 0 {
			body = "the migration plan is here"
		}
		page(t, db, fmt.Sprintf("p.%04d", i), container,
			fmt.Sprintf("Doc %04d", i), body, 1)
	}
	indexAll(t, x)

	half := 32
	registered := map[string]struct {
		statement string
		args      []any
	}{
		// THE POSTING SCAN, in the shape a fan-out issues it: a term, a
		// container filter and a bucket range at once, which is the plan
		// with the most for a planner to get wrong.
		"the posting scan under an assignment": {`
			SELECT p.doc_id, p.freq, d.length
			  FROM kb_postings p
			  JOIN kb_docs d ON d.id = p.doc_id
			 WHERE p.term = ? AND d.container IN (?)
			   AND d.search_shard >= ? AND d.search_shard < ?
			 ORDER BY p.freq DESC
			 LIMIT ?`,
			[]any{"migration", "ENG", 0, half, 5_000}},
		"the posting scan with no assignment": {`
			SELECT p.doc_id, p.freq, d.length
			  FROM kb_postings p
			  JOIN kb_docs d ON d.id = p.doc_id
			 WHERE p.term = ?
			 ORDER BY p.freq DESC
			 LIMIT ?`,
			[]any{"migration", 5_000}},
		"the term's document frequency": {
			`SELECT COUNT(*) FROM kb_postings WHERE term = ?`,
			[]any{"migration"}},
		"the corpus statistics": {
			`SELECT COUNT(*), COALESCE(AVG(length), 0) FROM kb_docs`, nil},
		"the hit hydration": {`
			SELECT id, source, source_id, container, title, excerpt
			  FROM kb_docs WHERE id IN (?, ?)`,
			[]any{"page:p.0001", "page:p.0002"}},
		"what the index already holds": {
			`SELECT source_id, source_rev FROM kb_docs WHERE id IN (?, ?)`,
			[]any{"page:p.0001", "page:p.0002"}},
		"the orphan walk": {`
			SELECT source_id FROM kb_docs WHERE source = 'page' AND source_id > ?
			  ORDER BY source_id LIMIT ?`,
			[]any{"", 200}},
		"the indexed page count": {
			`SELECT COUNT(*) FROM kb_docs WHERE source = 'page'`, nil},
		"one document's removal": {
			`DELETE FROM kb_docs WHERE source = ? AND source_id = ?`,
			[]any{"page", "p.0001"}},
		"one document's postings": {
			`DELETE FROM kb_postings WHERE doc_id = ?`, []any{"page:p.0001"}},
	}

	plans := map[string][]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
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

	// THE TERM DRIVES, IN BOTH SHAPES. A bucket range the planner can seek
	// is the one thing that would make kb_docs the outer table, and it
	// would do it silently: the answers stay correct and only a fan-out's
	// searches get slow.
	for _, name := range []string{
		"the posting scan under an assignment",
		"the posting scan with no assignment",
	} {
		joined := strings.Join(plans[name], "\n")
		if !strings.Contains(joined, "kb_postings") ||
			!strings.Contains(strings.SplitN(joined, "\n", 2)[0], "kb_postings") {
			t.Fatalf("%s does not drive on the posting list:\n%s\n\nDriving on "+
				"kb_docs walks every document in the assignment and probes the "+
				"postings per document — a term appears in far fewer documents "+
				"than a bucket range holds", name, joined)
		}
		if !strings.Contains(joined, "sqlite_autoindex_kb_postings_1") {
			t.Fatalf("%s reaches the postings by something other than the "+
				"(term, doc_id) primary key:\n%s", name, joined)
		}
		if !strings.Contains(joined, "kb_docs") ||
			!strings.Contains(joined, "id=?") {
			t.Fatalf("%s reaches kb_docs by something other than its primary "+
				"key:\n%s", name, joined)
		}
	}

	// THE INVERSE: an index no registered plan reaches is a copy of the row
	// order nobody reads, and one the planner can still choose.
	var declared []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			SELECT name FROM sqlite_master
			WHERE type = 'index' AND tbl_name IN ('kb_docs', 'kb_postings')
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
	t.Logf("lexical indexes: %v", declared)
}
