package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// keptRestore is the engine's part of continuing a kept session as a case plays
// it: the turns' rows on the executor log, the archive each object holds, and
// every row it was asked for.
type keptRestore struct {
	mu       sync.Mutex
	rows     map[string]executorkeep.TurnRow
	archives map[string][]byte
	// unreadable fails every fetch as an object the store no longer holds.
	unreadable bool
	// failOpen fails the open of a spool numbered so, from 1 — the fetch's
	// own check that the seal opens whole is the first, the launch's open of
	// what it carries the second — as a seal that does not authenticate does.
	failOpen int
	opened   int
	asked    []string
	// unreadRows, when set, is every read's answer: a log that could not be
	// read.
	unreadRows error
	// noRowFound answers a turn it holds no row for as the engine's restore
	// does, [ErrTurnRowNotFound], rather than as [errNoRow].
	noRowFound bool
}

// errNoRow and errUnreadable stand for the executor log's answers of a row it
// does not hold and an object the store no longer holds: a restore branches on
// no error of the log's, every one of them a session not continued.
var (
	errNoRow      = errors.New("no such turn on the executor log")
	errUnreadable = errors.New("the session's object is unreadable")
)

func (k *keptRestore) Session(_ context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, handle+"/"+turn)
	if k.unreadRows != nil {
		return executorkeep.TurnRow{}, k.unreadRows
	}
	row, ok := k.rows[turn]
	switch {
	case (!ok || row.Handle != handle) && k.noRowFound:
		return executorkeep.TurnRow{}, fmt.Errorf("%w: %w", ErrTurnRowNotFound, errNoRow)
	case !ok || row.Handle != handle:
		return executorkeep.TurnRow{}, errNoRow
	}
	return row, nil
}

func (k *keptRestore) Fetch(_ context.Context, row executorkeep.TurnRow, dst io.Writer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, ok := k.archives[row.Object.Key.String()]
	if k.unreadable || !ok {
		return errUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (k *keptRestore) OpenSpooled(_ executorkeep.TurnRow, sealed io.Reader) (io.Reader, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.opened++
	if k.opened == k.failOpen {
		return nil, errUnreadable
	}
	return sealed, nil
}

func (k *keptRestore) askedFor() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.asked)
}

// keptRow is the executor log's row of turn t1 as a capture kept stamps it:
// its session, its place and its object.
func keptRow(t *testing.T, kept SessionCapture) executorkeep.TurnRow {
	t.Helper()
	key, err := objstore.ParseKey(kept.Object)
	if err != nil {
		t.Fatalf("the kept capture names no object: %v", err)
	}
	return executorkeep.TurnRow{Handle: "swe", Turn: "t1",
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
		Object:        objstore.Object{Key: key, Hash: objstore.Hash(kept.ObjectHash), Size: kept.ObjectSize},
		ArchiveFormat: FakeArchiveFormat, LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq}
}

// keptTurn is a keeping rig whose turn t1 ran one job, collected with its
// session taken whole and kept, and the restore that continues it.
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
func keptTurn(t *testing.T) (*coordRig, *keptRestore, SessionCapture) {
	t.Helper()
	return keptTurnBy(t, nil)
}

// keptTurnBy is [keptTurn] taken by a coordinator built with what with sets on
// its options ([newCoordRigWith]) — its taker lease, for one: nil sets nothing.
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
func keptTurnBy(t *testing.T, with func(*CoordinatorOptions)) (*coordRig, *keptRestore, SessionCapture) {
	t.Helper()
	rig, _, keep := keepingRigWith(t, with)
	rig.runner.Carries = true
	rig.launchKept(t, "t1")
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	run := rig.get("t1")
	if run.Kept == nil || run.Kept.State != CaptureLanded {
		t.Fatalf("the row keeps %+v, want the job's landed capture", run.Kept)
	}
	uploads := keep.uploads()
	if len(uploads) != 1 {
		t.Fatalf("uploaded %d sessions, want the job's one", len(uploads))
	}
	restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, *run.Kept)},
		archives: map[string][]byte{run.Kept.Object: uploads[0]}}
	rig.coordinator.restore = restore
	return rig, restore, *run.Kept
}

// continuingKept is the relaunch of t1 that continues kept, with its three
// briefs told apart.
func continuingKept(kept SessionCapture) LaunchRequest {
	req := launchReq("t1")
	req.Brief = "the fresh brief"
	req.Session = &SessionContinuation{LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq,
		Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours"}
	return req
}

// launchRestoring launches req through a coordinator of the rig's own store,
// queue and spool directory that continues kept sessions from restore, as an
// engine's does.
func launchRestoring(t *testing.T, rig *coordRig, restore SessionRestore, req LaunchRequest) (LaunchResult, error) {
	t.Helper()
	c, err := NewCoordinator(CoordinatorOptions{
		Answers: rigAnswers(), Leads: rigLeads(),
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{}, Pushes: &pushSpy{},
		Lease:    leased(rigLease),
		Restore:  restore,
		SpoolDir: rig.coordinator.spoolDirectory(),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(c.Stop)
	return c.Launch(t.Context(), rig.manager, req)
}

// restoreSpools is every spool a launch left of a session it carried, in the
// rig's spool directory.
func restoreSpools(t *testing.T, rig *coordRig) []string {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(rig.coordinator.spoolDirectory(), restoreSpoolPrefix+"*"))
	if err != nil {
		t.Fatalf("listing the restore spools: %v", err)
	}
	return left
}

// A TURN'S KEPT SESSION IS CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS. The
// job before was collected and its box handed back, so the relaunch's box is a
// new one: the run is handed the job's kept archive to restore, the session it
// names to take up, the brief written for a session in a box that replaced the
// one it was last worked in — and, beside it, the fresh brief its script hands
// a session of its own if the restore fails there. The launch's record says
// which brief it handed, and its caller is told so before the run starts. And
// the spool it carried the session in, in the node's spool directory, is gone
// once the run has started.
//
// Mutation: continue only in the box the session lives in, hand the same-box
// brief to a new box, tell the caller nothing, or leave the spool, and this
// goes red.
func TestAKeptSessionIsContinuedInWhicheverBoxTheLaunchGets(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		req := continuingKept(kept)
		var decided []LaunchDecision
		req.Decided = func(d LaunchDecision) { decided = append(decided, d) }
		res, err := launchRestoring(t, rig, restore, req)
		if err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		if res.Reused {
			t.Fatalf("relaunch = %+v, want a new box: the collected job's box was handed back", res)
		}
		started := rig.runner.Started()
		last := started[len(started)-1]
		if last.Resume != "s-1" || last.Brief != "carry on in a box that replaced yours" ||
			last.FreshBrief != "the fresh brief" {
			t.Fatalf("the relaunched job started with session %q, brief %q and fresh brief %q; want the kept "+
				"session, the new-box continuation and the fresh brief beside it",
				last.Resume, last.Brief, last.FreshBrief)
		}
		carried := rig.runner.Carried()
		if len(carried) != 1 || string(carried[0].Body) != "the transcript" ||
			carried[0].Archive.SessionID != "s-1" || carried[0].Replace {
			t.Fatalf("the box was handed %+v, want the turn's kept archive, kept over no copy", carried)
		}
		if res.Continues != "s-1" || res.Brief != BriefContinuedNewBox ||
			rig.get("t1").Launch.Brief != BriefContinuedNewBox {
			t.Errorf("the launch answered %+v and recorded brief %q, want the new-box continuation",
				res, rig.get("t1").Launch.Brief)
		}
		if len(decided) != 1 || decided[0] != (LaunchDecision{Brief: BriefContinuedNewBox, Kept: true}) {
			t.Errorf("the launch's caller was told %+v, want once: the turn's kept session, in a new box", decided)
		}
		if left := restoreSpools(t, rig); len(left) != 0 {
			t.Errorf("the launch left the spool it carried the session in: %v", left)
		}
	})
}

