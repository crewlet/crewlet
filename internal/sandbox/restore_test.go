package sandbox

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// keptRestore is the engine's part of continuing a kept session as a case plays
// it: the turns' rows on the executor log, the archive each object holds, and
// every row it was asked for.
type keptRestore struct {
	mu       sync.Mutex
	rows     map[string]executordomain.Session
	archives map[string][]byte
	// unreadable fails every fetch as an object the store no longer holds.
	unreadable bool
	asked      []string
}

func (k *keptRestore) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {

	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, handle+"/"+turn)
	// A CAPTURE ANOTHER NODE'S KEEP RECORDED is seen only at linearizable.
	if fresh.Level != statelog.ReadLinearizable {
		return executordomain.Session{}, errors.New("read below the level a kept capture is seen at")
	}
	row, ok := k.rows[turn]
	if !ok || row.Handle != handle {
		return executordomain.Session{}, executordomain.ErrNotFound
	}
	return row, nil
}

func (k *keptRestore) Fetch(_ context.Context, row executordomain.Session, dst io.Writer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, ok := k.archives[row.Object.Key.String()]
	if k.unreadable || !ok {
		return executordomain.ErrUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (k *keptRestore) OpenSpooled(_ executordomain.Session, sealed io.Reader) (io.Reader, error) {
	return sealed, nil
}

func (k *keptRestore) askedFor() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.asked)
}

// keptRow is the executor log's row of turn t1 as a capture kept stamps it:
// its session, its place and its object.
func keptRow(t *testing.T, kept SessionCapture) executordomain.Session {
	t.Helper()
	key, err := objstore.ParseKey(kept.Object)
	if err != nil {
		t.Fatalf("the kept capture names no object: %v", err)
	}
	return executordomain.Session{Handle: "swe", Turn: "t1", ConversationKey: "chat:D1",
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
		Object:        objstore.Object{Key: key, Hash: objstore.Hash(kept.ObjectHash), Size: kept.ObjectSize},
		ArchiveFormat: FakeArchiveFormat, LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq,
		Home: kept.Home}
}

// keptTurn is a keeping rig whose turn t1 ran one job, collected with its
// session taken whole and kept, and the restore that continues it.
func keptTurn(t *testing.T) (*coordRig, *keptRestore, SessionCapture) {
	t.Helper()
	rig, _, keep := keepingRig(t)
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
	restore := &keptRestore{rows: map[string]executordomain.Session{"t1": keptRow(t, *run.Kept)},
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

// launchRestoring launches req through a coordinator of the rig's own store and
// queue that continues kept sessions from restore, as an engine's does.
func launchRestoring(t *testing.T, rig *coordRig, restore SessionRestore, req LaunchRequest) (LaunchResult, error) {
	t.Helper()
	c, err := NewCoordinator(CoordinatorOptions{
		Answers: rigAnswers(),
		Queue:   rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease:   leased(rigLease),
		Restore: restore,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(c.Stop)
	return c.Launch(t.Context(), rig.manager, req)
}

// A TURN'S KEPT SESSION IS CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS. The
// job before was collected and its box handed back, so the relaunch's box is a
// new one: the run is handed the turn's kept archive to restore, the session it
// names to take up, the brief written for a session in a box that replaced the
// one it was last worked in — and, beside it, the fresh brief its script hands
// a session of its own if the restore fails there. The launch's record says
// which brief it handed.
//
// Mutation: continue only in the box the session lives in, or hand the
// same-box brief to a new box, and this goes red.
func TestAKeptSessionIsContinuedInWhicheverBoxTheLaunchGets(t *testing.T) {
	t.Parallel()
	rig, restore, kept := keptTurn(t)
	res, err := launchRestoring(t, rig, restore, continuingKept(kept))
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
}

// THE BRIEF MATCHES WHAT THE RUN HAS. Of the three briefs, the launch hands the
// one its box and its session make true: the continuation where the session is
// taken up in the box it was last worked in — its own copy there where the job
// before ended cleanly in it, the archive restored over the copy where that
// job's harness was cut off — the new-box continuation in any other box, and
// the fresh brief where the session cannot be taken up here at all: a box whose
// harness cannot continue it, an object the store no longer holds, a turn whose
// kept session moved since the launch chose it. Without a kept capture, the job
// before's own session is continued in place only in its own box.
//
// Mutation: choose the brief before the box's harness and the object are asked,
// or restore over a cleanly ended box's own copy, and a row goes red.
func TestTheBriefMatchesWhatTheRunHas(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, interrupted, unreadable, inPlace, moved bool
		carryWhy                                        string
		want                                            BriefKind
		session                                         string
		carried, replace                                bool
	}{
		"its own box, ended cleanly":   {reused: true, want: BriefContinued, session: "s-1"},
		"its own box, cut off":         {reused: true, interrupted: true, want: BriefContinued, session: "s-1", carried: true, replace: true},
		"a new box":                    {want: BriefContinuedNewBox, session: "s-1", carried: true},
		"a box that cannot take it up": {carryWhy: "this box runs Claude Code 2.1.263", want: BriefFresh},
		"an unreadable object":         {unreadable: true, want: BriefFresh},
		"a kept session that moved":    {moved: true, want: BriefFresh},
		"in place, its own box":        {reused: true, inPlace: true, want: BriefContinued, session: "sess-ask"},
		"in place, a new box":          {inPlace: true, want: BriefFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.runner.Carries, rig.runner.CarryWhy = true, tc.carryWhy
			key := objstore.KeyAt(time.Now())
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
				Interrupted: tc.interrupted, Object: key.String(), ObjectHash: strings.Repeat("a", 64),
				ObjectSize: 14, Home: "/boxes/old/home"}
			restore := &keptRestore{rows: map[string]executordomain.Session{"t1": keptRow(t, kept)},
				archives: map[string][]byte{key.String(): []byte("the transcript")}, unreadable: tc.unreadable}
			rig.coordinator.restore = restore
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Kept: &kept}
			req := continuingKept(kept)
			if tc.moved {
				req.Session.CaptureSeq = 2
			}
			if tc.inPlace {
				req.Session = nil
				req.Continue = &Continuation{SessionID: "sess-ask", Brief: "carry on in the box you left"}
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused)
			defer got.release()
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
		})
	}
}

