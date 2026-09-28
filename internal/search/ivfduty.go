package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE EMBEDDING DUTY'S SECOND JOB: keeping the partition's semantic index
// current — ADR-0022.
//
// # Why the duty and nobody else
//
// The duty is the vector log's one writer, a fleet singleton, and the index is
// published on that log. Training where the vectors are published means one
// training per partition, billed once — k-means over every code plus a recall
// measurement that reads every exact vector — and a record every holder
// applies, rather than N nodes training N slightly different indexes from N
// slightly different moments of the log.
//
// # Two things must be true before it touches the index
//
// THIS NODE HAS APPLIED THE LOG. Every decision below reads the duty's own
// node's rows, and the duty moves between nodes on a lease: a node still
// catching up would see the index the log has already replaced — or none — and
// train over a healthy one, making every row of the partition stale on every
// holder to get back where it started. So the step runs only on a node whose
// applier has reached the end the log had when the tick began
// ([LogStanding.Current]), and it runs FIRST in the tick, before this tick's
// own embeds put the node behind again.
//
// EVERY NODE APPLYING THE LOG CAN READ WHAT IT WOULD PUBLISH. The index's
// records are a kind builds before them do not know, and a build that cannot
// read a record's ENVELOPE stops its applier rather than deferring it — which
// every build before the envelope stopped validating kinds does
// (`fix(search): decode a vector envelope whose kind a newer build added`).
// Deferral is the rolling upgrade's contract for a record a peer cannot
// apply; a stop is the opposite of it. So nothing about the index is published
// while any node the log counts advertises reading below [IndexRecordVersion]
// — a node that advertises nothing being one that cannot — and the fleet
// searches with the full scan until the upgrade has reached every one of them
// ([LogStanding.Readers]).
//
// # What a tick does, and why the steps are separate ticks
//
// Every tick reads the partition's own state — how many sources the configured
// embedding space holds, what index is installed, how many rows are filed under
// it and how big its fullest list is — and takes at most ONE step
// ([DecideIndex]):
//
//   - TRAIN, when the space has no index (and is at least [IVFMinCorpus]),
//     when the corpus has doubled or halved since training, or when its lists
//     have grown lopsided since the training filed them. Training that
//     concludes no probe count within [IVFProbeCeiling] meets the floor
//     publishes a verdict instead of an index, and one below [IVFMinCorpus]
//     publishes the verdict that it is too small, without training at all.
//   - ROLL OUT, when the installed index has rows not filed under it: every
//     batch of its rollout whose range holds such a row.
//   - MEASURE, when the installed index's last measurement is older than
//     [IVFMeasureInterval]: the same measurement a training makes, against the
//     corpus as it is now, publishing the probe count that meets the floor
//     today — or, when none within the ceiling does, training a new index from
//     the same reading in the same tick.
//
// Training and its rollout are deliberately TWO ticks. Each is bounded by the
// partition — the training by the k-means and the one exact pass, the rollout
// by one Hamming search per row against every centroid on every holder — and a
// tick is bounded by the duty's lease (the engine cuts it off before the lease
// can lapse, and a step cut off publishes nothing). The minute between them
// costs nothing: until the rollout has applied, a search reads the
// not-yet-filed rows in full ([Stage1]).
//
// # What a tick reads
//
// The head row, the per-list counts (at most a few thousand rows), and one
// count of the space over the model index kb_vectors carries — never a walk of
// the covering index. A training or a measurement additionally reads every
// code once and makes one exact pass over the wide table.
//
// # Why "rows not filed under the index" is the whole rollout trigger
//
// A row written after the index was installed is filed as it is written, so
// the only rows not filed under it are the ones that predate it. A rollout
// that has applied leaves none; one that has not, or lost a batch to an
// unresolved publish, leaves some — on a node that has applied everything the
// log holds, a batch that did not land — and the next tick publishes the
// batches that still have such rows. Re-filing is idempotent and every
// publication of a batch says the same thing, so a repeat costs work and never
// correctness, and the trigger needs no memory of what was published: it is
// read off the rows.

