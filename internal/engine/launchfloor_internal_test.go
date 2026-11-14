package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CONTINUED LAUNCH IS HELD ON ITS OWN FLOOR, NEVER THE STATIC ONE. A relaunch
// that continues its turn's session sends the session's context before its
// brief, so its floor is the two together; refused below it, the refusal
// carries that floor to the hold, which waits for a window with room for THIS
// launch and says what it is made of. Held to the static floor instead, a
// window with room for that alone released the resume into the same refusal
// at its launch, round after round.
//
// Mutation: hold on the static floor, or drop the floor from the refusal, and
// nothing holds the resume.
func TestARefusedContinuedLaunchIsHeldOnItsOwnFloor(t *testing.T) {
	t.Parallel()
	r := newFlooredParkRig(t, "100", "50")
	r.spend(t, 40)
	floor := staticFloor(r.e.Company()).continuing(55, 10)
	refused := executorHeadroom(t.Context(), staticRemaining{left: 60}, floor)
	if !errors.Is(refused, toolloop.ErrBudgetExhausted) || !strings.Contains(refused.Error(), "continues its session") {
		t.Fatalf("a continued launch with 60 tokens left under a floor of 65 = %v, want a budget refusal "+
			"saying what its floor is made of", refused)
	}
	held := r.e.heldByBudget(t.Context(), r.e.Company(), "lead", refused)
	if held == nil || held.Refusal.Condition != waitBudget || !held.Refusal.Until.Equal(berlinMidnight) ||
		!strings.Contains(held.Refusal.Reason, "continues its session") {
		t.Fatalf("held = %+v, want the resume held on the day that cannot carry its continued launch", held)
	}
	if refusal, held := r.e.budgetHold(t.Context(), r.e.Company(), "lead", nil); held {
		t.Fatalf("the static floor holds %+v, where the window has room for it: the case is not the one "+
			"a continued launch's own floor decides", refusal)
	}
}

// A SESSION NO WINDOW CAN CARRY IS NOT CONTINUED. A floor past the most room the
// seat's and the company's capped windows can leave the launch, each ceiling
// less what the launch's own turn charged there first, is one no window turning
// over ever meets, so its relaunch would wait for ever: such a session starts
// fresh, on its fresh brief's floor, which is held where it is past that room.
// And the floor is the larger of the static floor and the context with the
// brief, never their sum: the static floor counts the harness's own prompt,
// which the context's last request counted too.
//
// Mutation: add the static floor to the context, compare against the largest
// ceiling, or against a ceiling with the turn's own spend left in it, and a row
// goes red.
func TestASessionNoWindowCanCarryIsNotContinued(t *testing.T) {
	t.Parallel()
	capped := budgetBasis{org: coord.Caps{period.Day: 500}, seat: coord.Caps{period.Day: 120, period.Week: 900}}
	static := launchFloor{static: 50}
	// TWENTY SPENT BY THE TURN, in the day, the week and the month alike.
	turnSpent := map[period.Period]int{period.Day: 20, period.Week: 20, period.Month: 20}
	for name, tc := range map[string]struct {
		floor  launchFloor
		basis  budgetBasis
		spent  map[period.Period]int
		tokens int
		fits   bool
		// names is the figure the line names a floor past, and what it
		// says that figure is made of.
		names []string
	}{
		"within the smallest ceiling": {floor: static.continuing(90, 15), basis: capped, tokens: 105, fits: true},
		"past the smallest ceiling": {floor: static.continuing(110, 15), basis: capped, tokens: 125,
			names: []string{"past 120, the smallest ceiling"}},
		"past what the turn's own spend leaves of it": {floor: static.continuing(90, 15), basis: capped,
			spent: turnSpent, tokens: 105,
			names: []string{"past 100, the most room", "a ceiling of 120, less the 20 its own turn spent"}},
		"under the static floor": {floor: static.continuing(30, 5), basis: capped, tokens: 50, fits: true},
		"no session":             {floor: static, basis: capped, tokens: 50, fits: true},
		"nothing capped":         {floor: static.continuing(110, 15), tokens: 125, fits: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.floor.tokens(); got != tc.tokens {
				t.Errorf("the floor is %d tokens, want %d", got, tc.tokens)
			}
			why := tc.floor.fits(tc.basis.room(tc.spent))
			if (why == "") != tc.fits {
				t.Fatalf("fits = %q, want fitting %v", why, tc.fits)
			}
			for _, says := range tc.names {
				if !strings.Contains(why, says) {
					t.Errorf("fits = %q, want it to say %q", why, says)
				}
			}
		})
	}
}

// A SESSION THE LAUNCH'S MODEL CANNOT CARRY IS NOT CONTINUED. The launch reads the
// window its run is pinned to ([sandbox.ClaudeCodeWindow]), so a session kept
// under a long window, continued on a member whose model has a standard one,
// starts a session of its own rather than one whose first request cannot be sent.
//
// Mutation: read a fixed window, or skip the check, and a row goes red.
func TestASessionTheLaunchsModelCannotCarryIsNotContinued(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	for name, c := range map[string]struct {
		model     string
		context   int64
		continues bool
	}{
		"a long window carries it":            {model: "claude-opus-5-5", context: 600_000, continues: true},
		"a standard window cannot":            {model: "claude-haiku-4-5", context: 600_000},
		"a standard window carries a smaller": {model: "claude-haiku-4-5", context: 150_000, continues: true},
		"an unknown model is held to the fallback": {model: "claude-next-preview",
			context: sandbox.FallbackExecutorWindow},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: c.model})
			req := runner.AgentRunRequest{Session: &runner.SessionContinuation{Brief: "carry on",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: executor.ClaudeCodeName, ContextTokens: c.context}}}
			got, floor := l.continuedFloor(t.Context(), req, member, true)
			if (got.Session != nil) != c.continues {
				t.Fatalf("the session continues: %v, want %v", got.Session != nil, c.continues)
			}
			if want := sandbox.ClaudeCodeWindow(c.model).Carries(floor.context, floor.brief); !want && c.continues {
				t.Errorf("the floor kept (%d, %d), which the run's own window cannot carry", floor.context, floor.brief)
			}
		})
	}
}

