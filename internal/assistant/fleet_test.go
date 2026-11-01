package assistant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// present is the fleet's roster: the nodes named are present.
type present map[string]bool

func (p present) Present(_ context.Context, node string) (bool, error) { return p[node], nil }

func (p present) Nodes(context.Context) ([]string, error) {
	var out []string
	for n, ok := range p {
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// fleetOf is a fleet of two rigs, node-a and node-b, on one in-memory broker
// and one replicated store, each with its fleet half started over its service.
func fleetOf(t *testing.T, a, b evals.RigOptions) (*assistanttest.Rig, *assistanttest.Rig) {
	t.Helper()
	broker := queuememory.NewBroker()
	roster := present{"node-a": true, "node-b": true}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	ring := statelog.OneKey("k1", "fleet-material")
	half := func(node string) *live.Fleet {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		frames, err := signedask.NewOneWay[live.Sealed](live.FrameChannel, ring)
		if err != nil {
			t.Fatal(err)
		}
		replays, err := signedask.New[live.ReplayRequest, live.ReplayReply](live.ReplayChannel, ring)
		if err != nil {
			t.Fatal(err)
		}
		stops, err := signedask.New[live.StopRequest, live.StopReply](live.StopChannel, ring)
		if err != nil {
			t.Fatal(err)
		}
		f, err := live.NewFleet(live.FleetOptions{Node: node, Broker: q, Roster: roster,
			Cipher: cipher, Frames: frames, Replays: replays, Stops: stops})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	fa, fb := half("node-a"), half("node-b")
	a.Node, a.Remote = "node-a", assistant.FleetRemote(fa)
	ra := assistanttest.NewRig(t, a)
	b.Node, b.Remote, b.Store = "node-b", assistant.FleetRemote(fb), ra.Store
	rb := assistanttest.NewRig(t, b)
	for _, n := range []struct {
		f   *live.Fleet
		svc *assistant.Service
	}{{fa, ra.Svc}, {fb, rb.Svc}} {
		if err := n.f.Start(t.Context(), n.svc); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := n.f.Close(context.Background()); err != nil {
				t.Errorf("close the fleet half: %v", err)
			}
		})
	}
	return ra, rb
}

// stream follows a turn on rig r from after seq after, in the background.
func stream(t *testing.T, r *assistanttest.Rig, turn uuid.UUID, after uint64) (*evals.Collector,
	<-chan error) {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	c := &evals.Collector{}
	done := make(chan error, 1)
	go func() {
		defer cancel()
		err := r.Svc.Stream(ctx, r.Person.ID, r.Conversation, turn, after, c)
		if err == nil && ctx.Err() != nil {
			err = errors.New("the stream never ended")
		}
		done <- err
	}()
	return c, done
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A STREAM RESUMES ON ANOTHER NODE: a client whose connection moved to a node
// not running its turn draws every frame after its Last-Event-ID, exactly as
// the running node numbered them, and the turn's end.
//
// Mutation: drop the replay's frames in live.Fleet.catchUp (write none) and
// the resumed stream misses the frames appended before it followed.
func TestAStreamResumesOnAnotherNode(t *testing.T) {
	t.Parallel()
	a, b := fleetOf(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("The signing keys rotate on Friday.")}}, evals.RigOptions{})
	a.Model.Gate = make(chan struct{})
	accepted, err := a.Ask("resume", "When do the keys rotate?", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The client drew seq 1 on node-a, then reconnected to node-b.
	c, done := stream(t, b, accepted.TurnID, 1)
	waitUntil(t, func() bool { return len(c.Frames()) >= 2 })
	close(a.Model.Gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := a.Follow(accepted.TurnID, 1)
	got := c.Frames()
	if len(got) != len(want) {
		t.Fatalf("node-b drew %d frames after seq 1, the runner numbered %d: %v", len(got),
			len(want), kinds(got))
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || got[i].Kind != want[i].Kind {
			t.Fatalf("frame %d is %d %s on node-b and %d %s on the runner", i, got[i].Seq,
				got[i].Kind, want[i].Seq, want[i].Kind)
		}
	}
	if d := evals.Last(got, wire.EventDone); d == nil || d.Done.State != wire.TurnCompleted {
		t.Fatalf("node-b's stream ended %v, want the completed turn", kinds(got))
	}
}

// STOP REACHES THE RUNNING NODE: a Stop pressed on a node not running the turn
// stops it where it runs, and the running node refuses a Stop for a person
// whose turn it is not, stopping nothing.
//
// Mutation: drop the owner check in Service.StopHere and another person's
// stop ends the turn.
func TestStopReachesTheRunningNode(t *testing.T) {
	t.Parallel()
	a, b := fleetOf(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("Never said.")}}, evals.RigOptions{})
	a.Model.Gate = make(chan struct{})
	accepted, err := a.Ask("stop", "Who works here?", nil)
	if err != nil {
		t.Fatal(err)
	}
	stranger := evals.Person("ada", "ada", iam.GrantWorklet)
	if _, err := b.Svc.Stop(t.Context(), stranger.ID, accepted.TurnID,
		evals.Key("stranger")); !errors.Is(err, assistant.ErrNotFound) {
		t.Fatalf("another person's stop on node-b: err = %v, want ErrNotFound", err)
	}
	if _, err := a.Svc.StopHere(t.Context(), stranger.ID, accepted.TurnID); !errors.Is(err,
		live.ErrNotYours) {
		t.Fatalf("the runner asked to stop another person's turn: err = %v, want ErrNotYours", err)
	}
	if row, _ := a.Store.Turn(t.Context(), a.Person.ID, accepted.TurnID); row.State != wire.TurnRunning {
		t.Fatalf("another person's stop left the turn %s, want running", row.State)
	}
	answer, err := b.Svc.Stop(t.Context(), b.Person.ID, accepted.TurnID, evals.Key("stopping"))
	if err != nil || answer.State != wire.TurnStopped {
		t.Fatalf("the owner's stop on node-b = %+v, %v; want stopped", answer, err)
	}
	waitUntil(t, func() bool {
		row, _ := a.Store.Turn(t.Context(), a.Person.ID, accepted.TurnID)
		return row.State == wire.TurnStopped
	})
}

