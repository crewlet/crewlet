package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// steerRing is the keyring every node of a steer case's fleet holds.
var steerRing = statelog.OneKey("k1", "the steer cases' fleet key material")

// mustSteerDesk is a desk over ring.
func mustSteerDesk(t *testing.T, ring statelog.Keyring) *steerDesk {
	t.Helper()
	d, err := newSteerDesk(ring)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// founderNote is the note every case sends, as the founder's session.
func founderNote(turnID string) steer.Request {
	return steer.Request{
		Version: steer.WireVersion, TurnID: turnID, NoteID: "req-1",
		Note: "use staging", By: "founder", ByKind: iam.ActorHuman, OperatorID: "pat:ops-token",
	}
}

// sealedNote is req signed on wire the way steer_turn signs it: a fresh id and
// a deadline ahead.
func sealedNote(t *testing.T, wire *steerWire, req steer.Request) (steer.Request, []byte) {
	t.Helper()
	req.ID, req.Deadline = signedask.NewID(), time.Now().Add(time.Minute)
	sealed, err := wire.SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return req, sealed
}

// ask scatters one note the way steer_turn does, signed on wire, and returns
// every reply it believes.
func ask(t *testing.T, q interface {
	Ask(context.Context, string, []byte, int) ([][]byte, error)
}, wire *steerWire, turnID string, want int) []steer.Reply {
	t.Helper()
	req, sealed := sealedNote(t, wire, founderNote(turnID))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	replies, err := q.Ask(ctx, topics.SeatSteer, sealed, want)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	out := make([]steer.Reply, 0, len(replies))
	for _, r := range replies {
		reply, why, err := wire.OpenReply(r, req.ID)
		if err != nil || why != "" {
			t.Fatalf("a reply the asker cannot believe: %q, %v", why, err)
		}
		out = append(out, reply)
	}
	return out
}

// steerServing serves desk on q's broker for the length of the case.
func steerServing(t *testing.T, q *memory.Queue, desk *steerDesk) {
	t.Helper()
	stop, err := q.Serve(t.Context(), topics.SeatSteer, desk.answer)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

// steerClients is n started clients of one broker.
func steerClients(t *testing.T, n int) []*memory.Queue {
	t.Helper()
	broker := memory.NewBroker()
	out := make([]*memory.Queue, n)
	for i := range out {
		out[i] = broker.Client()
		if err := out[i].Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	return out
}

// ONLY THE SEAT HOLDER ANSWERS A STEER.
//
// Two nodes serve the subject; one runs the turn. A scatter that waits out its
// whole deadline (want 0) must collect exactly ONE reply — the running node's.
// A second answer from a node that does not run the turn would be a node
// claiming to have handed a note to a turn it cannot reach.
func TestOnlyTheSeatHolderAnswersASteer(t *testing.T) {
	t.Parallel()
	clients := steerClients(t, 2)
	holder, other := clients[0], clients[1]
	running, idle := mustSteerDesk(t, steerRing), mustSteerDesk(t, steerRing)
	steerServing(t, holder, running)
	steerServing(t, other, idle)
	box := steer.New()
	running.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)

	got := ask(t, other, idle.wire, "run-1", 0)
	if len(got) != 1 {
		t.Fatalf("%d nodes answered for one running turn: %+v", len(got), got)
	}
	if got[0].Status != steer.StatusAccepted || got[0].AgentHandle != "swe" ||
		got[0].Agent != sweAgent {
		t.Errorf("the holder answered %+v", got[0])
	}
	if notes := box.Drain(); len(notes) != 1 || notes[0].By != "founder" ||
		notes[0].ByKind != iam.ActorHuman || notes[0].OperatorID != "pat:ops-token" {
		t.Errorf("the turn holds %+v, want the one note from the founder", notes)
	}
	// NOBODY ANSWERS for a turn no node runs or remembers — which the tool
	// reports as unknown, never as not running.
	if got := ask(t, other, idle.wire, "run-unknown", 0); len(got) != 0 {
		t.Errorf("a turn nobody runs was answered: %+v", got)
	}
}

// A TURN THAT JUST ENDED IS ANSWERED CLOSED, not by silence: a note races a
// turn's end by seconds, and "nobody answered" would send a person to retry a
// note to a turn that is over. The memory is bounded.
func TestAnEndedTurnIsAnsweredClosedForAWhile(t *testing.T) {
	t.Parallel()
	d := mustSteerDesk(t, steerRing)
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, steer.New(), nil)
	d.close("run-1")
	status, _, seat, err := d.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"})
	if err != nil || status != steer.StatusClosed || seat.handle != "swe" || seat.agent != sweAgent {
		t.Fatalf("an ended turn answered (%s, %+v, %v), want closed on its seat", status, seat, err)
	}
	for i := range steerClosedMemory {
		id := "later-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		d.register(id, steerSeat{handle: "swe"}, steer.New(), nil)
		d.close(id)
	}
	if _, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"}); err == nil {
		t.Errorf("the desk still remembers a turn %d turns later — its memory is unbounded",
			steerClosedMemory)
	}
	if len(d.closed) != steerClosedMemory || len(d.order) != steerClosedMemory {
		t.Errorf("the desk remembers %d/%d ended turns, want %d", len(d.closed), len(d.order),
			steerClosedMemory)
	}
}

