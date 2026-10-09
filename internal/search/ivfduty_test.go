package search_test

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
)

// THE DUTY TRAINS AN INDEX, AND ITS ROLLOUT CONVERGES — through the real record
// path, on a real broker — AND EVERY LONG STEP OF IT SHOWS THE TICK'S BOUND ITS
// PROGRESS.
//
// The pure gates certify the arithmetic and the applier; this certifies the
// LOOP. Ticks run until the duty has nothing left to do, and by then: the
// corpus has been embedded, an index trained each time it reached the minimum
// or doubled, every such index rolled out on the tick after it, nothing is
// left unfiled, a search probes the index, and a tick over that state
// publishes nothing at all. Each step is one tick and each is a record every
// holder applies.
//
// # And the bound sees every step of it working
//
// The training shows the tick's bound its progress, in a training and in the
// day's measurement alike, and no exemption outlives the tick — and so does
// every request the tick sends, every vector it publishes and every batch of
// a rollout. The bound exists to cut off a tick that WEDGED, and a wedge is
// the absence of progress — so every long step of a training must show it,
// or on a node allowed one core, where a training at the largest corpus is
// over six minutes of reading and six more of arithmetic, the bound cut off
// every training such a node began, for ever. The reading of every code and
// the exact pass stream rows: each reports every [search.ProgressStride] of
// them. A request reports when the provider answers it, each vector it
// publishes reports again, and a rollout reports each batch record — each
// counted against the step that made it, so a publish's report never stands
// in for the request's. The k-means, the filing and the choice of a probe
// count cannot wedge — they are arithmetic over values in memory, reading
// their context every stride — so every context reading they make happens
// with the bound's clock stopped. And an exemption must be over by the end of
// the tick: one left open would stop the clock on the reads and publishes
// after it, which are what the bound is for.
//
// ONE RUN FOR BOTH HALVES, because the budget and the context the second
// watches through only count what they see and hand everything on — the duty
// under them runs exactly the loop it runs unwatched — and the run, 2 200
// sources embedded, trained twice, rolled out and measured a day on through a
// real broker, was most of a minute under the race detector each time it was
// built.
func TestTheDutyTrainsAnIndexAndItsRolloutConverges(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	// SIXTEEN TOPICS, where an index trained at 2 048 sources still meets
	// the floor at the corpus's 2 200 a day later — so the day's step is a
	// measurement, which TestAMeasurementThatMissesTheFloorRetrains is not.
	embedder := topicalEmbedder{width: 384, topics: 16, requests: new(atomic.Int64)}
	now := time.Unix(1_700_000_000, 0).UTC()
	budget := &watchedBudget{advanced: map[string]int{}}
	duty := boundedDuty(t, h, embedder, h.standing(),
		func() time.Time { return now }, budget)
	// ENOUGH THAT AN ANSWER IS A SMALL SHARE OF ITS TOPIC: the shipped
	// answer is 150 documents deep, and a topic much smaller than that is
	// one no probe of a few lists can hold.
	h.seedTasks(taskBodies(2_200))
	ctx := &arithmeticWatch{Context: t.Context(), budget: budget}

	state, rollouts := runToRestBy(t, h, duty, embedder.width,
		func() (int, error) { return duty.Tick(ctx) })
	if state.Sources != 2_200 || !state.Indexed || state.Head.Lists == 0 {
		t.Fatalf("the duty came to rest over %d of %d sources with index %+v",
			state.Sources, 2_200, state.Head)
	}
	if rollouts == 0 || state.Stale() != 0 {
		t.Fatalf("%d rollout tick(s) ran and %d rows are unfiled at rest",
			rollouts, state.Stale())
	}
	if state.Head.Measurement == nil || !state.Head.Measurement.Passed() {
		t.Fatalf("the installed index records the measurement %+v", state.Head.Measurement)
	}

	// EVERY REQUEST THE TICKS SENT, EVERY VECTOR AND ROLLOUT BATCH THEY
	// PUBLISHED, AND EVERY STRIDE OF BOTH READS, in the one training that
	// installed the index.
	strides := state.Sources / search.ProgressStride
	if strides < 2 {
		t.Fatalf("setup: %d sources is under two strides of rows", state.Sources)
	}
	trainedReads := budget.reported()
	requests := int(embedder.requests.Load())
	if batches := (state.Sources + search.EmbedBatch - 1) / search.EmbedBatch; requests < batches {
		t.Fatalf("setup: %d sources were embedded in %d request(s), and a request "+
			"carries at most %d", state.Sources, requests, search.EmbedBatch)
	}
	if got := trainedReads[requestFrame]; got != requests {
		t.Errorf("the ticks' %d provider request(s) reported progress %d time(s), "+
			"want once each, when it is answered: a request is bounded by its own "+
			"timeout and its answer is the tick's progress", requests, got)
	}
	if got := trainedReads[publishFrame]; got < state.Sources {
		t.Errorf("the ticks published %d vectors and reported progress for %d, want "+
			"every publish: a request's worth of them unreported is the longest a "+
			"live tick went silent", state.Sources, got)
	}
	reassigns := 0
	for _, env := range published(t, h) {
		if env.Op == search.OpReassign {
			reassigns++
		}
	}
	if got := trainedReads[rolloutFrame]; reassigns == 0 || got < reassigns {
		t.Errorf("the rollouts published %d batch record(s) and reported progress "+
			"for %d, want every one", reassigns, got)
	}
	for _, reader := range streamingReads {
		if got := trainedReads[reader]; got < strides {
			t.Errorf("%s reported progress %d time(s) over %d rows, want every %d "+
				"rows (%d): a read that shows none is cut off as wedged on a slow node",
				reader, got, state.Sources, search.ProgressStride, strides)
		}
	}
	trained := ctx.exempt.Load()

	// AND ITS ROLLOUT IS CUT AT THE DUTY'S OWN BATCH, which is what bounds
	// the rows one reassign apply re-files: every range but a source's last
	// held search.IVFReassignBatch of the ids the index was trained on, so of
	// the ids the corpus holds now it holds at least that many and at most
	// that many more than were embedded since.
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		ranges, ids := installedRollout(t, tx), embeddedIDs(t, tx)
		since, closed := state.Sources-state.Head.TrainedOn, 0
		for _, r := range ranges {
			if r.To == "" {
				continue
			}
			closed++
			held := 0
			for _, id := range ids[r.Source] {
				if r.Holds(r.Source, id) {
					held++
				}
			}
			if held < search.IVFReassignBatch || held > search.IVFReassignBatch+since {
				return fmt.Errorf("the rollout range %+v holds %d ids, and a cut "+
					"every %d of the %d trained on holds %d to %d of the %d now",
					r, held, search.IVFReassignBatch, state.Head.TrainedOn,
					search.IVFReassignBatch, search.IVFReassignBatch+since, state.Sources)
			}
		}
		if closed == 0 {
			return fmt.Errorf("the rollout of an index trained on %d ids is %d "+
				"open ranges — no cut at all", state.Head.TrainedOn, len(ranges))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// AND THE INDEX IS WHAT A SEARCH PROBES.
	query, err := embedder.Embed(t.Context(), "a query about something")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		hits, report, err := search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: pack(query), Model: embedModel, Dim: embedder.width, Limit: 5,
		})
		if err != nil {
			return err
		}
		if report.Method != search.Stage1IVF || report.Stale ||
			report.IVFGeneration != state.Head.Generation {
			return fmt.Errorf("the first stage reports %+v at rest", report)
		}
		if len(hits) != 5 {
			return fmt.Errorf("the probe answered %d hits", len(hits))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A DAY LATER THE DUTY MEASURES IT AGAIN, against the corpus as it is
	// then — one record, which re-files nothing and keeps the generation.
	now = now.Add(search.IVFMeasureInterval)
	published, err := duty.Tick(ctx)
	if err != nil || published != 1 {
		t.Fatalf("a day on, the tick published %d record(s): %v", published, err)
	}
	h.drain()
	measured, err := search.IndexStateOf(t.Context(), duty, embedder.width)
	if err != nil {
		t.Fatal(err)
	}
	if measured.Head.Generation != state.Head.Generation || measured.Stale() != 0 ||
		!measured.Head.MeasuredAt.Equal(now) || !measured.Head.Measurement.Passed() {
		t.Fatalf("after the day's measurement the index is %+v (measured %v, want "+
			"the same generation %d re-measured at %v with nothing re-filed)",
			measured.Head, measured.Head.MeasuredAt, state.Head.Generation, now)
	}

	// AND THE DAY'S MEASUREMENT SHOWED ITS PROGRESS TOO, every stride of both
	// reads again, with the arithmetic's clock stopped and nothing left open.
	measuredReads := budget.reported()
	for _, reader := range streamingReads {
		if got := measuredReads[reader] - trainedReads[reader]; got < strides {
			t.Errorf("the day's measurement's %s reported progress %d time(s), want %d",
				reader, got, strides)
		}
	}
	switch {
	case ctx.charged.Load() != 0:
		t.Errorf("%d of the index's context readings ran with the bound's clock "+
			"running (%d exempt): its arithmetic is charged to the tick", ctx.charged.Load(),
			ctx.exempt.Load())
	case trained == 0:
		t.Error("no stride of the training was seen at all — the watch reads nothing")
	case ctx.exempt.Load() == trained:
		t.Error("no probe count of the day's measurement was seen exempt")
	case budget.open.Load() != 0:
		t.Errorf("%d exemption(s) outlived the tick: the bound's clock stays stopped "+
			"on everything after it", budget.open.Load())
	}

	// AND EVERY RECORD THE DUTY PUBLISHED IS AT THE BASE VERSION: every kind
	// it writes, the index's included, is in the base format.
	for version, kinds := range publishedVersions(t, h) {
		for kind := range kinds {
			if want := 1; version != want {
				t.Fatalf("the duty published %s records at version %d, want %d",
					kind, version, want)
			}
		}
	}
}