// A RUN THAT CONTINUES A SESSION IS WIRED FOR EITHER SESSION ITS BOX MAY START,
// AND ONE THAT STARTS ITS OWN FOR ONE. Whether the box restores the session is
// the box's to find out, so a run handed a session to continue is handed the
// job's tools twice: under the bridge token of that session, and under the
// token of a session of its own, which its script installs in that one's place
// where its restore fails — the box's own servers beside each, and nothing
// else apart. The token its harness then dials is its box's word on which
// session it started. A run that starts a session of its own from the first has
// one session, and is handed one wiring.
//
// Mutation: hand the second wiring to every run or to none, or build it
// without the box's own servers, and a check goes red.
func TestARunThatContinuesASessionIsWiredForEitherSessionItsBoxStarts(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		const at = "https://engine.example.com/mcp/"
		opened := func(OpenedJob) (Opening, error) {
			return Opening{
				Servers: map[string]MCPServer{"crewlet": {Transport: TransportHTTP, URL: at + "kept"}},
				Steer:   SteerWiring{Server: "crewlet", Tool: "steer_take", URL: at + "kept/steer"},
				Fresh: &Opening{
					Servers: map[string]MCPServer{"crewlet": {Transport: TransportHTTP, URL: at + "fresh"}},
					Steer:   SteerWiring{Server: "crewlet", Tool: "steer_take", URL: at + "fresh/steer"},
				},
			}, nil
		}
		own := map[string]MCPServer{"repo": {Command: "repo-mcp"}}

		rig, restore, kept := keptTurn(t)
		req := continuingKept(kept)
		req.Opened, req.MCPServers = opened, own
		if _, err := launchRestoring(t, rig, restore, req); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		started := rig.runner.Started()
		last := started[len(started)-1]
		fresh := last.FreshWiring
		switch {
		case last.Resume != "s-1":
			t.Fatalf("the relaunch continues %q, want the kept session", last.Resume)
		case fresh == nil:
			t.Fatal("a run that continues a session was handed no wiring for a session of its own")
		case last.MCPServers["crewlet"].URL != at+"kept" || last.Steer.URL != at+"kept/steer":
			t.Errorf("the run is wired to %q and %q, want the continued session's token",
				last.MCPServers["crewlet"].URL, last.Steer.URL)
		case fresh.MCPServers["crewlet"].URL != at+"fresh" || fresh.Steer.URL != at+"fresh/steer":
			t.Errorf("a session of its own is wired to %q and %q, want its own token",
				fresh.MCPServers["crewlet"].URL, fresh.Steer.URL)
		case last.MCPServers["repo"].Command != "repo-mcp" || fresh.MCPServers["repo"].Command != "repo-mcp":
			t.Errorf("the box's own servers are %+v and %+v, want them beside each wiring",
				last.MCPServers["repo"], fresh.MCPServers["repo"])
		}

		bare := newCoordRig(t)
		first := launchReq("t9")
		first.Opened, first.MCPServers = opened, own
		if _, err := launchRestoring(t, bare, &keptRestore{}, first); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if started := bare.runner.Started(); len(started) != 1 || started[0].Resume != "" ||
			started[0].FreshWiring != nil || started[0].MCPServers["crewlet"].URL != at+"kept" {
			t.Errorf("a run starting a session of its own was started %+v, want one wiring", started)
		}
	})
}

// THE BRIEF MATCHES WHAT THE RUN HAS. Of the three briefs, the launch hands the
// one its box and its session make true: the continuation where the session is
// taken up in the box it was last worked in — its own copy there where the job
// before ended cleanly in it, the archive restored over the copy where that
// job's harness was cut off — the new-box continuation in any other box, and
// the fresh brief where the session cannot be taken up here at all: a box whose
// harness cannot continue it, an object the store no longer holds, a turn whose
// kept session moved since the launch chose it, a capture of the job taken
// since that was not kept, a log row that names another capture, and a kept
// capture of a job before the one this launch follows, a seal that does not
// open whole, a spool that does not open again once checked. Without a kept
// capture, the job before's own session is continued in place only in its own
// box. And whatever the launch hands, it leaves no spool of a session it
// fetched behind once its run has started, or did not.
//
// Mutation: choose the brief before the box's harness and the object are asked,
// restore over a cleanly ended box's own copy, skip any check the launch makes
// of the record as it opened the job, or leave a fetched spool behind where its
// fetch or its open fails, and a row goes red.
func TestTheBriefMatchesWhatTheRunHas(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, interrupted, unreadable, inPlace, moved bool
		newerUnkept, logMoved, earlierJob               bool
		failOpen                                        int
		carryWhy                                        string
		want                                            BriefKind
		session                                         string
		carried, replace                                bool
	}{
		"its own box, ended cleanly":    {reused: true, want: BriefContinued, session: "s-1"},
		"its own box, cut off":          {reused: true, interrupted: true, want: BriefContinued, session: "s-1", carried: true, replace: true},
		"a new box":                     {want: BriefContinuedNewBox, session: "s-1", carried: true},
		"a box that cannot take it up":  {carryWhy: "this box runs Claude Code 2.1.263", want: BriefFresh},
		"an unreadable object":          {unreadable: true, want: BriefFresh},
		"a kept session that moved":     {moved: true, want: BriefFresh},
		"a newer capture, not kept":     {newerUnkept: true, want: BriefFresh},
		"a log naming another capture":  {logMoved: true, want: BriefFresh},
		"an earlier job's kept capture": {earlierJob: true, want: BriefFresh},
		"in place, its own box":         {reused: true, inPlace: true, want: BriefContinued, session: "sess-ask"},
		"in place, a new box":           {inPlace: true, want: BriefFresh},
		"a seal that does not open":     {failOpen: 1, want: BriefFresh},
		"a spool that does not reopen":  {failOpen: 2, want: BriefFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			// ITS SPOOLS ITS OWN, so what a case leaves in them is the case's
			// to see.
			rig.coordinator.spoolDir = t.TempDir()
			rig.runner.Carries, rig.runner.CarryWhy = true, tc.carryWhy
			key := objstore.KeyAt(time.Now())
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
				Interrupted: tc.interrupted, Through: new(0), Object: key.String(),
				ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, Home: "/boxes/old/home"}
			row := keptRow(t, kept)
			if tc.logMoved {
				row.CaptureSeq = 2
			}
			restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": row},
				archives: map[string][]byte{key.String(): []byte("the transcript")}, unreadable: tc.unreadable,
				failOpen: tc.failOpen}
			rig.coordinator.restore = restore
			newest := kept
			if tc.newerUnkept {
				newest.CaptureSeq, newest.State, newest.Reason = 2, CaptureRefused, "past the seat's budget"
			}
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &newest, Kept: &kept}
			if tc.earlierJob {
				opened.LaunchSeq = 3
			}
			req := continuingKept(kept)
			if tc.moved {
				req.Session.CaptureSeq = 2
			}
			if tc.inPlace {
				req.Session = nil
				req.Continue = &Continuation{SessionID: "sess-ask", Brief: "carry on in the box you left"}
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			wantBrief := map[BriefKind]string{BriefContinued: "carry on in the box you left",
				BriefContinuedNewBox: "carry on in a box that replaced yours", BriefFresh: "the fresh brief"}[tc.want]
			if got.kind != tc.want || got.brief != wantBrief || got.session != tc.session {
				t.Fatalf("the launch hands brief %q (%s) and session %q; want %q (%s) and %q",
					got.brief, got.kind, got.session, wantBrief, tc.want, tc.session)
			}
			if (got.carried != nil) != tc.carried || (got.carried != nil && got.carried.Replace != tc.replace) {
				t.Fatalf("the launch carries %+v, want carried %v replacing %v", got.carried, tc.carried, tc.replace)
			}
			if tc.want != BriefFresh && got.fresh != "the fresh brief" {
				t.Errorf("a continued run carries fresh brief %q, want the fresh brief for its script", got.fresh)
			}
			if tc.want == BriefFresh && got.fresh != "" {
				t.Errorf("a fresh run carries a second brief %q", got.fresh)
			}
			if want := (LaunchDecision{Brief: tc.want, Kept: tc.want != BriefFresh && !tc.inPlace}); got.decision() != want {
				t.Errorf("the launch tells its caller %+v, want %+v", got.decision(), want)
			}
			// NO SPOOL LEFT once the run has started, or a fresh one in its
			// place: one the launch fetched and could not use is gone already.
			if !tc.carried {
				if left := restoreSpools(t, rig); len(left) != 0 {
					t.Errorf("the launch left a spool of the session behind: %v", left)
				}
			}
			got.release()
			if left := restoreSpools(t, rig); len(left) != 0 {
				t.Errorf("the run's start left the spool it carried the session in: %v", left)
			}
		})
	}
}

