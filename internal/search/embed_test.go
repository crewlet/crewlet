package search_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/textcut"
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
		hits, _, err = search.Semantic(t.Context(), tx, search.SemanticQuery{
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

// THE OPENING FITS THE MODEL. The corpus embeds a source's first 8 KiB, and
// the provider refuses an input past the model's own bound before sending
// anything — so against a model whose bound is smaller, an 8 KiB opening is not
// a longer opening but a batch refused on every tick. The duty sends the
// opening that fits.
func TestTheCorpusOpeningFitsTheModelsBound(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.SetLimits(embeddings.Limits{InputBytes: 64, BatchInputs: 128, BatchBytes: 1 << 20})
	h.seedTasks(map[string]string{
		"t-long": strings.Repeat("a runbook step that keeps on going ", 40),
	})
	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if published != 1 {
		t.Fatalf("the duty published %d record(s) for one long task under a "+
			"64-byte model", published)
	}
	for _, batch := range h.embedder.batches {
		for _, text := range batch {
			if len(text) > 64 {
				t.Errorf("the duty sent %d bytes to a model that takes 64", len(text))
			}
		}
	}
}

// THE DUTY SENDS THE PREPARED OPENING, AND ITS DIGEST IS OF WHAT IT SENT.
//
// The provider collapses every run of whitespace before it sends anything, so
// an opening cut BEFORE that spends the bound on indentation the model never
// sees — a body indented the way runbooks and code are embedded a fraction of
// what the bound allows — and a digest over that cut is a digest of bytes
// nobody embedded, so it can never be compared with what was. Prepared first
// and cut second, the opening is the bound's worth of text, the title then ONE
// space then the body, and the digest every replicated record carries is
// sha256 of exactly the bytes the provider received.
func TestTheDutySendsThePreparedOpeningAndDigestsWhatItSent(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	const bound = 96
	h.embedder.SetLimits(embeddings.Limits{InputBytes: bound, BatchInputs: 128, BatchBytes: 1 << 20})
	body := "\n\n    step one:      restart the worker\n" +
		strings.Repeat("            and then drain the queue\n", 12)
	h.seedTitled("t-run", "Runbook", body)

	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	requests := h.embedder.Requests()
	if len(requests) != 1 || len(requests[0]) != 1 {
		t.Fatalf("one task went to the provider as %v", requests)
	}
	sent := requests[0][0]
	if want := embeddings.Opening("Runbook "+body, bound); sent != want {
		t.Fatalf("the provider received %q, want the prepared opening %q", sent, want)
	}
	if !strings.HasPrefix(sent, "Runbook step one: restart the worker and then") {
		t.Errorf("the provider received %q — the title, one space, then the body "+
			"is what every stored vector was computed from", sent)
	}
	// THE FIXTURE HAS TO TELL THE TWO ORDERS APART: cut first, the same text
	// prepares to markedly less than the bound allows.
	cutFirst := embeddings.Prepare(textcut.Bytes("Runbook\n\n"+strings.TrimSpace(body), bound))
	if len(cutFirst) >= len(sent) {
		t.Fatalf("setup: a cut taken before the preparation keeps %d bytes and "+
			"the prepared opening %d, so this case proves nothing", len(cutFirst), len(sent))
	}

	h.drain()
	sum := sha256.Sum256([]byte(sent))
	if got := h.storedSHA(search.SourceTask, "t-run"); got != hex.EncodeToString(sum[:]) {
		t.Errorf("the stored digest is %q, not sha256 of the %d bytes the provider "+
			"received (%x)", got, len(sent), sum)
	}
}

// A SELECTION READS THE OPENING OF A BODY, NEVER THE WHOLE OF IT.
//
// A page holds up to 512 KiB and a selection is asked for a whole tick's worth
// of sources, so read whole it held up to half a gibibyte to send 8 KiB of
// each. It reads [search.EmbedReadChars] CHARACTERS — `substr` counts
// characters on text, which is what lets the bound be stated against bytes
// after the whitespace is collapsed — and a body of two-byte characters is the
// case that tells a character count from a byte count.
func TestASelectionReadsOnlyTheOpeningOfABody(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	long := strings.Repeat("é", 3*search.EmbedReadChars)
	h.seedTitled("t-long", "Long", long)
	writePage(t, h.db, pageRow{id: "p-long", container: "eng", title: "Long",
		body: long, status: "published", edit: 1, version: 1})

	for _, corpus := range []search.Corpus{
		search.TaskCorpus{DB: h.db.Replicated().Reader()},
		search.PageCorpus{DB: h.db.Replicated().Reader()},
	} {
		stale, _, err := corpus.Stale(t.Context(), embedModel, 64, 10)
		if err != nil {
			t.Fatalf("%s: Stale: %v", corpus.Source(), err)
		}
		if len(stale) != 1 {
			t.Fatalf("%s: %d stale document(s), want the one long one", corpus.Source(), len(stale))
		}
		body := stale[0].Body
		if !utf8.ValidString(body) || utf8.RuneCountInString(body) != search.EmbedReadChars {
			t.Errorf("%s: the selection read %d bytes holding %d characters (valid "+
				"UTF-8: %v), want exactly the first %d characters", corpus.Source(),
				len(body), utf8.RuneCountInString(body), utf8.ValidString(body),
				search.EmbedReadChars)
		}
	}
}

// A SOURCE WHOSE TEXT DID NOT CHANGE IS RESTAMPED, NOT SENT AGAIN.
//
// Every task record bumps the task's version — a status, an assignee, a move to
// another project — and each one used to buy a provider call and a new vector
// for the very text the stored vector was computed from, while the digest
// every record carries was documented as what stopped exactly that. Now the
// stored vector is republished under the new version and the new container: no
// request, the same bytes, and the container a scoped search filters on moved
// with the task. And the control: a changed text IS sent.
func TestAnUnchangedTextIsRestampedNotSentAgain(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTitled("t-a", "Rate limits", "429 backoff in the GitLab client")
	if published, err := h.duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the first tick published %d (%v)", published, err)
	}
	h.drain()
	sent := len(h.embedder.sent())
	first := h.storedRow(search.SourceTask, "t-a")

	h.moveTask("t-a", "OPS")
	published, err := h.duty.Tick(t.Context())
	if err != nil || published != 1 {
		t.Fatalf("the tick after a move published %d (%v), want the restamp", published, err)
	}
	if again := len(h.embedder.sent()); again != sent {
		t.Fatalf("a move that left the text alone sent %d request(s) — the stored "+
			"vector is that text's vector", again-sent)
	}
	h.drain()
	moved := h.storedRow(search.SourceTask, "t-a")
	if moved.rev != h.version || moved.container != "OPS" || moved.binContainer != "OPS" {
		t.Fatalf("after the restamp the rows say version %d in %q (sign code in %q), "+
			"want version %d in OPS on both", moved.rev, moved.container,
			moved.binContainer, h.version)
	}
	if !bytes.Equal(moved.embedding, first.embedding) || moved.sha != first.sha {
		t.Fatal("the restamp changed the vector or its digest; it must republish " +
			"the vector the text already has")
	}
	if current, total, err := (search.TaskCorpus{DB: h.db.Replicated().Reader()}).Coverage(
		t.Context(), embedModel, h.embedder.Width()); err != nil || current != total || total != 1 {
		t.Fatalf("coverage after the restamp is %d of %d (%v)", current, total, err)
	}

	// THE CONTROL: a new body is a new text, and is sent.
	h.seedTitled("t-a", "Rate limits", "a body somebody rewrote")
	if published, err := h.duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the tick after an edit published %d (%v)", published, err)
	}
	if again := len(h.embedder.sent()); again != sent+1 {
		t.Fatalf("an edited text went in %d request(s), want one", again-sent)
	}
}

