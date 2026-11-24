package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// tailRig is nodes on one broker and one fleet record: n1 serves the
// dashboard, and the node that launched the run's box (n2, unless a case says
// otherwise) hosts it, while the run's seat is held elsewhere.
type tailRig struct {
	pending  *CoordStore
	provider *FakeProvider
	runner   *FakeRunner
	manager  *Manager
	broker   *queuemem.Broker
	launch   string
}

const (
	askerNode  = "n1"
	boxHost    = "n2"
	seatHolder = "n4:holder"
)

func newTailRig(t *testing.T) *tailRig {
	t.Helper()
	return newTailRigOn(t, boxHost)
}

// newTailRigOn is a rig whose run's box was launched by launchNode — on its
// host, the direct cell's — while its seat is seatHolder's.
func newTailRigOn(t *testing.T, launchNode string) *tailRig {
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
		TurnID: "t1", AgentHandle: "swe", Role: "SWE", Harness: "claude-code",
		SandboxID: box.ID(), Placement: string(Direct), CommandID: "cmd-1",
		LaunchNode: launchNode,
	}
	if _, err := rig.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
		t.Fatalf("begin launch: %v", err)
	}
	if ok, err := rig.pending.MarkSuspended(ctx, "t1", Suspension{State: json.RawMessage(`{"x":1}`)}); err != nil || !ok {
		t.Fatalf("mark suspended: %v %v", ok, err)
	}
	if ok, err := rig.pending.ClaimOwnership(ctx, "t1", seatHolder, 1); err != nil || !ok {
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

// feeds is a node's readings, driving the rig's boxes.
func (rig *tailRig) feeds(t *testing.T) *LiveFeeds {
	t.Helper()
	feeds := NewLiveFeeds(t.Context(), LiveFeedsOptions{Manager: func() *Manager { return rig.manager }})
	t.Cleanup(feeds.Stop)
	return feeds
}

// serve makes a node an answerer, with readings of its own.
func (rig *tailRig) serve(t *testing.T, node string) *LiveFeeds {
	t.Helper()
	feeds := rig.feeds(t)
	rig.serveWith(t, node, rig.client(t), feeds)
	return feeds
}

func (rig *tailRig) serveWith(t *testing.T, node string, q TailServer, feeds *LiveFeeds) {
	t.Helper()
	stop, err := ServeTail(t.Context(), q, node, rig.pending, feeds, isCondition, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// reader is the asking node's reader. ITS BUDGET IS ROOM TO ANSWER, not the
// case's clock: a scatter returns on its one reply, so a generous budget costs
// a passing case nothing, while a tight one reads a loaded race-enabled run's
// late reply as a silent host. The one case that waits the budget out sets
// its own ([TestASilentHostIsNamedNotEmpty]).
func (rig *tailRig) reader(t *testing.T) *TailReader {
	return &TailReader{
		Node: askerNode, Pending: rig.pending, Queue: rig.client(t),
		Budget: 30 * time.Second, Condition: isCondition,
		// The ASKER has no readings of its own: an answer that came from
		// here instead of from the box's host would fail loudly.
	}
}

// tail asks for the rig's launch.
func (rig *tailRig) tail(t *testing.T, r *TailReader, cursor TailCursor) TailAnswer {
	t.Helper()
	got, err := r.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch, Cursor: cursor})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	return got
}

// held is what a viewer holds once it has an answer: a cursor at its end, and
// the text — appended, or replaced on a reset.
type held struct {
	text   string
	cursor TailCursor
}

func (h *held) take(t *testing.T, a TailAnswer) *Output {
	t.Helper()
	if a.Outcome != TailRunning || a.Output == nil {
		t.Fatalf("answer = %+v; want a tail", a)
	}
	out := a.Output
	if out.Reset {
		h.text = out.Text
	} else {
		if out.Start != h.cursor.Offset {
			t.Fatalf("a delta starts at %d; the viewer holds through %d", out.Start, h.cursor.Offset)
		}
		h.text += out.Text
	}
	h.cursor = TailCursor{Epoch: out.Epoch, Offset: out.End, Digest: out.Digest}
	return out
}

// ONLY THE BOX'S HOST ANSWERS, and it answers with the box's own output.
//
// Every node serves the subject, so a second answerer — the node that asked,
// or any other — replying with ITS read of a box it does not host would put a
// stranger's answer (or an empty one) first in a scatter that takes one reply.
func TestOnlyTheBoxsHostAnswersATailRequest(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "running go test ./...\n")
	// A bystander that serves too and CAN read the box (the same backend), so
	// only the addressing keeps it from answering. Its handler is captured so
	// the refusal is asserted directly below: through the broker it is a race
	// the asker's node filter hides.
	bystander := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, "n3", bystander, rig.feeds(t))
	rig.serve(t, boxHost)

	got := rig.tail(t, rig.reader(t), TailCursor{})
	if got.Outcome != TailRunning || got.Output == nil {
		t.Fatalf("outcome = %q with output %v; want the host's tail", got.Outcome, got.Output)
	}
	if got.Output.Text != "running go test ./...\n" || got.Node != boxHost {
		t.Errorf("answer = %+v from %q; want the host's box, named %s", got.Output, got.Node, boxHost)
	}
	if rig.runner.Reads() != 1 {
		t.Errorf("the box was read %d times; want exactly once, by its host", rig.runner.Reads())
	}

	// And the bystander, handed the very request the host answered, answers
	// nothing and reads nothing.
	reply, err := bystander.handler(t.Context(), tailRequestTo(t, rig, boxHost))
	if !errors.Is(err, errTailNotAddressed) || reply != nil {
		t.Errorf("a bystander asked for %s's box answered %q, %v; want no answer", boxHost, reply, err)
	}
	if rig.runner.Reads() != 1 {
		t.Errorf("the bystander read a box it does not host (%d reads)", rig.runner.Reads())
	}
}

