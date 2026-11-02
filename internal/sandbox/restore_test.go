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

	"github.com/crewlet/crewlet/internal/events/types"
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
// new one: the run is handed the job's kept archive to restore, the session it
// names to take up, the brief written for a session in a box that replaced the
// one it was last worked in — and, beside it, the fresh brief its script hands
// a session of its own if the restore fails there. The launch's record says
// which brief it handed, and its caller is told so before the run starts.
//
// Mutation: continue only in the box the session lives in, hand the same-box
// brief to a new box, or tell the caller nothing, and this goes red.
func TestAKeptSessionIsContinuedInWhicheverBoxTheLaunchGets(t *testing.T) {
	t.Parallel()
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
// capture of a job before the one this launch follows. Without a kept capture,
// the job before's own session is continued in place only in its own box.
//
// Mutation: choose the brief before the box's harness and the object are asked,
// restore over a cleanly ended box's own copy, or skip any check the launch
// makes of the record as it opened the job, and a row goes red.
func TestTheBriefMatchesWhatTheRunHas(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, interrupted, unreadable, inPlace, moved bool
		newerUnkept, logMoved, earlierJob               bool
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.runner.Carries, rig.runner.CarryWhy = true, tc.carryWhy
			key := objstore.KeyAt(time.Now())
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
				Interrupted: tc.interrupted, Object: key.String(), ObjectHash: strings.Repeat("a", 64),
				ObjectSize: 14, Home: "/boxes/old/home"}
			row := keptRow(t, kept)
			if tc.logMoved {
				row.CaptureSeq = 2
			}
			restore := &keptRestore{rows: map[string]executordomain.Session{"t1": row},
				archives: map[string][]byte{key.String(): []byte("the transcript")}, unreadable: tc.unreadable}
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
			if want := (LaunchDecision{Brief: tc.want, Kept: tc.want != BriefFresh && !tc.inPlace}); got.decision() != want {
				t.Errorf("the launch tells its caller %+v, want %+v", got.decision(), want)
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
// ended turn, a turn whose run is over, a runner of no harness that restores
// sessions, one that refuses the archive, a job that left no capture of its own
// — an earlier job's holding none of what it was told — a newest capture that
// was not kept, a log row naming another capture, and a turn that kept none:
// not, each saying why — the answer the launch acts on and every reader states.
//
// Mutation: drop any one condition and its row goes red.
func TestWhetherASessionContinuesIsOneDecision(t *testing.T) {
	t.Parallel()
	key := objstore.KeyAt(time.Now())
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, Object: key.String()}
	live := executordomain.Session{Handle: "swe", Turn: "t1", Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	ended := live
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	bare := live
	bare.Object = objstore.Object{}
	moved := live
	moved.CaptureSeq = 2
	run := &PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
	refused := kept
	refused.CaptureSeq, refused.State, refused.Reason = 2, CaptureRefused, "past the seat's budget"
	for name, tc := range map[string]struct {
		row       executordomain.Session
		run       *PendingRun
		follows   int64
		restorer  SessionRestorer
		continues bool
		why       string
	}{
		"a live turn's kept session": {row: live, run: run, follows: 2, restorer: fakeRestorer{}, continues: true},
		"a row naming no session":    {row: bare, run: run, follows: 2, restorer: fakeRestorer{}, why: "names no kept session"},
		"an ended turn":              {row: ended, run: run, follows: 2, restorer: fakeRestorer{}, why: "has ended"},
		"a turn whose run is over":   {row: live, follows: 2, restorer: fakeRestorer{}, why: "has ended"},
		"no runner restores it":      {row: live, run: run, follows: 2, why: "no runner of this build restores"},
		"its runner refuses it": {row: live, run: run, follows: 2,
			restorer: fakeRestorer{why: "older than 2.1.300"}, why: "older than 2.1.300"},
		"the job it follows left none": {row: live, run: run, follows: 3, restorer: fakeRestorer{},
			why: "left no capture"},
		"a newest capture not kept": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &refused,
			Kept: &kept}, follows: 2, restorer: fakeRestorer{}, why: "was not kept: refused"},
		"a log naming another capture": {row: moved, run: run, follows: 2, restorer: fakeRestorer{},
			why: "the executor log names the turn's capture"},
		"a turn that kept none": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2}, follows: 2,
			restorer: fakeRestorer{}, why: "kept no capture"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ContinuityOf(tc.row, tc.run, tc.follows, tc.restorer)
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
	t.Run("kept while it waits", func(t *testing.T) {
		t.Parallel()
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

// A CAPTURE'S OUTCOME IS STAMPED EVEN THROUGH A STORE'S BAD MOMENT. A stamp left
// begun reads as a keep still under way for as long as a capture can take —
// the turn's end waits that long on it, and a relaunch reads a capture that
// landed as one it cannot continue — so the outcome is asked again until the
// store takes it, within a record's budget.
//
// Mutation: stamp the outcome once, and the row keeps the capture begun.
func TestACapturesOutcomeIsStampedThroughAStoresBadMoment(t *testing.T) {
	t.Parallel()
	rig, _, kept := keptTurn(t)
	begun := kept
	begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
	if _, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil {
		t.Fatalf("StampCapture: %v", err)
	}
	// A COORDINATOR OF ITS OWN over the flaky store, so nothing the rig's
	// coordinator runs in the background reads the store this case swaps.
	flaky := &flakyStamps{PendingStore: rig.pending, fails: 2}
	c := &Coordinator{pending: flaky, recordBudget: 200 * time.Millisecond, sessionRetry: time.Millisecond}
	c.finishCapture(t.Context(), rig.get("t1"), begun, CaptureRefused, "the upload failed")
	if run := rig.get("t1"); run.Capture == nil || !run.Capture.samePlace(begun) ||
		run.Capture.State != CaptureRefused {
		t.Fatalf("the row holds %+v after the store's bad moment, want the capture's outcome stamped",
			run.Capture)
	}
	if flaky.attempts != 3 {
		t.Errorf("the outcome was stamped in %d attempts, want 3: two the store refused, then the one it took",
			flaky.attempts)
	}
}

// A SESSION ITS HARNESS DID NOT EXIT CLEANLY FROM IS CUT OFF, whoever packed it.
// The run's script packs the session after its harness exits whatever its
// status, so a harness killed while its wrapper lived on — a signal, its memory
// limit — is packed by the script, its transcript ending wherever the kill
// found it: the capture says so, as one the engine packs of a job still working
// does, and the launch that continues it restores it over the box's copy and
// tells its run it may end mid-call.
//
// Mutation: read a script-packed session as whole whatever its harness's exit,
// and the capture is kept as one that ended cleanly.
func TestASessionItsHarnessDidNotExitCleanlyFromIsCutOff(t *testing.T) {
	t.Parallel()
	for name, cutOff := range map[string]bool{"killed": true, "exited cleanly": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, _, _ := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			rig.runner.Finish(Result{Success: !cutOff, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
				Captured: &session.Archive, HarnessCutOff: cutOff})
			collectHeld(t, rig, "t1")
			run := rig.get("t1")
			if run.Kept == nil || run.Kept.Interrupted != cutOff {
				t.Fatalf("the row keeps %+v, want a capture interrupted %v", run.Kept, cutOff)
			}
			if takes := rig.runner.Takes(); len(takes) != 1 || takes[0].Pack {
				t.Errorf("the session was taken %+v, want the archive its script packed, as it was", takes)
			}
		})
	}
}

// A CAPTURE NAMES THE CALLS ITS SESSION HAD NOT BEEN ANSWERED. The bridge
// numbers a call as it arrives and records it once its tool answers, so the
// highest call the row holds when a session is packed says nothing of a call
// below it still in flight — a slow write beside a fast read — whose outcome
// the session may never have been handed: the capture names every place below
// its highest the row held no call at, the highest of them within the bound,
// for the continuation to list.
//
// Mutation: stamp the highest call alone, or name the lowest places past the
// bound rather than the highest, and a case goes red.
func TestACaptureNamesTheCallsItsSessionHadNotBeenAnswered(t *testing.T) {
	t.Parallel()
	held := func(seqs ...int) PendingRun {
		var run PendingRun
		for _, seq := range seqs {
			run.BridgeCalls = append(run.BridgeCalls, BridgeCall{Name: "slack_post", Seq: seq})
		}
		return run
	}
	if got := openPlaces(held(1, 2, 4, 6), 6); !slices.Equal(got, []int{3, 5}) {
		t.Errorf("a row holding 1, 2, 4 and 6 leaves %v open, want 3 and 5", got)
	}
	if got := openPlaces(held(1, 2, 3), 3); len(got) != 0 {
		t.Errorf("a row holding every call leaves %v open, want none", got)
	}
	// A LONG ROUND WHOSE MIDDLE THE ROW'S BOUND DROPPED: the places open
	// beside the calls in flight are old, and the highest are kept.
	run := held(1)
	for seq := 500; seq <= 600; seq++ {
		if seq != 597 {
			run.BridgeCalls = append(run.BridgeCalls, BridgeCall{Name: "slack_post", Seq: seq})
		}
	}
	got := openPlaces(run, 600)
	if len(got) != maxOpenPlaces || !slices.Contains(got, 597) || got[len(got)-1] != 597 {
		t.Errorf("a long round leaves %d places open ending %v, want the %d highest, 597 among them",
			len(got), got[max(len(got)-3, 0):], maxOpenPlaces)
	}
}

// A CUT-OFF SESSION IS HANDED AGAIN THE NOTES ITS JOB TOOK LAST. A take of a
// run's notes is recorded read before its answer reaches the harness, so a
// harness cut off between the two never had the notes of its last take; a take
// before the last was answered, since the harness went on to ask again, and
// the notes its brief read are in its first message. So a cut-off capture's
// cursor stops before the job's last take, and a whole one's holds every note
// read.
//
// Mutation: hold every note read whatever the harness's exit, count the
// brief's reads as a take, or stop before the job's first take, and a case
// goes red.
func TestACutOffSessionIsHandedAgainTheNotesItsJobTookLast(t *testing.T) {
	t.Parallel()
	read := func(id string, order int, boundary, launch string) RunNote {
		return RunNote{ID: id, Read: &NoteRead{Order: order, Boundary: boundary, Launch: launch}}
	}
	run := PendingRun{Steer: RunSteer{Reads: 5, Notes: []RunNote{
		read("brief", 1, string(types.SteerBoundaryBrief), "job-2"),
		read("earlier-job", 2, string(types.SteerBoundaryToolBatch), "job-1"),
		read("first-take", 3, string(types.SteerBoundaryToolBatch), "job-2"),
		read("last-take", 4, string(types.SteerBoundaryToolBatch), "job-2"),
		read("last-take-too", 4, string(types.SteerBoundaryToolBatch), "job-2"),
		read("review", 5, string(types.SteerBoundaryRound), ""),
	}}}
	if got := notesHeld(run, "job-2", false); got != 5 {
		t.Errorf("a whole session holds notes through %d, want every one read, 5", got)
	}
	if got := notesHeld(run, "job-2", true); got != 3 {
		t.Errorf("a cut-off session holds notes through %d, want 3, before its job's last take", got)
	}
	briefOnly := PendingRun{Steer: RunSteer{Reads: 1, Notes: []RunNote{
		read("brief", 1, string(types.SteerBoundaryBrief), "job-2")}}}
	if got := notesHeld(briefOnly, "job-2", true); got != 1 {
		t.Errorf("a cut-off session whose job took no notes holds through %d, want its brief's, 1", got)
	}
}