// IVFMeasureInterval is how long an installed index's measurement stands
// before the duty measures it again.
//
// A DAY. The retrain rule catches a corpus that doubled or halved; this
// catches one that DRIFTED inside those bounds — rewritten, re-scoped, grown by
// half with new subjects — where the probe count measured at training may no
// longer meet the floor and nothing else would notice until an operator's
// scheduled `crewlet search eval` failed. A measurement is a training without
// the k-means — every code read once and one exact pass — so a day bounds how
// long a drifted index can serve below the floor at one pass a day per
// partition, which the tick budget measured for a training
// ([BenchmarkIndexTraining]) comfortably covers; the evaluation the operator
// guide asks for is monthly, so the duty has re-measured thirty times before
// an operator would look.
const IVFMeasureInterval = 24 * time.Hour

// IndexRecordVersion is the record version every node applying the vector log
// must read before any record about the index is published on it: the highest
// version of the index's operations and its subject kind ([kindVersions]).
func IndexRecordVersion() int {
	version := 0
	for _, op := range []Op{OpCentroids, OpReassign, OpMeasure} {
		version = max(version, VersionOf(op, Subject{Source: IndexSource}))
	}
	return version
}

// IndexAction is the one step a tick takes on the partition's index.
type IndexAction string

const (
	// IndexKeep changes nothing: the index is current, there is none and
	// the partition is too small to want one, or the step may not act.
	IndexKeep IndexAction = "keep"

	// IndexTrain trains a new index (or publishes the verdict that there
	// should be none).
	IndexTrain IndexAction = "train"

	// IndexRollout re-files the rows the installed index has not filed.
	IndexRollout IndexAction = "rollout"

	// IndexMeasure measures the installed index against the corpus as it is
	// now.
	IndexMeasure IndexAction = "measure"
)

// IndexActions is every action, for the enum's validation.
var IndexActions = []IndexAction{IndexKeep, IndexTrain, IndexRollout, IndexMeasure}

// Valid reports whether an action is one this build takes.
func (a IndexAction) Valid() bool { return slices.Contains(IndexActions, a) }

// LogStanding is what the index step must know about the vector log before it
// may publish onto it, read by whoever holds the log's runner.
type LogStanding struct {
	// Current reports that this node has applied every record the log held
	// when the standing was read, deferring none — so its rows are the log's
	// state rather than a moment behind it.
	Current bool

	// Readers is every node the log counts — each that has reported a
	// position on it, and each live data node that has not yet — with the
	// highest record version its build advertises reading, ZERO for one that
	// advertises none: a build older than the advertisement, which is a
	// build that cannot read the index's records.
	Readers map[string]int
}

// IndexState is what a tick reads before it decides.
type IndexState struct {
	// Head is the installed index's row, and Indexed whether there is one.
	Head    IndexHead
	Indexed bool

	// Sources is how many rows the configured embedding space holds.
	Sources int

	// Filed is how many of them are filed under the installed index, and
	// Largest how many its fullest list holds. Both zero when the space
	// has no live index.
	Filed, Largest int

	// Behind reports that this node has not applied the log it would decide
	// from, and Held names the nodes applying the log that cannot read the
	// index's records — either one stops every step.
	Behind bool
	Held   []string

	// Now is the tick's instant, which the measurement's age is read at.
	Now time.Time
}

// Stale is how many rows of the space the installed index has not filed.
func (s IndexState) Stale() int { return s.Sources - s.Filed }

// DecideIndex is the step a tick takes: a pure function of what it read.
//
// IN THIS ORDER, and each case is why the next one can assume what it does: a
// node behind the log, or a fleet with a node that cannot read the index's
// records, takes no step at all; an index in another embedding space is no
// index; a verdict that the partition was too small is revisited the moment it
// is not; a corpus that moved by [IVFRetrainFactor] is re-derived whatever else
// is true, because the probe count was measured at another size; a live index
// with unfiled rows finishes its rollout before anything judges its lists; only
// a fully filed index has list sizes worth judging; and only a balanced one is
// worth re-measuring.
func DecideIndex(s IndexState, model string, dim int) IndexAction {
	if s.Behind || len(s.Held) > 0 {
		return IndexKeep
	}
	return decideIndex(s, model, dim)
}

