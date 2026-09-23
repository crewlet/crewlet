package search_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ROUND TRIP: the duty embeds, the record reaches a real broker, this
// node's applier consumes it, and the two-stage search answers from the rows
// it wrote.
//
// Every layer below has its own tests over fakes, and every one can be
// individually right while the composition is wrong: a subject the publisher
// builds and the applier does not recognise, a vector packed one way and read
// another, a scope the duty resolves and the framework refuses. None of that
// is visible without a real broker and a real store on both ends.
func TestTheEmbedDutyReachesTheSearch(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)

	h.seedTasks(map[string]string{
		"t-rate":  "rate limits and 429 backoff in the GitLab client",
		"t-cache": "the page cache size the store asks for at open",
		"t-empty": "",
	})

	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if published != 2 {
		t.Fatalf("the duty published %d record(s) for two embeddable tasks "+
			"and one with no text", published)
	}
	h.drain()

	// THE ANSWER COMES FROM THE ROWS THE APPLY WROTE, through the same
	// two-stage statement the engine runs.
	vector, err := h.embedder.Embed(t.Context(), "429 rate limit backoff")
	if err != nil {
		t.Fatalf("embed the query: %v", err)
	}
	var hits []search.SemanticHit
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		hits, err = search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: pack(vector), Model: embedModel,
			Dim: h.embedder.Width(), Limit: 5,
		})
		return err
	}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("the search returned %d hits over two embedded tasks", len(hits))
	}
	if hits[0].ID != "t-rate" {
		t.Fatalf("the nearest hit is %q, and the query shares every word with "+
			"t-rate", hits[0].ID)
	}

	// A SECOND TICK EMBEDS NOTHING, because the selection is derived from
	// the rows: a source whose vector is current is not selected, which is
	// what stops the duty paying the provider for the same corpus every
	// minute for the life of the deployment.
	again, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if again != 0 {
		t.Fatalf("a second tick published %d record(s) over an unchanged "+
			"corpus", again)
	}
}

// A REMOVED TASK'S VECTOR IS WITHDRAWN, and this is the direction nothing else
// can repair.
//
// The selection that embeds a source reads the source's own row. When that row
// is gone, no selection over it will ever mention the document again — so
// without the other direction a deleted task stays findable by meaning for
// ever, which is the search half of a deletion that did not happen.
func TestARemovedTaskLosesItsVector(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTasks(map[string]string{"t-gone": "something to forget"})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()
	if got := h.vectors(); got != 1 {
		t.Fatalf("%d vector(s) after the first tick, want 1", got)
	}

	h.removeTask("t-gone")
	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick after the removal: %v", err)
	}
	if published != 1 {
		t.Fatalf("the duty published %d record(s) for one removed task", published)
	}
	h.drain()
	if got := h.vectors(); got != 0 {
		t.Fatalf("%d vector(s) survive a removed task — the row it came from "+
			"is gone, so nothing else will ever select it", got)
	}
}

// A REMOVED LONG TASK LOSES EVERY WINDOW, not only its first.
//
// Each window is its own subject and its own row, and the applier forgets
// exactly the window a record names. The forget selection is the only thing
// that ever names a gone document's windows — the stale selection is driven by
// the source rows, which are gone — so a forget that named chunk 0 alone would
// leave the rest of the document findable by meaning with nothing left to
// select it. The single-window case above cannot see that: its one window IS
// chunk 0.
func TestARemovedLongTaskLosesEveryWindow(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTasks(map[string]string{
		"t-long": strings.Repeat("a long body with plenty of prose. ", 500),
		"t-kept": "a short task that stays",
	})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()
	before := h.vectors()
	if before < 3 {
		t.Fatalf("the long task produced %d vector(s) with its neighbour, want "+
			"several — without windows this case asserts nothing", before)
	}

	h.removeTask("t-long")
	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick after the removal: %v", err)
	}
	if want := before - 1; published != want {
		t.Fatalf("the duty published %d forget(s) for a removed task of %d "+
			"window(s)", published, want)
	}
	h.drain()
	if got := h.vectors(); got != 1 {
		t.Fatalf("%d vector(s) survive, want only the kept task's one: every "+
			"window of a removed document is findable by meaning until "+
			"something forgets it, and nothing else ever selects it", got)
	}
	// AND A TICK AFTER THAT HAS NOTHING LEFT TO FORGET, which is what says
	// the selection is over the rows rather than a list it keeps.
	if published, err := h.duty.Tick(t.Context()); err != nil || published != 0 {
		t.Fatalf("the tick after every window was forgotten published %d "+
			"record(s), err %v", published, err)
	}
}

