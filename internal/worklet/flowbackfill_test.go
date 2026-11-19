package worklet_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// shiftingPostures is a live projection whose posture a case moves.
type shiftingPostures struct {
	mu sync.Mutex
	p  worklet.Posture
}

func (s *shiftingPostures) SeatPostures() worklet.Posture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p
}

func (s *shiftingPostures) set(p worklet.Posture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p = p
}

// seatPast is what a case says the seats' retained events hold, from an
// instant on.
type seatPast struct {
	covered time.Time
	stops   map[string][]worklet.SeatStop
}

func (p seatPast) SeatPast(_ context.Context, seat string, _, _ time.Time) (worklet.SeatHistory, error) {
	return worklet.SeatHistory{Covered: p.covered, Stops: p.stops[seat]}, nil
}

// backfillDutyOn is the round trip's flow duty over postures, reading the
// seats' past from past (nil for none).
func backfillDutyOn(t *testing.T, r *roundTrip, postures worklet.Postures,
	past worklet.SeatPast) *worklet.FlowDuty {

	t.Helper()
	log, err := statelogtest.LocalReader(worklet.Domain{}, r.db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(r.db.Reader(), log, postures)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	writer, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: r.publisher, DB: r.db.Reader(), NodeID: r.nodeID, Claims: r.claims,
		Actor: r.nodeID, ActorKind: worklet.AuthorSystem,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("system writer: %v", err)
	}
	duty, err := worklet.NewFlowDuty(worklet.FlowDutyDeps{Reader: reader, Writer: writer,
		Chart: engChart, NodeID: r.nodeID, Past: past})
	if err != nil {
		t.Fatalf("flow duty: %v", err)
	}
	return duty
}

// flowChanges is an item's recorded changes of state, oldest first: those
// the duty read (backfilled false) or those the backfill derived.
func flowChanges(t *testing.T, r *roundTrip, id string, backfilled bool) []worklet.FlowEntry {
	t.Helper()
	var out []worklet.FlowEntry
	for _, e := range detailOf(t, r, id, time.Now()).FlowHistory {
		if e.Notice == "" && e.Backfilled == backfilled {
			out = append(out, e)
		}
	}
	slices.Reverse(out)
	return out
}

// A BACKFILLED FLOW IS THE RECORDED FLOW (ADR-0162).
//
// The backfill reads an item's past as the flow duty would have read it,
// had it been running: the duty's own expression at each instant the past
// could change, a change recorded where the flow left the one before it, under
// the key the duty derives. So over an item whose life the duty recorded tick
// by tick (blocked, moving once its blocker finished, waiting for a start,
// due once it passed, held by its seat's model limit, due again once the limit
// lifted), the backfill, reading the same past from the item's history, its
// blocker's and the seat's retained events alone, derives the same changes
// with the same keys, each dated by the fact that made it: the write, the
// start, the stop.
func TestABackfilledFlowIsTheRecordedFlow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	postures := &shiftingPostures{p: livePosture}
	limitFrom, limitTo, resets := 10*time.Minute, 20*time.Minute, 2*time.Hour
	past := &seatPast{covered: time.Unix(0, 0), stops: map[string][]worklet.SeatStop{}}
	duty := backfillDutyOn(t, r, postures, past)
	tick := func(now time.Time) { flowTick(t, r, duty, now) }

	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("create the blocker: %v", err)
	}
	r.drain()
	fileInProgress(t, r, "t-1", "bot", func(task *worklet.Task) {
		task.Relations = []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "t-0"}}
	})
	filed := detailOf(t, r, "t-1", time.Now()).Task.UpdatedAt
	tick(time.Now().UTC())

	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-finish", "t-0", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("finish the blocker: %v", err)
	}
	r.drain()
	finished := detailOf(t, r, "t-0", time.Now()).History[0].At
	tick(time.Now().UTC())

	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	reason := "the release's health check"
	if _, err := r.writer.UpdateTask(t.Context(), "op-park", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{StartAt: &start, WaitReason: &reason}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("park: %v", err)
	}
	r.drain()
	parked := detailOf(t, r, "t-1", time.Now()).Task.UpdatedAt
	tick(time.Now().UTC())

	tick(start.Add(time.Minute))
	stop := worklet.SeatStop{Cause: worklet.FlowCauseLimit, From: start.Add(limitFrom),
		To: start.Add(limitTo), Until: start.Add(resets)}
	past.stops["bot"] = []worklet.SeatStop{stop}
	postures.set(worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"bot": {Stop: worklet.FlowCauseLimit, StopSince: stop.From, StopUntil: stop.Until}}})
	tick(start.Add(limitFrom + time.Minute))
	postures.set(livePosture)
	tick(start.Add(limitTo + time.Minute))

	recorded := flowChanges(t, r, "t-1", false)
	states := make([]worklet.FlowState, 0, len(recorded))
	for _, e := range recorded {
		states = append(states, e.State)
	}
	want := []worklet.FlowState{worklet.FlowBlocked, worklet.FlowMoving, worklet.FlowWaiting,
		worklet.FlowDue, worklet.FlowHeld, worklet.FlowDue}
	if !slices.Equal(states, want) {
		t.Fatalf("the duty recorded %v, want %v: the case's own premise", states, want)
	}

	derived, err := duty.PastFlow(t.Context(), "t-1", start.Add(limitTo+2*time.Minute))
	if err != nil {
		t.Fatalf("read the item's past: %v", err)
	}
	if len(derived) != len(recorded) {
		t.Fatalf("the backfill derived %d changes from the item's past, %+v, where the "+
			"duty recorded %d", len(derived), derived, len(recorded))
	}
	dated := []time.Time{filed, finished, parked, start, stop.From, stop.To}
	for i, got := range derived {
		rec := recorded[i]
		if got.ID != rec.ID || got.State != rec.State || got.Cause != rec.Cause ||
			!sameAt(got.Since, rec.Since) || !sameAt(got.Until, rec.Until) || got.Seat != rec.Seat {
			t.Errorf("change %d: the backfill derived %s/%s since %v until %v on %q keyed %s, "+
				"where the duty recorded %s/%s since %v until %v on %q keyed %s", i,
				got.State, got.Cause, got.Since, got.Until, got.Seat, got.ID,
				rec.State, rec.Cause, rec.Since, rec.Until, rec.Seat, rec.ID)
		}
		if !got.Backfilled || got.Past == nil || !got.Past.At.Equal(dated[i]) || !got.Past.Seats {
			t.Errorf("change %d (%s) is dated %+v, want backfilled at %s with the seat's "+
				"events read", i, got.State, got.Past, dated[i])
		}
	}
}

