package learning

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// Hit is one recalled episode with its similarity.
type Hit struct {
	Episode    Episode
	Similarity float64
}

// RecallQuery bounds a similarity search.
type RecallQuery struct {
	Handle    string
	Embedding []float32

	// Limit is how many hits to return. 0 takes a small default: recall
	// goes into a prompt, and a dozen half-relevant memories crowd out the
	// task they were fetched for.
	Limit int

	// MinSimilarity floors what counts as a memory. Cosine similarity over
	// unrelated text still lands well above zero, so with no floor the
	// nearest N rows always come back — a seat with three episodes recalls
	// all three on every turn, however irrelevant.
	MinSimilarity float64

	// Kinds filters row shapes. Empty means raw episodes only: a compacted
	// cluster summarises many turns and reads in a prompt like one turn
	// that did all of them.
	Kinds []Kind
}

const (
	defaultRecallLimit = 5
	// defaultMinSimilarity is the floor when a caller states none.
	//
	// 0.3 rather than 0: two unrelated sentences from one embedding model
	// routinely score 0.1–0.25, so a zero floor returns the nearest rows
	// whatever they are. It is deliberately generous — the cost of a
	// marginal memory is prompt tokens, and the cost of missing the
	// relevant one is the seat repeating work it has already done.
	defaultMinSimilarity = 0.3
)

