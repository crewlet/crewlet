package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
)

// tailRig is two nodes on one broker and one fleet record: `asker` serves the
// dashboard, `owner` drives the run's box.
type tailRig struct {
	pending  *CoordStore
	provider *FakeProvider
	runner   *FakeRunner
	manager  *Manager
	broker   *queuemem.Broker
	launch   string
}

const (
	askerOwner = "n1:asker"
	boxOwner   = "n2:owner"
)

// everyBuildServes is a fleet whose every node answers a tail request.
type everyBuildServes map[string]bool

func (f everyBuildServes) OwnerFeature(_ context.Context, owner string, feature coord.Feature) (bool, error) {
	if feature != coord.FeatureSandboxTail {
		return false, nil
	}
	serves, known := f[owner]
	return !known || serves, nil
}

func newTailRig(t *testing.T) *tailRig {
	t.Helper()
	rig := &tailRig{
		pending:  NewCoordStore(coordmem.NewFleet()),
		provider: NewFakeProvider(),
		runner:   NewFakeRunner("claude-code"),
		broker:   queuemem.NewBroker(),
	}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: rig.provider},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	box, err := rig.provider.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	ctx := t.Context()
	run := PendingRun{
		TurnID: "t1", AgentHandle: "swe", Role: "SWE", CodingAgent: "claude-code",
		SandboxID: box.ID(), Placement: string(Direct), CommandID: "cmd-1",
	}
	if err := rig.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
		t.Fatalf("begin launch: %v", err)
	}
	if ok, err := rig.pending.MarkSuspended(ctx, "t1", Suspension{State: map[string]any{"x": 1}}); err != nil || !ok {
		t.Fatalf("mark suspended: %v %v", ok, err)
	}
	if ok, err := rig.pending.ClaimOwnership(ctx, "t1", boxOwner, 1); err != nil || !ok {
		t.Fatalf("claim ownership: %v %v", ok, err)
	}
	got, _, err := rig.pending.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	rig.launch = got.LaunchID
	return rig
}

// client is one started node on the rig's broker.
func (rig *tailRig) client(t *testing.T) *queuemem.Queue {
	t.Helper()
	q := rig.broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start queue: %v", err)
	}
	return q
}

