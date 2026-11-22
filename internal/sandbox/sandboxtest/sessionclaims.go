package sandboxtest

import (
	"context"
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

// A BIND WHOSE RECORD WRITE FAILS RELEASES ITS CLAIM: the claim is taken
// first, so a record never binds an id its claim does not hold, and a claim
// whose record write did not follow is released at once, the id free for
// another run's job while the failed one's record still stands.
//
// Mutation: write the record before the claim, or keep the claim past the
// failed write, and this goes red.
func testABindWhoseRecordFailsReleasesItsClaim(t *testing.T, staged *stagedRuns, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t-cut"))
	mustLaunched(t, s, run("t-next"))
	staged.failNext(nil, errors.New("the record's write failed"))
	cut := mustGet(t, s, "t-cut").LaunchID
	if _, err := s.BindSession(ctx, "t-cut", sandbox.SessionBind{Launch: cut, ID: claimed}); err == nil {
		t.Fatal("a bind whose record write failed answered bound")
	}
	if got := mustGet(t, s, "t-cut").Launch.Minted; got != "" {
		t.Fatalf("the record binds %q although its write failed", got)
	}
	if !bindable(t, s, "t-next", false) {
		t.Error("the failed bind's claim still holds the id")
	}
}

// A CLAIM A CRASH LEFT NEVER BLOCKS ITS OWN JOB: a bind cut after its claim and
// before its record (its process gone, nothing released) leaves the claim with
// its owner; the same job binding again takes it as its own, another run's job
// is refused while the record stands, and the record's end frees the id.
//
// Mutation: refuse the claim's own job, or let another run's job past it, and
// this goes red.
func testAClaimACrashLeftNeverBlocksItsOwnJob(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	job := mustLaunched(t, s, run("t-crashed"))
	mustLaunched(t, s, run("t-other"))
	if ok, err := runs.CreateSessionClaim(ctx, coord.SessionClaim{Session: claimed, Turn: "t-crashed",
		Launch: job}); err != nil || !ok {
		t.Fatalf("leaving the crashed bind's claim = %v, %v", ok, err)
	}
	if bindable(t, s, "t-other", false) {
		t.Error("another run's job bound an id a live run's claim holds")
	}
	if !bindable(t, s, "t-crashed", false) {
		t.Fatal("the claim's own job was refused the id it claimed")
	}
	if _, gone, err := end(ctx, s, "t-crashed", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !gone {
		t.Fatalf("ending the crashed run = %v, %v", gone, err)
	}
	if !bindable(t, s, "t-other", false) {
		t.Error("the id stayed claimed past the record that bound it")
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

// claimOf is claimed's claim as runs hold it, failing the case on an error.
func claimOf(t *testing.T, runs coord.SandboxRuns) (coord.SessionClaim, bool) {
	t.Helper()
	held, found, err := runs.SessionClaim(t.Context(), claimed)
	if err != nil {
		t.Fatalf("reading the claim: %v", err)
	}
	return held, found
}

// A BIND WHOSE WRITE LANDED KEEPS ITS CLAIM, its answer lost or not: the record
// read again binds the id, so the claim stays; and a retry finding the record
// bound and no claim standing makes the claim again, for the job it binds.
//
// Mutation: release on any failed write, or answer a bound record without its
// claim, and a check goes red.
func testABindWhoseWriteLandedKeepsItsClaim(t *testing.T, staged *stagedRuns, s sandbox.PendingStore) {
	ctx := t.Context()
	job := mustLaunched(t, s, run("t-lost"))
	staged.landRowThenFail(errors.New("the write's answer was lost"))
	if _, err := s.BindSession(ctx, "t-lost", sandbox.SessionBind{Launch: job, ID: claimed}); err == nil {
		t.Fatal("a bind whose answer was lost answered bound")
	}
	if got := mustGet(t, s, "t-lost").Launch.Minted; got != claimed {
		t.Fatalf("the record binds %q, want the write landed", got)
	}
	held, found := claimOf(t, staged)
	if !found || held.Launch != job {
		t.Fatalf("the landed bind's claim is %+v, %v, want its job's", held, found)
	}
	if _, err := staged.records.DeleteSessionClaim(ctx, claimed, held.Revision); err != nil {
		t.Fatalf("removing the claim: %v", err)
	}
	if _, err := s.BindSession(ctx, "t-lost", sandbox.SessionBind{Launch: job, ID: claimed}); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if held, found := claimOf(t, staged); !found || held.Launch != job {
		t.Errorf("the retry left the claim %+v, %v, want it made again for its job", held, found)
	}
}

// A FAILED RELAUNCH BIND LEAVES THE CONTINUED CLAIM WITH ITS EARLIER OWNER: a
// relaunch continuing its turn's session moves the claim to its job, and where
// its record write fails the claim moves back, never removed.
//
// Mutation: remove a moved claim on a failed bind, and this goes red.
func testAFailedRelaunchBindLeavesTheContinuedClaim(t *testing.T, staged *stagedRuns, s sandbox.PendingStore) {
	ctx := t.Context()
	first := mustLaunched(t, s, run("t-again"))
	if _, err := s.BindSession(ctx, "t-again", sandbox.SessionBind{Launch: first, ID: claimed}); err != nil {
		t.Fatalf("the first job's bind: %v", err)
	}
	next := mustBeginLaunch(t, s, run("t-again"))
	if err := s.AttachSandbox(ctx, "t-again", sandbox.BoxRef{SandboxID: "box-t-again", CommandID: "cmd-2",
		SessionID: claimed}, sandbox.Fence{}); err != nil {
		t.Fatalf("the relaunch's attach naming the session it continues: %v", err)
	}
	staged.failNext(nil, errors.New("the record's write failed"))
	if _, err := s.BindSession(ctx, "t-again", sandbox.SessionBind{Launch: next, ID: claimed,
		Continued: true}); err == nil {
		t.Fatal("a relaunch bind whose record write failed answered bound")
	}
	if held, found := claimOf(t, staged); !found || held.Launch != first {
		t.Errorf("the continued session's claim is %+v, %v, want it back with job %s", held, found, first)
	}
}

// A FAILED BIND RELEASES ITS CLAIM ON A CANCELLED CALLER: the failure it
// undoes is often the caller's context ending, so the release runs on a
// context of its own.
//
// Mutation: release on the caller's context, and this goes red.
func testAFailedBindReleasesOnACancelledCaller(t *testing.T, staged *stagedRuns, s sandbox.PendingStore) {
	job := mustLaunched(t, s, run("t-cancelled"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	staged.failRowThen(context.Canceled, cancel)
	if _, err := s.BindSession(ctx, "t-cancelled", sandbox.SessionBind{Launch: job, ID: claimed}); err == nil {
		t.Fatal("a bind cut by its caller's cancel answered bound")
	}
	if held, found := claimOf(t, staged); found {
		t.Errorf("the cancelled bind left its claim %+v", held)
	}
}