// Recall returns a seat's most similar past episodes.
//
// A SCAN, and the database does the arithmetic. There is still no ANN index
// reachable from the Go driver, re-measured at the pin, so every embedded
// row for the seat is visited — what the per-seat time index
// buys is that it is one seat's episodes rather than the whole table. A
// company's seat has thousands of turns, not millions.
//
// It used to visit them in Go: select every row, decode every vector, cosine
// each one. That was written when a second driver with no vector functions
// had to be served and it then ran on BOTH drivers unconditionally, because
// nothing ever called the other path. With one driver the
// ordering is `vector_distance_cos` in an ORDER BY, and only the rows that
// survive the LIMIT cross the driver boundary.
//
// Measured on this store, 5 000 episodes of 1 536 dimensions with ~1 KB of
// text apiece, keeping 5: the Go loop was 144 ms and 35.8 MB across the driver
// boundary; this is 34 ms and 35.8 KB.
//
// Rows with no embedding are skipped rather than scored: they were written
// during an embeddings outage, and treating a missing vector as a zero vector
// would score them as maximally dissimilar to everything and rank them
// consistently last — which reads as a judgment about their content.
//
// An episode scores as its NEAREST window — its summary is embedded whole in
// windows rather than cut, so a long turn is reachable by a query matching any
// part of what it did. [EpisodeWindowBytes] is why, and the statement below is
// how.
func (e *Episodes) Recall(ctx context.Context, q RecallQuery) ([]Hit, error) {
	if q.Handle == "" {
		return nil, fmt.Errorf("learning: recall needs a seat")
	}
	if len(q.Embedding) == 0 {
		return nil, ErrNoEmbedding
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	floor := q.MinSimilarity
	if floor == 0 {
		floor = defaultMinSimilarity
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = []Kind{KindRaw}
	}
	probe, width, err := vectorProbe(e.db, q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}
	widest, err := e.widestWindowSet(ctx, q.Handle, width)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}

	// RANK IDS, THEN FETCH THE WINNERS BY PRIMARY KEY.
	//
	// The inner query carries only what the ranking needs — an id, the
	// tie-break column and the distance — so the sort over the seat's whole
	// row set moves a few dozen bytes per row instead of the ~7 KB an
	// episode weighs once its text columns and its 6 KB vector are in
	// scope. Same fixture as above: 78 ms carrying every column through the
	// sort, 34 ms this way.
	//
	// The outer statement returns an UNORDERED set, which is fine here and
	// would be a bug in a query that returned it to a caller: [rank] below
	// imposes the total order in Go and deliberately does not lean on the
	// order the database gave. That was already true — it is written down
	// at rank — and this shape is what makes it load-bearing rather than
	// belt-and-braces.
	//
	// AN EPISODE SCORES AS ITS NEAREST WINDOW. A summary is embedded whole,
	// in windows (see [EpisodeWindowBytes]), packed end to end into the one
	// blob the row has with `embedding_windows` saying how many — so the
	// distance is a MIN over the windows rather than one call on the column.
	// MIN rather than a mean for the same reason internal/search gives: a
	// turn is worth recalling because part of what it did matches the query,
	// and averaging would rank a one-line summary that is entirely on topic
	// above a long one with a perfect paragraph, which is the opposite of
	// what windowing is for.
	//
	// ONE BRANCH PER ROW SHAPE, UNION ALL. A row with one window is scored
	// by one call on its whole blob and never reads `ord` at all; only a
	// multi-window row takes the ordinal path. That is not a tidiness split,
	// it is the only shape in which one long episode does not tax every
	// other recall the seat ever runs:
	//
	//   - `ord` has as many rows as the seat's WIDEST episode, and the
	//     correlated subquery over it is re-entered for every candidate row
	//     — so in a single branch the scan costs (rows × widest) whatever
	//     the rows themselves hold. [BenchmarkRecallOneLongEpisode], 2 000
	//     one-window rows plus ONE long episode, single branch: 12.2 ms with
	//     no long row, 17.5 ms with one of 20 windows, 61.8 ms with one of
	//     150. The long row is not what costs that — 20 vectors is 20 more
	//     distances — the 2 000 rows owning ONE window are, each re-scanning
	//     a 20- or 150-row ordinal list to rediscover that `ord.k < 1`.
	//   - Split, the same three cells are 10.3 ms, 10.5 ms and 24.9 ms: a
	//     seat's ordinary episodes stop paying for its longest one, and the
	//     first cell is the single-vector statement's own number, so the
	//     common row's plan is what it was before windowing existed.
	//   - There is NO CEILING on an episode's window count (see
	//     [EpisodeWindowBytes]), so the taxed case is one the design admits
	//     on purpose rather than a pathology — 20 windows is a ~70 KB
	//     summary, which a coalesced trigger reaches. A cap here would be
	//     the silent cut this replaced, moved into the read path.
	//
	// Both branches SEEK. The left one takes episodes_agent_ended_at_idx
	// (agent_handle=?), the seat scoping schema/node/0002 exists for; the
	// right one takes episodes_agent_windows_idx (agent_handle=? AND
	// embedding_windows>?), the partial index node migration 0030 ships —
	// which is why that index earns its place twice, once for the ordinal
	// count and once to find the rare rows that need it. Neither is a SCAN,
	// and TestRecallScansOneSeatRatherThanTheTable is the guard.
	//
	// The residue is a multi-window row's own cost, and it is quadratic in
	// its windows: `substr` over a packed blob loads the row's whole blob
	// per call, so 150 windows is 150 reads of ~900 KB — the whole of the
	// 24.9 ms cell above, for that ONE row. It stays because the alternative is a side table keyed
	// by (episode, window), which node migration 0030 rejects for a reason
	// that has not changed: an episode row rides the memsync compacted
	// changelog, and a second subject family for its windows could never
	// shrink. What the split buys is that this cost is confined to the rows
	// that caused it.
	//
	// `ord` is the window ordinals and the MIN over them is a CORRELATED
	// SCALAR SUBQUERY. Both halves of that are plan decisions, both are
	// load-bearing, and both were measured on the fixture above (2 000
	// episodes, 1 536 dimensions) against the single-vector statement's
	// 11 ms:
	//
	//   - A SCALAR SUBQUERY RATHER THAN A JOIN WITH A GROUP BY. Written as
	//     `JOIN ord … GROUP BY e.id`, the planner takes the PRIMARY KEY
	//     index to satisfy the grouping and walks the WHOLE episodes table
	//     in id order — throwing away exactly what
	//     episodes_agent_ended_at_idx is for (schema/node/0002: the
	//     agent-scoping is "what keeps that scan over one seat's thousands
	//     of rows rather than the whole table"), which on a node running
	//     twenty seats is a twentyfold regression nothing would report.
	//     As a scalar subquery the outer shape is the single-vector
	//     statement's, so EXPLAIN QUERY PLAN still says SEARCH e USING
	//     INDEX episodes_agent_ended_at_idx (agent_handle=?).
	//   - MATERIALIZED, AND THE COUNT IS A BOUND PARAMETER. A recursive CTE
	//     read from a correlated subquery is re-run PER OUTER ROW, and the
	//     re-run is not cheap even when it yields one row: 296 ms with the
	//     count bound, and 8.5 seconds when the CTE also had to re-derive
	//     the count from a per-seat MAX. MATERIALIZED pins it to one
	//     evaluation — a VALUES list built in Go measures the same and was
	//     rejected for it, because it makes the statement text vary with
	//     the count and buys nothing.
	//
	// So the count arrives from [Episodes.widestWindowSet], one cheap read
	// before this one.
	//
	// THE WIDTH FILTER IS NOW PER WINDOW — `embedding_windows * ?` — and
	// that multiplication is the whole reason migration 0030 stores a count
	// rather than deriving one: it makes the guard exact again, where
	// dividing the length by a width would accept a row embedded in another
	// model's space whenever the two divide.
	//
	// The kind filter is a bound list of short literals rather than
	// placeholders because it comes from a typed enum this package owns —
	// see kindList.
	rows, err := e.db.SQL().QueryContext(ctx, recallStatement(kinds),
		widest, probe, q.Handle, width, width, width, probe, q.Handle, width, 1-floor, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}
	candidates, err := collectEpisodes(rows)
	if err != nil {
		return nil, err
	}

	// SCORED AGAIN IN GO, over the rows that survived rather than over all
	// of them. Two reasons, and neither is distrust of the ordering — the
	// two agree to eight decimal places (measured).
	//
	// The first is that `Hit.Similarity` is read by callers and rendered
	// into prompts, so it has to keep meaning exactly what it meant: the
	// value [cosine] returns, computed in float64 from the same vectors.
	//
	// The second is a guard the SQL cannot express. vector_distance_cos
	// answers 0 — a PERFECT match — for a vector holding a NaN or an
	// infinity, so a single poisoned embedding would sort itself to the top
	// of every recall this seat ever ran. [cosine] rejects those, and the
	// rows it rejects are dropped here. That costs at most `limit`
	// computations times the row's window count, against the thousands the
	// loop used to do — and a poisoned WINDOW now costs its window rather
	// than its episode, which is [nearestWindow]'s own note.
	var hits []Hit
	for _, ep := range candidates {
		sim, ok := nearestWindow(q.Embedding, ep.Embeddings)
		if !ok || sim < floor {
			continue
		}
		hits = append(hits, Hit{Episode: ep, Similarity: sim})
	}
	rank(hits)
	return hits, nil
}