// A PROVIDER FAILURE COSTS THE BATCH AND NOT THE CORPUS.
//
// The tick's unit of work is a batch and the failures this meets are per batch
// — a rate limit, a timeout, one document the provider refuses. Failing the
// tick on the first one abandons every batch after it, so the corpus stops
// being embedded because of one document in it, and the symptom is a search
// that quietly stops improving.
func TestAFailedBatchDoesNotStopTheTick(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	// More than one batch's worth, with the FIRST batch poisoned.
	seed := map[string]string{}
	for i := range search.EmbedBatch + 5 {
		seed[fmt.Sprintf("t-%04d", i)] = fmt.Sprintf("document number %d", i)
	}
	h.seedTasks(seed)
	h.embedder.failFirst = true

	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("the tick returned an error rather than carrying: %v", err)
	}
	if published != 5 {
		t.Fatalf("the tick published %d record(s); the first batch of %d was "+
			"refused and the second holds 5", published, search.EmbedBatch)
	}
	h.drain()

	// AND THE NEXT TICK PICKS UP WHAT THE FAILED BATCH LEFT, because
	// nothing was recorded for it: the selection is over the rows.
	h.embedder.failFirst = false
	again, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if again != search.EmbedBatch {
		t.Fatalf("the retry published %d of the %d the failed batch held",
			again, search.EmbedBatch)
	}
}

// A CORPUS THAT IS ALWAYS BEHIND DOES NOT STARVE THE ONE AFTER IT.
//
// The tick's provider calls are one budget shared by every corpus, and spent
// corpus by corpus in order they all went to the first one that could take
// them: a company writing tasks faster than the duty embeds them — or simply
// one still cold-filling a hundred thousand — never reached its pages at all.
// Not one page embedded, not one trashed page's vector withdrawn, for as long
// as the backlog refilled, while the coverage gauge summed both corpora and
// reported a company that was merely behind.
func TestABackloggedCorpusDoesNotStarveTheOneAfterIt(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.blank = true
	tasks := &scriptedCorpus{source: search.SourceTask, backlog: -1}
	// THE SECOND CORPUS IS BEHIND TOO, and also carries a vector to
	// withdraw: under the old scheduling its selection never ran at all,
	// so the page somebody deleted stayed findable by meaning for ever.
	pages := &scriptedCorpus{source: search.SourcePage, backlog: -1,
		gone: []search.Subject{{Source: search.SourcePage, ID: "p-deleted"}}}

	published, err := h.dutyOver(tasks, pages).Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	calls := h.embedder.callsPerSource()
	want := search.EmbedBatchesPerTick / 2
	if calls[search.SourcePage] != want || calls[search.SourceTask] != want {
		t.Fatalf("the tick spent %d call(s) on tasks and %d on pages; two "+
			"corpora that are both behind divide the tick's %d evenly, and "+
			"a page corpus at zero is a company whose wiki is unsearchable "+
			"by meaning for as long as the task backlog refills",
			calls[search.SourceTask], calls[search.SourcePage],
			search.EmbedBatchesPerTick)
	}
	if total := calls[search.SourceTask] + calls[search.SourcePage]; total != search.EmbedBatchesPerTick {
		t.Fatalf("the tick made %d provider calls against a ceiling of %d — "+
			"the ceiling is what bounds the company's embedding bill, and "+
			"sharing it fairly may not raise it", total, search.EmbedBatchesPerTick)
	}
	if published != 1 {
		t.Fatalf("the tick published %d record(s); the provider embedded "+
			"nothing, so the one record is the second corpus's withdrawal — "+
			"and a corpus that is never selected withdraws nothing", published)
	}
}