// A MEASUREMENT THAT MISSES THE FLOOR RETRAINS IN THE SAME TICK, rather than
// recording a failing index for searches to read.
//
// Eight topics of about 256 sources each, and the 152 sources the corpus gains
// after the index is trained at 2 048 all in a NINTH topic its training never
// saw: each of those is filed in whichever list its noise lands it nearest, so
// a query among them no longer finds its neighbours in the lists it probes —
// a corpus that moved since training, which is what the daily measurement
// exists to catch. The same tick trains its replacement from the reading
// already in hand, so the one record it publishes is a new index that passes,
// and no search is left reading one that does not.
//
// THE MOVE IS BUILT, NOT DRAWN. This case once rested on the hash of each
// fixture text happening to produce a corpus whose index failed a day later,
// and any change to the text the duty sends re-drew it. A spread of 0.8 rather
// than the 0.5 the other cases use, measured: at 0.5 the retraining over nine
// tight topics needs more than half its lists and installs no index, which is
// the verdict another case covers rather than this one.
func TestAMeasurementThatMissesTheFloorRetrains(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 384, topics: 8, noise: 0.8, drifted: 1}
	now := time.Unix(1_700_000_000, 0).UTC()
	duty := indexDuty(t, h, embedder, h.standing(),
		func() time.Time { return now })
	bodies := taskBodies(2_200)
	for i := 2_048; i < 2_200; i++ {
		bodies[fmt.Sprintf("t%05d", i)] = fmt.Sprintf("%s %d", driftMarker, i)
	}
	h.seedTasks(bodies)
	state, _ := runToRest(t, h, duty, embedder.width)
	if !state.Indexed || state.Head.Lists == 0 || state.Head.TrainedOn != 2_048 {
		t.Fatalf("the duty came to rest with %+v, want the index trained at 2048", state.Head)
	}

	now = now.Add(search.IVFMeasureInterval)
	published, err := duty.Tick(t.Context())
	if err != nil || published != 1 {
		t.Fatalf("a day on, the tick published %d record(s): %v", published, err)
	}
	h.drain()
	after, err := search.IndexStateOf(t.Context(), duty, embedder.width)
	if err != nil {
		t.Fatal(err)
	}
	if after.Head.Generation == state.Head.Generation || after.Head.TrainedOn != 2_200 ||
		after.Head.Lists == 0 || !after.Head.MeasuredAt.Equal(now) ||
		!after.Head.Measurement.Passed() {
		t.Fatalf("a measurement that missed the floor left %+v — want a new index, "+
			"trained over the 2200 sources it measured, that passes", after.Head)
	}
	if kinds := publishedKinds(t, h); kinds[search.IndexSource] == 0 {
		t.Fatal("nothing about the index is on the log")
	}
}

