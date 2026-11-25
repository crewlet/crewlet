package sandboxtest

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A JOB'S BOX SAYS ONCE WHICH SESSION IT STARTED, AND ONLY FOR ITS OWN JOB.
// The first record stands — a job's box starts one session, and a second word,
// either way, is no truer than the first — a record naming a job the row has
// moved on from is nothing, as is one for a run with no record, a start that is
// no session is refused, and the next launch's job starts with nothing said:
// what one job's box started is not the next's.
//
// Mutation: let a second record write over the first, record it on another
// job's row, or carry it across a relaunch, and a check goes red.
func testABoxsSessionStartIsRecordedOnceForItsOwnJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-start"))
	if ok, err := s.MarkSessionStart(ctx, "t-start", "another-job", sandbox.SessionStartFresh); err != nil || ok {
		t.Errorf("a start for a job the record does not hold = %v, %v", ok, err)
	}
	if _, err := s.MarkSessionStart(ctx, "t-start", launch, "resumed"); err == nil {
		t.Error("a start that is no session was taken")
	}
	if ok, err := s.MarkSessionStart(ctx, "t-start", launch, sandbox.SessionStartFresh); err != nil || !ok {
		t.Fatalf("MarkSessionStart = %v, %v", ok, err)
	}
	if ok, err := s.MarkSessionStart(ctx, "t-start", launch, sandbox.SessionStartContinued); err != nil || ok {
		t.Errorf("a second start wrote over the first: %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t-start").Launch.SessionStart; got != sandbox.SessionStartFresh {
		t.Errorf("the record says %q, want the first word, %q", got, sandbox.SessionStartFresh)
	}
	mustBeginLaunch(t, s, run("t-start"))
	if got := mustGet(t, s, "t-start").Launch.SessionStart; got != "" {
		t.Errorf("the next job's record says %q, want nothing said", got)
	}
	if ok, err := s.MarkSessionStart(ctx, "t-none", "job-1", sandbox.SessionStartContinued); err != nil || ok {
		t.Errorf("a start for a run with no record = %v, %v", ok, err)
	}
}

// A JOB'S HARNESS CONNECTING ITS BRIDGE IS RECORDED ONCE, AND ONLY FOR ITS OWN
// JOB: a record naming a job the row has moved on from is nothing, as is one for
// a run with no record, and the next launch's job starts with nothing said, so a
// relaunch whose harness never connects is not read as one that did.
//
// Mutation: record it on another job's row, answer a second record as taken, or
// carry it across a relaunch, and a check goes red.
func testAHarnessConnectingItsBridgeIsRecordedOnceForItsOwnJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-bridge"))
	if mustGet(t, s, "t-bridge").Launch.BridgeConnected {
		t.Fatal("a job's record says its harness connected its bridge before anything said so")
	}
	if ok, err := s.MarkBridgeConnected(ctx, "t-bridge", "another-job"); err != nil || ok {
		t.Errorf("a connect for a job the record does not hold = %v, %v", ok, err)
	}
	if mustGet(t, s, "t-bridge").Launch.BridgeConnected {
		t.Error("another job's connect was recorded on this one")
	}
	if ok, err := s.MarkBridgeConnected(ctx, "t-bridge", launch); err != nil || !ok {
		t.Fatalf("MarkBridgeConnected = %v, %v", ok, err)
	}
	if ok, err := s.MarkBridgeConnected(ctx, "t-bridge", launch); err != nil || ok {
		t.Errorf("a second connect was recorded again: %v, %v", ok, err)
	}
	got := mustGet(t, s, "t-bridge")
	if !got.Launch.BridgeConnected || got.Handle().BridgeUnconnected {
		t.Errorf("the record says connected %v, its handle unconnected %v; want connected",
			got.Launch.BridgeConnected, got.Handle().BridgeUnconnected)
	}
	mustBeginLaunch(t, s, run("t-bridge"))
	if next := mustGet(t, s, "t-bridge"); next.Launch.BridgeConnected || !next.Handle().BridgeUnconnected {
		t.Error("the next job's record says its harness connected its bridge, which only the job before did")
	}
	if ok, err := s.MarkBridgeConnected(ctx, "t-none", "job-1"); err != nil || ok {
		t.Errorf("a connect for a run with no record = %v, %v", ok, err)
	}
}