// launcherStub is an executor launcher no case launches through.
type launcherStub struct{}

func (launcherStub) LaunchExecutor(context.Context, runner.AgentRunRequest) (runner.Launched, error) {
	return runner.Launched{}, nil
}

// A NOTE THAT MISSED THE TURN IS REPORTED EXPIRED.
//
// Taken while the turn's first segment ran, never read because the segment
// ended without launching a run that would have carried it: the person was
// answered `pending`, and the turn's record is the only place that can say what
// became of it. The closer every first segment calls on its way out reports it,
// on the turn, as sent — and a note sent after the end is answered closed.
func TestANoteThatMissedTheTurnIsReportedExpired(t *testing.T) {
	t.Parallel()
	desk := mustSteerDesk(t, steerRing)
	e, p := starting(t, refusingModels(t))
	e.steers = desk
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	r, err := company.RunnerFor("swe", nil, RunnerInput{
		Reply: turn.NoReply(), Launcher: launcherStub{}, Sessions: e.sessions, Publisher: p,
		Turn: runner.Turn{RunID: "run-1", WorkKey: "wk-1",
			Context: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: seat, Org: company.Org}},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	box := steer.New()
	closeSteer := e.openSteer("run-1", "swe", box, r)

	req := founderNote("run-1")
	req.ID, req.Deadline = signedask.NewID(), time.Now().Add(time.Minute)
	raw, err := desk.wire.SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := desk.answer(t.Context(), raw); err != nil {
		t.Fatalf("offering the note: %v", err)
	}
	closeSteer(t.Context())

	evs := p.published()
	at := position(evs, "agent_turn_steered")
	if at < 0 {
		t.Fatalf("no agent_turn_steered was published (%s)", typesOf(evs))
	}
	got, _ := events.DataAs[*types.AgentTurnSteered](evs[at])
	if got.Outcome != types.SteerExpired || got.NoteID != "req-1" || got.TurnID != "run-1" ||
		got.SteeredBy != "founder" || got.SteeredByKind != string(iam.ActorHuman) ||
		got.OperatorID != "pat:ops-token" || got.Note != "use staging" || got.Round != 0 {
		t.Errorf("the note is recorded %+v, want it expired, unread, naming who sent it", got)
	}
	if status, _, _, _ := desk.offer(t.Context(), "run-1", steer.Note{ID: "late", Text: "x"}); status != steer.StatusClosed {
		t.Errorf("a note after the turn ended answered %s, want closed", status)
	}
}

// sweAgent is the fixture seat's derived id, as a desk files it.
const sweAgent = "0f1d4c07-6d2a-4e2b-9a3c-5b8e17d04c21"

// A PROBE SAYS WHOSE TURN IT IS AND TAKES NOTHING.
//
// Who may steer a turn is decided on the turn's seat, and the serving node
// learns the seat only from the running node — so it asks first, and only an
// offer after a decision hands the note over. A probe that took a note would
// put it in front of the turn before anybody decided whether the person could
// send it.
func TestAProbeNamesTheTurnsSeatAndTakesNothing(t *testing.T) {
	t.Parallel()
	d := mustSteerDesk(t, steerRing)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)

	probe, raw := sealedNote(t, d.wire, steer.Request{Version: steer.WireVersion,
		TurnID: "run-1", Probe: true})
	out, err := d.answer(t.Context(), raw)
	if err != nil {
		t.Fatalf("a probe was refused: %v", err)
	}
	reply, why, err := d.wire.OpenReply(out, probe.ID)
	if err != nil || why != "" {
		t.Fatalf("the probe's reply is not one the asker can believe: %q, %v", why, err)
	}
	if reply.Status != steer.StatusAccepted || reply.Agent != sweAgent || reply.AgentHandle != "swe" {
		t.Errorf("the probe answered %+v, want accepted on the turn's seat", reply)
	}
	if notes := box.Drain(); len(notes) != 0 {
		t.Errorf("a probe put %d notes in front of the turn", len(notes))
	}

	// AND ONE FOR A TURN THAT ENDED is closed, on the seat it ran on.
	d.close("run-1")
	if status, seat, err := d.probe("run-1"); err != nil || status != steer.StatusClosed ||
		seat.agent != sweAgent {
		t.Errorf("a probe of an ended turn answered (%s, %+v, %v), want closed", status, seat, err)
	}
}