// capturingServer serves through the broker and keeps the handler it was
// given, so a case can put a request to one node's handler directly.
type capturingServer struct {
	TailServer
	handler queue.AnswerFunc
}

func (c *capturingServer) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	c.handler = h
	return c.TailServer.Serve(ctx, subject, answers, h)
}

// A SILENT HOST IS NAMED, NOT EMPTY.
//
// Nobody serving the subject is a node gone, wedged or partitioned. Read as an
// empty tail, a viewer would see a run that "said nothing", and a polling
// screen could never tell the difference.
func TestASilentHostIsNamedNotEmpty(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "never read\n")

	// Nobody answers, so this case waits the whole budget out: a short one.
	reader := rig.reader(t)
	reader.Budget = 200 * time.Millisecond
	got := rig.tail(t, reader, TailCursor{})
	if got.Outcome != TailHostSilent || got.Node != boxHost || got.Output != nil {
		t.Errorf("answer = %+v; want host_silent naming %s and no output", got, boxHost)
	}
}

// A LAUNCH THAT IS NOT RUNNING IS SAID TO BE, with the record's own reason —
// and no box is touched to say it.
func TestATailOfAJobThatIsNotRunningSaysWhy(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxHost)
	reader := rig.reader(t)
	got, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: "an-earlier-job"})
	if err != nil || got.Outcome != TailNotRunning || got.Status != StatusReplaced {
		t.Errorf("answer = %+v, %v; want not_running, replaced", got, err)
	}
	gone, err := reader.Tail(t.Context(), TailQuery{TurnID: "no-such-run", LaunchID: "x"})
	if err != nil || gone.Outcome != TailNotRunning || gone.Status != "" {
		t.Errorf("a run with no record = %+v, %v; want not_running with no status", gone, err)
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("a box was read for a job that is not running (%d)", rig.runner.Reads())
	}
}

// A JOB BEING LAUNCHED IS NOT A JOB THAT STOPPED. Its row exists, with its
// launch id, from before its box does, and it stays `launching` through the
// box's creation, its provisioning and the agent's start — minutes, on a
// cold template. Answered `not_running`, a screen opened in that window would
// stop asking for good and call the run over before it began.
//
// Mutation: answer `launching` as `not_running`, and the outcome is terminal.
func TestALaunchingJobIsLaunchingNotStopped(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "t2", AgentHandle: "swe", Harness: "claude-code", Placement: string(Direct),
	}, Fence{}); err != nil {
		t.Fatal(err)
	}
	launching, _, err := rig.pending.Get(t.Context(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	rig.serve(t, boxHost)
	got, err := rig.reader(t).Tail(t.Context(),
		TailQuery{TurnID: "t2", LaunchID: launching.LaunchID})
	if err != nil || got.Outcome != TailLaunching || got.Status != StatusLaunching {
		t.Errorf("a launching job = %+v, %v; want the launching outcome", got, err)
	}
}

// isCondition is the rig's rule for a condition of the box's host, standing in
// for the engine's (builtin.Condition): a coordination store that did not
// answer, and a wait that ran out.
func isCondition(err error) bool {
	return errors.Is(err, coord.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

// errBoxHost is a provider's own words beneath its mark, naming a host: what
// the host's log holds and no reply may.
var errBoxHost = errors.New("docker: cannot connect to the daemon at tcp://runtime.example.com:2376")

// THE HOST'S OWN FAILURE IS AN ERROR CLASSED BY WHAT CLEARS IT — never a tail
// that is merely empty, never "silent", since it answered, and never its text,
// which stays in the host's log. A box its runtime could not read is a class a
// wait clears, a setup somebody fixes is one no wait does, a condition of the
// host's own is [ErrTailUnanswered], and a fault is classed as nothing; the
// asker's error names the host and the run, which is where the detail is. Sent
// the text, the asker could only answer it as a fault of its own: a runtime
// outage read as a node that broke.
//
// Mutation: put the error's text back on the reply, class every failure as a
// fault, or read a condition of the host's as a fault, and a row goes red.
func TestAHostThatCannotReadTheBoxSaysWhatClearsIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		read       error
		why        Why
		unanswered bool
	}{
		"a provider that did not answer": {read: Because(WhyProviderUnavailable, errBoxHost),
			why: WhyProviderUnavailable},
		"a box its configuration cannot start": {read: Because(WhyConfiguration, errBoxHost),
			why: WhyConfiguration},
		"a store the host could not reach": {read: fmt.Errorf("read: %w: %w",
			coord.ErrUnavailable, errBoxHost), unanswered: true},
		"a fault of the host's own": {read: fmt.Errorf("read: %w", errBoxHost)},
	} {
		rig := newTailRig(t)
		rig.runner.LiveErr = c.read
		host := &capturingServer{TailServer: rig.client(t)}
		rig.serveWith(t, boxHost, host, rig.feeds(t))

		raw, err := host.handler(t.Context(), tailRequestTo(t, rig, boxHost))
		if err != nil {
			t.Fatalf("%s: the host's handler: %v", name, err)
		}
		if strings.Contains(string(raw), "runtime.example.com") || strings.Contains(string(raw), "2376") {
			t.Errorf("%s: the host's reply carries the failure's text: %s", name, raw)
		}

		_, err = rig.reader(t).Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
		why, classed := WhyOf(err)
		switch {
		case err == nil:
			t.Errorf("%s: Tail answered; want the host's failure", name)
		case !strings.Contains(err.Error(), "n2 reads run t1's box") ||
			strings.Contains(err.Error(), "runtime.example.com"):
			t.Errorf("%s: Tail err = %v; want the host and the run named, and no box's words", name, err)
		case why != c.why || classed != (c.why != ""):
			t.Errorf("%s: classed %q (%v); want %q", name, why, classed, c.why)
		case errors.Is(err, ErrTailUnanswered) != c.unanswered:
			t.Errorf("%s: a condition of the host's = %v; want %v", name,
				errors.Is(err, ErrTailUnanswered), c.unanswered)
		}
	}
}

// A HOST WITH NO BACKEND, AND A NODE THE RUN'S BOX IS NOT ON, each say so by
// class: a setup somebody fixes, and a wait. And a failure a newer build named
// that this one does not know is classed as nothing, which is what a fault is.
//
// Mutation: answer a request for a box on another node's host with a read of
// it, or class a failure this build does not know as a wait, and a check goes
// red.
func TestAHostThatCannotAnswerSaysWhyByClass(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "never read\n")
	bare := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, boxHost, bare, nil)
	if got := replyTo(t, bare, tailRequestTo(t, rig, boxHost)); got.Failed != TailFailure(WhyConfiguration) {
		t.Errorf("a host with no backend failed %q; want configuration", got.Failed)
	}

	// A NODE THE RUN'S BOX IS NOT ON, by its own read of the record: the
	// request named it as the box's host, and the record names another.
	former := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, "n3", former, rig.feeds(t))
	if got := replyTo(t, former, tailRequestTo(t, rig, "n3")); got.Failed != TailFailedMoving {
		t.Errorf("a node the run's box is not on failed %q; want moving", got.Failed)
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("a box was read by a node that does not host it (%d reads)", rig.runner.Reads())
	}

	// A READING WHOSE NODE LOST ITS BACKEND (an apply removed
	// providers.sandbox while the run went on) is a setup somebody fixes,
	// whether a peer asked or the host answered for itself.
	noBackend := NewLiveFeeds(t.Context(), LiveFeedsOptions{Manager: func() *Manager { return nil }})
	t.Cleanup(noBackend.Stop)
	removed := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, boxHost, removed, noBackend)
	if got := replyTo(t, removed, tailRequestTo(t, rig, boxHost)); got.Failed != TailFailure(WhyConfiguration) {
		t.Errorf("a host whose backend an apply removed failed %q; want configuration", got.Failed)
	}
	local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: noBackend, Condition: isCondition}
	_, err := local.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
	if why, _ := WhyOf(err); why != WhyConfiguration {
		t.Errorf("the host answering for itself with no backend classed %q (%v); want "+
			"configuration", why, err)
	}

	newer := &TailError{Node: "n2", TurnID: "t1", Failure: "a_class_from_a_newer_build"}
	if _, classed := WhyOf(newer); classed || errors.Is(newer, ErrTailUnanswered) || newer.Failure.Valid() {
		t.Errorf("a failure this build does not know was classed: %v", newer)
	}
	for _, f := range []TailFailure{TailFailedMoving, TailFailedUnanswered, TailFailedFault} {
		if !f.Valid() || Why(f).Valid() {
			t.Errorf("%q is not a host's own failure apart from every class", f)
		}
	}
	for _, why := range Whys {
		if !TailFailure(why).Valid() {
			t.Errorf("the class %q is not a failure a host may answer", why)
		}
	}
}

