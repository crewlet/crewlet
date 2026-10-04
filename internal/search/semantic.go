package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The semantic half, as one statement over two tables in one file.
//
// # Two stages, and the first one is now an index — ADR-0028
//
// There is no approximate-nearest-neighbour index on this driver — `caps.go`
// probes for one on every open and it is absent at the pin — so the honest
// choices were a full exact scan, an embedded search library with its own
// index format, file and backup story, or two-stage retrieval. Two-stage is
// what every production vector engine does anyway (DiskANN's rerank, pgvector's
// documented rerank subquery): a narrow table of sign codes is ranked first and
// the survivors are reranked EXACTLY against the vectors they came from, by
// primary key.
//
// For its first years the first stage read every row, and this doc argued —
// correctly, at the sizes it measured — that it needed no index. What changed
// is the corpus: the full scan costs 2.90 µs a source at p95 with one reader
// and 7.34 µs with eight (BenchmarkSemanticIVFUnderLoad's scan arm), so a node
// holding a thousand seats' first-year corpus pays ≈ 2.4 s under load against
// a one-second target. So the first stage is now an INVERTED FILE over the
// same sign codes ([Stage1], ivf.go): the codes are filed in lists by k-means
// in Hamming space, a query ranks the lists by its own code and reads the
// nearest ones, and the exact rerank above it is UNCHANGED. What the index costs and loses is
// measured, per partition, by the training that installs it, and again by
// `crewlet search eval` against the exact scan.
//
// Measured on this container at 3 072 dimensions, at the SHIPPED depths —
// 1 200 candidates, 150 returned — on 40 000 topical sources, p95, with the
// index trained, measured and installed as the embedding duty installs it
// (BenchmarkSemanticIVFUnderLoad):
//
//	                     one reader   eight readers   recall
//	the full scan          116 ms        294 ms       0.994
//	the index, half        73 ms         219 ms       0.992
//
// Half its lists is what that training measured it needed, and the most an
// installed index may probe ([IVFProbeCeiling]); at 10 000 sources the same
// training found nothing worth installing and the scan answered.
//
// The full scan is still the first stage below [IVFMinCorpus], on a partition
// whose training found no probe count worth installing, and during the minute
// a model change leaves a partition with no index in the new space.
//
// THE ONE-READER NUMBER IS NOT THE BUDGET. Eight concurrent readers on four
// cores cost 2.5x (the scan) and 3x (the index) that p95 — see
// BenchmarkSemanticScanUnderLoad and BenchmarkSemanticIVFUnderLoad, which report
// both axes precisely so the idle figure cannot be quoted as the supported
// one.
//
// # The one index on the table it scans, and why every other one stays gone
//
// The full scan's cost model is `N x c_row over a NARROW table`: a row is
// stored contiguously, so what makes it fast is that its rows are 400 bytes
// rather than 12 kilobytes. An index over a table whose rows are ALL read is a
// second copy of the row order plus a random access per row, and every row
// matches `model` and `dim` in a company that has not just changed model —
// measured, the indexed scan was 69 ms against 29 ms for the plain one, and 27
// ms against 20 ms with a container filter matching a third of the corpus.
//
// The inverted file changes the premise rather than the finding: a probe reads
// a SHARE of the rows, so what it needs is a probed list as one contiguous
// range. `kb_vectors_bin_ivf_idx` is that, and it is COVERING — every column
// the first stage reads is in it, so a list is one sequential read of the
// index with no access to the table at all. Replicated migration 0033 has the
// measurement against the two alternatives (an uncovering index on the list,
// and a table clustered on it). A full scan may read the covering index
// instead of the table: it is a copy of the same narrow rows in another order,
// which costs the same sequential read.
//
// The second stage's join is the other half and it cost far more: with an
// index leading on `source` present, the planner used it instead of the
// primary key, and each of 1 200 candidates scanned half of `kb_vectors`
// looking for its `source_id` — 1 min 44 s against 50 ms. Both indexes are
// dropped by the replicated estate's migration 0004, and
// `TestEveryIndexServesARegisteredQuery` is what stops them coming back.
//
// # The query's own sign code is computed inline, and that too is measured
//
// `vector1bit(?)` in the ORDER BY reads like a per-row call. It is not: the
// argument is a bound parameter and nothing else, so it is hoisted — the
// inline form, a one-row CTE cross-joined in, and a code precomputed by a
// separate statement all measure within noise of each other (50 / 49 / 50 ms).
// The inline form is one statement with no join to explain. The probe ranks
// the lists by the same code computed in Go ([Quantize], which agrees with
// `vector1bit` bit for bit), because the centroids are a value this process
// already decoded.
//
// # Why the container filter is on the NARROW table
//
// It is the only place it does any good. Pushing it onto the wide table would
// filter after the scan that the filter exists to shrink, and joining
// `kb_vectors` inside the stage-1 subquery to reach it is the shape the
// hundred seconds above came from.
//
// # Why the source filter has no index of its own, and why that is measured
//
// A filter on this table narrows the part of the scan that COSTS: the sign
// code's distance is computed only for a row the predicate admits, and next to
// it reading a 400-byte row is nearly free. So the filter halves the search
// for a source holding a fifth of the corpus with or without an index — and it
// has one anyway, because the primary key is `(source, source_id)` and the
// planner seeks its automatic index on `source = ?`. The gate asserts that
// plan for the statement this file builds.
//
// Measured at 40 000 sources and 3 072 dimensions, one reader, p50: the
// unfiltered scan 58 ms; a source holding 20 % of the corpus 29 ms, 50 % 48 ms,
// 80 % 66 ms. Every index tried beside it was no faster for a filtered query
// and some were far slower for the unfiltered one, which is 0004's finding
// again: `(model, dim, source)` 85 ms unfiltered, `(model, dim, source,
// search_shard)` 159 ms, and a COVERING `(source, search_shard, model, dim,
// bits, …)` — a second copy of the whole table — 28 ms for the 20 % source
// against the primary key's 29. None narrows the BUCKET range either: this
// planner seeks `source = ?` and reads the range as a residual predicate,
// which is exactly what the table scan already does with it.
//
// What the primary key's plan costs is a random access per admitted row, and a
// source holding MOST of the corpus pays for that: forcing a scan (`+b.source`)
// measured 57 ms against the seek's 66 at 80 % and 41 against 48 at 50 %, and
// 32 against 29 at 20 %. Within the noise of the machine at every mix, and no
// caller can know the mix — so the planner's choice stands.