// A RESTORED SESSION IS THE SEAT'S AND THE TURN'S OWN, and a sibling's end
// changes nothing about it. The launch reads the row of the turn it launches
// for, under the seat it launches as, and nothing else: a sibling turn of the
// same conversation that ended since — the conversation's head, whose session
// its next turn would take up — is never read, and the
// archive the run is handed is the object the turn's own row names. Read on
// another node, at the level that sees a capture another node's keep recorded.
//
// Mutation: resolve the session by the conversation rather than the turn, or
// read the row below linearizable, and this goes red.
func TestARestoredSessionIsTheSeatsAndTheTurnsOwn(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		sibling := keptRow(t, kept)
		sibling.Turn, sibling.SessionID = "t2", "s-sibling"
		sibling.Object.Key = objstore.KeyAt(time.Now())
		sibling.Ended = true
		restore.mu.Lock()
		restore.rows["t2"] = sibling
		restore.archives[sibling.Object.Key.String()] = []byte("the sibling's transcript")
		restore.mu.Unlock()

		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
		if why != "" || continued.Row.Turn != "t1" || continued.Row.SessionID != "s-1" ||
			!continued.Capture.samePlace(kept) {
			t.Fatalf("the relaunch continues %+v (%q), want the turn's own kept session", continued, why)
		}
		if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		if asked := restore.askedFor(); slices.ContainsFunc(asked, func(a string) bool { return a != "swe/t1" }) {
			t.Fatalf("the restore read %v, want the launching seat's own turn alone", asked)
		}
		if carried := rig.runner.Carried(); len(carried) != 1 || string(carried[0].Body) != "the transcript" {
			t.Fatalf("the run was handed %+v, want its own turn's session", carried)
		}
	})
}

// AN UNKEPT NEWEST CAPTURE, OR NONE, CONTINUES NOTHING, and says why. A job
// whose newest capture was refused went further than its kept one, so the kept
// one is not continued as if it were the job's last; a turn that kept none has
// nothing to continue; and both are answered, never an error, for the launch
// to start a session of its own — with what the job's session's last request
// carried all the same, as its newest capture measured it, for the floor of a
// launch that continues that session in its own box.
//
// Mutation: continue the kept capture whatever the newest attempt came to, or
// answer the context only of a capture that is continued, and a case goes red.
func TestAnUnkeptNewestCaptureStartsFreshAndSaysWhy(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, kept := keptTurn(t)
		refused := kept
		refused.CaptureSeq, refused.State, refused.Reason = kept.CaptureSeq+1, CaptureRefused, "past the seat's budget"
		refused.ContextTokens = 61000
		if _, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil {
			t.Fatalf("StampCapture: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
		if why == "" || !strings.Contains(why, "not kept") || continued.Capture.CaptureSeq != 0 {
			t.Fatalf("a turn whose newest capture was refused continues %+v (%q), want none, said", continued, why)
		}
		if continued.Context != 61000 {
			t.Errorf("the answer says the job's session carried %d tokens, want the 61000 its newest capture "+
				"measured, for the floor of a launch that continues it in its box", continued.Context)
		}

		bare := newCoordRig(t)
		bare.coordinator.restore = &keptRestore{}
		bare.launch("t9")
		if _, why := bare.coordinator.ContinuedCapture(t.Context(), "t9", bare.runner); !strings.Contains(why,
			"kept no capture") {
			t.Fatalf("a turn that kept no capture answered %q, want it said", why)
		}
	})
}

// fakeRestorer is a runner of a harness that restores its own sessions, and why
// it refuses what it does not.
type fakeRestorer struct{ why string }

func (f fakeRestorer) Restores(SessionArchive) (bool, string) { return f.why == "", f.why }
func (f fakeRestorer) CheckCarry(context.Context, Sandbox, SessionArchive) (CarryCheck, error) {
	return CarryCheck{Continues: f.why == "", Why: f.why}, nil
}

// WHETHER A SESSION CAN BE CONTINUED IS ONE DECISION, of the turn's row on the
// executor log and its run's record. A live turn whose job the relaunch follows
// left a kept capture, its newest, that the log's row names and the runner a
// launch would take it up with restores: continued. A row naming no object, an
// ended turn — saying only its conversation's next turn continues it — a turn
// whose run is over, a turn whose end is owed on its record or recorded already (ending, and
// no relaunch of it follows, though the log's row has not ended yet), a runner
// of no harness that restores sessions, one that refuses the archive, a job
// that left no capture of its own — an earlier job's holding none of what it
// was told — a newest capture that was not kept, a kept one that states no save
// point, a log row naming another capture, and a turn that kept none: not, each
// saying why — the answer the launch acts on and every reader states. A run
// whose ending is decided is ending from the decision on, before its end is
// owed: every launch refuses it. And a newest capture the run's record stamps
// refused while the log's row names it — its record landed unseen by that
// stamp — is not continued either, saying the log keeps it, never that it was
// not kept.
//
// Mutation: drop any one condition and its row goes red; or answer the refused
// capture the row names by the record alone, and its row says the session the
// row keeps was not kept.
func TestWhetherASessionContinuesIsOneDecision(t *testing.T) {
	t.Parallel()
	key := objstore.KeyAt(time.Now())
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, Object: key.String(), Through: new(0)}
	live := executorkeep.TurnRow{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	ended := live
	ended.Ended = true
	bare := live
	bare.Object = objstore.Object{}
	moved := live
	moved.CaptureSeq = 2
	run := &PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
	owing := *run
	owing.OwedEnd = []byte(`{"outcome":"completed"}`)
	recorded := *run
	recorded.EndRecorded = true
	stopping := *run
	stopping.Ending = &RecordedEnding{ID: "ending-1"}
	refused := kept
	refused.CaptureSeq, refused.State, refused.Reason = 2, CaptureRefused, "past the seat's budget"
	unplaced := kept
	unplaced.Through = nil
	settled := kept
	settled.CaptureSeq, settled.State, settled.Reason = 2, CaptureRefused, takerStoppedWhy(takingProcess)
	for name, tc := range map[string]struct {
		row       executorkeep.TurnRow
		run       *PendingRun
		follows   int64
		restorer  SessionRestorer
		continues bool
		why       string
		// unsaid is what the reason must not say.
		unsaid string
	}{
		"a live turn's kept session": {row: live, run: run, follows: 2, restorer: fakeRestorer{}, continues: true},
		"a row naming no session":    {row: bare, run: run, follows: 2, restorer: fakeRestorer{}, why: "names no kept session"},
		"an ended turn": {row: ended, run: run, follows: 2, restorer: fakeRestorer{},
			why: "continued only by its conversation's next turn"},
		"a turn whose run is over":     {row: live, follows: 2, restorer: fakeRestorer{}, why: "has ended"},
		"a turn whose end is owed":     {row: live, run: &owing, follows: 2, restorer: fakeRestorer{}, why: "is ending"},
		"a turn whose end is recorded": {row: live, run: &recorded, follows: 2, restorer: fakeRestorer{}, why: "is ending"},
		"a run whose ending is decided, its end not yet owed": {row: live, run: &stopping, follows: 2,
			restorer: fakeRestorer{}, why: "is ending"},
		"no runner restores it": {row: live, run: run, follows: 2, why: "no runner of this build restores"},
		"its runner refuses it": {row: live, run: run, follows: 2,
			restorer: fakeRestorer{why: "older than 2.1.300"}, why: "older than 2.1.300"},
		"the job it follows left none": {row: live, run: run, follows: 3, restorer: fakeRestorer{},
			why: "left no capture"},
		"a newest capture not kept": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &refused,
			Kept: &kept}, follows: 2, restorer: fakeRestorer{}, why: "was not kept: refused"},
		"a kept capture that states no save point": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2,
			Capture: &unplaced, Kept: &unplaced}, follows: 2, restorer: fakeRestorer{},
			why: "states no save point"},
		"a log naming another capture": {row: moved, run: run, follows: 2, restorer: fakeRestorer{},
			why: "the executor log names the turn's capture"},
		"a refused capture the log names all the same": {row: moved, run: &PendingRun{TurnID: "t1",
			LaunchSeq: 2, Capture: &settled, Kept: &kept}, follows: 2, restorer: fakeRestorer{},
			why: "(launch 2, capture 2) refused: its taker node-a stopped before it was kept; the executor " +
				"log keeps it all the same", unsaid: "was not kept"},
		"a turn that kept none": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2}, follows: 2,
			restorer: fakeRestorer{}, why: "kept no capture"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ContinuityOf(tc.row, tc.run, tc.follows, tc.restorer)
			if got.Continues != tc.continues || !strings.Contains(got.Why, tc.why) || (tc.continues && got.Why != "") ||
				(tc.unsaid != "" && strings.Contains(got.Why, tc.unsaid)) {
				t.Fatalf("ContinuityOf = %+v, want continues %v saying %q and not %q", got, tc.continues, tc.why,
					tc.unsaid)
			}
		})
	}
}

