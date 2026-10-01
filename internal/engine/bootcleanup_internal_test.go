package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A FAILED BOOT MUST LEAVE NOTHING RUNNING — THE SAME RULE ONE FRAME UP.
//
// [Engine.startNative] unwinds what it started, and for a while [New] did not
// unwind what IT started: every return below the native start closed the
// backends and nothing else. The engine holding the running runtime was then
// discarded, so nothing could ever be asked to stop it — the apply loops, the
// position heartbeat, the snapshot donor, the lexical indexer and a
// still-registered search-slice answerer all ran on, against a store the same
// failure path had just closed underneath them.
//
// Two observations, both taken the instant [New] returns and neither of them a
// wait, because both halves of the unwind are synchronous:
//
//   - the slice answerer is WITHDRAWN, which is the first step of
//     [native.shutdown] and therefore says the native runtime was stopped
//     rather than abandoned;
//   - this node's ADMISSION is gone, which says the unwind is the whole
//     teardown rather than the native part alone. An admission left behind
//     tells a coordinator this process may be publishing — and it is the
//     handshake's whole purpose to be believed.

// bootCleanupCompany fails at the FIRST step after the native start.
//
// Its embeddings model is a `${VAR}` nothing sets, which validates (the width
// is stated, so nothing has to be looked up in the model table) and then
// resolves to the empty string when `equip` builds the provider — the step
// immediately after [Engine.startNative] has succeeded. A config-driven
// failure rather than a broken broker verb, because the point of this case is
// that the runtime is fully up and healthy when the boot dies of something
// else entirely.
const bootCleanupCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
  embeddings:
    type: openai
    model: "${CREWLET_TEST_EMBEDDING_MODEL_NOBODY_SETS}"
    dimensions: 768
    api_key: "${K}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// servedQueue is the real broker, counting the answerer registrations it hands
// out and the withdrawals that follow.
//
// THE REGISTRATION IS THE OBSERVATION, and it is a better one than a goroutine
// count or a connection: startNative registers this node as an answerer for
// its peers' search fan-out, and a registration is a claim on buckets a
// coordinator counts as answered. One left behind by a node whose boot failed
// is not a leak somebody notices as memory — it is a peer's search silently
// returning a sixty-fourth of the corpus as a complete answer.
//
// PER SUBJECT, because a node registers more than one answerer — the search
// fan-out's and the fleet history's (internal/eventfan) — and each is a claim
// a peer relies on: a history answerer left behind by a failed boot answers
// peers from a store its caller is free to close.
type servedQueue struct {
	*jetstream.Queue

	mu        sync.Mutex
	served    map[string]int
	withdrawn map[string]int
}

// Serve registers for real and wraps the withdrawal so it can be counted.
func (q *servedQueue) Serve(ctx context.Context, subject string,
	fn queue.AnswerFunc) (queue.Unsubscribe, error) {

	stop, err := q.Queue.Serve(ctx, subject, fn)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	q.served[subject]++
	q.mu.Unlock()
	return func(ctx context.Context) error {
		q.mu.Lock()
		q.withdrawn[subject]++
		q.mu.Unlock()
		return stop(ctx)
	}, nil
}

// counts is how many answerers were registered and withdrawn on one subject.
func (q *servedQueue) counts(subject string) (served, withdrawn int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.served[subject], q.withdrawn[subject]
}

func TestAFailedBootStopsEverythingItAlreadyStarted(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(bootCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	real, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend this case needs",
			back.Queue)
	}
	watched := &servedQueue{Queue: real, served: map[string]int{}, withdrawn: map[string]int{}}
	// BORROWED backends, so the failure path leaves them open for the
	// assertions — and so this case sees what an embedded caller sees,
	// which is the deployment where nothing at all was closed and the
	// loops outlived the failed boot by the life of the process.
	borrowed := *back
	borrowed.Queue = watched

	if _, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, Backends: &borrowed,
	}); err == nil {
		t.Fatal("a company whose embedding model resolves to nothing built an " +
			"engine anyway — this case needs a boot that fails AFTER the " +
			"native start, so move it to whatever step now does")
	}

	served, withdrawn := watched.counts(topics.SearchSlice)
	if served != 1 {
		t.Fatalf("this node registered %d search answerers, want 1 — the boot "+
			"failed before the native runtime was up, so this case is no "+
			"longer testing what it says", served)
	}
	if withdrawn != 1 {
		t.Errorf("the boot failed and its search answerer was withdrawn %d "+
			"times, want 1: this node is still claiming bucket ranges its "+
			"peers count as answered, and the runtime behind them — the apply "+
			"loops, the position heartbeat, the donor, the indexer — is still "+
			"running against a store the caller is free to close", withdrawn)
	}
	served, withdrawn = watched.counts(topics.ObserveRead)
	if served != 1 {
		t.Fatalf("this node registered %d history answerers, want 1 — the "+
			"history answerer is armed before the native start, so this case "+
			"expects exactly one", served)
	}
	if withdrawn != 1 {
		t.Errorf("the boot failed and its history answerer was withdrawn %d "+
			"times, want 1: peers still ask this node for its turns and it "+
			"answers them from a store the caller is free to close", withdrawn)
	}

	admissions, err := back.Fleet.Admissions(context.Background())
	if err != nil {
		t.Fatalf("read the admissions: %v", err)
	}
	if len(admissions) != 0 {
		t.Errorf("the boot failed and %d admission(s) are still recorded, want "+
			"none: this node told the fleet it was about to publish and never "+
			"took it back, so a capacity operation has to treat a process "+
			"that is not running as a possible publisher", len(admissions))
	}
}

// THE EARLIEST FAILURE REACHES THE TEARDOWN WITH NOTHING STARTED AT ALL.
//
// A keyring that will not open is refused before the admission handshake,
// before the native start and before [node.New], so every field the teardown's
// order reaches for is still its zero value — and a teardown that dereferenced
// one would turn a boot failure with a clear message into a nil-pointer panic,
// which is strictly worse than the leak it replaced: a crash reports the
// cleanup's bug and loses the configuration error that caused it.
//
// The zero engine is that state, and asserting on it is how the claim in
// [Engine.teardown]'s doc — nil-safe and never-started-safe throughout — is
// checked rather than believed.
func TestTheTeardownIsSafeOnAnEngineThatStartedNothing(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.teardown(t.Context())
}