// A MINTED SESSION IS BOUND ONCE, TO ITS OWN JOB, and only as its box could
// have started it: a fresh job binds any id but the one its launch asked it to
// continue, a job that continued binds that one alone, and once bound the same
// id answers as bound while any other is refused. Another job's record, a gone
// one and no id a harness names are refused, and the next launch's job starts
// with nothing bound. The bound id is the session the job ran.
//
// Mutation: let a second id write over the first, bind on another job's row,
// take a resume's other id or a fall back's continued one, or carry the bind
// across a relaunch, and a check goes red.
func testAMintedSessionIsBoundOnceToItsOwnJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const (
		first  = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"
		second = "session_7d6c5b4a-3f2e-4d1c-8b9a-07f5e3d1c9b0"
		// Each turn below continues a session of its own: an id one run binds,
		// no other run binds while it lives (testAMintedSessionIsOneLiveRuns).
		resumed = "session_2c4e6a8b-1d3f-4b5c-9e7a-0b1c2d3e4f5a"
		fellOff = "session_3d5f7b9c-2e4a-4c6d-8f0b-1c2d3e4f5a6b"
	)
	refused := func(name, turn string, bind sandbox.SessionBind) {
		t.Helper()
		if _, err := s.BindSession(ctx, turn, bind); !errors.Is(err, sandbox.ErrSessionBindRefused) {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
	launch := mustLaunched(t, s, run("t-mint"))
	refused("another job", "t-mint", sandbox.SessionBind{Launch: "another-job", ID: first})
	refused("no id a harness names", "t-mint", sandbox.SessionBind{Launch: launch, ID: "../x"})
	refused("a run with no record", "t-none", sandbox.SessionBind{Launch: launch, ID: first})
	for range 2 {
		bound, err := s.BindSession(ctx, "t-mint", sandbox.SessionBind{Launch: launch, ID: first})
		if err != nil || bound.Launch.Minted != first {
			t.Fatalf("BindSession = %q, %v, want %s bound", bound.Launch.Minted, err, first)
		}
	}
	refused("a second id", "t-mint", sandbox.SessionBind{Launch: launch, ID: second})
	if got := mustGet(t, s, "t-mint"); got.Launch.Minted != first || got.SessionRan() != first {
		t.Errorf("the record binds %q and ran %q, want %s for both", got.Launch.Minted, got.SessionRan(), first)
	}
	mustBeginLaunch(t, s, run("t-mint"))
	if got := mustGet(t, s, "t-mint").Launch.Minted; got != "" {
		t.Errorf("the next job's record binds %q, want nothing bound", got)
	}

	// continuing is turn's job, its attach naming the session its launch continues.
	continuing := func(turn, session string) string {
		t.Helper()
		job := mustLaunched(t, s, run(turn))
		if err := s.AttachSandbox(t.Context(), turn, sandbox.BoxRef{SandboxID: "box-" + turn, CommandID: "cmd-1",
			SessionID: session}, sandbox.Fence{}); err != nil {
			t.Fatalf("the attach naming the continued session: %v", err)
		}
		return job
	}
	job := continuing("t-resume", resumed)
	refused("a resume reporting another", "t-resume", sandbox.SessionBind{Launch: job, ID: second, Continued: true})
	refused("a fall back reporting the continued", "t-resume", sandbox.SessionBind{Launch: job, ID: resumed})
	if bound, err := s.BindSession(ctx, "t-resume", sandbox.SessionBind{Launch: job, ID: resumed,
		Continued: true}); err != nil || bound.Launch.Minted != resumed {
		t.Errorf("a resume binding the session it continued = %q, %v", bound.Launch.Minted, err)
	}
	job = continuing("t-fell-back", fellOff)
	if bound, err := s.BindSession(ctx, "t-fell-back", sandbox.SessionBind{Launch: job, ID: second}); err != nil ||
		bound.Launch.Minted != second {
		t.Errorf("a fall back binding a session of its own = %q, %v", bound.Launch.Minted, err)
	}
}

// A JOB ITS TURN'S CAP ENDED IS RECORDED SO ONCE, AND ONLY FOR ITS OWN JOB. The
// collection that saw the job still working past its cap records it on the
// claimed row before it ends the job, and every later collection of that job
// reads it there: a record naming a job the row has moved on from is nothing,
// as is one for a run with no record; a second record changes nothing; and the
// next launch's job starts with nothing said, since what one job's cap ended is
// not the next's.
//
// Mutation: record it on another job's row, or carry it across a relaunch, and
// a check goes red.
func testAJobItsCapEndedIsRecordedOnceForItsOwnJob(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-capped"))
	mustClaim(t, s, "t-capped")
	if ok, err := s.MarkEndedAtDeadline(ctx, "t-capped", "another-job"); err != nil || ok {
		t.Errorf("the cap's end of a job the record does not hold = %v, %v", ok, err)
	}
	if ok, err := s.MarkEndedAtDeadline(ctx, "t-capped", launch); err != nil || !ok {
		t.Fatalf("MarkEndedAtDeadline = %v, %v", ok, err)
	}
	if ok, err := s.MarkEndedAtDeadline(ctx, "t-capped", launch); err != nil || ok {
		t.Errorf("a second record of the cap's end = %v, %v, want nothing written", ok, err)
	}
	if !mustGet(t, s, "t-capped").Launch.EndedAtDeadline {
		t.Fatal("the job's record does not say its turn's cap ended it")
	}
	mustBeginLaunch(t, s, run("t-capped"))
	if mustGet(t, s, "t-capped").Launch.EndedAtDeadline {
		t.Error("the next job's record says its turn's cap ended it, want nothing said")
	}
	if ok, err := s.MarkEndedAtDeadline(ctx, "t-none", "job-1"); err != nil || ok {
		t.Errorf("the cap's end of a run with no record = %v, %v", ok, err)
	}
}

// HOW A JOB'S HARNESS ENDED IS RECORDED FOR ITS OWN JOB, AND CUT OFF STANDS. The
// read that collects a job records on the claimed row whether its harness ended
// its own run, and a relaunch that carries the round on in the job's box reads
// it there: a record naming a job the row has moved on from is nothing, as is
// one for a run with no record; the same end again changes nothing; a cut-off
// end is recorded over an own run, and an own run never over a cut-off end,
// since a later read must never call whole a copy an earlier one found cut off;
// an end this build does not know is refused; and the next launch's job starts
// with nothing said, since how one job's harness ended is not the next's.
//
// Mutation: record an own run over a cut-off end, record it on another job's
// row, or carry it across a relaunch, and a check goes red.
func testAJobsHarnessEndIsRecordedForItsOwnJobAndCutOffStands(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-ended"))
	mustClaim(t, s, "t-ended")
	mark := func(job string, end sandbox.HarnessEnd) bool {
		t.Helper()
		ok, err := s.MarkHarnessEnd(ctx, "t-ended", job, sandbox.EndRead{End: end})
		if err != nil {
			t.Fatalf("MarkHarnessEnd(%s, %q): %v", job, end, err)
		}
		return ok
	}
	recorded := func() sandbox.HarnessEnd {
		t.Helper()
		return mustGet(t, s, "t-ended").Launch.HarnessEnd
	}
	if mark("another-job", sandbox.HarnessEndOwnRun) || recorded() != "" {
		t.Errorf("an end of a job the record does not hold was written: %q", recorded())
	}
	if !mark(launch, sandbox.HarnessEndOwnRun) || recorded() != sandbox.HarnessEndOwnRun {
		t.Fatalf("the job's record says %q, want its harness's own run", recorded())
	}
	if mark(launch, sandbox.HarnessEndOwnRun) {
		t.Error("the same end recorded again changed the record")
	}
	if !mark(launch, sandbox.HarnessEndCutOff) || recorded() != sandbox.HarnessEndCutOff {
		t.Fatalf("the job's record says %q after a read found its harness cut off, want cut off", recorded())
	}
	if mark(launch, sandbox.HarnessEndOwnRun) || recorded() != sandbox.HarnessEndCutOff {
		t.Errorf("an own run was recorded over a cut-off end: the record says %q", recorded())
	}
	if _, err := s.MarkHarnessEnd(ctx, "t-ended", launch, sandbox.EndRead{End: "saved"}); err == nil {
		t.Error("an end this build does not know was taken")
	}
	mustBeginLaunch(t, s, run("t-ended"))
	if got := recorded(); got != "" {
		t.Errorf("the next job's record says %q, want nothing said", got)
	}
	if ok, err := s.MarkHarnessEnd(ctx, "t-none", "job-1", sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil ||
		ok {
		t.Errorf("an end of a run with no record = %v, %v", ok, err)
	}
}

// HOW A JOB'S HARNESS ENDED RIDES EVERY WRITE OF THE JOB'S RECORD UNTIL THE NEXT
// LAUNCH. A person's answer can come days after the collection that recorded
// it, and the record is rewritten whole by every write in between — a status, a
// lease, a bridged call, a claim and its release, a park — each of which must
// write the end back as it read it, on the wire under its own key, so the
// relaunch the answer drives reads what the collection found; only the next
// launch, which replaces the job's record whole, says nothing again.
//
// Mutation: leave the field off the wire (`json:"-"`), or rebuild the job's
// record in a write that is not a relaunch's, and a check goes red.
func testAJobsHarnessEndRidesEveryWriteOfItsRecord(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-ended-writes"
	launch := mustLaunched(t, s, run(turn))
	if ok, err := s.MarkHarnessEnd(ctx, turn, launch, sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil ||
		!ok {
		t.Fatalf("MarkHarnessEnd = %v, %v", ok, err)
	}
	check := func(step string) {
		t.Helper()
		if got := mustGet(t, s, turn).Launch.HarnessEnd; got != sandbox.HarnessEndCutOff {
			t.Errorf("after %s the job's record says %q, want the cut-off end the collection recorded",
				step, got)
		}
	}
	check("the record")
	if err := s.SetStatus(ctx, turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write")
	if _, err := s.ClaimOwnership(ctx, turn, "node-b", 2); err != nil {
		t.Fatal(err)
	}
	check("a lease")
	if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_page"}}); err != nil {
		t.Fatal(err)
	}
	check("a bridged call")
	claimed := mustClaim(t, s, turn)
	check("the claim")
	mustRelease(t, s, claimed)
	check("its release")
	park(t, s, turn)
	check("a park")
	mustBeginLaunch(t, s, run(turn))
	if got := mustGet(t, s, turn).Launch.HarnessEnd; got != "" {
		t.Errorf("the next launch's job reads %q, the job before's end, want nothing said", got)
	}
}

// A RELEASE RECORDS HOW THE JOB'S HARNESS ENDED BY THE RECORD'S OWN RULE, in the
// write that returns the row to running ([sandbox.Release.HarnessEnd]): an end
// over nothing is recorded, a cut-off end over an own run too, and an own run
// over a recorded cut-off leaves the cut-off standing.
//
// Mutation: write the release's end over the record's whenever it carries one,
// and the cut-off is lost.
func testAReleaseRecordsTheHarnessEndByTheRecordsRule(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-released-end"
	mustLaunched(t, s, run(turn))
	for _, step := range []struct{ carries, want sandbox.HarnessEnd }{
		{sandbox.HarnessEndOwnRun, sandbox.HarnessEndOwnRun},
		{sandbox.HarnessEndCutOff, sandbox.HarnessEndCutOff},
		{sandbox.HarnessEndOwnRun, sandbox.HarnessEndCutOff},
	} {
		release := releaseOf(mustClaim(t, s, turn))
		release.HarnessEnd = step.carries
		if released, err := s.ReleaseClaim(ctx, turn, release); err != nil || !released {
			t.Fatalf("a release carrying %q = %v, %v", step.carries, released, err)
		}
		if got := mustGet(t, s, turn).Launch.HarnessEnd; got != step.want {
			t.Fatalf("a release carrying %q left the record saying %q, want %q", step.carries, got, step.want)
		}
	}
}

func testCollectPublishesThePhase(t *testing.T, s sandbox.PendingStore) {
	// THE DURABLE HALF OF PUBLISHING A COLLECTED RUN'S PHASE ONCE, and of
	// what that record states. The launch dates the job on the store's own
	// clock and names its model; the suspension files it under the
	// executor's iteration; the release that reopens the run to a retry
	// carries whether its record went out, and a later release that says
	// nothing never takes that back. A second launch is a second job with a
	// record of its own.
	//
	// IN A BUBBLE, so the store's clock moves only by the case's own wait: each
	// job is dated exactly the instant it launched, the second a second after
	// the first, where the machine's clock held a date only to an order a
	// coarse clock stamps equal and a stepped one reverses.
	synctest.Test(t, func(t *testing.T) {
		launched := time.Now().UTC()
		r := run("t1")
		r.Launch = sandbox.LaunchRecord{Model: "claude-sonnet-5",
			// Not the caller's to choose: the store dates the job.
			StartedAt: base, Published: true, Iteration: 9}
		mustLaunched(t, s, r)
		got := mustGet(t, s, "t1")
		facts := got.Launch
		if !facts.StartedAt.Equal(launched) {
			t.Errorf("the job is dated %s, not the instant it launched, %s", facts.StartedAt, launched)
		}
		if facts.Model != "claude-sonnet-5" || facts.Iteration != suspension().Iteration || facts.Published {
			t.Errorf("the launch record = %+v, want the model, iteration %d and nothing published",
				facts, suspension().Iteration)
		}

		claimed := mustClaim(t, s, "t1")
		release := releaseOf(claimed)
		release.Published = true
		if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || !released {
			t.Fatalf("release: released=%v err=%v", released, err)
		}
		retry := mustClaim(t, s, "t1")
		if !retry.Launch.Published {
			t.Fatal("the retry's claim came back without the publish the first attempt made")
		}
		mustRelease(t, s, retry)
		if !mustGet(t, s, "t1").Launch.Published {
			t.Error("a release that carried no publish erased the record of one")
		}

		// THE SECOND JOB A SECOND LATER, on the bubble's clock.
		time.Sleep(time.Second)
		relaunched := launched.Add(time.Second)
		mustBeginLaunch(t, s, run("t1"))
		next := mustGet(t, s, "t1")
		if f := next.Launch; f.Published || f.Iteration != 0 || !f.StartedAt.Equal(relaunched) {
			t.Errorf("the second job, launched %s, carries %+v: want a record of its own, dated then",
				relaunched, f)
		}
	})
}