// AN INTERRUPTED TURN IS CLOSED ONCE: two nodes finding at once that a turn's
// runner left the fleet both answer it interrupted, under ONE operation every
// node derives alike, and its end is recorded once.
//
// Mutation: derive the close's operation from the noticing node (add the node
// to Derivation.Identity in closeInterrupted) and the two nodes publish two
// operations for one end.
func TestAnInterruptedTurnIsClosedOnce(t *testing.T) {
	t.Parallel()
	a, b := fleetOf(t, evals.RigOptions{}, evals.RigOptions{})
	turn := orphan(t, a)
	var wg sync.WaitGroup
	streams := make([]*evals.Collector, 2)
	errs := make([]error, 2)
	for i, r := range []*assistanttest.Rig{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, done := stream(t, r, turn, 0)
			streams[i], errs[i] = c, <-done
		}()
	}
	wg.Wait()
	for i, c := range streams {
		if errs[i] != nil {
			t.Fatalf("stream %d: %v", i, errs[i])
		}
		d := evals.Last(c.Frames(), wire.EventDone)
		if d == nil || d.Done.State != wire.TurnInterrupted {
			t.Fatalf("stream %d ended %v, want the turn interrupted", i, kinds(c.Frames()))
		}
	}
	if n := a.Store.Closes(turn); n != 1 {
		t.Fatalf("the turn's end was recorded %d times, want once", n)
	}
	var ops []string
	for _, raw := range a.Store.Records() {
		var rec struct {
			Record string          `json:"record"`
			Write  assistant.Write `json:"write"`
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Record == "turn_closed" && rec.Write.Derived != nil {
			id, err := json.Marshal(rec.Write.Derived)
			if err != nil {
				t.Fatal(err)
			}
			ops = append(ops, string(id))
		}
	}
	if len(ops) == 0 || strings.Join(ops, "") != strings.Repeat(ops[0], len(ops)) {
		t.Fatalf("the nodes closed the turn under operations %v, want one they all derive", ops)
	}
}

// counted is the fleet's turn counter, as every node of the suite shares it,
// with the buckets it was asked about.
type counted struct {
	*memory.Fleet
	mu      sync.Mutex
	buckets []string
	err     error
}