// ONLY A NOTE THE FLEET SIGNED REACHES A TURN. The running node offers a note
// as the person it names, on the asking node's decision that they may steer
// the turn — so a note anybody who can reach the broker could write would put
// words in front of a running agent under anybody's name. Three ways to try —
// no signature, a signature under the fleet's key id with somebody else's
// material, and the fleet's signature over a note altered after it was made —
// and none is answered or offered.
func TestOnlyANoteTheFleetSignedReachesATurn(t *testing.T) {
	t.Parallel()
	d := mustSteerDesk(t, steerRing)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)

	unsigned, err := json.Marshal(founderNote("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, outsider := sealedNote(t, mustSteerDesk(t,
		statelog.OneKey("k1", "not the fleet's material")).wire, founderNote("run-1"))
	_, altered := sealedNote(t, d.wire, founderNote("run-1"))
	altered = bytes.Replace(altered, []byte(`"by":"founder"`), []byte(`"by":"ceo"`), 1)
	if !bytes.Contains(altered, []byte(`"by":"ceo"`)) {
		t.Fatal("the case did not alter the signed sender")
	}
	for name, sent := range map[string][]byte{
		"unsigned":                         unsigned,
		"signed under somebody else's key": outsider,
		"altered after it was signed":      altered,
	} {
		if out, err := d.answer(t.Context(), sent); err == nil || out != nil {
			t.Errorf("%s: answered %q, want nothing", name, out)
		}
	}
	if notes := box.Drain(); len(notes) != 0 {
		t.Fatalf("a note the fleet did not sign reached the turn: %+v", notes)
	}
}

// A NOTE THE FLEET SIGNED IS OFFERED ONCE: the same bytes arriving again — a
// note captured off the wire and sent back — are answered by nobody, however
// the first was answered.
func TestAReplayedNoteIsAnsweredByNobody(t *testing.T) {
	t.Parallel()
	d := mustSteerDesk(t, steerRing)
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, steer.New(), nil)
	_, sealed := sealedNote(t, d.wire, founderNote("run-1"))
	if _, err := d.answer(t.Context(), sealed); err != nil {
		t.Fatalf("the note itself was refused: %v", err)
	}
	if out, err := d.answer(t.Context(), sealed); err == nil || out != nil {
		t.Fatalf("a replayed note was answered %q", out)
	}
}

// A NOTE UNDER A KEY THE RUNNING NODE DOES NOT HOLD IS ANSWERED UNVERIFIED by
// that node alone, and nothing is offered: the key is a fact about the node,
// which an operator changes by adding it, so the person is told that rather
// than `unknown` — and a node the turn is not on stays silent, as it does for
// any note not its own.
func TestANoteUnderAnUnheldKeyIsAnsweredUnverified(t *testing.T) {
	t.Parallel()
	rotated := statelog.Keyring{ActiveID: "k2", Keys: []statelog.Key{
		steerRing.Keys[0], {ID: "k2", Material: "the key the fleet is rotating to"},
	}}
	clients := steerClients(t, 3)
	running, idle, asking := mustSteerDesk(t, steerRing), mustSteerDesk(t, steerRing),
		mustSteerDesk(t, rotated)
	steerServing(t, clients[0], running)
	steerServing(t, clients[1], idle)
	box := steer.New()
	running.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)

	got := ask(t, clients[2], asking.wire, "run-1", 0)
	if len(got) != 1 || got[0].Unverified == "" || got[0].Status != "" || got[0].Agent != "" {
		t.Fatalf("a note under an unheld key was answered %+v, want the running node "+
			"alone saying it could not verify it", got)
	}
	if notes := box.Drain(); len(notes) != 0 {
		t.Fatalf("a note the running node could not verify reached the turn: %+v", notes)
	}
}

