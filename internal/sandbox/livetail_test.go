package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
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

// fleetBuilds is what each incarnation's build advertises; one it does not
// name advertises everything this build does.
type fleetBuilds map[string][]coord.Feature

func (f fleetBuilds) OwnerFeatures(_ context.Context, owner string) ([]coord.Feature, error) {
	if features, known := f[owner]; known {
		return features, nil
	}
	return coord.Features, nil
}

// everyBuildServes is a fleet on this build.
var everyBuildServes = fleetBuilds{}

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
	if _, err := rig.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
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

// feeds is a node's readings, driving the rig's boxes.
func (rig *tailRig) feeds(t *testing.T) *LiveFeeds {
	t.Helper()
	feeds := NewLiveFeeds(t.Context(), LiveFeedsOptions{Manager: func() *Manager { return rig.manager }})
	t.Cleanup(feeds.Stop)
	return feeds
}

// serve makes an incarnation an answerer, with readings of its own.
func (rig *tailRig) serve(t *testing.T, owner string) *LiveFeeds {
	t.Helper()
	feeds := rig.feeds(t)
	rig.serveWith(t, owner, rig.client(t), feeds)
	return feeds
}

func (rig *tailRig) serveWith(t *testing.T, owner string, q TailServer, feeds *LiveFeeds) {
	t.Helper()
	stop, err := ServeTail(t.Context(), q, owner, rig.pending, feeds)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

// reader is the asking node's reader.
//
// ITS BUDGET IS ROOM TO ANSWER, not the case's clock: a scatter returns the
// moment its one reply lands, so a generous budget costs a passing case
// nothing, while the 200 ms it was flaked every answering case on a loaded
// race-enabled run — the owner's reply was simply later than that, and the
// case read it as a silent owner. The one case that waits the budget OUT sets
// its own ([TestASilentOwnerIsNamedNotEmpty]).
func (rig *tailRig) reader(t *testing.T, features TailFeatures) *TailReader {
	return &TailReader{
		Owner: askerOwner, Pending: rig.pending, Queue: rig.client(t),
		Features: features, Budget: 30 * time.Second,
		// The ASKER has no readings of its own: an answer that came from
		// here instead of from the owner would fail loudly.
	}
}

// tail asks for the rig's launch.
func (rig *tailRig) tail(t *testing.T, r *TailReader, cursor *TailCursor) TailAnswer {
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
	if a.Outcome != TailRunning || a.Output == nil || !a.Output.Cursor {
		t.Fatalf("answer = %+v; want a cursor-shaped tail", a)
	}
	out := a.Output
	if out.Start == nil || out.End == nil {
		t.Fatalf("a cursor answer without its offsets: start %v, end %v", out.Start, out.End)
	}
	if out.Reset {
		h.text = out.Text
	} else {
		if *out.Start != h.cursor.Offset {
			t.Fatalf("a delta starts at %d; the viewer holds through %d", *out.Start, h.cursor.Offset)
		}
		h.text += out.Text
	}
	h.cursor = TailCursor{Epoch: out.Epoch, Offset: *out.End, Digest: out.Digest}
	return out
}

// ONLY THE OWNER ANSWERS, and it answers with the box's own output.
//
// Every node serves the subject, so a second answerer — the node that asked,
// or any other — replying with ITS read of a box it does not drive would put a
// stranger's answer (or an empty one) first in a scatter that takes one reply.
func TestOnlyTheOwnerAnswersATailRequest(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "running go test ./...\n")
	// A bystander that serves too, and CAN read the box: it drives the same
	// backend, so the only thing keeping it from answering with its own read
	// is the addressing. Its handler is captured so the refusal is asserted
	// directly below — through the broker it would be a race the asker's
	// owner filter hides, and a test that catches the regression only when
	// the wrong node happens to answer first is not a test of it.
	bystander := &capturingServer{TailServer: rig.client(t)}
	rig.serveWith(t, "n3:bystander", bystander, rig.feeds(t))
	rig.serve(t, boxOwner)

	got := rig.tail(t, rig.reader(t, everyBuildServes), nil)
	if got.Outcome != TailRunning || got.Output == nil {
		t.Fatalf("outcome = %q with output %v; want the owner's tail", got.Outcome, got.Output)
	}
	if got.Output.Text != "running go test ./...\n" || got.Node != "n2" {
		t.Errorf("answer = %+v from %q; want the owner's box, named n2", got.Output, got.Node)
	}
	if rig.runner.Reads() != 1 {
		t.Errorf("the box was read %d times; want exactly once, by its owner", rig.runner.Reads())
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
	if rig.runner.Reads() != 1 {
		t.Errorf("the bystander read a box it does not own (%d reads)", rig.runner.Reads())
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
	rig.runner.Say(SourceTranscript, "never read\n")

	// Nobody answers, so this case waits the whole budget out: a short one.
	reader := rig.reader(t, everyBuildServes)
	reader.Budget = 200 * time.Millisecond
	got := rig.tail(t, reader, nil)
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
	got := rig.tail(t, rig.reader(t, fleetBuilds{boxOwner: {coord.FeatureSteer}}), nil)
	if got.Outcome != TailOwnerUpgrading || got.Node != "n2" {
		t.Errorf("answer = %+v; want owner_upgrading naming n2", got)
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("an owner that serves no tail was asked anyway (%d reads)", rig.runner.Reads())
	}
}

// A LAUNCH THAT IS NOT RUNNING IS SAID TO BE, with the record's own reason —
// and no box is touched to say it.
func TestATailOfAJobThatIsNotRunningSaysWhy(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
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
// cold template. Answered `not_running`, a screen opened in that window
// stopped asking for good and said the run was over before it had begun.
//
// Mutation: answer `launching` as `not_running`, and the outcome is terminal.
func TestALaunchingJobIsLaunchingNotStopped(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "t2", AgentHandle: "swe", CodingAgent: "claude-code", Placement: string(Direct),
	}, Fence{}); err != nil {
		t.Fatal(err)
	}
	launching, _, err := rig.pending.Get(t.Context(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	rig.serve(t, boxOwner)
	got, err := rig.reader(t, everyBuildServes).Tail(t.Context(),
		TailQuery{TurnID: "t2", LaunchID: launching.LaunchID})
	if err != nil || got.Outcome != TailLaunching || got.Status != StatusLaunching {
		t.Errorf("a launching job = %+v, %v; want the launching outcome", got, err)
	}
}

// THE OWNER'S OWN FAILURE IS AN ERROR, carrying its sentence — never a tail
// that is merely empty, and never "silent", since it answered.
func TestAnOwnerThatCannotReadTheBoxSaysSo(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.LiveErr = errFakeBox
	rig.serve(t, boxOwner)
	_, err := rig.reader(t, everyBuildServes).Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch})
	if err == nil || !strings.Contains(err.Error(), "n2 owns run t1") ||
		!strings.Contains(err.Error(), errFakeBox.Error()) {
		t.Errorf("Tail err = %v; want the owner's own failure, naming it", err)
	}
}

// A node that owns the run answers from its own box without a round trip.
func TestTheOwnerReadsItsOwnBoxDirectly(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceStderr, "local\n")
	reader := &TailReader{
		Owner: boxOwner, Pending: rig.pending, Queue: rig.client(t),
		Features: everyBuildServes, Feeds: rig.feeds(t),
	}
	got := rig.tail(t, reader, nil)
	if got.Outcome != TailRunning || got.Output.Text != "local\n" || got.Output.AsOf.IsZero() {
		t.Errorf("answer = %+v; want the local box's output, stamped", got)
	}
}

