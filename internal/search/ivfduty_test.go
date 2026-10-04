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

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE DUTY TRAINS AN INDEX, AND ITS ROLLOUT CONVERGES — through the real record
// path, on a real broker.
//
// The pure gates certify the arithmetic and the applier; this certifies the
// LOOP. Ticks run until the duty has nothing left to do, and by then: the
// corpus has been embedded, an index trained each time it reached the minimum
// or doubled, every such index rolled out on the tick after it, nothing is
// left unfiled, a search probes the index, and a tick over that state
// publishes nothing at all. Each step is one tick and each is a record every
// holder applies.
func TestTheDutyTrainsAnIndexAndItsRolloutConverges(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	// SIXTEEN TOPICS, where an index trained at 2 048 sources still meets
	// the floor at the corpus's 2 200 a day later — so the day's step is a
	// measurement, which TestAMeasurementThatMissesTheFloorRetrains is not.
	embedder := topicalEmbedder{width: 384, topics: 16}
	now := time.Unix(1_700_000_000, 0).UTC()
	duty := indexDuty(t, h, embedder, h.standing(map[string]int{"node-a": search.RecordVersion}),
		func() time.Time { return now })
	// ENOUGH THAT AN ANSWER IS A SMALL SHARE OF ITS TOPIC: the shipped
	// answer is 150 documents deep, and a topic much smaller than that is
	// one no probe of a few lists can hold.
	h.seedTasks(taskBodies(2_200))

	state, rollouts := runToRest(t, h, duty, embedder.width)
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

	// AND THE INDEX IS WHAT A SEARCH PROBES.
	query, err := embedder.Embed(t.Context(), "a query about something")
	if err != nil {
		t.Fatal(err)
	}
	if err := storetest.EstateOf(h.db).Read(t.Context(), func(tx *sql.Tx) error {
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
	published, err := duty.Tick(t.Context())
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

	// AND EVERY RECORD THE DUTY PUBLISHED IS AT THE VERSION ITS KINDS WERE
	// INTRODUCED AT: a document's at 1, which every build reads, and the
	// index's at 2.
	for version, kinds := range publishedVersions(t, h) {
		for kind := range kinds {
			want := 1
			if kind == search.IndexSource {
				want = search.IndexRecordVersion()
			}
			if version != want {
				t.Fatalf("the duty published %s records at version %d, want %d",
					kind, version, want)
			}
		}
	}
}

// A MEASUREMENT THAT MISSES THE FLOOR RETRAINS IN THE SAME TICK, rather than
// recording a failing index for searches to read.
//
// Eight topics of about 275 sources each: the index this corpus trains at
// 2 048 sources meets the floor at half its lists, and measured a day later
// over the 2 200 the corpus came to rest at it needs every list — a corpus
// that moved since training, which is what the daily measurement exists to
// catch. The same tick trains its replacement from the reading already in
// hand, so the one record it publishes is a new index that passes, and no
// search is left reading one that does not.
func TestAMeasurementThatMissesTheFloorRetrains(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 384, topics: 8}
	now := time.Unix(1_700_000_000, 0).UTC()
	duty := indexDuty(t, h, embedder, h.standing(map[string]int{"node-a": search.RecordVersion}),
		func() time.Time { return now })
	h.seedTasks(taskBodies(2_200))
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

// NOTHING ABOUT THE INDEX IS PUBLISHED WHILE A NODE APPLYING THE LOG CANNOT
// READ IT — AND NOTHING IS DECIDED FROM A NODE THAT HAS NOT APPLIED THE LOG.
//
// A build older than the index's records stops its applier at the first one:
// its envelope decode refuses a subject kind it does not know, and the
// framework stops rather than defers on an envelope it cannot read. So while
// the log counts a node advertising no build that reads them — or one that
// reads below [search.IndexRecordVersion] — the duty embeds as ever and
// publishes nothing about the index, and the corpus keeps the full scan.
// The moment the last such node is upgraded, the next tick trains.
//
// And a duty on a node that has not applied the whole log decides nothing from
// its rows: it would see the index the log already replaced, or none, and
// train over a healthy one.
func TestTheIndexWaitsForEveryReaderAndForTheLog(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 128, topics: 8}
	readers := map[string]int{"node-a": search.RecordVersion, "node-old": 0}
	behind := false
	standing := h.standing(readers)
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
		t.Fatalf("with a node on a build that cannot read the index, the duty "+
			"came to rest over %d sources with an index row %+v — that node's "+
			"applier stops on the first index record", state.Sources, state.Head)
	}
	if kinds := publishedKinds(t, h); kinds[search.IndexSource] != 0 {
		t.Fatalf("the log carries %d record(s) about the index while a node "+
			"cannot read one", kinds[search.IndexSource])
	}

	readers["node-old"] = search.IndexRecordVersion() - 1
	if state, _ = runToRest(t, h, duty, embedder.width); state.Indexed {
		t.Fatalf("a node reading below the index's version held nothing back: %+v",
			state.Head)
	}

	// BEHIND THE LOG, with every reader upgraded: still nothing.
	readers["node-old"] = search.IndexRecordVersion()
	behind = true
	if state, _ = runToRest(t, h, duty, embedder.width); state.Indexed {
		t.Fatalf("a duty that had not applied the log trained an index: %+v",
			state.Head)
	}
	behind = false
	if state, _ = runToRest(t, h, duty, embedder.width); !state.Indexed || state.Head.Lists == 0 {
		t.Fatalf("with every reader upgraded and the log applied, the duty came "+
			"to rest with %+v", state.Head)
	}
}

