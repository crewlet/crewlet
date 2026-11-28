package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
)

// hostRing is the keyring every node of a box-read case's fleet holds.
var hostRing = recordseal.OneKey("k1", "the box-read cases' fleet key material")

// hostNode is one node of a fleet for a box-read case: its own host — a backend
// whose boxes only it holds — its coordinator over the fleet's one run store,
// and its half of the box-read channel on the fleet's one broker.
type hostNode struct {
	id          string
	provider    *sandbox.FakeProvider
	runner      *sandbox.FakeRunner
	coordinator *sandbox.Coordinator
	reads       *boxReads
}

type nilResumer struct{}

func (nilResumer) Resume(context.Context, sandbox.ResumeRequest) error { return nil }

type nobodyAudience struct{}

func (nobodyAudience) ResolveAudience(sandbox.PendingRun, string) sandbox.Audience {
	return sandbox.Audience{}
}

func (nobodyAudience) MayAnswer(context.Context, string) (bool, error) { return true, nil }

// hostFleet is two nodes over one run store and one broker.
func hostFleet(t *testing.T) (store sandbox.PendingStore, a, b *hostNode) {
	t.Helper()
	store, a, b, _ = hostFleetOn(t)
	return store, a, b
}

// hostFleetOn is [hostFleet], with the broker the two nodes are on, for a case
// that puts a third party on it.
func hostFleetOn(t *testing.T) (store sandbox.PendingStore, a, b *hostNode, broker *queuemem.Broker) {
	t.Helper()
	return hostFleetServing(t, boxReadsHeld, boxReadsWaiting)
}

// hostFleetServing is [hostFleetOn] with each node making held reads of its
// boxes at once and keeping waiting more waiting, registered as a node is
// ([boxReadAnswers]), for a case about those bounds.
func hostFleetServing(t *testing.T, held, waiting int) (store sandbox.PendingStore, a, b *hostNode,
	broker *queuemem.Broker) {
	t.Helper()
	store = sandbox.NewCoordStore(coordmem.NewFleet())
	broker = queuemem.NewBroker()
	node := func(id string) *hostNode {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start queue: %v", err)
		}
		n := &hostNode{id: id, provider: sandbox.NewFakeProvider(), runner: sandbox.NewFakeRunner("claude-code")}
		manager, err := sandbox.NewManager(sandbox.ManagerOptions{
			Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: n.provider},
			Runners:   map[string]sandbox.Runner{"claude-code": n.runner},
		})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		if n.reads, err = newBoxReads(hostRing, id, q, func() *sandbox.Coordinator {
			return n.coordinator
		}); err != nil {
			t.Fatalf("newBoxReads: %v", err)
		}
		n.reads.budget = flatBudget(2 * time.Second)
		n.reads.slots = newBoxReadSlots(held, waiting)
		if n.coordinator, err = sandbox.NewCoordinator(sandbox.CoordinatorOptions{
			Pushes:  noPushes{},
			Answers: testAnswers(),
			Queue:   q, Pending: store, Manager: manager, Resume: nilResumer{},
			Audience: nobodyAudience{}, Node: id, Boxes: n.reads,
		}); err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		t.Cleanup(n.coordinator.Stop)
		stop, err := q.Serve(t.Context(), topics.SandboxBoxRead, boxReadAnswers(held, waiting), n.reads.answer)
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
		return n
	}
	return store, node("node-a"), node("node-b"), broker
}

// flatBudget is a box read budget of d for every read of every run, so a case
// can watch a host stay silent without waiting minutes for it.
func flatBudget(d time.Duration) func(sandbox.PendingRun, sandbox.BoxRead) time.Duration {
	return func(sandbox.PendingRun, sandbox.BoxRead) time.Duration { return d }
}

