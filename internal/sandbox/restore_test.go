package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// unreadable fails every read as an object the store no longer holds.
	unreadable bool
	// failOpen fails the open numbered so, from 1 — a restore's read of the
	// whole is the first, its read again into the box the second — as a seal
	// that does not authenticate does; readAgain, when set, is what every open
	// after the first hands out in the object's place, as a store that
	// answered otherwise the second time.
	failOpen  int
	readAgain func(stored []byte) []byte
	// streamed counts the objects [keptRestore.Open] streamed.
	streamed int
	asked    []string
	// unreadRows, when set, is every read's answer: a log that could not be
	// read.
	unreadRows error
	// noRowFound answers a turn it holds no row for as the engine's restore
	// does, [ErrTurnRowNotFound], rather than as [errNoRow].
	noRowFound bool
	// reading, when set, is called before every read of a streamed object,
	// so a case sees the node's disk while the session is read.
	reading func()
}

// errNoRow and errUnreadable stand for the executor log's answers of a row it
// does not hold and an object the store no longer holds: a restore branches on
// no error of the log's, every one of them a session not continued.
var (
	errNoRow      = errors.New("no such turn on the executor log")
	errUnreadable = errors.New("the session's object is unreadable")
)

func (k *keptRestore) Session(ctx context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, handle+"/"+turn)
	// A READ UNDER A CONTEXT THAT ENDED IS NOT ANSWERED, as the estate
	// router's is not: a fake that answered it settled what a wait whose
	// time had run out could not have read.
	if err := ctx.Err(); err != nil {
		return executorkeep.TurnRow{}, err
	}
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

// Open streams the archive row's object holds, as the store streams one
// opened: an object it does not hold, or every object where fetches fail, is
// unreadable.
func (k *keptRestore) Open(_ context.Context, row executorkeep.TurnRow) (io.ReadCloser, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.streamed++
	body, ok := k.archives[row.Object.Key.String()]
	if k.unreadable || !ok || k.streamed == k.failOpen {
		return nil, errUnreadable
	}
	if k.readAgain != nil && k.streamed > 1 {
		body = k.readAgain(slices.Clone(body))
	}
	return io.NopCloser(&watchedRead{r: bytes.NewReader(body), reading: k.reading}), nil
}

// watchedRead calls reading, where set, before each read of r.
type watchedRead struct {
	r       io.Reader
	reading func()
}

func (w *watchedRead) Read(p []byte) (int, error) {
	if w.reading != nil {
		w.reading()
	}
	return w.r.Read(p)
}

// streams is how many objects [keptRestore.Open] was asked to stream.
func (k *keptRestore) streams() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.streamed
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

// restoreSpools is every file under the rig's spool directory but a spool
// directory's lock, whatever it is named: a restore spools none.
func restoreSpools(t *testing.T, rig *coordRig) []string {
	t.Helper()
	left, err := spoolFiles(rig.coordinator.spoolDirectory())
	if err != nil {
		t.Fatalf("listing the restore spools: %v", err)
	}
	return left
}

// spoolFiles is every file under dir but a spool directory's lock.
func spoolFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case !d.IsDir() && d.Name() != spoolLockName:
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// spoolsWhileRead records, on every read of a session restore streams, the
// files under the rig's spool directory, and returns what it recorded: a
// restore that spools the session, even one it removes before the launch
// returns, is seen.
func spoolsWhileRead(rig *coordRig, restore *keptRestore) func() []string {
	var (
		mu   sync.Mutex
		seen []string
	)
	restore.reading = func() {
		found, err := spoolFiles(rig.coordinator.spoolDirectory())
		if err != nil {
			found = []string{err.Error()}
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, found...)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// A TURN'S KEPT SESSION IS CONTINUED IN WHICHEVER BOX ITS NEXT LAUNCH GETS. The
// job before was collected and its box handed back, so the relaunch's box is
// new: the run is handed the kept archive to restore, the session to take up,
// the brief written for a replaced box, and the fresh brief its script hands a
// session of its own if the restore fails. The launch's record says which
// brief it handed, its caller is told before the run starts, and the spool the
// session was carried in is gone once the run has started.
//
// Mutation: continue only in the box the session lives in, hand the same-box
// brief to a new box, tell the caller nothing, or leave the spool, and this
// goes red.
func TestAKeptSessionIsContinuedInWhicheverBoxTheLaunchGets(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		whileRead := spoolsWhileRead(rig, restore)
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
		if spooled := whileRead(); len(spooled) != 0 {
			t.Errorf("the launch spooled the session while it read it: %v", spooled)
		}
	})
}

// A KEPT SESSION READ AGAIN OTHERWISE FAILS ITS LAUNCH. A restore reads the
// object whole once, then again as the run starts, streamed into its box and
// held to the first read: a store that hands one byte more, or a byte changed,
// the second time fails the run's start, so the turn retries the launch, and
// the box is handed nothing whole to restore.
//
// Mutation: hand out the second read unchecked, and the run starts with what
// the store answered the second time.
func TestAKeptSessionReadAgainOtherwiseFailsTheLaunch(t *testing.T) {
	t.Parallel()
	for name, again := range map[string]func([]byte) []byte{
		"one byte more": func(stored []byte) []byte { return append(stored, '!') },
		"a byte changed": func(stored []byte) []byte {
			stored[len(stored)/2] ^= 1
			return stored
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, restore, kept := keptTurn(t)
				restore.readAgain = again
				whileRead := spoolsWhileRead(rig, restore)
				started := len(rig.runner.Started())
				if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err == nil {
					t.Fatal("the relaunch started on a second read that was not the first, want it failed")
				}
				if carried := rig.runner.Carried(); len(carried) != 0 {
					t.Errorf("the box was handed %+v to restore, want nothing", carried)
				}
				if now := len(rig.runner.Started()); now != started {
					t.Errorf("%d runs started, want none past the %d before", now-started, started)
				}
				if left := restoreSpools(t, rig); len(left) != 0 {
					t.Errorf("the launch spooled the session: %v", left)
				}
				if spooled := whileRead(); len(spooled) != 0 {
					t.Errorf("the launch spooled the session while it read it: %v", spooled)
				}
			})
		})
	}
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

