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
// search was 1 min 44 s with the two indexes the replicated estate's migration
// 0003 created, 50 ms without them, against 259 ms for the exact scan they were
// meant to beat. Two thousand times, from two indexes nobody would look at
// twice.
//
// # What it walks
//
// Every statement this package issues against the vector tables — the
// two-stage search in each shape its callers issue it, through the index and
// through the full scan, built by the function [search.Semantic] calls, and the
// reads the index's own duty and applier make — run through EXPLAIN QUERY PLAN
// on a corpus large enough that the planner has a real choice. Then the
// inverse: every index the schema declares must appear in at least one of
// those plans, or it is a copy of the row order nobody reads and a trap the
// planner can still fall into.
func TestEveryIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	const dim, model = 32, "plan-embed"
	db, generation := indexedStore(t, model, dim, 2_000)
	query := randomEmbedding(rand.New(rand.NewPCG(21, 21)), dim)

	// THE REGISTERED QUERIES: every read this package makes of the vector
	// tables. A statement not here is a statement whose plan nothing checks.
	type registration struct {
		statement string
		args      []any
	}
	reassign, reassignArgs := search.ReassignStatement(search.RolloutRange{
		Source: search.SourcePage, From: "s00100", To: "s00400",
	}, search.IndexHead{Generation: generation, Model: model, Dim: dim})
	lists := `[0, 1, 2]`
	registered := map[string]registration{
		"the evaluation's exact scan": {search.ExactTopsStatement(2),
			[]any{query, query, model, dim, 4 * dim}},
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
		"the index duty's count of the space": {search.SpaceCountStatement,
			[]any{model, dim}},
		"the training's codes": {search.TrainingCodesStatement, []any{model, dim}},
		"the rollout's unfiled rows": {search.StaleKeysStatement,
			[]any{model, dim, generation}},
		"a reassign's range": {reassign, reassignArgs},
		"the probe's look for unfiled rows": {search.StaleRowsStatement,
			[]any{model, dim, generation}},
		"a narrowed probe's count of its rows": {search.MatchingCountStatement(
			[]string{"b.source IN (?)", "b.container IN (?)"}),
			[]any{lists, model, dim, generation, "page", "ENG"}},
	}
	// THE TWO-STAGE SEARCH IN EVERY SHAPE A CALLER ISSUES IT, built by the
	// function Semantic itself calls rather than copied: a copy is a plan
	// for a statement nothing runs. The copy this replaced had no filter at
	// all, and both callers in the tree pass a source — the knowledge search
	// its container scope as well, and a fan-out its bucket range. Each
	// shape twice: through the index, and forced to the full scan.
	//
	// ONE PROBE A SEARCH, so every narrowed shape reads few enough lists to
	// stay on the index ([search.ProbeCount]) — a plan of the index is what
	// is being checked, and a narrowed shape handed to the scan would
	// certify the scan's plan twice.
	shapes := map[string]search.SemanticQuery{
		"the two-stage search": {},
		"the two-stage search over one source": {
			Sources: []search.Source{search.SourceTask},
		},
		"the two-stage search over one source in a scope": {
			Sources:    []search.Source{search.SourcePage},
			Containers: []string{"ENG", "OPS"},
		},
		"the two-stage search over one source under an assignment": {
			Sources:    []search.Source{search.SourcePage},
			Containers: []string{"ENG"},
			Shards:     search.Assignment{From: 0, To: 32},
		},
		"the two-stage search under an assignment": {
			Shards: search.Assignment{From: 16, To: 48},
		},
	}
	for name, shape := range shapes {
		shape.Probes = 1
		shapes[name] = shape
	}
	methods := map[string]search.Stage1Method{}
	build := func(tx *sql.Tx, suffix string, fullScan bool) error {
		for name, shape := range shapes {
			shape.Vector, shape.Model, shape.Dim = query, model, dim
			shape.FullScan = fullScan
			statement, args, report, err := search.SemanticStatement(t.Context(), tx, shape)
			if err != nil {
				return fmt.Errorf("%s: build the statement: %w", name, err)
			}
			registered[name+suffix] = registration{statement, args}
			methods[name+suffix] = report.Method
		}
		return nil
	}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		if err := build(tx, " through the index", false); err != nil {
			return err
		}
		return build(tx, " by the full scan", true)
	}); err != nil {
		t.Fatal(err)
	}
	// AND MID-ROLLOUT: a second index installed and not yet rolled out, so
	// the probe reads the unfiled rows beside its lists.
	second := trainedIndex(t, db, model, dim)
	second.Index.Seed++
	applyAt(t, db, second, 1<<30)
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return build(tx, " mid-rollout", false)
	}); err != nil {
		t.Fatal(err)
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

	for name, method := range methods {
		joined := strings.Join(plans[name], "\n")
		// THE JOIN REACHES kb_vectors BY ITS PRIMARY KEY. This is the exact
		// failure that cost the hundred seconds: an index leading on
		// `source` alone made every candidate scan half the table.
		if !strings.Contains(joined, "sqlite_autoindex_kb_vectors_1") ||
			!strings.Contains(joined, "source_id=?") {
			t.Fatalf("%s: the second stage reaches kb_vectors by something "+
				"other than its primary key:\n%s\n\nA seek on `source` alone "+
				"scans half the company per candidate, through rows twelve "+
				"kilobytes wide — measured at 1 min 44 s against 50 ms",
				name, joined)
		}
		wantMethod := search.Stage1IVF
		if strings.HasSuffix(name, "by the full scan") {
			wantMethod = search.Stage1Scan
		}
		if method != wantMethod {
			t.Fatalf("%s: the first stage planned as %q", name, method)
		}
		switch method {
		case search.Stage1Scan:
			// THE FULL SCAN READS THE NARROW ROWS SEQUENTIALLY — the
			// table, the primary key's own seek on `source` where a
			// source filter gives it one, or the covering index, which is
			// the same narrow rows in another order — and never an
			// index that sends it back to the table per row. The cost
			// model is N x c_row over 400-byte rows, and every
			// uncovering index measured over it was that read plus a
			// random access per row: see the package doc.
			first := firstStage(plans[name])
			if first != "SCAN kb_vectors_bin AS b" &&
				!strings.Contains(first, "USING INDEX sqlite_autoindex_kb_vectors_bin_1") &&
				!strings.Contains(first, "USING COVERING INDEX kb_vectors_bin_ivf_idx") {
				t.Fatalf("%s: the full scan reads kb_vectors_bin through %q:\n%s\n\n"+
					"only a scan, the primary key's own seek or the covering "+
					"index keeps it N x c_row", name, first, joined)
			}
		case search.Stage1IVF:
			// THE PROBE SEEKS EVERY LIST IT READS on the covering index,
			// by the whole key a list is — the space, the generation and
			// the list. A plan that seeks by less reads every list of the
			// generation, and one on any other index reads the table per
			// row: either is the scan's cost for the probe's answer.
			if !strings.Contains(joined,
				"USING COVERING INDEX kb_vectors_bin_ivf_idx (model=? AND dim=? AND ivf_gen=? AND ivf_list=?") {
				t.Fatalf("%s: the probe does not seek each list on the covering "+
					"index:\n%s", name, joined)
			}
			if strings.HasSuffix(name, "mid-rollout") &&
				!strings.Contains(joined, "USING COVERING INDEX kb_vectors_bin_ivf_idx (model=? AND dim=? AND ivf_gen<?)") {
				t.Fatalf("%s: the rows a rollout has not reached are not read "+
					"as one range of the covering index:\n%s", name, joined)
			}
		}
	}
	// A REASSIGN SEEKS ITS RANGE ON THE PRIMARY KEY — it re-files a
	// thousand rows, and a plan that walked the space to find them would
	// cost every holder a partition's read per batch.
	if joined := strings.Join(plans["a reassign's range"], "\n"); !strings.Contains(joined,
		"sqlite_autoindex_kb_vectors_bin_1 (source=? AND source_id>=? AND source_id<?)") {
		t.Fatalf("a reassign's range is not a seek on the primary key:\n%s", joined)
	}
	if joined := strings.Join(plans["the probe's look for unfiled rows"], "\n"); !strings.Contains(joined,
		"kb_vectors_bin_ivf_idx (model=? AND dim=? AND ivf_gen<?)") {
		t.Fatalf("the look for unfiled rows is not one seek on the covering "+
			"index — it runs before every probe:\n%s", joined)
	}
	if joined := strings.Join(plans["the rollout's unfiled rows"], "\n"); !strings.Contains(joined,
		"kb_vectors_bin_ivf_idx (model=? AND dim=? AND ivf_gen<?)") {
		t.Fatalf("the rollout's read of the unfiled rows is not one range of "+
			"the covering index:\n%s", joined)
	}
	// A NARROWED PROBE'S COUNT SEEKS EACH LIST ON THE COVERING INDEX and
	// reads no row of the table: it runs before the probe it sizes.
	if joined := strings.Join(plans["a narrowed probe's count of its rows"], "\n"); !strings.Contains(joined,
		"USING COVERING INDEX kb_vectors_bin_ivf_idx (model=? AND dim=? AND ivf_gen=? AND ivf_list=?") {
		t.Fatalf("a narrowed probe's count does not seek each list on the "+
			"covering index:\n%s", joined)
	}
	// AND THE DUTY COUNTS ITS SPACE ON kb_vectors' MODEL INDEX, every tick —
	// never by walking the wide table or the covering one.
	if joined := strings.Join(plans["the index duty's count of the space"], "\n"); !strings.Contains(joined,
		"USING COVERING INDEX kb_vectors_model_idx") {
		t.Fatalf("the duty's count of its space does not read kb_vectors' "+
			"model index:\n%s", joined)
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

// firstStage is the plan step that reads kb_vectors_bin.
func firstStage(steps []string) string {
	for _, step := range steps {
		if strings.Contains(step, "kb_vectors_bin") || strings.Contains(step, " b USING") {
			return step
		}
	}
	return ""
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