// A RESTAMP NEVER CROSSES AN EMBEDDING SPACE.
//
// A digest says which text a vector was computed from and nothing about WHICH
// MODEL computed it: after a model change at the same width, the text's digest
// is unchanged and the stored vector is the old model's — a point in a space
// no query of the new model ranks against. Restamped, the refill would never
// happen. The stored digest is read only where the stored row is in the space
// being embedded.
func TestARestampNeverCrossesAnEmbeddingSpace(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTitled("t-a", "Rate limits", "429 backoff in the GitLab client")
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()
	sent := len(h.embedder.sent())

	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: h.standing(map[string]int{"node-a": search.RecordVersion}),
		Embedder: h.embedder, Model: "another-model-at-the-same-width",
		Corpora: []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Budget:  unbounded{}, Refusals: search.NewRefusals(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the tick under a new model published %d (%v)", published, err)
	}
	if again := len(h.embedder.sent()); again != sent+1 {
		t.Fatalf("a source under a new model went in %d request(s), want one — "+
			"its stored vector is the old model's", again-sent)
	}
}

// A RESTAMP PUBLISHES ONLY A VECTOR WHOSE DIGEST STILL MATCHES.
//
// The selection names the digest; the vector is read again, with its digest, in
// a statement of its own — and published only where that digest is still the
// one the text would be sent under. A corpus that claims a match the store no
// longer holds (a newer vector landed between the two reads) restamps nothing,
// rather than publish a record asserting a text for a vector computed from
// another.
func TestARestampPublishesOnlyAVectorWhoseDigestStillMatches(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.seedTitled("t-a", "Rate limits", "the first body")
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.drain()
	h.seedTitled("t-a", "Rate limits", "a body somebody rewrote")
	sent := len(h.embedder.sent())

	claims := h.dutyOver(claimedMatch{TaskCorpus: search.TaskCorpus{DB: h.db.Replicated().Reader()}})
	published, err := claims.Tick(t.Context())
	if err != nil || published != 0 {
		t.Fatalf("a restamp the store no longer matches published %d (%v)", published, err)
	}
	if again := len(h.embedder.sent()); again != sent {
		t.Fatalf("a restamp sent %d request(s)", again-sent)
	}
}

// claimedMatch is the task corpus claiming every source's stored vector is its
// text's — the selection a racing write leaves behind.
type claimedMatch struct{ search.TaskCorpus }