// AND A CORPUS WITH NOTHING TO DO WASTES NOTHING.
//
// The share is a floor, not a quota: reserving four calls for a caught-up
// wiki would halve the rate a cold fill of the tracker proceeds at, for a
// corpus with no work to put them to. What a corpus does not use is taken by
// whoever still has a backlog, which is what makes the round robin a reserved
// share and its redistribution in one rule.
func TestAnIdleCorpusGivesItsShareToTheRest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		pageBacklog int
		wantTask    int
		wantPage    int
	}{
		{name: "an empty wiki", pageBacklog: 0,
			wantTask: search.EmbedBatchesPerTick, wantPage: 0},
		{name: "one page behind", pageBacklog: 1,
			wantTask: search.EmbedBatchesPerTick - 1, wantPage: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newEmbedHarness(t)
			h.embedder.blank = true
			tasks := &scriptedCorpus{source: search.SourceTask, backlog: -1}
			pages := &scriptedCorpus{source: search.SourcePage, backlog: tc.pageBacklog}

			if _, err := h.dutyOver(tasks, pages).Tick(t.Context()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			calls := h.embedder.callsPerSource()
			if calls[search.SourceTask] != tc.wantTask || calls[search.SourcePage] != tc.wantPage {
				t.Fatalf("the tick spent %d call(s) on tasks and %d on pages, "+
					"want %d and %d — a call the second corpus has no work for "+
					"belongs to the one that does, or the ceiling buys less "+
					"than it costs", calls[search.SourceTask],
					calls[search.SourcePage], tc.wantTask, tc.wantPage)
			}
		})
	}
}

// AN UNREADABLE CORPUS COSTS ITSELF AND NOT THE TICK.
//
// This is the same starvation arriving as an error rather than as a backlog:
// the corpora are visited in order, so a selection that fails and returns
// stops every corpus behind it — this tick and every tick, because the failure
// is a property of that corpus's own query. The tick still reports it, because
// a corpus nobody can select is an operator's problem rather than a quiet one.
func TestAnUnreadableCorpusDoesNotStopTheOnesAfterIt(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.blank = true
	boom := errors.New("the anti-join timed out")
	tasks := &scriptedCorpus{source: search.SourceTask, err: boom}
	pages := &scriptedCorpus{source: search.SourcePage, backlog: -1}

	_, err := h.dutyOver(tasks, pages).Tick(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("a corpus whose selection failed answered %v — a tick that "+
			"carries the failure must still report it", err)
	}
	calls := h.embedder.callsPerSource()
	if calls[search.SourcePage] != search.EmbedBatchesPerTick {
		t.Fatalf("the tick spent %d call(s) on the second corpus after the "+
			"first one's selection failed, want the whole ceiling of %d",
			calls[search.SourcePage], search.EmbedBatchesPerTick)
	}
}

// MORE CORPORA THAN THERE ARE CALLS IS A REFUSED WIRING.
//
// Below one call apiece there is no share left to guarantee: every tick starts
// its round at the front, so the corpora past the ceiling would be embedded
// never rather than slowly. Refusing says so at the wiring, which is the one
// place the extra corpus can be taken back out.
func TestTheDutyRefusesMoreCorporaThanATickCanServe(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	corpora := make([]search.Corpus, search.EmbedBatchesPerTick+1)
	for i := range corpora {
		corpora[i] = &scriptedCorpus{source: search.SourceTask, backlog: -1}
	}
	_, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Embedder: h.embedder, Model: embedModel,
		Corpora: corpora,
	})
	if err == nil {
		t.Fatalf("a duty with %d corpora and %d calls a tick was accepted — "+
			"the ones past the ceiling are never embedded, and the coverage "+
			"gauge sums them into a company that merely looks behind",
			len(corpora), search.EmbedBatchesPerTick)
	}
}

// A NON-FINITE COMPONENT NEVER REACHES THE STREAM.
//
// This is the one check that must happen at the WRITER rather than at the
// store's own boundary: a record is published to every node before any of them
// writes it, so a NaN refused at the write boundary is a poison message every
// applier fails on for ever, on a stream with no dead-letter path. And a
// vector_distance_cos of NaN answers 0 — a PERFECT match — so one such row
// would outrank every genuine hit in every search the company ever runs.
//
// The refusal costs the DOCUMENT and not the batch, which the second half
// asserts: one bad component must not throw away a hundred and twenty-seven
// good vectors the same provider call already paid for.
func TestAPoisonedVectorIsRefusedBeforeItIsPublished(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTasks(map[string]string{"t-nan": "poison", "t-fine": "also poison"})
	h.embedder.poisonID = 0

	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if published != 1 {
		t.Fatalf("the tick published %d record(s); one vector of two was "+
			"poisoned, and the other was already paid for", published)
	}
	h.drain()
	if got := h.vectors(); got != 1 {
		t.Fatalf("%d vector(s) were written, want the one good one", got)
	}
}