// A BRIEF'S KIND IS ONE OF FOUR, and a record naming another — one a build
// that knows a fifth wrote — reads as the value it is, never as one of them.
// Each of the three that continue a session says so, and the fresh one does
// not.
//
// Mutation: answer true for any value, drop a kind from [BriefKinds], or read
// the conversation's session as a fresh brief, and a row goes red.
func TestABriefKindIsOneOfFour(t *testing.T) {
	t.Parallel()
	for _, k := range []BriefKind{BriefContinued, BriefContinuedNewBox, BriefContinuedConversation, BriefFresh} {
		if !k.Valid() {
			t.Errorf("%q is not a brief", k)
		}
		if k.Continues() == (k == BriefFresh) {
			t.Errorf("%q continues a session: %v", k, k.Continues())
		}
	}
	if len(BriefKinds) != 4 {
		t.Errorf("there are %d briefs, want 4", len(BriefKinds))
	}
	for _, k := range []BriefKind{"", "continued_elsewhere"} {
		if k.Valid() {
			t.Errorf("%q reads as a brief", k)
		}
	}
}

// A RELAUNCH CONTINUES ONLY THE SESSION OF THE JOB IT FOLLOWS. The turn's first
// job kept its session; the job after it — a reviewer's send-back, or the job a
// person's answer carried on — left no capture of its own: its box went with
// its host before anything could be read off it. That job was told what the
// first job's session never read — the review, the answer — and the calls a
// continuation would list after the first job's capture are numbered in
// another order where a send-back began a round. So the relaunch that follows
// it continues nothing, asked by the launch's caller and again at the launch,
// and starts a session of its own, saying why.
//
// Mutation: continue the turn's newest kept capture whichever job took it, at
// either check, and the relaunch continues a session that never read what the
// job before it was told.
func TestARelaunchContinuesOnlyTheSessionOfTheJobItFollows(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
			AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		if run := rig.get("t1"); run.LaunchSeq != 2 || run.Kept == nil || !run.Kept.samePlace(kept) {
			t.Fatalf("the row is at launch %d keeping %+v, want launch 2 and the first job's session",
				run.LaunchSeq, run.Kept)
		}
		if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); !strings.Contains(why,
			"left no capture") || continued.Capture.CaptureSeq != 0 {
			t.Fatalf("the relaunch continues %+v (%q), want none, said: the job it follows left none",
				continued, why)
		}
		// AND THE LAUNCH, asked to continue the first job's session all the same,
		// starts one of its own.
		if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		started := rig.runner.Started()
		if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" ||
			len(rig.runner.Carried()) != 0 {
			t.Fatalf("the relaunch started with session %q and brief %q, want a session of its own",
				last.Resume, last.Brief)
		}
	})
}

// A RELAUNCH WAITS BRIEFLY FOR THE CAPTURE OF THE JOB IT FOLLOWS, AND NO LONGER.
// The job's keep is made off the read that collected it, and a relaunch can
// follow at once, so the relaunch waits for it — a keep that lands while it
// waits is continued — but only for as long as an ordinary keep takes: one
// still begun after that is a slow store or a stopped node's, which the frame
// holding the seat and a slot of the node's admission does not wait out for
// the capture's own budget of twenty minutes and more. It continues nothing,
// saying so.
//
// Mutation: wait out the capture's own budget on the relaunch, or stop waiting
// at once, and a case goes red.
func TestARelaunchWaitsBrieflyForTheCaptureOfTheJobItFollows(t *testing.T) {
	t.Parallel()
	t.Run("still being kept", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, kept := keptTurn(t)
			rig.coordinator.relaunchWait = 50 * time.Millisecond
			begun := kept
			begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
			if _, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil {
				t.Fatalf("StampCapture: %v", err)
			}
			asked := time.Now()
			if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); !strings.Contains(why,
				"still being kept") {
				t.Fatalf("a relaunch whose job's capture is still being kept answered %q, want it said", why)
			}
			if waited := time.Since(asked); waited > 10*time.Second {
				t.Errorf("the relaunch waited %s on the keep, past its own short wait", waited)
			}
		})
	})
	t.Run("kept while it waits", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			rig.coordinator.relaunchWait = 10 * time.Second
			begun := kept
			begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
			if _, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil {
				t.Fatalf("StampCapture: %v", err)
			}
			landed := begun
			landed.State = CaptureLanded
			row := keptRow(t, kept)
			row.CaptureSeq = landed.CaptureSeq
			go func() {
				time.Sleep(20 * time.Millisecond)
				restore.mu.Lock()
				restore.rows["t1"] = row
				restore.mu.Unlock()
				_, _ = rig.pending.StampCapture(context.Background(), "t1", landed)
			}()
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
			if why != "" || !continued.Capture.samePlace(landed) {
				t.Fatalf("the relaunch continues %+v (%q), want the capture kept while it waited", continued, why)
			}
		})
	})
}

// flakyStamps is a run store whose stamps of a capture fail the first fails
// times, as a coordination store having a bad moment does.
type flakyStamps struct {
	PendingStore
	mu       sync.Mutex
	fails    int
	attempts int
}

func (f *flakyStamps) StampCapture(ctx context.Context, turnID string, c SessionCapture) (bool, error) {
	f.mu.Lock()
	f.attempts++
	failing := f.attempts <= f.fails
	f.mu.Unlock()
	if failing {
		return false, errors.New("the store did not answer")
	}
	return f.PendingStore.StampCapture(ctx, turnID, c)
}

// A CAPTURE IS STAMPED EVEN THROUGH A STORE'S BAD MOMENT, its place and its
// outcome. A stamp left begun reads as a keep still under way for as long as a
// capture can take — the turn's end waits that long on it, and a relaunch reads
// a capture that landed as one it cannot continue — and a place never reserved
// is a capture nobody can know was made; so each is asked again until the store
// takes it, within a record's budget.
//
// IN A SYNCTEST BUBBLE, so the asks again are held to the record's budget on
// the bubble's clock, which only their own waits advance, never a starved
// runner's scheduling.
//
// Mutation: stamp the place or the outcome once, and the row keeps no capture,
// or keeps it begun.
func TestACaptureIsStampedThroughAStoresBadMoment(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, kept := keptTurn(t)
		begun := kept
		begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
		// A COORDINATOR OF ITS OWN over the flaky store, so nothing the rig's
		// coordinator runs in the background reads the store this case swaps.
		reserving := &flakyStamps{PendingStore: rig.pending, fails: 2}
		c := &Coordinator{pending: reserving, recordBudget: 200 * time.Millisecond, sessionRetry: time.Millisecond}
		if took, err := c.reserveCapture(t.Context(), time.Time{}, "t1", begun); err != nil || !took {
			t.Fatalf("the place was reserved %v (%v) through the store's bad moment, want reserved", took, err)
		}
		if run := rig.get("t1"); run.Capture == nil || !run.Capture.samePlace(begun) {
			t.Fatalf("the row holds %+v, want the capture's place reserved", run.Capture)
		}
		flaky := &flakyStamps{PendingStore: rig.pending, fails: 2}
		c.pending = flaky
		c.finishCapture(t.Context(), time.Time{}, rig.get("t1"), begun, CaptureRefused, "the upload failed")
		if run := rig.get("t1"); run.Capture == nil || !run.Capture.samePlace(begun) ||
			run.Capture.State != CaptureRefused {
			t.Fatalf("the row holds %+v after the store's bad moment, want the capture's outcome stamped",
				run.Capture)
		}
		if flaky.attempts != 3 {
			t.Errorf("the outcome was stamped in %d attempts, want 3: two the store refused, then the one it took",
				flaky.attempts)
		}
	})
}

