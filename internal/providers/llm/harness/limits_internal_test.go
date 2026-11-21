package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// capturedLimitEvent is Claude Code 2.1.325's rate-limit event from a real
// subscription run (2026-11-18), ids removed.
const capturedLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning",` +
	`"resetsAt":1795413600,"rateLimitType":"seven_day","utilization":0.57,"overageStatus":"rejected",` +
	`"overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":` +
	`{"utilization":0.05,"resetsAt":1795000800,"status":"allowed"},"seven_day":{"utilization":0.57,` +
	`"resetsAt":1795413600,"status":"allowed"}},"overageEnabled":false}}`

// withoutWindows is the same event without `unifiedWindows`.
const withoutWindows = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning",` +
	`"resetsAt":1795413600,"rateLimitType":"seven_day","utilization":0.57}}`

func fraction(f float64) *float64 { return &f }

// reading is one expected window, its fraction -1 where none is said.
type reading struct {
	window llm.LimitWindow
	hours  int
	model  string
	state  llm.LimitState
	used   float64
	resets int64
}

func readingsOf(t *testing.T, got []llm.LimitReading) []reading {
	t.Helper()
	out := make([]reading, 0, len(got))
	for _, r := range got {
		used := -1.0
		if r.Used != nil {
			used = *r.Used
		}
		var resets int64
		if !r.ResetsAt.IsZero() {
			resets = r.ResetsAt.Unix()
		}
		out = append(out, reading{r.Window, r.Hours, r.Model, r.State, used, resets})
	}
	return out
}

func sameReadings(t *testing.T, what string, got []llm.LimitReading, want []reading) {
	t.Helper()
	have := readingsOf(t, got)
	if len(have) != len(want) {
		t.Fatalf("%s: read %+v, want %+v", what, have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("%s: window %d is %+v, want %+v", what, i, have[i], want[i])
		}
	}
}

// EVERY WINDOW IS READ FROM THE EVENT THE HARNESS ALREADY WRITES, its own
// window's fields winning: the weekly window warns, which `unifiedWindows` calls allowed.
func TestEveryWindowIsReadFromTheCapturedEvent(t *testing.T) {
	t.Parallel()
	got, reached := readLimitReports(claudeCode(t), capturedLimitEvent)
	sameReadings(t, "the captured event", got, []reading{
		{llm.LimitSession, 5, "", llm.LimitOK, 0.05, 1795000800},
		{llm.LimitWeekly, 168, "", llm.LimitWarning, 0.57, 1795413600},
	})
	if reached != nil {
		t.Errorf("an allowed event reached %+v, want nothing", reached)
	}
}

// `unifiedWindows` IS OPTIONAL: any shape of it degrades to the event's own
// window, then to its state alone; a status the map does not hold is no report.
func TestALimitReportOfAnotherShapeDegradesToWhatItSays(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	for _, tc := range []struct {
		name  string
		event string
		want  []reading
	}{
		{"no object of windows", withoutWindows,
			[]reading{{llm.LimitWeekly, 168, "", llm.LimitWarning, 0.57, 1795413600}}},
		{"an object of windows that is no object",
			strings.Replace(capturedLimitEvent, `"unifiedWindows":{`, `"unifiedWindows":"x","was":{`, 1),
			[]reading{{llm.LimitWeekly, 168, "", llm.LimitWarning, 0.57, 1795413600}}},
		{"a window saying neither a state nor a fraction, and one the map does not name",
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour",` +
				`"unifiedWindows":{"seven_day":{"utilization":"high"},"ninety_day":{"utilization":0.2}}}}`,
			[]reading{{llm.LimitSession, 5, "", llm.LimitOK, -1, 0}}},
		{"a window with a fraction and no status reads ok",
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour",` +
				`"utilization":0.1,"unifiedWindows":{"seven_day":{"utilization":0.3}}}}`,
			[]reading{{llm.LimitSession, 5, "", llm.LimitOK, 0.1, 0}, {llm.LimitWeekly, 168, "", llm.LimitOK, 0.3, 0}}},
		{"a model's own window", `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning",` +
			`"rateLimitType":"seven_day_opus","utilization":0.83,"resetsAt":1795413600}}`,
			[]reading{{llm.LimitModel, 168, "opus", llm.LimitWarning, 0.83, 1795413600}}},
		{"a status the map does not hold", strings.Replace(capturedLimitEvent, `"allowed_warning"`, `"throttled"`, 1), nil},
		{"a line of the model's own words", `{"type":"assistant","rate_limit_info":{"status":"rejected"}}`, nil},
	} {
		got, _ := readLimitReports(p, tc.event)
		sameReadings(t, tc.name, got, tc.want)
	}
}