// A NODE COUNTED WHILE A STEP RAN HOLDS WHAT THE STEP WOULD PUBLISH.
//
// The step is decided from the readers read at the tick's start, and a
// training publishes minutes later — on a slow node many, its arithmetic
// exempt from the tick's bound. A node that begins counting on the log in
// between — an old binary booted as a new data node, a node rolled back
// mid-upgrade — stops its applier on the first index record rather than
// deferring it. So the readers are read again beside every index append: a
// node counted during the training's arithmetic holds the centroids record,
// one counted after the first batch of a rollout holds the rest, one counted
// during a measurement holds its record, and once it is upgraded the duty
// takes each step again and finishes.
func TestAReaderCountedWhileAStepRunsHoldsWhatItWouldPublish(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 128, topics: 8}
	standing := h.standing(map[string]int{"node-a": search.RecordVersion})
	var (
		mu     sync.Mutex
		joined bool
		// joinOn is the step whose progress counts an old node in.
		joinOn string
		now    = time.Unix(1_700_000_000, 0).UTC()
	)
	join := func(on string) {
		mu.Lock()
		defer mu.Unlock()
		if joinOn == on {
			joined = true
		}
	}
	duty := boundedDuty(t, h, embedder, func(ctx context.Context) (search.LogStanding, error) {
		s, err := standing(ctx)
		mu.Lock()
		defer mu.Unlock()
		if joined {
			s.Readers = maps.Clone(s.Readers)
			s.Readers["node-old"] = 0
		}
		return s, err
	}, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}, hookedBudget{advanced: func() { join("rollout") }, exempt: func() { join("arithmetic") }})
	h.seedTasks(taskBodies(2_000))
	arm := func(on string, counted bool) {
		mu.Lock()
		defer mu.Unlock()
		joinOn, joined = on, counted
	}

	// THE TRAINING: its arithmetic counts the old node in, after the tick
	// decided to train with every reader upgraded.
	arm("arithmetic", false)
	state, _ := runToRest(t, h, duty, embedder.width)
	if state.Sources != 2_000 || state.Indexed {
		t.Fatalf("a node counted while the index trained did not hold it: the duty "+
			"came to rest over %d sources with %+v", state.Sources, state.Head)
	}
	if kinds := publishedKinds(t, h); kinds[search.IndexSource] != 0 {
		t.Fatalf("the log carries %d record(s) about the index, published past a "+
			"node counted while the training ran", kinds[search.IndexSource])
	}

	// UPGRADED: the next tick trains again and publishes its centroids.
	arm("", false)
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the training tick published %d record(s) (%v), want its centroids", published, err)
	}
	h.drain()
	if state, _ = search.IndexStateOf(t.Context(), duty, embedder.width); !state.Indexed ||
		state.Stale() == 0 {
		t.Fatalf("after the training the index reads %+v with %d rows unfiled, want "+
			"an installed index whose rollout is still to come", state.Head, state.Stale())
	}

	// THE ROLLOUT: its first batch counts the old node in, and the rest wait.
	arm("rollout", false)
	published, err := duty.Tick(t.Context())
	if err != nil || published != 1 {
		t.Fatalf("a rollout that counted a node after its first batch published %d "+
			"batch(es) (%v), want that first one alone", published, err)
	}
	h.drain()
	if state, _ = search.IndexStateOf(t.Context(), duty, embedder.width); state.Stale() == 0 {
		t.Fatal("the whole rollout landed past a node counted after its first batch")
	}
	arm("", false)
	if state, _ = runToRest(t, h, duty, embedder.width); !state.Indexed || state.Stale() != 0 {
		t.Fatalf("once every reader was upgraded the duty came to rest with %d rows "+
			"unfiled (%+v)", state.Stale(), state.Head)
	}

	// A DAY ON, THE MEASUREMENT: its arithmetic counts the old node in, and
	// its record waits for the upgrade too.
	mu.Lock()
	now = now.Add(search.IVFMeasureInterval)
	mu.Unlock()
	arm("arithmetic", false)
	if published, err := duty.Tick(t.Context()); err != nil || published != 0 {
		t.Fatalf("a measurement that counted a node while it ran published %d "+
			"record(s) (%v), want none", published, err)
	}
	arm("", false)
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the upgraded fleet's measurement published %d record(s) (%v), "+
			"want its one", published, err)
	}
	h.drain()
	if measured, _ := search.IndexStateOf(t.Context(), duty, embedder.width); !measured.Head.MeasuredAt.Equal(now) {
		t.Fatalf("the measurement is dated %v, want %v", measured.Head.MeasuredAt, now)
	}
}