// A NODE WITH NO RULE FOR A CONDITION IS NOT AN ANSWERER: handed none, every
// failure of its own would be told as a fault.
func TestATailAnswererNeedsAConditionRule(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	if _, err := ServeTail(t.Context(), rig.client(t), boxHost, rig.pending,
		rig.feeds(t), nil, queue.NewAnswerBudget(nil)); err == nil {
		t.Error("ServeTail took no rule for a condition")
	}
}

// tailRequestTo is a request for the rig's launch, addressed to node as its
// box's host.
func tailRequestTo(t *testing.T, rig *tailRig, node string) []byte {
	t.Helper()
	raw, err := json.Marshal(tailRequest{Version: tailWireVersion, TurnID: "t1",
		LaunchID: rig.launch, Node: node})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// replyTo is one node's reply to a request, decoded.
func replyTo(t *testing.T, server *capturingServer, req []byte) tailReply {
	t.Helper()
	raw, err := server.handler(t.Context(), req)
	if err != nil {
		t.Fatalf("the handler: %v", err)
	}
	var rep tailReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// THE BOX'S HOST ANSWERS FROM ITS OWN BOX without a round trip.
//
// Mutation: ask the broker whoever hosts the box, and with nobody serving the
// host answers itself silent.
func TestTheHostReadsItsOwnBoxDirectly(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceStderr, "local\n")
	reader := &TailReader{
		Node: boxHost, Pending: rig.pending, Queue: rig.client(t),
		Feeds: rig.feeds(t), Condition: isCondition,
	}
	got := rig.tail(t, reader, TailCursor{})
	if got.Outcome != TailRunning || got.Output.Text != "local\n" || got.Output.AsOf.IsZero() {
		t.Errorf("answer = %+v; want the local box's output, stamped", got)
	}
}

// A PEEK NEVER WAKES A BOX. A running record whose box is paused — the moment
// between a collection pausing the box and the record moving on — is answered
// as what it is, `box_paused`, and the box is left paused, on the host's own
// read and across the fleet alike.
//
// Mutation: read through Reconnect, and the box is woken to be read.
func TestAPeekNeverWakesAPausedBox(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "never read\n")
	run, _, err := rig.pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	box := rig.provider.Box(run.SandboxID)
	if err := box.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	rig.serve(t, boxHost)
	local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: rig.feeds(t),
		Condition: isCondition}
	for name, reader := range map[string]*TailReader{
		"across the fleet": rig.reader(t),
		"on its host":      local,
	} {
		got, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
		if err != nil || got.Outcome != TailBoxPaused || got.Output != nil {
			t.Errorf("%s: answer = %+v, %v; want box_paused with no output", name, got, err)
		}
	}
	if !box.Paused() || rig.runner.Reads() != 0 {
		t.Errorf("paused=%v reads=%d; want the box left paused and unread", box.Paused(), rig.runner.Reads())
	}
}

