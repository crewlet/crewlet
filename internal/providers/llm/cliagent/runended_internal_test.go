package cliagent

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// shipped is an entry on the CLI's shipped profile — its own limit markers,
// measured against the CLI — whose binary does not exist: nothing here runs it.
func shipped(t *testing.T, agent, model string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: agent, Agent: agent, Model: model, StateDir: dir,
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
// AND NOTHING ELSE. The run is the CLI's own, so nothing a call on this host reads ever
// saw its limit: the next launch on the entry — a reviewer's send-back an hour
// into the same window — walked into the same spent plan. Read through the
// same classification every call takes, by the profile each CLI ships with:
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

// A REBUILT ENTRY KEEPS ITS SIT-OUT. Every apply builds every entry afresh, and
// an entry with an unchanged spec is the same login, so the same spent plan —
// carried, or every seat on the node relearned the window with a call each:
// its window, the instant the vendor said the limit lifts, which a refusal
// names to a person, and how far its backoff has stepped. A later bench is
// kept where it ends later; an entry that sits nothing out carries nothing.
//
// Mutation: carry the window alone, and the rebuilt entry's refusal names no
// reset and its next unnamed report starts the backoff from its first step.
func TestARebuiltEntryKeepsItsSitOut(t *testing.T) {
	t.Parallel()
	collected := time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)
	clock := func() time.Time { return collected.Add(time.Minute) }
	before := shipped(t, "claude-code", "sonnet")
	before.now = clock
	if !before.RunEnded(t.Context(), RunEnd{Event: spentLine, Failed: true, At: collected}) {
		t.Fatal("the premise: the old entry sits out")
	}
	before.sitOut.mu.Lock()
	before.sitOut.unnamed = 2
	before.sitOut.mu.Unlock()
	after := shipped(t, "claude-code", "sonnet")
	after.now = clock
	after.CarrySitOut(before)
	var refused *llm.Error
	if err := after.SittingOut(); !errors.As(err, &refused) {
		t.Fatal("the rebuilt entry was handed back the spent plan")
	} else if want := time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC); !refused.ResetsAt.Equal(want) {
		t.Errorf("the rebuilt entry's refusal names the reset %v, want %v", refused.ResetsAt, want)
	}
	after.sitOut.mu.Lock()
	unnamed := after.sitOut.unnamed
	after.sitOut.mu.Unlock()
	if unnamed != 2 {
		t.Errorf("the rebuilt entry's backoff is at step %d, want the carried 2", unnamed)
	}
	clean := shipped(t, "claude-code", "sonnet")
	clean.CarrySitOut(shipped(t, "claude-code", "sonnet"))
	if err := clean.SittingOut(); err != nil {
		t.Errorf("an entry carried from one sitting nothing out sits out: %v", err)
	}
}
