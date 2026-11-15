package sandbox

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
)

// A SEAT A RUN HOLDS HAS ITS INBOX HELD, AND ONLY FOR AS LONG AS THE RUN DOES.
//
// The spin this replaces: a held seat's mail was parked — republished onto the
// inbox it had just been fetched from — and nothing stopped the consumer, so
// every waiting message went round fetch, screen, publish, ack for the whole
// run. Held instead, the consumer fetches nothing until the run stops holding
// the seat, and the hold follows the coordinator's own transitions rather than
// waiting for a delivery to notice: taken when a run starts holding, lifted
// the moment it parks on a question (so the answer can arrive).
func TestASeatsInboxIsHeldExactlyWhileARunHoldsIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	run := rig.launch("t1")
	if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
		AgentHandle: "swe", TurnID: run.TurnID, LaunchID: run.LaunchID,
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if !holds.holding("swe") {
		t.Fatal("a seat a running job holds has an open inbox: its mail would be fetched, " +
			"refused and republished for the length of the run")
	}

	// THE RUN PARKS ON A QUESTION, and gives the seat back: the answer
	// arrives on this inbox, so it must be open.
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?")})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.get("t1").Status != StatusAwaiting {
		t.Fatal("the run did not park")
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after its run parked on a question — the " +
			"answer to it could never arrive")
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe", "release swe"}) {
		t.Fatalf("holds = %v, want one hold and one release", got)
	}
}

// A SEAT RECOVERED MID-RUN IS HELD BEFORE ITS MAILBOX OPENS, and a seat this
// node releases is FORGOTTEN rather than released: the release's detach has
// already dropped the hold with the attachment, and a Release from here would
// lift the one this node takes again the moment it re-acquires the seat.
func TestARecoveredRunHoldsTheSeatAndAReleaseForgetsTheHold(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	rig.launch("t1")
	if err := rig.recoverSeat(t.Context(), "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if !holds.holding("swe") {
		t.Fatal("a seat recovered with a running job was not held before its mailbox opened")
	}

	rig.coordinator.ReleaseSeat("swe")
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v: the release lifted a hold the detach had already dropped", got)
	}

	// RE-ACQUIRED: believed unheld, so the hold is taken again.
	if err := rig.recoverSeat(t.Context(), "node-a", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe", "hold swe"}) {
		t.Fatalf("holds = %v, want the hold taken again on the re-acquired seat", got)
	}
}

// A DELIVERY THAT RACED THE HOLD asks for it again, which is how a hold the
// queue refused is retried: at the spacing the delivery's own deferral sets.
func TestHoldSeatRetriesAHoldTheQueueRefused(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	refusing := &flakyHold{fail: true}
	rig.coordinator.hold = refusing
	rig.launch("t1")
	if err := rig.recoverSeat(t.Context(), "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if refusing.held {
		t.Fatal("the refusing hold reports held")
	}
	refusing.fail = false
	rig.coordinator.HoldSeat(t.Context(), "swe")
	if !refusing.held {
		t.Fatal("a delivery reaching a held seat did not take the hold the queue refused before")
	}
}

// flakyHold is a [SeatHold] the queue refuses until a case lets it through.
type flakyHold struct {
	fail bool
	held bool
}

func (h *flakyHold) Hold(context.Context, string, queue.Attachment) error {
	if h.fail {
		return errRefusedCall
	}
	h.held = true
	return nil
}

func (h *flakyHold) Release(context.Context, string, queue.Attachment) error {
	h.held = false
	return nil
}

// A LAUNCH HOLDS ITS SEAT FROM THE ROW IT WRITES, before the box exists and with
// no started event processed at all.
//
// The window this closes: the seat used to count as held only once the run's
// started event came round on the seat's control topic, a subscription the
// launching turn never waits for. Mail drained beside the launching delivery,
// and mail fetched the moment the turn returned, reached a seat the screening
// called free and ran a turn beside the job.
func TestALaunchHoldsItsSeatFromTheRowItWrites(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	witness := &seatWitness{FakeProvider: rig.provider, coordinator: rig.coordinator, holds: holds}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := rig.coordinator.Launch(t.Context(), manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !witness.checked {
		t.Fatal("the launch never provisioned a box, so the order was not observed")
	}
	if !witness.held || !witness.inboxHeld {
		t.Fatalf("while the box was being provisioned the seat was held=%v and its inbox held=%v: "+
			"mail reaching the seat in that window runs a turn beside the job",
			witness.held, witness.inboxHeld)
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); !held || !holds.holding("swe") {
		t.Fatal("the launch returned with its seat free, waiting on a started event the " +
			"launching turn never waits for")
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v, want the one hold the launch took", got)
	}

	// ITS ANNOUNCEMENT, when it does come round, recounts the same answer
	// and moves nothing.
	run := rig.get("t1")
	if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
		AgentHandle: "swe", TurnID: run.TurnID, LaunchID: run.LaunchID,
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v: the started event moved a hold the launch already took", got)
	}
}