// THE BRIEF MATCHES WHAT THE RUN HAS: the continuation in the box last worked
// in (its copy after a clean end, the archive over a cut-off one), the new-box
// continuation elsewhere, the fresh brief where it cannot be taken up (a
// harness that cannot, an object gone, a kept session moved, a row naming
// another capture or an earlier job's, a seal, spool or second read that does
// not open), and a box's own copy only where its harness takes it up in place
// ([SessionRestorer.CheckInPlace]). A newer capture not kept is none of these.
// It spools nothing either way.
//
// Mutation: choose the brief before the box and object are asked, restore over
// a clean copy, skip a check of the record as opened, skip CheckInPlace on
// either path to the box's own copy, or spool the session, and a row goes red.
func TestTheBriefMatchesWhatTheRunHas(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, interrupted, unreadable, inPlace, moved bool
		newerUnkept, logMoved, earlierJob               bool
		failOpen                                        int
		carryWhy, inPlaceWhy                            string
		want                                            BriefKind
		session                                         string
		carried, replace                                bool
	}{
		"its own box, ended cleanly":      {reused: true, want: BriefContinued, session: "s-1"},
		"its own box, cut off":            {reused: true, interrupted: true, want: BriefContinued, session: "s-1", carried: true, replace: true},
		"a new box":                       {want: BriefContinuedNewBox, session: "s-1", carried: true},
		"a box that cannot take it up":    {carryWhy: "this box runs Claude Code 2.1.263", want: BriefFresh},
		"an unreadable object":            {unreadable: true, want: BriefFresh},
		"a kept session that moved":       {moved: true, want: BriefFresh},
		"a newer capture, not kept":       {newerUnkept: true, want: BriefContinuedNewBox, session: "s-1", carried: true},
		"a log naming another capture":    {logMoved: true, want: BriefFresh},
		"an earlier job's kept capture":   {earlierJob: true, want: BriefFresh},
		"in place, its own box":           {reused: true, inPlace: true, want: BriefContinued, session: "sess-ask"},
		"in place, a new box":             {inPlace: true, want: BriefFresh},
		"a seal that does not open":       {failOpen: 1, want: BriefFresh},
		"a read again that does not open": {failOpen: 2, want: BriefFresh},
		"its own box, ended cleanly, a harness refusing its copy": {reused: true,
			inPlaceWhy: "this box runs Codex 0.172.0", want: BriefFresh},
		"in place, its own box, a harness refusing its copy": {reused: true, inPlace: true,
			inPlaceWhy: "this box runs Codex 0.172.0", want: BriefFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			// ITS SPOOLS ITS OWN, so what a case leaves in them is the case's
			// to see.
			rig.coordinator.spoolDir = t.TempDir()
			rig.runner.Carries, rig.runner.CarryWhy, rig.runner.InPlaceWhy = true, tc.carryWhy, tc.inPlaceWhy
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
			whileRead := spoolsWhileRead(rig, restore)
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
			// NO SPOOL, while the run is handed what it carries as after.
			if left := restoreSpools(t, rig); len(left) != 0 {
				t.Errorf("the launch spooled the session: %v", left)
			}
			if got.carried != nil {
				_, _ = io.Copy(io.Discard, got.carried.Body)
			}
			if spooled := whileRead(); len(spooled) != 0 {
				t.Errorf("the launch spooled the session while it read it: %v", spooled)
			}
		})
	}
}

// A NOT-CONTINUED LINE NAMES WHAT THE LAUNCH CONTINUES INSTEAD: the box's copy
// where it may continue it, else a session of its own, written once that is
// decided, never "a session of its own" for a run going on in the copy. A
// capture the copy went past is passed over for it and said so
// ([copyWentPast]); one the record does not keep is refused for that.
//
// Mutation: log before the fallback is decided, name a session of its own
// whatever the launch continues, pass a capture over silently, or judge one
// the record does not keep, and a case goes red.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestANotContinuedLineNamesWhatTheLaunchContinuesInstead(t *testing.T) {
	const (
		refusedKept = "not the one its launch was asked to continue"
		ranOut      = "ran out of"
		wentPast    = "went on past the kept capture (launch 1, capture 1)"
	)
	for _, tc := range []struct {
		name                   string
		chosen, reused, ranOut bool
		// wentOn keeps a capture taken while its job still worked, of the
		// session the box's copy is, and keepsChosen leaves it the one the
		// launch was asked to continue.
		wentOn, keepsChosen bool
		kind                BriefKind
		session, outcome    string
		reasons             []string
	}{
		{name: "a refused kept session, the box's copy continued", chosen: true, reused: true,
			kind: BriefContinued, session: "sess-ask", outcome: continuesBoxCopy, reasons: []string{refusedKept}},
		{name: "a refused kept session in a box of its own", chosen: true,
			kind: BriefFresh, outcome: startsItsOwn, reasons: []string{refusedKept}},
		{name: "a refused kept session, the box's copy run out of context", chosen: true, reused: true,
			ranOut: true, kind: BriefFresh, outcome: startsItsOwn, reasons: []string{refusedKept, ranOut}},
		{name: "the box's copy run out of context, nothing chosen", reused: true, ranOut: true,
			kind: BriefFresh, outcome: startsItsOwn, reasons: []string{ranOut}},
		{name: "the box's copy, nothing chosen", reused: true, kind: BriefContinued, session: "sess-ask"},
		{name: "a kept session the box's copy went past, the copy continued", chosen: true, reused: true,
			wentOn: true, keepsChosen: true, kind: BriefContinued, session: "sess-ask", outcome: continuesBoxCopy,
			reasons: []string{wentPast}},
		{name: "a refused kept session the box's copy went past", chosen: true, reused: true, wentOn: true,
			kind: BriefContinued, session: "sess-ask", outcome: continuesBoxCopy, reasons: []string{refusedKept}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			rig.coordinator.spoolDir = t.TempDir()
			key := objstore.KeyAt(time.Now())
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
				Through: new(0), Object: key.String(), ObjectHash: strings.Repeat("a", 64), ObjectSize: 14}
			if tc.wentOn {
				kept.Final, kept.Interrupted, kept.SessionID = false, true, "sess-ask"
			}
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
				archives: map[string][]byte{key.String(): []byte("the transcript")}}
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept}
			if tc.ranOut {
				opened.ExhaustedSessions = []string{"sess-ask"}
			}
			req := continuingKept(kept)
			if !tc.keepsChosen {
				// REFUSED: the record keeps another capture than the one the
				// launch was asked to continue.
				req.Session.CaptureSeq = 2
			}
			if !tc.chosen {
				req.Session = nil
			}
			req.Continue = &Continuation{SessionID: "sess-ask", Brief: "carry on in the box you left"}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if got.kind != tc.kind || got.session != tc.session {
				t.Fatalf("the launch hands a %s brief and session %q, want a %s brief and %q", got.kind,
					got.session, tc.kind, tc.session)
			}
			var lines []string
			for line := range strings.Lines(logs.String()) {
				if strings.Contains(line, "msg=executor_session_not_continued") {
					lines = append(lines, line)
				}
			}
			if len(lines) != len(tc.reasons) {
				t.Fatalf("logged %d not-continued lines, want %d: %q", len(lines), len(tc.reasons), lines)
			}
			for i, line := range lines {
				if !strings.Contains(line, tc.reasons[i]) || !strings.Contains(line, tc.outcome) {
					t.Errorf("line %d is %q, want it saying %q and naming what the launch continues: %q", i,
						line, tc.reasons[i], tc.outcome)
				}
			}
		})
	}
}