// EACH SESSION A LAUNCH MAY CONTINUE IS HELD TO ITS OWN FLOOR. A relaunch may be
// offered the turn's kept session and the job before's own copy in its box, each
// with a brief of its own, and its box decides which it takes up as the run
// starts — the kept one where it can restore it, the copy only where it cannot.
// So a session whose own floor one request of the launch's model cannot carry is
// dropped alone, the other still offered; the launch is held to the larger floor
// of the sessions it still offers, since its box may take up either; and only
// where neither is left does its run start a session of its own, on its fresh
// brief's floor. Held both to the larger floor and dropped together, a kept
// session whose short brief fits was dropped for the copy's longer one, which
// the launch sends only where its box cannot restore the kept session.
//
// Mutation: drop both sessions where either cannot be carried, or hold the
// launch to the floor of a session it dropped, and a row goes red.
func TestEachSessionALaunchMayContinueIsHeldToItsOwnFloor(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	const model = "claude-haiku-4-5"
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: model})
	// A CONTEXT ONE REQUEST UNDER THE MODEL'S WINDOW CARRIES with a short brief,
	// and not with a long one.
	const contextTokens = 150_000
	short, long := "carry on", strings.Repeat("a call the copy may lack. ", 40_000)
	window := sandbox.ClaudeCodeWindow(model)
	carriesShort := window.Carries(contextTokens, harness.EstimateTokens(short))
	carriesLong := window.Carries(contextTokens, harness.EstimateTokens(long))
	if !carriesShort || carriesLong {
		t.Fatalf("the %d-token window carries the short brief %v and the long one %v: the case is not the one "+
			"it means", window.Tokens, carriesShort, carriesLong)
	}
	for name, tc := range map[string]struct {
		kept, inPlace string
		// keeps and copies say which sessions the launch still offers; floor is
		// the brief of the one whose floor it is held to, "" for the static one.
		keeps, copies bool
		floor         string
	}{
		"both carried":                       {kept: short, inPlace: short, keeps: true, copies: true, floor: short},
		"the copy's longer brief is not":     {kept: short, inPlace: long, keeps: true, floor: short},
		"the kept one's longer brief is not": {kept: long, inPlace: short, copies: true, floor: short},
		"neither is carried":                 {kept: long, inPlace: long},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := runner.AgentRunRequest{
				Session: &runner.SessionContinuation{Brief: tc.kept, Replaced: tc.kept,
					Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
						Harness: executor.ClaudeCodeName, ContextTokens: contextTokens}},
				Continue: &runner.Continuation{SessionID: "s-1", Brief: tc.inPlace, ContextTokens: contextTokens},
			}
			got, floor := l.continuedFloor(t.Context(), req, member, true)
			if keeps := got.Session != nil; keeps != tc.keeps {
				t.Errorf("the launch offers the kept session: %v, want %v", keeps, tc.keeps)
			}
			if copies := got.Continue != nil && got.Continue.SessionID != ""; copies != tc.copies {
				t.Errorf("the launch offers the copy in its box: %v, want %v", copies, tc.copies)
			}
			want := staticFloor(nil)
			if tc.floor != "" {
				want = want.continuing(contextTokens, harness.EstimateTokens(tc.floor))
			}
			if floor != want {
				t.Errorf("the launch is held to %+v, want %+v", floor, want)
			}
		})
	}
}

// longBrief is a brief of about ten thousand tokens, as a fresh run carrying a
// round's calls on is handed, or a continuation telling a cut-off session what
// it may lack.
var longBrief = strings.Repeat("a call of the round, told again. ", 1250)

// floorRig is a seat whose day is capped at ceiling with spent already counted
// in it, under an executor floor of static (the default where nil), on an
// engine that takes a launch all the way to a box: the engine, the turn's
// launcher, and the runner its jobs start on.
func floorRig(t *testing.T, ceiling, spent int, static *int) (*Engine, *agentLauncher, *sandbox.FakeRunner) {
	t.Helper()
	return cappedFloorRig(t, org.TokenCeilings{period.Day: ceiling}, nil, spent, static)
}

// cappedFloorRig is [floorRig] under seatCaps, the seat's ceilings, and
// companyCaps, the company's, either nil for none.
func cappedFloorRig(t *testing.T, seatCaps, companyCaps org.TokenCeilings, spent int,
	static *int) (*Engine, *agentLauncher, *sandbox.FakeRunner) {
	t.Helper()
	c, seat := executorCompany(t)
	seat.TokenBudget = seatCaps
	c.Org.TokenBudget = companyCaps
	c.Config.TurnEngine.ExecutorMinBudgetTokens = static
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	fleet := coordmemory.NewFleet()
	e.backends.Fleet = fleet
	if spent > 0 {
		id, ok := c.Org.AgentIDFor(seat)
		if !ok {
			t.Fatal("the seat has no agent id")
		}
		if _, err := fleet.PostCharge(t.Context(), coord.AgentScope(id.String()), spent,
			coord.WindowsAt(time.Now(), c.Config.Location())); err != nil {
			t.Fatalf("PostCharge: %v", err)
		}
	}
	return e, launcherOn(e, c, seat, "t1"), jobs
}

// busyFloorRig is [floorRig] in a busy company: the company's own day is
// capped at ceiling, the smallest ceiling of either scope, and the seat's at
// twice it, so the window that leaves the launch the least is the one every
// seat's spend reaches ([othersSpent]).
func busyFloorRig(t *testing.T, ceiling int) (*Engine, *agentLauncher, *sandbox.FakeRunner) {
	t.Helper()
	return cappedFloorRig(t, org.TokenCeilings{period.Day: 2 * ceiling},
		org.TokenCeilings{period.Day: ceiling}, 0, nil)
}

// othersSpent puts tokens on the company's counter as another seat's spend, in
// the windows of now: spend in the company's window that is neither the
// launching seat's nor its turn's.
func othersSpent(t *testing.T, e *Engine, tokens int) {
	t.Helper()
	if tokens <= 0 {
		return
	}
	if _, err := e.backends.Fleet.PostCharge(t.Context(), coord.AgentScope(seatOf("colleague").String()), tokens,
		coord.WindowsAt(time.Now(), e.Company().Config.Location())); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
}

// A FRESH LAUNCH IS HELD TO ITS BRIEF. The static floor
// (turn_engine.executor_min_budget_tokens) is a run's first request before its
// brief: the harness's own prompt, its tools and the seat's system prompt. A
// launch that continues no session sends its whole brief in that request, so it
// needs the static floor with the brief beside it, and its refusal and the hold
// that waits on it say the brief's share. Held to the static floor alone, a
// fresh run carrying a round's calls on passed its floor in a window that could
// not carry its first request. And a floor exactly at the most room a window
// can leave the launch is not past that room, so it is not held: a window with
// all of it left admits the launch, and one a token short refuses it, where a
// launch held at the static floor would go out.
//
// Mutation: count no brief beside the static floor where no context is known,
// leave the brief out of the refusal or of the hold's reason, or hold a floor
// at the room as one past it, and a case goes red.
func TestAFreshLaunchIsHeldToItsBrief(t *testing.T) {
	t.Parallel()
	brief := harness.EstimateTokens(longBrief)
	need := new(config.TurnEngine).ExecutorFloor() + brief
	for name, tc := range map[string]struct {
		// ceiling is the seat's day, the one capped window: the most room it
		// can leave a launch whose turn charged nothing in it.
		ceiling, left int
		launches      bool
	}{
		"a token short of the static floor with its brief": {ceiling: 100_000, left: need - 1},
		"the static floor with its brief":                  {ceiling: 100_000, left: need, launches: true},
		"a token short of it, at the room":                 {ceiling: need, left: need - 1},
		"all of it, at the room":                           {ceiling: need, left: need, launches: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, l, jobs := floorRig(t, tc.ceiling, tc.ceiling-tc.left, nil)
			req := launchRequest(l, nil)
			req.Brief = longBrief
			_, err := l.LaunchExecutor(t.Context(), req)
			if tc.launches {
				if err != nil || len(jobs.Started()) != 1 {
					t.Fatalf("a fresh launch with %d tokens left under a floor of %d = %v, %d jobs started; "+
						"want it launched", tc.left, need, err, len(jobs.Started()))
				}
				return
			}
			var refused *floorRefusal
			if !errors.Is(err, toolloop.ErrBudgetExhausted) || !errors.As(err, &refused) ||
				refused.floor.tokens() != need || refused.floor.brief != brief {
				t.Fatalf("a fresh launch with %d tokens left = %v, want a budget refusal on the static floor "+
					"with its brief of about %d beside it, %d", tc.left, err, brief, need)
			}
			if share := fmt.Sprintf("its brief is about %d tokens", brief); !strings.Contains(err.Error(), share) {
				t.Errorf("the refusal says %q, want it to say %q", err, share)
			}
			held := e.heldByBudget(t.Context(), l.company, l.seat.Handle(), err)
			if share := fmt.Sprintf("its brief about %d beside", brief); held == nil ||
				!strings.Contains(held.Refusal.Reason, share) {
				t.Errorf("held = %+v, want the resume held on its window, the reason saying %q", held, share)
			}
			refusedCleanly(t, e, "t1", jobs)
		})
	}
}