func (c *counted) Allow(ctx context.Context, bucket string, limit int, window time.Duration,
	now time.Time) (bool, error) {

	c.mu.Lock()
	if strings.HasPrefix(bucket, "assistant.turn:") {
		// Only the turn rate is this suite's; a stream's opens are counted
		// on their own buckets.
		c.buckets = append(c.buckets, bucket)
	}
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return false, err
	}
	return c.Fleet.Allow(ctx, bucket, limit, window, now)
}

// THE TURN RATE IS COUNTED ACROSS THE FLEET: a person's starts on every node
// share one count, so spreading questions over the fleet does not multiply
// their allowance.
//
// Mutation: count per node (put the node in the bucket's name) and node-b
// starts a turn past the person's minute.
func TestTheTurnRateIsCountedAcrossTheFleet(t *testing.T) {
	t.Parallel()
	counter := &counted{Fleet: memory.NewFleet()}
	a := assistanttest.NewRig(t, evals.RigOptions{Node: "node-a", Rate: counter,
		Rounds: []evals.Round{evals.Say("ok")}})
	b := assistanttest.NewRig(t, evals.RigOptions{Node: "node-b", Rate: counter,
		Store: a.Store})
	accepted, err := a.Ask("first", "Hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Follow(accepted.TurnID, 0)
	if len(counter.buckets) != 1 {
		t.Fatalf("one start was counted %d times on the fleet", len(counter.buckets))
	}
	// The rest of the person's minute, spent on other nodes.
	for range assistant.TurnsPerMinute - 1 {
		if ok, err := counter.Fleet.Allow(t.Context(), counter.buckets[0],
			assistant.TurnsPerMinute, time.Minute, b.Clock.Now()); err != nil || !ok {
			t.Fatalf("the fleet refused a start inside the minute: %v %v", ok, err)
		}
	}
	_, err = b.Ask("over", "Hi", nil)
	busy, ok := errorAs[*assistant.Busy](err)
	if !ok || busy.RetryAfter <= 0 || busy.RetryAfter > time.Minute {
		t.Fatalf("a start past the person's minute on another node: err = %v, want Busy "+
			"with a wait inside the minute", err)
	}
	if len(counter.buckets) != 2 || counter.buckets[1] != counter.buckets[0] {
		t.Fatalf("the two nodes counted the person in buckets %v, want one", counter.buckets)
	}
}

// A FLEET COUNT THAT CANNOT BE READ FALLS BACK TO THE NODE'S OWN RATE: a
// person's question is not refused for a store the engine cannot reach, and
// the node's own limit still holds them (never no limit at all).
//
// Mutation: leave the node's own admission to the fleet's count where one is
// wired (skip admitHere when Rate is set) and the burst passes.
func TestATurnRateTheFleetCannotCountFallsBackToTheNode(t *testing.T) {
	t.Parallel()
	counter := &counted{Fleet: memory.NewFleet(), err: errors.New("the store is away")}
	var rounds []evals.Round
	for range assistant.TurnBurst {
		rounds = append(rounds, evals.Say("ok"))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Rate: counter, Rounds: rounds})
	for i := range assistant.TurnBurst {
		accepted, err := f.Ask("burst"+string(rune('a'+i)), "Hi", nil)
		if err != nil {
			t.Fatalf("turn %d on an unreadable fleet count: %v", i, err)
		}
		f.Follow(accepted.TurnID, 0)
	}
	if _, err := f.Ask("over", "Hi", nil); !errors.Is(err, assistant.ErrBusy) {
		t.Fatalf("a turn past the node's burst with the fleet count away: err = %v, want "+
			"ErrBusy", err)
	}
}

// orphan is a turn of r's person recorded running on a node that has left the
// fleet.
func orphan(t *testing.T, r *assistanttest.Rig) uuid.UUID {
	t.Helper()
	turn := evals.ID("turn/orphan")
	if _, err := r.Store.OpenTurn(t.Context(), assistant.Write{Owner: r.Person.ID,
		By: iam.ActorFor(r.Person), Key: evals.Key("orphan")}, assistantdomain.NewTurn{
		Conversation: r.Conversation, Turn: turn, Node: "node-gone",
		PromptVersion: assistant.PromptVersion}); err != nil {
		t.Fatal(err)
	}
	return turn
}