// A BOX READ WAITS AS LONG AS ITS KIND TAKES: a stop's and a loss's read end the
// job before they take its session, and so does a collection's of a turn with a
// cap, whose host ends a job still working once the cap has passed, and records
// that it did — so each of their asks waits for that end on top of a
// collection's of a turn with none, and a capped collection's for that record
// on top of a stop's. The budget is the one for the read asked of the run asked about, never
// one figure for every read, which a node's asks are built with.
//
// Mutation: ask with one budget whatever the read, or read it off the read
// alone, and the stop's ask is made under the collection's, or a capped
// collection's under an uncapped one's.
func TestABoxReadWaitsAsLongAsItsKindTakes(t *testing.T) {
	t.Parallel()
	store, a, b := hostFleet(t)
	fresh, err := newBoxReads(hostRing, "node-c", nil, func() *sandbox.Coordinator { return nil })
	if err != nil {
		t.Fatal(err)
	}
	uncapped := sandbox.PendingRun{TurnID: "t0"}
	capped := sandbox.PendingRun{TurnID: "t0", Deadline: time.Now().Add(time.Hour).UTC()}
	for _, run := range []sandbox.PendingRun{uncapped, capped} {
		for _, read := range sandbox.BoxReads {
			if got, want := fresh.budget(run, read), sandbox.BoxReadBudget(run, read, runnerThreadTraits()[run.Harness]); got != want {
				t.Errorf("a node's %s ask of a run capped at %v waits %v, want %v", read, run.Deadline, got, want)
			}
		}
	}
	if fresh.budget(uncapped, sandbox.BoxReadStop) <= fresh.budget(uncapped, sandbox.BoxReadCollect) {
		t.Errorf("a stop's ask waits %v, no longer than a collection's %v: nothing waits for the job's end",
			fresh.budget(uncapped, sandbox.BoxReadStop), fresh.budget(uncapped, sandbox.BoxReadCollect))
	}
	// A CAPPED COLLECTION WAITS THE END AS A STOP'S DOES, and its host's record
	// that the cap ended a job it saw working besides.
	if fresh.budget(capped, sandbox.BoxReadCollect) <= fresh.budget(capped, sandbox.BoxReadStop) {
		t.Errorf("a capped collection's ask waits %v, a stop's %v: the end its host makes past the cap, "+
			"and its record of it, are not waited for", fresh.budget(capped, sandbox.BoxReadCollect),
			fresh.budget(capped, sandbox.BoxReadStop))
	}

	run := runOnHost(t, store, a, "t1")
	run.LaunchNode = "node-z"
	var asked []sandbox.BoxRead
	var about []string
	b.reads.budget = func(of sandbox.PendingRun, read sandbox.BoxRead) time.Duration {
		asked, about = append(asked, read), append(about, of.TurnID)
		return 100 * time.Millisecond
	}
	if _, _, err := b.reads.ReadBox(t.Context(), run, sandbox.BoxReadStop); !errors.Is(err, sandbox.ErrBoxHostSilent) {
		t.Fatalf("ReadBox of a node nobody serves = %v; want a silent host", err)
	}
	if len(asked) == 0 || asked[0] != sandbox.BoxReadStop || about[0] != run.TurnID {
		t.Fatalf("the stop's ask took the budget of %v for %v, want the stop's own for %s", asked, about, run.TurnID)
	}
}