// decideIndex is [DecideIndex] with the log's standing set aside: the step
// the partition wants, which a tick that may not act still reports.
func decideIndex(s IndexState, model string, dim int) IndexAction {
	inSpace := s.Indexed && s.Head.InSpace(model, dim)
	live := inSpace && s.Head.Lists > 0
	switch {
	case !inSpace, s.Head.Why == VerdictTooSmall:
		if s.Sources >= IVFMinCorpus {
			return IndexTrain
		}
		return IndexKeep
	case s.Sources >= IVFRetrainFactor*s.Head.TrainedOn,
		s.Sources*IVFRetrainFactor <= s.Head.TrainedOn:
		return IndexTrain
	case live && s.Stale() > 0:
		return IndexRollout
	case live && lopsided(s):
		return IndexTrain
	case live && s.Now.Sub(s.Head.MeasuredAt) >= IVFMeasureInterval:
		return IndexMeasure
	}
	return IndexKeep
}

// lopsided reports lists that have grown lopsided SINCE THE TRAINING filed
// them: the fullest list above [IVFImbalance] times the mean, AND its share of
// the corpus at least [IVFRetrainFactor] times what the training left it.
//
// # Why against the training and not against the mean alone
//
// A list that large is a probe reading four lists' worth of rows whenever it
// is chosen, which is the cost model's premise failing — but a retrain only
// cures it when the lopsidedness is NEW. A corpus with many identical codes
// (blank tasks, a template copied a thousand times) files them all in one list
// whatever the seed, because identical codes are nearest to the same centroid:
// 2 000 copies of one code in a 22 000-source partition left a largest list of
// about 2 020 against a mean of 85 after every training. Judged against the
// mean alone, every tick after the rollout decided to train again — a full
// k-means and exact pass, a new generation making every row stale on every
// holder, and a rollout re-filing the partition, every two minutes for ever.
// Judged against what the training achieved, that index comes to rest, and one
// whose corpus has drifted since — the fullest list doubling its share — is
// still retrained, which is the case the rule exists for.
func lopsided(s IndexState) bool {
	if s.Sources <= 0 || s.Head.TrainedOn <= 0 {
		return false
	}
	aboveMean := s.Largest*s.Head.Lists > IVFImbalance*s.Sources
	grown := s.Largest*s.Head.TrainedOn >= IVFRetrainFactor*max(s.Head.Largest, 1)*s.Sources
	return aboveMean && grown
}

// heldBy is every reader below the index's record version, sorted.
func heldBy(readers map[string]int) []string {
	var out []string
	for node, reads := range readers {
		if reads < IndexRecordVersion() {
			out = append(out, node)
		}
	}
	sort.Strings(out)
	return out
}

// maintainIndex takes this tick's step on the partition's index, and reports
// how many records it published.
func (e *Embedder) maintainIndex(ctx context.Context, dim int) (int, error) {
	standing, err := e.deps.Standing(ctx)
	if err != nil {
		return 0, fmt.Errorf("search: read the vector log's standing: %w", err)
	}
	state, err := e.indexState(ctx, dim)
	if err != nil {
		return 0, err
	}
	state.Behind, state.Held, state.Now = !standing.Current, heldBy(standing.Readers), e.deps.Now()

	action := DecideIndex(state, e.deps.Model, dim)
	if wanted := decideIndex(state, e.deps.Model, dim); action == IndexKeep && wanted != IndexKeep {
		switch {
		case len(state.Held) > 0:
			// WARN, because it can outlast an upgrade: a counted node
			// that is offline on an old build holds the index back until
			// it returns upgraded or an operator forgets its position,
			// exactly as it pins the log's trim.
			e.deps.Logger.WarnContext(ctx, "search_index_held",
				"wanted", string(wanted), "nodes", state.Held,
				"reads_below", IndexRecordVersion(),
				"detail", "a node applying the vector log advertises no "+
					"build that reads the index's records, and one that "+
					"cannot read them stops its applier on the first; the "+
					"full scan answers until every counted node is upgraded")
		default:
			e.deps.Logger.InfoContext(ctx, "search_index_behind",
				"wanted", string(wanted),
				"detail", "this node has not applied the whole vector log, "+
					"so it does not decide the index from its rows this tick")
		}
	}
	switch action {
	case IndexTrain:
		return e.train(ctx, dim, state)
	case IndexRollout:
		return e.rollout(ctx, state.Head)
	case IndexMeasure:
		return e.measure(ctx, dim, state.Head)
	}
	return 0, nil
}