func (c claimedMatch) Stale(ctx context.Context, model string, dim, limit int) ([]search.Document, []string, error) {
	docs, gone, err := c.TaskCorpus.Stale(ctx, model, dim, limit)
	for i, doc := range docs {
		sum := sha256.Sum256([]byte(embeddings.Opening(doc.Title+" "+doc.Body, search.EmbedInputBytes)))
		docs[i].StoredSHA = hex.EncodeToString(sum[:])
	}
	return docs, gone, err
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

// EVERY VECTOR WITHDRAWN SHOWS THE TICK'S BOUND ITS PROGRESS.
//
// A withdrawal costs no provider call, so it is outside the batches a tick
// reports as it embeds — and there can be a selection's worth of them a
// corpus, 1 024 after a bulk purge, each a publish. Unreported, that was the
// longest stretch a live tick went without showing progress: on a slow broker
// a tick withdrawing steadily was cut off as wedged, which is the
// slow-but-advancing tick the bound exists NOT to cut off. So each one
// reports as it is published.
func TestEveryWithdrawalShowsTheTicksBoundItsProgress(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	const removed = 40
	bodies := make(map[string]string, removed)
	for i := range removed {
		bodies[fmt.Sprintf("t-%02d", i)] = fmt.Sprintf("something to forget, number %d", i)
	}
	h.seedTasks(bodies)
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("setup: tick: %v", err)
	}
	h.drain()
	if got := h.vectors(); got != removed {
		t.Fatalf("setup: %d vector(s) after the first tick, want %d", got, removed)
	}
	for id := range bodies {
		h.removeTask(id)
	}

	budget := &countedBudget{}
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(),
		Log:      search.Domain{}.Stream().Name,
		Standing: h.standing(map[string]int{"node-a": search.RecordVersion}),
		Embedder: h.embedder, Model: embedModel,
		Corpora:  []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Budget:   budget,
		Refusals: search.NewRefusals(),
	})
	if err != nil {
		t.Fatalf("build the duty: %v", err)
	}
	published, err := duty.Tick(t.Context())
	if err != nil || published != removed {
		t.Fatalf("setup: the tick published %d record(s), want the %d withdrawals: %v",
			published, removed, err)
	}
	if got := budget.advanced.Load(); got < removed {
		t.Fatalf("a tick that withdrew %d vectors and embedded nothing reported progress "+
			"%d time(s), want once a withdrawal", removed, got)
	}
}

// EVERY VECTOR PUBLISHED SHOWS THE TICK'S BOUND ITS PROGRESS, and so does every
// provider call answered — apart, never as one stretch.
//
// A batch is up to 128 publishes after its call, and counted as one stretch
// with the call they were the longest a live tick went without showing
// progress: a broker slow enough to take the bound's length over them cut off
// a tick that was publishing steadily, which the engine's own test of the
// bound met on a loaded machine as 108 published of 300 and the tick reported
// wedged.
func TestEveryEmbeddedVectorShowsTheTicksBoundItsProgress(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	const extra = 5 // a second, short batch
	seed := map[string]string{}
	for i := range search.EmbedBatch + extra {
		seed[fmt.Sprintf("t-%04d", i)] = fmt.Sprintf("document number %d", i)
	}
	h.seedTasks(seed)

	budget := &countedBudget{}
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(),
		Log:      search.Domain{}.Stream().Name,
		Standing: h.standing(map[string]int{"node-a": search.RecordVersion}),
		Embedder: h.embedder, Model: embedModel,
		Corpora:  []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Budget:   budget,
		Refusals: search.NewRefusals(),
	})
	if err != nil {
		t.Fatalf("build the duty: %v", err)
	}
	published, err := duty.Tick(t.Context())
	if err != nil || published != search.EmbedBatch+extra {
		t.Fatalf("the tick published %d record(s) (%v), want %d", published, err,
			search.EmbedBatch+extra)
	}
	const calls = 2
	if got := budget.advanced.Load(); got < int64(published+calls) {
		t.Fatalf("a tick that made %d provider calls and published %d vectors "+
			"reported progress %d time(s), want once a call and once a publish",
			calls, published, got)
	}
}

// countedBudget counts the reports of progress a tick makes, and grants every
// exemption.
type countedBudget struct{ advanced atomic.Int64 }

func (b *countedBudget) Advanced()      { b.advanced.Add(1) }
func (b *countedBudget) Exempt() func() { return func() {} }

// EVERY REQUEST IS ONE THE MODEL ACCEPTS FOR ITS SIZE.
//
// The duty forms its own requests through the provider's packing rule, so a
// request past the model's input count or its request total is never formed —
// sent, it would be refused on every tick, and before the duty planned its own
// requests a tick of long sources did exactly that against OpenAI's 300 000
// tokens. The twin enforces the same limits by the same code, and counts every
// request it was sent.
func TestEveryRequestTheDutyFormsFitsTheModel(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	limits := embeddings.Limits{InputBytes: 512, BatchInputs: 6, BatchBytes: 2_000, InputOverhead: 3}
	h.embedder.SetLimits(limits)
	seed := map[string]string{}
	for i := range 40 {
		seed[fmt.Sprintf("t-%02d", i)] = strings.Repeat(fmt.Sprintf("long body %d ", i), 30)
	}
	h.seedTasks(seed)

	published, err := h.duty.Tick(t.Context())
	if err != nil || published != 40 {
		t.Fatalf("the tick published %d of 40 (%v)", published, err)
	}
	requests := h.embedder.Requests()
	for i, request := range requests {
		used := 0
		for _, input := range request {
			used += len(input) + limits.InputOverhead
		}
		if len(request) > limits.BatchInputs || used > limits.BatchBytes {
			t.Errorf("request %d carried %d inputs and %d bytes, past the model's "+
				"%d inputs and %d bytes", i, len(request), used, limits.BatchInputs,
				limits.BatchBytes)
		}
	}
	if len(requests) < 2 {
		t.Fatalf("setup: 40 sources went in %d request(s), so the limits never bound", len(requests))
	}
}