// hookedBudget is a budget that runs a hook on each report — the progress a
// step shows, and the arithmetic it exempts — and bounds nothing.
type hookedBudget struct{ advanced, exempt func() }

func (b hookedBudget) Advanced() { b.advanced() }

func (b hookedBudget) Exempt() func() {
	b.exempt()
	return func() {}
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
		Publisher: h.publisher, Estate: storetest.EstateOf(h.db).Reader(), Log: statelog.EstateStream(search.Domain{}).Name,
		Standing: standing, Embedder: embedder, Model: embedModel,
		Corpora: []search.Corpus{search.TaskCorpus{DB: storetest.EstateOf(h.db).Reader()}},
		Now:     now,
		Budget:  budget,
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

func taskBodies(n int) map[string]string {
	bodies := map[string]string{}
	for i := range n {
		bodies[fmt.Sprintf("t%05d", i)] = fmt.Sprintf("task %d", i)
	}
	return bodies
}

// publishedVersions is every record version on the log, with the subject
// kinds written at it.
func publishedVersions(t *testing.T, h *embedHarness) map[int]map[search.Source]bool {
	t.Helper()
	last, err := h.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]map[search.Source]bool{}
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
	last, err := h.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[search.Source]int{}
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
		out[env.Subject.Source]++
	}
	return out
}

// topicalEmbedder embeds a text as its topic's centre plus a little noise,
// both derived from the text alone — the one property a duty test needs from
// a provider being that the corpus it produces has topics to find.
type topicalEmbedder struct {
	width, topics int
}

func (e topicalEmbedder) Width() int { return e.width }

func (e topicalEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	sum := h.Sum64()
	topic := int(sum % uint64(e.topics))
	centre := rand.New(rand.NewPCG(uint64(topic), 0xCE47E))
	noise := rand.New(rand.NewPCG(sum, 0x401E))
	out := make([]float32, e.width)
	for i := range out {
		out[i] = float32(centre.NormFloat64() + 0.5*noise.NormFloat64())
	}
	return out, nil
}