// indexState reads what a tick decides from, in one snapshot.
func (e *Embedder) indexState(ctx context.Context, dim int) (IndexState, error) {
	var s IndexState
	err := e.deps.Store.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if s.Head, s.Indexed, err = readIndexHead(ctx, tx); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, spaceCountStatement,
			e.deps.Model, dim).Scan(&s.Sources); err != nil {
			return fmt.Errorf("search: count the embedding space: %w", err)
		}
		if !s.Indexed || !s.Head.live(e.deps.Model, dim) {
			return nil
		}
		counts, err := readListCounts(ctx, tx)
		if err != nil {
			return err
		}
		for list := range counts {
			filed := counts.Total(list)
			s.Filed += filed
			s.Largest = max(s.Largest, filed)
		}
		return nil
	})
	return s, err
}

// trainingSet is one snapshot's reading of everything a training or a
// measurement needs.
type trainingSet struct {
	corpus TrainingCorpus
	trials []IVFTrial

	// heldOut is the query documents' rows, ascending — never trained on.
	heldOut []int

	// ids is each source's ids in key order, which a rollout is cut from.
	ids map[Source][]string

	// filed is each row's filing, which a measurement groups by list.
	filed []filing
}

// train trains the partition's index from its own rows and publishes it — or
// publishes the verdict that it should have none.
func (e *Embedder) train(ctx context.Context, dim int, state IndexState) (int, error) {
	var basis int64
	if state.Indexed {
		basis = state.Head.Generation
	}
	if state.Sources < IVFMinCorpus {
		return e.publishIndex(ctx, dim, IndexRecord{
			Log: e.deps.Log, Basis: basis, Seed: IVFSeed(e.deps.Log, basis),
			TrainedOn: state.Sources, Why: VerdictTooSmall,
		})
	}
	var set trainingSet
	err := e.deps.Store.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		set, err = readTrainingSet(ctx, tx, e.deps.Model, dim)
		return err
	})
	if err != nil {
		return 0, err
	}
	return e.trainFrom(ctx, dim, set, basis)
}

// trainFrom trains over one reading and publishes the index or the verdict.
func (e *Embedder) trainFrom(ctx context.Context, dim int, set trainingSet, basis int64) (int, error) {
	codes := set.corpus.Codes
	n := codes.Len()
	if n-len(set.heldOut) < IVFMinCorpus {
		// THE CORPUS SHRANK BETWEEN THE TWO READS — a purge landing in
		// the gap. Nothing to train; the next tick re-reads it.
		return 0, nil
	}
	seed := IVFSeed(e.deps.Log, basis)
	lists := IVFLists(n)
	index, err := TrainIVF(ctx, codes, lists, seed, set.heldOut)
	if err != nil {
		return 0, err
	}
	byList := GroupByList(index.Assign(codes), lists)
	if err = ctx.Err(); err != nil {
		return 0, fmt.Errorf("search: the index's training stopped filing the "+
			"corpus: %w", err)
	}
	choice, err := ChooseProbes(ctx, &set.corpus, byList, index, set.trials)
	if err != nil {
		return 0, err
	}
	measurement := choice.Measurement
	record := IndexRecord{
		Log: e.deps.Log, Basis: basis, Seed: seed, TrainedOn: n,
		Largest: LargestList(byList), Measurement: &measurement,
	}
	if choice.Worthwhile(lists) {
		record.Lists, record.Probes = lists, choice.Probes
		record.Centroids, record.Rollout = index.Bytes(), RolloutRanges(set.ids)
	} else {
		record.Why, record.Largest = VerdictNotWorthwhile, 0
	}
	e.deps.Logger.InfoContext(ctx, "search_index_trained",
		"sources", n, "lists", lists, "probes", choice.Probes,
		"recall", measurement.Recall, "floor", measurement.Floor,
		"worst_shape", string(measurement.Shape),
		"head_misses", measurement.HeadMisses, "shapes", fmt.Sprintf("%+v", choice.Shapes),
		"installed", record.Lists > 0)
	return e.publishIndex(ctx, dim, record)
}