// widestWindowSet is how many window ordinals a recall over this seat needs.
//
// A SEPARATE READ, because inside the recall statement this question cost the
// recall: a per-seat MAX derived inside the ordinal CTE was re-evaluated for
// every row the scan visited, and turned an 11 ms recall into 8.5 seconds on
// 2 000 episodes (measured). Asked here it is one seek — 0.14 ms on the same
// fixture — against the partial index node migration 0030 ships for it.
//
// PARTIAL ON `embedding_windows > 1`, which is what makes the index nearly
// empty and the seek nearly free: a summary short enough to be one window is
// almost every summary. It answers exactly anyway, because a seat with no row
// above one HAS a maximum of one — COALESCE supplies it — and one ordinal is
// all a single-window row can use.
//
// SCOPED TO THE PROBE'S WIDTH, the same predicate the recall's own filter
// applies, so the ordinal count is exactly as long as the rows it will score:
// a row from another embedding space, or one whose count and blob disagree,
// is excluded from both rather than lengthening the list for a row that is
// never scored.
//
// WHAT A CONCURRENT WRITE COSTS IS BOUNDED AND SELF-CORRECTING. A row written
// between this read and the recall's own can hold MORE windows than this
// answers, and it is then scored on its first `widest` of them for that one
// query — never excluded, and never a loss of anything stored: every window
// is on the row, [nearestWindow] re-scores whatever the statement returns over
// ALL of them, and the next recall generates enough ordinals. Closing that gap
// would mean holding both reads in one transaction, and this store's
// transactions take the file's single write lock at BEGIN (see
// internal/store/begin.go) — so an exact answer here would serialise every
// seat's turn-start recall against every writer in the process.
func (e *Episodes) widestWindowSet(ctx context.Context, handle string, width int) (int, error) {
	var widest int
	err := e.db.SQL().QueryRowContext(ctx, widestWindowStatement,
		handle, width).Scan(&widest)
	if err != nil {
		return 0, err
	}
	// A seat with no comparable multi-window row still needs ordinal 0, and
	// a stored count below one names no window at all — both answer one.
	return max(widest, 1), nil
}