// handingDesk is a desk whose turns' boxes are handed to run records on a
// coordination store of their own, and the store and what the desk publishes.
func handingDesk(t *testing.T) (*steerDesk, *sandbox.CoordStore, *pub) {
	t.Helper()
	d := mustSteerDesk(t, steerRing)
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	seals, err := newSteerSeals(steerRing)
	if err != nil {
		t.Fatal(err)
	}
	p := &pub{}
	d.runs = newRunSteers(seals, store, p)
	return d, store, p
}

// launchedRun is turnID's run record as a launch opens it, anchored on the swe
// seat: the brief's notes read by the job it opens, and the box's handed notes
// filed waiting after them.
func launchedRun(t *testing.T, store *sandbox.CoordStore, runs *runSteers, turnID string,
	brief, handed []steer.Note,
) sandbox.PendingRun {
	t.Helper()
	briefed, err := runs.briefNotes(turnID, 1, brief)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := runs.handedNotes(turnID, handed)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: turnID, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE", CodingAgent: "claude-code",
		Steer:      sandbox.RunSteer{Seat: runs.anchor(turnID, sweAgent, "swe")},
		BriefNotes: briefed, HandedNotes: sealed,
	}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	return opened
}

// handToRecord plays a turn's launch handing its first segment's box over: the
// hand-over begins, the write that creates the run's record carries what the
// box held, written — whatever runs between that write and the launch saying
// so — and the hand-over is settled as recorded. It answers the record as the
// launch opened it.
func handToRecord(t *testing.T, d *steerDesk, store *sandbox.CoordStore, turnID string,
	brief []steer.Note, written func(),
) sandbox.PendingRun {
	t.Helper()
	hand := d.handOver(turnID)
	if hand == nil {
		t.Fatalf("the desk handed nothing over for %s", turnID)
	}
	opened := launchedRun(t, store, d.runs, turnID, brief, hand.held())
	if written != nil {
		written()
	}
	hand.recorded()
	return opened
}

// recordedNotes is the ids of the notes on turnID's record, in the order they
// were filed, and which wait.
func recordedNotes(t *testing.T, store *sandbox.CoordStore, turnID string) (all, waiting []string) {
	t.Helper()
	run, found, err := store.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get %s = %v, %v", turnID, found, err)
	}
	notes := slices.Clone(run.Steer.Notes)
	slices.SortFunc(notes, func(a, b sandbox.RunNote) int { return a.Seq - b.Seq })
	for _, n := range notes {
		all = append(all, n.ID)
		if n.Waiting() {
			waiting = append(waiting, n.ID)
		}
	}
	return all, waiting
}