// A BOX THAT IS GONE IS SAID TO BE, not failed: its backend says it is gone for
// good — reaped, or a local box whose root did not outlive its node — so
// nothing more will be read from it, and its waiter records its job lost.
// Answered as an error, it would tell a person watching that the node broke.
//
// Mutation: drop either arm that answers ErrBoxGone, and that path's answer is
// an error.
func TestATailOfAGoneBoxIsGoneNotAFailure(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "never read\n")
	rig.provider.AttachFunc = func(context.Context, string) error {
		return fmt.Errorf("fake: the box was reaped: %w", ErrBoxGone)
	}
	rig.serve(t, boxHost)
	local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: rig.feeds(t),
		Condition: isCondition}
	for name, reader := range map[string]*TailReader{
		"across the fleet": rig.reader(t),
		"on its host":      local,
	} {
		got, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
		if err != nil || got.Outcome != TailBoxGone || got.Output != nil || got.Node != "n2" {
			t.Errorf("%s: answer = %+v, %v; want box_gone naming n2, with no output", name, got, err)
		}
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("a gone box was read %d times", rig.runner.Reads())
	}
}

// A LAUNCH THAT STOPPED IS LET GO, on the host's own read as on a peer's
// answer: nobody asks about it again, so its reading — up to [liveHold] of
// text — is dropped by the request that learned it stopped rather than a
// sweep later ([TestAReadingNobodyAsksAboutIsLetGoOnItsOwnClock] is the host
// that is never asked again).
//
// Mutation: drop either Forget, and that path's reading stays.
func TestAStoppedLaunchsReadingIsLetGo(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "working\n")
	served := rig.serve(t, boxHost)
	own := rig.feeds(t)
	local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: own,
		Condition: isCondition}
	remote := rig.reader(t)
	for name, reader := range map[string]*TailReader{"across the fleet": remote, "on its host": local} {
		if got := rig.tail(t, reader, TailCursor{}); got.Outcome != TailRunning {
			t.Fatalf("%s: answer = %+v; want a tail", name, got)
		}
	}
	if readings(served) != 1 || readings(own) != 1 {
		t.Fatalf("readings: served %d, own %d; want one each while the job runs",
			readings(served), readings(own))
	}
	if err := rig.pending.SetStatus(t.Context(), "t1", StatusAwaiting, Fence{}); err != nil {
		t.Fatal(err)
	}
	for name, reader := range map[string]*TailReader{"across the fleet": remote, "on its host": local} {
		if got := rig.tail(t, reader, TailCursor{}); got.Outcome != TailNotRunning {
			t.Fatalf("%s: answer = %+v; want not_running", name, got)
		}
	}
	// The asker reads the record first and answers not_running itself, so a
	// peer is told only by its own read; ask it the way an asker that read
	// the record a moment earlier would.
	raw, err := json.Marshal(tailRequest{Version: tailWireVersion, TurnID: "t1",
		LaunchID: rig.launch, Node: boxHost})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client(t).Ask(t.Context(), topics.ObserveExecutorTail, raw, 1); err != nil {
		t.Fatal(err)
	}
	if readings(served) != 0 || readings(own) != 0 {
		t.Errorf("readings: served %d, own %d; want none once the launch stopped",
			readings(served), readings(own))
	}
}

// A READING NOBODY ASKS ABOUT IS LET GO ON ITS OWN CLOCK, with no request to
// prompt it. In a fleet the host is never told a launch stopped: the node
// serving the dashboard reads the run's record, answers `not_running` itself
// and never asks the host again — and a viewer who closes the tab asks
// nobody anything. Swept only inside a request, a quiet host would keep every
// watched reading until the node stopped.
//
// Mutation: sweep only inside a request (drop the tick's drop), and the host
// keeps the reading however long nobody asks.
func TestAReadingNobodyAsksAboutIsLetGoOnItsOwnClock(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "working\n")
	clock := &stepClock{at: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	ticks := make(chan time.Time)
	host := startLiveFeeds(t.Context(), LiveFeedsOptions{
		Manager: func() *Manager { return rig.manager }, Now: clock.now,
	}, ticks)
	t.Cleanup(host.Stop)
	rig.serveWith(t, boxHost, rig.client(t), host)
	asker := rig.reader(t)

	if got := rig.tail(t, asker, TailCursor{}); got.Outcome != TailRunning {
		t.Fatalf("answer = %+v; want a tail", got)
	}
	if err := rig.pending.SetStatus(t.Context(), "t1", StatusAwaiting, Fence{}); err != nil {
		t.Fatal(err)
	}
	if got := rig.tail(t, asker, TailCursor{}); got.Outcome != TailNotRunning {
		t.Fatalf("answer = %+v; want not_running", got)
	}
	if readings(host) != 1 {
		t.Fatalf("the host holds %d readings; the asker answered not_running without asking it, "+
			"so want the one it was asked for", readings(host))
	}

	// Within liveIdle of the last request, a tick keeps it.
	clock.advance(liveIdle)
	sweepOnce(ticks, clock.now())
	if readings(host) != 1 {
		t.Fatalf("a reading asked about %v ago was dropped; want it kept for %v", liveIdle, liveIdle)
	}
	// Past it, the next tick lets it go — and nothing else had to ask.
	clock.advance(time.Second)
	sweepOnce(ticks, clock.now())
	eventually(t, func() bool { return readings(host) == 0 }, "the host kept a reading nobody "+
		"has asked about for longer than liveIdle")
}

// sweepOnce hands a node's sweep one tick and waits for the sweep it starts to
// have run: the sweep takes ticks one at a time, so a second tick is taken only
// once the first one's sweep is over.
func sweepOnce(ticks chan<- time.Time, at time.Time) {
	ticks <- at
	ticks <- at
}