// runOnHost seeds a running job host launched, its box on host's own backend,
// and returns its row.
func runOnHost(t *testing.T, store sandbox.PendingStore, host *hostNode, turnID string) sandbox.PendingRun {
	t.Helper()
	ctx := t.Context()
	box, err := host.provider.Create(ctx, sandbox.Spec{TurnID: turnID, Placement: sandbox.Direct})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Harness: "claude-code",
		LaunchNode: host.id,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Placement: string(sandbox.Direct),
		Harness: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if ok, err := store.MarkSuspended(ctx, turnID, sandbox.Suspension{
		State: json.RawMessage(`{"version":4,"iteration":1}`)}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	run, _, err := store.Get(ctx, turnID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A BOX ON ANOTHER NODE'S HOST IS READ BACK BY THAT HOST, over the fleet's
// signed channel: the node holding the run's seat asks, the host reads its own
// box — and pauses it, as the collection it is part of does — and the run's
// account crosses back.
//
// Mutation: let any node answer, or none, and the read comes back from the
// asker's own backend, which holds no such box, or not at all.
func TestABoxOnAnotherHostIsReadBackByThatHost(t *testing.T) {
	t.Parallel()
	store, a, b := hostFleet(t)
	run := runOnHost(t, store, a, "t1")
	a.runner.Finish(sandbox.Result{Success: true, Text: "fixed on node-a's host", InputTokens: 900})
	// THE COMPLETION'S CLAIM, as node-b — the seat's holder — takes it.
	claimed, won, err := store.ClaimForResume(t.Context(), "t1",
		sandbox.CompletionTail(run.LaunchID), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}

	result, ended, err := b.reads.ReadBox(t.Context(), claimed, sandbox.BoxReadCollect)
	if err != nil {
		t.Fatalf("ReadBox: %v", err)
	}
	if result.Text != "fixed on node-a's host" || result.InputTokens != 900 || ended {
		t.Errorf("read back %+v (ended %v); want node-a's account of its box", result, ended)
	}
	if !a.provider.Box(run.SandboxID).Paused() {
		t.Error("the host did not pause the box its collection read")
	}

	// AND ONLY THE NODE THE REQUEST NAMES ANSWERS IT: node-b, handed it,
	// answers nothing and reads nothing.
	sealed, err := b.reads.wire.SealRequest(sandbox.BoxReadRequest{
		Version: sandbox.BoxReadWireVersion, ID: "for-node-a", Deadline: time.Now().Add(time.Minute),
		BudgetMS: time.Minute.Milliseconds(), Node: "node-a", TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID,
		Read: sandbox.BoxReadCollect,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := b.reads.answer(t.Context(), sealed); !errors.Is(err, errBoxReadNotHere) || reply != nil {
		t.Errorf("node-b answered a read addressed to node-a: %q, %v", reply, err)
	}
}

// A HOST THAT DOES NOT ANSWER IS SAID TO BE SILENT, never a box that failed: a
// request for a node with no answerer — stopped, restarting or gone — comes
// back as [sandbox.ErrBoxHostSilent] once the ask's budget is spent.
func TestAHostThatDoesNotAnswerIsSilent(t *testing.T) {
	t.Parallel()
	store, a, b := hostFleet(t)
	run := runOnHost(t, store, a, "t1")
	run.LaunchNode = "node-c"
	b.reads.budget = flatBudget(200 * time.Millisecond)

	_, _, err := b.reads.ReadBox(t.Context(), run, sandbox.BoxReadCollect)
	if !errors.Is(err, sandbox.ErrBoxHostSilent) || errors.Is(err, sandbox.ErrBoxNotAsked) {
		t.Errorf("ReadBox of a node nobody serves = %v; want a silent host, asked", err)
	}
}

// AN ASK THAT NEVER LEFT THIS NODE IS ONE IT COULD NOT MAKE, never a host's
// silence: its broker refusing the request, or its context leaving it no time
// to ask with, says nothing about the box's host, which never heard it. Both
// are handled as a host that did not answer, and both name themselves
// ([sandbox.ErrBoxNotAsked]), because a stop counts a host's silence only from
// an ask that reached the broker and came back with nothing, and would
// otherwise start counting it from an ask the host was never put.
//
// Mutation: label a failed ask, or the return for no time left, a host that
// did not answer, and the ask that never left reads as the host's silence.
func TestAnAskThatNeverLeftThisNodeIsOneItCouldNotMake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// unasked leaves node-b's ask nothing to cross by: it fails the
		// broker, or hands back a context with too little time to ask with.
		unasked func(context.Context, *queuemem.Broker) (context.Context, context.CancelFunc)
	}{
		{"its broker gone", func(ctx context.Context, broker *queuemem.Broker) (context.Context, context.CancelFunc) {
			broker.Fail()
			return context.WithCancel(ctx)
		}},
		{"under a millisecond left", func(ctx context.Context, _ *queuemem.Broker) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, time.Millisecond-time.Microsecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store, a, b, broker := hostFleetOn(t)
				run := runOnHost(t, store, a, "t1")
				a.runner.Finish(sandbox.Result{Success: true})
				claimed, won, err := store.ClaimForResume(t.Context(), "t1",
					sandbox.CompletionTail(run.LaunchID), sandbox.Fence{})
				if err != nil || !won {
					t.Fatalf("ClaimForResume = %v, %v", won, err)
				}
				reads := (&watchedReads{}).watch(a)
				ctx, cancel := tc.unasked(t.Context(), broker)
				defer cancel()
				_, _, err = b.reads.ReadBox(ctx, claimed, sandbox.BoxReadCollect)
				if !errors.Is(err, sandbox.ErrBoxNotAsked) || !errors.Is(err, sandbox.ErrBoxHostSilent) {
					t.Errorf("ReadBox = %v; want an ask this node could not make, handled as a host "+
						"that did not answer", err)
				}
				if made := reads.made(); len(made) != 0 {
					t.Errorf("the host made %d reads of an ask that never left its asker, want none", len(made))
				}
			})
		})
	}
}

// A HOST READS FOR A PEER ONLY WHAT THE RUN'S ROW LICENSES: a running job's box
// is not paused for a request, whoever signed it.
func TestAHostRefusesARequestTheRowDoesNotLicense(t *testing.T) {
	t.Parallel()
	store, a, b := hostFleet(t)
	run := runOnHost(t, store, a, "t1")
	a.runner.Finish(sandbox.Result{Success: true})

	_, _, err := b.reads.ReadBox(t.Context(), run, sandbox.BoxReadCollect)
	if !errors.Is(err, sandbox.ErrBoxNotHosted) {
		t.Errorf("ReadBox of a running job = %v; want the host's refusal", err)
	}
	if a.provider.Box(run.SandboxID).Paused() {
		t.Error("the host paused a running job's box for a request")
	}
}