// AN OFFER RACING THE LAUNCH IS NEVER ANSWERED CLOSED. A note that read "no
// record" a moment before the launch wrote one is scattered to the node that
// held the turn's box: it lands in the box and rides the write that creates the
// record, or meets the turn being handed and waits for that write, or finds the
// turn handed and is filed on the record by that node — and every way it waits
// there for the run, under the person's request id, behind the note the box
// held first.
//
// Mutation: answer the box's `closed` without looking again, file an offer that
// meets the hand-over at once rather than after the write, or hand the box's
// notes over after the record exists, and this goes red.
func TestAnOfferRacingTheLaunchIsNeverAnsweredClosed(t *testing.T) {
	t.Parallel()
	d, store, _ := handingDesk(t)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)
	if got := box.Offer(steer.Note{ID: "before", Text: "use staging", By: "founder"}); got != steer.StatusAccepted {
		t.Fatalf("the box answered %s", got)
	}

	const racers = 16
	type answer struct {
		id     string
		status steer.Status
	}
	answers := make(chan answer, racers)
	start := make(chan struct{})
	for i := range racers {
		go func() {
			<-start
			id := "racer-" + string(rune('a'+i))
			status, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: id, Text: "tag it", By: "founder"})
			if err != nil {
				t.Errorf("offer %d: %v", i, err)
			}
			answers <- answer{id: id, status: status}
		}()
	}
	close(start)
	handToRecord(t, d, store, "run-1", nil, nil)
	all, waiting := recordedNotes(t, store, "run-1")
	for range racers {
		got := <-answers
		switch {
		case got.status == steer.StatusClosed:
			t.Fatal("an offer racing the launch was told the turn had ended")
		case got.status == steer.StatusAccepted:
			if _, waiting := recordedNotes(t, store, "run-1"); !slices.Contains(waiting, got.id) {
				t.Errorf("note %s was answered accepted and does not wait on the record", got.id)
			}
		}
	}
	if len(all) == 0 || all[0] != "before" || !slices.Contains(waiting, "before") {
		t.Errorf("the note the box held before the launch is not the record's first, waiting: %v (waiting %v)",
			all, waiting)
	}
	// AND AFTER THE HAND-OVER the scatter files onto the record as the
	// record answers.
	status, _, seat, err := d.offer(t.Context(), "run-1", steer.Note{ID: "before", Text: "use staging"})
	if err != nil || status != steer.StatusAccepted || seat.agent != sweAgent {
		t.Errorf("a retry after the hand-over answered (%s, %+v, %v), want accepted on the seat",
			status, seat, err)
	}
}

// THE BOX'S NOTES ARE ON THE RECORD AHEAD OF ONE FILED THERE AS IT IS WRITTEN.
// Once the launch's write lands, any node's steer_turn finds the record and
// files on it directly — before this node's launch has gone on to anything
// else. The box's notes ride that write, so they are first, and they are
// counted against the turn's bounds before the direct filing is: a box that
// held as many notes as may wait leaves the direct filing answered `full`,
// never a note the box accepted refused and expired in its place.
//
// Mutation: file the box's notes on the record after its write, one
// compare-and-swap each, as the hand-over did, and the direct filing lands first
// and the last of the box's notes is refused.
func TestTheBoxsNotesAreOnTheRecordAheadOfOneFiledAsItIsWritten(t *testing.T) {
	t.Parallel()
	d, store, p := handingDesk(t)
	box := steer.New()
	var expired []steer.Note
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box,
		func(_ context.Context, notes []steer.Note) { expired = append(expired, notes...) })
	var held []string
	for i := range steer.MaxPendingNotes {
		id := "box-" + string(rune('a'+i))
		if got := box.Offer(steer.Note{ID: id, Text: "x", By: "founder"}); got != steer.StatusAccepted {
			t.Fatalf("the box answered %s to %s", got, id)
		}
		held = append(held, id)
	}
	var direct builtin.RunFiled
	handToRecord(t, d, store, "run-1", nil, func() {
		var err error
		direct, err = d.runs.File(t.Context(), "run-1", steer.Note{ID: "direct", Text: "y", By: "founder"})
		if err != nil {
			t.Fatalf("the direct filing: %v", err)
		}
	})
	all, waiting := recordedNotes(t, store, "run-1")
	if !slices.Equal(all, held) || !slices.Equal(waiting, held) {
		t.Errorf("the record holds %v (waiting %v), want the box's notes alone, waiting in the order taken: %v",
			all, waiting, held)
	}
	if direct.Status != steer.StatusFull {
		t.Errorf("the direct filing behind a full hand-over answered %s, want full", direct.Status)
	}
	if len(expired) != 0 || len(steered(p)) != 0 {
		t.Errorf("the hand-over reported notes expired: %v, %+v", expired, steered(p))
	}
}