// stepClock is a clock a case moves by hand, read from more than one goroutine.
type stepClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// STOP ENDS THE SWEEP and waits for it, so the goroutine is gone when Stop
// returns — and a second Stop, which a node's teardown and a case's cleanup can
// both make, returns too.
//
// GONE IS READ, NOT WAITED OUT, in a bubble: once Stop has returned and every
// goroutine of the case is blocked, a sweep still running waits on its ticks,
// and a tick offered without waiting reaches it. A quiet window after Stop
// would catch only a sweep quick enough to take a tick inside it.
//
// Mutation: leave the sweep running past the readings' end, and Stop never
// returns; return from Stop without waiting for it as well, and the sweep
// takes the tick.
func TestStopEndsTheSweep(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ticks := make(chan time.Time)
		feeds := startLiveFeeds(t.Context(), LiveFeedsOptions{}, ticks)
		stopped := make(chan struct{})
		go func() {
			feeds.Stop()
			feeds.Stop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return: the sweep did not end")
		}
		synctest.Wait()
		select {
		case ticks <- time.Now():
			t.Fatal("a stopped node's sweep took a tick")
		default:
		}
	})
}

// A NODE THAT STOPPED READS NO BOX. Its readings end before its seats are
// released and the boxes stop being its to read, and a request that lands in
// between is told the read failed rather than starting one — and leaves no
// reading behind, since the sweep that would let it go has ended too.
//
// Mutation: leave the readings' context running at Stop, and the box is read;
// keep what a request after Stop makes, and the node holds a reading for ever.
func TestAStoppedNodeReadsNoBox(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "working\n")
	feeds := rig.feeds(t)
	feeds.Stop()
	local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: feeds,
		Condition: isCondition}
	if got, err := local.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch}); !errors.Is(err, ErrTailUnanswered) {
		t.Errorf("answer = %+v, %v; want the read's failure from a node that stopped, "+
			"which a wait clears", got, err)
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("a stopped node read the box %d times", rig.runner.Reads())
	}
	if readings(feeds) != 0 {
		t.Errorf("a stopped node kept %d readings a request made after it stopped", readings(feeds))
	}
}

// readings is how many readings a node keeps.
func readings(feeds *LiveFeeds) int {
	feeds.mu.Lock()
	defer feeds.mu.Unlock()
	return len(feeds.feeds)
}

// ---------------------------------------------------------------------
// the cursor
// ---------------------------------------------------------------------

// A VIEWER IS SENT WHAT IT LACKS. The first answer is a reset carrying what
// there is; every later one carries only what was written since, starting
// where the viewer holds through — never what it already holds again.
//
// Mutation: answer every cursor with a reset, and the second answer re-sends
// the first line.
func TestAViewerIsSentOnlyWhatItLacks(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	feeds := rig.serve(t, boxHost)
	reader := rig.reader(t)
	rig.runner.Say(SourceTranscript, "[tool] bash: go test ./...\n")

	var view held
	first := view.take(t, rig.tail(t, reader, view.cursor))
	if !first.Reset || view.text != "[tool] bash: go test ./...\n" {
		t.Fatalf("the first answer = %+v; want a reset carrying what there is", first)
	}
	rig.runner.Say(SourceTranscript, "the suite passed\n")
	waitPastReuse(feeds)
	second := view.take(t, rig.tail(t, reader, view.cursor))
	if second.Reset || second.Text != "the suite passed\n" {
		t.Errorf("the second answer = %+v; want only the new line", second)
	}
	if view.text != "[tool] bash: go test ./...\nthe suite passed\n" {
		t.Errorf("the viewer holds %q; want both lines once each", view.text)
	}
}

// A CURSOR ANSWER ALWAYS STATES ITS OFFSETS, zero included, as the wire
// carries it. A reading that has settled nothing yet answers at end 0 and is
// followed from start 0. Dropped as empty, a screen would compare a missing
// start with the 0 it holds through, discard the delta and reset a poll later,
// and a REST caller told to send `end` back would have none.
//
// Mutation: tag the offsets `omitempty`, and the answers at zero carry
// neither.
func TestACursorAnswerAlwaysStatesItsOffsets(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxHost)
	reader := rig.reader(t)
	wire := func(a TailAnswer) map[string]any {
		t.Helper()
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Output map[string]any `json:"output"`
		}
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		return back.Output
	}

	var view held
	first := rig.tail(t, reader, view.cursor)
	view.take(t, first)
	next := rig.tail(t, reader, view.cursor)
	view.take(t, next)
	if next.Output.Reset {
		t.Fatalf("a cursor at the reading's end 0 was answered %+v; want a delta", next.Output)
	}
	for name, answer := range map[string]TailAnswer{"the reset at 0": first, "the delta from 0": next} {
		out := wire(answer)
		for _, key := range []string{"epoch", "start", "end", "digest"} {
			if _, present := out[key]; !present {
				t.Errorf("%s carries no %q on the wire: %v", name, key, out)
			}
		}
		if out["start"] != float64(0) || out["end"] != float64(0) {
			t.Errorf("%s = start %v, end %v; want both 0", name, out["start"], out["end"])
		}
	}
}

// waitPastReuse moves a node's readings past [LiveReuse], so the next request
// reads the box rather than being answered from the last read.
func waitPastReuse(feeds *LiveFeeds) {
	feeds.mu.Lock()
	defer feeds.mu.Unlock()
	for _, feed := range feeds.feeds {
		feed.mu.Lock()
		feed.readAt = feed.readAt.Add(-LiveReuse)
		feed.mu.Unlock()
	}
}

// A CURSOR THAT DOES NOT MATCH IS A RESET, NEVER A SPLICE: another reading's
// epoch, a digest of text this reading never settled, an offset past its end
// or before its start. Each would otherwise append the reading's tail to text
// it does not follow.
//
// Mutation: skip the digest check, and a viewer holding different text of the
// same length is sent a delta.
func TestACursorThatDoesNotMatchIsAReset(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxHost)
	reader := rig.reader(t)
	rig.runner.Say(SourceTranscript, "one\ntwo\n")
	var view held
	view.take(t, rig.tail(t, reader, view.cursor))

	for name, cursor := range map[string]TailCursor{
		"another epoch":  {Epoch: "transcript@99", Offset: view.cursor.Offset, Digest: view.cursor.Digest},
		"another digest": {Epoch: view.cursor.Epoch, Offset: view.cursor.Offset, Digest: "0123456789abcdef"},
		"past the end":   {Epoch: view.cursor.Epoch, Offset: view.cursor.Offset + 10, Digest: view.cursor.Digest},
		// A peer's request is decoded off the wire, where nothing held the
		// offset to a whole number: a negative one is a reset, not a slice
		// out of range in the host's answer.
		"before the start": {Epoch: view.cursor.Epoch, Offset: -1, Digest: view.cursor.Digest},
	} {
		got := rig.tail(t, reader, cursor)
		if got.Output == nil || !got.Output.Reset || got.Output.Text != "one\ntwo\n" {
			t.Errorf("%s: answer = %+v; want a reset carrying the reading's text", name, got.Output)
		}
	}
}