// A RELAUNCH IN ITS JOB'S OWN BOX CONTINUES THE COPY THAT WENT PAST ITS
// CAPTURE: a person's answer carrying the round on there, offered the box's
// copy of a capture taken while the job still worked, continues the copy and
// restores nothing over it. The capture is restored as before in another box,
// where nothing offers the copy, where the copy is another session, and where
// the capture is final; beside a refused final take as beside none.
//
// Mutation: restore the capture over the copy, pass a final capture over, or
// pass one over for another session's copy, a box with no copy or no copy at
// all, and a case goes red.
func TestARelaunchInItsJobsOwnBoxContinuesTheCopyThatWentPastItsCapture(t *testing.T) {
	t.Parallel()
	const (
		copyBrief = "carry the box's copy on"
		keptBrief = "carry on in the box you left"
		newBox    = "carry on in a box that replaced yours"
	)
	for name, tc := range map[string]struct {
		// final keeps a capture taken once its job had ended, and unnamed one
		// whose stamp names no session.
		final, unnamed, reused bool
		// offered is the copy the launch is offered, nil for none at all.
		offered *Continuation
		want    BriefKind
		brief   string
		session string
		// kept is the launch's decision: the turn's kept capture, carried
		// into the box or found there.
		kept, carried bool
	}{
		"its own box, offered the copy": {reused: true,
			offered: &Continuation{SessionID: "s-1", Brief: copyBrief},
			want:    BriefContinued, brief: copyBrief, session: "s-1"},
		"another box, offered the copy": {
			offered: &Continuation{SessionID: "s-1", Brief: copyBrief},
			want:    BriefContinuedNewBox, brief: newBox, session: "s-1", kept: true, carried: true},
		"its own box, a lost job's carry": {reused: true,
			offered: &Continuation{Brief: "carry the lost round on"},
			want:    BriefContinued, brief: keptBrief, session: "s-1", kept: true, carried: true},
		"its own box, a send-back": {reused: true,
			want: BriefContinued, brief: keptBrief, session: "s-1", kept: true, carried: true},
		"its own box, the copy past its floor": {reused: true,
			offered: &Continuation{Brief: copyBrief},
			want:    BriefContinued, brief: keptBrief, session: "s-1", kept: true, carried: true},
		"its own box, a copy of another session": {reused: true,
			offered: &Continuation{SessionID: "s-other", Brief: copyBrief},
			want:    BriefContinued, brief: keptBrief, session: "s-1", kept: true, carried: true},
		"its own box, a final capture": {final: true, reused: true,
			offered: &Continuation{SessionID: "s-1", Brief: copyBrief},
			want:    BriefContinued, brief: keptBrief, session: "s-1", kept: true},
		"its own box, a capture naming no session, no copy": {unnamed: true, reused: true,
			offered: &Continuation{Brief: copyBrief},
			want:    BriefContinued, brief: keptBrief, session: "s-1", kept: true, carried: true},
	} {
		for _, refusedFinal := range []bool{false, true} {
			shape := "the job's newest"
			if refusedFinal {
				shape = "beside a final take of the job refused"
			}
			t.Run(shape+": "+name, func(t *testing.T) {
				t.Parallel()
				rig := newCoordRig(t)
				rig.coordinator.spoolDir = t.TempDir()
				rig.runner.Carries = true
				key := objstore.KeyAt(time.Now())
				// THE SHAPE A CAPPED COLLECTION LEAVES where its end of the job
				// failed: the job's newest capture, taken while it still worked,
				// cut off, landed and kept.
				kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: tc.final,
					Interrupted: !tc.final, Through: new(0), Object: key.String(),
					ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, SessionID: "s-1"}
				if tc.unnamed {
					kept.SessionID = ""
				}
				restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
					archives: map[string][]byte{key.String(): []byte("the transcript")}}
				rig.coordinator.restore = restore
				opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept}
				if refusedFinal {
					final := kept
					final.CaptureSeq, final.State, final.Final, final.Interrupted = 2, CaptureRefused, true, false
					final.Reason = "the object store had no room for the session"
					opened.Capture = &final
				}
				req := continuingKept(kept)
				req.Continue = tc.offered
				got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
					rig.runner, tc.reused, false)
				t.Cleanup(got.release)
				if got.kind != tc.want || got.brief != tc.brief || got.session != tc.session {
					t.Fatalf("the launch hands brief %q (%s) on session %q; want %q (%s) on %q", got.brief, got.kind,
						got.session, tc.brief, tc.want, tc.session)
				}
				if got.decision().Kept != tc.kept {
					t.Errorf("the launch tells its caller %+v, want the turn's kept session %v", got.decision(), tc.kept)
				}
				if (got.carried != nil) != tc.carried {
					t.Errorf("the launch carries %+v into the box, want a capture carried %v", got.carried, tc.carried)
				}
				// NOTHING OF THE CAPTURE IS READ for a launch that passes it over:
				// not even the turn's row, which only a capture tried is checked
				// against.
				if asked := restore.askedFor(); !tc.kept && len(asked) != 0 {
					t.Errorf("a launch that continues the box's copy read the rows %v", asked)
				}
			})
		}
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

		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
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