// A PEEK NEVER WAKES A BOX. A running record whose box is paused — the moment
// between a collection pausing the box and the record moving on — is answered
// as what it is, `box_paused`, and the box is left paused, on the owner's own
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
	rig.serve(t, boxOwner)
	local := &TailReader{Owner: boxOwner, Pending: rig.pending, Feeds: rig.feeds(t)}
	for name, reader := range map[string]*TailReader{
		"across the fleet": rig.reader(t, everyBuildServes),
		"on the owner":     local,
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

// A LAUNCH THAT STOPPED IS LET GO, on the owner's own read as on a peer's
// answer: nobody asks about it again, so its reading — up to [liveHold] of
// text — is dropped by the request that learned it stopped, not kept until
// some later request's idle sweep, which on a quiet node never comes.
//
// Mutation: drop either Forget, and that path's reading stays.
func TestAStoppedLaunchsReadingIsLetGo(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "working\n")
	served := rig.serve(t, boxOwner)
	own := rig.feeds(t)
	local := &TailReader{Owner: boxOwner, Pending: rig.pending, Feeds: own}
	remote := rig.reader(t, everyBuildServes)
	for name, reader := range map[string]*TailReader{"across the fleet": remote, "on the owner": local} {
		if got := rig.tail(t, reader, &TailCursor{}); got.Outcome != TailRunning {
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
	for name, reader := range map[string]*TailReader{"across the fleet": remote, "on the owner": local} {
		if got := rig.tail(t, reader, &TailCursor{}); got.Outcome != TailNotRunning {
			t.Fatalf("%s: answer = %+v; want not_running", name, got)
		}
	}
	// The asker reads the record first and answers not_running itself, so a
	// peer is told only by its own read; ask it the way an asker that read
	// the record a moment earlier would.
	raw, err := json.Marshal(tailRequest{Version: tailWireVersion, TurnID: "t1",
		LaunchID: rig.launch, Owner: boxOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client(t).Ask(t.Context(), topics.ObserveSandboxTail, raw, 1); err != nil {
		t.Fatal(err)
	}
	if readings(served) != 0 || readings(own) != 0 {
		t.Errorf("readings: served %d, own %d; want none once the launch stopped",
			readings(served), readings(own))
	}
}

// A NODE THAT STOPPED READS NO BOX. Its readings end before its seats are
// released and the boxes stop being its to read, and a request that lands in
// between is told the read failed rather than starting one.
//
// Mutation: leave the readings' context running at Stop, and the box is read.
func TestAStoppedNodeReadsNoBox(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "working\n")
	feeds := rig.feeds(t)
	feeds.Stop()
	local := &TailReader{Owner: boxOwner, Pending: rig.pending, Feeds: feeds}
	if got, err := local.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch}); err == nil {
		t.Errorf("answer = %+v; want the read's failure from a node that stopped", got)
	}
	if rig.runner.Reads() != 0 {
		t.Errorf("a stopped node read the box %d times", rig.runner.Reads())
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
// where the viewer holds through — never the window again.
//
// Mutation: answer every cursor with a reset, and the second answer re-sends
// the first line.
func TestAViewerIsSentOnlyWhatItLacks(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	feeds := rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
	rig.runner.Say(SourceTranscript, "[tool] bash: go test ./...\n")

	var view held
	first := view.take(t, rig.tail(t, reader, &view.cursor))
	if !first.Reset || view.text != "[tool] bash: go test ./...\n" {
		t.Fatalf("the first answer = %+v; want a reset carrying what there is", first)
	}
	rig.runner.Say(SourceTranscript, "the suite passed\n")
	waitPastReuse(feeds)
	second := view.take(t, rig.tail(t, reader, &view.cursor))
	if second.Reset || second.Text != "the suite passed\n" {
		t.Errorf("the second answer = %+v; want only the new line", second)
	}
	if view.text != "[tool] bash: go test ./...\nthe suite passed\n" {
		t.Errorf("the viewer holds %q; want both lines once each", view.text)
	}
	if second.WindowBytes != MaxRunTextBytes {
		t.Errorf("window_bytes = %d; want the record's bound, which a viewer holds", second.WindowBytes)
	}
}

// A CURSOR ANSWER ALWAYS STATES ITS OFFSETS, zero included, as the wire
// carries it. A reading that has settled nothing yet answers at end 0 and is
// followed from start 0; with the offsets dropped as empty, a screen compared a
// missing start with the 0 it held through, threw the delta away and asked for
// a reset a poll later — and a REST caller told to send `end` back had none. A
// window carries no cursor field at all.
//
// Mutation: tag the offsets `omitempty` on plain integers again, and the
// answers at zero carry neither.
func TestACursorAnswerAlwaysStatesItsOffsets(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
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
	first := rig.tail(t, reader, &view.cursor)
	view.take(t, first)
	next := rig.tail(t, reader, &view.cursor)
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

	window := wire(rig.tail(t, reader, nil))
	for _, key := range []string{"cursor", "epoch", "start", "end", "digest", "reset"} {
		if _, present := window[key]; present {
			t.Errorf("a window carries the cursor field %q: %v", key, window)
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
// or before its start. Each would otherwise append the reading's tail to text it does not follow.
//
// Mutation: skip the digest check, and a viewer holding different text of the
// same length is sent a delta.
func TestACursorThatDoesNotMatchIsAReset(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
	rig.runner.Say(SourceTranscript, "one\ntwo\n")
	var view held
	view.take(t, rig.tail(t, reader, &view.cursor))

	for name, cursor := range map[string]TailCursor{
		"another epoch":  {Epoch: "transcript@99", Offset: view.cursor.Offset, Digest: view.cursor.Digest},
		"another digest": {Epoch: view.cursor.Epoch, Offset: view.cursor.Offset, Digest: "0123456789abcdef"},
		"past the end":   {Epoch: view.cursor.Epoch, Offset: view.cursor.Offset + 10, Digest: view.cursor.Digest},
		// A peer's request is decoded off the wire, where nothing held the
		// offset to a whole number: a negative one is a reset, not a slice
		// out of range in the owner's answer.
		"before the start": {Epoch: view.cursor.Epoch, Offset: -1, Digest: view.cursor.Digest},
	} {
		got := rig.tail(t, reader, &cursor)
		if got.Output == nil || !got.Output.Reset || got.Output.Text != "one\ntwo\n" {
			t.Errorf("%s: answer = %+v; want a reset carrying the reading's text", name, got.Output)
		}
	}
}

// AN OWNER MOVE ON THE SAME BUILD CONTINUES, and one onto a build that derives
// the stream differently RESETS. The new owner's reading of the same stream
// from the same origin is the same text, which the digest proves; under a
// changed parser it is not, which the digest catches though the epoch and the
// offset both match.
func TestAnOwnerMoveContinuesOnTheSameBuildAndResetsOnAnother(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "first\n")
	const nextOwner = "n4:next"
	first := rig.serve(t, boxOwner)
	rig.serve(t, nextOwner)
	reader := rig.reader(t, everyBuildServes)
	var view held
	view.take(t, rig.tail(t, reader, &view.cursor))

	if ok, err := rig.pending.ClaimOwnership(t.Context(), "t1", nextOwner, 2); err != nil || !ok {
		t.Fatalf("move the run: %v %v", ok, err)
	}
	rig.runner.Say(SourceTranscript, "second\n")
	moved := view.take(t, rig.tail(t, reader, &view.cursor))
	if moved.Reset || moved.Text != "second\n" {
		t.Errorf("after a move on the same build = %+v; want only the new line", moved)
	}

	if ok, err := rig.pending.ClaimOwnership(t.Context(), "t1", boxOwner, 3); err != nil || !ok {
		t.Fatalf("move the run back: %v %v", ok, err)
	}
	// The first owner's reading is long gone — a restart — and the one it
	// starts reads the same stream under a build that derives it
	// differently.
	first.Forget("t1", rig.launch)
	rig.runner.Rewrite("FIRST\nSECOND\nthird\n")
	got := view.take(t, rig.tail(t, reader, &view.cursor))
	if !got.Reset || view.text != "FIRST\nSECOND\nthird\n" {
		t.Errorf("after a move onto another derivation = %+v; want a reset", got)
	}
}

// AN OWNER THAT READS NO CURSOR IS ASKED FOR ITS WINDOW. An older build ignores
// a cursor and answers its window, which a viewer reading by cursor would
// append to what it holds as though it followed it; so the cursor is sent only
// to an owner that advertises it, and the answer from any other is a window —
// carrying no cursor of its own, so the screen knows to replace.
//
// Mutation: send the cursor regardless, and the request the old owner sees
// carries one.
func TestAnOwnerThatReadsNoCursorIsAskedForItsWindow(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "a line\n")
	seen := &recordingServer{TailServer: rig.client(t)}
	rig.serveWith(t, boxOwner, seen, rig.feeds(t))
	older := fleetBuilds{boxOwner: {coord.FeatureSandboxTail}}

	got := rig.tail(t, rig.reader(t, older), &TailCursor{})
	if got.Output == nil || got.Output.Cursor || got.Output.Text != "a line\n" {
		t.Errorf("an older owner's answer = %+v; want its window, cursorless", got.Output)
	}
	if req := seen.last(); req.Cursor != nil {
		t.Errorf("an owner that reads no cursor was sent one: %+v", req.Cursor)
	}
}

// recordingServer keeps every request a node was asked.
type recordingServer struct {
	TailServer
	mu   sync.Mutex
	reqs []tailRequest
}

func (s *recordingServer) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return s.TailServer.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req tailRequest
		if json.Unmarshal(raw, &req) == nil {
			s.mu.Lock()
			s.reqs = append(s.reqs, req)
			s.mu.Unlock()
		}
		return h(ctx, raw)
	})
}

func (s *recordingServer) last() tailRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

// A REQUEST WITH NO CURSOR IS ANSWERED THE WINDOW IT ALWAYS WAS: the last
// [MaxLiveOutputBytes] in whole lines, cut where earlier output exists, and no
// cursor field at all — what an older asker, and the REST route's existing
// callers, replace their screen with on every poll.
func TestARequestWithNoCursorIsAnsweredTheWindow(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	for i := range 400 {
		rig.runner.Say(SourceTranscript, fmt.Sprintf("[tool] bash: step %03d of a long run\n", i))
	}
	rig.serve(t, boxOwner)
	got := rig.tail(t, rig.reader(t, everyBuildServes), nil).Output
	if got.Cursor || got.Epoch != "" || got.Reset {
		t.Errorf("a cursorless answer carries cursor fields: %+v", got)
	}
	if len(got.Text) > MaxLiveOutputBytes || !got.Cut || !strings.HasPrefix(got.Text, "[tool] bash: step") ||
		!strings.HasSuffix(got.Text, "step 399 of a long run\n") {
		t.Errorf("the window is %d bytes, cut=%v, %.30q…; want the last 8 KiB in whole lines, cut",
			len(got.Text), got.Cut, got.Text)
	}
	if got.WindowBytes != MaxLiveOutputBytes {
		t.Errorf("window_bytes = %d; want %d", got.WindowBytes, MaxLiveOutputBytes)
	}
}

// A VIEWER TOO FAR BEHIND IS RESET to the record's bound, in whole lines: more
// than it may hold was written since it last asked, and a delta would make it
// hold more than the record will.
func TestAViewerTooFarBehindIsReset(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	feeds := rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
	rig.runner.Say(SourceTranscript, "start\n")
	var view held
	view.take(t, rig.tail(t, reader, &view.cursor))

	line := strings.Repeat("x", 99) + "\n"
	for range (MaxRunTextBytes / len(line)) + 50 {
		rig.runner.Say(SourceTranscript, line)
	}
	waitPastReuse(feeds)
	got := view.take(t, rig.tail(t, reader, &view.cursor))
	if !got.Reset || len(got.Text) > MaxRunTextBytes || !strings.HasPrefix(got.Text, "xxx") ||
		strings.Contains(got.Text, "start") || !got.Cut || *got.Start == 0 {
		t.Errorf("a viewer far behind = reset %v, %d bytes from %d, cut %v; want the last %d in whole lines",
			got.Reset, len(got.Text), *got.Start, got.Cut, MaxRunTextBytes)
	}
}

// N VIEWERS COST ONE READ OF THE BOX. A read used to be made per request, so
// every viewer added a whole read of the box every three seconds; requests
// arriving together now share the one read in flight, and requests inside
// [LiveReuse] of it share its answer.
//
// Mutation: read the box per request, and the count is the viewers'.
func TestManyViewersCostOneReadOfTheBox(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "busy\n")
	gate := make(chan struct{})
	rig.runner.LiveGate = gate
	rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)

	const viewers = 8
	var wg sync.WaitGroup
	answers := make(chan TailAnswer, viewers)
	for range viewers {
		wg.Go(func() {
			got, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch, Cursor: &TailCursor{}})
			if err != nil {
				t.Errorf("Tail: %v", err)
			}
			answers <- got
		})
	}
	// Every viewer is waiting on the one read before it is let through.
	eventually(t, func() bool { return rig.runner.Waiting() > 0 }, "no read began")
	time.Sleep(50 * time.Millisecond)
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
}