// A MOVE OF THE SEAT CONTINUES THE READING, and a reading its host begins
// again under a build that derives the stream differently RESETS. The box is
// read on its host whoever holds the seat, so a viewer's cursor stays good
// across the move; a host's new reading of the same stream from the same
// origin under a changed parser is other text, which the digest catches
// though the epoch and the offset both match.
//
// Mutation: drop the digest's check, and the other derivation's text is sent
// as a delta.
func TestASeatMoveContinuesTheReadingAndAnotherDerivationResets(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "first\n")
	host := rig.serve(t, boxHost)
	rig.serve(t, "n5")
	reader := rig.reader(t)
	var view held
	view.take(t, rig.tail(t, reader, view.cursor))

	if ok, err := rig.pending.ClaimOwnership(t.Context(), "t1", "n5:next", 2); err != nil || !ok {
		t.Fatalf("move the seat: %v %v", ok, err)
	}
	rig.runner.Say(SourceTranscript, "second\n")
	waitPastReuse(host)
	moved := view.take(t, rig.tail(t, reader, view.cursor))
	if moved.Reset || moved.Text != "second\n" {
		t.Errorf("after a move of the seat = %+v; want only the new line", moved)
	}

	// The host's reading is long gone — a restart — and the one it starts
	// reads the same stream under a build that derives it differently.
	host.Forget("t1", rig.launch)
	rig.runner.Rewrite("FIRST\nSECOND\nthird\n")
	got := view.take(t, rig.tail(t, reader, view.cursor))
	if !got.Reset || view.text != "FIRST\nSECOND\nthird\n" {
		t.Errorf("after a restart onto another derivation = %+v; want a reset", got)
	}
}

// A VIEWER TOO FAR BEHIND IS RESET to the record's bound, in whole lines: more
// than it may hold was written since it last asked, and a delta would make it
// hold more than the record will.
func TestAViewerTooFarBehindIsReset(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	feeds := rig.serve(t, boxHost)
	reader := rig.reader(t)
	rig.runner.Say(SourceTranscript, "start\n")
	var view held
	view.take(t, rig.tail(t, reader, view.cursor))

	line := strings.Repeat("x", 99) + "\n"
	for range (MaxRunTextBytes / len(line)) + 50 {
		rig.runner.Say(SourceTranscript, line)
	}
	waitPastReuse(feeds)
	got := view.take(t, rig.tail(t, reader, view.cursor))
	if !got.Reset || len(got.Text) > MaxRunTextBytes || !strings.HasPrefix(got.Text, "xxx") ||
		strings.Contains(got.Text, "start") || !got.Cut || got.Start == 0 {
		t.Errorf("a viewer far behind = reset %v, %d bytes from %d, cut %v; want the last %d in whole lines",
			got.Reset, len(got.Text), got.Start, got.Cut, MaxRunTextBytes)
	}
}

// N VIEWERS COST ONE READ OF THE BOX: requests arriving together share the one
// read in flight, and requests inside [LiveReuse] of it share its answer.
//
// EVERY VIEWER WAITS ON THE ONE READ before it is let through: synctest.Wait
// returns once every goroutine of the case is blocked, so each request has
// reached the host and waits on the read in flight. A nap for a head start
// would let a late viewer ride the reuse window and leave the in-flight
// sharing untested.
//
// Mutation: read the box per request, or start a read for a request that
// finds one in flight, and the count is the viewers'.
func TestManyViewersCostOneReadOfTheBox(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newTailRig(t)
		rig.runner.Say(SourceTranscript, "busy\n")
		gate := make(chan struct{})
		rig.runner.LiveGate = gate
		rig.serve(t, boxHost)
		reader := rig.reader(t)

		const viewers = 8
		var wg sync.WaitGroup
		answers := make(chan TailAnswer, viewers)
		for range viewers {
			wg.Go(func() {
				got, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch, Cursor: TailCursor{}})
				if err != nil {
					t.Errorf("Tail: %v", err)
				}
				answers <- got
			})
		}
		synctest.Wait()
		if began := rig.runner.Waiting(); began != 1 {
			t.Errorf("%d waiting viewers began %d reads of the box; want the one they share", viewers, began)
		}
		close(gate)
		wg.Wait()
		close(answers)
		for got := range answers {
			if got.Output == nil || got.Output.Text != "busy\n" {
				t.Errorf("a viewer was answered %+v; want the shared read", got.Output)
			}
		}
		if reads := rig.runner.Reads(); reads != 1 {
			t.Errorf("%d viewers cost %d reads of the box; want one", viewers, reads)
		}
	})
}