// nearestWindow is the similarity of the query to the CLOSEST of an episode's
// window vectors, and false when none of them can be scored.
//
// THE SAME ARITHMETIC THE SQL DID, in the direction Hit.Similarity is read in:
// the statement above minimises a cosine DISTANCE and this maximises a cosine
// SIMILARITY over the same set, so the window it picks is the same one. Written
// out rather than inferred from the distance the statement computed, because
// that distance is not carried back — see the comment at the call site for the
// two things this pass exists to do that the SQL cannot.
//
// A window [cosine] refuses does not disqualify the episode: refusal means that
// ONE vector is unusable (a poisoned component, a width that does not match),
// and the row is still worth ranking on the windows that are fine. It is only
// when no window scores at all that the episode drops out, which is the same
// answer the single-vector shape gave for its one unusable vector.
func nearestWindow(query []float32, windows [][]float32) (float64, bool) {
	best, scored := 0.0, false
	for _, window := range windows {
		sim, ok := cosine(query, window)
		if !ok {
			continue
		}
		if !scored || sim > best {
			best, scored = sim, true
		}
	}
	return best, scored
}

// recallStatement is the seat-similarity scan, for one kind filter.
//
// TWO BRANCHES OVER ONE SEAT, and the predicates partition the rows rather
// than overlapping: `embedding_windows = 1` and `embedding_windows > 1`, each
// guarded by the width check its own shape needs, so no episode can be scored
// twice and none of them falls between the two. A row with a count of zero is
// a row with no vector — the writer stores NULL and 0 together — and it is
// excluded by the same `embedding IS NOT NULL` both branches carry, which the
// right-hand one states although its length check already implies it: an
// asymmetry there reads as an omission, and the next reader has to redo this
// paragraph to find out it is not one.
//
// A FUNCTION rather than a literal at the call site, because the plan this
// statement gets is an invariant with a test —
// TestRecallScansOneSeatRatherThanTheTable runs EXPLAIN QUERY PLAN over
// exactly this text — and a statement a test has to retype is a statement the
// test stops describing.
func recallStatement(kinds []Kind) string {
	return `WITH RECURSIVE ord(k) AS MATERIALIZED (
	     SELECT 0
	     UNION ALL
	     SELECT ord.k + 1 FROM ord WHERE ord.k + 1 < ?
	 )
	 SELECT ` + episodeColumns + ` FROM episodes WHERE id IN (
	    SELECT id FROM (
	        SELECT e.id AS id, e.ended_at AS ended_at,
	               vector_distance_cos(e.embedding, ?) AS distance
	        FROM episodes e
	        WHERE e.agent_handle = ?
	          AND e.embedding IS NOT NULL
	          AND e.embedding_windows = 1
	          AND length(e.embedding) = ?
	          AND e.kind IN (` + kindList(kinds) + `)
	        UNION ALL
	        SELECT e.id AS id, e.ended_at AS ended_at,
	               (SELECT MIN(vector_distance_cos(
	                    substr(e.embedding, 1 + ord.k * ?, ?), ?))
	                  FROM ord WHERE ord.k < e.embedding_windows) AS distance
	        FROM episodes e
	        WHERE e.agent_handle = ?
	          AND e.embedding IS NOT NULL
	          AND e.embedding_windows > 1
	          AND length(e.embedding) = e.embedding_windows * ?
	          AND e.kind IN (` + kindList(kinds) + `)
	    )
	    WHERE distance <= ?
	    ORDER BY distance ASC, ended_at DESC, id DESC
	    LIMIT ?
	 )`
}

// widestWindowStatement is the ordinal-count read, and it is named for the
// same reason recallStatement is: its plan is the whole point of the partial
// index node migration 0030 adds, and the test that asserts it must read the
// statement rather than a copy of it.
const widestWindowStatement = `SELECT COALESCE(MAX(embedding_windows), 1) FROM episodes
	 WHERE agent_handle = ?
	   AND embedding_windows > 1
	   AND length(embedding) = embedding_windows * ?`

