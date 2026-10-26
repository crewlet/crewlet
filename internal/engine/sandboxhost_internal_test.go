package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// hostRing is the keyring every node of a box-read case's fleet holds.
var hostRing = statelog.OneKey("k1", "the box-read cases' fleet key material")

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

// hostFleet is two nodes over one run store and one broker.
func hostFleet(t *testing.T) (store sandbox.PendingStore, a, b *hostNode) {
	t.Helper()
	store = sandbox.NewCoordStore(coordmem.NewFleet())
	broker := queuemem.NewBroker()
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
		n.reads.budget = 2 * time.Second
		if n.coordinator, err = sandbox.NewCoordinator(sandbox.CoordinatorOptions{
			Queue: q, Pending: store, Manager: manager, Resume: nilResumer{},
			Audience: nobodyAudience{}, Node: id, Boxes: n.reads,
		}); err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		t.Cleanup(n.coordinator.Stop)
		stop, err := q.Serve(t.Context(), topics.SandboxBoxRead, n.reads.answer)
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
		return n
	}
	return store, node("node-a"), node("node-b")
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
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", CodingAgent: "claude-code",
		LaunchNode: host.id,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Placement: string(sandbox.Direct),
		CodingAgent: "claude-code",
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
		Node: "node-a", TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID,
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
	b.reads.budget = 200 * time.Millisecond

	if _, _, err := b.reads.ReadBox(t.Context(), run, sandbox.BoxReadCollect); !errors.Is(err, sandbox.ErrBoxHostSilent) {
		t.Errorf("ReadBox of a node nobody serves = %v; want a silent host", err)
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
	for _, ring := range []statelog.Keyring{
		statelog.OneKey("k1", "not the fleet's key material"),
		statelog.OneKey("k9", "a key the fleet does not hold"),
	} {
		stranger, err := newBoxReads(ring, "node-x", nil, func() *sandbox.Coordinator { return nil })
		if err != nil {
			t.Fatal(err)
		}
		forged, err := stranger.wire.SealRequest(sandbox.BoxReadRequest{
			Version: sandbox.BoxReadWireVersion, ID: "forged-" + ring.ActiveID,
			Deadline: time.Now().Add(time.Minute),
			Node:     "node-a", TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID,
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