func sameAt(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// THE BACKFILL RECORDS THE PAST BEFORE THE DUTY ONCE (ADR-0162).
//
// An item in progress before the duty first read it has its past recorded,
// marked backfilled, dated by that past and saying it read no seat's events
// where none were wired; the duty's own reads never take a backfilled change
// for the latest, so a hold it already told is neither recorded nor told
// again; an item that entered progress after the duty began has no past to
// fill; and a second run, here or on a node that took the duty over, writes
// nothing.
func TestTheBackfillRecordsThePastBeforeTheDutyOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("create the blocker: %v", err)
	}
	r.drain()
	fileInProgress(t, r, "t-1", "bot", func(task *worklet.Task) {
		task.Priority = worklet.PriorityUrgent
		task.Relations = []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "t-0"}}
	})
	filed := detailOf(t, r, "t-1", time.Now()).Task.UpdatedAt
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-finish", "t-0", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("finish the blocker: %v", err)
	}
	r.drain()
	finished := detailOf(t, r, "t-0", time.Now()).History[0].At

	// THE DUTY BEGINS while the seat is out of its model limit: it reads the
	// urgent item held and tells the lead.
	now := time.Now().UTC()
	held := worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"bot": {Stop: worklet.FlowCauseLimit, StopSince: now, StopUntil: now.Add(time.Hour)}}}
	duty := backfillDutyOn(t, r, fixedPostures(held), nil)
	flowTick(t, r, duty, now)
	flowTick(t, r, duty, now.Add(time.Minute))
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("the held urgent item told %v, want its lead once: the case's premise", got)
	}
	fileInProgress(t, r, "t-2", "bot", nil)
	flowTick(t, r, duty, now.Add(2*time.Minute))

	fill := duty.Backfill(t.Context(), now.Add(3*time.Minute))
	r.drain()
	if !fill.Done || fill.Past != 1 || fill.Changes != 2 || fill.Unseated != 2 {
		t.Fatalf("the backfill did %+v, want one item's past of two changes, read without "+
			"the seats' events, and the walk done", fill)
	}
	past := flowChanges(t, r, "t-1", true)
	if len(past) != 2 || past[0].State != worklet.FlowBlocked || past[1].State != worklet.FlowMoving {
		t.Fatalf("the item's backfilled flow is %+v, want blocked then moving", past)
	}
	for i, at := range []time.Time{filed, finished} {
		if !past[i].At.Equal(at) || past[i].Past == nil || !past[i].Past.At.Equal(at) ||
			past[i].Past.Seats {
			t.Errorf("backfilled change %d is dated %s with %+v, want %s and no seat's "+
				"events read", i, past[i].At, past[i].Past, at)
		}
	}
	history := detailOf(t, r, "t-1", time.Now()).FlowHistory
	if last := history[len(history)-1]; !last.Backfilled || last.State != worklet.FlowBlocked {
		t.Errorf("the item's flow history ends %+v, want its oldest change, the "+
			"backfilled blocked, however late it was published", last)
	}
	if got := flowChanges(t, r, "t-2", true); len(got) != 0 {
		t.Errorf("an item the duty read from its start was backfilled %+v", got)
	}

	// THE DUTY READS ITS OWN LATEST CHANGE, never the backfill's.
	if got := flowTick(t, r, duty, now.Add(4*time.Minute)); got.Changes != 0 || got.Notices != 0 {
		t.Errorf("after the backfill the duty recorded %d changes and told %d, want "+
			"nothing: the item is still held since the same instant", got.Changes, got.Notices)
	}
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Errorf("after the backfill the held item told %v, want the lead once", got)
	}

	// AND A SECOND RUN, on a node that took the duty over, writes nothing.
	again := backfillDutyOn(t, r, fixedPostures(held), nil)
	if fill := again.Backfill(t.Context(), now.Add(5*time.Minute)); fill.Changes != 0 || !fill.Done {
		t.Errorf("a second backfill did %+v, want nothing written and the walk done", fill)
	}
	r.drain()
	if got := flowChanges(t, r, "t-1", true); len(got) != 2 {
		t.Errorf("after a second backfill the item holds %d backfilled changes, want 2", len(got))
	}
}
