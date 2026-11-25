package harness

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// usageScreen is Claude Code 2.1.325's `-p /usage --output-format json` on a plan,
// measured (trimmed to the fields read): no turn, no API time, no cost.
const usageScreen = `{"type":"result","subtype":"success","is_error":false,"num_turns":0,` +
	`"duration_api_ms":0,"total_cost_usd":0,"result":"You are currently using your subscription ` +
	`to power your Claude Code usage\n\nCurrent session: 13%% used · resets Nov 25, 6:29am (UTC)\n` +
	`Current week (all models): %d%% used · resets Nov 28, 10:59am (UTC)\n` +
	`Current week (Fable): 0%% used · resets Nov 28, 11am (UTC)"}`

// refusedScreen is the same read while the vendor refuses it: the header alone.
const refusedScreen = `{"type":"result","subtype":"success","is_error":false,"num_turns":0,` +
	`"result":"You are currently using your subscription to power your Claude Code usage"}`

// fakePlanRead plays a plan read: FAKE_PLAN_STDOUT when the fence reached it,
// else a model's answer, as an unfenced unknown command would be.
func fakePlanRead() {
	if os.Getenv("FAKE_FENCE") != "fenced" {
		fmt.Print(`{"type":"result","num_turns":1,"total_cost_usd":0.11,"result":"no such command"}`)
		os.Exit(0)
	}
	fmt.Print(os.Getenv("FAKE_PLAN_STDOUT"))
}

// planProvider is an entry signed in with a login in files whose plan read is
// [fakePlanRead], read the way the shipped Claude Code profile reads it.
func planProvider(t *testing.T, stdout string, fence string) (*Provider, *testClock) {
	t.Helper()
	run := "-test.run=TestFakeHarness"
	p := fakeProvider(t, map[string]string{"FAKE_STORE": "1", "FAKE_PLAN_STDOUT": stdout}, map[string]any{
		"credential_paths": []any{".fake/creds.json"},
		"status_args":      []any{run, "store-status"},
		"plan_read": map[string]any{
			"args":  []any{run, "store-plan"},
			"fence": map[string]any{"FAKE_FENCE": fence},
			"text":  []any{"result"}, "turns": []any{"num_turns"},
			"windows": []any{
				map[string]any{"opens": "Current session:", "window": "session", "hours": 5},
				map[string]any{"opens": "Current week (all models):", "window": "weekly", "hours": 168},
			},
			"used_suffix": "% used", "reset_separator": "resets ", "reset_unit": "clock",
		},
	})
	clock := &testClock{now: time.Date(2026, time.November, 25, 5, 30, 0, 0, time.UTC)}
	p.now = clock.read
	return p, clock
}

// weekly is the sign-in's weekly reading.
func weekly(t *testing.T, p *Provider) Limit {
	t.Helper()
	for _, l := range p.Plan().State(p.now()).Limits {
		if l.Window == llm.LimitWeekly {
			return l
		}
	}
	t.Fatal("no weekly reading")
	return Limit{}
}

// THE PLAN IS READ WITH NO MODEL CALL, through the harness's own screen, fenced.
// Each window's line gives its use and reset; a line the read does not name (one
// model's week) is not a plan-wide window.
//
// Mutation: drop the fence from the read's environment, and the fake answers as
// a model would, which the turn count refuses.
func TestAPlanReadReadsTheHarnesssUsageScreen(t *testing.T) {
	t.Parallel()
	p, clock := planProvider(t, fmt.Sprintf(usageScreen, 72), "fenced")
	readings, err := p.ReadPlan(t.Context())
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if len(readings) != 2 {
		t.Fatalf("read %d windows, want the session and the week: %+v", len(readings), readings)
	}
	w := weekly(t, p)
	if w.Used == nil || *w.Used != 0.72 || w.State != llm.LimitOK || w.Hours != 168 ||
		!w.ResetsAt.Equal(time.Date(2026, time.November, 28, 10, 59, 0, 0, time.UTC)) ||
		!w.Seen.Equal(clock.read()) {
		t.Errorf("the week reads %+v, want 72%% used, ok, resetting Nov 28 10:59, seen now", w)
	}
	if err := p.SittingOut(); err != nil {
		t.Errorf("a plan with room sits out: %v", err)
	}
}

// A READ THAT RAN A MODEL TURN IS REFUSED, and so is one naming no window (the
// vendor refused the read): neither changes what the sign-in holds. An entry
// signed in with a token has no read at all.
//
// Mutation: accept a read whatever its turn count, and the unfenced case reads.
func TestAPlanReadThatFailsChangesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stdout, fence string
		want          error
	}{
		"it ran a model turn": {stdout: fmt.Sprintf(usageScreen, 50), fence: "open", want: ErrPlanReadRanModel},
		"the vendor refused":  {stdout: refusedScreen, fence: "fenced"},
		"no JSON":             {stdout: "Error: something", fence: "fenced"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, clock := planProvider(t, tc.stdout, tc.fence)
			reset := clock.read().Add(5 * 24 * time.Hour)
			p.bench(limitReport{reset: reset, said: "You've hit your weekly limit", at: clock.read(),
				window: llm.LimitWeekly})
			clock.advance(time.Minute)
			_, err := p.ReadPlan(t.Context())
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("ReadPlan answered %v, want a failure (%v)", err, tc.want)
			}
			if got := p.Plan().State(clock.read()); len(got.Limits) != 0 ||
				len(got.SittingOut) != 1 || !got.SittingOut[0].Until.Equal(reset) {
				t.Errorf("a failed read changed the sign-in: %+v", got)
			}
		})
	}
	token := shippedEntry(t, "claude-code", Auth{Mode: AuthSubscription, Token: "sk-ant-oat01-not-real"}, nil)
	if _, err := token.ReadPlan(t.Context()); !errors.Is(err, ErrNoPlanRead) {
		t.Errorf("a token's entry read its plan: %v, want ErrNoPlanRead", err)
	}
}

