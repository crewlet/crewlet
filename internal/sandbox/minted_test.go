package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
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
// another turn holds and a report other than the one bound already each FAIL
// THE RUN CLOSED: no session, no capture, and its error saying why first. An
// executor log that cannot say leaves the bind UNDECIDED: the read is handed
// back to be made again, nothing bound, nothing dropped, and no claim kept.
//
// Mutation: bind without asking the executor log, read a log that cannot say
// as a refusal or an answer, collect a report beside a bind of another, or
// bind a run that reported nothing, and a case goes red.
func TestAMintingJobIsBoundAsItIsCollected(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		holders   SessionHolders
		prebound  string
		collected string
		bound     string
		refused   string
		undecided bool
	}{
		"a run that made no call and reported": {holders: heldSessions{}, collected: mintedSession,
			bound: mintedSession},
		"a run killed before its report": {holders: heldSessions{}},
		"an id another turn holds": {holders: heldSessions{held: map[string]bool{mintedSession: true}},
			collected: mintedSession, refused: "is another turn's"},
		"a log that cannot say": {holders: heldSessions{err: errors.New("no data node answered")},
			collected: mintedSession, undecided: true},
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
			if tc.undecided {
				_, claimed, _ := rig.pending.runs.SessionClaim(t.Context(), tc.collected)
				if err == nil || errors.Is(err, ErrSessionBindRefused) || rig.get("t1").Launch.Minted != "" || claimed {
					t.Errorf("a log that cannot say answered the read %v, bound %q, claim kept %v: want it handed "+
						"back undecided, nothing bound or claimed", err, rig.get("t1").Launch.Minted, claimed)
				}
				return
			}
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

// claimSeen is an executor log that names no session, recording for each id it
// is asked whether the store held a claim of it at that moment.
type claimSeen struct {
	runs interface {
		SessionClaim(context.Context, string) (coord.SessionClaim, bool, error)
	}
	mu      sync.Mutex
	claimed []bool
}

func (c *claimSeen) SessionHeld(ctx context.Context, id string) (bool, error) {
	_, held, err := c.runs.SessionClaim(ctx, id)
	c.mu.Lock()
	c.claimed = append(c.claimed, held)
	c.mu.Unlock()
	return false, err
}

// A FRESH ID IS CLAIMED BEFORE THE EXECUTOR LOG IS ASKED: a second job asking
// the log in the window before the first job's claim would find neither the
// claim nor the log guarding the id. So the log is asked only once the claim
// stands.
//
// Mutation: ask the log before the claim, and this goes red.
func TestAFreshIDIsClaimedBeforeTheLogIsAsked(t *testing.T) {
	t.Parallel()
	seen := &claimSeen{}
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Holders = seen })
	seen.runs = rig.pending.runs
	report := mintedSession
	reporting(rig, &report)
	launched := rig.launch("t1")
	if _, err := rig.coordinator.bindReported(t.Context(), launched, mintedSession); err != nil {
		t.Fatalf("the bind: %v", err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.claimed) != 1 || !seen.claimed[0] {
		t.Errorf("the log was asked with the id claimed: %v, want once, claimed", seen.claimed)
	}
}

// loggedSessions is the executor log a keeping rig's writes stand for: a
// session is held once a capture naming it has landed there.
type loggedSessions struct{ writes *sessionWrites }

func (l loggedSessions) SessionHeld(_ context.Context, id string) (bool, error) {
	l.writes.mu.Lock()
	defer l.writes.mu.Unlock()
	for _, c := range l.writes.captures {
		if c.err == nil && c.Stamp.SessionID == id {
			return true, nil
		}
	}
	return false, nil
}

// A MINTED ID IS NEVER UNGUARDED: the claim holds it from the bind until the
// executor log names it, and is released then, the log's seek holding it
// after. At each step a second run's job binding the id is refused.
//
// Mutation: release the claim before the capture lands, or keep it after, and
// a step goes red.
func TestAMintedIDIsHeldByItsClaimUntilTheLogNamesIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRigWith(t, nil)
		rig.coordinator.holders = loggedSessions{writes: writes}
		report := mintedSession
		reporting(rig, &report)
		rig.launchKept(t, "t1")
		rig.launchKept(t, "t2")
		second := func(step string) {
			t.Helper()
			if _, err := rig.coordinator.bindReported(t.Context(), rig.get("t2"), mintedSession); !errors.Is(err,
				ErrSessionBindRefused) {
				t.Errorf("%s: a second run's job binding the id answered %v, want it refused", step, err)
			}
		}
		if _, err := rig.coordinator.bindReported(t.Context(), rig.get("t1"), mintedSession); err != nil {
			t.Fatalf("the first job's bind: %v", err)
		}
		second("bound, before its capture")
		spooled, _, err := rig.coordinator.readLost(t.Context(), rig.get("t1"))
		if err != nil {
			t.Fatalf("the lost run's read: %v", err)
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
		if held, _ := (loggedSessions{writes: writes}).SessionHeld(t.Context(), mintedSession); !held {
			t.Fatal("the capture never landed on the log, so this proves nothing of the release")
		}
		if _, claimed, _ := rig.pending.runs.SessionClaim(t.Context(), mintedSession); claimed {
			t.Error("the claim outlived the capture's landing on the log")
		}
		second("captured, its claim released")
	})
}

// A RESUME BINDS BY THE ENGINE'S WORD ON WHETHER IT CONTINUED: the token its
// box dialled ([LaunchRecord.SessionStart]), never the id it reported. A box
// that took the continued session up binds that one alone, unasked of the log
// that holds it; one that started its own binds any id but that one.
//
// Mutation: read whether a launch continued off the reported id, and a case
// goes red.
func TestAResumeBindsByTheEnginesWordOnWhetherItContinued(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		start    SessionStart
		reported string
		binds    bool
	}{
		"took it up, reporting it":           {SessionStartContinued, mintedSession, true},
		"took it up, reporting another":      {SessionStartContinued, otherMinted, false},
		"started its own, reporting the old": {SessionStartFresh, mintedSession, false},
		"started its own, reporting its own": {SessionStartFresh, otherMinted, true},
		"said nothing, reporting the old":    {"", mintedSession, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
				o.Holders = heldSessions{held: map[string]bool{mintedSession: true}}
			})
			report := tc.reported
			reporting(rig, &report)
			launched := rig.launchWith("t1", func(r *PendingRun) { r.SessionID = mintedSession })
			if tc.start != "" {
				if ok, err := rig.pending.MarkSessionStart(t.Context(), "t1", launched.LaunchID, tc.start); err != nil || !ok {
					t.Fatalf("MarkSessionStart = %v, %v", ok, err)
				}
			}
			_, err := rig.coordinator.bindReported(t.Context(), rig.get("t1"), tc.reported)
			if bound := err == nil; bound != tc.binds {
				t.Errorf("the resume bound %s: %v (%v), want %v", tc.reported, bound, err, tc.binds)
			}
		})
	}
}