// A SESSION ITS HARNESS DID NOT EXIT CLEANLY FROM IS CUT OFF, whoever packed it.
// The run's script packs the session after its harness exits whatever its
// status, so a harness killed while its wrapper lived on — a signal, its memory
// limit — is packed by the script, its transcript ending wherever the kill
// found it: the capture says so, as one the engine packs of a job still working
// does, and the launch that continues it restores it over the box's copy and
// tells its run it may end mid-call. The executor log's row says so too, with
// the context the session's last request carried: the run's record that knew
// both is deleted with the turn, and a launch continuing the session after the
// turn ended reads them there.
//
// Mutation: read a script-packed session as whole whatever its harness's exit,
// and the capture is kept as one that ended cleanly; leave the cut-off or the
// context off the stamp recordCapture hands the log, and the capture asked of it
// says the session ended cleanly, with no context. The engine's word of that
// stamp onto the log's row is held by
// TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives.
func TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff(t *testing.T) {
	t.Parallel()
	for name, cutOff := range map[string]bool{"killed": true, "exited cleanly": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				session.ContextTokens = 61_000
				rig.runner.Finish(Result{Success: !cutOff, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
					Captured: &session.Archive, HarnessCutOff: cutOff})
				collectHeld(t, rig, "t1")
				run := rig.get("t1")
				if run.Kept == nil || run.Kept.Interrupted != cutOff {
					t.Fatalf("the row keeps %+v, want a capture interrupted %v", run.Kept, cutOff)
				}
				if captures := writes.captured(); len(captures) != 1 || captures[0].Stamp.Interrupted != cutOff ||
					captures[0].Stamp.ContextTokens != 61_000 {
					t.Errorf("the log was asked to record %+v, want one capture interrupted %v of 61000 tokens",
						captures, cutOff)
				}
				if takes := rig.runner.Takes(); len(takes) != 1 || takes[0].Pack {
					t.Errorf("the session was taken %+v, want the archive its script packed, as it was", takes)
				}
			})
		})
	}
}

// A CAPTURE STILL NAMES THE OPEN PLACES AN OLDER PEER READS. A build before the
// save point's count read a capture's place by the order the bridge admitted
// calls: the highest call the row held when the session was packed, and every
// place below it the row held no call at — a call in flight, a slow write
// beside a fast read — the highest of them within the bound, for its
// continuation to list. This build writes them for that build, mid-upgrade, and
// reads its own count instead (SessionCapture.Through), because of the long
// round's case below: the places the bound dropped are the highest the row
// leaves unlisted, so they fill the slots, and a call in flight below them is
// named in none — which that build reads as answered.
//
// Mutation: stamp the highest call alone, or name the lowest places past the
// bound rather than the highest, and a case goes red.
func TestACaptureStillNamesTheOpenPlacesAnOlderPeerReads(t *testing.T) {
	t.Parallel()
	held := func(seqs ...int) BridgeLog {
		var calls BridgeLog
		for _, seq := range seqs {
			calls.Calls = append(calls.Calls, BridgeCall{Name: "slack_post", Seq: seq})
		}
		return calls
	}
	if got := openPlaces(held(1, 2, 4, 6), 6); !slices.Equal(got, []int{3, 5}) {
		t.Errorf("a log holding 1, 2, 4 and 6 leaves %v open, want 3 and 5", got)
	}
	if got := openPlaces(held(1, 2, 3), 3); len(got) != 0 {
		t.Errorf("a log holding every call leaves %v open, want none", got)
	}
	// A LONG ROUND WHOSE MIDDLE THE LOG'S BOUND DROPPED: the highest places
	// are kept, the dropped ones among them, so a call in flight below them
	// all is named in none — the reading the count replaces.
	calls := held(1)
	for seq := 500; seq <= 600; seq++ {
		if seq != 597 {
			calls.Calls = append(calls.Calls, BridgeCall{Name: "slack_post", Seq: seq})
		}
	}
	got := openPlaces(calls, 600)
	if len(got) != maxOpenPlaces || !slices.Contains(got, 597) || got[len(got)-1] != 597 {
		t.Errorf("a long round leaves %d places open ending %v, want the %d highest, 597 among them",
			len(got), got[max(len(got)-3, 0):], maxOpenPlaces)
	}
}

// A CAPTURE STATES THE NOTES IT HOLDS ONLY WHERE ITS HARNESS ENDED ITS OWN RUN.
// A session its harness ended its own run in holds every note read when it was
// packed. One cut off may lack its job's last take of them — a take is
// recorded read before its answer reaches the harness — and is not known to
// hold its job's brief, which told the notes the session it continued lacked,
// an earlier job's among them: so it is known to hold none, whatever its job
// read, and a launch that continues it tells it every note the turn has read.
// A cursor stopped before the job's last take said it held what its brief
// told, which may never have reached it.
//
// Mutation: hold every note read whatever the harness's exit, or hold none
// whatever it was, and a case goes red.
func TestACaptureStatesTheNotesItHoldsOnlyWhereItsHarnessEndedItsOwnRun(t *testing.T) {
	t.Parallel()
	read := func(id string, order int, boundary, launch string) RunNote {
		return RunNote{ID: id, Read: &NoteRead{Order: order, Boundary: boundary, Launch: launch}}
	}
	run := PendingRun{Steer: RunSteer{Reads: 5, Notes: []RunNote{
		read("earlier-job", 1, string(types.SteerBoundaryToolBatch), "job-1"),
		read("brief", 2, string(types.SteerBoundaryBrief), "job-2"),
		read("first-take", 3, string(types.SteerBoundaryToolBatch), "job-2"),
		read("last-take", 4, string(types.SteerBoundaryToolBatch), "job-2"),
		read("review", 5, string(types.SteerBoundaryRound), ""),
	}}}
	if got := notesHeld(run, false); got != 5 {
		t.Errorf("a session its harness ended its own run in holds notes through %d, want every one read, 5", got)
	}
	if got := notesHeld(run, true); got != 0 {
		t.Errorf("a cut-off session states it holds notes through %d, want none: not its job's last take, "+
			"nor what its brief told", got)
	}
	briefOnly := PendingRun{Steer: RunSteer{Reads: 1, Notes: []RunNote{
		read("brief", 1, string(types.SteerBoundaryBrief), "job-2")}}}
	if got := notesHeld(briefOnly, true); got != 0 {
		t.Errorf("a cut-off session whose job read only its brief's note states it holds notes through %d, "+
			"want none", got)
	}
}

// A CAPTURE'S NOTE CURSOR STOPS BEFORE A READ ITS HARNESS NEVER HAD. A take
// whose write landed after its answer was lost is marked unanswered on the
// record: the session lacks those notes however cleanly its harness ended,
// while it holds the answered reads after them, which one number cannot say. So
// the cursor stops before the first read marked unanswered, whichever job's
// take it was: a mark can land after a later brief was built, and that brief
// did not tell the note as marked. A launch that continues the session tells
// every note past it, as notes it may already show. A read a later launch's
// brief told again is no longer marked, its write recording it read by its
// job: that job's session holds it from its brief. A record with none marked
// holds every read, and a capture cut off states none either way.
//
// Mutation: count a read marked unanswered as held, stop before the last such
// read rather than the first, stop only before the capture's own job's, or stop
// before a read a brief told again, and a case goes red.
func TestACapturesNoteCursorStopsBeforeAReadItsHarnessNeverHad(t *testing.T) {
	t.Parallel()
	read := func(id string, order int, launch string, unanswered bool) RunNote {
		return RunNote{ID: id, Read: &NoteRead{Order: order, Boundary: string(types.SteerBoundaryToolBatch),
			Launch: launch, Unanswered: unanswered}}
	}
	for name, tc := range map[string]struct {
		notes []RunNote
		want  int
	}{
		"no read marked": {notes: []RunNote{read("a", 1, "job-2", false), read("b", 2, "job-2", false),
			read("c", 3, "job-2", false)}, want: 3},
		"the job's own take marked": {notes: []RunNote{read("a", 1, "job-2", false), read("b", 2, "job-2", true),
			read("c", 3, "job-2", false)}, want: 1},
		"two takes marked, the first stops it": {notes: []RunNote{read("a", 1, "job-2", false),
			read("b", 2, "job-2", true), read("c", 3, "job-2", true)}, want: 1},
		"the marked take listed after a later read": {notes: []RunNote{read("c", 3, "job-2", false),
			read("b", 2, "job-2", true), read("a", 1, "job-2", false)}, want: 1},
		"an earlier job's take marked": {notes: []RunNote{read("a", 1, "job-1", true), read("b", 2, "job-2", false),
			read("c", 3, "job-2", false)}, want: 0},
		"an earlier job's take marked, told again by the job's brief": {notes: []RunNote{
			{ID: "a", Read: &NoteRead{Order: 1, Boundary: string(types.SteerBoundaryBrief), Launch: "job-2",
				Retold: &NoteRead{Order: 1, Boundary: string(types.SteerBoundaryToolBatch), Launch: "job-1",
					Unanswered: true}}},
			read("b", 2, "job-2", false), read("c", 3, "job-2", false)}, want: 3},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := PendingRun{LaunchID: "job-2", Steer: RunSteer{Reads: 3, Notes: tc.notes}}
			if got := notesHeld(run, false); got != tc.want {
				t.Errorf("a session its harness ended its own run in holds notes through %d, want %d", got, tc.want)
			}
			if got := notesHeld(run, true); got != 0 {
				t.Errorf("a cut-off session states it holds notes through %d, want none", got)
			}
		})
	}
}