// AN UNMEASURED CONTINUATION IS HELD TO ITS BRIEF. A session no capture measured
// (every copy in a company that keeps no session) has no known context, and a
// launch continuing it in its box sends its brief beside the harness's own
// prompt as surely as a fresh run does: its floor is the static floor with that
// brief beside it, the launch refused below it, and it is judged by the capped
// windows as a measured session's floor is: past the smallest ceiling the
// session is not continued, the launch starting its run on its fresh brief, and
// the line saying what that floor is made of, never a last request of nothing.
// Held to the static floor alone, a cut-off copy told every call of its round
// was continued in a window that could not carry its first request.
//
// Mutation: count no brief beside the static floor where no context is known,
// hold a session's floor as the fresh brief's is, or say an unmeasured
// session's last request carried nothing, and a case goes red.
func TestAnUnmeasuredContinuationIsHeldToItsBrief(t *testing.T) {
	t.Parallel()
	brief := harness.EstimateTokens(longBrief)
	static := new(config.TurnEngine).ExecutorFloor()
	need := static + brief
	notKept := runner.SessionBasis{Why: "the newest capture was not kept"}
	inPlace := func() *runner.Continuation { return &runner.Continuation{SessionID: "s-1", Brief: longBrief} }

	t.Run("refused below the static floor with its brief", func(t *testing.T) {
		t.Parallel()
		const ceiling = 100_000
		k := newKeptLaunch(t, 40000, ceiling, ceiling-(need-1))
		k.claimed(t)
		_, err := k.relaunch(t, notKept, inPlace())
		var refused *floorRefusal
		if !errors.Is(err, toolloop.ErrBudgetExhausted) || !errors.As(err, &refused) ||
			refused.floor.context != 0 || refused.floor.brief != brief || refused.floor.tokens() != need {
			t.Fatalf("a launch continuing an unmeasured copy with %d tokens left = %v, want a budget refusal "+
				"on the static floor with its brief of about %d beside it, %d", need-1, err, brief, need)
		}
	})
	t.Run("past the smallest ceiling, not continued", func(t *testing.T) {
		t.Parallel()
		// A DAY OF 30000: room for the static floor and the fresh brief beside
		// it, and none for the copy's long brief beside it.
		k := newKeptLaunch(t, 40000, 30000, 0)
		k.claimed(t)
		decided, err := k.relaunch(t, notKept, inPlace())
		if err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		started := k.jobs.Started()
		if last := started[len(started)-1]; len(decided) != 1 || decided[0].Continues || last.Resume != "" ||
			last.Brief != "the fresh brief" {
			t.Fatalf("the relaunch started with session %q and brief %q, deciding %+v; want a session of its "+
				"own on the fresh brief", last.Resume, last.Brief, decided)
		}
	})
	t.Run("the line says what the floor is made of", func(t *testing.T) {
		t.Parallel()
		why := launchFloor{static: static}.continuing(0, brief).fits(
			budgetBasis{seat: coord.Caps{period.Day: 30000}}.room(nil))
		if !strings.Contains(why, "no capture measured its context") || strings.Contains(why, "last request") ||
			!strings.Contains(why, fmt.Sprintf("about %d", brief)) {
			t.Errorf("an unmeasured session past the smallest ceiling is not continued because %q, want it "+
				"to say no capture measured its context and what its brief is", why)
		}
	})
}

// A LAUNCH IS HELD TO THE LARGEST FLOOR OF ITS BRIEFS. A launch offered a
// session is still handed its fresh brief, which its box starts a session of
// its own on wherever it takes none up, a restore that fails in the box
// included: so its floor is the largest of the floors of every brief it may
// hand its run, the fresh one among them. Held to the sessions' floors alone, a
// launch whose fresh brief was the longest went out below the floor of the run
// its box may start.
//
// Mutation: leave the fresh brief's floor out of the launch's, and a row goes
// red.
func TestALaunchIsHeldToTheLargestFloorOfItsBriefs(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	l := &agentLauncher{engine: &Engine{}, company: c, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	const kept, replaced = "carry on in the box you left", "carry on in a box that replaced yours"
	static := staticFloor(c)
	for name, tc := range map[string]struct {
		fresh   string
		context int64
		want    launchFloor
	}{
		"the fresh brief's is the largest": {fresh: longBrief, context: 1000,
			want: static.continuing(0, harness.EstimateTokens(longBrief))},
		"a session's is the largest": {fresh: "the fresh brief", context: 60000,
			want: static.continuing(60000, harness.EstimateTokens(replaced))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := runner.AgentRunRequest{Brief: tc.fresh,
				Session: &runner.SessionContinuation{Brief: kept, Replaced: replaced,
					Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
						Harness: executor.ClaudeCodeName, ContextTokens: tc.context}},
				Continue: &runner.Continuation{SessionID: "s-1", Brief: "carry on", ContextTokens: tc.context},
			}
			got, floor := l.continuedFloor(t.Context(), req, member, true)
			if got.Session == nil || got.Continue == nil || got.Continue.SessionID == "" {
				t.Fatalf("the launch dropped a session it can carry: kept %v, copy %+v", got.Session != nil,
					got.Continue)
			}
			if floor != tc.want {
				t.Errorf("the launch is held to %+v (%d tokens), want %+v (%d)", floor, floor.tokens(), tc.want,
					tc.want.tokens())
			}
		})
	}
}