// A PERMANENTLY REFUSED INPUT CANNOT HOLD ITS NEIGHBOURS BACK.
//
// A refusal says the REQUEST is unacceptable and not which part of it, and the
// selection is oldest first, so before the duty split a refused request every
// one of its neighbours was refused with it on every tick for ever. Now the
// request is split until the input refused is alone: everything else is
// published in the tick the refusal is met in, at no more than 1 + 2·log₂n
// requests, and the input refused alone costs only itself — and on the ticks
// after it, nothing at all until its retry is due, when it is offered ALONE.
func TestAPermanentlyRefusedInputCannotHoldItsNeighboursBack(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.refuse("poison")
	seed := map[string]string{}
	for i := range search.EmbedBatch {
		seed[fmt.Sprintf("t-%04d", i)] = fmt.Sprintf("document number %d", i)
	}
	// THE OLDEST, so it is in the first request every tick forms.
	seed["t-0000"] = "a poison pill the provider never accepts"
	h.seedTasks(seed)

	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if published != search.EmbedBatch-1 {
		t.Fatalf("the tick published %d of the %d sources the provider accepts",
			published, search.EmbedBatch-1)
	}
	sent := len(h.embedder.sent())
	if bound := 1 + 2*7; sent > bound {
		t.Fatalf("isolating one refused input among %d took %d requests, past "+
			"1 + 2·log₂n = %d", search.EmbedBatch, sent, bound)
	}
	h.drain()
	if got := h.vectors(); got != search.EmbedBatch-1 {
		t.Fatalf("%d vector(s) stored, want every neighbour of the refused input", got)
	}

	// HELD BACK: the next tick sends nothing for it.
	if published, err = h.duty.Tick(t.Context()); err != nil || published != 0 {
		t.Fatalf("the tick after the refusal published %d (%v)", published, err)
	}
	if again := len(h.embedder.sent()); again != sent {
		t.Fatalf("the tick after the refusal sent %d request(s) for an input "+
			"refused alone — it is held back until its retry is due", again-sent)
	}

	// AND ONCE DUE, OFFERED ALONE: one request carrying it and nothing else,
	// although a neighbour written since stands right behind it — sent with
	// it, the neighbour would be refused with it and split out again.
	h.advance(search.EmbedRefusalRetry)
	h.seedTasks(map[string]string{"t-new": "a document written since"})
	if published, err = h.duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the retry tick published %d (%v), want the new neighbour", published, err)
	}
	requests := h.embedder.sent()[sent:]
	if len(requests) != 2 || len(requests[0]) != 1 || !strings.Contains(requests[0][0], "poison") ||
		len(requests[1]) != 1 || !strings.Contains(requests[1][0], "written since") {
		t.Fatalf("the retry tick sent %q, want the refused input alone and then "+
			"its neighbour", requests)
	}
}

// AN INPUT REFUSED ALONE IS EMBEDDED ONCE THE PROVIDER TAKES IT.
//
// A refusal is a fact about a provider and a text, not a verdict on the source:
// a gateway fixed or a content rule relaxed has it embedded at the next retry,
// and the memory of the refusal goes. A source whose TEXT changed is a
// different input, offered at once with its neighbours — and the memory of
// what it used to say, offered nowhere since, is forgotten once twice the
// retry has passed, or the memory would grow for the life of the process.
func TestARefusedInputIsEmbeddedOnceItIsAccepted(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.refuse("poison")
	h.seedTasks(map[string]string{"t-a": "a poison pill", "t-b": "a poison pill too"})
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := h.refusals.Len(); got != 2 {
		t.Fatalf("%d refusal(s) remembered for two inputs refused alone", got)
	}

	// t-b IS REWRITTEN: a new text, a new input, offered at once.
	h.embedder.accept("poison")
	h.seedTasks(map[string]string{"t-b": "an ordinary body now"})
	published, err := h.duty.Tick(t.Context())
	if err != nil || published != 1 {
		t.Fatalf("the tick after t-b was rewritten published %d (%v), want t-b", published, err)
	}
	h.drain()

	// t-a IS ACCEPTED AT ITS RETRY, and forgotten.
	h.advance(search.EmbedRefusalRetry)
	if published, err = h.duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the retry tick published %d (%v), want t-a", published, err)
	}
	h.drain()
	if got := h.vectors(); got != 2 {
		t.Fatalf("%d vector(s) stored, want both", got)
	}
	if got := h.refusals.Len(); got != 1 {
		t.Fatalf("%d refusal(s) remembered, want only t-b's old text — t-a's went "+
			"when it was embedded", got)
	}

	// AND t-b's OLD TEXT GOES once nothing has offered it for twice the retry.
	h.advance(search.EmbedRefusalRetry)
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := h.refusals.Len(); got != 0 {
		t.Fatalf("%d refusal(s) remembered for texts no selection returns any more", got)
	}
}

