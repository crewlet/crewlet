package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// keptLog is the executor log and the session store a relaunch continues a kept
// session from, as a case plays them: each turn's row, and each object's archive.
type keptLog struct {
	mu       sync.Mutex
	rows     map[string]executordomain.Session
	archives map[string][]byte
}

func (k *keptLog) Session(_ context.Context, handle, turn string,
	_ statelog.Freshness) (executordomain.Session, error) {

	k.mu.Lock()
	defer k.mu.Unlock()
	row, ok := k.rows[turn]
	if !ok || row.Handle != handle {
		return executordomain.Session{}, executordomain.ErrNotFound
	}
	return row, nil
}

func (k *keptLog) Fetch(_ context.Context, row executordomain.Session, dst io.Writer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, ok := k.archives[row.Object.Key.String()]
	if !ok {
		return executordomain.ErrUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (k *keptLog) OpenSpooled(_ executordomain.Session, sealed io.Reader) (io.Reader, error) {
	return sealed, nil
}

// keptLaunch is a seat whose turn t1 launched one job, whose session a capture
// kept with what it accounts for — its calls, its notes, a harness cut off, its
// last request's context — and the launcher a relaunch of the turn is made
// through, over an engine whose coordinator continues kept sessions. ceiling
// caps the seat's day, zero capping nothing; spent is what the day has
// counted.
type keptLaunch struct {
	e    *Engine
	l    *agentLauncher
	jobs *sandbox.FakeRunner
	kept sandbox.SessionCapture
	ctx  context.Context
}

func newKeptLaunch(t *testing.T, contextTokens int64, ceiling, spent int) keptLaunch {
	t.Helper()
	c, seat := executorCompany(t)
	if ceiling > 0 {
		seat.TokenBudget = org.TokenCeilings{period.Day: ceiling}
	}
	sessions := &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}}
	e, runners := launchRigRestoring(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}), sessions)
	fleet := coordmemory.NewFleet()
	e.backends.Fleet = fleet
	jobs := runners["claude-code"]
	jobs.Carries = true
	l := launcherOn(e, c, seat, "t1")
	ctx := t.Context()
	if _, err := l.LaunchExecutor(ctx, launchRequest(l, nil)); err != nil {
		t.Fatalf("the turn's first launch: %v", err)
	}
	pending := e.sandbox.Load().pending
	row, _, err := pending.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	key := objstore.KeyAt(time.Now())
	kept := sandbox.SessionCapture{LaunchSeq: row.LaunchSeq, CaptureSeq: 1, Launch: row.LaunchID,
		State: sandbox.CaptureLanded, Final: true, Interrupted: true,
		BridgeSeq: 3, BridgeOpen: []int{2}, NoteCursor: 4, ContextTokens: contextTokens,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
		ArchiveFormat: sandbox.FakeArchiveFormat, Object: key.String(),
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, Home: "/boxes/4f2a/home"}
	if _, err := pending.StampCapture(ctx, "t1", kept); err != nil {
		t.Fatalf("StampCapture: %v", err)
	}
	sessions.mu.Lock()
	sessions.rows["t1"] = executordomain.Session{Handle: seat.Handle(), Turn: "t1",
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
		Object:        objstore.Object{Key: key, Hash: objstore.Hash(kept.ObjectHash), Size: kept.ObjectSize},
		ArchiveFormat: sandbox.FakeArchiveFormat, LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq,
		Home: kept.Home}
	sessions.archives[key.String()] = []byte("the transcript")
	sessions.mu.Unlock()
	if spent > 0 {
		id, ok := c.Org.AgentIDFor(seat)
		if !ok {
			t.Fatal("the seat has no agent id")
		}
		if _, err := fleet.PostCharge(ctx, coord.AgentScope(id.String()), spent,
			coord.WindowsAt(time.Now(), c.Config.Location())); err != nil {
			t.Fatalf("PostCharge: %v", err)
		}
	}
	return keptLaunch{e: e, l: l, jobs: jobs, kept: kept, ctx: ctx}
}