// seatWitness is a provider that records, as the launch provisions its box,
// whether the coordinator already holds the launching seat.
type seatWitness struct {
	*FakeProvider
	coordinator              *Coordinator
	holds                    *holdSpy
	checked, held, inboxHeld bool
}

func (w *seatWitness) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	w.checked = true
	w.held, _ = w.coordinator.SeatRuns("swe")
	w.inboxHeld = w.holds.holding("swe")
	return w.FakeProvider.Create(ctx, spec)
}

// A RECOUNT NEVER WRITES BACK OVER A LAUNCH IT DID NOT SEE. Another run's start
// recounts the seat from the store; the listing is taken, a launch writes its
// row and holds the seat, and the recount lands. Written back as read, it
// would free the seat the launch just held — beside a job that is starting.
func TestARecountDoesNotFreeASeatALaunchHeldUnderIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	holds := rig.withHold()
	racing := &listRace{PendingStore: rig.pending}
	rig.coordinator.pending = racing
	racing.during = func() {
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
			t.Errorf("Launch: %v", err)
		}
	}

	if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
		AgentHandle: "swe", TurnID: "t0",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); !held || !holds.holding("swe") {
		t.Fatalf("held = %v, holds = %v: a recount read before the launch freed the seat the "+
			"launch held", held, holds.log)
	}
}

// listRace is a store whose first seat listing is taken BEFORE something else
// happens and returned after it.
type listRace struct {
	PendingStore
	during func()
	once   bool
}

func (s *listRace) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	if !s.once {
		s.once = true
		s.during()
	}
	return runs, err
}

// A LAUNCH THAT FAILS GIVES ITS SEAT BACK — every way it can fail after the row
// it holds the seat from: at the box, at the job, and at the turn's own
// suspension into the row, which [Coordinator.FailRun] settles.
func TestAFailedLaunchGivesItsSeatBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		derail func(*coordRig)
		// suspendFails launches cleanly and then fails the turn's
		// suspension, the way the engine does when it cannot write the
		// conversation.
		suspendFails bool
	}{
		{name: "the box cannot be provisioned", derail: func(r *coordRig) {
			r.provider.CreateErr = errors.New("no capacity")
		}},
		{name: "the harness cannot be started", derail: func(r *coordRig) {
			r.runner.StartErr = errors.New("the harness is not installed")
		}},
		{name: "the turn cannot suspend into the run", derail: func(*coordRig) {}, suspendFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			holds := rig.withHold()
			tc.derail(rig)
			_, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1"))
			if tc.suspendFails {
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if err := rig.coordinator.FailRun(t.Context(), "t1",
					types.ExecutorFailureSuspensionUnrecorded, "the conversation could not be written"); err != nil {
					t.Fatalf("FailRun: %v", err)
				}
			} else if err == nil {
				t.Fatal("a launch that could not finish reported success")
			}
			if held, _ := rig.coordinator.SeatRuns("swe"); held {
				t.Fatal("a launch that ended holds its seat: every later message waits behind a job " +
					"that is not running")
			}
			if holds.holding("swe") {
				t.Fatalf("holds = %v: the seat's inbox stays held after its launch ended", holds.log)
			}
		})
	}
}

// A HOLD DECIDED BEFORE ITS SEAT'S RELEASE NEVER GATES THE SEAT'S NEXT ATTACHMENT.
//
// The window: a transition decides "hold" while this node holds the seat, and
// its queue call lands after the release has detached the inbox and the
// coordinator has forgotten the seat. Keyed by the subscription alone, that
// hold was carried onto the inbox this node attached when it took the seat
// back, and a recovery with nothing to drive took no hold of its own and so
// lifted none: the seat consumed no mail here until it moved again. Taken for
// the attachment the decision was made under, it is refused.
//
// Mutation: hold the inbox's current attachment rather than the one the
// decision was made for, and the retaken inbox attaches held.
func TestAHoldDecidedBeforeTheReleaseDoesNotGateTheNextAttachment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newInboxRig(t)
	r.take(1)

	// THE RELEASE LANDS BETWEEN THE DECISION AND THE QUEUE CALL: the seat's
	// inbox detached, then the coordinator told, as a node's release does.
	r.hold.between = func() {
		if _, err := r.q.Detach(ctx, seatInbox, seatInboxGroup); err != nil {
			t.Errorf("Detach: %v", err)
		}
		r.coordinator.ReleaseSeat("swe")
	}
	r.start("t1")
	if !r.hold.asked {
		t.Fatal("the started run decided no hold, so the window was never staged")
	}

	// THE RUN SETTLES ON ANOTHER HOLDER, and this node takes the seat back
	// with nothing to recover.
	if _, _, err := r.pending.DecideEnding(ctx, "t1", Decision{
		License: License{WhileIn: Active, Launch: EveryLaunch}, Reason: "x", Detail: "x",
	}); err != nil {
		t.Fatalf("DecideEnding: %v", err)
	}
	if _, _, err := r.pending.Finish(ctx, "t1", r.get("t1").Ending.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	r.take(2)
	if err := r.q.Publish(ctx, seatInbox, &events.Event{ID: uuid.New(), Type: "after-retake",
		Source: "seathold_test", Timestamp: time.Now().UTC()}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case typ := <-r.got:
		if typ != "after-retake" {
			t.Fatalf("delivered %q, want the mail published after the retake", typ)
		}
	default:
		t.Fatalf("the seat's inbox attached held, holds %v: a hold decided before the release "+
			"landed on the attachment this node made when it took the seat back, and nothing "+
			"that knew of it was left to lift it", r.q.PauseHolds(seatInbox, seatInboxGroup))
	}
}