// A FAILURE THAT IS NOT ABOUT AN INPUT ENDS THE TICK'S REQUESTS, and the next
// tick embeds what it left.
//
// A rate limit, a timeout or a server down is a fact about the provider, and
// the next request would meet it too: sending the rest of the tick's thirty-two
// is thirty-one more answers of the same kind, each up to a minute. Nothing is
// lost by stopping — the selection is derived from the rows, so whatever the
// tick did not embed is selected again.
func TestATransientFailureEndsTheTicksRequests(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	seed := map[string]string{}
	for i := range search.EmbedBatch + 5 {
		seed[fmt.Sprintf("t-%04d", i)] = fmt.Sprintf("document number %d", i)
	}
	seed["t-0000"] = "the provider is busy with this one"
	h.seedTasks(seed)
	h.embedder.FailTransiently("busy", 1)

	published, err := h.duty.Tick(t.Context())
	if err != nil {
		t.Fatalf("the tick returned an error rather than carrying: %v", err)
	}
	if published != 0 || len(h.embedder.Requests()) != 1 {
		t.Fatalf("after a transient failure the tick published %d in %d request(s), "+
			"want it to stop at the first", published, len(h.embedder.Requests()))
	}

	again, err := h.duty.Tick(t.Context())
	if err != nil || again != search.EmbedBatch+5 {
		t.Fatalf("the next tick published %d of %d (%v)", again, search.EmbedBatch+5, err)
	}
}

// A TICK SAYS THE CONFIGURATION IS REFUSED ONLY WHEN IT IS: inputs refused
// alone, nothing accepted, and no other failure. Then each input refused alone
// is named and one line says what they have in common. A tick that met one
// refusal and was then stopped by a rate limit sent requests nobody refused,
// and blaming the configuration for it sends an operator to fix a setting
// that is fine.
func TestATickSaysTheConfigurationIsRefusedOnlyWhenItIs(t *testing.T) {
	t.Parallel()
	seed := map[string]string{}
	for i := range 8 {
		seed[fmt.Sprintf("t-%04d", i)] = fmt.Sprintf("document number %d", i)
	}

	h := newEmbedHarness(t)
	h.seedTasks(seed)
	h.embedder.refuse("") // every input, as a refused configuration is
	if _, err := h.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := h.logs.count("search_embed_every_request_refused"); got != 1 {
		t.Fatalf("a tick the provider refused whole said so %d time(s), want once", got)
	}
	if h.logs.count("search_embed_input_refused") == 0 {
		t.Fatal("no input refused alone was named")
	}

	stopped := newEmbedHarness(t)
	seed["t-0000"] = "a poison pill the provider never accepts"
	stopped.seedTasks(seed)
	stopped.embedder.refuse("poison")
	stopped.embedder.FailTransiently("document", 1)
	if _, err := stopped.duty.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if stopped.logs.count("search_embed_request_failed") == 0 {
		t.Fatal("the case did not reach the transient failure it is about")
	}
	if got := stopped.logs.count("search_embed_every_request_refused"); got != 0 {
		t.Fatalf("a tick stopped by a transient failure blamed the configuration "+
			"%d time(s)", got)
	}
}

// A CORPUS THAT IS ALWAYS BEHIND DOES NOT STARVE THE ONE AFTER IT.
//
// The tick's provider requests are one budget shared by every corpus, and
// spent corpus by corpus in order they all went to the first one that could
// take them: a company writing tasks faster than the duty embeds them — or
// simply one still cold-filling a hundred thousand — never reached its pages
// at all. Not one page embedded, not one trashed page's vector withdrawn, for
// as long as the backlog refilled, while the coverage gauge summed both corpora
// and reported a company that was merely behind.
//
// A model that takes sixteen inputs a request is what makes the REQUEST
// ceiling bind here: each corpus has sixty-four requests of backlog, and the
// tick has thirty-two.
func TestABackloggedCorpusDoesNotStarveTheOneAfterIt(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	h.embedder.blank = true
	h.embedder.SetLimits(embeddings.Limits{InputBytes: 8192, BatchInputs: 16, BatchBytes: 300_000})
	tasks := &scriptedCorpus{source: search.SourceTask, backlog: -1}
	// THE SECOND CORPUS IS BEHIND TOO, and also carries a vector to
	// withdraw: under the old scheduling its selection never ran at all,
	// so the page somebody deleted stayed findable by meaning for ever.
	pages := &scriptedCorpus{source: search.SourcePage, backlog: -1,
		gone: []string{"p-deleted"}}

	published, err := h.dutyOver(tasks, pages).Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	calls := h.embedder.callsPerSource()
	want := search.EmbedRequestsPerTick / 2
	if calls[search.SourcePage] != want || calls[search.SourceTask] != want {
		t.Fatalf("the tick spent %d request(s) on tasks and %d on pages; two "+
			"corpora that are both behind divide the tick's %d evenly, and "+
			"a page corpus at zero is a company whose wiki is unsearchable "+
			"by meaning for as long as the task backlog refills",
			calls[search.SourceTask], calls[search.SourcePage],
			search.EmbedRequestsPerTick)
	}
	if total := calls[search.SourceTask] + calls[search.SourcePage]; total != search.EmbedRequestsPerTick {
		t.Fatalf("the tick made %d provider requests against a ceiling of %d — "+
			"the ceiling is what bounds the company's embedding bill, and "+
			"sharing it fairly may not raise it", total, search.EmbedRequestsPerTick)
	}
	if published != 1 {
		t.Fatalf("the tick published %d record(s); the provider embedded "+
			"nothing, so the one record is the second corpus's withdrawal — "+
			"and a corpus that is never selected withdraws nothing", published)
	}
}