// A FRESH FLOOR PAST THE ROOM IS HELD AT THE STATIC FLOOR, OR AT THE ROOM BELOW
// IT. Config holds every capped ceiling at or above the static floor alone,
// never above the static floor with a brief beside it, so a fresh brief can
// take a launch's floor past the most room a window can leave the launch: each
// ceiling less what the launch's own turn charged there first, the least of
// every capped window of either scope. And the fresh brief is the one brief a
// launch has nothing to drop for, so that floor is held. At the static floor
// where the static floor fits in the room: a window with that much left admits
// the launch whatever other seats spent there first, and one a token short of
// it refuses it. At the room where the launch's own turn left less than the
// static floor: a window holding nothing but that turn's spend admits it, and
// one a token past it refuses it. The refusal and the hold's reason say where
// it is held and what the room is made of, the brief's own estimate kept rather
// than cut to what the room leaves. Never held under a ceiling config refuses,
// below the static floor, nor at a room the turn's own spend used up, a window
// with no room for a single token. Held at the room where the static floor
// fits, a launch in a busy company waited for a window its other seats had
// spent nothing in, which they may never leave it.
//
// Mutation: refuse the floor past the room at every window, hold it at the room
// where the static floor fits, at the static floor where the room is below it,
// at the smallest ceiling, by cutting the brief to what the room leaves, at the
// window leaving the most room, at the seat's windows alone, with the turn's
// own spend left in the room, never below the static floor, under a ceiling
// below it or at a room used up, or leave the hold out of the refusal or of the
// hold's reason, and a case goes red.
func TestAFreshFloorPastTheRoomIsHeldAtTheStaticFloorOrTheRoomBelowIt(t *testing.T) {
	t.Parallel()
	brief := harness.EstimateTokens(longBrief)
	static := new(config.TurnEngine).ExecutorFloor()
	// A COMPANY DAY OF 30000, THE SMALLEST CEILING: at the static floor or
	// above it, as config holds every ceiling, and below the static floor with
	// the brief beside it.
	const ceiling = 30_000
	if static > ceiling || static+brief <= ceiling {
		t.Fatalf("the static floor %d with a brief of %d beside it is not past the %d ceiling, or the static "+
			"floor alone is: the case is not the one it means", static, brief, ceiling)
	}
	for name, tc := range map[string]struct {
		// own is what the launch's own turn charged through its meter before
		// the launch, and others what other seats spent in the company's
		// window.
		own, others int
		// held is the floor the launch is held at, and launches whether the
		// window it reads admits it.
		held     int
		launches bool
	}{
		"a full window": {held: static, launches: true},
		"the static floor left after other seats spent": {others: ceiling - static, held: static,
			launches: true},
		"a token short of the static floor": {others: ceiling - static + 1, held: static},
		"the static floor left after its own turn and other seats spent": {own: 1_000,
			others: ceiling - static - 1_000, held: static, launches: true},
		"a room its own turn left below the static floor": {own: 6_000, held: ceiling - 6_000, launches: true},
		"a token past its own turn's spend there":         {own: 6_000, others: 1, held: ceiling - 6_000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, l, jobs := busyFloorRig(t, ceiling)
			othersSpent(t, e, tc.others)
			// THE TURN'S OWN METER, which its context assembly was charged
			// through before the launch, as every dispatched turn's is.
			m := e.meterFor(l.company, l.seat.Handle())
			l.turn.Budget = m.auxiliary()
			if tc.own > 0 {
				if err := m.Record(t.Context(), tc.own, time.Now()); err != nil {
					t.Fatalf("Record: %v", err)
				}
			}
			left := ceiling - tc.own - tc.others
			req := launchRequest(l, nil)
			req.Brief = longBrief
			_, err := l.LaunchExecutor(t.Context(), req)
			if tc.launches {
				if err != nil || len(jobs.Started()) != 1 {
					t.Fatalf("a fresh launch held at %d with %d tokens left, after %d of its own turn's spend "+
						"and %d of other seats' = %v, %d jobs started; want it launched", tc.held, left, tc.own,
						tc.others, err, len(jobs.Started()))
				}
				return
			}
			var refused *floorRefusal
			if !errors.Is(err, toolloop.ErrBudgetExhausted) || !errors.As(err, &refused) ||
				refused.floor.tokens() != tc.held || refused.floor.brief != brief {
				t.Fatalf("a fresh launch past the room with %d tokens left = %v, want a budget refusal held at "+
					"%d, its brief of about %d kept", left, err, tc.held, brief)
			}
			// WHAT THE ROOM IS MADE OF, and where in it the floor is held.
			room := fmt.Sprintf("past %d, the smallest ceiling", ceiling)
			at := "held at turn_engine.executor_min_budget_tokens alone"
			if tc.held < static {
				room = fmt.Sprintf("past %d, the most room the seat's or the company's capped windows can "+
					"leave the launch (a ceiling of %d, less the %d its own turn spent there", tc.held, ceiling,
					tc.own)
				at = "held at that room, which only a window holding nothing but its own turn's spend leaves"
			}
			for _, says := range []string{fmt.Sprintf("its brief is about %d tokens", brief), room, at} {
				if !strings.Contains(err.Error(), says) {
					t.Errorf("the refusal says %q, want it to say %q", err, says)
				}
			}
			hold := e.heldByBudget(t.Context(), l.company, l.seat.Handle(), err)
			reason := fmt.Sprintf("the %d an executor launch needs (its brief about %d beside "+
				"turn_engine.executor_min_budget_tokens, %d, is %s", tc.held, brief, static, room)
			if hold == nil || !strings.Contains(hold.Refusal.Reason, reason) ||
				!strings.Contains(hold.Refusal.Reason, "so it is "+at) {
				t.Errorf("held = %+v, want the resume held on its window, the reason saying %q and %q", hold,
					reason, "so it is "+at)
			}
			refusedCleanly(t, e, "t1", jobs)
		})
	}
	t.Run("the least room of either scope and every period", func(t *testing.T) {
		t.Parallel()
		const static, brief = 50, 100
		seatDay := budgetBasis{org: coord.Caps{period.Day: 500}, seat: coord.Caps{period.Day: 120, period.Week: 900}}
		spent := func(tokens int) map[period.Period]int {
			return map[period.Period]int{period.Day: tokens, period.Week: tokens, period.Month: tokens}
		}
		for name, tc := range map[string]struct {
			basis budgetBasis
			spent map[period.Period]int
			held  int
		}{
			"the static floor, within the seat's day, the least room of three": {basis: seatDay,
				held: static},
			"the static floor, within what its own turn left of the seat's day": {basis: seatDay,
				spent: spent(30), held: static},
			"the room its own turn left of the seat's day, below the static floor": {basis: seatDay,
				spent: spent(80), held: 40},
			"the room of the company's week, where that leaves the least": {basis: budgetBasis{
				org: coord.Caps{period.Week: 100}, seat: coord.Caps{period.Day: 120}}, spent: spent(60), held: 40},
		} {
			f := launchFloor{static: static}.fresh(brief, tc.basis.room(tc.spent))
			if !f.held() || f.tokens() != tc.held || f.brief != brief {
				t.Errorf("%s: the fresh floor is held %v at %d tokens with a brief of %d, want held at %d with "+
					"its brief of %d kept", name, f.held(), f.tokens(), f.brief, tc.held, brief)
			}
		}
	})
	t.Run("no hold under a ceiling config refuses, nor at a room its own turn used up", func(t *testing.T) {
		t.Parallel()
		for name, room := range map[string]windowRoom{
			"a ceiling below the static floor": budgetBasis{seat: coord.Caps{period.Day: static - 1}}.room(nil),
			"a room its own turn used up": budgetBasis{seat: coord.Caps{period.Day: ceiling}}.room(
				map[period.Period]int{period.Day: ceiling, period.Week: ceiling, period.Month: ceiling}),
		} {
			f := launchFloor{static: static}.fresh(brief, room)
			if f.held() || f.tokens() != static+brief {
				t.Errorf("%s: the fresh floor is held %v at %d tokens, want it unheld at the static floor with "+
					"its brief beside it, %d", name, f.held(), f.tokens(), static+brief)
			}
		}
	})
}