// AN ABANDONED REQUEST FREES ITS ANSWER SLOT. The owner's answer waits for the
// box only its share of the asker's budget, and then says the read is still
// going — and the read goes on, for the reading rather than the request, so
// the next request is answered from what it found without reading again.
//
// Mutation: answer under the registration's own context, and the request
// waits as long as the box does.
func TestAnAbandonedRequestFreesItsAnswerSlot(t *testing.T) {
	t.Parallel()
	rig := newTailRig(t)
	rig.runner.Say(SourceTranscript, "slow\n")
	gate := make(chan struct{})
	rig.runner.LiveGate = gate
	rig.serve(t, boxOwner)
	reader := rig.reader(t, everyBuildServes)
	reader.Budget = 400 * time.Millisecond

	began := time.Now()
	_, err := reader.Tail(t.Context(), TailQuery{TurnID: "t1", LaunchID: rig.launch, Cursor: &TailCursor{}})
	if err == nil || !strings.Contains(err.Error(), "took longer than this request could wait") {
		t.Fatalf("a request on a slow box = %v; want the owner saying the read is still going", err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the request waited %v on a box that never answered; want its budget", waited)
	}
	close(gate)
	eventually(t, func() bool { return rig.runner.Reads() == 1 }, "the read did not go on")
	reader.Budget = 30 * time.Second
	got := rig.tail(t, reader, &TailCursor{})
	if got.Output == nil || got.Output.Text != "slow\n" || rig.runner.Reads() != 1 {
		t.Errorf("the next request = %+v after %d reads; want what the read found, read once",
			got.Output, rig.runner.Reads())
	}
}

// THE OWNER ANSWERS INSIDE THE BUDGET ITS ASKER SENT, falling back to the fleet
// read budget for an asker on a build that sent none.
func TestTheOwnerAnswersInsideTheAskersBudget(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		budgetMS int64
		want     time.Duration
	}{
		"an asker's own": {budgetMS: 1000, want: 750 * time.Millisecond},
		"an older asker": {budgetMS: 0, want: time.Duration(float64(TailReadBudget) * answerShare)},
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
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errFakeBox = fakeBoxError("the box stopped answering")

type fakeBoxError string

func (e fakeBoxError) Error() string { return string(e) }