// kindList renders a kind filter as SQL literals.
//
// The values are this package's own typed enum — [KindRaw] and
// [KindCompacted], both fixed identifiers — so there is no caller input in the
// statement. Placeholders would be safer against a future where that stops
// being true, and would also make the statement text vary with the number of
// kinds, which costs a prepared-statement entry per shape; the enum being
// closed is what makes the trade honest. A value outside it renders as a
// quoted string that matches no row, which is the same answer a placeholder
// would give.
func kindList(kinds []Kind) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, "'"+strings.ReplaceAll(string(k), "'", "''")+"'")
	}
	return strings.Join(out, ", ")
}

// vectorProbe packs a query embedding for binding, and reports the byte width
// a stored row must have to be comparable with it.
//
// THE WIDTH FILTER IS NOT AN OPTIMISATION. vector_distance_cos fails the whole
// statement on a width mismatch — "Vectors must have the same dimensions",
// raised during iteration, after the query has already succeeded — and a
// company that changes its embedding model leaves exactly those rows behind.
// The Go loop skipped them silently (cosine returns false on a shape
// mismatch); without the length filter the SQL would turn that same history
// into a recall that errors instead of one that returns what it can.
//
// The filter its callers build is `length(embedding) = embedding_windows * W`
// rather than `= W`, because a row's blob holds one vector per window of its
// text. The count is what keeps the comparison EXACT: dividing the length by W
// instead would admit a row from another model's space whenever the two widths
// happen to divide, and that row sorts on nonsense and spends a slot of the
// LIMIT before the Go pass can refuse it.
func vectorProbe(db *store.DB, embedding []float32) ([]byte, int, error) {
	blob, err := db.EncodeVector(embedding)
	if err != nil {
		return nil, 0, err
	}
	return blob, len(blob), nil
}

// rank orders hits: most similar first, then most recent, then by id
// descending.
//
// A TOTAL order, and self-contained: it does not lean on the order the
// database returned. SQL leaves the order of equal ORDER BY keys unspecified,
// and SQLite happens to give insertion order — which means a tie-break that
// deferred to the input would be correct today and silently change with a
// query plan. Two episodes of one recurring task score identically, and a seat
// that recalled a different one on every turn would be reacting to its own
// storage layout.
//
// Split out so it can be exercised on a shuffled slice. Through Recall the
// database's incidental determinism masks it completely.
func rank(hits []Hit) {
	// EVERY KEY DESCENDS — most similar, then newest, then the higher id —
	// so every compare takes its arguments reversed.
	slices.SortFunc(hits, func(a, b Hit) int {
		return cmp.Or(
			cmp.Compare(b.Similarity, a.Similarity),
			b.Episode.EndedAt.Compare(a.Episode.EndedAt),
			cmp.Compare(b.Episode.ID, a.Episode.ID),
		)
	})
}

// cosine returns the cosine similarity of two vectors, and false when it is
// not defined for them.
//
// Undefined in three ways, all of which happen: mismatched widths (a company
// that changed embedding model mid-life), a zero vector (an embedding of empty
// text), and a non-finite component (a provider returning NaN or an
// overflowed value). The first is caught by shape; the other two both arrive
// at the same place — a NaN result — which is why there is ONE check for them
// rather than one each.
//
// It matters that they are caught at all rather than ranked: a NaN compares
// false against everything, so a single one lands wherever the sort's pivot
// choices put it, and a seat's recall order becomes a property of its data
// layout.
func cosine(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// A zero vector divides by zero; a non-finite component propagates.
	// Both land on NaN, and Cauchy-Schwarz bounds a finite result to
	// [-1, 1], so an infinity here could only come from the same place.
	if math.IsNaN(sim) || math.IsInf(sim, 0) {
		return 0, false
	}
	return sim, true
}

// There is no VectorSearchAvailable here any more.
//
// It reported db.Caps().VectorFunctions "for the operator surface", and no
// operator surface ever called it — while recall, the one thing the answer was
// about, ignored it and ran the Go loop either way. Recall is now written
// against vector_distance_cos on the one driver that has it,
// so the question has one answer for a given build, and the place it is
// actually reported is the store_opened log line, which prints all three
// capabilities at every start.