// AND THE SOURCES A TICK MAY EMBED ARE DIVIDED THE SAME WAY.
//
// With requests of a full [search.EmbedBatch], the binding ceiling is the
// tick's sources rather than its requests — eight requests' worth — and the
// round robin divides those as evenly: four requests, 512 vectors, each.
func TestTheTicksSourcesAreDividedBetweenTheCorpora(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	tasks := &scriptedCorpus{source: search.SourceTask, backlog: -1}
	pages := &scriptedCorpus{source: search.SourcePage, backlog: -1}

	published, err := h.dutyOver(tasks, pages).Tick(t.Context())
	if err != nil || published != search.EmbedSourcesPerTick {
		t.Fatalf("the tick published %d (%v), want the %d a tick allows",
			published, err, search.EmbedSourcesPerTick)
	}
	calls := h.embedder.callsPerSource()
	if want := search.EmbedSourcesPerTick / search.EmbedBatch / 2; calls[search.SourceTask] != want ||
		calls[search.SourcePage] != want {
		t.Fatalf("the tick spent %d full request(s) on tasks and %d on pages, want %d each",
			calls[search.SourceTask], calls[search.SourcePage], want)
	}
}

// AND A CORPUS WITH NOTHING TO DO WASTES NOTHING.
//
// The share is a floor, not a quota: reserving half the requests for a
// caught-up wiki would halve the rate a cold fill of the tracker proceeds at,
// for a corpus with no work to put them to. What a corpus does not use is
// taken by whoever still has a backlog, which is what makes the round robin a
// reserved share and its redistribution in one rule.
func TestAnIdleCorpusGivesItsShareToTheRest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		pageBacklog int
		wantTask    int
		wantPage    int
	}{
		{name: "an empty wiki", pageBacklog: 0,
			wantTask: search.EmbedRequestsPerTick, wantPage: 0},
		{name: "one page behind", pageBacklog: 1,
			wantTask: search.EmbedRequestsPerTick - 1, wantPage: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newEmbedHarness(t)
			h.embedder.blank = true
			h.embedder.SetLimits(embeddings.Limits{InputBytes: 8192, BatchInputs: 16, BatchBytes: 300_000})
			tasks := &scriptedCorpus{source: search.SourceTask, backlog: -1}
			pages := &scriptedCorpus{source: search.SourcePage, backlog: tc.pageBacklog}

			if _, err := h.dutyOver(tasks, pages).Tick(t.Context()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			calls := h.embedder.callsPerSource()
			if calls[search.SourceTask] != tc.wantTask || calls[search.SourcePage] != tc.wantPage {
				t.Fatalf("the tick spent %d request(s) on tasks and %d on pages, "+
					"want %d and %d — a request the second corpus has no work "+
					"for belongs to the one that does, or the ceiling buys less "+
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
	h.embedder.SetLimits(embeddings.Limits{InputBytes: 8192, BatchInputs: 16, BatchBytes: 300_000})
	boom := errors.New("the anti-join timed out")
	tasks := &scriptedCorpus{source: search.SourceTask, err: boom}
	pages := &scriptedCorpus{source: search.SourcePage, backlog: -1}

	_, err := h.dutyOver(tasks, pages).Tick(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("a corpus whose selection failed answered %v — a tick that "+
			"carries the failure must still report it", err)
	}
	calls := h.embedder.callsPerSource()
	if calls[search.SourcePage] != search.EmbedRequestsPerTick {
		t.Fatalf("the tick spent %d request(s) on the second corpus after the "+
			"first one's selection failed, want the whole ceiling of %d",
			calls[search.SourcePage], search.EmbedRequestsPerTick)
	}
}

// A DUTY WITH NO BUDGET IS A REFUSED WIRING: its caller bounds every tick, and
// one that did not say which bound would measure a training by its length
// rather than its progress — which on a one-core node cut off every training
// at the largest corpus.
func TestTheDutyRefusesAWiringWithNoBudget(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	_, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: h.standing(nil), Embedder: h.embedder, Model: embedModel,
		Corpora:  []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Refusals: search.NewRefusals(),
	})
	if err == nil || !strings.Contains(err.Error(), "EmbedDeps.Budget") {
		t.Fatalf("a duty with no budget built as %v, want a refusal naming EmbedDeps.Budget", err)
	}
}

// A DUTY WITH NO REFUSAL MEMORY IS A REFUSED WIRING: the duty is rebuilt every
// tick, so a memory defaulted inside it would last one tick, and an input the
// model refuses would be isolated again at the front of every tick for ever.
func TestTheDutyRefusesAWiringWithNoRefusalMemory(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	_, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: h.standing(nil), Embedder: h.embedder, Model: embedModel,
		Corpora: []search.Corpus{search.TaskCorpus{DB: h.db.Replicated().Reader()}},
		Budget:  unbounded{},
	})
	if err == nil || !strings.Contains(err.Error(), "EmbedDeps.Refusals") {
		t.Fatalf("a duty with no refusal memory built as %v, want a refusal naming "+
			"EmbedDeps.Refusals", err)
	}
}