// A REQUEST THE FLEET DID NOT SIGN READS NOTHING: the host carries a read out,
// and only the fleet's signature makes a request an instruction it takes.
//
// Mutation: answer a request whatever its frame, and the forged one reads the
// box.
func TestABoxReadTheFleetDidNotSignReadsNothing(t *testing.T) {
	t.Parallel()
	store, a, _ := hostFleet(t)
	run := runOnHost(t, store, a, "t1")
	if _, won, err := store.ClaimForResume(t.Context(), "t1",
		sandbox.CompletionTail(run.LaunchID), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	// TWO FORGERIES: material that is not the fleet's under the fleet's key
	// id — a MAC that fails — and a key id the fleet does not hold, whose
	// body is still decoded, untrusted, for the refusal to name.
	for _, ring := range []recordseal.Keyring{
		recordseal.OneKey("k1", "not the fleet's key material"),
		recordseal.OneKey("k9", "a key the fleet does not hold"),
	} {
		stranger, err := newBoxReads(ring, "node-x", nil, func() *sandbox.Coordinator { return nil })
		if err != nil {
			t.Fatal(err)
		}
		forged, err := stranger.wire.SealRequest(sandbox.BoxReadRequest{
			Version: sandbox.BoxReadWireVersion, ID: "forged-" + ring.ActiveID,
			Deadline: time.Now().Add(time.Minute), BudgetMS: time.Minute.Milliseconds(),
			Node: "node-a", TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID,
			Read: sandbox.BoxReadCollect,
		})
		if err != nil {
			t.Fatal(err)
		}
		if reply, err := a.reads.answer(t.Context(), forged); err == nil || reply != nil {
			t.Errorf("a request signed under %s was answered %q, %v", ring.ActiveID, reply, err)
		}
		if a.provider.Box(run.SandboxID).Paused() {
			t.Errorf("a request signed under %s paused the box", ring.ActiveID)
		}
	}
}

// A REPLY TOO LARGE FOR THE BROKER IS CARRIED AT THE SMALLER BOUND. A host holds
// each piece of a run's own text to the most one condensation reads before it
// signs its reply; three such pieces fit the broker's ceiling unless JSON grows
// them several-fold, which text full of quotes and angle brackets makes it do —
// and a reply the broker refuses is a host that never answered, every
// collection of the run handed back uncounted, the run never settled. Then the
// host sends each piece at what the run's record carries of it, with the note
// saying so.
//
// Mutation: keep the first bound whatever the sealed reply weighs, and the
// reply is past the broker's ceiling.
func TestAReplyTooLargeForTheBrokerIsCarriedAtTheSmallerBound(t *testing.T) {
	t.Parallel()
	store, a, b := hostFleet(t)
	run := runOnHost(t, store, a, "t1")
	// EACH PIECE AT THE FIRST BOUND, written so JSON grows it about fourfold.
	heavy := strings.Repeat(strings.Repeat(`"<`, 40)+"\n", sandbox.MaxCondenseBytes/81)
	a.runner.Finish(sandbox.Result{Text: heavy, Error: heavy})
	claimed, won, err := store.ClaimForResume(t.Context(), "t1",
		sandbox.CompletionTail(run.LaunchID), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	sealed, err := b.reads.wire.SealRequest(sandbox.BoxReadRequest{
		Version: sandbox.BoxReadWireVersion, ID: "heavy-read", Deadline: time.Now().Add(time.Minute),
		BudgetMS: time.Minute.Milliseconds(), Node: "node-a", TurnID: "t1", LaunchID: claimed.LaunchID, SandboxID: claimed.SandboxID,
		Read: sandbox.BoxReadCollect,
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := a.reads.answer(t.Context(), sealed)
	if err != nil {
		t.Fatalf("the host's answer: %v", err)
	}
	if len(reply) > queue.MaxPayloadBytes {
		t.Fatalf("the host's sealed reply is %d bytes, past the broker's %d", len(reply), queue.MaxPayloadBytes)
	}
	rep, why, err := b.reads.wire.OpenReply(reply, "heavy-read")
	if err != nil || why != "" || rep.Result == nil {
		t.Fatalf("OpenReply = %+v, %q, %v", rep, why, err)
	}
	for name, piece := range map[string]string{
		"report": rep.Result.Text, "failure": rep.Result.Error,
	} {
		if len(piece) > sandbox.MaxRunTextBytes || !strings.Contains(piece, "not shown") {
			t.Errorf("the %s crossed at %d bytes (note %v), want it held to %d with the note saying so",
				name, len(piece), strings.Contains(piece, "not shown"), sandbox.MaxRunTextBytes)
		}
	}
}

// A REPLY ONLY THE ASKED HOST SIGNED IS THE ANSWER. Every node serves the box
// read, and the asker takes the first reply the fleet signed FROM THE NODE IT
// ASKED: a reply naming another node, or one sealed under material that is not
// the fleet's, is no answer about the box, and the asker hears a silent host.
//
// Mutation: take a reply whatever node it names, and another node's account of
// the run is read as its host's.
func TestOnlyTheAskedHostsSignedReplyIsTheAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// ring seals the third party's reply, and node is the node it names.
		ring recordseal.Keyring
		node string
	}{
		{"a reply naming another node", hostRing, "node-c"},
		{"a reply the fleet did not sign", recordseal.OneKey("k1", "not the fleet's key material"), "node-z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, a, b, broker := hostFleetOn(t)
			run := runOnHost(t, store, a, "t1")
			// THE BOX'S HOST answers nobody here: the run names node-z,
			// which serves nothing, and the third party answers for it.
			run.LaunchNode = "node-z"
			b.reads.budget = flatBudget(500 * time.Millisecond)
			fleet, err := newBoxReads(hostRing, "node-c", nil, func() *sandbox.Coordinator { return nil })
			if err != nil {
				t.Fatal(err)
			}
			replier, err := newBoxReads(tc.ring, "node-c", nil, func() *sandbox.Coordinator { return nil })
			if err != nil {
				t.Fatal(err)
			}
			q := broker.Client()
			if err := q.Start(t.Context()); err != nil {
				t.Fatalf("start queue: %v", err)
			}
			taken := signedask.NewGuard()
			stop, err := q.Serve(t.Context(), topics.SandboxBoxRead, queue.ConcurrentAnswers, func(ctx context.Context, raw []byte) ([]byte, error) {
				admitted := fleet.wire.Admit(ctx, raw, taken, time.Now())
				if admitted.Disposition != signedask.Admitted {
					return nil, errors.New(admitted.Why)
				}
				return replier.wire.SealReply(sandbox.BoxReadReply{
					Version: sandbox.BoxReadWireVersion, Request: admitted.ID, Node: tc.node,
					Result: &sandbox.Result{Success: true, Text: "not the host's account"},
				})
			})
			if err != nil {
				t.Fatalf("serve: %v", err)
			}
			t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

			result, _, err := b.reads.ReadBox(t.Context(), run, sandbox.BoxReadStop)
			if !errors.Is(err, sandbox.ErrBoxHostSilent) {
				t.Fatalf("ReadBox = %+v, %v; want the asked host silent", result, err)
			}
		})
	}
}

// watchedReads stands in for a host's read of its box ([boxReads.answerRead]):
// it records when each read started and the deadline it was handed, holds the
// first hold reads until release closes, and answers each with an account of
// the run — or makes the real read, where through is set.
type watchedReads struct {
	hold    int
	release chan struct{}
	through bool

	mu    sync.Mutex
	reads []watchedRead
}

// watchedRead is one read a host made: the turn it read, when it started and
// the deadline its context was handed.
type watchedRead struct {
	turn     string
	at       time.Time
	deadline time.Time
}

func (w *watchedReads) read(ctx context.Context, host *sandbox.Coordinator,
	req sandbox.BoxReadRequest) sandbox.BoxReadReply {
	deadline, _ := ctx.Deadline()
	w.mu.Lock()
	w.reads = append(w.reads, watchedRead{turn: req.TurnID, at: time.Now(), deadline: deadline})
	held := len(w.reads) <= w.hold
	w.mu.Unlock()
	if held {
		<-w.release
	}
	if w.through {
		return sandbox.AnswerBoxRead(ctx, host, req)
	}
	return sandbox.BoxReadReply{Version: sandbox.BoxReadWireVersion, Request: req.ID, Node: req.Node,
		Result: &sandbox.Result{Success: true, Text: "read on " + req.Node}}
}

// made is every read the host made so far.
func (w *watchedReads) made() []watchedRead {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.reads)
}

