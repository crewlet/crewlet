package search_test

import (
	"context"
	"database/sql"
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
	t        *testing.T
	db       *store.DB
	log      *js.DomainLog
	duty     *search.Embedder
	applier  search.Applier
	embedder *scriptedEmbedder
	consumed uint64
	version  int64
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
	h.duty, err = search.NewEmbedder(search.EmbedDeps{
		Publisher: publisher, Embedder: h.embedder, Model: embedModel,
		Corpora: []search.Corpus{search.TaskCorpus{DB: db}},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("build the duty: %v", err)
	}
	return h
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
}

func (s *scriptedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	fail, poison := s.failFirst, s.poisonID
	s.mu.Unlock()
	if fail && first {
		return nil, fmt.Errorf("the provider refused this batch")
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