// MORE CORPORA THAN A TICK CAN SERVE IS A REFUSED WIRING.
//
// Below one full request apiece there is no share left to guarantee: every
// tick starts its round at the front, so the corpora past the ceiling would be
// embedded never rather than slowly. Refusing says so at the wiring, which is
// the one place the extra corpus can be taken back out.
func TestTheDutyRefusesMoreCorporaThanATickCanServe(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	corpora := make([]search.Corpus, search.EmbedSourcesPerTick/search.EmbedBatch+1)
	for i := range corpora {
		corpora[i] = &scriptedCorpus{source: search.SourceTask, backlog: -1}
	}
	_, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: h.standing(nil),
		Embedder: h.embedder, Model: embedModel, Corpora: corpora,
		// EVERY OTHER FIELD WIRED, so the refusal is the corpora's.
		Budget: unbounded{}, Refusals: search.NewRefusals(),
	})
	if err == nil {
		t.Fatalf("a duty with %d corpora and %d full requests a tick was accepted "+
			"— the ones past the ceiling are never embedded, and the coverage "+
			"gauge sums them into a company that merely looks behind",
			len(corpora), search.EmbedSourcesPerTick/search.EmbedBatch)
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

	// refusals is the memory every duty this harness builds shares, held
	// across ticks as the engine holds it.
	refusals *search.Refusals

	// now is the duty's clock, moved by [embedHarness.advance].
	now time.Time

	// logs is what every duty this harness builds logged.
	logs *lockedBuffer
}

// lockedBuffer is a log sink a duty may write while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// count is how many logged lines carry msg.
func (b *lockedBuffer) count(msg string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), "msg="+msg+" ")
}

// advance moves the duty's clock.
func (h *embedHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

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
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	h := &embedHarness{
		t: t, db: db, log: log, applier: search.NewApplier(),
		embedder: &scriptedEmbedder{Fake: embeddings.NewFake(64), poisonID: -1},
		refusals: search.NewRefusals(),
		now:      time.Unix(1_700_000_000, 0).UTC(),
		logs:     &lockedBuffer{},
	}
	rows, err := search.NewRows(db.Replicated().Reader(), search.Domain{}.Stream())
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: search.Domain{}, Spec: search.Domain{}.Stream(), Log: log, Records: log, Rows: rows,
		Fence: search.NewFence(), Gates: search.NewGates(), Waiter: &embedWaiter{}, Voids: embedWaiter{},
		NodeID: "node-a", Generation: func() uint32 { return 0 }, Identity: &embedWaiter{},
		ResolveBudget: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	h.publisher = publisher
	h.duty = h.dutyOver(search.TaskCorpus{DB: db.Replicated().Reader()})
	return h
}

// dutyOver builds a duty over the corpora a test dictates, on this harness's
// own publisher — so a scheduling case reaches the same broker and the same
// record path as the round trip above.
func (h *embedHarness) dutyOver(corpora ...search.Corpus) *search.Embedder {
	h.t.Helper()
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: h.publisher, Estate: h.db.Replicated().Reader(), Log: search.Domain{}.Stream().Name,
		Standing: h.standing(map[string]int{"node-a": search.RecordVersion}),
		Embedder: h.embedder, Model: embedModel, Corpora: corpora,
		Now:      func() time.Time { return h.now },
		Budget:   unbounded{},
		Refusals: h.refusals,
		Logger:   slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		h.t.Fatalf("build the duty: %v", err)
	}
	return duty
}

// standing is the log's standing as this harness's one node holds it: current
// when [embedHarness.drain] has applied every record the log holds, and the
// fleet the readers a test dictates.
func (h *embedHarness) standing(readers map[string]int) func(context.Context) (search.LogStanding, error) {
	return func(ctx context.Context) (search.LogStanding, error) {
		last, err := h.log.End(ctx)
		if err != nil {
			return search.LogStanding{}, err
		}
		return search.LogStanding{Current: h.consumed >= last, Readers: readers}, nil
	}
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
	// IN ID ORDER, because each task's version and update instant are
	// minted in this loop and the duty embeds in update order: seeded in a
	// map's order, which documents a tick embedded — and so which corpus an
	// index was trained on — changed from run to run.
	slices.Sort(ids)
	for _, id := range ids {
		// A DOCUMENT WITH NO TEXT AT ALL needs an empty title too: a task
		// titled after its own id is embeddable text, and a fixture that
		// meant to test the empty case would silently be testing the
		// ordinary one.
		title := id
		if bodies[id] == "" {
			title = ""
		}
		h.seedTitled(id, title, bodies[id])
	}
}