// A RESTORED SESSION IS THE SEAT'S AND THE TURN'S OWN, and a sibling's end
// changes nothing about it. The launch reads the row of the turn it launches
// for, under the seat it launches as, and nothing else: a sibling turn of the
// same conversation that ended since — the conversation's newest ended session,
// which a later piece's next turn would take up — is never read, and the
// archive the run is handed is the object the turn's own row names. Read on
// another node, at the level that sees a capture another node's keep recorded.
//
// Mutation: resolve the session by the conversation rather than the turn, or
// read the row below linearizable, and this goes red.
func TestARestoredSessionIsTheSeatsAndTheTurnsOwn(t *testing.T) {
	t.Parallel()
	rig, restore, kept := keptTurn(t)
	sibling := keptRow(t, kept)
	sibling.Turn, sibling.SessionID = "t2", "s-sibling"
	sibling.Object.Key = objstore.KeyAt(time.Now())
	sibling.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	restore.mu.Lock()
	restore.rows["t2"] = sibling
	restore.archives[sibling.Object.Key.String()] = []byte("the sibling's transcript")
	restore.mu.Unlock()

	continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner, false)
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
}

// AN UNKEPT NEWEST CAPTURE, OR NONE, CONTINUES NOTHING, and says why. A turn
// whose newest capture was refused went further than its kept one, so the
// kept one is not continued as if it were the turn's last; a turn that kept
// none has nothing to continue; and both are answered, never an error, for the
// launch to start a session of its own.
//
// Mutation: continue the kept capture whatever the newest attempt came to, and
// the refused row goes red.
func TestAnUnkeptNewestCaptureStartsFreshAndSaysWhy(t *testing.T) {
	t.Parallel()
	rig, _, kept := keptTurn(t)
	refused := kept
	refused.CaptureSeq, refused.State, refused.Reason = kept.CaptureSeq+1, CaptureRefused, "past the seat's budget"
	if _, err := rig.pending.StampCapture(t.Context(), "t1", refused); err != nil {
		t.Fatalf("StampCapture: %v", err)
	}
	if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner, false); why == "" ||
		!strings.Contains(why, "not kept") {
		t.Fatalf("a turn whose newest capture was refused continues %+v (%q), want none, said", continued, why)
	}

	bare := newCoordRig(t)
	bare.coordinator.restore = &keptRestore{}
	bare.launch("t9")
	if _, why := bare.coordinator.ContinuedCapture(t.Context(), "t9", bare.runner, false); !strings.Contains(why,
		"kept no capture") {
		t.Fatalf("a turn that kept no capture answered %q, want it said", why)
	}
}

// fakeRestorer is a runner of a harness that restores its own sessions, and why
// it refuses what it does not.
type fakeRestorer struct{ why string }

func (f fakeRestorer) Restores(SessionArchive) (bool, string) { return f.why == "", f.why }
func (f fakeRestorer) CheckCarry(context.Context, Sandbox, SessionArchive) (CarryCheck, error) {
	return CarryCheck{Continues: f.why == "", Why: f.why}, nil
}

