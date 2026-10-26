package cliagent

import (
	"path/filepath"
	"testing"
	"time"
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

// AN EXECUTOR RUN THAT SPENT THE PLAN SITS ITS ENTRY OUT, ON THE VENDOR'S WORD
// AND NOTHING ELSE. The run is the CLI's own, so nothing a call on this host reads ever
// saw its limit: the next launch on the entry — a reviewer's send-back an hour
// into the same window — walked into the same spent plan. Read through the
// same classification every call takes, by the profile each CLI ships with:
// Claude Code's report where its answer should be, or in the error of a run it
// said failed; OpenCode's typed failure event, whose payload the runner
// collects as the run's error. A model's answer that merely MENTIONS a limit,
// and an error that is no limit, bench nothing.
func TestAnExecutorRunThatSpentThePlanSitsItsEntryOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent, model  string
		answer, err         string
		failed, wantBenched bool
	}{
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
			if got := p.RunEnded(tc.answer, tc.err, tc.failed); got != tc.wantBenched {
				t.Fatalf("RunEnded = %v, want %v", got, tc.wantBenched)
			}
			if sat := p.SittingOut() != nil; sat != tc.wantBenched {
				t.Errorf("the entry sits out %v, want %v", sat, tc.wantBenched)
			}
		})
	}
}

// A REBUILT ENTRY KEEPS ITS SIT-OUT. Every apply builds every entry afresh, and
// an entry with an unchanged spec is the same login, so the same spent plan —
// carried, or every seat on the node relearned the window with a call each.
// A later bench is kept where it ends later; an entry that sits nothing out
// carries nothing.
func TestARebuiltEntryKeepsItsSitOut(t *testing.T) {
	t.Parallel()
	before := shipped(t, "claude-code", "sonnet")
	if !before.RunEnded("Usage limit reached · resets 5pm", "", false) {
		t.Fatal("the premise: the old entry sits out")
	}
	after := shipped(t, "claude-code", "sonnet")
	after.CarrySitOut(before)
	if after.SittingOut() == nil {
		t.Fatal("the rebuilt entry was handed back the spent plan")
	}
	clean := shipped(t, "claude-code", "sonnet")
	clean.CarrySitOut(shipped(t, "claude-code", "sonnet"))
	if err := clean.SittingOut(); err != nil {
		t.Errorf("an entry carried from one sitting nothing out sits out: %v", err)
	}
}