// A HELD LAUNCH SAYS SO, NAMING THE FLOOR IT IS HELD TO. An operator reading a
// seat whose run took its window past its ceiling is told why by one line: the
// launch's fresh brief took its floor past the most room a window could leave
// it, so that floor was held, the brief's estimate that took it there, and the
// floor the launch is held to and where. Held at the static floor, the fresh
// floor is below the room, so a session the launch offers, its own floor
// within that room, may hold the launch higher: the line then names that
// session's floor, and still says that a run its box starts on the fresh brief
// may take its window past its ceiling. Said before the sessions were judged,
// it named the static floor for a launch held to the session's.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package otherwise
// leaves at its boot default.
//
// Mutation: drop the line, name the fresh floor where a session's holds the
// launch, say it is held at the static floor there, or say it only where the
// launch's own floor is the held one, and a case goes red.
func TestAHeldLaunchSaysItIsHeld(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	brief := harness.EstimateTokens(longBrief)
	static := new(config.TurnEngine).ExecutorFloor()
	const ceiling = 30_000
	_, l, jobs := floorRig(t, ceiling, 0, nil)
	l.turn.RunID = "held-launch-says-so"
	req := launchRequest(l, nil)
	req.Brief = longBrief
	if _, err := l.LaunchExecutor(t.Context(), req); err != nil || len(jobs.Started()) != 1 {
		t.Fatalf("a held launch in a full window = %v, %d jobs started; want it launched", err,
			len(jobs.Started()))
	}
	var lines []map[string]any
	for _, record := range logs.records(t) {
		if record["msg"] == "executor_floor_held" && record["turn_id"] == l.turn.RunID {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("the held launch said %d executor_floor_held lines, want 1", len(lines))
	}
	if floor, _ := lines[0]["floor"].(float64); int(floor) != static {
		t.Errorf("the line says the floor is %v, want %d, the static floor it is held at", lines[0]["floor"],
			static)
	}
	detail, _ := lines[0]["detail"].(string)
	for _, says := range []string{fmt.Sprintf("about %d tokens", brief), fmt.Sprintf("past %d,", ceiling)} {
		if !strings.Contains(detail, says) {
			t.Errorf("the line's detail is %q, want it to say %q", detail, says)
		}
	}
	if outcome, _ := lines[0]["outcome"].(string); !strings.Contains(outcome,
		"held at turn_engine.executor_min_budget_tokens alone") {
		t.Errorf("the line's outcome is %q, want it to say the launch is held at the static floor alone", outcome)
	}

	// A KEPT SESSION WHOSE OWN FLOOR FITS THE ROOM, above the static floor the
	// fresh one is held at: the launch is held to the session's floor, and the
	// line names that one.
	const sessionContext = 27_000
	sessionFloor := sessionContext + harness.EstimateTokens("carry on")
	if sessionFloor <= static || sessionFloor > ceiling {
		t.Fatalf("a session floor of %d is not between the static floor %d and the %d ceiling: the case is not "+
			"the one it means", sessionFloor, static, ceiling)
	}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	l.turn.RunID = "held-launch-offering-a-session"
	got, floor := l.continuedFloor(t.Context(), runner.AgentRunRequest{Brief: longBrief,
		Session: &runner.SessionContinuation{Brief: "carry on", Replaced: "carry on",
			Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
				Harness: executor.ClaudeCodeName, ContextTokens: sessionContext}}}, member, true)
	if got.Session == nil || floor.tokens() != sessionFloor {
		t.Fatalf("the launch offers the kept session %v and is held to %d, want it offered and the launch held "+
			"to its floor, %d", got.Session != nil, floor.tokens(), sessionFloor)
	}
	lines = nil
	for _, record := range logs.records(t) {
		if record["msg"] == "executor_floor_held" && record["turn_id"] == l.turn.RunID {
			lines = append(lines, record)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("a launch whose fresh floor is held and a session's holds it higher said %d executor_floor_held "+
			"lines, want 1: its box may still start its run on the fresh brief", len(lines))
	}
	if said, _ := lines[0]["floor"].(float64); int(said) != sessionFloor {
		t.Errorf("the line says the floor is %v, want %d, the session's the launch is held to", lines[0]["floor"],
			sessionFloor)
	}
	outcome, _ := lines[0]["outcome"].(string)
	for _, says := range []string{fmt.Sprintf("held at %d, the floor of a session it offers its run", sessionFloor),
		"starts a session of its own on the fresh brief", "sandbox_spend_over_budget"} {
		if !strings.Contains(outcome, says) {
			t.Errorf("the line's outcome is %q, want it to say %q", outcome, says)
		}
	}
	if strings.Contains(outcome, "executor_min_budget_tokens alone") {
		t.Errorf("the line's outcome is %q, which says the launch is held at the static floor it is held above",
			outcome)
	}
}

// busyLeadDoc is leadDoc in a busy company: the company's own day, capped at
// %c, is the window every seat's spend reaches, and the Lead's own day (%d) is
// larger, so the company's is the smallest ceiling.
const busyLeadDoc = `
name: Acme
timezone: Europe/Berlin
token_budget:
  day: %c
turn_engine:
  executor_min_budget_tokens: %f
roles:
  - name: Lead
    handle: lead
    token_budget:
      day: %d
`

// A TURN'S OWN SPEND NEVER HOLDS ITS LAUNCH FOR EVER, NOR DOES OTHER SEATS'.
// Every dispatched turn charges its context assembly through its meter, on the
// seat's auxiliary chain, before its launch reads the window, and charges it
// again every time the delivery runs; and in a busy company the other seats
// spend in the company's window first after every turnover. Held at the
// ceiling, a fresh launch whose brief takes its floor past the room its own
// turn leaves was refused, parked to the turnover and refused again in every
// window for as long as the delivery lasted. Held at that room, it went out
// only in a window nothing else had spent in, which a busy company whose own
// day is the smallest ceiling never left it. Held at the static floor, which
// fits in that room, it goes out in the first window with that much left,
// whatever else was spent there, and in the next where something else took the
// first below it; and so does a floor within the ceiling but past what its own
// turn leaves of it.
//
// AND A FLOOR WITHIN THE ROOM, PAST WHAT THE BUSY WINDOW LEAVES, WAITS ONE
// TURNOVER. The other seats' spend before the launch's read is not in the
// room, so a fresh floor between what the busy window leaves and the room
// (briefs of 3001 to 4000 tokens here, where the window leaves 28000 and the
// room is 29000), and a measured session's floor there, are refused in the
// first window and parked; the park records the window, and the delivery's
// next launch, in the next window, is held at the static floor and drops the
// session its window still leaves no room for, so it goes out there. Waited for
// in every window, such a launch went out in none, where before its brief was
// counted a fresh one went out at once, and a measured session's, whose floor
// counted its brief already, had that band then too. A floor the busy window
// leaves room for goes out at once, and one past the room is held at once. A
// seat that leaves the node between the two windows takes nothing of the park
// to its next holder, which parks it once more: it goes out in the third. And
// the delivery's launch TAKES the park's record, so a later delivery's launch
// in the same window, which never waited, is held to its floor and parked.
//
// ONLY A LAUNCH'S REFUSAL BELOW ITS FLOOR IS RECORDED. A first window that
// refused the turn's round, or whose gate parked the seat before any turn ran,
// refused no launch, so the next window's launch, its floor in the band, never
// waited for that floor: it is refused at it, parked, and goes out in the
// window after. A park an apply releases before its window turns over hands its
// launch back into the window it was refused in, which has not turned over:
// held to its floor there as before, it is parked again, and goes out in the
// next. And a waited try's fresh floor says it is held after its launch was
// refused below its floor, never that it is the floor refused: in the measured
// session's row the session's floor was, which that try drops.
//
// Mutation: hold at the ceiling, at the room where the static floor fits in it,
// or not at all, take the room as the ceiling with the turn's own spend left in
// it, read the turn's spend off any meter but the one the turn charged
// through, or tally no auxiliary call the meter posts; leave the wait
// unbounded, record no window with the park, or one with a park on a round's
// refusal or the gate's, read the window as turned over in the window it
// names, keep a session its window still leaves no room for, keep the record
// when the seat leaves the node or drop it only after the park's hold is
// lifted, read the record rather than take it, or say a waited floor is the
// floor its launch was refused below; and a case goes red.
func TestATurnsOwnSpendNeverHoldsItsLaunchForEver(t *testing.T) {
	t.Parallel()
	// A DAY OF 30000 UNDER A FLOOR OF 25000, and a thousand tokens of context
	// assembly every turn charges before its launch.
	const ceiling, static, assembly = 30_000, 25_000, 1_000
	for name, tc := range map[string]struct {
		// brief is the fresh brief's estimate, and before what else the
		// seat's window holds when the delivery first arrives.
		brief, before int
		// others is what other seats spend in the company's window before
		// every launch reads it, in a company whose own day is capped at the
		// ceiling and the Lead's at twice it.
		others int
		// session, where set, is the context a measured session the
		// launch may continue last carried, which the launch offers with a
		// brief of its own.
		session int64
		// moves is whether the seat leaves the node at the first turnover,
		// its mail handed back at once to the node that holds it next.
		moves bool
		// later is whether a second delivery reaches the seat once the
		// first's launch went out, in the same window: its launch never
		// waited, and is held to its floor.
		later bool
		// round is whether the delivery's first turn charges a round its
		// window cannot take before it launches: refused there, as the tool
		// loop refuses one, the turn parks its seat on a window that refused
		// no launch.
		round bool
		// released is whether an apply releases the first park before its
		// window turns over, raising the Lead's own day while the company's
		// stays the smallest: the delivery's next launch is in the window its
		// first was refused in.
		released bool
		// turns is how many times the delivery runs before its launch goes
		// out: once per window it waits in, none in a window whose gate
		// parked it before any turn ran, and once more where an apply hands
		// it back early.
		turns int
	}{
		"held past every window, in one nothing else spent in": {brief: 10_000, turns: 1},
		"held past every window, in one a background pass spent in first": {brief: 10_000, before: 2_000,
			turns: 1},
		"held past every window, past one left below the static floor": {brief: 10_000, before: 5_000,
			turns: 2},
		"held past every window, in a busy company's other seats spend in first": {brief: 10_000,
			others: 1_000, turns: 1},
		"within the ceiling, past what its own turn leaves": {brief: 4_500, turns: 1},
		// THE BAND, in a busy company: the busy window leaves 28000 at the
		// launch's read, and the room is 29000.
		"a floor the busy window leaves room for": {brief: 3_000, others: 1_000, turns: 1},
		"a floor a token past what the busy window leaves, within the room": {brief: 3_001, others: 1_000,
			turns: 2},
		"a floor at the room, past what the busy window leaves": {brief: 4_000, others: 1_000, turns: 2,
			later: true},
		"a floor a token past the room": {brief: 4_001, others: 1_000, turns: 1},
		"a measured session's floor within the room, past what the busy window leaves": {brief: 100,
			session: 28_500, others: 1_000, turns: 2},
		"a seat that leaves the node between the two windows": {brief: 4_000, others: 1_000, moves: true,
			turns: 3},
		// ONLY A LAUNCH'S REFUSAL IS RECORDED: the first window refused a
		// round, or its gate parked the seat before any turn ran (6000 spent
		// leaves it 24000, short of the static floor), and the next window's
		// launch never waited for its floor.
		"a round refused in the first window, then a floor within the room": {brief: 4_000, others: 1_000,
			round: true, turns: 3},
		"a gate that parked the seat before any turn, then a floor within the room": {brief: 4_000,
			before: 6_000, others: 1_000, turns: 2},
		// AND A PARK AN APPLY RELEASES EARLY hands its launch back into the
		// window it was refused in.
		"a park an apply releases before its window turns over": {brief: 4_000, others: 1_000,
			released: true, turns: 3},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lead := ceiling
			if tc.others > 0 {
				lead = 2 * ceiling
			}
			r := newFlooredParkRig(t, fmt.Sprint(lead), fmt.Sprint(static))
			if tc.others > 0 {
				// THE COMPANY'S OWN DAY CAPPED AT THE CEILING, the smallest:
				// the rig's company with that one key more, its seat's ids
				// unchanged, since they derive from the company's name and the
				// seat's handle.
				r.e.epoch.current.Store(companyFor(t, strings.NewReplacer("%c", fmt.Sprint(ceiling),
					"%d", fmt.Sprint(lead), "%f", fmt.Sprint(static)).Replace(busyLeadDoc)))
			}
			if tc.before > 0 {
				r.spend(t, tc.before)
			}
			brief := strings.Repeat("a", 4*tc.brief)
			colleague := coord.AgentScope(seatOf("colleague").String())
			var mu sync.Mutex
			launched, tries := 0, 0
			// refused is the floor each launch was refused below, in turn,
			// and said what a waited try's held floor said of why it is held.
			var refused []int
			said := ""
			r.mu.Lock()
			r.turnFn = func() (turn.Result, error) {
				ctx := t.Context()
				mu.Lock()
				tries++
				first := tries == 1
				mu.Unlock()
				// THE COMPANY CURRENT AS THE TURN STARTS, as every turn pins
				// its own: an apply between two turns is the next one's.
				c := r.e.Company()
				seat := c.Org.AgentSeatByHandle("lead")
				// THE TURN'S METER, and its context assembly charged through
				// it before the launch, as every dispatched turn charges its
				// own.
				m := r.e.meterFor(c, "lead")
				if err := m.Record(ctx, assembly, r.clock()); err != nil {
					return turn.Result{}, err
				}
				// AND THE OTHER SEATS' SPEND, which reaches the company's
				// window before this launch reads it, in every window.
				if tc.others > 0 {
					if _, err := r.fleet.PostCharge(ctx, colleague, tc.others,
						coord.WindowsAt(r.clock(), c.Config.Location())); err != nil {
						return turn.Result{}, err
					}
				}
				// A ROUND THE WINDOW CANNOT TAKE, charged as the tool loop
				// charges one: refused, and counted, so the window is full,
				// and the turn ends on that refusal before it launches.
				if tc.round && first {
					err := toolloop.Charge(ctx, m.budget(), ceiling)
					if !errors.Is(err, toolloop.ErrBudgetExhausted) {
						t.Errorf("a round of the whole ceiling beside the turn's own spend = %v, want it refused", err)
					}
					return turn.Result{}, err
				}
				l := &agentLauncher{engine: r.e, company: c, seat: seat,
					turn: &turnctx.Turn{RunID: "t1", Seat: seat, Budget: m.auxiliary()}}
				req := runner.AgentRunRequest{Brief: brief}
				if tc.session > 0 {
					req.Session = &runner.SessionContinuation{Brief: "carry on", Replaced: "carry on",
						Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
							Harness: executor.ClaudeCodeName, ContextTokens: tc.session}}
				}
				_, floor := l.continuedFloor(ctx, req, nil, true)
				if floor.held() && floor.waited.Valid() {
					mu.Lock()
					said = floor.heldFor()
					mu.Unlock()
				}
				if err := executorHeadroom(ctx, r.e.remainingFor(c, "lead"), floor); err != nil {
					var below *floorRefusal
					if errors.As(err, &below) {
						mu.Lock()
						refused = append(refused, below.floor.tokens())
						mu.Unlock()
					}
					return turn.Result{}, err
				}
				mu.Lock()
				launched++
				mu.Unlock()
				return turn.Result{}, nil
			}
			r.mu.Unlock()
			out := func() int {
				mu.Lock()
				defer mu.Unlock()
				return launched
			}
			r.deliver(t)
			if tc.released {
				// AN APPLY BEFORE THE TURNOVER that raises the Lead's own day,
				// the company's still the smallest: the park is released at
				// once, and the delivery runs again in the window its launch
				// was refused in.
				if r.runs() != 1 || !slices.Contains(r.holds(), pauseReasonBudget) {
					t.Fatalf("precondition: the first launch did not park its seat (runs %d, holds %v)", r.runs(),
						r.holds())
				}
				r.renew(t)
				raised := companyFor(t, strings.NewReplacer("%c", fmt.Sprint(ceiling), "%d", fmt.Sprint(3*ceiling),
					"%f", fmt.Sprint(static)).Replace(busyLeadDoc))
				r.e.epoch.current.Store(raised)
				r.e.reconcileBudgetParks(t.Context(), raised)
				if r.runs() != 2 {
					t.Fatalf("after an apply released the park %d turns ran, want the parked delivery run again at "+
						"once", r.runs())
				}
			}
			// THREE TURNOVERS at most, each the next Berlin midnight: a launch
			// held for ever goes out in none of them.
			for day := 0; day < 3 && out() == 0; day++ {
				alarms := r.alarms.live()
				if len(alarms) != 1 {
					t.Fatalf("after %d turns and no launch, %d alarms are armed, want the park's one: holds %v",
						r.runs(), len(alarms), r.holds())
				}
				r.renew(t)
				r.mu.Lock()
				r.now = berlinMidnight.Add(time.Duration(day)*24*time.Hour + time.Second)
				r.mu.Unlock()
				if tc.moves && day == 0 {
					// THE SEAT LEAVES THIS NODE, its parks forgotten as its
					// release forgets them, and its mail is handed back at
					// once to the node that holds it next: the same engine
					// here, which holds nothing of the park it forgot.
					r.e.forgetBudgetPark(t.Context(), seatOf("lead"))
					continue
				}
				alarms[0].fire()
			}
			if got := out(); got != 1 || r.runs() != tc.turns {
				t.Fatalf("the launch went out %d times over %d turns, want once, on turn %d: a held launch "+
					"waits for ever, or past a window that had room for it, or goes out below a floor it "+
					"never waited for", got, r.runs(), tc.turns)
			}
			// A WAITED FLOOR SAYS WHAT ITS RECORD KNOWS: that its launch was
			// refused below its floor in the first window, never that the
			// fresh brief's floor it holds is the floor refused, which was the
			// session's here.
			if tc.session > 0 {
				mu.Lock()
				firstBelow, says := 0, said
				if len(refused) > 0 {
					firstBelow = refused[0]
				}
				mu.Unlock()
				if want := int(tc.session) + harness.EstimateTokens("carry on"); firstBelow != want {
					t.Fatalf("the first window refused the launch below %d, want the session's floor, %d: the "+
						"case is not the one it means", firstBelow, want)
				}
				day := period.At(period.Day, parkedAt, r.e.Company().Config.Location())
				if want := fmt.Sprintf("a floor held after its launch was refused below its floor in the %s "+
					"window %s, which has turned over since", day.Period, day.Label); says != want {
					t.Errorf("the waited try's fresh floor says it is %q, want %q: that floor was never the "+
						"one refused", says, want)
				}
			}
			if !tc.later {
				return
			}
			r.deliver(t)
			if got := out(); got != 1 || r.runs() != tc.turns+1 || !slices.Contains(r.holds(), pauseReasonBudget) {
				t.Fatalf("a later delivery in the window after the wait went out %d times in all over %d turns, "+
					"its seat held %v; want its launch held to its floor and parked, since it never waited",
					got, r.runs(), r.holds())
			}
		})
	}
}

