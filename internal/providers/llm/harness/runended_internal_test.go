package harness

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// shipped is an entry on the harness's shipped profile — its own limit markers,
// measured against the harness — whose binary does not exist: nothing here runs it.
func shipped(t *testing.T, agent, model string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: agent, Harness: agent, Model: model, StateDir: dir,
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")},
		Timeout:   time.Minute, Auth: Auth{Mode: AuthAPIKey, APIKey: "sk-not-real"},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", agent, err)
	}
	return p
}

// spentLine is the result line Claude Code 2.1.312 ends a run on when the plan
// is spent, measured: a status where the words would be a sentinel's, and the
// words deliberately none ("You've hit your" opens answers too).
const spentLine = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
	`"result":"You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)"}`

// AN EXECUTOR RUN THAT SPENT THE PLAN SITS ITS ENTRY OUT, ON THE VENDOR'S WORD
// AND NOTHING ELSE. The run is the harness's own, so nothing a call on this host reads ever
// saw its limit: the next launch on the entry — a reviewer's send-back an hour
// into the same window — walked into the same spent plan. Read through the
// same classification every call takes, by the profile each harness ships with:
// Claude Code's result line by its status first, then its report where its
// answer should be, or in the error of a run it said failed; OpenCode's typed
// failure event, whose payload the runner collects as the run's error. A
// model's answer that merely MENTIONS a limit, and an error that is no limit,
// bench nothing.
//
// Mutation: hand RunEnded's classification no terminal event, and Claude Code
// 2.1.312's spent plan benches nothing.
func TestAnExecutorRunThatSpentThePlanSitsItsEntryOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent, model  string
		answer, err, event  string
		failed, wantBenched bool
	}{
		{name: "claude code 2.1.312's result line, by its status",
			agent: "claude-code", model: "sonnet",
			answer: "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
			err:    "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
			event:  spentLine, failed: true, wantBenched: true},
		{name: "claude code 2.1.312's words with no result line",
			agent: "claude-code", model: "sonnet",
			answer: "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
			err:    "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
			failed: true},
		{name: "claude code's report standing where the answer should be",
			agent: "claude-code", model: "sonnet",
			answer: "Usage limit reached · resets 5pm", wantBenched: true},
		{name: "claude code's report in a run it said failed",
			agent: "claude-code", model: "sonnet",
			err: "the run stopped: Usage limit reached · resets 5pm", failed: true, wantBenched: true},
		{name: "an answer that mentions a limit",
			agent: "claude-code", model: "sonnet",
			answer: "Added backoff for when GitHub says Usage limit reached."},
		{name: "opencode's quota event",
			agent: "opencode", model: "anthropic/claude-sonnet-5",
			err: `{"type":"provider.quota","message":"You have exhausted your quota"}`, failed: true,
			wantBenched: true},
		{name: "opencode's own failure that is no limit",
			agent: "opencode", model: "anthropic/claude-sonnet-5",
			err: `{"type":"provider.unknown","message":"boom"}`, failed: true},
		{name: "opencode's limit class spoken in prose",
			agent: "opencode", model: "anthropic/claude-sonnet-5",
			answer: `the provider said "provider.quota" earlier`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := shipped(t, tc.agent, tc.model)
			end := RunEnd{Answer: tc.answer, Error: tc.err, Event: tc.event, Failed: tc.failed}
			if got := p.RunEnded(t.Context(), end); got != tc.wantBenched {
				t.Fatalf("RunEnded = %v, want %v", got, tc.wantBenched)
			}
			if sat := p.SittingOut() != nil; sat != tc.wantBenched {
				t.Errorf("the entry sits out %v, want %v", sat, tc.wantBenched)
			}
		})
	}
}

// A RUN'S RESET IS READ AGAINST WHEN ITS ENDING WAS FIRST READ. Claude Code
// names the reset as a time of day, the next occurrence of which is the
// instant: read against the collection, a 4:30pm an hour and a quarter away
// sits the entry out and the refusal names it; read again after a resume that
// was held past it, the same ending names an instant already gone, and sits
// nothing out — read against that later instant, it was the next day's
// 4:30pm, and the entry sat out for nothing.
//
// Mutation: read the reset against the provider's clock rather than the
// ending's instant, and the late reading sits the entry out; read it against no
// instant at all, and the first reading lands in year one and sits nothing out.
func TestARunsResetIsReadAgainstWhenItsEndingWasFirstRead(t *testing.T) {
	t.Parallel()
	collected := time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC) // 16:16 in Vancouver
	resets := time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC)
	end := RunEnd{Event: spentLine, Failed: true, At: collected,
		Error: "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)"}

	first := shipped(t, "claude-code", "sonnet")
	first.now = func() time.Time { return collected.Add(time.Minute) }
	if !first.RunEnded(t.Context(), end) {
		t.Fatal("the spent plan was not read as one")
	}
	var refused *llm.Error
	if err := first.SittingOut(); !errors.As(err, &refused) || !refused.ResetsAt.Equal(resets) {
		t.Fatalf("SittingOut = %v, want a refusal naming the reset at %v", err, resets)
	}

	late := shipped(t, "claude-code", "sonnet")
	late.now = func() time.Time { return collected.Add(3 * time.Hour) }
	late.RunEnded(t.Context(), end)
	if err := late.SittingOut(); err != nil {
		t.Fatalf("an ending read again after its reset passed sits the entry out: %v", err)
	}
}