// EVERY STAMP OF A CAPTURE STATES THE NOTES ITS SESSION HOLDS, TAKEN OR REFUSED.
// The note cursor on a capture's stamp is what a launch that continues it tells
// it the notes past, on this build where its harness ended its own run and on
// an older build whatever it ended: so the stamp of a session its harness ended
// its own run in states every note the record says was read up to the first
// read it marks unanswered — a take whose answer never reached the harness —
// and the stamp of one cut off states none — the stamp of a capture taken, and
// that of one refused before its place was reserved, alike. An older build
// tells a continuation of a capture cut off every note the turn has read, and
// one of a capture whose take went unanswered that take's notes, only because
// its stamp states so.
//
// Mutation: stamp a capture cut off with every note read, or one whose take
// went unanswered with that take's notes, where it is taken or where it is
// refused before its place, and that case goes red.
func TestEveryStampOfACaptureStatesTheNotesItsSessionHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// cutOff is a job whose harness was killed, its script packing after
		// it; else one whose harness ended its own run.
		cutOff bool
		// untaken fails the take, so the capture is refused before its place
		// was reserved.
		untaken bool
		// unanswered settles the job's second take as one whose answer never
		// reached its harness.
		unanswered bool
	}{
		{name: "a job whose harness ended its own run, its capture taken"},
		{name: "a job whose harness was killed, its capture taken", cutOff: true},
		{name: "a job whose harness ended its own run, its take refused", untaken: true},
		{name: "a job whose harness was killed, its take refused", cutOff: true, untaken: true},
		{name: "a job whose harness ended its own run, a take of it unanswered, its capture taken",
			unanswered: true},
		{name: "a job whose harness ended its own run, a take of it unanswered, its take refused",
			unanswered: true, untaken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				launched := rig.launchKept(t, "t1")
				// TWO TAKES OF THE JOB'S NOTES, so the record says two were read.
				for _, id := range []string{"first", "second"} {
					fileNote(t, rig.pending, "t1", id)
					taken, err := rig.pending.TakeNotes(t.Context(), "t1", NoteTake{IDs: []string{id},
						Launch: launched.LaunchID, Statuses: []string{StatusRunning},
						Read: NoteRead{Phase: "execute", Round: 1, Boundary: string(types.SteerBoundaryToolBatch),
							Take: "take-" + id}})
					if err != nil || len(taken) != 1 {
						t.Fatalf("taking note %s took %d (%v), want it", id, len(taken), err)
					}
				}
				if reads := rig.get("t1").Steer.Reads; reads != 2 {
					t.Fatalf("the record says %d notes were read, want 2", reads)
				}
				if tc.unanswered {
					if settled, err := rig.pending.SettleTake(t.Context(), "t1",
						NoteSettle{Take: "take-second", Unanswered: true}); err != nil || len(settled) != 1 {
						t.Fatalf("settling the second take = %d, %v, want its note", len(settled), err)
					}
				}
				if tc.untaken {
					rig.runner.Session.TakeErr = errors.New("the box's session could not be read")
				}
				archive := rig.runner.Session.Archive
				rig.runner.Finish(Result{Success: !tc.cutOff, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
					Captured: &archive, HarnessCutOff: tc.cutOff})
				// COLLECTED, the resume after it refused, so the row stays for the
				// case.
				rig.resumer.failWith(errors.New("held for the case to read the row"))
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Logf("OnCompleted: %v", err)
				}
				rig.awaitKeeps()
				state, want := CaptureLanded, 2
				if tc.untaken {
					state = CaptureRefused
				}
				switch {
				case tc.cutOff:
					want = 0
				case tc.unanswered:
					want = 1
				}
				capture := rig.get("t1").Capture
				if capture == nil || capture.State != state || capture.Interrupted != tc.cutOff {
					t.Fatalf("the row holds %+v, want the capture %s, cut off: %v", capture, state, tc.cutOff)
				}
				if capture.NoteCursor != want {
					t.Errorf("the capture is stamped holding the notes read through %d, want %d", capture.NoteCursor,
						want)
				}
			})
		})
	}
}