// proposing is a rig whose turn proposes a task and parks on it, and whose
// resumed segment then answers.
func proposing(t *testing.T) evals.RigOptions {
	return evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "create_task")},
		Acts:  []string{"create_work_item"},
		Rounds: []evals.Round{
			evals.Call("c1", "create_task", map[string]any{"title": "Rotate keys"}),
			evals.Say("Created it.")}}
}

// A RESUMED TURN CONTINUES ITS NUMBERING ON ANOTHER NODE: a turn that parked
// on one node and was approved on another numbers its next segment after
// every frame the first node told, so a drawer, which drops a frame numbered
// at or below the last it drew, draws every frame of the new segment, and a
// stream of it on the first node follows the node now running it rather than
// ending on the parked segment it still holds.
//
// Mutation: open the resumed segment with no floor (pass 0 to openStream in
// Resume) and node-b numbers it from 1, every frame of it at or below what
// the drawer already drew.
func TestAResumedTurnContinuesItsNumberingOnAnotherNode(t *testing.T) {
	t.Parallel()
	opts := proposing(t)
	a, b := fleetOf(t, opts, evals.RigOptions{Tools: opts.Tools, Acts: opts.Acts,
		Rounds: []evals.Round{evals.Say("Created it.")}})
	accepted, err := a.Ask("propose", "File a task to rotate keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	parked := evals.Last(a.Follow(accepted.TurnID, 0), wire.EventDone)
	if parked == nil || parked.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing turn ended %+v, want waiting_for_approval", parked)
	}
	proposals := a.Store.Proposals()
	if len(proposals) != 1 {
		t.Fatalf("the turn recorded %d proposals, want 1", len(proposals))
	}
	ctx := iam.WithPrincipal(context.Background(), b.Person)
	resumed, err := b.Svc.Resume(ctx, approval.Resumption{Owner: b.Person.ID,
		Conversation: b.Conversation, Turn: accepted.TurnID, Proposal: proposals[0].ID,
		Read: b.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Created ENG-7."},
		Card:    cardAt(proposals[0], wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.AfterSeq != parked.Seq {
		t.Fatalf("the resumed segment streams after seq %d, want the parked segment's last, %d",
			resumed.AfterSeq, parked.Seq)
	}
	// The drawer drew the parked segment on node-a and follows there.
	c, done := stream(t, a, accepted.TurnID, parked.Seq)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	frames := c.Frames()
	if len(frames) == 0 || frames[0].Seq != parked.Seq+1 {
		t.Fatalf("node-a's stream of the resumed segment drew %v, want it to continue at seq %d",
			kinds(frames), parked.Seq+1)
	}
	for i, f := range frames {
		if f.Seq != parked.Seq+1+uint64(i) {
			t.Fatalf("frame %d is seq %d, want an unbroken run from %d", i, f.Seq, parked.Seq+1)
		}
	}
	if d := evals.Last(frames, wire.EventDone); d == nil || d.Done.State != wire.TurnCompleted {
		t.Fatalf("node-a's stream ended %v, want the resumed turn completed", kinds(frames))
	}
}

// A TURN WHOSE CLOSE DID NOT LAND IS SETTLED: the runner retries its close
// under the same operation until it lands, so the conversation is free for
// the next question rather than holding a turn that reads running forever.
//
// Mutation: make no retry (drop the settle call in conclude) and the turn
// still reads running after the person was told it completed.
func TestATurnWhoseCloseDidNotLandIsSettled(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{
		evals.Say("Done."), evals.Say("Done again.")}})
	f.Store.FailCloses(1)
	accepted, err := f.Ask("settle", "Hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone); d == nil ||
		d.Done.State != wire.TurnCompleted {
		t.Fatalf("the turn ended %+v, want completed", d)
	}
	waitUntil(t, func() bool {
		row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
		return row.State == wire.TurnCompleted
	})
	if n := f.Store.Closes(accepted.TurnID); n != 1 {
		t.Fatalf("the turn's end was recorded %d times, want once", n)
	}
	if _, err := f.Ask("next", "And then?", nil); err != nil {
		t.Fatalf("the next question in the conversation: %v", err)
	}
}

// AN INTERRUPTED CLOSE THAT DID NOT LAND IS NOT REPORTED AS ONE: the stream
// fails rather than telling the person the turn ended, and the next stream
// closes it. It fails with the close's unknown outcome as itself, never as
// the assistant's own Unavailable, which carried a wait of two seconds
// whatever the outcome was.
//
// Mutation: log the failed close and answer interrupted (the old behaviour)
// and the first stream ends a turn the record still says runs; wrap the
// unknown outcome in an Unavailable again (writeFailure) and it fails as one.
func TestAnInterruptedCloseThatDidNotLandIsSurfaced(t *testing.T) {
	t.Parallel()
	a, _ := fleetOf(t, evals.RigOptions{}, evals.RigOptions{})
	turn := orphan(t, a)
	a.Store.FailCloses(1)
	c, done := stream(t, a, turn, 0)
	if err := <-done; !errors.Is(err, assistant.ErrOutcomeUnknown) ||
		errors.Is(err, assistant.ErrUnavailable) {
		t.Fatalf("a stream whose close did not land: err = %v, want its unknown outcome "+
			"as itself", err)
	}
	if d := evals.Last(c.Frames(), wire.EventDone); d != nil {
		t.Fatalf("the stream told the person the turn ended %s though nothing recorded it",
			d.Done.State)
	}
	c, done = stream(t, a, turn, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d := evals.Last(c.Frames(), wire.EventDone); d == nil ||
		d.Done.State != wire.TurnInterrupted || a.Store.Closes(turn) != 1 {
		t.Fatalf("the retried stream ended %v with %d closes, want interrupted once",
			kinds(c.Frames()), a.Store.Closes(turn))
	}
}

// A SEGMENT TOLD ON ANOTHER NODE IS NUMBERED PAST AND FOLLOWED: a page action
// an approval emitted on node-b tells frames after the parked record's last
// seq, a stream on node-a (the record's node, holding only the parked
// segment) is answered from node-b, and the resume on node-a numbers its
// segment after node-b's frames, asked of the fleet, where the parked record
// alone would repeat them.
//
// Mutation: number a segment after the parked record alone (return parked
// from continuing without asking the fleet) and node-a's segment starts at a
// seq node-b's emission already told.
func TestASegmentToldOnAnotherNodeIsNumberedPast(t *testing.T) {
	t.Parallel()
	opts := proposing(t)
	a, b := fleetOf(t, opts, evals.RigOptions{Tools: opts.Tools, Acts: opts.Acts})
	accepted, err := a.Ask("propose", "File a task to rotate keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	parked := evals.Last(a.Follow(accepted.TurnID, 0), wire.EventDone)
	p := a.Store.Proposals()[0]
	card := cardAt(p, wire.ProposalApproved)
	ctx := iam.WithPrincipal(context.Background(), b.Person)
	emitted, err := b.Svc.Emit(ctx, b.Person.ID, b.Conversation, accepted.TurnID,
		approval.Emission{Card: &card})
	if err != nil {
		t.Fatal(err)
	}
	if emitted.AfterSeq != parked.Seq {
		t.Fatalf("node-b's emission streams after %d, want the parked segment's last, %d",
			emitted.AfterSeq, parked.Seq)
	}
	// The drawer follows on node-a, which the record names and which holds
	// only the parked segment: it is answered from node-b, holding the
	// emission.
	c, done := stream(t, a, accepted.TurnID, parked.Seq)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := c.Frames(); len(got) != 3 || got[0].Kind != wire.EventCard ||
		got[0].Seq != parked.Seq+1 {
		t.Fatalf("node-a's stream drew %v after seq %d, want node-b's card, status and done",
			kinds(got), parked.Seq)
	}
	told := evals.Last(c.Frames(), wire.EventDone)
	resumed, err := a.Svc.Resume(iam.WithPrincipal(context.Background(), a.Person),
		approval.Resumption{Owner: a.Person.ID, Conversation: a.Conversation,
			Turn: accepted.TurnID, Proposal: p.ID, Read: a.Grant("approve"),
			State: wire.ProposalApplied, Card: cardAt(p, wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	if told == nil || resumed.AfterSeq != told.Seq {
		t.Fatalf("node-a's resumed segment streams after %d, want node-b's emission's last, %v",
			resumed.AfterSeq, told)
	}
}