// SemanticQuery is one semantic search.
type SemanticQuery struct {
	// Vector is the query embedding, PACKED LITTLE-ENDIAN FLOAT32 — the
	// same layout the column holds, which is what makes the width filter
	// a byte-length comparison rather than a declared-type claim.
	Vector []byte

	// Model and Dim are the embedding space. BOTH are required: a model
	// change at the same width leaves two incompatible spaces in one
	// candidate pool, and `vector_distance_cos` over two widths is
	// undefined and fails the whole statement rather than the row.
	Model string
	Dim   int

	// Sources narrows to "page", "task", or both. Empty is both.
	Sources []Source

	// Containers narrows to these containers. EMPTY IS EVERY ONE, which
	// on the native backend means the whole company — the engine IS the
	// boundary here, and there is no second account to launder a read
	// through.
	Containers []string

	// Limit caps the hits. Zero takes [ReturnDepth], which is the depth
	// the fusion below it is written against.
	Limit int

	// Candidates caps stage one. Zero takes [Stage1Depth]; anything above
	// [BinaryCandidateCeiling] is clamped to it.
	Candidates int

	// Shards is the bucket range this scan may read.
	//
	// THE ZERO VALUE IS EVERY BUCKET, which is what a single node has and
	// what every query on a fleet that has not divided its corpus gets. A
	// zero value meaning "no buckets" would answer every search with
	// nothing — silently, because an empty answer and an empty corpus are
	// the same shape.
	//
	// What the predicate buys today is the MEASUREMENT: stage one's cost
	// becomes a function of the bucket range scanned rather than of the
	// corpus held, which is the number a fan-out is decided from.
	Shards Assignment

	// Probes overrides how many lists the index is probed at. ZERO IS THE
	// INDEX'S OWN COUNT — the smallest its training measured meeting the
	// floor — which is what every search runs at; a positive count is an
	// evaluation asking what another one would buy, clamped to the lists
	// the index has.
	Probes int

	// FullScan reads every row of the space whatever index the partition
	// has: the exact first stage the index approximates, which is what an
	// evaluation compares it against. False is the ordinary search.
	FullScan bool
}

