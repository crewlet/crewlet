package sandbox

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// olderRow is a run's row as cf65c6ae22 reads it, the last build before a job
// parked on its own calls: the older peer a rolling upgrade mixes with this
// build. PINNED to the keys that build decides on, never this build's
// PendingRun; every other key rides its PendingRun.Extra unread.
type olderRow struct {
	Status   string           `json:"status"`
	Question string           `json:"question"`
	WaitsFor *olderWait       `json:"waits_for"`
	HeldCall *json.RawMessage `json:"held_call"`
	Answer   *json.RawMessage `json:"answer"`
	Ending   *json.RawMessage `json:"ending"`
}

// olderWait is that build's RunWait: a wait on the run Turn names.
type olderWait struct {
	Turn string `json:"turn"`
	Seat string `json:"seat"`
}

// olderAwaiting is that build's sandbox.Awaiting.
var olderAwaiting = []string{"awaiting", "reseed"}

// olderReads decodes row's bytes as that build does.
func olderReads(t *testing.T, row PendingRun) olderRow {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encoding the row: %v", err)
	}
	var older olderRow
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatalf("the older peer cannot decode the row: %v", err)
	}
	return older
}

// waitsNow is that build's internal/engine waitsNow: the watch answers a row
// it holds true for as a wait on WaitsFor.Turn, and one naming no run as that
// run gone ("The run you waited for, , left its seat: gone.").
func (r olderRow) waitsNow() bool {
	return r.WaitsFor != nil && r.Answer == nil && r.Ending == nil && slices.Contains(olderAwaiting, r.Status)
}

// parks is that build's PendingRun.asked, which decides that a collected job
// parks rather than ends ([Coordinator.OnCompleted] there).
func (r olderRow) parks() bool { return r.Question != "" }

// putTo is that build's internal/engine audienceResolver.ResolveAudience: a
// wait to nobody, a held call to who may approve it, every other question to
// the seat's people (its label resolved, or the lead chain as the fallback).
func (r olderRow) putTo() string {
	switch {
	case r.WaitsFor != nil:
		return "nobody"
	case r.HeldCall != nil:
		return "approvers"
	}
	return "people"
}

// AN OLDER PEER NEVER READS A CALL PARK AS A WAIT ON NO RUN. A job parked on
// its own calls is marked in its parked calls, never in WaitsFor, so a holder
// on the build before this one, collecting or recovering it in a rolling
// upgrade, does not answer it as a wait on the run "" gone. It parks it as the
// question its row names, put to the seat's people, whose answer is a real
// person's answer, until a holder on this build carries it on.
//
// Mutation: write a WaitsFor beside the mark in RecordAsk, and the older
// peer's watch answers the park as a run gone, and puts it to nobody.
func TestAnOlderPeerNeverReadsACallParkAsAWaitOnNoRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnItsCalls(t, rig)
	row := rig.get("t1")
	if !row.WaitsOnOwnCalls() {
		t.Fatalf("the row is no park on its own calls: %+v", row.ParkedCalls)
	}
	for _, status := range olderAwaiting {
		row.Status = status
		older := olderReads(t, row)
		if older.waitsNow() {
			t.Errorf("%s: the older peer's watch reads the call park as a wait on run %q", status,
				older.WaitsFor.Turn)
		}
		if !older.parks() || older.putTo() != "people" {
			t.Errorf("%s: the older peer parks it %v, put to %s; want parked, put to people", status,
				older.parks(), older.putTo())
		}
	}
}

// olderControlTypes is every type that build's Coordinator.OnEvent acts on off
// a seat's control topic; it returns nil for any other, which its
// internal/engine controlOutcome acknowledges: dropped, unrecorded.
var olderControlTypes = []string{"executor_launched", "executor_finished", "executor_answer_given"}