// watch has n's host make its reads through w.
func (w *watchedReads) watch(n *hostNode) *watchedReads {
	if w.release == nil {
		w.release = make(chan struct{})
	}
	n.reads.answerRead = w.read
	return w
}

// remoteRun is a run whose box is on node-a's host, as node-b asks about it.
func remoteRun(turn string) sandbox.PendingRun {
	return sandbox.PendingRun{TurnID: turn, LaunchNode: "node-a", LaunchID: "launch-" + turn,
		SandboxID: "box-" + turn}
}

// hostShareOf is the host's share of an asker's wait d.
func hostShareOf(d time.Duration) time.Duration {
	return time.Duration(float64(d) * sandbox.BoxReadAnswerShare)
}

// A HOST BEHIND ITS ASKER'S CLOCK READS ONLY ITS SHARE OF THE ASKER'S WAIT. The
// asker's wait crosses as a duration and the host counts its share of it from
// the request's arrival on its own clock, so a host an hour behind its asker
// reads for three quarters of the two seconds the asker waits — never for the
// hour between the two clocks besides, reading a session out of the box for an
// asker that has gone — and an asker whose own context ends sooner carries
// that shorter wait, so its host reads for its share of that.
//
// Mutation: read for the share of the time to the request's deadline on the
// host's clock, or send the full budget whatever the asker's context, and the
// read is handed the hour as well, or the budget its asker does not wait.
func TestAHostBehindItsAskersClockReadsOnlyItsShareOfTheAskersWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// asking is how long the asker's own context lasts, zero for no
		// deadline of its own; wait is what the asker then waits.
		asking, wait time.Duration
	}{
		{"its asker waiting its whole budget", 0, 2 * time.Second},
		{"its asker's own context ending sooner", time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				_, a, b := hostFleet(t)
				b.reads.now = func() time.Time { return time.Now().Add(time.Hour) }
				reads := (&watchedReads{}).watch(a)
				ctx := t.Context()
				if tc.asking > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.asking)
					defer cancel()
				}
				start := time.Now()
				result, _, err := b.reads.ReadBox(ctx, remoteRun("t1"), sandbox.BoxReadCollect)
				if err != nil || result.Text != "read on node-a" {
					t.Fatalf("ReadBox = %+v, %v; want the host's account", result, err)
				}
				made := reads.made()
				if len(made) != 1 {
					t.Fatalf("the host made %d reads, want one", len(made))
				}
				if want := start.Add(hostShareOf(tc.wait)); !made[0].at.Equal(start) || !made[0].deadline.Equal(want) {
					t.Errorf("the host read from %v until %v, want from %v until %v: three quarters of "+
						"the %v its asker waits", made[0].at, made[0].deadline, start, want, tc.wait)
				}
			})
		})
	}
}