// AN ENDING READ AGAIN IS THE SAME REPORT. A completion held on a limit is
// collected again when its hold lifts, and every collection reads the job's
// ending. Dated by the reading, the ending opened a fresh window each time,
// and one that named no reset stepped the backoff as a probe that met the limit
// again would: the resume that waited out a minute found the entry benched for
// two before any call was made, then five, then fifteen, for good, and one
// whose vendor named a reset days out was benched afresh every hour. Dated by
// its first collection, the second reading falls in the window the first
// opened, which is over: it sits nothing out, says so, and leaves the backoff
// where the first reading put it. A report made after the window is the next
// probe's, and takes the next step.
//
// Mutation: date the window by the provider's clock in bench, and the second
// reading sits the entry out again.
func TestAnEndingReadAgainIsTheSameReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent, model string
		end                RunEnd
		collected          time.Time
		window, next       time.Duration
		step               int
	}{
		{name: "a quota that named no reset", agent: "opencode", model: "anthropic/claude-sonnet-5",
			end: RunEnd{Failed: true,
				Error: `{"type":"provider.quota","message":"You have exhausted your quota"}`},
			collected: time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC),
			window:    limitBackoff[0], next: limitBackoff[1], step: 1},
		{name: "a reset beyond the hour", agent: "claude-code", model: "sonnet",
			end:       RunEnd{Event: spentLine, Failed: true},
			collected: time.Date(2026, time.October, 27, 21, 16, 0, 0, time.UTC), // 2:16pm in Vancouver
			window:    maxLimitBench, next: maxLimitBench},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &testClock{now: tc.collected}
			p := shipped(t, tc.agent, tc.model)
			p.now = clock.read
			end := tc.end
			end.At = tc.collected
			if !p.RunEnded(t.Context(), end) {
				t.Fatal("the premise: the first reading sits the entry out")
			}
			if left := sitOutUntil(p).Sub(clock.read()); left != tc.window {
				t.Fatalf("the first reading sits out %v, want %v", left, tc.window)
			}

			clock.advance(tc.window + time.Second)
			if p.RunEnded(t.Context(), end) {
				t.Error("the ending read again after its window says it sat the entry out")
			}
			if err := p.SittingOut(); err != nil {
				t.Errorf("the ending read again after its window sits the entry out again: %v", err)
			}
			if step := unnamedStep(p); step != tc.step {
				t.Errorf("the backoff is at step %d after one report read twice, want %d", step, tc.step)
			}

			later := tc.end
			later.At = clock.read()
			if !p.RunEnded(t.Context(), later) {
				t.Fatal("a report made after the window sat nothing out")
			}
			if left := sitOutUntil(p).Sub(clock.read()); left != tc.next {
				t.Errorf("a report made after the window sits out %v, want %v", left, tc.next)
			}
		})
	}
}

// A SUCCESS FROM BEFORE THE WINDOW'S END IS NO ANSWER. A run that succeeded
// is collected when its seat gets to it — launched before the limit, held
// behind its seat's conditions, collected again after a hold — and may be
// read while the entry sits out a limit another run reported. It says the
// vendor answered before that report or inside its window, not that the limit
// lifted: the window keeps the reset the vendor named, which every refusal in
// it tells a person ("resumes at 4:30"), and the backoff keeps its step. A
// success dated after the window is the answer it looks like.
//
// Mutation: let a run's success end the report whatever its date, and the
// refusal names no reset.
func TestASuccessFromBeforeTheWindowsEndLeavesItsReportStanding(t *testing.T) {
	t.Parallel()
	collected := time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)
	resets := time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC)
	clock := &testClock{now: collected.Add(time.Minute)}
	p := shipped(t, "claude-code", "sonnet")
	p.now = clock.read
	if !p.RunEnded(t.Context(), RunEnd{Event: spentLine, Failed: true, At: collected}) {
		t.Fatal("the premise: the spent plan sits the entry out")
	}
	setUnnamed(p, 2)

	for _, at := range []time.Time{collected.Add(-time.Hour), collected.Add(5 * time.Minute)} {
		p.RunEnded(t.Context(), RunEnd{Answer: "fixed the flaky test", At: at})
	}
	var refused *llm.Error
	if err := p.SittingOut(); !errors.As(err, &refused) || !refused.ResetsAt.Equal(resets) {
		t.Errorf("after successes from before and inside the window, SittingOut = %v, "+
			"want the refusal naming the reset at %v", err, resets)
	}
	if step := unnamedStep(p); step != 2 {
		t.Errorf("a success from before the window's end moved the backoff to step %d, want 2", step)
	}

	clock.advance(resets.Sub(clock.read()))
	p.RunEnded(t.Context(), RunEnd{Answer: "fixed the flaky test", At: resets})
	if step := unnamedStep(p); step != 0 {
		t.Errorf("a success dated after the window left the backoff at step %d, want it started again", step)
	}
}
