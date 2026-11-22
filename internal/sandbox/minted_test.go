package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// Session ids as a minting harness spells them: Kimi's are `session_<uuid>`.
const (
	mintedSession = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"
	otherMinted   = "session_7d6c5b4a-3f2e-4d1c-8b9a-07f5e3d1c9b0"
)

// heldSessions is an executor log naming the sessions held, or failing every
// question with err.
type heldSessions struct {
	held map[string]bool
	err  error
}

func (h heldSessions) SessionHeld(_ context.Context, id string) (bool, error) {
	return h.held[id], h.err
}

// reporting has the rig's runner mint its session, its box reporting what
// report holds at each read.
func reporting(rig *coordRig, report *string) {
	rig.runner.Reports = func(context.Context, Sandbox, RunHandle) (string, error) { return *report, nil }
}

// A MINTING JOB IS BOUND AS IT IS COLLECTED, a run that made no call included:
// the session its collection read binds to its record when no turn holds it,
// and a run killed before its report collects none and binds none. An id
// another turn holds, an executor log that cannot say, and a report other than
// the one bound already each FAIL THE RUN CLOSED: no session, no capture, and
// its error saying why first.
//
// Mutation: bind without asking the executor log, take a log that cannot say
// for a fresh id, collect a report beside a bind of another, or bind a run
// that reported nothing, and a case goes red.
func TestAMintingJobIsBoundAsItIsCollected(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		holders   SessionHolders
		prebound  string
		collected string
		bound     string
		refused   string
	}{
		"a run that made no call and reported": {holders: heldSessions{}, collected: mintedSession,
			bound: mintedSession},
		"a run killed before its report": {holders: heldSessions{}},
		"an id another turn holds": {holders: heldSessions{held: map[string]bool{mintedSession: true}},
			collected: mintedSession, refused: "is another turn's"},
		"a log that cannot say": {holders: heldSessions{err: errors.New("no data node answered")},
			collected: mintedSession, refused: "could not be told"},
		"no log at all":                     {collected: mintedSession, refused: "nothing can tell"},
		"a report other than the bound":     {holders: heldSessions{}, prebound: otherMinted, collected: mintedSession, bound: otherMinted, refused: "bound session"},
		"the bound report read again":       {holders: heldSessions{}, prebound: mintedSession, collected: mintedSession, bound: mintedSession},
		"a bound job that now reports none": {holders: heldSessions{}, prebound: mintedSession, bound: mintedSession, refused: "read none"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Holders = tc.holders })
			report := tc.collected
			reporting(rig, &report)
			launched := rig.launch("t1")
			if tc.prebound != "" {
				if _, err := rig.pending.BindSession(t.Context(), "t1", SessionBind{Launch: launched.LaunchID,
					ID: tc.prebound}); err != nil {
					t.Fatalf("binding the earlier report: %v", err)
				}
			}
			collected := wholeAccount()
			collected.SessionID = tc.collected
			rig.runner.Finish(collected)
			result, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect)
			if err != nil {
				t.Fatalf("the collection: %v", err)
			}
			row := rig.get("t1")
			if row.Launch.Minted != tc.bound || row.SessionRan() != tc.bound {
				t.Errorf("the record binds %q and ran %q, want %q", row.Launch.Minted, row.SessionRan(), tc.bound)
			}
			failed := !result.Success && result.SessionID == "" && result.Captured == nil &&
				strings.HasPrefix(result.Error, ErrSessionBindRefused.Error())
			if failed != (tc.refused != "") || !strings.Contains(result.Error, tc.refused) {
				t.Errorf("the collected run failed closed: %v (success %v, session %q, %q), want %v naming %q",
					failed, result.Success, result.SessionID, result.Error, tc.refused != "", tc.refused)
			}
			if tc.refused == "" && result.SessionID != tc.collected {
				t.Errorf("the collected session is %q, want %q", result.SessionID, tc.collected)
			}
		})
	}
}

// A MINTING JOB IS SAVED ONLY ONCE ITS REPORT IS BOUND: a save before the box
// reports its session is skipped, saying no session was reported yet, and
// packs nothing; the next save, after the report, binds it and is taken.
//
// Mutation: pack before binding, or save a job that reported nothing, and this
// goes red.
func TestAMintingJobIsSavedOnlyOnceItsReportIsBound(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) {
			taking(thisProcess, newTakerStore())(o)
			o.Saves = memory.NewFleet()
			o.Holders = heldSessions{}
		})
		rig.runner.SavesSessions = true
		report := ""
		reporting(rig, &report)
		rig.launchKept(t, "t1")
		if out := saveOf(rig, "t1", nil); out.class != saveNothing || !strings.Contains(out.why, "no session reported yet") {
			t.Fatalf("a save before the report came to %+v, want it skipped as reporting no session yet", out)
		}
		if takes := rig.runner.Takes(); len(takes) != 0 || rig.get("t1").Launch.Minted != "" {
			t.Fatalf("a save before the report took %+v and bound %q, want nothing", takes,
				rig.get("t1").Launch.Minted)
		}
		report = mintedSession
		if out := saveOf(rig, "t1", nil); out.class != saveLanded {
			t.Fatalf("the save after the report came to %+v, want it landed", out)
		}
		if bound := rig.get("t1").Launch.Minted; bound != mintedSession {
			t.Errorf("the save bound %q, want the reported %s", bound, mintedSession)
		}
	})
}

// A LOST MINTING JOB BINDS ITS REPORT BEFORE ITS SESSION IS TAKEN, as a
// recovered job's loss read does: a box that reported binds its id and is
// taken; one that reported nothing, or is gone, leaves the job sessionless, so
// nothing relaunches as though a session existed.
//
// Mutation: take a lost job's box before binding, or bind a box that reported
// nothing, and a case goes red.
func TestALostMintingJobBindsItsReportBeforeItsSessionIsTaken(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		report string
		gone   bool
		bound  string
	}{
		"a box that reported":         {report: mintedSession, bound: mintedSession},
		"a box that reported nothing": {},
		"a box that is gone":          {report: mintedSession, gone: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Holders = heldSessions{} })
				report := tc.report
				reporting(rig, &report)
				launched := rig.launchKept(t, "t1")
				if tc.gone {
					if err := rig.provider.Kill(t.Context(), launched.SandboxID); err != nil {
						t.Fatalf("Kill: %v", err)
					}
				}
				spooled, kept, err := rig.coordinator.readLost(t.Context(), rig.get("t1"))
				if err != nil {
					t.Fatalf("the lost run's read: %v", err)
				}
				rig.coordinator.keepLater(t.Context(), spooled)
				rig.awaitKeeps()
				row := rig.get("t1")
				if row.Launch.Minted != tc.bound || row.SessionRan() != tc.bound {
					t.Errorf("the record binds %q and ran %q, want %q", row.Launch.Minted, row.SessionRan(), tc.bound)
				}
				if took := len(rig.runner.Takes()) > 0; took != (tc.bound != "") {
					t.Errorf("the lost box was taken: %v (kept %+v), want %v", took, kept, tc.bound != "")
				}
			})
		})
	}
}