// AN OFFER THAT MEETS THE HAND-OVER WAITS FOR THE WRITE. Between the box's
// intake ending and the write that creates the record the note has nowhere it
// could land behind the box's notes, so it waits — and is filed on the record
// the moment that write lands, behind them.
//
// Mutation: answer the offer from the box (closed), or file it before the
// write, and this goes red.
func TestAnOfferThatMeetsTheHandOverWaitsForTheWrite(t *testing.T) {
	t.Parallel()
	d, store, _ := handingDesk(t)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)
	box.Offer(steer.Note{ID: "held", Text: "x", By: "founder"})
	hand := d.handOver("run-1")
	answered := make(chan steer.Status, 1)
	go func() {
		status, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: "late", Text: "y", By: "founder"})
		if err != nil {
			t.Errorf("offer: %v", err)
		}
		answered <- status
	}()
	select {
	case got := <-answered:
		t.Fatalf("an offer during the hand-over was answered %s before the record existed", got)
	case <-time.After(100 * time.Millisecond):
	}
	if status, _, err := d.probe("run-1"); err != nil || status != steer.StatusAccepted {
		t.Errorf("a probe during the hand-over answered (%s, %v), want accepted", status, err)
	}
	launchedRun(t, store, d.runs, "run-1", nil, hand.held())
	hand.recorded()
	if got := <-answered; got != steer.StatusAccepted {
		t.Fatalf("the offer that waited answered %s, want accepted on the record", got)
	}
	if all, waiting := recordedNotes(t, store, "run-1"); !slices.Equal(all, []string{"held", "late"}) ||
		!slices.Equal(waiting, all) {
		t.Errorf("the record holds %v (waiting %v), want the box's note, then the late one", all, waiting)
	}
}

// A LAUNCH THAT FAILS BEFORE ITS WRITE GIVES THE BOX ITS NOTES BACK: no record
// holds them, so they are the box's again — at its front, read first by
// whatever reads the box next — and the turn takes notes in its box again.
//
// Mutation: leave the box handed, or drop what it held, and this goes red.
func TestALaunchThatFailsBeforeItsWriteGivesTheBoxItsNotesBack(t *testing.T) {
	t.Parallel()
	d, _, _ := handingDesk(t)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)
	box.Offer(steer.Note{ID: "held", Text: "x", By: "founder"})
	hand := d.handOver("run-1")
	if got := ids(hand.held()); !slices.Equal(got, []string{"held"}) {
		t.Fatalf("the hand-over took %v, want the box's note", got)
	}
	hand.unrecorded(t.Context())
	status, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: "after", Text: "y", By: "founder"})
	if err != nil || status != steer.StatusAccepted {
		t.Fatalf("an offer after the failed launch answered (%s, %v), want accepted in the box", status, err)
	}
	if got := ids(box.Drain()); !slices.Equal(got, []string{"held", "after"}) {
		t.Errorf("the box holds %v, want the note it handed back first", got)
	}
}

// ids is the ids of notes.
func ids(notes []steer.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

// EVERY NOTE THE HAND-OVER ACCEPTED IS ON THE RECORD: an offer answered
// `accepted` across the hand-over is one the run will read, never one that was
// lost between the box and the record.
func TestEveryNoteAcceptedAcrossTheHandOverIsOnTheRecord(t *testing.T) {
	t.Parallel()
	d, store, _ := handingDesk(t)
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, steer.New(), nil)
	accepted := make(chan string, steer.MaxPendingNotes)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range steer.MaxPendingNotes {
			id := "n-" + string(rune('a'+i))
			status, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: id, Text: "x", By: "founder"})
			if err == nil && status == steer.StatusAccepted {
				accepted <- id
			}
		}
	}()
	handToRecord(t, d, store, "run-1", nil, nil)
	<-done
	close(accepted)
	all, _ := recordedNotes(t, store, "run-1")
	for id := range accepted {
		if !slices.Contains(all, id) {
			t.Errorf("note %s was answered accepted and is not on the record (%v)", id, all)
		}
	}
}

// A HANDED PROBE NAMES THE SEAT: a lead's request that read "no record" asks the
// fleet whose turn it is, and the node that handed the box over answers for the
// turn's seat — so the decision is taken on it and the offer that follows is
// filed on the record.
func TestAHandedProbeNamesTheSeat(t *testing.T) {
	t.Parallel()
	d, store, _ := handingDesk(t)
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, steer.New(), nil)
	handToRecord(t, d, store, "run-1", nil, nil)

	status, seat, err := d.probe("run-1")
	if err != nil || status != steer.StatusAccepted || seat.agent != sweAgent || seat.handle != "swe" {
		t.Fatalf("a probe of a handed turn answered (%s, %+v, %v), want accepted on its seat",
			status, seat, err)
	}
}