// A HOST AHEAD OF ITS ASKER'S CLOCK STILL READS, WITHIN THE ALLOWANCE. A host
// whose clock stands ahead of its asker's by more than the asker's wait still
// takes the read, up to the two hours the channel allows two clocks
// ([sandbox.BoxReadClockAllowance]), and reads for its share of the wait from
// the request's arrival, the run's account crossing back; past them it
// refuses the read as one whose asker has gone, reads nothing, and its asker
// hears a host that did not answer — not one that was busy.
//
// Mutation: admit with no allowance, or one of an hour and a half, and a host
// an hour or two ahead refuses the read; judge no deadline at all, and one past
// the allowance reads.
func TestAHostAheadOfItsAskersClockStillReadsWithinTheAllowance(t *testing.T) {
	t.Parallel()
	const wait = 2 * time.Second
	for _, tc := range []struct {
		name  string
		ahead time.Duration
		reads bool
	}{
		{"an hour past the asker's wait", wait + time.Hour, true},
		{"a second inside the allowance", wait + sandbox.BoxReadClockAllowance - time.Second, true},
		{"a second past the allowance", wait + sandbox.BoxReadClockAllowance + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store, a, b := hostFleet(t)
				run := runOnHost(t, store, a, "t1")
				a.runner.Finish(sandbox.Result{Success: true, Text: "fixed on node-a's host", InputTokens: 900})
				claimed, won, err := store.ClaimForResume(t.Context(), "t1",
					sandbox.CompletionTail(run.LaunchID), sandbox.Fence{})
				if err != nil || !won {
					t.Fatalf("ClaimForResume = %v, %v", won, err)
				}
				a.reads.now = func() time.Time { return time.Now().Add(tc.ahead) }
				reads := (&watchedReads{through: true}).watch(a)

				start := time.Now()
				result, _, err := b.reads.ReadBox(t.Context(), claimed, sandbox.BoxReadCollect)
				made := reads.made()
				if !tc.reads {
					if !errors.Is(err, sandbox.ErrBoxHostSilent) || errors.Is(err, sandbox.ErrBoxHostBusy) ||
						errors.Is(err, sandbox.ErrBoxNotAsked) {
						t.Errorf("ReadBox of a host past the allowance = %v; want a host that did not answer, "+
							"asked", err)
					}
					if len(made) != 0 {
						t.Errorf("a host past the allowance made %d reads, want none", len(made))
					}
					return
				}
				if err != nil || result.Text != "fixed on node-a's host" || result.InputTokens != 900 {
					t.Fatalf("ReadBox read %q (%d tokens), %v; want node-a's account of its box",
						result.Text, result.InputTokens, err)
				}
				if len(made) != 1 || !made[0].deadline.Equal(start.Add(hostShareOf(wait))) {
					t.Errorf("the host made %+v, want one read until %v", made, start.Add(hostShareOf(wait)))
				}
			})
		})
	}
}

// A BOX READ ANOTHER BUILD CANNOT READ WHOLE READS NOTHING. A host takes only a
// request of its own wire version that carries its asker's wait and nothing
// it does not declare: an older asker's request (version 1, no wait), one of
// another version, one carrying no wait, and one carrying a member this build
// does not declare are each refused — answered with nothing, so the asker
// hears a host that did not answer, logged on the named node — rather than
// read short or read for a wait it does not know.
//
// Mutation: drop the version check, the wait check, or the record of the
// members the request does not declare, and the read of another version, the
// read with no wait, or the read carrying a member this build ignores is made.
func TestABoxReadAnotherBuildCannotReadWholeReadsNothing(t *testing.T) {
	logs := captureLogs(t)
	_, a, _ := hostFleet(t)
	reads := (&watchedReads{}).watch(a)
	domain, _ := a.reads.wire.Domains()
	signer, err := recordseal.NewSigner(domain, hostRing)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// version, budget and extra are the request's wire version, the wait
		// it carries (absent where negative) and the members it carries
		// besides.
		version int
		budget  int64
		extra   map[string]any
		read    bool
	}{
		{"this build's request", sandbox.BoxReadWireVersion, 60_000, nil, true},
		{"an older asker's request", 1, -1, nil, false},
		{"version 1 carrying a wait", 1, 60_000, nil, false},
		{"version 3 carrying a wait", 3, 60_000, nil, false},
		{"this build's version carrying no wait", sandbox.BoxReadWireVersion, 0, nil, false},
		{"a member this build does not declare", sandbox.BoxReadWireVersion, 60_000,
			map[string]any{"trace": "x"}, false},
	} {
		turn := "t-" + strings.ReplaceAll(tc.name, " ", "-")
		body := map[string]any{"v": tc.version, "id": signedask.NewID(), "deadline": time.Now().Add(time.Minute),
			"node": "node-a", "turn_id": turn, "launch_id": "l-1", "sandbox_id": "box-1",
			"read": sandbox.BoxReadCollect}
		if tc.budget >= 0 {
			body["budget_ms"] = tc.budget
		}
		maps.Copy(body, tc.extra)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		before := len(reads.made())
		from := len(logs.records(t))
		reply, err := a.reads.answer(t.Context(), signer.Seal(raw))
		read := len(reads.made()) > before
		if tc.read {
			if err != nil || reply == nil || !read {
				t.Errorf("%s: answered %d bytes, %v, read %v; want it read", tc.name, len(reply), err, read)
			}
			continue
		}
		if err == nil || reply != nil || read {
			t.Errorf("%s: answered %d bytes, %v, read %v; want nothing read and nothing answered",
				tc.name, len(reply), err, read)
		}
		refused := false
		for _, record := range logs.records(t)[from:] {
			refused = refused || record["msg"] == "sandbox_box_read_refused" &&
				record["disposition"] == "wire" && record["turn_id"] == turn
		}
		if !refused {
			t.Errorf("%s: the named node logged no refusal of another build's read", tc.name)
		}
	}
}