// AN OLDER PEER NEVER SEES THE ENGINE'S ANSWER AS A PERSON'S. The engine
// carries a run parked on its own calls, or on a run, on by a wake of a type
// that build does not act on, so an older holder drops it rather than record
// a person's answer; and the run's row, at every step of being carried on
// (claimed, taken by its turn, given back by a deferred resume), holds no
// answer that build would open and tell as "the person you asked answered".
//
// Mutation: deliver the wake as an executor_answer_given (or record the
// carrying on as an answer on the row), and the older peer reads a person's
// answer.
func TestAnOlderPeerNeverSeesTheEnginesAnswerAsAPersons(t *testing.T) {
	t.Parallel()
	for name, park := range map[string]func(*testing.T, *coordRig){
		"its own calls": parkOnItsCalls,
		"a run":         parkOnARunGone,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			park(t, rig)
			var rows []PendingRun
			rig.resumer.beforeBegin = func(context.Context) { rows = append(rows, rig.get("t1")) }
			rig.resumer.during = func(_ context.Context, _ PendingRun) { rows = append(rows, rig.get("t1")) }
			wake := wakeOf(t, rig)
			if slices.Contains(olderControlTypes, wake.Type) {
				t.Fatalf("the engine's answer travels as %s, which the older peer records", wake.Type)
			}
			rig.resumer.failWith(errNotYet)
			if err := rig.coordinator.OnEvent(t.Context(), wake); err == nil {
				t.Fatal("a resume that failed spent the wake")
			}
			rows = append(rows, rig.get("t1"))
			rig.resumer.failWith(nil)
			if err := rig.coordinator.OnEvent(t.Context(), wakeOf(t, rig)); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			if calls := rig.resumer.calls(); len(calls) != 1 || calls[0].Asked == nil ||
				calls[0].Asked.By != "" || calls[0].Asked.Answer != "" {
				t.Fatalf("resumed %+v, want the run carried on once with no person's answer", calls)
			}
			if len(rows) < 3 {
				t.Fatalf("read %d rows along the way, want the claim, the take and the give-back", len(rows))
			}
			for i, row := range rows {
				if older := olderReads(t, row); older.Answer != nil {
					t.Errorf("row %d (%s): the older peer finds an answer on it to tell as a person's",
						i, row.Status)
				}
			}
		})
	}
}

// errNotYet is a resume that fails before its turn, giving its claim back.
var errNotYet = errorString("the model provider did not answer")

// errorString is an error that is its text.
type errorString string

func (e errorString) Error() string { return string(e) }

// wakeOf is the wake the engine's watch puts on the seat's control topic for
// t1, its wait over, as the older peer's queue hands it on: decoded off the
// wire.
func wakeOf(t *testing.T, rig *coordRig) *events.Event {
	t.Helper()
	rig.queue.mu.Lock()
	before := len(rig.queue.published)
	rig.queue.mu.Unlock()
	desk := AnswerDesk{Pending: rig.pending, Queue: rig.queue}
	if err := desk.DeliverWaitOver(t.Context(), rig.get("t1")); err != nil {
		t.Fatalf("DeliverWaitOver: %v", err)
	}
	rig.queue.mu.Lock()
	published := slices.Clone(rig.queue.published[before:])
	rig.queue.mu.Unlock()
	if len(published) != 1 || published[0].topic != controlSubject(rig.get("t1").AgentID) {
		t.Fatalf("published %+v, want one wake on the seat's control topic", published)
	}
	raw, err := json.Marshal(published[0].event)
	if err != nil {
		t.Fatal(err)
	}
	var wire events.Event
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return &wire
}

// parkOnARunGone parks t1 by `wait_for_run` on a run whose row is gone, and
// has the rig's coordinator read it as gone: a wait the engine carries on.
func parkOnARunGone(t *testing.T, rig *coordRig) {
	t.Helper()
	rig.coordinator.waitedFor = func(_ context.Context, run PendingRun) (types.RunWaited, bool) {
		return types.RunWaited{Turn: run.WaitsFor.Turn, Seat: run.WaitsFor.Seat, Left: types.RunLeftGone,
			Pushed: types.RunPushedUnknown}, true
	}
	rig.launch("t1")
	rig.ask("t1", Ask{Question: RunWaitQuestion("it pushes first"), Wait: &RunWait{
		Turn: "0190f3a8-0000-7000-8000-0000000000a1", Seat: "swe", Reason: "it pushes first"}})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.WaitsFor == nil {
		t.Fatalf("status = %q, waits for %+v; want the run parked on its wait", got.Status, got.WaitsFor)
	}
}