// A RUN'S NEWER REPORT OF A WINDOW REPLACES ITS EARLIER ONE, and its last
// rejected report is the limit it met (unknown where the map names none).
func TestARunsLastReportOfAWindowWins(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	later := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour",` +
		`"utilization":1,"resetsAt":1795000800}}`
	got, reached := readLimitReports(p, capturedLimitEvent+"\n"+later)
	sameReadings(t, "two reports", got, []reading{
		{llm.LimitSession, 5, "", llm.LimitReached, 1, 1795000800},
		{llm.LimitWeekly, 168, "", llm.LimitWarning, 0.57, 1795413600},
	})
	if reached == nil || reached.Window != llm.LimitSession || reached.ResetsAt.Unix() != 1795000800 {
		t.Errorf("reached %+v, want the 5-hour window resetting at 1795000800", reached)
	}
	_, reached = readLimitReports(p, rejected("ninety_day", reportedAt.Add(time.Hour)))
	if reached == nil || reached.Window != llm.LimitUnknown {
		t.Errorf("an unnamed window reached %+v, want %q", reached, llm.LimitUnknown)
	}
}

// A SIGN-IN KEEPS THE NEWEST READING OF EACH WINDOW: a run collected late never
// undoes a later call's word.
func TestASignInKeepsTheNewestReadingOfEachWindow(t *testing.T) {
	t.Parallel()
	plan := newPlan()
	t1, t2 := reportedAt, reportedAt.Add(time.Minute)
	plan.Observe([]llm.LimitReading{{Window: llm.LimitSession, Hours: 5, State: llm.LimitOK, Used: fraction(0.2)}}, t2)
	plan.Observe([]llm.LimitReading{
		{Window: llm.LimitSession, Hours: 5, State: llm.LimitOK, Used: fraction(0.5)},
		{Window: llm.LimitModel, Hours: 168, Model: "opus", State: llm.LimitWarning, Used: fraction(0.9)},
		{Window: llm.LimitWeekly, Hours: 168, State: llm.LimitOK, Used: fraction(0.4)},
	}, t1)
	got := plan.State(t2).Limits
	if len(got) != 3 {
		t.Fatalf("limits %+v, want three windows", got)
	}
	for i, want := range []struct {
		window llm.LimitWindow
		used   float64
		seen   time.Time
	}{{llm.LimitSession, 0.2, t2}, {llm.LimitWeekly, 0.4, t1}, {llm.LimitModel, 0.9, t1}} {
		if got[i].Window != want.window || *got[i].Used != want.used || !got[i].Seen.Equal(want.seen) {
			t.Errorf("window %d is %+v, want %s at %v seen %v", i, got[i], want.window, want.used, want.seen)
		}
	}
}

// AN EXECUTOR RUN'S LIMIT REPORT REACHES ITS SIGN-IN, answered or not: a seat's
// own runs are most of what a plan spends.
func TestAnExecutorRunsLimitReportReachesItsSignIn(t *testing.T) {
	t.Parallel()
	p := shipped(t, "claude-code", "sonnet")
	p.RunEnded(t.Context(), RunEnd{Answer: "done", Limits: capturedLimitEvent, At: reportedAt})
	got := p.Plan().State(reportedAt).Limits
	if len(got) != 2 || got[0].Window != llm.LimitSession || got[1].Window != llm.LimitWeekly ||
		!got[0].Seen.Equal(reportedAt) {
		t.Errorf("limits %+v, want the 5-hour and weekly windows seen at %v", got, reportedAt)
	}
}