// A HOST TAKES EVERY BOX READ AS IT ARRIVES. Its registration is wider than the
// reads it makes, so no read waits unseen in the broker's buffer where its
// wait goes uncounted: a read past those made at once waits inside the answer,
// handed what is left of its share, so it ends at the arrival plus the share
// whatever it waited and its asker's last quarter stays whole for the
// crossings; one whose share runs out while waiting is answered busy then,
// before its asker stops listening; one past those waiting is answered busy at
// once. Busy is a host that did not answer.
//
// Mutation: drop the waiting bound, hand a waited read its whole share or a
// share of what its asker still waits, wait for a slot with no end, or
// register no wider than the reads held and waiting: red.
func TestAHostTakesEveryBoxReadAsItArrives(t *testing.T) {
	t.Parallel()
	const wait = 2 * time.Second
	// asking asks node-a about a read of turn in a goroutine of its own, and
	// answers what ReadBox did once it returns.
	asking := func(b *hostNode, turn string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, _, err := b.reads.ReadBox(context.Background(), remoteRun(turn), sandbox.BoxReadCollect)
			done <- err
		}()
		return done
	}
	busy := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, sandbox.ErrBoxHostBusy) || !errors.Is(err, sandbox.ErrBoxHostSilent) {
			t.Errorf("%s = %v; want a busy host, handled as one that did not answer", what, err)
		}
	}
	t.Run("past those reading and waiting, busy at once", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			_, a, b, _ := hostFleetServing(t, 2, 1)
			reads := (&watchedReads{hold: 3}).watch(a)
			first := []<-chan error{asking(b, "t1"), asking(b, "t2"), asking(b, "t3")}
			synctest.Wait()
			if made := len(reads.made()); made != 2 {
				t.Fatalf("the host made %d reads at once, want 2", made)
			}
			start := time.Now()
			_, _, err := b.reads.ReadBox(t.Context(), remoteRun("t4"), sandbox.BoxReadCollect)
			busy(t, "a fourth read", err)
			if took := time.Since(start); took != 0 {
				t.Errorf("a read past those reading and waiting was answered after %v, want at once", took)
			}
			close(reads.release)
			for _, done := range first {
				<-done
			}
		})
	})
	t.Run("a read that waited, handed what is left of its share", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			_, a, b, _ := hostFleetServing(t, 1, 1)
			reads := (&watchedReads{hold: 1}).watch(a)
			held := asking(b, "t1")
			synctest.Wait()
			arrived := time.Now()
			waiting := asking(b, "t2")
			const waited = 400 * time.Millisecond
			time.Sleep(waited)
			close(reads.release)
			if err := <-waiting; err != nil {
				t.Fatalf("the read that waited = %v; want it read", err)
			}
			<-held
			made := reads.made()
			if len(made) != 2 {
				t.Fatalf("the host made %d reads, want 2", len(made))
			}
			from := arrived.Add(waited)
			if want := arrived.Add(hostShareOf(wait)); !made[1].at.Equal(from) || !made[1].deadline.Equal(want) {
				t.Errorf("the read that waited %v ran from %v until %v, want from %v until %v",
					waited, made[1].at, made[1].deadline, from, want)
			}
		})
	})
	t.Run("a read whose share runs out waiting, busy before its asker leaves", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			_, a, b, _ := hostFleetServing(t, 1, 1)
			reads := (&watchedReads{hold: 1}).watch(a)
			held := asking(b, "t1")
			synctest.Wait()
			start := time.Now()
			_, _, err := b.reads.ReadBox(t.Context(), remoteRun("t2"), sandbox.BoxReadCollect)
			busy(t, "a read waiting while the one read never ends", err)
			if took := time.Since(start); took != hostShareOf(wait) {
				t.Errorf("the waiting read was answered busy after %v, want %v: its share of its asker's wait",
					took, hostShareOf(wait))
			}
			close(reads.release)
			<-held
		})
	})
	t.Run("a registration wide enough for every read it answers busy", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			_, a, b, _ := hostFleetServing(t, 2, 1)
			reads := (&watchedReads{hold: 3}).watch(a)
			first := []<-chan error{asking(b, "t1"), asking(b, "t2"), asking(b, "t3")}
			synctest.Wait()
			start := time.Now()
			past := make([]<-chan error, queue.ConcurrentAnswers)
			for i := range past {
				past[i] = asking(b, fmt.Sprintf("past-%d", i))
			}
			for i, done := range past {
				busy(t, fmt.Sprintf("read %d past the waiting bound", i), <-done)
			}
			if took := time.Since(start); took != 0 {
				t.Errorf("the reads past the waiting bound were answered after %v, want at once", took)
			}
			close(reads.release)
			for _, done := range first {
				<-done
			}
		})
	})
}