// WHAT A READER IS TOLD OF A RELAUNCH IS THE RELAUNCH'S OWN DECISION, and the
// half a person's answer takes beside it. A live turn's kept capture the target's
// runner restores continues in any box; one it does not — or one not kept — is
// continued only in the box the run parked in, where the run holds that box and
// it ran the row's session, the relaunch's member is of the box's harness in its
// cell, and a box on an engine host is on the host of the node holding the seat:
// the launch's own tests of a box it continues in; anywhere else, not at all —
// and only that, in a company that keeps no session. A lost job's box continues
// nothing. And it is undecided, never a no, wherever a relaunch would not decide
// now either: a job still working, or a claim whose read has not taken the job's
// final capture and whose turn has not begun, a capture still being kept, a
// target that cannot be said, a round lost a second time, and a collected job's
// resume, which carries its round on in its box only where its sign-in was
// replaced while it ran — each until the resumed turn says, as it begins, what
// it does with the round: from then on, its review included, the row is decided
// on what it said, whether or not its read took a capture. A claim of a
// person's answer reads no box, and is decided whatever its job kept. And a
// row naming the capture its run's record stamps refused — the record a taker
// handed on before its lease's window closed, landed after the relaunch's own
// wait had settled it — is decided not continued, saying the log keeps it.
//
// Mutation: drop the parked box's half or any test of it — its host, the session
// it ran — read any undecided state as a no, read a claim whose read took the
// final capture, or whose turn has said what it does, as one still collecting,
// leave a turn that said it undecided, let a box of another harness or cell
// count as the parked one, or tell a refused capture the row names by the
// run's record alone, and a case goes red.
func TestWhatAReaderIsToldOfARelaunchIsTheRelaunchsOwnDecision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	key := objstore.KeyAt(now)
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, Object: key.String(), At: now,
		Through: new(0)}
	row := executorkeep.TurnRow{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	parked := func() PendingRun {
		return PendingRun{TurnID: "t1", Status: StatusAwaiting, SandboxID: "box-1", Placement: string(Direct),
			LaunchNode: "node-a", Harness: "claude-code", SessionID: "s-1", LaunchSeq: 2, Capture: &kept,
			Kept: &kept, BoxSince: now.Add(-time.Hour)}
	}
	target := RelaunchTarget{Harness: "claude-code", Placement: Direct, Node: "node-a", Restorer: fakeRestorer{}}
	older := target
	older.Restorer = fakeRestorer{why: "older than 2.1.300"}
	moved := older
	moved.Node = "node-b"
	final := kept
	final.Final = true
	for name, tc := range map[string]struct {
		run      func(*PendingRun)
		target   RelaunchTarget
		decided  bool
		in       ContinuesIn
		whyHolds string
		// ending is a run whose ending is decided, which no relaunch of it
		// follows: of a turn of no conversation, as this row is, no launch
		// continues it at all.
		ending bool
	}{
		"its kept capture":  {target: target, decided: true, in: ContinuesInAnyBox},
		"in its parked box": {target: older, decided: true, in: ContinuesInParkedBox, whyHolds: "older than"},
		"its box reclaimed": {target: older, decided: true, whyHolds: "is gone",
			run: func(r *PendingRun) { r.SandboxID = "" }},
		"a member elsewhere": {decided: true, whyHolds: "in the container cell",
			target: RelaunchTarget{Harness: "claude-code", Placement: Container, Node: "node-a",
				Restorer: older.Restorer}},
		"a member of another harness": {decided: true, whyHolds: "launched on opencode",
			target: RelaunchTarget{Harness: "opencode", Placement: Direct, Node: "node-a",
				Restorer: older.Restorer}},
		"a box on the host of a node that no longer holds the seat": {target: moved, decided: true,
			whyHolds: "cannot continue in it"},
		"a box that ran another session of the turn": {target: older, decided: true,
			whyHolds: "another session of the turn", run: func(r *PendingRun) {
				r.Launch.FreshSessionID, r.Launch.SessionStart = "f-1", SessionStartFresh
			}},
		"a lost job's box": {target: older, decided: true, whyHolds: "older than", run: func(r *PendingRun) {
			r.Status, r.Launch.Collected, r.Lost = StatusRunning, true, &JobLoss{Cause: LossBoxGone}
		}},
		"a job still running": {target: target, whyHolds: "still working",
			run: func(r *PendingRun) { r.Status, r.LaunchSeq = StatusRunning, 3 }},
		"a claim still collecting": {target: target, whyHolds: "being collected",
			run: func(r *PendingRun) { r.Status = StatusResumed }},
		"a claim whose read took the final capture": {target: target, decided: true, in: ContinuesInAnyBox,
			run: func(r *PendingRun) { r.Status, r.Capture, r.Kept = StatusResumed, &final, &final }},
		"a collected job held": {target: target, decided: true, in: ContinuesInAnyBox,
			run: func(r *PendingRun) { r.Status, r.Launch.Collected = StatusRunning, true }},
		"a collected job's resume, its capture no other box continues": {target: older, whyHolds: "sign-in",
			run: func(r *PendingRun) { r.Status, r.Launch.Collected = StatusRunning, true }},
		"a claim whose turn collects the job, its capture no other box continues": {target: older,
			decided: true, whyHolds: "collects its job", run: func(r *PendingRun) {
				r.Status, r.Capture, r.Kept, r.Launch.ResumeCarry = StatusResumed, &final, &final, ResumeCollects
			}},
		"a claim whose turn carries the round on in its box": {target: older, decided: true,
			in: ContinuesInParkedBox, whyHolds: "older than", run: func(r *PendingRun) {
				r.Status, r.Capture, r.Kept, r.Launch.ResumeCarry = StatusResumed, &final, &final, ResumeCarriesOn
			}},
		"a claim whose turn began, its read having taken no capture": {target: target, decided: true,
			in: ContinuesInAnyBox, run: func(r *PendingRun) {
				r.Status, r.Launch.ResumeCarry = StatusResumed, ResumeCollects
			}},
		"a claim whose turn said what this build does not read": {target: older, whyHolds: "does not read",
			run: func(r *PendingRun) {
				r.Status, r.Capture, r.Kept, r.Launch.ResumeCarry = StatusResumed, &final, &final, "later"
			}},
		"an answer's claim whose job kept no capture": {target: target, decided: true,
			in: ContinuesInParkedBox, whyHolds: "kept no capture", run: func(r *PendingRun) {
				r.Status, r.Capture, r.Kept = StatusResumed, nil, nil
				r.Answer = &RecordedAnswer{EventIDs: []string{"ev-1"}}
			}},
		"a round lost a second time, its turn collecting what it submitted": {target: target, decided: true,
			in: ContinuesInAnyBox, run: func(r *PendingRun) {
				r.Status, r.RoundLosses, r.Launch.ResumeCarry = StatusResumed, 1, ResumeCollects
				r.Lost = &JobLoss{Cause: LossBoxGone}
			}},
		"a round lost a second time, its turn carrying it on": {target: target, decided: true,
			whyHolds: "in a session of its own", run: func(r *PendingRun) {
				r.Status, r.RoundLosses, r.Launch.ResumeCarry = StatusResumed, 1, ResumeCarriesOn
				r.Lost = &JobLoss{Cause: LossBoxGone}
			}},
		"a round lost a second time": {target: target, whyHolds: "lost a second time", run: func(r *PendingRun) {
			r.Status, r.Launch.Collected, r.RoundLosses = StatusRunning, true, 1
			r.Lost = &JobLoss{Cause: LossBoxGone}
		}},
		"a capture the log took after its stamp was settled refused": {target: target, decided: true,
			whyHolds: "the executor log keeps it all the same", run: func(r *PendingRun) {
				settled := final
				settled.State, settled.Reason = CaptureRefused, takerStoppedWhy(takingProcess)
				r.Status, r.Launch.Collected, r.Lost = StatusRunning, true, &JobLoss{Cause: LossBoxGone}
				r.Capture, r.Kept = &settled, nil
			}},
		"a capture being kept": {target: target, whyHolds: "still being kept", run: func(r *PendingRun) {
			begun := kept
			begun.CaptureSeq, begun.State, begun.At = 2, CaptureBegun, now.Add(-time.Second)
			r.Capture = &begun
		}},
		"a target that cannot be said": {whyHolds: "sitting out",
			target: RelaunchTarget{Undecided: true, Why: "every member is sitting out its plan"}},
		"no member at all": {decided: true, whyHolds: "no runner member",
			target: RelaunchTarget{Why: "the seat's chain has no runner member"}},
		"a decided ending": {target: target, decided: true, whyHolds: "is ending", ending: true,
			run: func(r *PendingRun) { r.Ending = &RecordedEnding{ID: "ending-1"} }},
		"a company that keeps no session": {decided: true, in: ContinuesInParkedBox,
			whyHolds: "keeps no executor session",
			target: RelaunchTarget{Harness: "claude-code", Placement: Direct, Node: "node-a",
				Restorer: fakeRestorer{}, NoKept: NoKeptSession}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := parked()
			if tc.run != nil {
				tc.run(&run)
			}
			got := ReadContinuityOf(row, "", &run, tc.target, ContinuedByNextTurn, now)
			by := ContinuedByRelaunch
			if tc.ending {
				by = ""
			}
			switch {
			case got.By != by:
				t.Fatalf("ReadContinuityOf = %+v, want it about %q", got, by)
			case got.Decided != tc.decided:
				t.Fatalf("ReadContinuityOf = %+v, want decided %v", got, tc.decided)
			case got.Continues != (tc.in != "") || got.In != tc.in:
				t.Fatalf("ReadContinuityOf = %+v, want continued in %q", got, tc.in)
			case !strings.Contains(got.Why, tc.whyHolds):
				t.Fatalf("ReadContinuityOf = %+v, want it saying %q", got, tc.whyHolds)
			case tc.in == ContinuesInAnyBox && got.Why != "":
				t.Fatalf("ReadContinuityOf = %+v, want no reason beside a session continued in any box", got)
			}
		})
	}
}

// A ROW IS READ AGAIN AS A RELAUNCH READS IT ONLY WHERE THAT CAN CHANGE ITS
// ANSWER: where the decision reaches the row's capture and the row names
// another than the one its run's record keeps — a copy of the log behind the
// record — never where it names the kept one, which decides as the relaunch's
// read would, nor where the job still works, its capture is still being kept,
// or its turn has ended, which decide without the row's capture.
//
// Mutation: read every live row again, or none, and a case goes red.
func TestARowIsReadAgainOnlyWhereThatCanChangeItsAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 2, State: CaptureLanded, At: now, Through: new(0)}
	run := func() *PendingRun {
		return &PendingRun{TurnID: "t1", Status: StatusAwaiting, LaunchSeq: 2, Capture: &kept, Kept: &kept}
	}
	naming := func(capture int64) executorkeep.TurnRow {
		return executorkeep.TurnRow{Handle: "swe", Turn: "t1", LaunchSeq: 2, CaptureSeq: capture}
	}
	ended := naming(1)
	ended.Ended = true
	working := run()
	working.Status, working.LaunchSeq = StatusRunning, 3
	for name, tc := range map[string]struct {
		row   executorkeep.TurnRow
		run   *PendingRun
		again bool
	}{
		"a row behind its run's record":    {row: naming(1), run: run(), again: true},
		"a row naming the kept capture":    {row: naming(2), run: run()},
		"a row whose job is still working": {row: naming(1), run: working},
		"an ended turn's row":              {row: ended, run: run()},
		"a row whose run is over":          {row: naming(1)},
	} {
		if got := RereadsRow(tc.row, tc.run, now); got != tc.again {
			t.Errorf("%s: read again %v, want %v", name, got, tc.again)
		}
	}
}

