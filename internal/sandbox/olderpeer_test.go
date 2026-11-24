package sandbox_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
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
func olderReads(t *testing.T, row sandbox.PendingRun) olderRow {
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
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	job := launchedRun(t, store, "t1")
	call := sandbox.CallWait{Tool: "delegate", Seq: 3}
	if outcome, err := store.RecordAsk(ctx, "t1", job, sandbox.Ask{Call: &call}); err != nil ||
		outcome != sandbox.AskRecorded {
		t.Fatalf("parking on the call: %v, %v", outcome, err)
	}
	row, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("Get: %v, found %v", err, found)
	}
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