// SemanticHit is one ranked document, with the EXACT distance rather than the
// quantized one.
//
// The rerank's whole purpose is that the score a caller sees is computed from
// the full vector: a sign code decides which documents are looked at and never
// how they are ordered.
type SemanticHit struct {
	Source    Source
	ID        string
	Container string
	Distance  float64
}

// SemanticScanBudget bounds one semantic search.
//
// ONE SECOND, which is the interactive target the supported corpus is declared
// against. What fits inside it depends on HOW MANY searches run at once and on
// which first stage runs, and the two benchmarks are where the coefficients
// come from.
//
// Both first stages measured on ONE corpus and one query set
// (BenchmarkSemanticIVFUnderLoad, 40 000 topical sources, 3 072 dimensions,
// p95):
//
//	                          one reader         eight readers
//	the full scan             2.90 µs a source   7.34 µs a source
//	the index, half its lists 1.84 µs            5.48 µs
//
// So a partition answered by the scan holds ≈ 345 000 embeddable sources
// inside the budget while its node is idle and ≈ 136 000 while eight searches
// compete for its cores, and one whose index probes half its lists — the most
// any installed index may ([IVFProbeCeiling]) — ≈ 545 000 and ≈ 183 000. Half
// is the FLOOR of what an index buys rather than its typical figure: a
// partition whose training chose an eighth reads a quarter as many rows as
// one at half, and the probe share a partition's training chose is on the
// `search_index_trained` log line and in `crewlet search eval`.
//
// It is a CEILING on the statement rather than a promise about it — the point
// is that a query over a corpus somebody grew past the projection fails a
// search instead of holding this store's reader for as long as it takes.
const SemanticScanBudget = time.Second

// Stage1Method is how the first stage chose its candidates.
type Stage1Method string

const (
	// Stage1Scan read every row of the embedding space.
	Stage1Scan Stage1Method = "scan"

	// Stage1IVF read the lists of the partition's index nearest the query,
	// plus in full any row a rollout had not yet re-filed.
	Stage1IVF Stage1Method = "ivf"
)

// Stage1Methods is every method, for the enum's validation.
var Stage1Methods = []Stage1Method{Stage1Scan, Stage1IVF}

// Valid reports whether a method off the wire is one this build knows.
func (m Stage1Method) Valid() bool { return slices.Contains(Stage1Methods, m) }

// ScanReason is why a first stage scanned rather than probing an index.
type ScanReason string

const (
	// ScanUnindexed is a partition that has never had an index.
	ScanUnindexed ScanReason = "unindexed"

	// ScanRetired is a partition whose last training IN THE QUERY'S SPACE
	// concluded it should have none ([IndexVerdict] says why).
	ScanRetired ScanReason = "retired"

	// ScanOtherSpace is an index — or a verdict — about another embedding
	// space than the query's: a model change the duty has not re-trained
	// for yet. Whatever it says, it says nothing about this search.
	ScanOtherSpace ScanReason = "other_space"

	// ScanFiltered is a search narrowed to so few rows near its query that
	// the index would have to read more than its ceiling of lists to find
	// as many of them as an unfiltered probe reads ([ProbeCount]) — the
	// full scan reads every matching row instead.
	ScanFiltered ScanReason = "filtered"

	// ScanForced is a caller that asked for the full scan
	// ([SemanticQuery.FullScan]).
	ScanForced ScanReason = "forced"
)