// THE OPERATION ID IS DERIVED FROM THE RECORD, not minted per attempt.
//
// It is the only dedupe this domain has — there is no operation ledger — so a
// fresh id per attempt would make the broker's duplicate window collapse
// nothing, and every retry of the same work would be a second record on the
// stream. Derived, a retry carries the same id; a genuinely newer vector for
// the same source carries a different one, because the source version and the
// text digest are both in it.
func TestARetryOfTheSameWorkCarriesTheSameOperationID(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTasks(map[string]string{"t-a": "the first body"})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	first := h.opIDs()
	if len(first) != 1 {
		t.Fatalf("%d record(s) on the stream after one tick", len(first))
	}

	// THE SAME WORK AGAIN: nothing was applied, so the selection returns
	// the same document and the duty publishes it a second time.
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	second := h.opIDs()
	if len(second) != 1 || second[0] != first[0] {
		t.Fatalf("after a retry the stream holds %v; the first attempt wrote "+
			"%v, and a derived id is what lets the broker's duplicate window "+
			"collapse the second — which is the only dedupe this domain has, "+
			"since it keeps no operation ledger", second, first)
	}

	// A CHANGED BODY IS DIFFERENT WORK.
	h.drain()
	h.seedTasks(map[string]string{"t-a": "a body somebody rewrote"})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	third := h.opIDs()
	if len(third) == 0 || third[len(third)-1] == first[0] {
		t.Fatalf("a re-embed after the text changed left the stream at %v — it "+
			"carries the op id of the vector it replaces, so the broker "+
			"collapses it and the new vector is never written", third)
	}
}

// ---- the harness ------------------------------------------------------ //

const embedModel = "fake-embed"

type embedHarness struct {
	t         *testing.T
	db        *store.DB
	log       *js.DomainLog
	duty      *search.Embedder
	publisher *statelog.Publisher
	applier   search.Applier
	embedder  *scriptedEmbedder
	consumed  uint64
	version   int64
}

func newEmbedHarness(t *testing.T) *embedHarness {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := search.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD THIS HARNESS OVERRIDES: the shipped
	// default is sized for the declared supported corpus, and an embedded
	// broker in a temporary directory refuses to reserve it.
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := &embedHarness{
		t: t, db: db, log: log, applier: search.NewApplier(),
		embedder: &scriptedEmbedder{Fake: embeddings.NewFake(64), poisonID: -1},
	}
	rows, err := search.NewRows(db)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: search.Domain{}, Log: log, Rows: rows,
		Fence: search.NewFence(), Gates: search.NewGates(), Waiter: &embedWaiter{},
		NodeID: "node-a", Generation: func() uint32 { return 0 },
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	h.publisher = publisher
	h.duty = h.dutyOver(search.TaskCorpus{DB: db})
	return h
}

// dutyOver builds a duty over the corpora a test dictates, on this harness's
// own publisher — so a scheduling case reaches the same broker and the same
// record path as the round trip above.
func (h *embedHarness) dutyOver(corpora ...search.Corpus) *search.Embedder {
	h.t.Helper()
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Embedder: h.embedder, Model: embedModel,
		Corpora: corpora,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		h.t.Fatalf("build the duty: %v", err)
	}
	return duty
}

// seedTasks writes tracker rows DIRECTLY, because this suite is about the
// embedding path rather than about the tracker's own write path — and the only
// thing the duty reads from those rows is the four columns below.
func (h *embedHarness) seedTasks(bodies map[string]string) {
	h.t.Helper()
	ids := make([]string, 0, len(bodies))
	for id := range bodies {
		ids = append(ids, id)
	}
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		for _, id := range ids {
			h.version++
			document := fmt.Sprintf(`{"body":%q}`, bodies[id])
			// A DOCUMENT WITH NO TEXT AT ALL needs an empty title
			// too: a task titled after its own id is embeddable
			// text, and a fixture that meant to test the empty case
			// would silently be testing the ordinary one.
			title := id
			if bodies[id] == "" {
				title = ""
			}
			if _, err := tx.ExecContext(h.t.Context(), `
				INSERT INTO tracker_tasks
					(id, key, project_key, root_id, type, title, status,
					 status_group, rank, version, created_at, updated_at,
					 document)
				VALUES (?, ?, 'ENG', ?, 'task', ?, 'todo', 'open', 'm0',
				        ?, 0, ?, ?)
				ON CONFLICT (id) DO UPDATE SET
					document = excluded.document, version = excluded.version,
					updated_at = excluded.updated_at`,
				id, strings.ToUpper(id), id, title, h.version, h.version, document,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		h.t.Fatalf("seed: %v", err)
	}
}

func (h *embedHarness) removeTask(id string) {
	h.t.Helper()
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(),
			`UPDATE tracker_tasks SET removed_at = 1 WHERE id = ?`, id)
		return err
	}); err != nil {
		h.t.Fatalf("remove: %v", err)
	}
}

