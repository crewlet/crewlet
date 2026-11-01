package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE LIVE TURNS' HOLDS ARE READ FROM EVERY ROW AND EVERY SEAT HOLDER — the
// code-retention pass's [code.Retention.Holds], over a running node: a key a
// run row holds, whatever the row's status (a newer build's included), a key a
// running turn of a seat holder holds in its memory, and nothing else. And
// never an answer on an unknown: a seat holder that does not answer inside the
// budget is named in an error, never read as holding nothing.
//
// Mutations: read the run store through ListActive — the newer build's row's
// key is unheld, red; read the rows alone — the running turn's key is unheld,
// red; read a silent holder as holding nothing — no error, red.
func TestCodeHoldsReadsRowsAndEverySeatHolder(t *testing.T) {
	t.Parallel()
	boot := testBootstrap(t)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &boot)
	company, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &boot, Company: company})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.node.Attached()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.node.Attached()) == 0 {
		t.Fatal("the node attached no seat in 10s, so no seat holder would be asked")
	}

	// A PARKED RUN'S ROW, holding A and a seat's branch, and a row a newer
	// build wrote at a status this build does not know, holding C — each of a
	// seat this company does not place, so no recovery of this node's seats
	// reads them. A key is opaque to the reader: a seat branch's
	// (code.RefHoldKey) is read as a repository's is.
	refA := code.RefHoldKey("0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "refs/heads/seats/elsewhere/a")
	refB := code.RefHoldKey("0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "refs/heads/seats/elsewhere/b")
	for _, row := range []sandbox.PendingRun{
		{TurnID: "run-a", AgentHandle: "elsewhere", Status: sandbox.StatusAwaiting,
			CodeHolds: []string{"repo-a", refA}},
		{TurnID: "run-c", AgentHandle: "elsewhere", Status: "rehearsing", CodeHolds: []string{"repo-c"}},
	} {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if created, err := e.backends.Fleet.CreateSandboxRun(t.Context(), row.TurnID, raw); err != nil || !created {
			t.Fatalf("seed %s's row: %v %v", row.TurnID, created, err)
		}
	}
	// A RUNNING TURN OF THIS SEAT HOLDER, holding B and a seat's branch in its
	// memory.
	running := turnctx.NewHolds(nil)
	for _, key := range []string{"repo-b", refB} {
		if err := running.Take(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	done := e.turnHolds.running(running)
	defer done()

	got, err := e.codeHolds(t.Context(), []string{"repo-a", "repo-b", "repo-c", "repo-d", refA, refB})
	if err != nil {
		t.Fatalf("codeHolds: %v", err)
	}
	if !got["repo-a"] || !got["repo-b"] || !got["repo-c"] || got["repo-d"] || !got[refA] || !got[refB] ||
		len(got) != 5 {
		t.Fatalf("held %v, want repo-a and a branch (a row), repo-b and a branch (a running turn) and repo-c "+
			"(a newer build's row)", got)
	}

	// A TURN THAT RETURNED holds nothing in memory any more.
	done()
	if got, err = e.codeHolds(t.Context(), []string{"repo-b"}); err != nil || got["repo-b"] {
		t.Fatalf("after its turn returned, repo-b reads %v, %v — want unheld", got, err)
	}

	// A SEAT HOLDER THAT DOES NOT ANSWER is an unknown, named.
	if _, _, err := e.backends.Coord.TryAcquire(t.Context(), coord.SeatResource(uuid.New()),
		coord.AcquireOptions{Owner: "silent-node:boot-1", TTL: time.Minute}); err != nil {
		t.Fatalf("seat a silent holder: %v", err)
	}
	if _, err := e.codeHolds(t.Context(), []string{"repo-b"}); err == nil ||
		!strings.Contains(err.Error(), "silent-node:boot-1") {
		t.Fatalf("a silent seat holder = %v, want an error naming it", err)
	}
}

// failingRuns is a run store that cannot be listed, and failingLeases a lease
// listing that cannot be read.
type failingRuns struct{}

func (failingRuns) ListRuns(context.Context) ([]sandbox.PendingRun, error) {
	return nil, errors.New("the coordination store refused the listing")
}

type failingLeases struct{}

func (failingLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.New("the lease bucket refused the listing")
}

type listedRuns []sandbox.PendingRun

func (r listedRuns) ListRuns(context.Context) ([]sandbox.PendingRun, error) { return r, nil }

type listedLeases []coord.Lease

func (l listedLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) { return l, nil }

// AN UNREAD SOURCE IS AN ERROR, NEVER "NOTHING HELD": a run store that cannot
// be listed, seat leases that cannot be read, and a holder whose answer this
// build cannot read — a version it does not speak — each fail the read naming
// what was not read, so the pass expires nothing that page.
//
// Mutation: skip a reply at another version as answered — the old build's
// holder reads as holding nothing, red.
func TestAnUnreadHoldsSourceIsAnError(t *testing.T) {
	t.Parallel()
	var self runningTurns
	holder := []coord.Lease{{Resource: coord.SeatResource(uuid.New()), Owner: "node-b:boot-1"}}
	answer := func(version int) func(context.Context, string, []byte, func([]byte) bool) error {
		return func(_ context.Context, _ string, _ []byte, each func([]byte) bool) error {
			raw, err := json.Marshal(codeHoldsAnswer{Version: version, Owner: "node-b:boot-1"})
			if err != nil {
				return err
			}
			each(raw)
			return nil
		}
	}
	for name, tc := range map[string]struct {
		r    holdsReader
		want string
	}{
		"a run store that cannot be listed": {holdsReader{runs: failingRuns{}, leases: listedLeases(holder),
			askEach: answer(codeHoldsProtocol), self: &self, budget: time.Second}, "run store"},
		"seat leases that cannot be read": {holdsReader{runs: listedRuns(nil), leases: failingLeases{},
			askEach: answer(codeHoldsProtocol), self: &self, budget: time.Second}, "seat leases"},
		"a holder answering at another version": {holdsReader{runs: listedRuns(nil),
			leases: listedLeases(holder), askEach: answer(codeHoldsProtocol + 1), self: &self,
			budget: time.Second}, "node-b:boot-1"},
	} {
		if _, err := tc.r.holds(t.Context(), []string{"repo-a"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %v, want an error naming %q", name, err, tc.want)
		}
	}
	// AND A NODE WITH NO BROKER IS THE FLEET: its own turns are every turn.
	h := turnctx.NewHolds([]string{"repo-a"})
	done := self.running(h)
	defer done()
	got, err := holdsReader{runs: listedRuns(nil), self: &self}.holds(t.Context(), []string{"repo-a", "repo-z"})
	if err != nil || !got["repo-a"] || got["repo-z"] {
		t.Fatalf("a node with no broker read %v, %v — want its own turn's repo-a", got, err)
	}
}

// A NODE HOLDING NO SEAT NEVER STANDS IN FOR A HOLDER. Every engine answers
// the holds question, whatever its roles, so a node with no seat lease — a git
// or ingress node, a data node without seats, one a fleet with fewer seats
// than nodes left none — answers it too, and the read must wait for the
// holders the seat leases NAME, never for as many replies as there are
// holders: the first answers to arrive would fill that count with a non-holder's
// and leave a holder's unread. Two nodes on one broker, both answering: node-a
// asks and holds no seat, and its own answer reaches the reader before node-b
// answers at all; node-b holds the only seat lease. Every read hears node-b,
// and what node-a's stopping turn holds is believed beside it. A holder the
// leases name that serves nothing is the only one an error names, and an ask
// that ended before its budget claims no silence of the budget's length.
//
// Mutation: end the ask once it has as many replies as the leases name
// holders — node-a's answer fills it, and every read names node-b silent, red.
func TestANodeHoldingNoSeatNeverStandsInForAHolder(t *testing.T) {
	t.Parallel()
	const nodeA, nodeB = "node-a:boot-1", "node-b:boot-1"
	broker := memory.NewBroker()
	qa, qb := broker.Client(), broker.Client()
	for _, q := range []*memory.Queue{qa, qb} {
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start a node's queue: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
	}
	var turnsA, turnsB runningTurns
	defer turnsA.running(turnctx.NewHolds([]string{"repo-a"}))()
	defer turnsB.running(turnctx.NewHolds([]string{"repo-b"}))()

	// NODE-A'S ANSWER FIRST, every read: node-b's answerer waits on the
	// read's gate, which the reader's seam opens once node-a's reply has
	// reached the reader — so a count of replies would end before node-b's.
	var (
		mu   sync.Mutex
		gate chan struct{}
	)
	opened := func() chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		return gate
	}
	stopA, err := qa.Serve(t.Context(), topics.CodeHolds, queue.ConcurrentAnswers, answerCodeHolds(nodeA, &turnsA))
	if err != nil {
		t.Fatalf("serve node-a's holds: %v", err)
	}
	t.Cleanup(func() { _ = stopA(context.Background()) })
	answerB := answerCodeHolds(nodeB, &turnsB)
	stopB, err := qb.Serve(t.Context(), topics.CodeHolds, queue.ConcurrentAnswers,
		func(ctx context.Context, raw []byte) ([]byte, error) {
			select {
			case <-opened():
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return answerB(ctx, raw)
		})
	if err != nil {
		t.Fatalf("serve node-b's holds: %v", err)
	}
	t.Cleanup(func() { _ = stopB(context.Background()) })
	askEach := func(ctx context.Context, subject string, request []byte, each func([]byte) bool) error {
		g := opened()
		var once sync.Once
		return qa.AskEach(ctx, subject, request, func(reply []byte) bool {
			more := each(reply)
			var a codeHoldsAnswer
			if json.Unmarshal(reply, &a) == nil && a.Owner == nodeA {
				once.Do(func() { close(g) })
			}
			return more
		})
	}
	read := func(leases listedLeases) (map[string]bool, error) {
		mu.Lock()
		gate = make(chan struct{})
		mu.Unlock()
		r := holdsReader{runs: listedRuns(nil), leases: leases, askEach: askEach, self: &turnsA,
			budget: 30 * time.Second}
		return r.holds(t.Context(), []string{"repo-a", "repo-b", "repo-z"})
	}

	onlyB := listedLeases{{Resource: coord.SeatResource(uuid.New()), Owner: nodeB}}
	for i := range 5 {
		got, err := read(onlyB)
		if err != nil {
			t.Fatalf("read %d: %v — want node-b heard, whatever answered first", i, err)
		}
		if !got["repo-b"] || !got["repo-a"] || got["repo-z"] || len(got) != 2 {
			t.Fatalf("read %d held %v, want repo-b (the holder's turn) and repo-a (the asker's "+
				"stopping turn, believed)", i, got)
		}
	}

	// A HOLDER THAT SERVES NOTHING is named alone; the ask ended when every
	// node serving had answered, long before its budget, and says no more.
	_, err = read(append(onlyB, coord.Lease{Resource: coord.SeatResource(uuid.New()), Owner: "silent-node:boot-1"}))
	switch {
	case err == nil:
		t.Fatal("a seat holder that never answered read as holding nothing")
	case !strings.Contains(err.Error(), "silent-node:boot-1"):
		t.Fatalf("the read failed with %v, want the silent holder named", err)
	case strings.Contains(err.Error(), nodeA) || strings.Contains(err.Error(), nodeB):
		t.Fatalf("the read failed with %v, which names a node that answered", err)
	case strings.Contains(err.Error(), "inside"):
		t.Fatalf("the read failed with %v, claiming a silence of its whole budget for an ask that "+
			"ended when every node serving had answered", err)
	}
}

// A HOLD MOVING ONTO ITS TURN'S FIRST ROW IS NEVER MISSED. A native turn's
// first launch writes its holds on a NEW row (BeginLaunch), and the turn
// suspends and forgets them in memory as its frame returns. A row written
// after the reader listed the run store, by a turn whose frame returned before
// the running turns were asked, was in neither answer, and the pass expired a
// scratch repository the parked run depends on (M3-decisions M3-O61). Here
// the run store lists no row first and, as it answers, the turn launches — its
// row written, its frame returned — so the running turns hold nothing when
// asked: the rows read after the ask hold the key. And a listing after the
// ask that fails is an unknown, named, like the first.
//
// Mutations: drop the listing after the ask — repo-s reads unheld, red; read
// its failure as nothing held — no error, red.
func TestAHoldMovingOntoItsTurnsFirstRowIsNeverMissed(t *testing.T) {
	t.Parallel()
	var self runningTurns
	runs := &launchingRuns{launch: self.running(turnctx.NewHolds([]string{"repo-s"})),
		row: sandbox.PendingRun{TurnID: "run-s", AgentHandle: "dev", Status: sandbox.StatusAwaiting,
			CodeHolds: []string{"repo-s"}}}
	got, err := holdsReader{runs: runs, self: &self}.holds(t.Context(), []string{"repo-s", "repo-z"})
	if err != nil || !got["repo-s"] || got["repo-z"] || len(got) != 1 {
		t.Fatalf("a turn that launched while its holds were read: %v, %v — want repo-s held, "+
			"from the row its launch wrote", got, err)
	}
	if runs.listed != 2 {
		t.Fatalf("the run store was listed %d times, want once before the ask and once after", runs.listed)
	}

	runs = &launchingRuns{launch: func() {}, failAfter: true}
	if _, err := (holdsReader{runs: runs, self: &self}).holds(t.Context(), []string{"repo-s"}); err == nil ||
		!strings.Contains(err.Error(), "after the running turns were asked") {
		t.Fatalf("a run store that could not be listed after the ask = %v, want an error naming it", err)
	}
}

// launchingRuns is a run store a turn's first launch lands in while it is
// being listed: its first listing holds no row and, as it answers, the turn
// launches — its row written, its frame returned (launch forgets its set);
// every later listing holds the row, or fails where failAfter is set.
type launchingRuns struct {
	launch    func()
	row       sandbox.PendingRun
	failAfter bool
	listed    int
}

func (r *launchingRuns) ListRuns(context.Context) ([]sandbox.PendingRun, error) {
	r.listed++
	switch {
	case r.listed == 1:
		r.launch()
		return nil, nil
	case r.failAfter:
		return nil, errors.New("the coordination store refused the listing")
	}
	return []sandbox.PendingRun{r.row}, nil
}

// A TURN HOLDS WHAT ITS ROW MAY: the most one turn may hold
// (turnctx.MaxHolds) is the most its run row carries (sandbox.MaxCodeHolds),
// so a turn's whole set always fits the row a launch writes it on — and
// neither package imports the other, so this is where the two meet.
func TestATurnHoldsWhatItsRowMay(t *testing.T) {
	t.Parallel()
	if turnctx.MaxHolds != sandbox.MaxCodeHolds {
		t.Fatalf("a turn may hold %d keys and its row carry %d", turnctx.MaxHolds, sandbox.MaxCodeHolds)
	}
}