// A READ WAITING FOR A SLOT WHEN ITS HOST WITHDRAWS IS ANSWERED WITHDRAWN. The
// host is draining or restarting, not busy: answered busy, its asker would hold
// the host present and start its silence again on every ask of a node on its
// way out, where a host that withdrew mid-read is heard as one that did not
// answer at once. So a read the withdrawal finds waiting for one of the host's
// slots is answered as the host stopping, never as busy, and reads nothing.
//
// Mutation: answer busy whatever ended the wait for a slot, and the reply is a
// busy host's.
func TestAReadWaitingForASlotWhenItsHostWithdrawsIsAnsweredWithdrawn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		_, a, b, _ := hostFleetServing(t, 1, 1)
		reads := (&watchedReads{hold: 1}).watch(a)
		held := make(chan error, 1)
		go func() {
			_, _, err := b.reads.ReadBox(context.Background(), remoteRun("t1"), sandbox.BoxReadCollect)
			held <- err
		}()
		synctest.Wait()
		run := remoteRun("t2")
		sealed, err := a.reads.wire.SealRequest(sandbox.BoxReadRequest{
			Version: sandbox.BoxReadWireVersion, ID: "withdrawn-t2",
			Deadline: time.Now().Add(time.Minute), BudgetMS: (2 * time.Second).Milliseconds(),
			Node: "node-a", TurnID: run.TurnID, LaunchID: run.LaunchID, SandboxID: run.SandboxID,
			Read: sandbox.BoxReadCollect,
		})
		if err != nil {
			t.Fatal(err)
		}
		// THE ANSWERER'S OWN LIFETIME, which its withdrawal cancels.
		serving, withdraw := context.WithCancel(t.Context())
		type answered struct {
			body []byte
			err  error
		}
		done := make(chan answered, 1)
		go func() {
			body, err := a.reads.answer(serving, sealed)
			done <- answered{body, err}
		}()
		synctest.Wait()
		withdraw()
		got := <-done
		if got.err != nil {
			t.Fatalf("the read the withdrawal found waiting = %v; want a reply", got.err)
		}
		reply, why, err := a.reads.wire.OpenReply(got.body, "withdrawn-t2")
		if err != nil {
			t.Fatalf("open the reply: %v (%s)", err, why)
		}
		if !reply.Withdrawn || reply.Busy {
			t.Errorf("the reply = withdrawn %v, busy %v; want the host stopping, never busy",
				reply.Withdrawn, reply.Busy)
		}
		if err := reply.Err(); !errors.Is(err, sandbox.ErrBoxHostSilent) || errors.Is(err, sandbox.ErrBoxHostBusy) {
			t.Errorf("the reply reads %v; want a host that did not answer, never a busy one", err)
		}
		if made := len(reads.made()); made != 1 {
			t.Errorf("the host made %d reads, want only the one it held", made)
		}
		close(reads.release)
		if err := <-held; err != nil {
			t.Fatalf("the held read = %v; want it read", err)
		}
	})
}

// A READ IS HELD ONLY WITH SOME OF ITS SHARE LEFT. A slot taken for a request
// whose share had already run out would hand the read no time to make, an
// error its asker reads as the box unread, so the slot is freed at once and
// the request refused, which its caller answers busy, as one whose share ran
// out while it waited; a request with its share before it is held.
//
// Mutation: hold a read whatever is left of its share, and the spent request
// is held, keeping the slot from the next.
func TestABoxReadWhoseShareIsSpentIsRefusedAndFreesItsSlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const share = time.Second
		slots := newBoxReadSlots(1, 1)
		if waited, held := slots.take(t.Context(), time.Now().Add(-share), share); held || waited != share {
			t.Fatalf("a read whose share ran out before it was taken = waited %v, held %v; want refused "+
				"after %v", waited, held, share)
		}
		if taken := len(slots.held); taken != 0 {
			t.Fatalf("the refused read left %d slots taken, want none", taken)
		}
		if waited, held := slots.take(t.Context(), time.Now(), share); !held || waited != 0 {
			t.Errorf("a read with its whole share = waited %v, held %v; want held at once", waited, held)
		}
		slots.leave()
	})
}