// A LAUNCH REFUSED BELOW ITS FLOOR WAITS ONE TURNOVER AT MOST, ON A RESUME TOO.
// A resumed segment's relaunch has no mail to park: its hold names the window
// the relaunch was refused in, the release counts that onto the job's record,
// and the turn's next relaunch reads it there. In a window after it, the
// relaunch's fresh brief's floor is held at the static floor, and a session its
// windows still leave no room for, measured or not, is dropped for the fresh
// brief, where one they leave room for is still continued. In the window it was
// refused in, the relaunch is held to its floor as before; and a record naming
// no window, as a build without the field wrote the job's record, reads as
// never refused. A session whose floor is exactly what its windows leave is
// still continued, since its launch goes out at exactly its floor; and a read
// of what they leave that fails drops no session, the launch's own read of the
// same counters deciding it then. A relaunch that waited and is refused all the
// same, its window short of the static floor too, says why it is held: a floor
// held after its launch was refused below its floor in a window that has turned
// over since, never past a room it is within, nor that floor the one refused;
// and a session it drops says what its windows leave. Only a launch's refusal
// below its floor names its window, in a window short of its floor or full
// alike, never a round's: a turn whose round was refused never waited for its
// launch's floor. In a busy company whose day of 30000 leaves 28000 once other
// seats have spent there, a brief of 4000 beside the static floor is 29000:
// within the room, past what the window leaves.
//
// Mutation: leave the wait unbounded, ignore the job's record, read the window
// as turned over in the window it names, drop a session the window leaves room
// for, exactly its floor included, or keep one it does not, drop every session
// on a read that fails, keep the copy no capture measured, say a waited floor
// is held past the room or is the floor refused, leave the window out of a
// dropped session's line, name the window of a round's refusal, or name none
// for a launch's in either window, and a case goes red.
func TestALaunchRefusedBelowItsFloorWaitsOneTurnoverAtMost(t *testing.T) {
	t.Parallel()
	static := new(config.TurnEngine).ExecutorFloor()
	const ceiling, others, brief = 30_000, 2_000, 4_000
	left := ceiling - others
	band := strings.Repeat("a", 4*brief)
	if need := static + brief; need <= left || need > ceiling {
		t.Fatalf("a floor of %d is not past the %d the window leaves and within its %d ceiling: the case is "+
			"not the one it means", need, left, ceiling)
	}
	// busy is the rig in its busy window, and the day the launch is in and
	// the one before it, as the launch's own meter cuts them.
	busy := func(t *testing.T) (*Engine, *agentLauncher, *sandbox.FakeRunner, sandbox.BudgetWindow,
		sandbox.BudgetWindow) {
		t.Helper()
		e, l, jobs := busyFloorRig(t, ceiling)
		othersSpent(t, e, others)
		day := e.meterFor(l.company, l.seat.Handle()).windows()[0]
		return e, l, jobs, sandbox.BudgetWindow{Period: day.Period, Label: day.Label},
			sandbox.BudgetWindow{Period: day.Period, Label: day.Shift(-1).Label}
	}
	// kept is the turn's kept session, measured at context, which a launch
	// offers with a brief of carry tokens.
	kept := func(context int64) *runner.SessionContinuation {
		return &runner.SessionContinuation{Brief: "carry on", Replaced: "carry on",
			Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
				Harness: executor.ClaudeCodeName, ContextTokens: context}}
	}
	carry := harness.EstimateTokens("carry on")
	for name, tc := range map[string]struct {
		// record is the window the job's record names: none, the day before
		// the launch's, or the launch's own day.
		record   string
		launches bool
	}{
		"a record naming no window, as an older build wrote it, reads as never refused": {record: "none"},
		"refused in the day before, it goes out at the static floor":                    {record: "before", launches: true},
		"refused in the day it is in, it is held to its floor as before":                {record: "today"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, l, jobs, today, before := busy(t)
			record := map[string]sandbox.BudgetWindow{"before": before, "today": today}[tc.record]
			l.relaunchRefusedIn = &record
			req := launchRequest(l, nil)
			req.Brief = band
			_, err := l.LaunchExecutor(t.Context(), req)
			if tc.launches {
				if err != nil || len(jobs.Started()) != 1 {
					t.Fatalf("a relaunch refused in %+v with %d left = %v, %d jobs started; want it launched at the "+
						"static floor %d", record, left, err, len(jobs.Started()), static)
				}
				return
			}
			var refused *floorRefusal
			if !errors.As(err, &refused) || refused.floor.held() || refused.floor.tokens() != static+brief {
				t.Fatalf("a relaunch whose record names %+v with %d left = %v, want a refusal at its floor, %d, "+
					"unheld", record, left, err, static+brief)
			}
			refusedCleanly(t, e, "t1", jobs)
		})
	}
	t.Run("the sessions a relaunch that waited still offers", func(t *testing.T) {
		t.Parallel()
		_, l, _, today, before := busy(t)
		member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName,
			model: "claude-opus-5-5"})
		for name, tc := range map[string]struct {
			record sandbox.BudgetWindow
			req    runner.AgentRunRequest
			keeps  bool
			floor  int
		}{
			"a measured session its window leaves room for is continued": {record: before,
				req: runner.AgentRunRequest{Brief: "the fresh brief", Session: kept(27_000)}, keeps: true,
				floor: 27_000 + carry},
			// AT EXACTLY ITS FLOOR, as its launch goes out at exactly its
			// floor ([executorHeadroom]): continued, never started fresh for
			// nothing.
			"a measured session whose floor is exactly what its window leaves is continued": {record: before,
				req: runner.AgentRunRequest{Brief: "the fresh brief", Session: kept(int64(left - carry))}, keeps: true,
				floor: left},
			"a measured session its window is still short of is dropped": {record: before,
				req: runner.AgentRunRequest{Brief: "the fresh brief", Session: kept(28_500)}, floor: static},
			"a copy no capture measured, its window still short of it, is dropped": {record: before,
				req: runner.AgentRunRequest{Brief: "the fresh brief",
					Continue: &runner.Continuation{SessionID: "s-1", Brief: band}}, floor: static},
			"in the day it was refused in, a session is held to as before": {record: today,
				req: runner.AgentRunRequest{Brief: "the fresh brief", Session: kept(28_500)}, keeps: true,
				floor: 28_500 + carry},
		} {
			record := tc.record
			l.relaunchRefusedIn = &record
			got, floor := l.continuedFloor(t.Context(), tc.req, member, true)
			offered := got.Session != nil || (got.Continue != nil && got.Continue.SessionID != "")
			if offered != tc.keeps || floor.tokens() != tc.floor {
				t.Errorf("%s: the launch offers a session %v and is held to %d, want %v and %d", name, offered,
					floor.tokens(), tc.keeps, tc.floor)
			}
		}
	})
	t.Run("a read of what the windows leave that fails drops no session", func(t *testing.T) {
		t.Parallel()
		e, l, _, _, before := busy(t)
		member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName,
			model: "claude-opus-5-5"})
		// THE COUNTERS UNREADABLE after the turnover: the room and the
		// turnover read none, and the launch's own read of the same counters
		// ([executorHeadroom]) fails the launch rather than a session being
		// given up on a store's blip.
		e.backends.Fleet = unreachableFleet{fleetBase: e.backends.Fleet}
		l.relaunchRefusedIn = &before
		got, floor := l.continuedFloor(t.Context(), runner.AgentRunRequest{Brief: "the fresh brief",
			Session: kept(28_500)}, member, true)
		if got.Session == nil || floor.tokens() != 28_500+carry {
			t.Errorf("a relaunch that waited, its windows unreadable, offers the session: %v, held to %d; want "+
				"it offered and held to its floor, %d", got.Session != nil, floor.tokens(), 28_500+carry)
		}
	})
	t.Run("a relaunch that waited and is short of the static floor says why it is held", func(t *testing.T) {
		t.Parallel()
		e, l, jobs, _, before := busy(t)
		othersSpent(t, e, ceiling-static-others+1)
		l.relaunchRefusedIn = &before
		req := launchRequest(l, nil)
		req.Brief = band
		_, err := l.LaunchExecutor(t.Context(), req)
		var refused *floorRefusal
		if !errors.As(err, &refused) || !refused.floor.held() || refused.floor.tokens() != static {
			t.Fatalf("a relaunch that waited, with %d left = %v, want a refusal held at the static floor %d",
				static-1, err, static)
		}
		why := fmt.Sprintf("a floor held after its launch was refused below its floor in the day window %s, "+
			"which has turned over since", before.Label)
		for _, says := range []string{why, "held at turn_engine.executor_min_budget_tokens alone"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the refusal says %q, want it to say %q", err, says)
			}
		}
		if hold := e.heldByBudget(t.Context(), l.company, l.seat.Handle(), err); hold == nil ||
			!strings.Contains(hold.Refusal.Reason, "is "+why+", so it is held at") {
			t.Errorf("held = %+v, want the reason saying it is held because %q", hold, why)
		}
		refusedCleanly(t, e, "t1", jobs)
		// AND A SESSION IT DROPS SAYS WHAT ITS WINDOWS LEAVE, AND AFTER WHICH
		// WINDOW.
		short := staticFloor(l.company).continuing(28_500, 2).stillShort(left, before)
		for _, says := range []string{fmt.Sprintf("its windows leave %d", left), "day window " + before.Label} {
			if !strings.Contains(short, says) {
				t.Errorf("a dropped session's line says %q, want it to say %q", short, says)
			}
		}
	})
	t.Run("only a launch's refusal below its floor names its window", func(t *testing.T) {
		t.Parallel()
		for name, spent := range map[string]int{
			// A DAY LEFT BELOW THE STATIC FLOOR, so the window holds a round's
			// refusal and a launch's alike, as one too short.
			"a window short of the static floor": ceiling - static + 1,
			// AND ONE WITH NOTHING LEFT, which refuses either outright.
			"a full window": ceiling,
		} {
			e, l, _ := busyFloorRig(t, ceiling)
			othersSpent(t, e, spent)
			day := e.meterFor(l.company, l.seat.Handle()).windows()[0]
			launch := &floorRefusal{floor: staticFloor(l.company).fresh(brief, windowRoom{}), left: ceiling - spent}
			if held := e.heldByBudget(t.Context(), l.company, l.seat.Handle(), launch); held == nil ||
				held.Refusal.Window != (sandbox.BudgetWindow{Period: day.Period, Label: day.Label}) {
				t.Errorf("%s: a launch refused below its floor is held as %+v, want the hold naming the day %q "+
					"it waits on", name, held, day.Label)
			}
			round := fmt.Errorf("the round was refused: %w", toolloop.ErrBudgetExhausted)
			if held := e.heldByBudget(t.Context(), l.company, l.seat.Handle(), round); held == nil ||
				held.Refusal.Window != (sandbox.BudgetWindow{}) {
				t.Errorf("%s: a round's refusal is held as %+v, want it held naming no window", name, held)
			}
		}
	})
}

