package sandboxtest

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// claimed is one id the cases below bind, as a minting harness spells it.
const claimed = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"

// bindable reports whether turn's current job binds claimed, failing the case
// on any answer but a bind or a refusal.
func bindable(t *testing.T, s sandbox.PendingStore, turn string, continued bool) bool {
	t.Helper()
	job := mustGet(t, s, turn).LaunchID
	_, err := s.BindSession(t.Context(), turn, sandbox.SessionBind{Launch: job, ID: claimed, Continued: continued})
	switch {
	case err == nil:
		return true
	case errors.Is(err, sandbox.ErrSessionBindRefused):
		return false
	}
	t.Fatalf("binding %s for %s: %v", claimed, turn, err)
	return false
}

// A MINTED SESSION IS ONE LIVE RUN'S: once a job binds an id, a job of another
// run binding the same id is refused, so a box reporting an id another live
// box's harness minted takes nothing over. A relaunch of the turn that
// continues the session keeps the claim, and the record's end gives it up, the
// id free again.
//
// Mutation: bind without the claim, drop the claim on a relaunch, or keep it
// past the record's end, and a check goes red.
func testAMintedSessionIsOneLiveRuns(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-first"))
	mustLaunched(t, s, run("t-second"))
	if !bindable(t, s, "t-first", false) {
		t.Fatal("the first job could not bind a session nobody claims")
	}
	if bindable(t, s, "t-second", false) {
		t.Error("another run's job bound the session the first run's job holds")
	}
	mustBeginLaunch(t, s, run("t-first"))
	if err := s.AttachSandbox(ctx, "t-first", sandbox.BoxRef{SandboxID: "box-t-first", CommandID: "cmd-2",
		SessionID: claimed}, sandbox.Fence{}); err != nil {
		t.Fatalf("the relaunch's attach naming the session it continues: %v", err)
	}
	if !bindable(t, s, "t-first", true) {
		t.Fatal("the relaunch continuing its turn's session could not bind it")
	}
	if bindable(t, s, "t-second", false) {
		t.Error("another run's job bound the session once the first run relaunched: the claim did not move " +
			"with the turn")
	}
	if _, gone, err := end(ctx, s, "t-first", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !gone {
		t.Fatalf("ending the first run = %v, %v", gone, err)
	}
	if !bindable(t, s, "t-second", false) {
		t.Error("the session stayed claimed past the record that bound it")
	}
}

// A BIND CUT BETWEEN ITS CLAIM AND ITS RECORD LEAVES NO CLAIM PAST THE RECORD:
// the claim is taken first, so a record never binds an id its claim does not
// hold, and a claim whose record write did not follow holds the id only as long
// as the run's record does.
//
// Mutation: write the record before the claim, or leave a claim past its
// record, and this goes red.
func testABindCutBeforeItsRecordLeavesNoClaimPastIt(t *testing.T, staged *stagedRuns, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-cut"))
	mustLaunched(t, s, run("t-next"))
	staged.failNext(nil, errors.New("the node stopped before the record's write"))
	cut := mustGet(t, s, "t-cut").LaunchID
	if _, err := s.BindSession(ctx, "t-cut", sandbox.SessionBind{Launch: cut, ID: claimed}); err == nil {
		t.Fatal("a bind whose record write failed answered bound")
	}
	if got := mustGet(t, s, "t-cut").Launch.Minted; got != "" {
		t.Fatalf("the record binds %q although its write failed", got)
	}
	if bindable(t, s, "t-next", false) {
		t.Error("another run bound the id while the cut run's record still stands")
	}
	if _, gone, err := end(ctx, s, "t-cut", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !gone {
		t.Fatalf("ending the cut run = %v, %v", gone, err)
	}
	if !bindable(t, s, "t-next", false) {
		t.Error("the cut bind's claim held the id past its run's record")
	}
}

// A CLAIM WHOSE RUN HAS NO RECORD BINDS NOTHING: a store that sweeps a gone
// run's claims later than the record's removal (the KV backend's marker pass)
// still holds one for a while, and a live run's job binding the id takes it
// over, while a claim a live run's record still holds refuses.
//
// Mutation: read a gone run's claim as held, and this goes red.
func testAClaimWhoseRunHasNoRecordBindsNothing(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-live"))
	if ok, err := runs.CreateSessionClaim(ctx, coord.SessionClaim{Session: claimed, Turn: "t-swept-late",
		Launch: "job-gone"}); err != nil || !ok {
		t.Fatalf("leaving a gone run's claim = %v, %v", ok, err)
	}
	if !bindable(t, s, "t-live", false) {
		t.Fatal("a live run's job was refused an id only a gone run's claim names")
	}
	held, found, err := runs.SessionClaim(ctx, claimed)
	if err != nil || !found || held.Turn != "t-live" {
		t.Errorf("the claim reads %+v, %v, %v, want it the live run's", held, found, err)
	}
}