// A HOLD DECIDED WHILE THE SEAT IS HELD IS TAKEN, for the attachment its
// recovery was handed: the case above's refusal is the queue's answer to an
// attachment that ended, never every hold refused.
//
// Mutation: hold for the zero attachment, and no hold is taken.
func TestAHoldIsTakenForTheAttachmentTheSeatWasRecoveredFor(t *testing.T) {
	t.Parallel()
	r := newInboxRig(t)
	r.take(1)
	r.start("t1")
	if got := r.q.PauseHolds(seatInbox, seatInboxGroup); !slices.Equal(got, []string{"sandbox"}) {
		t.Fatalf("holds = %v, want the run's hold on the inbox the seat was recovered for", got)
	}
}

// inboxRig is a coordinator rig whose seat hold is taken on a real queue's
// inbox, as the engine's is, and taken as a node takes a seat.
type inboxRig struct {
	*coordRig
	q    *queuememory.Queue
	hold *inboxHold
	got  chan string
}

func newInboxRig(t *testing.T) *inboxRig {
	t.Helper()
	q := queuememory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	r := &inboxRig{coordRig: newCoordRig(t), q: q, hold: &inboxHold{q: q}, got: make(chan string, 4)}
	r.coordinator.hold = r.hold
	return r
}

// take is a node's acquisition of the seat under epoch: the attachment read,
// the seat recovered for it, then the inbox attached.
func (r *inboxRig) take(epoch int64) {
	r.t.Helper()
	ctx := r.t.Context()
	at, err := r.q.Attachment(ctx, seatInbox, seatInboxGroup)
	if err != nil {
		r.t.Fatalf("Attachment: %v", err)
	}
	if err := r.recoverSeatOn(ctx, "node-a", epoch, at); err != nil {
		r.t.Fatalf("RecoverSeat: %v", err)
	}
	if err := r.q.Subscribe(ctx, seatInbox, seatInboxGroup,
		func(_ context.Context, ev *events.Event) queue.Result {
			r.got <- ev.Type
			return queue.Ack()
		}); err != nil {
		r.t.Fatalf("Subscribe: %v", err)
	}
}

// start launches turnID's job and announces its start, which counts the run as
// holding the seat and decides the seat's hold.
func (r *inboxRig) start(turnID string) {
	r.t.Helper()
	run := r.launch(turnID)
	if err := r.coordinator.OnStarted(r.t.Context(), types.ExecutorLaunched{
		AgentHandle: "swe", TurnID: run.TurnID, LaunchID: run.LaunchID,
	}); err != nil {
		r.t.Fatalf("OnStarted: %v", err)
	}
}

const (
	seatInbox      = "crewlet.agent.swe.inbox"
	seatInboxGroup = "seat-swe"
)

// inboxHold is a [SeatHold] on a real queue's inbox, as the engine's is. between
// runs once, after the coordinator has decided a hold and before its queue call.
type inboxHold struct {
	q       *queuememory.Queue
	between func()
	asked   bool
}

func (h *inboxHold) Hold(ctx context.Context, _ string, inbox queue.Attachment) error {
	h.asked = true
	if f := h.between; f != nil {
		h.between = nil
		f()
	}
	return h.q.PauseTopic(ctx, seatInbox, seatInboxGroup, inbox, "sandbox")
}

func (h *inboxHold) Release(ctx context.Context, _ string, inbox queue.Attachment) error {
	return h.q.ResumeTopic(ctx, seatInbox, seatInboxGroup, inbox, "sandbox")
}