// A FLOOR TURNED OFF HOLDS NO LAUNCH WHOSE CONTEXT IS UNKNOWN. `0` turns
// turn_engine.executor_min_budget_tokens off, and it keeps doing so for a
// launch that continues no measured session, whatever its brief: counted
// beside a floor of nothing, the brief alone would refuse launches the company
// turned the floor off for. A launch that continues a session whose context a
// capture measured is still held to that context with its brief.
//
// Mutation: count the brief beside a floor turned off, or turn the floor off
// for a measured session too, and a case goes red.
func TestAFloorTurnedOffHoldsNoLaunchWhoseContextIsUnknown(t *testing.T) {
	t.Parallel()
	t.Run("a fresh launch with a long brief", func(t *testing.T) {
		t.Parallel()
		// A DAY OF 100000 WITH 99000 SPENT: a thousand left, a tenth of the
		// brief.
		_, l, jobs := floorRig(t, 100_000, 99_000, new(0))
		req := launchRequest(l, nil)
		req.Brief = longBrief
		if _, err := l.LaunchExecutor(t.Context(), req); err != nil || len(jobs.Started()) != 1 {
			t.Fatalf("a fresh launch under a floor turned off = %v, %d jobs started; want it launched", err,
				len(jobs.Started()))
		}
	})
	t.Run("a continued session whose context a capture measured", func(t *testing.T) {
		t.Parallel()
		c, seat := executorCompany(t)
		c.Config.TurnEngine.ExecutorMinBudgetTokens = new(0)
		l := &agentLauncher{engine: &Engine{}, company: c, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
		member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
		req := runner.AgentRunRequest{Brief: longBrief, Session: &runner.SessionContinuation{Brief: "carry on",
			Replaced: "carry on", Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
				Harness: executor.ClaudeCodeName, ContextTokens: 40000}}}
		_, floor := l.continuedFloor(t.Context(), req, member, true)
		if want := 40000 + harness.EstimateTokens("carry on"); floor.tokens() != want {
			t.Fatalf("a session whose last request carried 40000 under a floor turned off is held to %+v "+
				"(%d tokens), want %d", floor, floor.tokens(), want)
		}
		if err := executorHeadroom(t.Context(), staticRemaining{left: 30000}, floor); !errors.Is(err,
			toolloop.ErrBudgetExhausted) {
			t.Errorf("a launch continuing it with 30000 left = %v, want a budget refusal", err)
		}
	})
}