// drain consumes every record the broker holds beyond what this node applied,
// one transaction per record, exactly as the framework's own loop does.
func (h *embedHarness) drain() {
	h.t.Helper()
	last, err := h.log.End(h.t.Context())
	if err != nil {
		h.t.Fatalf("read the log's end: %v", err)
	}
	for seq := h.consumed + 1; seq <= last; seq++ {
		_, payload, storedAt, ok, err := h.log.At(h.t.Context(), seq)
		if err != nil {
			h.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		env, err := search.Domain{}.Envelope(payload)
		if err != nil {
			h.t.Fatalf("decode record %d: %v", seq, err)
		}
		record := statelog.Record{
			Envelope: env,
			Position: statelog.Position{
				Stream: search.Domain{}.Stream().Name, Generation: env.Gen, Seq: seq,
			},
			Payload: payload, StoredAt: storedAt,
		}
		if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
			_, err := h.applier.Apply(h.t.Context(), tx, record,
				statelog.ApplyOptions{StoredAt: storedAt})
			return err
		}); err != nil {
			h.t.Fatalf("apply record %d: %v", seq, err)
		}
	}
	h.consumed = last
}

func (h *embedHarness) vectors() int {
	h.t.Helper()
	var n int
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM kb_vectors`).Scan(&n)
	}); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

// opIDs is every operation id on the stream, in order.
func (h *embedHarness) opIDs() []string {
	h.t.Helper()
	last, err := h.log.End(h.t.Context())
	if err != nil {
		h.t.Fatalf("read the log's end: %v", err)
	}
	var out []string
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := h.log.At(h.t.Context(), seq)
		if err != nil {
			h.t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		env, err := search.DecodeEnvelope(payload)
		if err != nil {
			h.t.Fatalf("decode record %d: %v", seq, err)
		}
		out = append(out, env.OpID)
	}
	return out
}

// scriptedEmbedder is the fake with two faults a test can ask for.
type scriptedEmbedder struct {
	*embeddings.Fake
	mu        sync.Mutex
	calls     int
	failFirst bool
	// poisonID is the index within a batch whose vector is made
	// non-finite, or -1 for none.
	poisonID int
	// blank answers every input with no vector at all, which is a real
	// state — an empty source — and is what lets a scheduling test spend
	// the tick's provider calls without publishing a record for each of
	// the thousand documents they carry.
	blank bool
	// batches is what each call was asked to embed, in order, so a test
	// can see how the tick's calls were divided.
	batches [][]string
}

func (s *scriptedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	fail, poison, blank := s.failFirst, s.poisonID, s.blank
	s.batches = append(s.batches, append([]string(nil), texts...))
	s.mu.Unlock()
	if fail && first {
		return nil, fmt.Errorf("the provider refused this batch")
	}
	if blank {
		return make([][]float32, len(texts)), nil
	}
	out, err := s.Fake.EmbedBatch(ctx, texts)
	if err != nil || poison < 0 || poison >= len(out) {
		return out, err
	}
	if v := out[poison]; len(v) > 0 {
		v[0] = float32(nan())
	}
	return out, nil
}

// callsPerSource is how many provider calls each source's documents were sent
// in, read back from the text itself — [scriptedCorpus] titles every document
// with its own source.
func (s *scriptedEmbedder) callsPerSource() map[search.Source]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[search.Source]int{}
	for _, batch := range s.batches {
		if len(batch) == 0 {
			continue
		}
		out[search.Source(strings.SplitN(batch[0], " ", 2)[0])]++
	}
	return out
}

// scriptedCorpus is a corpus whose backlog a test dictates.
//
// It stands in for the real two deliberately: what the scheduling has to
// survive is a corpus that is STILL behind after the duty has spent everything
// on it, and seeding a million tracker rows to express that would be testing
// the fixture. `backlog` of -1 is that corpus — every selection answers with a
// full limit's worth, exactly as a company writing faster than the duty embeds
// looks from here.
type scriptedCorpus struct {
	source  search.Source
	backlog int
	gone    []search.Subject
	err     error
}

func (c *scriptedCorpus) Source() search.Source { return c.source }

func (c *scriptedCorpus) Stale(_ context.Context, _ string, _, limit int) ([]search.Document, []search.Subject, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	n := c.backlog
	if n < 0 || n > limit {
		n = limit
	}
	docs := make([]search.Document, n)
	for i := range docs {
		docs[i] = search.Document{
			ID:        fmt.Sprintf("%s-%05d", c.source, i),
			Container: "ENG",
			Version:   1,
			// THE SOURCE IS THE FIRST WORD OF THE TEXT, which is
			// what lets the embedder's record say which corpus a
			// provider call was spent on.
			Title: fmt.Sprintf("%s document %05d", c.source, i),
		}
	}
	return docs, c.gone, nil
}

func (c *scriptedCorpus) Coverage(context.Context, string, int) (int, int, error) {
	return 0, 0, nil
}

func nan() float64 { var zero float64; return zero / zero }

// embedWaiter is a waiter that is always caught up, which is honest for a
// domain nothing waits on.
type embedWaiter struct{}

func (embedWaiter) Committed() statelog.Position { return statelog.Position{} }
func (embedWaiter) WaitCommitted(context.Context, statelog.Position) error {
	return nil
}
func (embedWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return nil
}

// A LONG DOCUMENT IS FINDABLE BY ITS TAIL, end to end: the duty splits it, a
// real broker carries a record per window, this node's applier writes them,
// and the two-stage search answers from the rows.
//
// This is the whole point of chunking, and no unit test over the splitter can
// see it: the old duty cut every source at 8 KiB, so a query matching only
// text below that cut returned the document it did NOT match instead — and
// every layer in between was individually correct.
func TestALongDocumentIsFoundByTextPastTheOldCut(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)

	// The distinguishing words sit well past 8 KiB; before them is filler
	// the other task matches just as well.
	filler := strings.Repeat("the platform has many procedures. ", 400)
	h.seedTasks(map[string]string{
		"t-long":  filler + "rate limits and 429 backoff in the gitlab client",
		"t-other": filler + "the page cache size the store asks for at open",
	})

	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()

	if got := h.vectors(); got <= 2 {
		t.Fatalf("two documents of %d bytes produced %d vector(s): they were "+
			"cut rather than chunked", len(filler), got)
	}

	vector, err := h.embedder.Embed(t.Context(), "429 rate limit backoff")
	if err != nil {
		t.Fatalf("embed the query: %v", err)
	}
	var hits []search.SemanticHit
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		var err error
		hits, err = search.Semantic(t.Context(), tx, search.SemanticQuery{
			Vector: pack(vector), Model: embedModel,
			Dim: h.embedder.Width(), Limit: 5,
		})
		return err
	}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("the search returned %d hits over two documents — a document "+
			"is one hit however many windows it has", len(hits))
	}
	if hits[0].ID != "t-long" {
		t.Errorf("the nearest hit is %q: the words the query shares with "+
			"t-long are past the old cut, so they reached no vector",
			hits[0].ID)
	}
}

// A DOCUMENT THAT SHRANK LOSES THE WINDOWS IT NO LONGER FILLS.
//
// The anti-join that selects stale sources is driven by the SOURCE rows, and
// they say only that the document is current — so nothing else will ever
// mention the orphaned windows. Left behind, they hold text the document no
// longer contains and stay findable by meaning for ever.
func TestADocumentThatShrankDropsItsExtraWindows(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)

	h.seedTasks(map[string]string{
		"t-1": strings.Repeat("a long first draft with plenty of prose. ", 500),
	})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	h.drain()
	before := h.vectors()
	if before < 3 {
		t.Fatalf("the long draft produced %d vector(s), want several", before)
	}

	// Rewritten to one line. The version moves, so the duty re-selects it.
	h.seedTasks(map[string]string{"t-1": "cut to one line."})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	h.drain()

	if got := h.vectors(); got != 1 {
		t.Errorf("the document holds %d vector(s) after shrinking to one "+
			"window, down from %d: the rest carry text it no longer contains "+
			"and nothing else will ever select them", got, before)
	}
}