// measure measures the installed index against the corpus as it is now and
// publishes what it found — or, when no probe count within the ceiling meets
// the floor any more, trains its replacement from the same reading.
//
// THE RETRAIN IS IN THE SAME TICK, deliberately: the reading is the expensive
// half of a training and it is already in hand, and publishing the failure
// first would leave every search below the floor for another tick to record a
// fact the next record replaces anyway. So a measurement record only ever says
// an index passes, and a failing index is replaced rather than described.
func (e *Embedder) measure(ctx context.Context, dim int, head IndexHead) (int, error) {
	var set trainingSet
	var index IVF
	err := e.deps.Store.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if set, err = readTrainingSet(ctx, tx, e.deps.Model, dim); err != nil {
			return err
		}
		index, err = (*ivfMemo)(nil).load(ctx, tx, head)
		return err
	})
	if err != nil {
		return 0, err
	}
	byList := make([][]int32, head.Lists)
	for row, f := range set.filed {
		if f.generation != head.Generation || f.list < 0 || f.list >= head.Lists {
			// A ROW THE INDEX HAS NOT FILED, written between the tick's
			// state and this reading: the rollout's to finish first.
			return 0, nil
		}
		byList[f.list] = append(byList[f.list], int32(row))
	}
	choice, err := ChooseProbes(ctx, &set.corpus, byList, index, set.trials)
	if err != nil {
		return 0, err
	}
	e.deps.Logger.InfoContext(ctx, "search_index_measured",
		"generation", head.Generation, "sources", set.corpus.Codes.Len(),
		"probes", choice.Probes, "was", head.Probes,
		"recall", choice.Measurement.Recall, "floor", choice.Measurement.Floor,
		"worst_shape", string(choice.Measurement.Shape),
		"head_misses", choice.Measurement.HeadMisses,
		"passes", choice.Worthwhile(head.Lists))
	if !choice.Worthwhile(head.Lists) {
		return e.trainFrom(ctx, dim, set, head.Generation)
	}
	subject := MeasureSubject(head.Generation)
	if err := e.append(ctx, subject, VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   subject,
			Op:        OpMeasure,
			CreatedAt: e.deps.Now().UTC(),
			Scope:     statelog.ScopeSet{Paths: []string{IndexScopePath(subject)}},
		},
		Measure: &MeasureRecord{Index: head.Generation, Probes: choice.Probes,
			Measurement: choice.Measurement},
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// readTrainingSet reads the space's codes in key order, what each row's
// shapes filter on, and the held-out trials a training measures recall on.
//
// KEY ORDER — source, then source id — because it is the tie break the SQL
// probe declares, and [IVFCandidates] breaks ties on the row number: loaded in
// any other order, a training would measure a candidate pool the installed
// index never produces.
func readTrainingSet(ctx context.Context, tx *sql.Tx, model string, dim int) (trainingSet, error) {
	set := trainingSet{
		corpus: TrainingCorpus{Codes: NewCodes(CodeWords(dim), 0)},
		ids:    map[Source][]string{},
	}
	row := map[string]int{}
	containers := map[string]int32{}
	rows, err := tx.QueryContext(ctx, trainingCodesStatement, model, dim)
	if err != nil {
		return trainingSet{}, fmt.Errorf("search: read the partition's codes: %w", err)
	}
	for rows.Next() {
		var source, id, container string
		var bits []byte
		var f filing
		if err = rows.Scan(&source, &id, &container, &bits, &f.generation, &f.list); err != nil {
			_ = rows.Close()
			return trainingSet{}, err
		}
		var code []uint64
		if code, err = codeFromBits(bits, dim); err != nil {
			_ = rows.Close()
			return trainingSet{}, err
		}
		c, ok := containers[container]
		if !ok {
			c = int32(len(containers))
			containers[container] = c
		}
		row[Key(Source(source), id)] = set.corpus.Codes.Len()
		set.corpus.Codes.Append(code)
		set.corpus.Sources = append(set.corpus.Sources, Source(source))
		set.corpus.Containers = append(set.corpus.Containers, c)
		set.ids[Source(source)] = append(set.ids[Source(source)], id)
		set.filed = append(set.filed, f)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return trainingSet{}, err
	}
	_ = rows.Close()

	// THE EVALUATION'S OWN SAMPLE, SHAPES AND GROUND TRUTH, so an index this
	// installs is judged on the evidence `crewlet search eval` would use.
	queries, err := sampleDocuments(ctx, tx, model, dim, EvalQueries)
	if err != nil {
		return trainingSet{}, err
	}
	shapes := make([][]ShapeQuery, len(queries))
	for i, q := range queries {
		shapes[i] = shapesFor(q, sourcesIn(set.ids))
	}
	tops, err := exactTops(ctx, tx, queries, shapes, model, dim, ReturnDepth)
	if err != nil {
		return trainingSet{}, err
	}
	for i, q := range queries {
		self, ok := row[Key(q.Source, q.ID)]
		if !ok {
			// A VECTOR WITH NO CODE, which the applier never writes.
			return trainingSet{}, fmt.Errorf("search: the query document %s "+
				"has a vector and no sign code", Key(q.Source, q.ID))
		}
		set.heldOut = append(set.heldOut, self)
		code := Quantize(unpack(q.Vector))
		for j, shape := range shapes[i] {
			want := make([]int, len(tops[i][j].Keys))
			for k, key := range tops[i][j].Keys {
				at, ok := row[key]
				if !ok {
					// A VECTOR WITH NO CODE, counted as a document no
					// probe can find, which is the truth about it.
					at = -1
				}
				want[k] = at
			}
			filter := ShapeFilter{Shape: shape.Shape, Source: shape.Source, Container: -1}
			if shape.Shape == ShapeContainer {
				filter.Container = containers[shape.Container]
			}
			set.trials = append(set.trials, IVFTrial{Query: code, Filter: filter,
				Self: self, Want: want, Floor: FloorAt(tops[i][j].Matching)})
		}
	}
	slices.Sort(set.heldOut)
	set.heldOut = slices.Compact(set.heldOut)
	return set, nil
}

