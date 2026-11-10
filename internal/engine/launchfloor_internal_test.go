package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
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
	if refusal, held := r.e.budgetHold(t.Context(), r.e.Company(), "lead", staticFloor(r.e.Company())); held {
		t.Fatalf("the static floor holds %+v, where the window has room for it: the case is not the one "+
			"a continued launch's own floor decides", refusal)
	}
}

// A SESSION NO WINDOW CAN CARRY IS NOT CONTINUED. A floor past the smallest
// ceiling of any capped window of the seat's or the company's is one no window
// turning over ever meets, so its relaunch would wait for ever: such a session
// starts fresh, on the static floor every ceiling is already held above. And
// the floor is the larger of the static floor and the context with the brief,
// never their sum: the static floor counts the harness's own prompt, which the
// context's last request counted too.
//
// Mutation: add the static floor to the context, or compare against the
// largest ceiling, and a row goes red.
func TestASessionNoWindowCanCarryIsNotContinued(t *testing.T) {
	t.Parallel()
	capped := budgetBasis{org: coord.Caps{period.Day: 500}, seat: coord.Caps{period.Day: 120, period.Week: 900}}
	static := launchFloor{static: 50}
	for name, tc := range map[string]struct {
		floor  launchFloor
		basis  budgetBasis
		tokens int
		fits   bool
	}{
		"within the smallest ceiling": {floor: static.continuing(90, 15), basis: capped, tokens: 105, fits: true},
		"past the smallest ceiling":   {floor: static.continuing(110, 15), basis: capped, tokens: 125},
		"under the static floor":      {floor: static.continuing(30, 5), basis: capped, tokens: 50, fits: true},
		"no session":                  {floor: static, basis: capped, tokens: 50, fits: true},
		"nothing capped":              {floor: static.continuing(110, 15), tokens: 125, fits: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.floor.tokens(); got != tc.tokens {
				t.Errorf("the floor is %d tokens, want %d", got, tc.tokens)
			}
			why := tc.floor.fits(tc.basis)
			if (why == "") != tc.fits || (!tc.fits && !strings.Contains(why, "120")) {
				t.Errorf("fits = %q, want fitting %v, else naming the smallest ceiling", why, tc.fits)
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
// where neither is left does its run start a session of its own, on the static
// floor. Held both to the larger floor and dropped together, a kept session
// whose short brief fits was dropped for the copy's longer one, which the launch
// sends only where its box cannot restore the kept session.
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
