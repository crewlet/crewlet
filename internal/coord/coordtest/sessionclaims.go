package coordtest

import "github.com/crewlet/crewlet/internal/coord"

const (
	claimedSession = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"
	otherSession   = "session_7d6c5b4a-3f2e-4d1c-8b9a-07f5e3d1c9b0"
)

// claimOf reads sessionID's claim, failing the case on an error.
func (h *fleetHarness) claimOf(sessionID string) (coord.SessionClaim, bool) {
	h.t.Helper()
	c, held, err := h.f.SessionClaim(h.ctx, sessionID)
	if err != nil {
		h.t.Fatalf("SessionClaim(%s): %v", sessionID, err)
	}
	return c, held
}

var sessionClaimCases = []fleetCase{
	{name: "a session is claimed once, moved and removed only at the revision read", fn: func(h *fleetHarness) {
		for _, bad := range []coord.SessionClaim{
			{Turn: "turn-1", Launch: "job-1"}, {Session: claimedSession, Launch: "job-1"},
			{Session: claimedSession, Turn: "turn-1"},
		} {
			if ok, err := h.f.CreateSessionClaim(h.ctx, bad); err == nil || ok {
				h.t.Errorf("CreateSessionClaim(%+v) = (%v, %v), want a refusal", bad, ok, err)
			}
		}
		claim := coord.SessionClaim{Session: claimedSession, Turn: "turn-1", Launch: "job-1"}
		for i, want := range []bool{true, false} {
			if ok, err := h.f.CreateSessionClaim(h.ctx, claim); err != nil || ok != want {
				h.t.Fatalf("claim %d answered %v (%v), want %v", i, ok, err, want)
			}
		}
		read, held := h.claimOf(claimedSession)
		if !held || read.Turn != "turn-1" || read.Launch != "job-1" || read.Revision == 0 {
			h.t.Fatalf("the claim read back as %+v, %v", read, held)
		}
		if _, held := h.claimOf(otherSession); held {
			h.t.Error("a session nobody claimed reads as claimed")
		}
		moved := claim
		moved.Launch = "job-2"
		if ok, err := h.f.MoveSessionClaim(h.ctx, moved, read.Revision+100); err != nil || ok {
			h.t.Errorf("a move at a revision never read = (%v, %v), want false", ok, err)
		}
		if ok, err := h.f.MoveSessionClaim(h.ctx, moved, read.Revision); err != nil || !ok {
			h.t.Fatalf("the move at the revision read = (%v, %v)", ok, err)
		}
		again, _ := h.claimOf(claimedSession)
		if again.Launch != "job-2" || again.Revision == read.Revision {
			h.t.Errorf("the moved claim reads back as %+v, want job-2 at a new revision", again)
		}
		if ok, err := h.f.DeleteSessionClaim(h.ctx, claimedSession, read.Revision); err != nil || ok {
			h.t.Errorf("a removal at the revision before the move = (%v, %v), want false", ok, err)
		}
		if ok, err := h.f.DeleteSessionClaim(h.ctx, claimedSession, again.Revision); err != nil || !ok {
			h.t.Fatalf("the removal at the revision read = (%v, %v)", ok, err)
		}
		if _, held := h.claimOf(claimedSession); held {
			h.t.Error("a removed claim reads as held")
		}
		if ok, err := h.f.CreateSessionClaim(h.ctx, claim); err != nil || !ok {
			h.t.Errorf("a removed claim could not be made again: (%v, %v)", ok, err)
		}
	}},
	// A CLAIM CARRIES ITS RUN'S RECORD'S LIFETIME: once the record is removed
	// and the store's sweep has run, its claims are gone, and a live run's
	// stay. So a claim made by a job that crashed before its record bound the
	// session never holds the id past its run.
	//
	// Mutation: keep a claim past its run's record, or sweep a live run's, and
	// this goes red.
	{name: "a session claim goes with its run's record", fn: func(h *fleetHarness) {
		h.createRun("turn-gone", `{"status":"running"}`)
		h.createRun("turn-live", `{"status":"running"}`)
		for id, turn := range map[string]string{claimedSession: "turn-gone", otherSession: "turn-live"} {
			if ok, err := h.f.CreateSessionClaim(h.ctx, coord.SessionClaim{Session: id, Turn: turn,
				Launch: "job-1"}); err != nil || !ok {
				h.t.Fatalf("claiming %s for %s: (%v, %v)", id, turn, ok, err)
			}
		}
		h.removeRun("turn-gone")
		h.sweepAll()
		if c, held := h.claimOf(claimedSession); held {
			h.t.Errorf("the removed run's claim outlived its record: %+v", c)
		}
		if c, held := h.claimOf(otherSession); !held || c.Turn != "turn-live" {
			h.t.Errorf("the live run's claim reads as %+v, %v, want it kept", c, held)
		}
		if runs, err := h.f.SandboxRuns(h.ctx); err != nil || len(runs) != 1 || runs[0].Key != "turn-live" {
			h.t.Errorf("the runs listing is %v (%v), want the live run alone, no claim among them", runs, err)
		}
	}},
}