// serve makes an incarnation an answerer, driving the rig's boxes.
func (rig *tailRig) serve(t *testing.T, owner string) {
	t.Helper()
	stop, err := ServeTail(t.Context(), rig.client(t), owner, rig.pending,
		func() *Manager { return rig.manager }, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// reader is the asking node's reader.
func (rig *tailRig) reader(t *testing.T, features TailFeatures) *TailReader {
	return &TailReader{
		Owner: askerOwner, Pending: rig.pending, Queue: rig.client(t),
		Features: features, Budget: 200 * time.Millisecond,
		// The ASKER has no sandbox backend of its own: an answer that
		// came from here instead of from the owner would fail loudly.
		Manager: func() *Manager { return nil },
	}
}

// ONLY THE OWNER ANSWERS, and it answers with the box's own output.
//
// Every node serves the subject, so a second answerer — the node that asked,
// or any other — replying with ITS read of a box it does not drive would put a
// stranger's answer (or an empty one) first in a scatter that takes one reply.
func TestOnlyTheOwnerAnswersATailRequest(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.SetOutput(Output{Text: "running go test ./...", Source: SourceTranscript})
	// A bystander that serves too, and CAN read the box: it drives the same
	// backend, so the only thing keeping it from answering with its own peek
	// is the addressing. Its handler is captured so the refusal is asserted
	// directly below — through the broker it would be a race the asker's
	// owner filter hides, and a test that catches the regression only when
	// the wrong node happens to answer first is not a test of it.
	bystander := &capturingServer{TailServer: rig.client(t)}
	stop, err := ServeTail(t.Context(), bystander, "n3:bystander", rig.pending,
		func() *Manager { return rig.manager }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	rig.serve(t, boxOwner)

	got, err := rig.reader(t, everyBuildServes{}).Tail(t.Context(), "t1", rig.launch)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got.Outcome != TailRunning || got.Output == nil {
		t.Fatalf("outcome = %q with output %v; want the owner's tail", got.Outcome, got.Output)
	}
	if got.Output.Text != "running go test ./..." || got.Node != "n2" {
		t.Errorf("answer = %+v from %q; want the owner's box, named n2", got.Output, got.Node)
	}
	if rig.runner.Peeks() != 1 {
		t.Errorf("the box was peeked %d times; want exactly once, by its owner", rig.runner.Peeks())
	}

	// And the bystander, handed the very request the owner answered, answers
	// nothing and reads nothing.
	req, err := json.Marshal(tailRequest{Version: tailWireVersion, TurnID: "t1", LaunchID: rig.launch, Owner: boxOwner})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := bystander.handler(t.Context(), req)
	if !errors.Is(err, errTailNotAddressed) || reply != nil {
		t.Errorf("a bystander asked for %s's run answered %q, %v; want no answer", boxOwner, reply, err)
	}
	if rig.runner.Peeks() != 1 {
		t.Errorf("the bystander peeked a box it does not own (%d peeks)", rig.runner.Peeks())
	}
}

// capturingServer serves through the broker and keeps the handler it was
// given, so a case can put a request to one node's handler directly.
type capturingServer struct {
	TailServer
	handler queue.AnswerFunc
}

func (c *capturingServer) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	c.handler = h
	return c.TailServer.Serve(ctx, subject, h)
}

// A SILENT OWNER IS NAMED, NOT EMPTY.
//
// Nobody serving the subject is the shape of a node that is gone, wedged or
// partitioned. Read as an empty tail, a person watching would see a run that
// has "said nothing" — the opposite of the truth, and a screen that polls has
// no way to notice the difference.
func TestASilentOwnerIsNamedNotEmpty(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.SetOutput(Output{Text: "never read", Source: SourceTranscript})

	got, err := rig.reader(t, everyBuildServes{}).Tail(t.Context(), "t1", rig.launch)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got.Outcome != TailOwnerSilent || got.Node != "n2" || got.Output != nil {
		t.Errorf("answer = %+v; want owner_silent naming n2 and no output", got)
	}
}

// AN OWNER ON AN OLDER BUILD IS NOT ASKED, and is not called silent: asking
// would wait out the budget on every poll for a reply that can never come.
func TestAnOwnerThatCannotAnswerIsNotAsked(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxOwner)
	got, err := rig.reader(t, everyBuildServes{boxOwner: false}).Tail(t.Context(), "t1", rig.launch)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got.Outcome != TailOwnerUpgrading || got.Node != "n2" {
		t.Errorf("answer = %+v; want owner_upgrading naming n2", got)
	}
	if rig.runner.Peeks() != 0 {
		t.Errorf("an owner that serves no tail was asked anyway (%d peeks)", rig.runner.Peeks())
	}
}

// A LAUNCH THAT IS NOT RUNNING IS SAID TO BE, with the record's own reason —
// and no box is touched to say it.
func TestATailOfAJobThatIsNotRunningSaysWhy(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxOwner)
	got, err := rig.reader(t, everyBuildServes{}).Tail(t.Context(), "t1", "an-earlier-job")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got.Outcome != TailNotRunning || got.Status != StatusReplaced {
		t.Errorf("answer = %+v; want not_running, replaced", got)
	}
	gone, err := rig.reader(t, everyBuildServes{}).Tail(t.Context(), "no-such-run", "x")
	if err != nil || gone.Outcome != TailNotRunning || gone.Status != "" {
		t.Errorf("a run with no record = %+v, %v; want not_running with no status", gone, err)
	}
	if rig.runner.Peeks() != 0 {
		t.Errorf("a box was peeked for a job that is not running (%d)", rig.runner.Peeks())
	}
}

// THE OWNER'S OWN FAILURE IS AN ERROR, carrying its sentence — never a tail
// that is merely empty, and never "silent", since it answered.
func TestAnOwnerThatCannotReadTheBoxSaysSo(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.PeekErr = errFakeBox
	rig.serve(t, boxOwner)
	_, err := rig.reader(t, everyBuildServes{}).Tail(t.Context(), "t1", rig.launch)
	if err == nil || !strings.Contains(err.Error(), "n2 owns run t1") ||
		!strings.Contains(err.Error(), errFakeBox.Error()) {
		t.Errorf("Tail err = %v; want the owner's own failure, naming it", err)
	}
}

// A node that owns the run answers from its own box without a round trip.
func TestTheOwnerReadsItsOwnBoxDirectly(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.SetOutput(Output{Text: "local", Source: SourceStderr})
	reader := &TailReader{
		Owner: boxOwner, Pending: rig.pending, Queue: rig.client(t),
		Features: everyBuildServes{}, Manager: func() *Manager { return rig.manager },
	}
	got, err := reader.Tail(t.Context(), "t1", rig.launch)
	if err != nil || got.Outcome != TailRunning || got.Output.Text != "local" || got.Output.AsOf.IsZero() {
		t.Errorf("answer = %+v, %v; want the local box's output, stamped", got, err)
	}
}

var errFakeBox = fakeBoxError("the box stopped answering")

type fakeBoxError string

func (e fakeBoxError) Error() string { return string(e) }