// NOTHING IS DECIDED FROM A NODE THAT HAS NOT APPLIED THE LOG.
//
// A duty on a node that has not applied the whole log would see the index the
// log already replaced, or none, and train over a healthy one. So while the
// node is behind, the duty embeds as ever and publishes nothing about the
// index, and the corpus keeps the full scan; the moment it has applied the
// log, the next tick trains.
func TestTheIndexWaitsForTheLog(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 128, topics: 8}
	behind := true
	standing := h.standing()
	duty := indexDuty(t, h, embedder, func(ctx context.Context) (search.LogStanding, error) {
		s, err := standing(ctx)
		if behind {
			s.Current = false
		}
		return s, err
	}, func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	h.seedTasks(taskBodies(2_000))

	state, _ := runToRest(t, h, duty, embedder.width)
	if state.Sources != 2_000 || state.Indexed {
		t.Fatalf("a duty that had not applied the log came to rest over %d sources "+
			"with an index row %+v", state.Sources, state.Head)
	}
	if kinds := publishedKinds(t, h); kinds[search.IndexSource] != 0 {
		t.Fatalf("the log carries %d record(s) about the index from a node behind it",
			kinds[search.IndexSource])
	}
	behind = false
	if state, _ = runToRest(t, h, duty, embedder.width); !state.Indexed || state.Head.Lists == 0 {
		t.Fatalf("with the log applied, the duty came to rest with %+v", state.Head)
	}
}

