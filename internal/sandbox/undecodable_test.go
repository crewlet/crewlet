package sandbox

import (
	"bytes"
	"testing"
)

// garbled is a run record nothing can decode: written by no build, or
// corrupted on its way to the store.
var garbled = []byte("{not json")

// writeGarbled puts one such record beside the store's runs, the seat's own
// for all any reader can tell.
func writeGarbled(t *testing.T, store *CoordStore) {
	t.Helper()
	if created, err := store.runs.CreateSandboxRun(t.Context(), "t-garbled", garbled); err != nil || !created {
		t.Fatalf("CreateSandboxRun = %v, %v", created, err)
	}
}

// untouched asserts the unreadable record is exactly as it was written: no
// reader deletes or rewrites what it could not read.
func untouched(t *testing.T, store *CoordStore) {
	t.Helper()
	record, found, err := store.runs.SandboxRun(t.Context(), "t-garbled")
	if err != nil || !found || !bytes.Equal(record.Value, garbled) {
		t.Fatalf("the unreadable record is %q (found %v, err %v), want it left exactly as written",
			record.Value, found, err)
	}
}

// EVERY LISTING SKIPS A RECORD IT CANNOT DECODE and returns the rest: the
// dashboard's and the observer's (ListActive), a seat's (ListActiveForSeat)
// and a retiring mailbox's (ListActiveForSeatID). Failing the listing failed
// every reader of every seat's runs on one record.
func TestEveryListingSkipsARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	writeGarbled(t, rig.pending)
	for name, list := range map[string]func() ([]PendingRun, error){
		"all":        func() ([]PendingRun, error) { return rig.pending.ListActive(t.Context()) },
		"by handle":  func() ([]PendingRun, error) { return rig.pending.ListActiveForSeat(t.Context(), "swe") },
		"by seat id": func() ([]PendingRun, error) { return rig.pending.ListActiveForSeatID(t.Context(), sweSeat()) },
		"the epochs": func() ([]PendingRun, error) {
			return rig.pending.list(t.Context(), func(PendingRun) bool { return true })
		},
	} {
		runs, err := list()
		if err != nil || len(runs) != 1 || runs[0].TurnID != "t1" {
			t.Errorf("%s = %v, %v; want t1 alone, the unreadable record skipped", name, runs, err)
		}
	}
	untouched(t, rig.pending)
}

// THE WAITER POLLS EVERY RUN IT CAN READ beside one it cannot.
func TestTheWaiterPollsBesideARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	writeGarbled(t, rig.pending)
	if _, err := rig.waiter.pass(t.Context()); err != nil {
		t.Fatalf("the waiter's pass beside an unreadable record = %v, want it to poll t1", err)
	}
	untouched(t, rig.pending)
}

// A SEAT'S RECOVERY TAKES OVER EVERY RUN IT CAN READ beside one it cannot,
// rather than failing, which handed back every seat the node claimed.
func TestASeatRecoversBesideARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	writeGarbled(t, rig.pending)
	if err := rig.recoverSeat(t.Context(), "node-b:1", 2); err != nil {
		t.Fatalf("RecoverSeat beside an unreadable record: %v", err)
	}
	if got := rig.get("t1"); got.Owner != "node-b:1" || got.OwnerEpoch != 2 {
		t.Fatalf("t1 is owned by %q at %d, want it recovered under node-b:1 at 2", got.Owner, got.OwnerEpoch)
	}
	untouched(t, rig.pending)
}

// A REPLY STILL REACHES ITS PARKED RUN beside a record nothing can read: the
// answer lookup lists the seat's runs, and failing it handed every reply back.
func TestAnAnswerIsRecordedBesideARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.park("t1")
	writeGarbled(t, rig.pending)
	handled, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "use main", nil))
	if err != nil || handled != AnswerConsumed {
		t.Fatalf("TryResumeFromAnswer = %v, %v, want the answer recorded on t1", handled, err)
	}
	untouched(t, rig.pending)
}

// A RETIREMENT ENDS EVERY RUN IT CAN READ AND LEAVES THE ONE IT CANNOT exactly
// as it is, for the documented repair: a record it could not read is never one
// it deletes or rewrites.
func TestARetirementLeavesARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	writeGarbled(t, rig.pending)
	if err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", "retirement:1", 12); err != nil {
		t.Fatalf("RetireSeat beside an unreadable record: %v", err)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("t1 after the retirement: found %v, err %v; want it ended", found, err)
	}
	untouched(t, rig.pending)
}