// WHETHER A SESSION CAN BE CONTINUED IS ONE DECISION. A row that names a kept
// session, of a turn still live, that the runner a launch would take it up
// with restores: continued. A row naming no object, an ended turn, a runner of
// no harness that restores sessions, and one that refuses the archive: not,
// each saying why — the answer the launch acts on and every reader states.
//
// Mutation: drop any one condition and its row goes red.
func TestWhetherASessionContinuesIsOneDecision(t *testing.T) {
	t.Parallel()
	live := executordomain.Session{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Size: 1}}
	ended := live
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	bare := live
	bare.Object = objstore.Object{}
	for name, tc := range map[string]struct {
		row       executordomain.Session
		live      bool
		restorer  SessionRestorer
		continues bool
		why       string
	}{
		"a live turn's kept session": {row: live, live: true, restorer: fakeRestorer{}, continues: true},
		"a row naming no session":    {row: bare, live: true, restorer: fakeRestorer{}, why: "names no kept session"},
		"an ended turn":              {row: ended, live: true, restorer: fakeRestorer{}, why: "has ended"},
		"a turn whose run is over":   {row: live, restorer: fakeRestorer{}, why: "has ended"},
		"no runner restores it":      {row: live, live: true, why: "no runner of this build restores"},
		"its runner refuses it":      {row: live, live: true, restorer: fakeRestorer{why: "older than 2.1.300"}, why: "older than 2.1.300"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ContinuityOf(tc.row, tc.live, tc.restorer)
			if got.Continues != tc.continues || !strings.Contains(got.Why, tc.why) || (tc.continues && got.Why != "") {
				t.Fatalf("ContinuityOf = %+v, want continues %v saying %q", got, tc.continues, tc.why)
			}
		})
	}
}

// A BRIEF'S KIND IS ONE OF THREE, and a record naming another — one a build
// that knows a fourth wrote — reads as the value it is, never as one of them.
//
// Mutation: answer true for any value, and the unknown rows go red.
func TestABriefKindIsOneOfThree(t *testing.T) {
	t.Parallel()
	for _, k := range []BriefKind{BriefContinued, BriefContinuedNewBox, BriefFresh} {
		if !k.Valid() {
			t.Errorf("%q is not a brief", k)
		}
	}
	for _, k := range []BriefKind{"", "continued_elsewhere"} {
		if k.Valid() {
			t.Errorf("%q reads as a brief", k)
		}
	}
}

// A ROUND CARRIED ON CONTINUES ONLY A SESSION ITS ROUND TOOK. A reviewer's
// send-back began a round whose job went with its host before anything could be
// read off its box, so the turn's newest kept session is the round before's: it
// holds neither the send-back nor the round's work, and the calls a
// continuation lists after it would be numbered in the new round's order. A
// relaunch that carries that round on starts a session of its own, saying why —
// asked by the launch's caller and again at the launch — where a send-back,
// which begins a round, continues it. The row says where each round began: a
// launch that begins one moves it, one that carries the round on keeps it.
//
// Mutation: leave the round out of either check, or move the round's start on
// a launch that carries it on, and the carried-on round continues a session
// that never read its send-back.
func TestARoundCarriedOnContinuesOnlyItsOwnRoundsSession(t *testing.T) {
	t.Parallel()
	rig, restore, kept := keptTurn(t)
	// A SEND-BACK'S JOB, LOST WITH ITS BOX: it began a round and left no
	// capture of its own.
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
		AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if run := rig.get("t1"); run.RoundLaunch != 2 || run.Kept == nil || run.Kept.LaunchSeq != 1 {
		t.Fatalf("the row's round began at launch %d and it keeps %+v, want launch 2 and the first "+
			"launch's session", run.RoundLaunch, run.Kept)
	}
	if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner, true); !strings.Contains(why,
		"before its round began") {
		t.Fatalf("a round carried on continues an earlier round's session (%q), want none, said", why)
	}
	if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner, false); why != "" ||
		!continued.Capture.samePlace(kept) {
		t.Fatalf("a send-back continues %+v (%q), want the round before's session", continued, why)
	}
	carried := continuingKept(kept)
	carried.Continue = &Continuation{}
	if _, err := launchRestoring(t, rig, restore, carried); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	started := rig.runner.Started()
	if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" ||
		len(rig.runner.Carried()) != 0 {
		t.Fatalf("the carried-on round started with session %q and brief %q, want a session of its own",
			last.Resume, last.Brief)
	}
	if run := rig.get("t1"); run.RoundLaunch != 2 || run.LaunchSeq != 3 {
		t.Errorf("the carried-on launch %d moved its round's start to %d, want it kept at 2",
			run.LaunchSeq, run.RoundLaunch)
	}
}