// indexDuty is a duty over the harness's tasks, embedding with embedder and
// reading the log's standing from standing.
func indexDuty(t *testing.T, h *embedHarness, embedder topicalEmbedder, standing func(context.Context) (search.LogStanding, error), now func() time.Time) *search.Embedder {
	t.Helper()
	return boundedDuty(t, h, embedder, standing, now, unbounded{})
}

// boundedDuty is [indexDuty] with its ticks held to budget.
func boundedDuty(t *testing.T, h *embedHarness, embedder topicalEmbedder, standing func(context.Context) (search.LogStanding, error), now func() time.Time, budget search.Budget) *search.Embedder {
	t.Helper()
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: standing, Embedder: embedder, Model: embedModel,
		Corpora:  []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Now:      now,
		Budget:   budget,
		Refusals: search.NewRefusals(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return duty
}

// runToRest ticks the duty, applying the log after each tick, until two ticks
// in a row publish nothing, and reports the state it came to rest in and how
// many ticks rolled an index out.
func runToRest(t *testing.T, h *embedHarness, duty *search.Embedder, dim int) (search.IndexState, int) {
	t.Helper()
	return runToRestBy(t, h, duty, dim, func() (int, error) { return duty.Tick(t.Context()) })
}

// runToRestBy is [runToRest] with every tick taken by tick.
func runToRestBy(t *testing.T, h *embedHarness, duty *search.Embedder, dim int, tick func() (int, error)) (search.IndexState, int) {
	t.Helper()
	var state search.IndexState
	quiet, rollouts := 0, 0
	for ticks := 0; quiet < 2; ticks++ {
		if ticks == 20 {
			t.Fatalf("the duty was still publishing after %d ticks: %+v", ticks, state)
		}
		published, err := tick()
		if err != nil {
			t.Fatalf("tick %d: %v", ticks, err)
		}
		if state.Stale() > 0 && published > 0 {
			rollouts++
		}
		h.drain()
		if state, err = search.IndexStateOf(t.Context(), duty, dim); err != nil {
			t.Fatal(err)
		}
		if published == 0 {
			quiet++
		} else {
			quiet = 0
		}
	}
	return state, rollouts
}

// installedRollout is the installed index's rollout, batch by batch.
func installedRollout(t *testing.T, tx *sql.Tx) []search.RolloutRange {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(),
		`SELECT source, from_id, to_id FROM kb_ivf_rollout ORDER BY batch`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []search.RolloutRange
	for rows.Next() {
		var r search.RolloutRange
		var source string
		if err := rows.Scan(&source, &r.From, &r.To); err != nil {
			t.Fatal(err)
		}
		r.Source = search.Source(source)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// embeddedIDs is every id with a vector, by source.
func embeddedIDs(t *testing.T, tx *sql.Tx) map[search.Source][]string {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), `SELECT source, source_id FROM kb_vectors`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[search.Source][]string{}
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			t.Fatal(err)
		}
		out[search.Source(source)] = append(out[search.Source(source)], id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func taskBodies(n int) map[string]string {
	bodies := map[string]string{}
	for i := range n {
		bodies[fmt.Sprintf("t%05d", i)] = fmt.Sprintf("task %d", i)
	}
	return bodies
}

// published is the envelope of every record on the log, in log order.
func published(t *testing.T, h *embedHarness) []search.RecordEnvelope {
	t.Helper()
	last, err := h.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []search.RecordEnvelope
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := h.log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		env, err := search.DecodeEnvelope(payload)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	return out
}

// publishedVersions is every record version on the log, with the subject
// kinds written at it.
func publishedVersions(t *testing.T, h *embedHarness) map[int]map[search.Source]bool {
	t.Helper()
	out := map[int]map[search.Source]bool{}
	for _, env := range published(t, h) {
		if out[env.V] == nil {
			out[env.V] = map[search.Source]bool{}
		}
		out[env.V][env.Subject.Source] = true
	}
	return out
}

// publishedKinds counts the log's records by subject kind.
func publishedKinds(t *testing.T, h *embedHarness) map[search.Source]int {
	t.Helper()
	out := map[search.Source]int{}
	for _, env := range published(t, h) {
		out[env.Subject.Source]++
	}
	return out
}

// topicalEmbedder embeds a text as its topic's centre plus a little noise,
// both derived from the text alone — the one property a duty test needs from
// a provider being that the corpus it produces has topics to find.
//
// A text carrying [driftMarker] belongs to one of `drifted` topics PAST the
// ordinary ones, which is how a case makes a corpus MOVE by construction: the
// documents written after an index was trained land in a topic its training
// never saw, rather than in whichever topics a hash happened to favour.
type topicalEmbedder struct {
	width, topics int

	// noise is the spread around a topic's centre; zero is 0.5.
	noise float64

	// drifted is how many topics a drift-marked text is spread over.
	drifted int

	// requests, where not nil, counts the batch requests sent to the
	// embedder.
	requests *atomic.Int64
}

// driftMarker is the word that files a text in a drifted topic.
const driftMarker = "drift"

func (e topicalEmbedder) Width() int { return e.width }

func (e topicalEmbedder) Model() string { return "topical" }

// Limits are the fake provider's, which are the default model's: this embedder
// differs from it in what a vector looks like, never in what it accepts.
func (e topicalEmbedder) Limits() embeddings.Limits { return embeddings.NewFake(e.width).Limits() }

func (e topicalEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	sum := h.Sum64()
	topic := int(sum % uint64(e.topics))
	if e.drifted > 0 && strings.Contains(text, driftMarker) {
		topic = e.topics + int(sum%uint64(e.drifted))
	}
	spread := e.noise
	if spread == 0 {
		spread = 0.5
	}
	centre := rand.New(rand.NewPCG(uint64(topic), 0xCE47E))
	noise := rand.New(rand.NewPCG(sum, 0x401E))
	out := make([]float32, e.width)
	for i := range out {
		out[i] = float32(centre.NormFloat64() + spread*noise.NormFloat64())
	}
	return out, nil
}

func (e topicalEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if e.requests != nil {
		e.requests.Add(1)
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v, err := e.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// streamingReads are the reads of a training that stream rows, and so report
// their progress as they go.
var streamingReads = []string{"search.readTrainingSet", "search.exactTops"}

// The tick's other steps that report progress: a provider request, when it
// is answered; each vector an answered request publishes; and each batch
// record a rollout publishes.
const (
	requestFrame = "search.(*Embedder).request"
	publishFrame = "search.(*Embedder).publishAll"
	rolloutFrame = "search.(*Embedder).rollout"
)

// reporters is every frame a report of progress is counted against: the
// nearest on the stack is the reporter, so a vector published inside a
// request is counted as the publish's and never as the request's.
var reporters = append(slices.Clone(streamingReads), requestFrame, publishFrame,
	rolloutFrame)

// watchedBudget counts the exemptions open, and the reports of progress by
// the step that made them.
type watchedBudget struct {
	open atomic.Int64

	mu       sync.Mutex
	advanced map[string]int
}

func (b *watchedBudget) Exempt() func() {
	b.open.Add(1)
	return sync.OnceFunc(func() { b.open.Add(-1) })
}

func (b *watchedBudget) Advanced() {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if i := slices.IndexFunc(reporters, func(name string) bool {
			return strings.HasSuffix(frame.Function, "/"+name)
		}); i >= 0 {
			b.mu.Lock()
			b.advanced[reporters[i]]++
			b.mu.Unlock()
			return
		}
		if !more {
			return
		}
	}
}

// reported is how many times each read has reported progress so far.
func (b *watchedBudget) reported() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.advanced)
}

// arithmeticWatch is a tick's context that notes, at every reading the
// index's arithmetic makes of it — a training or filing stride, a probe count
// measured — whether the budget's clock was stopped.
type arithmeticWatch struct {
	context.Context
	budget          *watchedBudget
	exempt, charged atomic.Int64
}

// arithmetic is where the index's CPU-bound steps read their context.
var arithmetic = []string{"search.walkStrides", "search.ChooseProbes"}

func (c *arithmeticWatch) Err() error {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if slices.ContainsFunc(arithmetic, func(name string) bool {
			return strings.HasSuffix(frame.Function, "/"+name)
		}) {
			if c.budget.open.Load() > 0 {
				c.exempt.Add(1)
			} else {
				c.charged.Add(1)
			}
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Err()
}