// AN ABANDONED REQUEST FREES ITS ANSWER SLOT. The host waits for the box only
// its share of the asker's budget, then says the read is still going; the read
// goes on for the reading, so the next request is answered from it unread.
//
// Put to the host's own handler, since the host answers at three quarters of
// the budget and across the broker a loaded run's reply can miss the last
// quarter and read as a silent host. In a bubble, with the box's read held on
// the gate, the answer comes at exactly its timer's share, where a wall-clock
// bound would pass a host that overstays and fail one a loaded runner delays.
//
// Mutation: answer under the registration's own context, and the answer waits
// as long as the box does.
func TestAnAbandonedRequestFreesItsAnswerSlot(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newTailRig(t)
		rig.runner.Say(SourceTranscript, "slow\n")
		gate := make(chan struct{})
		rig.runner.LiveGate = gate
		host := &capturingServer{TailServer: rig.client(t)}
		rig.serveWith(t, boxHost, host, rig.feeds(t))

		ask := tailRequest{
			Version: tailWireVersion, TurnID: "t1", LaunchID: rig.launch, Node: boxHost,
			Cursor: TailCursor{}, BudgetMS: 400,
		}
		req, err := json.Marshal(ask)
		if err != nil {
			t.Fatal(err)
		}
		answered := make(chan []byte, 1)
		began := time.Now()
		go func() {
			raw, err := host.handler(t.Context(), req)
			if err != nil {
				t.Errorf("the host's handler: %v", err)
			}
			answered <- raw
		}()
		var raw []byte
		select {
		case raw = <-answered:
		case <-time.After(5 * time.Second):
			close(gate)
			t.Fatal("the host's answer waited on a box that never answered; want its share of the budget")
		}
		if waited := time.Since(began); waited != ask.answerBudget() {
			t.Errorf("the host answered after %v on a 400 ms budget; want its share, %v", waited, ask.answerBudget())
		}
		var rep tailReply
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Failed != TailFailure(WhyProviderUnavailable) {
			t.Fatalf("the host's answer on a slow box = %+v; want the box not answering yet", rep)
		}
		close(gate)
		eventually(t, func() bool { return rig.runner.Reads() == 1 }, "the read did not go on")
		got := rig.tail(t, rig.reader(t), TailCursor{})
		if got.Output == nil || got.Output.Text != "slow\n" || rig.runner.Reads() != 1 {
			t.Errorf("the next request = %+v after %d reads; want what the read found, read once",
				got.Output, rig.runner.Reads())
		}
	})
}

// A REQUEST ITS CALLER CANCELLED IS NO CLASS, the box's read still in flight:
// a drain, a lost lease or a stopped turn is the caller's own act, and classed,
// a reader that cannot see the context would send an operator to the provider.
// A request its own bound ended, on the same read, is the box not answering yet.
//
// IN A BUBBLE, with the box's read held on the gate, so the cancel lands while
// the read is in flight and the bound runs out on its timer alone.
//
// Mutation: drop the caller's-cancel exception in unanswered, and the cancelled
// request is classed as the provider's; mark nothing there, and the bounded one
// is not.
func TestARequestItsCallerCancelledIsNoClass(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newTailRig(t)
		rig.runner.Say(SourceTranscript, "slow\n")
		gate := make(chan struct{})
		rig.runner.LiveGate = gate
		host := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: rig.feeds(t), Condition: isCondition}
		q := TailQuery{TurnID: "t1", LaunchID: rig.launch}

		ctx, cancel := context.WithCancel(t.Context())
		failed := make(chan error, 1)
		go func() {
			_, err := host.Tail(ctx, q)
			failed <- err
		}()
		synctest.Wait()
		cancel()
		err := <-failed
		if why, classed := WhyOf(err); classed || !errors.Is(err, context.Canceled) {
			t.Errorf("a request its caller cancelled was classed %q (%v): %v; want no class, and "+
				"the cancel", why, classed, err)
		}

		_, err = host.Tail(t.Context(), q)
		if why, _ := WhyOf(err); why != WhyProviderUnavailable || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a request its own bound ended was classed %q: %v; want the provider not "+
				"answering yet", why, err)
		}
		if n := rig.runner.Waiting(); n != 1 {
			t.Errorf("%d reads of the box were in flight through both requests; want the one", n)
		}
		close(gate)
	})
}

// THE HOST ANSWERS INSIDE THE BUDGET ITS ASKER SENT, and inside the fleet read
// budget for a request that names none — the zero value's meaning.
func TestTheHostAnswersInsideTheAskersBudget(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		budgetMS int64
		want     time.Duration
	}{
		"an asker's own": {budgetMS: 1000, want: 750 * time.Millisecond},
		"none named":     {budgetMS: 0, want: time.Duration(float64(TailReadBudget) * answerShare)},
	} {
		if got := (tailRequest{BudgetMS: c.budgetMS}).answerBudget(); got != c.want {
			t.Errorf("%s: answer budget = %v; want %v", name, got, c.want)
		}
	}
}

// EVERY OUTCOME IS ONE THIS BUILD NAMES, and the launching one is among them.
func TestEveryTailOutcomeIsValid(t *testing.T) {
	t.Parallel()
	for _, o := range TailOutcomes {
		if !o.Valid() {
			t.Errorf("%q is not valid", o)
		}
	}
	if !slices.Contains(TailOutcomes, TailLaunching) || TailOutcome("stopped").Valid() {
		t.Error("the outcome set is not the one this build answers")
	}
}