// fakeRestorer is a runner of a harness that restores its own sessions, and why
// it refuses what it does not.
type fakeRestorer struct{ why string }

func (f fakeRestorer) Restores(SessionArchive) (bool, string) { return f.why == "", f.why }
func (f fakeRestorer) RestoresUnder(archive SessionArchive, _ string) (bool, string) {
	return f.Restores(archive)
}
func (f fakeRestorer) CheckCarry(context.Context, Sandbox, SessionArchive) (CarryCheck, error) {
	return CarryCheck{Continues: f.why == "", Why: f.why}, nil
}
func (f fakeRestorer) CheckInPlace(context.Context, Sandbox) (CarryCheck, error) {
	return CarryCheck{Continues: f.why == "", Why: f.why}, nil
}

// WHETHER A SESSION CAN BE CONTINUED IS ONE DECISION, of the turn's row on the
// log and its run's record: continued where the followed job's newest kept
// capture is named by the row and restored by the runner, whatever newer
// capture or save sits beside it; not, each saying why, for every condition
// [ContinuityOf] names (an ending decided among them). A newest capture the
// record stamps refused while the row names it is not continued, nor the older
// one kept beside it, and the reason says the log keeps it.
//
// Mutation: drop any condition and its row goes red; answer the refused
// capture by the record or the row alone, and its row misstates it; continue
// only the newest, or refuse beside a newer one not yet refused, and the rows
// beside a newer capture go red.
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
	// beside is the record keeping kept with newer as the job's newest
	// capture, and save under way where it is set.
	beside := func(newer SessionCapture, save *SessionCapture) *PendingRun {
		return &PendingRun{TurnID: "t1", LaunchSeq: 2, Capture: &newer, Kept: &kept, Saving: save}
	}
	newer := func(state CaptureState, at time.Time) SessionCapture {
		c := kept
		c.CaptureSeq, c.State, c.At, c.Object = 2, state, at, objstore.KeyAt(at).String()
		return c
	}
	now := time.Now()
	standing := newer(CaptureRefused, now)
	standing.Reason, standing.StandbyAt, standing.StandbyTaker = "past the seat's budget", now, takingProcess
	save := newer(CaptureBegun, now)
	save.Interrupted = true
	overtaking := newer(CaptureRefused, now)
	overtaking.CaptureSeq, overtaking.Final, overtaking.Reason = 3, true, "past the seat's budget"
	earlierKept := kept
	earlierKept.LaunchSeq = 1
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
		"the kept capture beside a newer one of its job refused": {row: live, run: beside(refused, nil),
			follows: 2, restorer: fakeRestorer{}, continues: true},
		"the kept capture beside a newer one of its job being kept": {row: live,
			run: beside(newer(CaptureBegun, now), nil), follows: 2, restorer: fakeRestorer{}, continues: true},
		"the kept capture beside a newer one of its job begun long ago": {row: live,
			run: beside(newer(CaptureBegun, now.Add(-2*SessionCaptureBudget)), nil), follows: 2,
			restorer: fakeRestorer{}, continues: true},
		"the kept capture beside a newer one of its job a read stands by for": {row: live,
			run: beside(standing, nil), follows: 2, restorer: fakeRestorer{}, continues: true},
		"the kept capture beside a newer one of its job awaiting its answer": {row: live,
			run: beside(newer(CaptureUnresolved, now), nil), follows: 2, restorer: fakeRestorer{}, continues: true},
		"the kept capture beside a save of its job under way, a final take refused": {row: live,
			run: beside(overtaking, &save), follows: 2, restorer: fakeRestorer{}, continues: true},
		"a newest capture not kept, the job keeping none before it": {row: live, run: &PendingRun{TurnID: "t1",
			LaunchSeq: 2, Capture: &refused, Kept: &earlierKept}, follows: 2, restorer: fakeRestorer{},
			why: "was not kept: refused: past the seat's budget, and the job kept none before it"},
		"a newer capture the log took ahead of its stamp": {row: moved,
			run: beside(newer(CaptureBegun, now), nil), follows: 2, restorer: fakeRestorer{},
			why: "the executor log names the turn's capture (launch 2, capture 2), and its run's record keeps " +
				"(launch 2, capture 1)"},
		"a kept capture that states no save point": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2,
			Capture: &unplaced, Kept: &unplaced}, follows: 2, restorer: fakeRestorer{},
			why: "states no save point"},
		"a log naming another capture": {row: moved, run: run, follows: 2, restorer: fakeRestorer{},
			why: "the executor log names the turn's capture"},
		"a refused capture the log names all the same": {row: moved, run: &PendingRun{TurnID: "t1",
			LaunchSeq: 2, Capture: &settled, Kept: &kept}, follows: 2, restorer: fakeRestorer{},
			why: "(launch 2, capture 2) refused: its taker node-a stopped before it was kept; the executor " +
				"log keeps it all the same", unsaid: "was not kept"},
		"a refused capture the log names all the same, nor the older one kept beside it": {row: moved,
			run: beside(settled, nil), follows: 2, restorer: fakeRestorer{},
			why: "nor the older capture of it the record keeps (launch 2, capture 1), which the turn's row " +
				"does not name", unsaid: "was not kept"},
		"a turn that kept none": {row: live, run: &PendingRun{TurnID: "t1", LaunchSeq: 2}, follows: 2,
			restorer: fakeRestorer{}, why: "kept no capture"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ContinuityOf(tc.row, tc.run, tc.follows, RestorerUnder(tc.restorer, ""))
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
// job kept its session; the job after it (a send-back, or one a person's answer
// carried on) left no capture: its box went with its host. It was told what
// the first session never read, and call numbering differs where a send-back
// began a round. So the relaunch after it continues nothing, asked by the
// caller and again at the launch, and starts a session of its own, saying why.
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
		if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, "")); !strings.Contains(why,
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
// the capture's own life of an hour and more. Past the wait it continues the
// capture its job kept before, as it would beside one never kept.
//
// Mutation: wait out the capture's own budget on the relaunch, or stop waiting
// at once, or refuse the kept capture beside the one still being kept, and a
// case goes red.
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
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
			if why != "" || !continued.Capture.samePlace(kept) {
				t.Fatalf("a relaunch whose job's newer capture is still being kept continues %+v (%q), want "+
					"the capture the job kept before it", continued, why)
			}
			if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
				t.Errorf("the row holds %+v, want the newer capture still being kept", got)
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
			continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
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
// outcome. A stamp left begun reads as a keep still under way (the turn's end
// waits on it, and a relaunch reads a landed capture as one it cannot
// continue), and a place never reserved is a capture nobody knows was made; so
// each is asked again until the store takes it, within a record's budget.
//
// IN A SYNCTEST BUBBLE, so the budget runs on the bubble's clock, never a
// starved runner's scheduling.
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
		c := &Coordinator{pending: reserving, recordBudget: 200 * time.Millisecond, sessionRetry: time.Millisecond,
			life: t.Context()}
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
// The run's script packs the session whatever its harness's exit status, so a
// harness killed while its wrapper lived on (a signal, its memory limit) is
// packed with its transcript ending wherever the kill found it: the capture
// says so, and the launch continuing it restores it over the box's copy and
// tells its run it may end mid-call. The executor log's row says so too, with
// the last request's context, since the run's record dies with the turn.
//
// Mutation: read a script-packed session as whole, or leave the cut-off or
// context off the stamp recordCapture hands the log, and a case goes red; the
// log row is held by
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
// calls: the highest call the row held when packed, and the highest places
// below it with no call (in flight, a slow write), within the bound, for its
// continuation to list. This build writes them for that build, mid-upgrade,
// and reads its own count instead (SessionCapture.Through): see the long
// round's case below, where places the bound dropped fill the slots and a call
// in flight below them is named in none, which that build reads as answered.
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
// whose write landed after its answer was lost is marked unanswered: the
// session lacks those notes however cleanly its harness ended, yet holds the
// answered reads after them, which one number cannot say. So the cursor stops
// before the first read marked unanswered, whichever job's take it was (a mark
// can land after a later brief was built). A launch continuing the session
// tells every note past it. A read a later brief told again is no longer
// marked; a capture cut off states none either way.
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

// EVERY STAMP OF A CAPTURE STATES THE NOTES ITS SESSION HOLDS, TAKEN OR REFUSED
// ([SessionCapture.NoteCursor]): where the harness ended its own run, every
// read up to the first marked unanswered; where it was cut off, none, for a
// capture taken and one refused before its place alike, since an older build
// tells a continuation by the stamp alone. A served note unread stops it before
// its place, a read marked restarted stops it, a confirmed read counts, and an
// item stops nothing.
//
// Mutation: stamp a capture cut off with every note read, or past an unanswered
// take, a served note's place or a restarted read, or short of a confirmed read
// or an item's place, and that case goes red.
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
		// served hands the job's second note over by a hook's serve rather
		// than a take: "served" leaves the serve unconfirmed, and "held" and
		// "restarted" confirm it, its session showing it taken after its last
		// restart or only before.
		served string
		// item files an item for the job after both notes, and serves it,
		// unconfirmed.
		item bool
	}{
		{name: "a job whose harness ended its own run, its capture taken"},
		{name: "a job whose harness was killed, its capture taken", cutOff: true},
		{name: "a job whose harness ended its own run, its take refused", untaken: true},
		{name: "a job whose harness was killed, its take refused", cutOff: true, untaken: true},
		{name: "a job whose harness ended its own run, a take of it unanswered, its capture taken",
			unanswered: true},
		{name: "a job whose harness ended its own run, a take of it unanswered, its take refused",
			unanswered: true, untaken: true},
		{name: "a job whose harness ended its own run, a note its hook was served unconfirmed",
			served: "served"},
		{name: "a job whose harness ended its own run, a note its session shows taken", served: "held"},
		{name: "a job whose harness ended its own run, a note its session shows taken before a restart",
			served: "restarted"},
		{name: "a job whose harness ended its own run, an item for it served", item: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				launched := rig.launchKept(t, "t1")
				serve := func(id string) {
					t.Helper()
					if served, err := rig.pending.ServeHook(t.Context(), "t1", HookServe{IDs: []string{id},
						Launch: launched.LaunchID, Statuses: []string{StatusRunning},
						Serve: HookServed{Take: "take-" + id,
							Boundary: string(types.SteerBoundaryToolBatch)}}); err != nil || len(served) != 1 {
						t.Fatalf("serving note %s served %d (%v), want it", id, len(served), err)
					}
				}
				// TWO OF THE JOB'S NOTES HANDED OVER, so the record says two
				// were read, or placed.
				for _, id := range []string{"first", "second"} {
					fileNote(t, rig.pending, "t1", id)
					if id == "second" && tc.served != "" {
						serve(id)
						continue
					}
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
				verdict := HookSettle{Launch: launched.LaunchID}
				switch tc.served {
				case "held":
					verdict.Held = []string{"take-second"}
				case "restarted":
					verdict.Restarted = []string{"take-second"}
				}
				if verdict.Held != nil || verdict.Restarted != nil {
					if settled, err := rig.pending.SettleHook(t.Context(), "t1", verdict); err != nil ||
						len(settled) != 1 {
						t.Fatalf("the verdict on the second note = %d, %v, want its note", len(settled), err)
					}
				}
				if tc.item {
					// PUT ON THE RECORD BY THE STORE'S OWN WRITE, as an item
					// is filed: no filer may file one ([ErrNoteKind]).
					if _, won, _, err := rig.pending.writeFound(t.Context(), "t1", true, Fence{},
						func(run *PendingRun) bool {
							run.Steer.Seq++
							run.Steer.Notes = append(slices.Clone(run.Steer.Notes), RunNote{ID: "item",
								Seq: run.Steer.Seq, Sealed: []byte("sealed:item"), At: rig.now,
								For: launched.LaunchID})
							return true
						}); err != nil || !won {
						t.Fatalf("put the job's item on its record: %v, %v", won, err)
					}
					serve("item")
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
				case tc.unanswered, tc.served == "served", tc.served == "restarted":
					want = 1
				case tc.item:
					want = 3
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
// half a person's answer takes beside it (the parked box, by the launch's own
// tests of a box it continues in). Undecided, never a no, wherever a relaunch
// would not decide now either ([ReadContinuityOf]'s list: a stamp it waits on
// or asks, by its own place, an earlier job's keep only while the turn kept
// none), and decided once the resumed turn says what it does. A row naming a
// capture the record stamps refused is not continued, saying the log keeps it.
//
// Mutation: drop the parked box's half or a test of it (its entry included), read an undecided state
// as a no or a decided one as undecided, decide or wait on the wrong stamp
// (past its window, an earlier job's, a company keeping none), name the newest
// place for a save, or decide by [ContinuityOf] alone, and a case goes red.
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
	target := RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: Direct, Node: "node-a", Restorer: fakeRestorer{}}
	older := target
	older.Restorer = fakeRestorer{why: "older than 2.1.300"}
	moved := older
	moved.Node = "node-b"
	// bound is older on a harness not measured to resume on its launch's route,
	// launched under entry-a, and rebound the same under entry-b.
	bound := older
	bound.LaunchRouted, bound.Entry = false, "entry-a"
	rebound := bound
	rebound.Entry = "entry-b"
	final := kept
	final.Final = true
	// unanswered is the job's capture after its kept one, its object minted at
	// minted, its record sent and never answered.
	unanswered := func(minted time.Time) *SessionCapture {
		c := kept
		c.CaptureSeq, c.State, c.Object, c.At = 2, CaptureUnresolved, objstore.KeyAt(minted).String(), minted
		return &c
	}
	// overtook is the job's final take at the place after save, refused: a
	// read that collected the job while save was still being kept, whose keep
	// the store then refused.
	overtook := func(save SessionCapture) *SessionCapture {
		c := final
		c.CaptureSeq, c.State, c.Interrupted, c.At = save.CaptureSeq+1, CaptureRefused, false, now
		c.Reason = "the object store had no room for the session"
		return &c
	}
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
		// moved is a row naming the place after the kept capture's.
		moved bool
	}{
		"its kept capture":  {target: target, decided: true, in: ContinuesInAnyBox},
		"in its parked box": {target: older, decided: true, in: ContinuesInParkedBox, whyHolds: "older than"},
		"in its parked box, under the entry its job ran under": {target: bound, decided: true,
			in: ContinuesInParkedBox, whyHolds: "older than", run: func(r *PendingRun) { r.Launch.Entry = "entry-a" }},
		"in its parked box, under another entry": {target: rebound, decided: true,
			whyHolds: "continues only under the entry it ran under",
			run:      func(r *PendingRun) { r.Launch.Entry = "entry-a" }},
		"its box reclaimed": {target: older, decided: true, whyHolds: "is gone",
			run: func(r *PendingRun) { r.SandboxID = "" }},
		"a member elsewhere": {decided: true, whyHolds: "in the container cell",
			target: RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: Container, Node: "node-a",
				Restorer: older.Restorer}},
		"a member of another harness": {decided: true, whyHolds: "launched on opencode",
			target: RelaunchTarget{Harness: "opencode", LaunchRouted: true, Placement: Direct, Node: "node-a",
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
		"a capture whose record awaits its answer": {target: target, whyHolds: "asks the executor log",
			run: func(r *PendingRun) { r.Capture = unanswered(now.Add(-time.Minute)) }},
		"a save of the job being kept, beside a refused final take that overtook it": {target: target,
			whyHolds: "(launch 2, capture 2) is still being kept", run: func(r *PendingRun) {
				save := kept
				save.CaptureSeq, save.State, save.Interrupted, save.At = 2, CaptureBegun, true, now.Add(-time.Second)
				r.Saving, r.Capture = &save, overtook(save)
			}},
		"a save of the job whose record awaits its answer, beside a refused final take that overtook it": {
			target: target, whyHolds: "(launch 2, capture 2) was sent and never answered",
			run: func(r *PendingRun) {
				save := unanswered(now.Add(-time.Minute))
				save.Interrupted = true
				r.Saving, r.Capture = save, overtook(*save)
			}},
		"a lost job's capture whose record awaits its answer": {target: target, whyHolds: "asks the executor log",
			run: func(r *PendingRun) {
				r.Status, r.Launch.Collected, r.Lost = StatusRunning, true, &JobLoss{Cause: LossBoxGone}
				r.Capture = unanswered(now.Add(-time.Minute))
			}},
		"a capture whose record went unanswered past the window a record may name it in": {target: target,
			decided: true, in: ContinuesInAnyBox,
			run: func(r *PendingRun) {
				r.Capture = unanswered(now.Add(-objstore.RecordWithin - 2*objstore.AssumedClockSkew - time.Minute))
			}},
		"a newer capture of the job refused, beside the one it kept": {target: target, decided: true,
			in: ContinuesInAnyBox, run: func(r *PendingRun) {
				refused := kept
				refused.CaptureSeq, refused.State, refused.Reason = 2, CaptureRefused, "past the seat's budget"
				r.Capture = &refused
			}},
		"a refused capture the row names, beside the one the job kept": {target: target, moved: true,
			decided: true, in: ContinuesInParkedBox, whyHolds: "the executor log keeps it all the same",
			run: func(r *PendingRun) {
				settled := kept
				settled.CaptureSeq, settled.State, settled.Reason = 2, CaptureRefused, takerStoppedWhy(takingProcess)
				r.Capture = &settled
			}},
		"an earlier job's capture whose record awaits its answer": {target: target, decided: true,
			in: ContinuesInParkedBox, whyHolds: "has left no capture", run: func(r *PendingRun) {
				r.LaunchSeq, r.Capture = 3, unanswered(now.Add(-time.Minute))
			}},
		"an earlier job's capture being kept beside a capture the turn kept": {target: target, decided: true,
			in: ContinuesInParkedBox, whyHolds: "has left no capture", run: func(r *PendingRun) {
				begun := kept
				begun.CaptureSeq, begun.State, begun.At = 2, CaptureBegun, now.Add(-time.Second)
				r.LaunchSeq, r.Capture = 3, &begun
			}},
		"an earlier job's save being kept beside a capture the turn kept": {target: target, decided: true,
			in: ContinuesInParkedBox, whyHolds: "has left no capture", run: func(r *PendingRun) {
				save := kept
				save.CaptureSeq, save.State, save.Interrupted, save.At = 2, CaptureBegun, true, now.Add(-time.Second)
				r.LaunchSeq, r.Saving, r.Capture = 3, &save, overtook(save)
			}},
		"an earlier job's capture being kept, the turn having kept none": {target: target,
			whyHolds: "an earlier job of the turn (launch 2, capture 2) is still being kept",
			run: func(r *PendingRun) {
				begun := kept
				begun.CaptureSeq, begun.State, begun.At = 2, CaptureBegun, now.Add(-time.Second)
				r.LaunchSeq, r.Capture, r.Kept = 3, &begun, nil
			}},
		"an earlier job's save being kept, the turn having kept none": {target: target,
			whyHolds: "an earlier job of the turn (launch 2, capture 2) is still being kept",
			run: func(r *PendingRun) {
				save := kept
				save.CaptureSeq, save.State, save.Interrupted, save.At = 2, CaptureBegun, true, now.Add(-time.Second)
				r.LaunchSeq, r.Saving, r.Capture, r.Kept = 3, &save, overtook(save), nil
			}},
		"a capture whose record awaits its answer, in a company that keeps no session": {decided: true,
			in: ContinuesInParkedBox, whyHolds: "keeps no executor session",
			target: RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: Direct, Node: "node-a",
				Restorer: fakeRestorer{}, NoKept: NoKeptSession},
			run: func(r *PendingRun) { r.Capture = unanswered(now.Add(-time.Minute)) }},
		"a target that cannot be said": {whyHolds: "sitting out",
			target: RelaunchTarget{Undecided: true, Why: "every member is sitting out its plan"}},
		"no member at all": {decided: true, whyHolds: "no runner member",
			target: RelaunchTarget{Why: "the seat's chain has no runner member"}},
		"a decided ending": {target: target, decided: true, whyHolds: "is ending", ending: true,
			run: func(r *PendingRun) { r.Ending = &RecordedEnding{ID: "ending-1"} }},
		"a company that keeps no session": {decided: true, in: ContinuesInParkedBox,
			whyHolds: "keeps no executor session",
			target: RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: Direct, Node: "node-a",
				Restorer: fakeRestorer{}, NoKept: NoKeptSession}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := parked()
			if tc.run != nil {
				tc.run(&run)
			}
			read := row
			if tc.moved {
				read.CaptureSeq = 2
			}
			got := ReadContinuityOf(read, "", &run, tc.target, ContinuedByNextTurn, now)
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
	// ONE PREDICATE BEHIND THE READER AND THE RELAUNCH: past its wait, a
	// relaunch continues the capture the job kept beside a newer capture or
	// save of that job that may still land, which [ContinuityOf] says; within
	// it, it waits for or asks the newer one, and continues that one where it
	// lands. So for every way the newer one may still land, the decision past
	// the wait continues the kept capture, and the reader is undecided.
	for name, newer := range map[string]func(*PendingRun){
		"a newer capture being kept": func(r *PendingRun) {
			begun := kept
			begun.CaptureSeq, begun.State, begun.At = 2, CaptureBegun, now.Add(-time.Second)
			r.Capture = &begun
		},
		"a newer capture a read stands by for": func(r *PendingRun) {
			refused := kept
			refused.CaptureSeq, refused.State, refused.Reason = 2, CaptureRefused, "past the seat's budget"
			refused.StandbyAt, refused.StandbyTaker = now.Add(-time.Second), takingProcess
			r.Capture = &refused
		},
		"a newer capture awaiting its answer": func(r *PendingRun) { r.Capture = unanswered(now.Add(-time.Minute)) },
		"a save being kept beside a refused final take": func(r *PendingRun) {
			save := kept
			save.CaptureSeq, save.State, save.Interrupted, save.At = 2, CaptureBegun, true, now.Add(-time.Second)
			r.Saving, r.Capture = &save, overtook(save)
		},
		"a save awaiting its answer beside a refused final take": func(r *PendingRun) {
			save := unanswered(now.Add(-time.Minute))
			save.Interrupted = true
			r.Saving, r.Capture = save, overtook(*save)
		},
	} {
		t.Run("past and within the wait: "+name, func(t *testing.T) {
			t.Parallel()
			run := parked()
			newer(&run)
			if past := ContinuityOf(row, &run, run.LaunchSeq, target.restorer()); !past.Continues {
				t.Fatalf("ContinuityOf = %+v, want the capture the job kept continued past the wait", past)
			}
			if got := ReadContinuityOf(row, "", &run, target, ContinuedByNextTurn, now); got.Decided {
				t.Fatalf("ReadContinuityOf = %+v, want undecided while the relaunch would still wait for or "+
					"ask the newer one", got)
			}
		})
	}
}

// A ROW IS READ AGAIN AS A RELAUNCH READS IT ONLY WHERE THAT CAN CHANGE ITS
// ANSWER ([RereadsRow]): where the decision reaches the row's capture and the
// row names another than the record keeps, or the kept one beside a newer
// capture the record did not keep; never where it names the kept one alone,
// or the job works, or a stamp the relaunch waits on or asks is in flight, or
// the turn ended. An earlier job's keep beside a kept capture, and a capture
// past its window, wait on nothing, and the row is read again.
//
// Mutation: read every live row again, or none, or the wrong one of the cases
// above, and a case goes red.
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
	// earlierKeeping is a run whose job before the one a relaunch would follow
	// is still keeping a capture after the turn's kept one.
	earlierKeeping := run()
	earlierKeeping.LaunchSeq = 3
	earlierKeeping.Capture = &SessionCapture{LaunchSeq: 2, CaptureSeq: 3, State: CaptureBegun,
		At: now.Add(-time.Second), Through: new(0)}
	// unanswered is a run whose newest capture's record was sent and never
	// answered, its object minted at minted.
	unanswered := func(minted time.Time) *PendingRun {
		r := run()
		r.Capture = &SessionCapture{LaunchSeq: 2, CaptureSeq: 3, State: CaptureUnresolved,
			Object: objstore.KeyAt(minted).String(), At: minted, Through: new(0)}
		return r
	}
	// refusedNewer is a run whose job's capture after the kept one was
	// refused.
	refusedNewer := run()
	refusedNewer.Capture = &SessionCapture{LaunchSeq: 2, CaptureSeq: 3, State: CaptureRefused,
		Reason: "past the seat's budget", At: now, Through: new(0)}
	// besideASave is a run whose job's final take at place 4 was refused while
	// a save of the job at place 3 was in state, its object minted at minted.
	besideASave := func(state CaptureState, minted time.Time) *PendingRun {
		r := run()
		save := SessionCapture{LaunchSeq: 2, CaptureSeq: 3, State: state, Interrupted: true,
			Object: objstore.KeyAt(minted).String(), At: minted, Through: new(0)}
		final := SessionCapture{LaunchSeq: 2, CaptureSeq: 4, State: CaptureRefused, Final: true, At: now,
			Through: new(0)}
		r.Saving, r.Capture = &save, &final
		return r
	}
	for name, tc := range map[string]struct {
		row   executorkeep.TurnRow
		run   *PendingRun
		again bool
	}{
		"a row behind its run's record": {row: naming(1), run: run(), again: true},
		"a row whose job's save under way is being kept": {row: naming(1),
			run: besideASave(CaptureBegun, now.Add(-time.Second))},
		"a row whose job's save under way awaits its record's answer": {row: naming(1),
			run: besideASave(CaptureUnresolved, now.Add(-time.Minute))},
		"a row behind its record beside an earlier job's capture being kept": {row: naming(1),
			run: earlierKeeping, again: true},
		"a row naming the kept capture":    {row: naming(2), run: run()},
		"a row whose job is still working": {row: naming(1), run: working},
		"an ended turn's row":              {row: ended, run: run()},
		"a row whose run is over":          {row: naming(1)},
		"a row whose newest capture awaits its record's answer": {row: naming(3),
			run: unanswered(now.Add(-time.Minute))},
		"a row whose newest capture went unanswered past its window": {row: naming(3),
			run: unanswered(now.Add(-objstore.RecordWithin - 2*objstore.AssumedClockSkew - time.Minute)), again: true},
		"a row naming the kept capture beside a newer one of its job refused": {row: naming(2),
			run: refusedNewer, again: true},
	} {
		if got := RereadsRow(tc.row, tc.run, now); got != tc.again {
			t.Errorf("%s: read again %v, want %v", name, got, tc.again)
		}
	}
}

// A ROUND IS CARRIED ON IN THE SESSION ITS JOB'S BOX RAN, never the one the
// job's launch chose. A launch continuing a session names, beside it, the
// session its script starts of its own where the box cannot take that one up,
// and the box says which it started by the token its harness dials: a
// person's answer, or a replaced sign-in, carries the round on in the one the
// box said, and in none where the box's word never reached the row; there it
// continues from the turn's kept capture, or a session of its own. A launch
// continuing no session names no second: the one it named is the one it ran.
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
		if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, "")); why != NoKeptSession ||
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

// RowCrossing is none: the silent store is in the node's own process, so a
// row crosses nothing and a record's bound is its reason alone, which the
// cases below time.
func (s *silentStamps) RowCrossing() time.Duration { return 0 }

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
			c := &Coordinator{pending: &silentStamps{}, recordBudget: time.Minute, sessionRetry: time.Millisecond,
				life: t.Context()}
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
			// whose by its records' two graces after that, a pause's grace
			// before the read's own.
			read, cancel := context.WithTimeout(t.Context(), 3*discardGrace+2*time.Second)
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