// relaunch launches the turn again continuing basis's session, with briefs
// that say which box they are for, and answers how the launch decided.
func (k keptLaunch) relaunch(t *testing.T, basis runner.SessionBasis, inPlace *runner.Continuation) (
	[]runner.SessionDecision, error) {
	t.Helper()
	req := launchRequest(k.l, nil)
	req.Brief = "the fresh brief"
	if basis.Continues() {
		req.Session = &runner.SessionContinuation{Basis: basis,
			Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours"}
	}
	req.Continue = inPlace
	var decided []runner.SessionDecision
	req.Decided = func(d runner.SessionDecision) { decided = append(decided, d) }
	_, err := k.l.LaunchExecutor(k.ctx, req)
	return decided, err
}

// A RELAUNCH IS TOLD THE KEPT SESSION OF THE JOB IT FOLLOWS, AS ITS CAPTURE
// ACCOUNTS FOR IT. The engine's launcher answers the runner's question from the
// coordinator's choice and the turn's row on the executor log: the session, its
// place, the calls it had been answered — the highest, and the places below it
// still in flight — the notes it had read, whether its harness was cut off,
// where it was last worked, and what its last request carried, which the
// launch's floor is made of.
//
// Mutation: drop any of them from the answer, and the brief that continues the
// session tells it the wrong things, or the floor counts nothing.
func TestAProbeAnswersTheKeptSessionOfTheJobARelaunchFollows(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	got := k.l.ProbeSession(k.ctx)
	want := runner.SessionBasis{LaunchSeq: k.kept.LaunchSeq, CaptureSeq: 1, SessionID: "s-1",
		BridgeSeq: 3, BridgeOpen: []int{2}, NoteCursor: 4, Interrupted: true, Home: "/boxes/4f2a/home",
		ContextTokens: 40000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the probe answered %+v, want %+v", got, want)
	}
}

// A RELAUNCH CONTINUES ITS SESSION IN THE BOX IT GETS, WITH THE BRIEF FOR THAT
// BOX. The launch is handed the session the probe answered with both its briefs
// — for the box it was last worked in and for any other — takes it up in
// whichever box the launch gets, starts the run on the brief that box makes
// true, and tells the runner how it decided before the run starts.
//
// Mutation: hand the sandbox layer either brief in the other's place, or tell
// the runner nothing, and this goes red.
func TestARelaunchContinuesItsSessionInTheBoxItGetsWithTheBriefForIt(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	decided, err := k.relaunch(t, k.l.ProbeSession(k.ctx), nil)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if len(decided) != 1 || !decided[0].Continues || !decided[0].Kept {
		t.Fatalf("the runner was told %+v, want once: the turn's kept session", decided)
	}
	want := "carry on in the box you left"
	if decided[0].NewBox {
		want = "carry on in a box that replaced yours"
	}
	started := k.jobs.Started()
	if last := started[len(started)-1]; last.Resume != "s-1" || last.Brief != want ||
		last.FreshBrief != "the fresh brief" {
		t.Fatalf("the relaunched job started with session %q, brief %q and fresh brief %q; want the kept "+
			"session and %q beside the fresh brief", last.Resume, last.Brief, last.FreshBrief, want)
	}
}

// A CONTINUED LAUNCH IS HELD TO ITS SESSION'S FLOOR, AND HELD ON IT. A relaunch
// that continues its session sends the session's context before its brief, so
// its launch needs that much room: below it the launch is refused as a budget
// refusal carrying that floor — the turn's kept session's, or the job's own in
// its box — and the resume is held on a window with room for it, never released
// into the same refusal by the static floor a fresh launch needs.
//
// Mutation: hold the launch to the static floor, leave the session's context out
// of the floor, or hold the resume on the static floor, and a case goes red.
func TestAContinuedLaunchIsHeldToItsSessionsFloor(t *testing.T) {
	t.Parallel()
	for name, inPlace := range map[string]bool{"the turn's kept session": false, "its own in its box": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A DAY OF 100000 WITH 70000 SPENT: room for the static floor
			// (25000), and none for a session that sends 40000 first.
			k := newKeptLaunch(t, 40000, 100000, 70000)
			basis := k.l.ProbeSession(k.ctx)
			var continued *runner.Continuation
			if inPlace {
				basis = runner.SessionBasis{Why: "the newest capture was not kept", ContextTokens: 40000}
				continued = &runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: 40000}
			}
			_, err := k.relaunch(t, basis, continued)
			var refused *floorRefusal
			if !errors.Is(err, toolloop.ErrBudgetExhausted) || !errors.As(err, &refused) ||
				refused.floor.context != 40000 {
				t.Fatalf("a launch continuing a 40000-token session with 30000 left = %v, want a budget "+
					"refusal on the session's floor", err)
			}
			held := k.e.heldByBudget(k.ctx, k.l.company, k.l.seat.Handle(), err)
			if held == nil || !strings.Contains(held.Refusal.Reason, "continues its session") {
				t.Fatalf("held = %+v, want the resume held on the session's floor", held)
			}
		})
	}
}

// A SESSION NO WINDOW CAN CARRY STARTS FRESH; ONE ANY WINDOW CAN, OR NONE CAPS,
// IS CONTINUED. The launch weighs the session's context with the brief that
// continues it against the smallest ceiling of the seat's capped windows: past
// it, no window turning over ever leaves room, so the session is not continued
// — the run starts one of its own on the static floor, told so before it
// starts — where a session within it is continued whatever its size, and a seat
// with no capped window continues any: no budget is protected by refusing it.
//
// Mutation: refuse a session for its size alone, wait for a floor no window can
// meet, or decide by the size of the session without its brief, and a case goes
// red.
func TestASessionNoWindowCanCarryStartsFresh(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		context   int64
		ceiling   int
		continues bool
	}{
		"past the smallest ceiling":   {context: 150000, ceiling: 100000},
		"within the smallest ceiling": {context: 90000, ceiling: 100000, continues: true},
		"no window capped":            {context: 150000, continues: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newKeptLaunch(t, tc.context, tc.ceiling, 0)
			decided, err := k.relaunch(t, k.l.ProbeSession(k.ctx), nil)
			if err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			started := k.jobs.Started()
			last := started[len(started)-1]
			if continued := last.Resume == "s-1"; continued != tc.continues ||
				len(decided) != 1 || decided[0].Continues != tc.continues {
				t.Fatalf("the relaunch started with session %q, deciding %+v; want continuing %v",
					last.Resume, decided, tc.continues)
			}
			if !tc.continues && last.Brief != "the fresh brief" {
				t.Errorf("a session no window carries started with brief %q, want the fresh one", last.Brief)
			}
		})
	}
}
