package engine

import (
	"context"
	"maps"
	"path/filepath"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
)

// A FAILED BOOT MUST LEAVE NOTHING RUNNING: [Engine.startNative]'s unwind rule
// one frame up, in [New]. An engine discarded with its runtime up can never be
// stopped, and its loops run on against a store the failure path closed.
// Observed the instant [New] returns, never by a wait, because both halves of
// the unwind are synchronous:
//
//   - every answerer the native start registered is WITHDRAWN, the first step
//     of [native.shutdown], so the native half was stopped, not abandoned;
//   - this node's ADMISSION is gone, so the unwind was the whole teardown: an
//     admission left behind tells a coordinator this process may be publishing.

// bootCleanupCompany fails at the FIRST step after the native start: its
// embeddings model is a `${VAR}` nothing sets, which validates (width and
// limits are stated, so no model-table lookup) and resolves to "" when `equip`
// builds the provider. A config failure rather than a broken broker verb, so
// the runtime is fully up and healthy when the boot dies.
const bootCleanupCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
  embeddings:
    type: openai
    model: "${CREWLET_TEST_EMBEDDING_MODEL_NOBODY_SETS}"
    dimensions: 768
    max_input_tokens: 512
    max_batch_inputs: 32
    max_batch_tokens: 16384
    api_key: "${K}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// servedQueue is the real broker, counting per subject the answerer
// registrations it hands out and the withdrawals that follow.
//
// THE REGISTRATION IS THE OBSERVATION, better than a goroutine count: it is a
// claim a peer relies on, and a search answerer left by a failed boot has a
// peer's search silently return a sixty-fourth of the corpus as complete. PER
// SUBJECT, because a node registers several (search, fleet history via
// internal/eventfan, the steer desk, the estate server), and one total would
// let one leak while another was withdrawn twice.
type servedQueue struct {
	*jetstream.Queue

	mu        sync.Mutex
	served    map[string]int
	withdrawn map[string]int
}

// Serve registers for real and wraps the withdrawal so it is counted.
func (q *servedQueue) Serve(ctx context.Context, subject string, answers int,
	fn queue.AnswerFunc) (queue.Unsubscribe, error) {

	stop, err := q.Queue.Serve(ctx, subject, answers, fn)
	if err != nil {
		return nil, err
	}
	q.count(q.served, subject)
	return func(ctx context.Context) error {
		q.count(q.withdrawn, subject)
		return stop(ctx)
	}, nil
}

// count adds one to a subject's tally.
func (q *servedQueue) count(tally map[string]int, subject string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	tally[subject]++
}

// tallies copies both tallies.
func (q *servedQueue) tallies() (served, withdrawn map[string]int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return maps.Clone(q.served), maps.Clone(q.withdrawn)
}

func TestAFailedBootStopsEverythingItAlreadyStarted(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(bootCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	real, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend this case needs",
			back.Queue)
	}
	watched := &servedQueue{Queue: real, served: map[string]int{},
		withdrawn: map[string]int{}}
	// BORROWED backends: the failure path leaves them open for the
	// assertions, as an embedded caller's are, where leaked loops outlive
	// the failed boot by the life of the process.
	borrowed := *back
	borrowed.Queue = watched

	if _, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, Backends: &borrowed,
	}); err == nil {
		t.Fatal("a company whose embedding model resolves to nothing built an " +
			"engine anyway — this case needs a boot that fails AFTER the " +
			"native start, so move it to whatever step now does")
	}

	served, withdrawn := watched.tallies()
	if got := served[search.SliceSubject]; got != 1 {
		t.Fatalf("this node registered %d search answerers, want 1 — the boot "+
			"failed before the native half was up, so this case is no "+
			"longer testing what it says", got)
	}
	// AND THE ESTATE SERVER, registered before the native start: a failed
	// data node must not stay on the stateless nodes' list of who to ask.
	// Asserted registered, so the withdrawal loop below judges it.
	nodeID, err := config.ResolveNodeID(&b, nil)
	if err != nil {
		t.Fatalf("resolve the node id: %v", err)
	}
	// BOTH OF ITS LANES: a node left answering proposals for repositories
	// it commits for would decide pushes from a runtime being torn down.
	for _, subject := range []string{estate.Subject(nodeID), estate.ProposalSubject(nodeID)} {
		if got := served[subject]; got != 1 {
			t.Fatalf("this node registered the estate server's %s %d time(s), "+
				"want 1 — a data node registers both of its lanes before the "+
				"native start, so this case expects exactly one each", subject, got)
		}
	}
	// THE HISTORY ANSWERER TOO, armed before the native start on every
	// node: asserted registered, so its withdrawal is judged.
	if got := served[topics.ObserveRead]; got != 1 {
		t.Fatalf("this node registered %d history answerers, want 1 — the "+
			"history answerer is armed before the native start, so this case "+
			"expects exactly one", got)
	}
	for subject, n := range served {
		if withdrawn[subject] != n {
			t.Errorf("the boot failed and its %s answerer was registered %d "+
				"time(s) and withdrawn %d: this node is still answering its "+
				"peers for a runtime — the apply loops, the position heartbeat, "+
				"the donor, the indexer — still running against a store the "+
				"caller is free to close", subject, n, withdrawn[subject])
		}
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

// A FAILED BOOT WITHDRAWS GITLET'S STATUS ANSWERER TOO: it registers beside
// Gitlet inside the native start (codestatus.go), so a boot dying after it
// must not leave the node answering the fleet's code status for a host its
// unwind stopped. The same company hosting code, on an every-role node.
//
// Mutation: withdraw nothing in native.stopGitletHost, and the answerer is left
// registered.
func TestAFailedBootWithdrawsItsGitletStatus(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	brokertest.GitletCacheIn(&b.Gitlet)
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(bootCleanupCompany + "code:\n  backend: gitlet\n"))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	RequireNodeGit(t, &b, cfg)
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	real, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend this case needs", back.Queue)
	}
	watched := &servedQueue{Queue: real, served: map[string]int{}, withdrawn: map[string]int{}}
	borrowed := *back
	borrowed.Queue = watched

	if _, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: &borrowed}); err == nil {
		t.Fatal("a company whose embedding model resolves to nothing built an engine anyway — this case " +
			"needs a boot that fails AFTER the native start built Gitlet")
	}
	served, withdrawn := watched.tallies()
	if served[topics.CodeStatus] != 1 || withdrawn[topics.CodeStatus] != 1 {
		t.Fatalf("the boot failed after building Gitlet, and its status answerer was registered %d "+
			"time(s) and withdrawn %d, want once each", served[topics.CodeStatus], withdrawn[topics.CodeStatus])
	}
}

// THE EARLIEST FAILURE REACHES THE TEARDOWN WITH NOTHING STARTED AT ALL: a
// keyring that will not open is refused before the admission handshake, the
// native start and [node.New], so every field the teardown reaches for is
// zero, and a dereference would bury the configuration error under a
// nil-pointer panic. The zero engine is that state, checking [Engine.teardown]'s
// claim to be nil-safe and never-started-safe throughout.
func TestTheTeardownIsSafeOnAnEngineThatStartedNothing(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.teardown(t.Context())
}