// sourcesIn is every source a reading holds rows of, ascending.
func sourcesIn(ids map[Source][]string) []Source {
	out := make([]Source, 0, len(ids))
	for source := range ids {
		out = append(out, source)
	}
	slices.Sort(out)
	return out
}

// publishIndex writes one centroids record and reports how many it published.
func (e *Embedder) publishIndex(ctx context.Context, dim int, index IndexRecord) (int, error) {
	if err := e.append(ctx, IndexCentroids, VectorRecord{
		RecordEnvelope: RecordEnvelope{
			Subject:   IndexCentroids,
			Op:        OpCentroids,
			CreatedAt: e.deps.Now().UTC(),
			Scope:     statelog.ScopeSet{Paths: []string{IndexScopePath(IndexCentroids)}},
		},
		Model: e.deps.Model,
		Dim:   dim,
		Index: &index,
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// rollout publishes every batch of the installed index's rollout whose range
// still holds a row the index has not filed.
//
// ONLY THOSE BATCHES, and that is complete for every holder, not only this
// one: this node has applied the whole log (the step's precondition), so a
// row it holds unfiled is a row whose batch never landed, and a holder
// replaying the log holds the same rows at the same positions — every batch
// that did land is on the log under its own subject, which nothing but a
// re-publication of the same batch can replace.
func (e *Embedder) rollout(ctx context.Context, head IndexHead) (int, error) {
	var ranges []RolloutRange
	pending := map[int]bool{}
	err := e.deps.Store.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if ranges, err = readRollout(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, staleKeysStatement, head.Model, head.Dim,
			head.Generation)
		if err != nil {
			return fmt.Errorf("search: read the rows the index has not filed: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var source, id string
			if err := rows.Scan(&source, &id); err != nil {
				return err
			}
			if n, ok := batchOf(ranges, Source(source), id); ok {
				pending[n] = true
			}
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	batches := make([]int, 0, len(pending))
	for n := range pending {
		batches = append(batches, n)
	}
	slices.Sort(batches)
	published := 0
	for _, n := range batches {
		subject := ReassignSubject(head.Generation, n)
		if err := e.append(ctx, subject, VectorRecord{
			RecordEnvelope: RecordEnvelope{
				Subject:   subject,
				Op:        OpReassign,
				CreatedAt: e.deps.Now().UTC(),
				Scope:     statelog.ScopeSet{Paths: []string{IndexScopePath(subject)}},
			},
			Reassign: &ReassignRecord{Index: head.Generation, Batch: n,
				Batches: len(ranges)},
		}); err != nil {
			return published, err
		}
		published++
	}
	e.deps.Logger.InfoContext(ctx, "search_index_rollout",
		"generation", head.Generation, "batches", published, "of", len(ranges))
	return published, nil
}

// readRollout is the installed index's rollout, batch by batch.
func readRollout(ctx context.Context, tx *sql.Tx) ([]RolloutRange, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT batch, source, from_id, to_id FROM kb_ivf_rollout ORDER BY batch`)
	if err != nil {
		return nil, fmt.Errorf("search: read the index's rollout: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RolloutRange
	for rows.Next() {
		var n int
		var r RolloutRange
		var source string
		if err := rows.Scan(&n, &source, &r.From, &r.To); err != nil {
			return nil, err
		}
		if n != len(out) {
			return nil, errors.New("search: the index's rollout skips a batch — " +
				"kb_ivf_rollout was written by something other than the vector applier")
		}
		r.Source = Source(source)
		out = append(out, r)
	}
	return out, rows.Err()
}

// batchOf is the batch of a rollout whose range holds one source id.
func batchOf(ranges []RolloutRange, source Source, id string) (int, bool) {
	// THE RANGES ARE GROUPED BY SOURCE AND ASCENDING WITHIN IT
	// ([validateRollout]), so the first range of the source ending after
	// the id is the one holding it.
	n := sort.Search(len(ranges), func(i int) bool {
		r := ranges[i]
		if r.Source != source {
			return r.Source > source
		}
		return r.To == "" || id < r.To
	})
	if n < len(ranges) && ranges[n].Holds(source, id) {
		return n, true
	}
	return 0, false
}

// RolloutRanges cuts a rollout over the ids each source holds, ascending.
//
// THE WHOLE KEY SPACE, not only the ids read: each source's ids are cut every
// [IVFReassignBatch], its first range starts at the source's beginning and its
// last runs to its end — and every source this build embeds gets at least one
// open range, whether or not the partition holds a row of it — so a row
// written anywhere in the key space falls in exactly one batch.
func RolloutRanges(ids map[Source][]string) []RolloutRange {
	var out []RolloutRange
	for _, source := range sourcesIn(withEverySource(ids)) {
		held := ids[source]
		from := ""
		for start := IVFReassignBatch; start < len(held); start += IVFReassignBatch {
			out = append(out, RolloutRange{Source: source, From: from, To: held[start]})
			from = held[start]
		}
		out = append(out, RolloutRange{Source: source, From: from})
	}
	return out
}

// withEverySource is ids with an empty entry for every source this build
// embeds that it lacks.
func withEverySource(ids map[Source][]string) map[Source][]string {
	out := make(map[Source][]string, len(ids)+len(Sources))
	for source, held := range ids {
		out[source] = held
	}
	for _, source := range Sources {
		if _, ok := out[source]; !ok {
			out[source] = nil
		}
	}
	return out
}

// The duty's reads of the vector tables, named so the plan gate explains the
// statements the duty runs rather than copies of them.
const (
	// spaceCountStatement counts the embedding space over kb_vectors'
	// model index, which covers it — the narrowest way to count it.
	spaceCountStatement = `
		SELECT COUNT(*) FROM kb_vectors WHERE model = ? AND dim = ?`

	// trainingCodesStatement is every code in the space, in key order,
	// with what a shape filters on and where the row is filed.
	trainingCodesStatement = `
		SELECT source, source_id, container, bits, ivf_gen, ivf_list
		FROM kb_vectors_bin
		WHERE model = ? AND dim = ?
		ORDER BY source, source_id`

	// staleKeysStatement is every row of the space the installed index has
	// not filed — one range of the covering index.
	staleKeysStatement = `
		SELECT source, source_id FROM kb_vectors_bin
		WHERE model = ? AND dim = ? AND ivf_gen < ?`
)
