package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// keptLog is the executor log and the session store a relaunch continues a kept
// session from, as a case plays them: each turn's row, and each object's archive.
type keptLog struct {
	mu       sync.Mutex
	rows     map[string]executordomain.Session
	archives map[string][]byte
}

func (k *keptLog) Session(_ context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	row, ok := k.rows[turn]
	if !ok || row.Handle != handle {
		return executorkeep.TurnRow{}, executordomain.ErrNotFound
	}
	return row.Kept(), nil
}

func (k *keptLog) Fetch(_ context.Context, row executorkeep.TurnRow, dst io.Writer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, ok := k.archives[row.Object.Key.String()]
	if !ok {
		return executordomain.ErrUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (k *keptLog) OpenSpooled(_ executorkeep.TurnRow, sealed io.Reader) (io.Reader, error) {
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
		Through: new(2), Omitted: []int{1}, NoteCursor: 4, ContextTokens: contextTokens,
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

// claimed puts the turn's job under the claim that relaunches it, its box held,
// as a person's answer carries a parked run on: the job's suspension written,
// and its resume claimed.
func (k keptLaunch) claimed(t *testing.T) {
	t.Helper()
	pending := k.e.sandbox.Load().pending
	blob, err := execstate.Encode(execstate.State{Version: execstate.Version, Round: 1,
		Task: "fix the failing test"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if ok, err := pending.MarkSuspended(k.ctx, "t1", sandbox.Suspension{State: blob, Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended: %v %v", ok, err)
	}
	row, _, err := pending.Get(k.ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok, err := pending.ClaimForResume(k.ctx, "t1", sandbox.Tail{Launch: row.LaunchID,
		From: []string{sandbox.StatusRunning}}, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume: %v %v", ok, err)
	}
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
// place, the calls it had been answered — its save point, how many of the
// round's calls the log had taken when its capture read it, and the reads at
// or below it that its job's brief counted without listing them — the notes it
// had read, whether its harness was cut off, where it was last worked, what its
// last request carried, which the launch's floor is made of, the harness that
// wrote it, which the member the launch picks must run, and the turn whose
// session it is, which a run that starts one of its own instead is told it may
// read.
//
// Mutation: drop any of them from the answer, and the brief that continues the
// session tells it the wrong things, the floor counts nothing, the launch
// cannot hold its member to the session's harness, or a fresh run is never told
// the session is there.
func TestAProbeAnswersTheKeptSessionOfTheJobARelaunchFollows(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	got := k.l.ProbeSession(k.ctx)
	want := runner.SessionBasis{LaunchSeq: k.kept.LaunchSeq, CaptureSeq: 1, SessionID: "s-1",
		Harness: "claude-code", Through: 2, Owed: []int{1}, NoteCursor: 4, Interrupted: true,
		Home: "/boxes/4f2a/home", ContextTokens: 40000, KeptTurn: "t1"}
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
// into the same refusal by a window with room for a fresh launch alone.
//
// Mutation: hold the launch to the static floor, leave the session's context out
// of the floor, or hold the resume on the static floor, and a case goes red.
func TestAContinuedLaunchIsHeldToItsSessionsFloor(t *testing.T) {
	t.Parallel()
	for name, inPlace := range map[string]bool{"the turn's kept session": false, "its own in its box": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A DAY OF 100000 WITH 70000 SPENT: room for a fresh launch (the
			// static floor, 25000, beside its short brief), and none for a
			// session that sends 40000 first.
			k := newKeptLaunch(t, 40000, 100000, 70000)
			basis := k.l.ProbeSession(k.ctx)
			var continued *runner.Continuation
			if inPlace {
				basis = runner.SessionBasis{Why: "the newest capture was not kept", ContextTokens: 40000}
				continued = &runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: 40000}
				k.claimed(t)
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
// continues it against the most room the seat's and the company's capped
// windows can leave the launch, each ceiling less what its own turn spent
// there: past it, no window turning over ever leaves room, so the session is
// not continued: the run starts one of its own on its fresh brief's floor,
// told so before it starts, where a session within it is continued whatever
// its size, and a seat with no capped window continues any: no budget is
// protected by refusing it.
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

// boxSays is what the run's box says to its bridge as its harness first reaches
// it: an MCP handshake on the endpoint its script wired its harness with, as
// the harness's own client opens one.
func (k keptLaunch) boxSays(t *testing.T, endpoint string) {
	t.Helper()
	at, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("the endpoint %q is no URL: %v", endpoint, err)
	}
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", k.e.bridge.Handler())
	req := httptest.NewRequest(http.MethodPost, at.Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,`+
		`"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},`+
		`"clientInfo":{"name":"harness","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	mux.ServeHTTP(httptest.NewRecorder(), req)
}

// A RELAUNCH'S BOX SAYS WHICH SESSION IT STARTED, AND THE RUN IS HELD TO IT. A
// relaunch that continues the turn's kept session hands its run the seat's tools
// twice — under the bridge token of that session, and under the job's other
// token, for a session its script starts of its own where the restore fails in
// the box. The token its harness first dials is the box's word: the runner is
// told which session the run is in before any call, and the job's record keeps
// it — the fall back where the box started its own — for every later rebuild of
// the run's guard, a relaunch carrying the run on included.
//
// Mutation: wire the run once, tell the runner nothing on the box's word, or
// record the session the launch chose whatever the box said, and a case goes
// red.
func TestARelaunchsBoxSaysWhichSessionItStartedAndTheRunIsHeldToIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fellBack bool
		want     sandbox.SessionStart
	}{
		"its box took the session up":          {want: sandbox.SessionStartContinued},
		"its box started a session of its own": {fellBack: true, want: sandbox.SessionStartFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newKeptLaunch(t, 40000, 0, 0)
			req := launchRequest(k.l, nil)
			req.Brief = "the fresh brief"
			req.Session = &runner.SessionContinuation{Basis: k.l.ProbeSession(k.ctx),
				Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours"}
			var mu sync.Mutex
			var said []bool
			req.Started = func(continued bool) {
				mu.Lock()
				defer mu.Unlock()
				said = append(said, continued)
			}
			if _, err := k.l.LaunchExecutor(k.ctx, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			started := k.jobs.Started()
			last := started[len(started)-1]
			if last.Resume != "s-1" || last.FreshWiring == nil {
				t.Fatalf("the relaunch continues %q wired for a session of its own %+v, want the kept "+
					"session wired both ways", last.Resume, last.FreshWiring)
			}
			chosen := last.MCPServers[config.ToolServerName].URL
			other := last.FreshWiring.MCPServers[config.ToolServerName].URL
			if chosen == "" || other == "" || chosen == other {
				t.Fatalf("the run's bridge is wired at %q and %q, want two tokens of the job", chosen, other)
			}
			dialled := chosen
			if tc.fellBack {
				dialled = other
			}
			k.boxSays(t, dialled)
			mu.Lock()
			told := append([]bool(nil), said...)
			mu.Unlock()
			if len(told) != 1 || told[0] == tc.fellBack {
				t.Fatalf("the runner was told %v, want once that the box continued: %v", told, !tc.fellBack)
			}
			pending := k.e.sandbox.Load().pending
			deadline := time.Now().Add(10 * time.Second)
			for {
				row, _, err := pending.Get(k.ctx, "t1")
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if row.Launch.SessionStart == tc.want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the job's record says %q, want %q", row.Launch.SessionStart, tc.want)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// A RESUME READS WHICH SESSION THE JOB'S BOX SAID IT STARTED, off the job's
// record: the runner's every rebuild of the run's guard — a collection, a
// relaunch carrying the run on, a send-back continuing its session — holds the
// skills the job's launch armed only where the box took up the session the
// launch chose, and a record that says the box started its own, or says
// nothing, holds none of them.
//
// Mutation: hand the runner the session the launch chose whatever the record
// says, and the fall back's row goes red.
func TestAResumeReadsWhichSessionTheJobsBoxStarted(t *testing.T) {
	t.Parallel()
	for start, want := range map[sandbox.SessionStart]bool{
		sandbox.SessionStartContinued: true,
		sandbox.SessionStartFresh:     false,
		"":                            false,
	} {
		in := resumeInput{Run: sandbox.PendingRun{Launch: sandbox.LaunchRecord{SessionStart: start}}}
		if got := runnerResume(in, nil, nil, nil).Continued; got != want {
			t.Errorf("a job whose box said %q resumes continued %v, want %v", start, got, want)
		}
	}
}

// A RESUME READS HOW THE JOB'S HARNESS ENDED, off the job's record, as the record
// says it: a relaunch that carries the round on in the job's box tells the
// session there that it was cut off, or may have been, every call of the round
// and every note the turn has read unless the record says its harness ended its
// own run, so only that word says so — a cut-off end, a job an older build
// collected, which recorded nothing, and a word this build does not know all say
// it may lack them — and the word itself reaches the relaunch, which tells a
// copy recorded cut off that it was and any other that it may have been.
//
// Mutation: read anything but a cut-off end as an own run, and the rows of the
// older build's collection and the unknown word go red; hand the relaunch a
// word other than the record's own, and a row goes red.
func TestAResumeReadsHowTheJobsHarnessEnded(t *testing.T) {
	t.Parallel()
	for end, ownRun := range map[sandbox.HarnessEnd]bool{
		sandbox.HarnessEndOwnRun: true,
		sandbox.HarnessEndCutOff: false,
		"":                       false,
		"saved":                  false,
	} {
		in := resumeInput{Run: sandbox.PendingRun{Launch: sandbox.LaunchRecord{HarnessEnd: end}}}
		got := runnerResume(in, nil, nil, nil).HarnessEnd
		if got != end {
			t.Errorf("a job whose record says its harness ended %q resumes with %q", end, got)
		}
		if got.EndedOwnRun() != ownRun {
			t.Errorf("a job whose record says its harness ended %q resumes as ending its own run %v, want %v",
				end, got.EndedOwnRun(), ownRun)
		}
	}
}

// A RELAUNCH IS TARGETED AT THE MEMBER IT WOULD BE LAUNCHED ON, as the node
// holding the seat picks it ([Engine.RelaunchTargets], which a node with no
// broker, the whole fleet, answers from its own pick): the seat's first member
// with an in-box runner that is not sitting out its plan there, the cell its
// run lands in, that node, and its runner — never the runner of the harness
// that wrote the session, which a seat whose plan member is spent is not
// relaunched on. A chain every member of which sits out has no member yet, and
// says it cannot be said rather than that none restores the session; a handle
// the company holds no seat under has none.
//
// Mutation: answer the catalogue's runner for a harness, skip the sit-out,
// resolve the cell otherwise than the launch does, leave the holder off the
// target, or read a chain sitting out as one with no runner, and a case goes
// red.
func TestARelaunchIsTargetedAtTheMemberItWouldBeLaunchedOn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		members   []chainMember
		harness   string
		cell      sandbox.Placement
		undecided bool
	}{
		"the chain's head, in the catalogue's default cell": {harness: "claude-code", cell: sandbox.Direct,
			members: []chainMember{{key: "api", agent: "claude-code", model: "claude-golden"}}},
		"the next member past a spent plan, in its own cell": {harness: "opencode", cell: sandbox.Container,
			members: []chainMember{
				{key: "plan", agent: "claude-code", model: "opus", spent: true},
				{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5", runIn: config.Placement(sandbox.Container)},
			}},
		"every member sitting out": {undecided: true,
			members: []chainMember{{key: "plan", agent: "claude-code", model: "opus", spent: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, tc.members...)
			e, runners := launchRigRestoring(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}), &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}})
			e.id = "node-a"
			id, ok := c.Org.AgentIDFor(seat)
			if !ok {
				t.Fatal("the seat has no agent id")
			}
			targets := e.RelaunchTargets(t.Context(), id, seat.Handle(), []sandbox.RelaunchAsk{{Turn: "t1"}})
			if len(targets) != 1 {
				t.Fatalf("one turn asked about was answered %d targets", len(targets))
			}
			got := targets[0]
			if tc.undecided {
				if !got.Undecided || got.Restorer != nil || !strings.Contains(got.Why, "sitting out its plan") {
					t.Fatalf("a chain sitting out is targeted %+v, want it undecided, saying so", got)
				}
				return
			}
			want, ok := sandbox.Runner(runners[tc.harness]).(sandbox.ArchiveRestorer)
			if !ok {
				t.Fatalf("the %s runner restores no carried session, so this case proves nothing", tc.harness)
			}
			if got.Undecided || got.Harness != tc.harness || got.Placement != tc.cell || got.Node != "node-a" ||
				got.Restorer != want || got.Why != "" {
				t.Fatalf("a relaunch is targeted %+v, want the %s member's runner in the %s cell on node-a",
					got, tc.harness, tc.cell)
			}
		})
	}
	c, _ := executorCompany(t)
	e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	if got := e.localRelaunchTarget("nobody", "t1"); got.Restorer != nil || got.Undecided ||
		!strings.Contains(got.Why, "no seat") {
		t.Errorf("a handle no seat holds is targeted %+v, want no member, said", got)
	}
}

// A RESUMED TURN'S RELAUNCH IS TARGETED FROM THE REVISION IT RUNS ON. A resumed
// segment pins the company revision it was admitted under, and a reviewer's
// send-back from it is launched on that revision's chain whatever an apply put
// live since; so the node running it answers what that turn's relaunch is
// launched on from the segment's revision, while a turn with no segment running
// here — whose next one is admitted under the revision live when it comes — is
// answered from the live one. Two turns of the seat, one page, two answers. And
// a conversation's next turn, asked about by no turn, is answered from the live
// revision whatever segment is pinned: its first launch is admitted under that
// one.
//
// Mutation: answer every turn from the revision the node runs, or an ask of no
// turn from a pinned segment's, and a row is decided for a member its launch is
// never launched on.
func TestAResumedTurnsRelaunchIsTargetedFromTheRevisionItRunsOn(t *testing.T) {
	t.Parallel()
	pinned, seat := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e, _ := launchRigRestoring(t, pinned, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}), &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}})
	// THE TURN'S SEGMENT IS RUNNING ON THIS NODE, pinned to the revision it
	// was admitted under, as [Engine.resumeTurn] registers it.
	e.resumedSegments.Store("t-open", pinned)
	// AN APPLY SINCE: the seat's llm is now an OpenCode entry in the
	// container cell, under the same catalogue.
	applied, _ := chainCompany(t, chainMember{key: "oc", agent: "opencode",
		model: "anthropic/claude-sonnet-5", runIn: config.Placement(sandbox.Container)})
	applied.catalogue = pinned.catalogue
	e.epoch.current.Store(applied)
	id, ok := pinned.Org.AgentIDFor(seat)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	got := e.RelaunchTargets(t.Context(), id, seat.Handle(), []sandbox.RelaunchAsk{
		{Turn: "t-open"}, {Turn: "t-parked"}, {},
	})
	if len(got) != 3 {
		t.Fatalf("three sessions asked about were answered %d targets", len(got))
	}
	if open := got[0]; open.Undecided || open.Harness != "claude-code" || open.Placement != sandbox.Direct {
		t.Errorf("the open segment's turn is targeted %+v, want the claude-code member of the revision it "+
			"was admitted under, in the direct cell", open)
	}
	if parked := got[1]; parked.Undecided || parked.Harness != "opencode" || parked.Placement != sandbox.Container {
		t.Errorf("a turn with no segment open is targeted %+v, want the opencode member of the revision "+
			"live now, in the container cell", parked)
	}
	if next := got[2]; next.Undecided || next.Harness != "opencode" || next.Placement != sandbox.Container {
		t.Errorf("a conversation's next turn, asked by no turn, is targeted %+v, want the opencode member "+
			"of the revision live now, whatever segment is pinned", next)
	}
	// AND ONCE THE SEGMENT ENDS, the turn's next one is admitted under the
	// live revision, which answers it.
	e.resumedSegments.Delete("t-open")
	if after := e.RelaunchTargets(t.Context(), id, seat.Handle(),
		[]sandbox.RelaunchAsk{{Turn: "t-open"}}); after[0].Harness != "opencode" {
		t.Errorf("a turn whose segment ended is targeted %+v, want the live revision's member", after[0])
	}
}

// A RELAUNCH TARGET SAYS WHETHER THE COMPANY KEEPS SESSIONS, as the node
// holding the seat runs it: where `turn_engine.conversation_session.enabled` is
// false, or the node has no object store to keep one in, a relaunch continues
// no kept session whatever the turn kept — as the coordinator's own check says
// at the launch — and a company that keeps them says nothing.
//
// Mutation: drop the keep from the target, or set it whatever the switch says,
// and a case goes red.
func TestARelaunchTargetSaysWhetherTheCompanyKeepsSessions(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects, off bool
		noKept       bool
	}{
		"a company that keeps sessions":   {objects: true},
		"a company that keeps none":       {objects: true, off: true, noKept: true},
		"a node with nowhere to keep one": {noKept: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := executorCompany(t)
			if tc.off {
				c.Config.TurnEngine.ConversationSession.Enabled = org.Off()
			}
			e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}))
			if tc.objects {
				store, err := objstore.NewStore(memobj.New())
				if err != nil {
					t.Fatal(err)
				}
				e.objects = &objectStore{store: store}
			}
			got := e.localRelaunchTarget(seat.Handle(), "t1")
			if (got.NoKept == sandbox.NoKeptSession) != tc.noKept || (!tc.noKept && got.NoKept != "") {
				t.Fatalf("the target says %q of keeping sessions, want kept none %v", got.NoKept, tc.noKept)
			}
		})
	}
}

// A RELAUNCH'S PROBE ASKS EVERY MEMBER OF THE CHAIN, never one picked: the
// launch picks its member once, as it launches, and a member's plan can be
// spent or come back between the probe and that pick, so the briefs that
// continue a session are written wherever some member restores it — a Claude
// Code session for a chain whose Claude Code plan is spent now — and the
// reason where none drives the harness that wrote it says so.
//
// Mutation: ask only the member a launch would pick now, and the spent plan's
// session reads as one no member continues.
func TestARelaunchsProbeAsksEveryMemberOfTheChain(t *testing.T) {
	t.Parallel()
	claude := sandbox.SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: sandbox.FakeArchiveFormat}
	for name, tc := range map[string]struct {
		members  []chainMember
		restores bool
		why      string
	}{
		"a spent plan of the session's harness behind another": {restores: true,
			members: []chainMember{
				{key: "plan", agent: "claude-code", model: "opus", spent: true},
				{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"},
			}},
		"no member of the session's harness": {why: "no member of the seat's executor chain runs claude-code",
			members: []chainMember{{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, tc.members...)
			_, runners := launchRigRestoring(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}), &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}})
			for _, r := range runners {
				r.Carries = true
			}
			restores, err := c.chainRestores(seat, c.catalogue)
			if err != nil {
				t.Fatalf("chainRestores: %v", err)
			}
			ok, why := restores.Restores(claude)
			if ok != tc.restores || !strings.Contains(why, tc.why) {
				t.Fatalf("the chain restores the session %v (%q), want %v saying %q", ok, why, tc.restores, tc.why)
			}
		})
	}
}

// A LAUNCH HOLDS ITS SESSION'S FLOOR TO THE MEMBER IT PICKS. The probe wrote the
// briefs continuing a session some member of the chain restores; the launch
// picks its member once, as it launches, and where that member runs another
// harness than the one that wrote the session — its plan spent, or another's
// back, since the probe — it continues none: the launch starts the run fresh,
// held to its fresh brief's floor rather than to a session it will not send,
// and refused only where that floor does not fit. So with the job before's own
// session in its box, which a member whose run lands in another cell cannot
// continue there.
//
// Mutation: reckon the floor before the pick, for the session the probe
// answered or for the box's whatever the member, and the launch is refused on
// a floor its run never needed.
func TestALaunchHoldsItsSessionsFloorToTheMemberItPicks(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 100000, 70000)
	basis := k.l.ProbeSession(k.ctx)
	if !basis.Continues() || basis.Harness != "claude-code" {
		t.Fatalf("the probe answered %+v, want the kept Claude Code session", basis)
	}
	basis.Harness = "opencode"
	decided, err := k.relaunch(t, basis, nil)
	if err != nil {
		t.Fatalf("a launch whose member runs another harness than the session's: %v, want it launched on "+
			"its fresh brief's floor", err)
	}
	if len(decided) != 1 || decided[0].Continues {
		t.Fatalf("the runner was told %+v, want once: a session of its own", decided)
	}
	started := k.jobs.Started()
	if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" {
		t.Fatalf("the relaunch started with session %q and brief %q, want a session of its own on the "+
			"fresh brief", last.Resume, last.Brief)
	}

	// AND THE JOB'S OWN SESSION IN ITS BOX, which a member whose run lands in
	// another cell cannot continue there: a box lives in one cell's backend.
	inBox := newKeptLaunch(t, 40000, 100000, 70000)
	inBox.claimed(t)
	member := inBox.l.company.Config.Providers.LLM["api"]
	member.RunIn = config.Placement(sandbox.Container)
	inBox.l.company.Config.Providers.LLM["api"] = member
	decided, err = inBox.relaunch(t, runner.SessionBasis{Why: "the newest capture was not kept",
		ContextTokens: 40000}, &runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: 40000})
	if err != nil {
		t.Fatalf("a launch whose member cannot continue in the job's box: %v, want it launched on its fresh "+
			"brief's floor", err)
	}
	if len(decided) != 1 || decided[0].Continues {
		t.Fatalf("the runner was told %+v, want once: a session of its own", decided)
	}
}

// A BOX ON ANOTHER NODE'S HOST ADDS NOTHING TO A LAUNCH'S FLOOR. The job
// before's own session in its box counts toward a relaunch's floor only where
// the launch can continue in that box, and a box in a cell on an engine host is
// continued in by the node that launched it and no other: a seat that moved
// since leaves the box to that node, and its relaunch here starts a session of
// its own in a fresh box. So the relaunch is held to its fresh brief's floor,
// and is launched, fresh, where the box's session would not have fit.
//
// Mutation: drop the host test from the floor's reckoning of the job's box, and
// the relaunch is refused below a floor its run never needs.
func TestABoxOnAnotherNodesHostAddsNothingToALaunchsFloor(t *testing.T) {
	t.Parallel()
	// A DAY OF 100000 WITH 70000 SPENT: room for a fresh launch (the static
	// floor, 25000, beside its short brief), and none for the box's session,
	// which sends 40000 first.
	k := newKeptLaunch(t, 40000, 100000, 70000)
	k.claimed(t)
	// THE SEAT MOVED: this node is node-b, whose coordinator launches here,
	// and the job's direct box is on the host of the node that launched it.
	rt := k.e.sandbox.Load()
	moved, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Audience: noAudience{}, Queue: k.e.backends.Queue, Pending: rt.pending,
		Manager: k.l.company.catalogue, Resume: &resumer{engine: k.e}, Node: "node-b",
		Pushes: noPushes{},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(moved.Stop)
	k.e.useSandbox(rt.pending, moved)
	k.e.id = "node-b"
	decided, err := k.relaunch(t, runner.SessionBasis{Why: "the newest capture was not kept", ContextTokens: 40000},
		&runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: 40000})
	if err != nil {
		t.Fatalf("a relaunch on another node than its job's box's: %v, want it launched on its fresh brief's "+
			"floor", err)
	}
	if len(decided) != 1 || decided[0].Continues {
		t.Fatalf("the runner was told %+v, want once: a session of its own", decided)
	}
	started := k.jobs.Started()
	if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" {
		t.Fatalf("the relaunch started with session %q and brief %q, want a session of its own on the "+
			"fresh brief", last.Resume, last.Brief)
	}
}

// unreadRuns is the run store a launch reads its turn's record from, whose read
// of a record fails, as a coordination store having a bad moment answers it.
type unreadRuns struct{ sandbox.PendingStore }

func (unreadRuns) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, errors.New("the coordination store did not answer")
}

// A JOB'S BOX WHOSE RECORD CANNOT BE READ HOLDS A LAUNCH TO ITS SESSION. Whether
// a relaunch can continue in the job before's box is read off the run's record,
// and where that read fails nothing says it cannot: the box's session counts
// toward the floor, the safe direction for a floor — a launch held back on room
// it may not need, never one started that its budget cannot carry.
//
// Mutation: read an unreadable record as a box the launch cannot continue in,
// and the launch goes out on its fresh brief's floor.
func TestAJobsBoxWhoseRecordCannotBeReadHoldsTheLaunchToItsSession(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 100000, 70000)
	k.claimed(t)
	rt := k.e.sandbox.Load()
	k.e.useSandbox(unreadRuns{PendingStore: rt.pending}, rt.coordinator)
	_, err := k.relaunch(t, runner.SessionBasis{Why: "the newest capture was not kept", ContextTokens: 40000},
		&runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: 40000})
	var refused *floorRefusal
	if !errors.Is(err, toolloop.ErrBudgetExhausted) || !errors.As(err, &refused) ||
		refused.floor.context != 40000 {
		t.Fatalf("a relaunch whose job's record cannot be read = %v, want a budget refusal on the box "+
			"session's floor", err)
	}
}

// A RUN TAKEN OVER IS HELD TO WHAT ITS BOX SAYS, AS ITS RECORD NAMES THE BRIEF.
// A node that took a run over before its box first reached the bridge hears
// the box's word on its first request there, and has only the job's record to
// tell whether the launch on another node continued a session at all: a job
// whose record names a continuing brief is held to the session its box says
// it started — the one its launch chose, or one of its own, written on the
// job's record — and one whose launch continued none is held to its own, and
// nothing is recorded, as its launch's node would have done.
//
// Mutation: read the record's brief the other way round, or arm the reopened
// run whatever its box said, and a case goes red.
func TestARunTakenOverIsHeldToWhatItsBoxSaysAsItsRecordNamesTheBrief(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		brief    sandbox.BriefKind
		fellBack bool
		armed    bool
		want     sandbox.SessionStart
	}{
		"a continuing launch whose box took the session up": {brief: sandbox.BriefContinuedNewBox,
			armed: true, want: sandbox.SessionStartContinued},
		"a continuing launch whose box started its own": {brief: sandbox.BriefContinuedNewBox, fellBack: true,
			want: sandbox.SessionStartFresh},
		"a launch that continued no session": {brief: sandbox.BriefFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := sandbox.NewCoordStore(coordmemory.NewFleet())
			run, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"},
				sandbox.Fence{})
			if err != nil {
				t.Fatalf("BeginLaunch: %v", err)
			}
			run.Launch.Brief = tc.brief
			ledger := newBridgeLedger(store, nil)
			ledger.bind(run.NamedJob(run.OpenedAfter, ""))
			var armed []bool
			reopenedStarted(ledger, run, func(continued bool) { armed = append(armed, continued) })(ctx,
				tc.fellBack)
			if len(armed) != 1 || armed[0] != tc.armed {
				t.Fatalf("the reopened run was held to %v, want once to a continued session: %v", armed, tc.armed)
			}
			// A JOB WHOSE LAUNCH CONTINUED NONE has nothing written, and nothing
			// sent to write it: its record is read once.
			deadline := time.Now().Add(10 * time.Second)
			for {
				row, _, err := store.Get(ctx, "t1")
				switch {
				case err != nil:
					t.Fatalf("Get: %v", err)
				case row.Launch.SessionStart == tc.want:
					return
				case tc.want == "" || time.Now().After(deadline):
					t.Fatalf("the job's record says %q, want %q", row.Launch.SessionStart, tc.want)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