// A READING DECIDES THE PLAN-WIDE WINDOW, so no call is a probe. A week used up
// sits the sign-in out until it turns over, days away, with no call made; a plan
// read below the headroom line ends a session or weekly sit-out opened before it
// (the plan reset or was raised); one above it, a sit-out whose window nothing
// names, or a run's reading (dated when the run is collected) keeps it.
//
// Mutation: drop the room rule, and the reset week stays out; read room from the
// warning line's far side, and the 85% case comes back into a near-spent plan;
// let any reading end it, and the late run's does.
func TestAReadingSitsAPlanOutAndBringsItBack(t *testing.T) {
	t.Parallel()
	p, clock := planProvider(t, fmt.Sprintf(usageScreen, 100), "fenced")
	if _, err := p.ReadPlan(t.Context()); err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	until := sitOutUntil(p)
	if want := time.Date(2026, time.November, 28, 10, 59, 0, 0, time.UTC); !until.Equal(want) {
		t.Fatalf("a used-up week sits out until %v, want its reset %v", until, want)
	}
	if err := p.SittingOut(); llm.KindOf(err) != llm.KindRateLimit {
		t.Fatalf("a used-up week refuses %v, want the sit-out", err)
	}

	for name, tc := range map[string]struct {
		used   float64
		window llm.LimitWindow
		run    bool
		back   bool
	}{
		"the week reset":                                  {used: 0.03, window: llm.LimitWeekly, back: true},
		"the week is still near its end":                  {used: 0.85, window: llm.LimitWeekly},
		"a limit no window names":                         {used: 0.03, window: llm.LimitUnknown},
		"a run collected after the limit, reporting room": {used: 0.03, window: llm.LimitWeekly, run: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := newPlan()
			at := time.Date(2026, time.November, 25, 6, 0, 0, 0, time.UTC)
			plan.wide.record(limitReport{reset: at.Add(3 * 24 * time.Hour), said: "limit", at: at,
				window: tc.window})
			used := tc.used
			plan.observe([]llm.LimitReading{{Window: llm.LimitWeekly, Hours: 168, State: llm.LimitOK,
				Used: &used, ResetsAt: at.Add(7 * 24 * time.Hour)}}, at.Add(time.Minute), !tc.run)
			if _, out := plan.wide.sitOut(at.Add(2*time.Minute), ""); out == tc.back {
				t.Errorf("sitting out after the reading = %v, want %v", out, !tc.back)
			}
		})
	}
	_ = clock
}

// A READING FROM BEFORE A SIT-OUT DOES NOT END IT: the limit was heard after it.
//
// Mutation: drop the since check in reckon, and the old reading ends the window.
func TestAnOlderReadingDoesNotEndANewerSitOut(t *testing.T) {
	t.Parallel()
	plan := newPlan()
	at := time.Date(2026, time.November, 25, 6, 0, 0, 0, time.UTC)
	plan.wide.record(limitReport{reset: at.Add(24 * time.Hour), said: "limit", at: at, window: llm.LimitWeekly})
	used := 0.1
	plan.observe([]llm.LimitReading{{Window: llm.LimitWeekly, State: llm.LimitOK, Used: &used}},
		at.Add(-time.Minute), true)
	if _, out := plan.wide.sitOut(at.Add(time.Minute), ""); !out {
		t.Error("a reading made before the limit ended its sit-out")
	}
}

// A LIMIT A CALL HEARS ASKS FOR A READ, once per window it opens.
//
// Mutation: drop readWanted from bench, and no read is asked for.
func TestAHeardLimitAsksForAPlanRead(t *testing.T) {
	t.Parallel()
	p, clock := planProvider(t, "", "fenced")
	if !p.Plan().ReadWanted().IsZero() {
		t.Fatal("a read is wanted before any limit")
	}
	p.bench(limitReport{said: "rate limit", at: clock.read()})
	if got := p.Plan().ReadWanted(); !got.Equal(clock.read()) {
		t.Errorf("ReadWanted = %v, want the report's instant %v", got, clock.read())
	}
}

// THE SHIPPED CLAUDE CODE PROFILE READS THE SCREEN AS MEASURED: a spent week is
// reached, with its reset; the session's own line is read beside it.
//
// Mutation: change the shipped windows' opening text, and nothing is read.
func TestTheShippedClaudeCodeProfileReadsItsUsageScreen(t *testing.T) {
	t.Parallel()
	p := shipped(t, "claude-code", "sonnet")
	at := time.Date(2026, time.November, 25, 5, 30, 0, 0, time.UTC)
	readings, err := p.planReadings(&rawResult{stdout: fmt.Sprintf(usageScreen, 100)}, at)
	if err != nil {
		t.Fatalf("planReadings: %v", err)
	}
	if len(readings) != 2 || readings[1].Window != llm.LimitWeekly || readings[1].State != llm.LimitReached ||
		!readings[1].ResetsAt.Equal(time.Date(2026, time.November, 28, 10, 59, 0, 0, time.UTC)) {
		t.Errorf("read %+v, want the session and a reached week resetting Nov 28 10:59", readings)
	}
	if p.profile.PlanRead.Fence["ANTHROPIC_BASE_URL"] == "" {
		t.Error("the shipped read is not fenced from the model")
	}
}