// ScanReasons is every reason, for the enum's validation.
var ScanReasons = []ScanReason{ScanUnindexed, ScanRetired, ScanOtherSpace,
	ScanFiltered, ScanForced}

// Valid reports whether a reason off the wire is one this build knows.
func (r ScanReason) Valid() bool { return slices.Contains(ScanReasons, r) }

// Stage1Report says what the first stage read.
type Stage1Report struct {
	Method Stage1Method

	// Lists is the index's list count and Probed how many of them were
	// read; both zero on a scan. Probed/Lists is the share of the partition
	// the probe read, which is what a search's cost is proportional to.
	Lists, Probed int

	// IVFGeneration is the index the probe read: the packed position of the
	// centroids record that installed it. Zero on a scan.
	IVFGeneration int64

	// Stale reports that the index was mid-rollout: rows it had not
	// re-filed yet were read in full beside the probed lists.
	Stale bool

	// Why is the reason a scan was chosen, empty on a probe.
	Why ScanReason
}

// Candidate is one document the first stage kept, in rank order.
type Candidate struct {
	Source    Source
	ID        string
	Container string
}

// Semantic runs the two-stage search and returns the exactly-reranked hits,
// with what the first stage read.
//
// IT RAISES rather than answering empty, and the seam above it is what turns a
// failure into the empty block a turn tolerates: this is the storage layer,
// where "the store would not answer" and "nothing matched" are different
// facts, and collapsing them here would make a broken index look exactly like
// a company that has written nothing down.
func Semantic(ctx context.Context, tx *sql.Tx, q SemanticQuery) ([]SemanticHit, Stage1Report, error) {
	statement, args, report, err := semanticStatement(ctx, tx, q)
	if err != nil {
		return nil, Stage1Report{}, err
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, Stage1Report{}, fmt.Errorf("search: the semantic scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SemanticHit
	for rows.Next() {
		var hit SemanticHit
		var source string
		if err := rows.Scan(&source, &hit.ID, &hit.Container, &hit.Distance); err != nil {
			return nil, Stage1Report{}, fmt.Errorf("search: read a semantic hit: %w", err)
		}
		hit.Source = Source(source)
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, Stage1Report{}, fmt.Errorf("search: the semantic scan: %w", err)
	}
	return out, report, nil
}

// Stage1 answers the first-stage candidates for one query inside one
// partition's file: the index's probe when the partition has a current index
// in the query's embedding space, and the full scan below [IVFMinCorpus], on
// a partition with no index, or when the caller asks for it.
//
// THE SAME STATEMENT [Semantic] runs its rerank over, built by the same plan,
// so the candidates a partition reports and the ones its own search reranks
// cannot differ.
func Stage1(ctx context.Context, tx *sql.Tx, q SemanticQuery) ([]Candidate, Stage1Report, error) {
	q, err := q.normalised()
	if err != nil {
		return nil, Stage1Report{}, err
	}
	plan, err := planStage1(ctx, tx, q)
	if err != nil {
		return nil, Stage1Report{}, err
	}
	args := append(append([]any{}, plan.withArgs...), plan.bodyArgs...)
	rows, err := tx.QueryContext(ctx, plan.with+plan.body, args...)
	if err != nil {
		return nil, Stage1Report{}, fmt.Errorf("search: the first stage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var source string
		if err := rows.Scan(&source, &c.ID, &c.Container); err != nil {
			return nil, Stage1Report{}, fmt.Errorf("search: read a candidate: %w", err)
		}
		c.Source = Source(source)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, Stage1Report{}, fmt.Errorf("search: the first stage: %w", err)
	}
	return out, plan.report, nil
}

// normalised validates a query and fills its defaulted depths.
func (q SemanticQuery) normalised() (SemanticQuery, error) {
	if len(q.Vector) == 0 {
		return q, fmt.Errorf("search: the semantic half needs a query vector")
	}
	if q.Model == "" || q.Dim <= 0 {
		return q, fmt.Errorf("search: the semantic half needs the model and " +
			"the width the corpus was embedded at — without both, one pool " +
			"holds two embedding spaces and the ranking between them is " +
			"arithmetic on incompatible vectors")
	}
	if want := 4 * q.Dim; len(q.Vector) != want {
		return q, fmt.Errorf("search: the query vector is %d bytes and the "+
			"width says %d — vector_distance_cos over mismatched lengths is "+
			"undefined and fails the whole statement", len(q.Vector), want)
	}
	if q.Limit <= 0 {
		q.Limit = ReturnDepth
	}
	if q.Candidates <= 0 {
		q.Candidates = Stage1Depth
	}
	q.Candidates = min(q.Candidates, BinaryCandidateCeiling)
	// A CANDIDATE POOL SMALLER THAN THE ANSWER is a rerank that cannot
	// fill the page it was asked for, whatever it finds.
	q.Candidates = max(q.Candidates, q.Limit)
	return q, nil
}

// semanticStatement is the one statement [Semantic] issues for q, its
// arguments and what its first stage will read.
//
// A FUNCTION OF ITS OWN so the plan gate explains THIS statement rather than a
// copy of it. The copy the gate used to hold had neither the source filter nor
// the container filter nor the bucket range, and every caller in the tree
// passes at least one of them — so the plan the gate certified was the plan of
// a query nothing issues, and the plans of the queries that ARE issued were
// certified by nobody.
func semanticStatement(ctx context.Context, tx *sql.Tx, q SemanticQuery) (string, []any, Stage1Report, error) {
	q, err := q.normalised()
	if err != nil {
		return "", nil, Stage1Report{}, err
	}
	plan, err := planStage1(ctx, tx, q)
	if err != nil {
		return "", nil, Stage1Report{}, err
	}

	// THE ARGUMENTS ARE BUILT IN STATEMENT ORDER, which is the only order a
	// positional bind has: the probe list's own clause, then the exact
	// distance's vector, then the first stage, then the width filter and
	// the limit.
	args := append([]any{}, plan.withArgs...)
	args = append(args, q.Vector)
	args = append(args, plan.bodyArgs...)
	args = append(args, len(q.Vector), q.Limit)

	// THE TIE BREAK IS DECLARED AT BOTH STAGES. Hamming distance over a
	// 3 072-bit code takes at most 3 073 distinct values whatever the
	// corpus size, so ties are the common case rather than the corner —
	// and without a declared tie break the candidate pool depends on the
	// scan's own row order, so two nodes rerank different pools and a
	// paged answer can repeat or skip a document.
	statement := plan.with + `
		SELECT c.source, c.source_id, c.container,
		       vector_distance_cos(v.embedding, ?) AS distance
		FROM (` + plan.body + `) AS c
		JOIN kb_vectors v ON v.source = c.source AND v.source_id = c.source_id
		WHERE length(v.embedding) = ?
		ORDER BY distance, c.source, c.source_id
		LIMIT ?`
	return statement, args, plan.report, nil
}

// stage1Plan is the first stage as SQL: an optional WITH clause naming the
// probed lists, and a SELECT of (source, source_id, container) in rank order,
// limited to the candidate depth.
type stage1Plan struct {
	with     string
	withArgs []any
	body     string
	bodyArgs []any
	report   Stage1Report
}

// planStage1 decides between the probe and the scan, and builds the SQL.
//
// # How many lists, when the search is narrowed
//
// A search narrowed to a source or to containers keeps only the rows its
// filter matches, so it reads lists nearest first until it has seen as many
// MATCHING rows as the unfiltered probe reads rows, and runs the full scan
// when that would take more than the index's ceiling ([ProbeCount]). How many
// rows of each source every list holds is kb_ivf_lists, which the applier
// keeps beside the rows; a container filter's matches are COUNTED on the
// covering index, a doubling run of lists at a time, since containers are too
// many to keep a count of. A bucket range (a fan-out's slice) narrows nothing:
// the slices are disjoint and every node reads the same lists of its own, so
// together they read exactly the unfiltered probe.
//
// # The probe, and the rows a rollout has not re-filed yet
//
// An index is installed by one record and its rows are re-filed by many — a
// rollout of reassign records the duty publishes right after it. Until the
// last has applied, some rows are still filed under the index this one
// replaced (or under none), and their lists mean nothing here. They are read
// IN FULL beside the probed lists, so the answer is never missing a document
// because a rollout is in flight: the set is bounded by the rollout and
// shrinks as it applies. The union costs a materialised pass over both parts,
// which is why a search checks for such rows first (one seek on the covering
// index) and skips the union when there are none — every search but the ones
// in a rollout's own minute.
func planStage1(ctx context.Context, tx *sql.Tx, q SemanticQuery) (stage1Plan, error) {
	narrowing, narrowingArgs := narrowingFilters(q)
	filters, filterArgs := stage1Filters(q)
	scan := func(why ScanReason) stage1Plan {
		where := append([]string{"b.model = ?", "b.dim = ?"}, filters...)
		args := append([]any{q.Model, q.Dim}, filterArgs...)
		args = append(args, q.Vector, q.Candidates)
		return stage1Plan{
			body: `
			SELECT b.source, b.source_id, b.container
			FROM kb_vectors_bin b
			WHERE ` + strings.Join(where, " AND ") + `
			ORDER BY vector_distance_cos(b.bits, vector1bit(?)),
			         b.source, b.source_id
			LIMIT ?`,
			bodyArgs: args,
			report:   Stage1Report{Method: Stage1Scan, Why: why},
		}
	}
	if q.FullScan {
		return scan(ScanForced), nil
	}
	head, indexed, err := readIndexHead(ctx, tx)
	switch {
	case err != nil:
		return stage1Plan{}, err
	case !indexed:
		return scan(ScanUnindexed), nil
	case !head.InSpace(q.Model, q.Dim):
		// THE SPACE FIRST: a verdict about another space says nothing about
		// this one, and reporting it as this partition's would send an
		// operator after a training of a model they have left.
		return scan(ScanOtherSpace), nil
	case head.Lists == 0:
		return scan(ScanRetired), nil
	}
	index, err := probeMemo.load(ctx, tx, head)
	if err != nil {
		return stage1Plan{}, err
	}
	probes := head.Probes
	if q.Probes > 0 {
		probes = min(q.Probes, head.Lists)
	}
	order := index.ProbeOrder(Quantize(unpack(q.Vector)))
	read, ok, err := probeLists(ctx, tx, q, head, order, probes, narrowing, narrowingArgs)
	if err != nil {
		return stage1Plan{}, err
	}
	if !ok {
		return scan(ScanFiltered), nil
	}
	lists, err := json.Marshal(order[:read])
	if err != nil {
		return stage1Plan{}, err
	}

	var stale int
	if err := tx.QueryRowContext(ctx, staleRowsStatement,
		q.Model, q.Dim, head.Generation).Scan(&stale); err != nil {
		return stage1Plan{}, fmt.Errorf("search: look for rows the index has "+
			"not re-filed: %w", err)
	}

	// THE PROBED LISTS ARE ONE JSON ARRAY rather than a placeholder each: a
	// probe of two thousand lists would pass the driver's bound-parameter
	// ceiling on its own, and the join reads the array as a table it walks
	// once, seeking the covering index for each list in it.
	plan := stage1Plan{
		with:     `WITH ivf_probe(list) AS (SELECT value FROM json_each(?))`,
		withArgs: []any{string(lists)},
		report: Stage1Report{
			Method: Stage1IVF, Lists: head.Lists, Probed: read,
			IVFGeneration: head.Generation, Stale: stale != 0,
		},
	}
	// THE PROBE, over the columns a caller names: the ranked form reads the
	// code in its ORDER BY, and the union's branch hands it up to the one
	// ORDER BY above both branches.
	//
	// ITS FILTERS ARE RESIDUAL, never a way in: each reads its column through
	// a unary plus ([residual]), which leaves the value unchanged and makes
	// it an expression no index can seek. Every column they name is in the
	// covering index the probe seeks list by list, so applying them to those
	// rows costs nothing — and left seekable, a search narrowed to one source
	// was planned as a seek of the PRIMARY KEY on `source` instead: every
	// row of that source, table and all, read once per probed list, which is
	// the full scan's cost times the probe count.
	probeFilters := residual(filters)
	probe := func(columns string) string {
		out := `
			SELECT ` + columns + `
			FROM ivf_probe p CROSS JOIN kb_vectors_bin b
			  ON b.model = ? AND b.dim = ? AND b.ivf_gen = ? AND b.ivf_list = p.list`
		if len(probeFilters) > 0 {
			out += `
			WHERE ` + strings.Join(probeFilters, " AND ")
		}
		return out
	}
	probeArgs := slices.Concat([]any{q.Model, q.Dim, head.Generation}, filterArgs)
	if stale == 0 {
		plan.body = probe("b.source, b.source_id, b.container") + `
			ORDER BY vector_distance_cos(b.bits, vector1bit(?)),
			         b.source, b.source_id
			LIMIT ?`
		plan.bodyArgs = slices.Concat(probeArgs, []any{q.Vector, q.Candidates})
		return plan, nil
	}
	where := append([]string{"b.model = ?", "b.dim = ?", "b.ivf_gen < ?"}, filters...)
	plan.body = `
			SELECT u.source, u.source_id, u.container FROM (` +
		probe("b.source, b.source_id, b.container, b.bits") + `
			UNION ALL
			SELECT b.source, b.source_id, b.container, b.bits
			FROM kb_vectors_bin b
			WHERE ` + strings.Join(where, " AND ") + `
			) AS u
			ORDER BY vector_distance_cos(u.bits, vector1bit(?)),
			         u.source, u.source_id
			LIMIT ?`
	plan.bodyArgs = slices.Concat(probeArgs,
		[]any{q.Model, q.Dim, head.Generation}, filterArgs,
		[]any{q.Vector, q.Candidates})
	return plan, nil
}

// probeLists is how many lists of order a search reads, and false when it
// must run the full scan instead ([ProbeCount]).
func probeLists(ctx context.Context, tx *sql.Tx, q SemanticQuery, head IndexHead, order []int, probes int, narrowing []string, narrowingArgs []any) (int, bool, error) {
	if len(narrowing) == 0 {
		return min(probes, len(order)), true, nil
	}
	counts, err := readListCounts(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	total := counts.Total
	var matching func(int) (int, error)
	if len(q.Containers) == 0 {
		matching = func(list int) (int, error) { return counts.Of(list, q.Sources), nil }
	} else {
		// A CONTAINER FILTER IS COUNTED on the covering index — the rows
		// of each list it keeps — in runs that double from the probe
		// count, so a filter met early counts little and one never met
		// stops at the ceiling.
		counted := map[int]int{}
		next := 0
		matching = func(list int) (int, error) {
			if n, ok := counted[list]; ok {
				return n, nil
			}
			run := order[next:min(len(order), next+max(probes, next))]
			if err := countMatching(ctx, tx, q, head, run, narrowing, narrowingArgs, counted); err != nil {
				return 0, err
			}
			next += len(run)
			return counted[list], nil
		}
	}
	return ProbeCount(order, probes, head.Lists/IVFProbeCeiling, total, matching)
}

// countMatching counts, into counted, the rows of each list in lists that the
// search's narrowing filters keep — every list, a zero where none match.
func countMatching(ctx context.Context, tx *sql.Tx, q SemanticQuery, head IndexHead, lists []int, narrowing []string, narrowingArgs []any, counted map[int]int) error {
	for _, list := range lists {
		counted[list] = 0
	}
	encoded, err := json.Marshal(lists)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, matchingCountStatement(narrowing),
		slices.Concat([]any{string(encoded), q.Model, q.Dim, head.Generation},
			narrowingArgs)...)
	if err != nil {
		return fmt.Errorf("search: count the lists' rows the filter keeps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var list, n int
		if err := rows.Scan(&list, &n); err != nil {
			return err
		}
		counted[list] = n
	}
	return rows.Err()
}

// matchingCountStatement counts the rows of each probed list that the
// narrowing filters keep — a seek per list on the covering index, which holds
// every column the filters read, with the filters RESIDUAL for the probe's
// own reason ([residual]).
func matchingCountStatement(narrowing []string) string {
	return `
		WITH ivf_probe(list) AS (SELECT value FROM json_each(?))
		SELECT b.ivf_list, COUNT(*)
		FROM ivf_probe p CROSS JOIN kb_vectors_bin b
		  ON b.model = ? AND b.dim = ? AND b.ivf_gen = ? AND b.ivf_list = p.list
		WHERE ` + strings.Join(residual(narrowing), " AND ") + `
		GROUP BY b.ivf_list`
}

// staleRowsStatement asks whether any row of the space predates the installed
// index's rollout — one seek on the covering index, which leads on the space
// and then the generation.
const staleRowsStatement = `
	SELECT EXISTS (SELECT 1 FROM kb_vectors_bin
	               WHERE model = ? AND dim = ? AND ivf_gen < ?)`

// narrowingFilters are the filters that narrow what a search can RETURN — its
// sources and containers — in bind order. A bucket range is not one: see
// [planStage1].
func narrowingFilters(q SemanticQuery) ([]string, []any) {
	var where []string
	var args []any
	if len(q.Sources) > 0 {
		where = append(where, "b.source IN ("+placeholders(len(q.Sources))+")")
		for _, s := range q.Sources {
			args = append(args, string(s))
		}
	}
	if len(q.Containers) > 0 {
		where = append(where, "b.container IN ("+placeholders(len(q.Containers))+")")
		for _, c := range q.Containers {
			args = append(args, c)
		}
	}
	return where, args
}

// stage1Filters are the caller's narrowing predicates over the narrow table,
// and its bucket range, in bind order.
func stage1Filters(q SemanticQuery) ([]string, []any) {
	where, args := narrowingFilters(q)
	if q.Shards.To > q.Shards.From {
		where = append(where, "b.search_shard >= ? AND b.search_shard < ?")
		args = append(args, q.Shards.From, q.Shards.To)
	}
	return where, args
}

// residual is filters with every column of the narrow table read through a
// unary plus — `+b.source` — so a planner applies each to the rows another
// index already found rather than choosing it as the way in. The value is
// unchanged: unary plus on a column is the column.
func residual(filters []string) []string {
	out := make([]string, len(filters))
	for i, f := range filters {
		out[i] = strings.ReplaceAll(f, "b.", "+b.")
	}
	return out
}

// placeholders builds `?, ?, …`.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Key is how a hit is named to the fusion, which joins two ranked lists of
// documents that came from two different tables in two different estates.
//
// ONE SPELLING, here, because the lexical half and the semantic half each
// produce it and the fusion compares them: written twice, a page would fuse
// against itself under two names and every hybrid answer would be wrong in a
// way no single-half test could see.
func Key(source Source, id string) string { return string(source) + ":" + id }

// Hybrid fuses a keyword list and a semantic list into one answer.
//
// # Why the two halves are fused in Go rather than joined in SQL
//
// They are in DIFFERENT DATABASE FILES. The lexical index is this node's own —
// every node tokenises its own rows with no network — and the vectors are
// replicated, applied from a committed record. No transaction spans the two
// estates and no read joins across them, so a `UNION` was never available
// here; reciprocal rank fusion consumes RANKS, so nothing is lost by doing it
// over two lists of ids.
//
// What fusion absorbs is the loss of a document the keyword half also found,
// which merely slides down. What it does NOT absorb is the loss of a
// semantic-only document, which leaves the answer entirely — and that is
// exactly the class the semantic half exists for. See
// TestFuseIsNotMonotoneInTheSemanticList.
func Hybrid(keyword, semantic []string, limit int) []string {
	fused := Fuse(keyword, semantic)
	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}
	return fused
}