// eventually waits for done, failing the case past a few seconds.
func eventually(t *testing.T, done func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// THE HOST LOGS WHAT IT MET, with its text, the one place the text goes: at
// WARN for a fault or a class no wait clears, at DEBUG for one a wait clears.
// ONE EVENT, `sandbox_tail_failed`, whether the host serves a peer or answers
// for itself (every single-node deployment). What reaches the query surface is
// marked as logged there, so its own line is a trace. A class a newer host
// named that this build doesn't know is said by the asker.
//
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: drop the log line from ServeTail's failure or from the host's own
// read, log a waiting class at WARN, leave the local failure unmarked, or say
// nothing of an unknown class, and a check goes red.
func TestTheHostLogsATailFailureByWhatClearsIt(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelDebug, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	lineAfter := func(before int, msg string) string {
		for line := range strings.SplitSeq(out.String()[before:], "\n") {
			if strings.Contains(line, "msg="+msg) {
				return line
			}
		}
		return ""
	}
	for name, c := range map[string]struct {
		read    error
		failure TailFailure
		level   string
	}{
		"a fault":                      {fmt.Errorf("read: %w", errBoxHost), TailFailedFault, "level=WARN"},
		"a configuration":              {Because(WhyConfiguration, errBoxHost), TailFailure(WhyConfiguration), "level=WARN"},
		"a provider that waits":        {Because(WhyProviderUnavailable, errBoxHost), TailFailure(WhyProviderUnavailable), "level=DEBUG"},
		"a store the host can't reach": {fmt.Errorf("read: %w: %w", coord.ErrUnavailable, errBoxHost), TailFailedUnanswered, "level=DEBUG"},
	} {
		rig := newTailRig(t)
		rig.runner.LiveErr = c.read
		for how, ask := range map[string]func(){
			"serving a peer": func() {
				host := &capturingServer{TailServer: rig.client(t)}
				rig.serveWith(t, boxHost, host, rig.feeds(t))
				if _, err := host.handler(t.Context(), tailRequestTo(t, rig, boxHost)); err != nil {
					t.Fatalf("%s: the host's handler: %v", name, err)
				}
			},
			"answering for itself": func() {
				local := &TailReader{Node: boxHost, Pending: rig.pending, Feeds: rig.feeds(t),
					Condition: isCondition}
				_, err := local.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
				var logged interface{ LoggedAtSource() bool }
				if !errors.As(err, &logged) || !logged.LoggedAtSource() {
					t.Errorf("%s, %s: the host's own failure %v reaches the query surface "+
						"unmarked as logged", name, "answering for itself", err)
				}
			},
		} {
			before := len(out.String())
			ask()
			line := lineAfter(before, "sandbox_tail_failed")
			for _, want := range []string{c.level, "failure=" + string(c.failure), "runtime.example.com"} {
				if !strings.Contains(line, want) {
					t.Errorf("%s, %s: logged %q; want %s in it", name, how, line, want)
				}
			}
		}
	}

	// A CLASS A NEWER HOST NAMED is said by the asker, then answered as a
	// fault.
	rig := newTailRig(t)
	newer := rig.client(t)
	stop, err := newer.Serve(t.Context(), topics.ObserveExecutorTail, queue.ConcurrentAnswers,
		func(context.Context, []byte) ([]byte, error) {
			return json.Marshal(tailReply{Version: tailWireVersion, Node: boxHost,
				Failed: "a_class_from_a_newer_build"})
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	before := len(out.String())
	_, err = rig.reader(t).Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
	if _, classed := WhyOf(err); err == nil || classed || errors.Is(err, ErrTailUnanswered) {
		t.Errorf("an unknown class answered %v; want a fault", err)
	}
	if line := lineAfter(before, "sandbox_tail_failure_unknown"); !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, "a_class_from_a_newer_build") {
		t.Errorf("an unknown class was said as %q; want a WARN naming it", line)
	}
}

// A BOX IS READ ON ITS HOST, whoever holds the run's seat: the seat moved while
// the job worked, and the box is a process tree or a container on the node that
// launched it, which no other node can reach. So the request is addressed to
// that node, and the seat's holder — which could not read the box — is not
// asked.
//
// Mutation: address the tail to the seat's holder, and the holder answers for
// a box it cannot reach while the host is never asked.
func TestALocalBoxsTailIsReadOnTheNodeThatLaunchedIt(t *testing.T) {
	t.Parallel()
	rig := newTailRigOn(t, "n3")
	rig.runner.Say(SourceTranscript, "running go test ./...\n")
	holder := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, nodeOf(seatHolder), holder, rig.feeds(t))
	rig.serve(t, "n3")

	got := rig.tail(t, rig.reader(t), TailCursor{})
	if got.Outcome != TailRunning || got.Output == nil || got.Node != "n3" {
		t.Fatalf("answer = %+v; want the tail read on n3, the box's host", got)
	}
	if got.Output.Text != "running go test ./...\n" {
		t.Errorf("the tail is %q; want the box's own output", got.Output.Text)
	}

	// THE SEAT'S HOLDER, handed the same request, answers nothing.
	if reply, err := holder.handler(t.Context(), tailRequestTo(t, rig, "n3")); !errors.Is(err, errTailNotAddressed) ||
		reply != nil {
		t.Errorf("the seat's holder answered a request for n3's box: %q, %v", reply, err)
	}

	// And the box's own node answers it from its own readings.
	local := &TailReader{Node: "n3", Pending: rig.pending, Feeds: rig.feeds(t), Condition: isCondition}
	if answer := rig.tail(t, local, TailCursor{}); answer.Outcome != TailRunning || answer.Node != "n3" {
		t.Errorf("the box's host answered %+v from its own readings; want its tail", answer)
	}
}

// A RUNNING ROW THAT NAMES NO LAUNCH NODE IS NO NODE'S TO READ in a fleet:
// every launch names its node with its box, so the row is one no host can be
// asked about, and is an error rather than a silence naming nobody. A node
// with no broker is the whole fleet, whose every box is its own, and reads it.
//
// Mutation: ask the broker for a nodeless row, and the answer is a silence
// naming no node; drop the no-broker fallback (`|| r.Queue == nil`), and a node
// with no broker errors on its own box.
func TestARunningRowNamingNoLaunchNodeIsNoNodesToRead(t *testing.T) {
	t.Parallel()
	rig := newTailRigOn(t, "")
	rig.runner.Say(SourceTranscript, "working\n")
	answer, err := rig.reader(t).Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
	if err == nil {
		t.Errorf("a running row naming no node was answered %+v; want an error", answer)
	}
	// A NODE WITH AN ID, as every engine's reader has, so the row reaches its
	// own readings through the no-broker branch alone, never by matching "".
	alone := &TailReader{Node: "n3", Pending: rig.pending, Feeds: rig.feeds(t), Condition: isCondition}
	if got := rig.tail(t, alone, TailCursor{}); got.Outcome != TailRunning || got.Output.Text != "working\n" {
		t.Errorf("a node with no broker answered %+v; want its own box's tail", got)
	}
}