// A ROUND IS CARRIED ON IN THE SESSION ITS JOB'S BOX RAN, never the one the
// job's launch chose. A launch that continues a session names, beside it, the
// session its script starts of its own where its box cannot take that one up,
// and its box says which it started by the token its harness dials: a person's
// answer, or a replaced sign-in, carries the round on in the one the box said —
// the one of its own where it fell back, the chosen one where it took that up —
// and in none at all where the box's word never reached the row, which cannot
// tell the two apart; there the round is carried on from the turn's kept
// capture, or in a session of its own. A launch that continues no session
// names no second one, and its box has nothing to say: the session it named is
// the one it ran.
//
// Mutation: carry the round on in the session the launch chose whatever the box
// said or where it said nothing, or leave the launch's second session off the
// job's record, and a case goes red.
func TestARoundIsCarriedOnInTheSessionItsJobsBoxRan(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		if first := rig.get("t1"); first.Launch.FreshSessionID != "" {
			t.Fatalf("a launch that continued no session named a second one, %q", first.Launch.FreshSessionID)
		}
		if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		fresh := rig.get("t1").Launch.FreshSessionID
		if fresh == "" {
			t.Fatal("a launch that continues a session named no session its script starts in its place")
		}
		run := PendingRun{TurnID: "t1", SessionID: "s-1", SandboxID: "box-1",
			Launch: LaunchRecord{FreshSessionID: fresh}}
		for name, tc := range map[string]struct {
			said SessionStart
			want string
		}{
			"its box took the chosen session up":   {said: SessionStartContinued, want: "s-1"},
			"its box started a session of its own": {said: SessionStartFresh, want: fresh},
			"its box has not said":                 {want: ""},
		} {
			said := run
			said.Launch.SessionStart = tc.said
			if got := said.SessionRan(); got != tc.want {
				t.Errorf("%s: the job ran %q, want %q", name, got, tc.want)
			}
			if asked := askedRun(said, AnswerWords{Text: "use main"}); asked.SessionID != tc.want {
				t.Errorf("%s: an answer carries the round on in %q, want %q", name, asked.SessionID, tc.want)
			}
		}
		// A LAUNCH THAT CONTINUED NO SESSION ran the one it named.
		own := PendingRun{TurnID: "t1", SessionID: "s-own", SandboxID: "box-1"}
		if got := own.SessionRan(); got != "s-own" {
			t.Errorf("a job whose launch continued no session ran %q, want the one its launch named", got)
		}
	})
}

// A COMPANY THAT KEEPS NO SESSION CONTINUES NONE IT KEPT BEFORE. With
// `turn_engine.conversation_session.enabled: false` nothing is kept, and a
// relaunch continues at most the session its own box holds — whatever a capture
// kept while the switch was on, which the relaunch's caller is told it does not
// continue, and which the launch, asked to continue it all the same, does not
// fetch: it starts a session of its own.
//
// Mutation: continue a kept capture whatever the switch says, at either check,
// and the relaunch restores a session the company keeps none of.
func TestACompanyThatKeepsNoSessionContinuesNoneItKeptBefore(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		keep := rig.coordinator.keep.(*sessionKeep)
		keep.mu.Lock()
		keep.policy.Keep = false
		keep.mu.Unlock()
		if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); why != NoKeptSession ||
			continued.Capture.CaptureSeq != 0 {
			t.Fatalf("the relaunch continues %+v (%q), want none, said: the company keeps none", continued, why)
		}
		launcher, err := NewCoordinator(CoordinatorOptions{
			Answers: rigAnswers(), Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
			Resume: &resumeSpy{}, Audience: &audienceSpy{}, Lease: leased(rigLease), Pushes: &pushSpy{},
			Restore: restore, Keep: keep, SpoolDir: rig.coordinator.spoolDirectory(),
		})
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		t.Cleanup(launcher.Stop)
		if _, err := launcher.Launch(t.Context(), rig.manager, continuingKept(kept)); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		started := rig.runner.Started()
		if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" ||
			len(rig.runner.Carried()) != 0 {
			t.Fatalf("the relaunch started with session %q and brief %q, want a session of its own",
				last.Resume, last.Brief)
		}
	})
}

// silentStamps is a run store whose stamps of a capture are never answered, as
// a coordination store in an outage does: each waits out its caller's context.
// Where only is set, only stamps in that state go unanswered, and every other
// is the store's: a reservation the store never answers, say, and the settle
// of its place after it.
type silentStamps struct {
	PendingStore
	only  CaptureState
	asked atomic.Int32
}

func (s *silentStamps) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	if s.only != "" && stamp.State != s.only {
		return s.PendingStore.StampCapture(ctx, turnID, stamp)
	}
	s.asked.Add(1)
	<-ctx.Done()
	return false, ctx.Err()
}

// A CAPTURE'S RECORDS STOP AT THE READ'S TIME, however long the store keeps
// silent. A take is part of a read that must leave its box's pause and its
// answer their room ([Coordinator.takeContext]): its place is reserved within
// the take's own time and its outcome stamped by the read's `by`, each asked
// again while the store does not answer — and neither past its bound, whatever
// is left of a record's budget, so the read goes on to pause its box.
//
// Mutation: bound a stamp by the record's budget alone, ignoring by, or reserve
// a take's place by the read's by rather than the take's own deadline, and a
// record waits out what the read leaves its pause.
func TestACapturesRecordsStopAtTheReadsTime(t *testing.T) {
	t.Parallel()
	t.Run("each record stops at its bound", func(t *testing.T) {
		t.Parallel()
		// IN A BUBBLE, where the silent store's wait ends at the bound's own
		// instant: a record asks until exactly its bound, and one that asked
		// on past it, by a second or by the record's whole budget, reads so.
		inBubble(t, func(t *testing.T) {
			c := &Coordinator{pending: &silentStamps{}, recordBudget: time.Minute, sessionRetry: time.Millisecond}
			stamp := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureBegun}
			for name, record := range map[string]func(by time.Time){
				"a place's reservation": func(by time.Time) { _, _ = c.reserveCapture(t.Context(), by, "t1", stamp) },
				"an outcome's stamp":    func(by time.Time) { c.stampSettled(t.Context(), by, "t1", stamp) },
			} {
				began := time.Now()
				record(began.Add(300 * time.Millisecond))
				if took := time.Since(began); took != 300*time.Millisecond {
					t.Errorf("%s asked a silent store for %s against a bound 300ms off, want it stopped there",
						name, took)
				}
			}
		})
	})
	t.Run("a take reserves its place within its own time", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			// SILENT ON THE RESERVATION ALONE: the settle of the place after it is
			// answered, so the take's time is the reservation's to spend.
			silent := &silentStamps{PendingStore: rig.coordinator.pending, only: CaptureBegun}
			rig.coordinator.pending, rig.coordinator.recordBudget = silent, time.Minute
			// A READ WITH A DEADLINE, whose take ends two seconds from now and
			// whose by a pause's grace after that.
			read, cancel := context.WithTimeout(t.Context(), 2*discardGrace+2*time.Second)
			defer cancel()
			began := time.Now()
			spooled, _ := rig.coordinator.takeCapture(read, run, rig.provider.Box(run.SandboxID), rig.runner,
				readCapture(BoxReadCollect, Result{}, true))
			if took := time.Since(began); took > 20*time.Second {
				t.Errorf("the take asked a silent store to reserve its place for %s, past its own deadline "+
					"two seconds off", took)
			}
			if spooled != nil || silent.asked.Load() == 0 {
				t.Errorf("the take spooled %+v after %d reservations, want none spooled, the place asked for",
					spooled, silent.asked.Load())
			}
			if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
				t.Errorf("the place the take asked for stands %+v, want it settled refused", c)
			}
		})
	})
}