// A FAILED LAUNCH EXPIRES ITS BOX'S NOTES THROUGH THE SEGMENT'S CLOSE: a launch
// that never went hands nothing over, so the box is the segment's still, and
// what it holds when the segment ends is recorded expired by the close every
// segment makes — once.
func TestAFailedLaunchExpiresItsBoxNotes(t *testing.T) {
	t.Parallel()
	d, _, _ := handingDesk(t)
	box := steer.New()
	calls := 0
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box,
		func(context.Context, []steer.Note) { calls++ })
	box.Offer(steer.Note{ID: "taken", Text: "x"})
	// The brief takes the note, the launch fails before its write, and the
	// runner gives it back ([runner.SteerSource.Return]).
	brief := box.Brief(t.Context())
	box.Return(t.Context(), brief)

	if notes := d.close("run-1"); len(notes) != 1 || notes[0].ID != "taken" {
		t.Fatalf("the close returned %+v, want the note the failed launch gave back", notes)
	}
	if calls != 0 {
		t.Errorf("the desk recorded expiries itself (%d calls); the closer does", calls)
	}
	if status, _, _, _ := d.offer(t.Context(), "run-1", steer.Note{ID: "late", Text: "x"}); status != steer.StatusClosed {
		t.Errorf("a note after the segment ended answered %s, want closed", status)
	}
}

// A LAUNCH THAT FAILS AFTER ITS WRITE LEAVES ITS NOTES TO THE RECORD'S ENDING.
// The write that creates the record holds the brief's notes, read by the job it
// opens, and the box's, waiting — and the launch can still fail after it, its
// box not made, its CLI not started. Nothing is handed back: the segment's close
// reports nothing, and the run's ending, which every such failure decides,
// reports every one of them expired — the brief's too, read by a job that never
// started — once, under its own ids.
//
// Mutation: hand the brief's notes back to the box as a failed launch did, or
// leave a never-started job's brief reads out of the ending's, and a note is
// reported twice or never.
func TestALaunchThatFailsAfterItsWriteLeavesItsNotesToTheRecordsEnding(t *testing.T) {
	t.Parallel()
	d, store, p := handingDesk(t)
	box := steer.New()
	d.register("run-1", steerSeat{agent: sweAgent, handle: "swe"}, box, nil)
	box.Offer(steer.Note{ID: "briefed", Text: "use staging", By: "founder"})
	brief := box.Brief(t.Context())
	box.Offer(steer.Note{ID: "held", Text: "and tag it", By: "founder"})
	opened := handToRecord(t, d, store, "run-1", brief, nil)
	// THE LAUNCH FAILS: the runner hands nothing back, since the launch named
	// the job it opened ([runner.Launched.LaunchID]).
	if expired := d.close("run-1"); len(expired) != 0 {
		t.Fatalf("the segment's close returned %+v, want nothing: the record holds them", expired)
	}
	run := mustRow(t, store, "run-1")
	if run.LaunchID != opened.LaunchID || run.Launch.Started {
		t.Fatalf("the record is %+v, want the job the launch opened, never started", run)
	}
	run.Ending = &sandbox.RecordedEnding{ID: "abandoned", At: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	if err := d.runs.expired(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rec := range steered(p) {
		if rec.Outcome != types.SteerExpired {
			t.Errorf("the ending published %+v", rec)
		}
		got = append(got, rec.NoteID)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"briefed", "held"}) {
		t.Errorf("the ending reported %v expired, want the brief's note and the box's, once each", got)
	}
	status, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: "late", Text: "x", By: "founder"})
	if err != nil || status != steer.StatusAccepted {
		t.Errorf("a late note after the close answered (%s, %v), want it filed on the record", status, err)
	}
}

// A RESUMED SEGMENT THAT ENDS THE TURN IS REMEMBERED CLOSED: the run's record
// goes with the run, so a note that reaches this node after it is answered
// `closed` by the scatter — never by nobody, which the person reads as a lost
// note to send again.
func TestAResumedSegmentThatEndsTheTurnIsRememberedClosed(t *testing.T) {
	t.Parallel()
	d := mustSteerDesk(t, steerRing)
	if _, _, _, err := d.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"}); err == nil {
		t.Fatal("a desk that never held the turn answered for it")
	}
	d.remember("run-1", steerSeat{agent: sweAgent, handle: "swe"})
	status, _, seat, err := d.offer(t.Context(), "run-1", steer.Note{ID: "n", Text: "x"})
	if err != nil || status != steer.StatusClosed || seat.agent != sweAgent {
		t.Errorf("a note to a turn a resumed segment ended answered (%s, %+v, %v), want closed",
			status, seat, err)
	}
}