// seedTitled writes one tracker row with the title a test dictates, at the
// next version and update instant.
func (h *embedHarness) seedTitled(id, title, body string) {
	h.t.Helper()
	h.version++
	document, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		h.t.Fatalf("encode the body: %v", err)
	}
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(), `
			INSERT INTO tracker_tasks
				(id, key, project_key, root_id, type, title, status,
				 status_group, rank, version, created_at, updated_at,
				 document)
			VALUES (?, ?, 'ENG', ?, 'task', ?, 'todo', 'open', 'm0',
			        ?, 0, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				title = excluded.title, document = excluded.document,
				version = excluded.version, updated_at = excluded.updated_at`,
			id, strings.ToUpper(id), id, title, h.version, h.version, document)
		return err
	}); err != nil {
		h.t.Fatalf("seed: %v", err)
	}
}

// moveTask files a task in another project, at its next version, with its text
// left as it is.
func (h *embedHarness) moveTask(id, project string) {
	h.t.Helper()
	h.version++
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.t.Context(), `
			UPDATE tracker_tasks SET project_key = ?, version = ?, updated_at = ?
			WHERE id = ?`, project, h.version, h.version, id)
		return err
	}); err != nil {
		h.t.Fatalf("move %s: %v", id, err)
	}
}

// vectorRow is what the vector tables hold for one source.
type vectorRow struct {
	rev                     int64
	container, binContainer string
	sha                     string
	embedding               []byte
}

// storedRow reads both vector rows for one source.
func (h *embedHarness) storedRow(source search.Source, id string) vectorRow {
	h.t.Helper()
	var row vectorRow
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), `
			SELECT v.source_rev, v.container, b.container, v.text_sha, v.embedding
			FROM kb_vectors v
			JOIN kb_vectors_bin b ON b.source = v.source AND b.source_id = v.source_id
			WHERE v.source = ? AND v.source_id = ?`, string(source), id).Scan(
			&row.rev, &row.container, &row.binContainer, &row.sha, &row.embedding)
	}); err != nil {
		h.t.Fatalf("read the vector rows of %s %s: %v", source, id, err)
	}
	return row
}

// storedSHA is the text digest the vector row for one source carries.
func (h *embedHarness) storedSHA(source search.Source, id string) string {
	h.t.Helper()
	var sha string
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT text_sha FROM kb_vectors WHERE source = ? AND source_id = ?`,
			string(source), id).Scan(&sha)
	}); err != nil {
		h.t.Fatalf("read the digest of %s %s: %v", source, id, err)
	}
	return sha
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

// scriptedEmbedder is the fake with the faults a test can ask for.
type scriptedEmbedder struct {
	*embeddings.Fake
	mu sync.Mutex
	// poisonID is the index within a request whose vector is made
	// non-finite, or -1 for none.
	poisonID int
	// blank answers every input with no vector at all, which is a real
	// state — an empty source — and is what lets a scheduling test spend
	// the tick's provider requests without publishing a record for each of
	// the thousand documents they carry.
	blank bool
	// refused are markers a request carrying any input that contains one
	// is refused for, in the provider's own class ([embeddings.ErrRefused]),
	// as a provider refuses the same input every time it is sent — until a
	// test says it no longer does ([scriptedEmbedder.accept]).
	refused []string
	// batches is what each call was asked to embed, in order — refused
	// ones included — so a test can see how the tick's requests were
	// divided.
	batches [][]string
}

// refuse makes every request carrying an input that contains marker refused.
func (s *scriptedEmbedder) refuse(marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = append(s.refused, marker)
}

// accept stops refusing requests for marker.
func (s *scriptedEmbedder) accept(marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = slices.DeleteFunc(s.refused, func(m string) bool { return m == marker })
}

// sent is every call the embedder was asked to answer, in order.
func (s *scriptedEmbedder) sent() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches)
}

func (s *scriptedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	poison, blank := s.poisonID, s.blank
	s.batches = append(s.batches, append([]string(nil), texts...))
	refused := slices.Clone(s.refused)
	s.mu.Unlock()
	for _, marker := range refused {
		for _, text := range texts {
			if strings.Contains(text, marker) {
				return nil, &embeddings.Error{Model: embedModel, Status: 400,
					Class: embeddings.ErrRefused,
					Err:   fmt.Errorf("the script refuses inputs containing %q", marker)}
			}
		}
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

// callsPerSource is how many provider requests each source's documents were sent
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
	gone    []string
	err     error
}

func (c *scriptedCorpus) Source() search.Source { return c.source }

func (c *scriptedCorpus) Stale(_ context.Context, _ string, _, limit int) ([]search.Document, []string, error) {
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

// Voided voids nothing: no reanchor places a rule on this harness's log.
func (embedWaiter) Voided(uint32, uint64) (statelog.Reason, bool) { return "", false }

// StreamIdentity is always the live stream: this harness never rebuilds its
// log.
func (embedWaiter) StreamIdentity() error { return nil }
func (embedWaiter) Truncated() error      { return nil }
func (embedWaiter) WaitCommitted(context.Context, statelog.Position) error {
	return nil
}
func (embedWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return nil
}

// unbounded is a tick nothing bounds: every report of progress is taken and
// every exemption granted and none kept, for the tests whose ticks the
// engine's bound is not the subject of.
type unbounded struct{}

func (unbounded) Advanced()      {}
func (unbounded) Exempt() func() { return func() {} }