func (e topicalEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
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

// A TRAINING SHOWS THE TICK'S BOUND ITS PROGRESS, in a training and in the
// day's measurement alike, and no exemption outlives the tick — and so does
// every batch the tick embeds.
//
// The bound exists to cut off a tick that WEDGED, and a wedge is the absence
// of progress — so every long step of a training must show it, or on a node
// allowed one core, where a training at the largest corpus is over six
// minutes of reading and six more of arithmetic, the bound cut off every
// training such a node began, for ever. The reading of every code and the
// exact pass stream rows: each reports every [search.ProgressStride] of them,
// and the tick reports every batch it embeds.
// The k-means, the filing and the choice of a probe count cannot wedge — they
// are arithmetic over values in memory, reading their context every stride —
// so every context reading they make happens with the bound's clock stopped.
// And an exemption must be over by the end of the tick: one left open would
// stop the clock on the reads and publishes after it, which are what the
// bound is for.
func TestATrainingShowsTheTicksBoundItsProgress(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	embedder := topicalEmbedder{width: 384, topics: 16}
	now := time.Unix(1_700_000_000, 0).UTC()
	budget := &watchedBudget{advanced: map[string]int{}}
	duty := boundedDuty(t, h, embedder, h.standing(map[string]int{"node-a": search.RecordVersion}),
		func() time.Time { return now }, budget)
	h.seedTasks(taskBodies(2_200))
	ctx := &arithmeticWatch{Context: t.Context(), budget: budget}

	state, _ := runToRestBy(t, h, duty, embedder.width,
		func() (int, error) { return duty.Tick(ctx) })
	if !state.Indexed || state.Head.Lists == 0 {
		t.Fatalf("setup: the duty came to rest with no index: %+v", state.Head)
	}
	// EVERY STRIDE OF BOTH READS, in the one training that installed it.
	strides := state.Sources / search.ProgressStride
	if strides < 2 {
		t.Fatalf("setup: %d sources is under two strides of rows", state.Sources)
	}
	trainedReads := budget.reported()
	if batches := (2_200 + search.EmbedBatch - 1) / search.EmbedBatch; trainedReads[tickFrame] < batches {
		t.Fatalf("the ticks that embedded 2200 sources reported progress %d time(s), "+
			"want once a batch (%d)", trainedReads[tickFrame], batches)
	}
	for _, reader := range streamingReads {
		if got := trainedReads[reader]; got < strides {
			t.Fatalf("%s reported progress %d time(s) over %d rows, want every %d "+
				"rows (%d): a read that shows none is cut off as wedged on a slow node",
				reader, got, state.Sources, search.ProgressStride, strides)
		}
	}
	trained := ctx.exempt.Load()
	now = now.Add(search.IVFMeasureInterval)
	if published, err := duty.Tick(ctx); err != nil || published != 1 {
		t.Fatalf("setup: a day on, the tick published %d record(s): %v", published, err)
	}
	measuredReads := budget.reported()
	for _, reader := range streamingReads {
		if got := measuredReads[reader] - trainedReads[reader]; got < strides {
			t.Fatalf("the day's measurement's %s reported progress %d time(s), want %d",
				reader, got, strides)
		}
	}
	switch {
	case ctx.charged.Load() != 0:
		t.Fatalf("%d of the index's context readings ran with the bound's clock "+
			"running (%d exempt): its arithmetic is charged to the tick", ctx.charged.Load(),
			ctx.exempt.Load())
	case trained == 0:
		t.Fatal("no stride of the training was seen at all — the watch reads nothing")
	case ctx.exempt.Load() == trained:
		t.Fatal("no probe count of the day's measurement was seen exempt")
	case budget.open.Load() != 0:
		t.Fatalf("%d exemption(s) outlived the tick: the bound's clock stays stopped "+
			"on everything after it", budget.open.Load())
	}
}

// streamingReads are the reads of a training that stream rows, and so report
// their progress as they go.
var streamingReads = []string{"search.readTrainingSet", "search.exactTops"}

// tickFrame is the tick itself, which reports every batch it embeds and every
// vector it withdraws.
const tickFrame = "search.(*Embedder).Tick"

// reporters is every frame a report of progress is counted against, a read
// before the tick that called it: the nearest on the stack is the reporter.
var reporters = append(slices.Clone(streamingReads), tickFrame)

// watchedBudget counts the exemptions open, and the reports of progress by
// the read that made them.
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
