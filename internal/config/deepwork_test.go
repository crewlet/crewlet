package config

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// deepEntry is an agent-mode Claude Code entry deep work may run on.
const deepEntry = "    deep:\n      agent: claude-code\n      model: sonnet\n      mode: agent\n"

// deepCompany is a company running the assistant on its usual entry, with
// deep work's block lines appended under assistant.deep_work.
func deepCompany(entries, block string) string {
	return assistantCompany(assistantEntry+entries, "  enabled: true\n  llm: [assistant]\n  deep_work:\n"+block)
}

// DEEP WORK RUNS ONLY ON AN AGENT-MODE CLAUDE CODE ENTRY, and is
// handed out from one place, which refuses what admission refuses.
func TestDeepWorkIsHandedOutOnlyForAnAgentModeEntry(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, deepCompany(deepEntry, "    llm: deep\n"))
	key, entry, err := cfg.AssistantDeepWork()
	if err != nil || key != "deep" || !entry.AgentMode() {
		t.Fatalf("AssistantDeepWork = %q, %+v, %v", key, entry, err)
	}
	if off := mustCompany(t, assistantCompany(assistantEntry, "  enabled: true\n  llm: [assistant]\n")); off != nil {
		if _, _, err := off.AssistantDeepWork(); !errors.Is(err, ErrDeepWorkOff) {
			t.Errorf("a company with no deep_work: err = %v, want ErrDeepWorkOff", err)
		}
	}
	for name, c := range map[string]struct{ entries, block, field string }{
		"no llm":      {deepEntry, "    max_steps: 20\n", "assistant.deep_work.llm"},
		"unknown llm": {deepEntry, "    llm: nope\n", "assistant.deep_work.llm"},
		"text mode": {"    deep:\n      agent: claude-code\n      model: sonnet\n",
			"    llm: deep\n", "assistant.deep_work.llm"},
		"no runner": {"    deep:\n      agent: codex\n      model: gpt-5\n      mode: agent\n",
			"    llm: deep\n", "assistant.deep_work.llm"},
		"no session to continue": {"    deep:\n      agent: opencode\n      model: x/y\n      mode: agent\n",
			"    llm: deep\n", "assistant.deep_work.llm"},
		"unconfigured e2b": {"    deep:\n      agent: claude-code\n      mode: agent\n      run_in: e2b\n",
			"    llm: deep\n", "providers.llm.deep.run_in"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rejects(t, deepCompany(c.entries, c.block), c.field)
		})
	}
}

// DEEP WORK'S BOUNDS: each range refused past either end, each zero its
// default, each stored value out of range clamped, and the wall clock never
// past the read grant a turn holds.
func TestDeepWorkValidatesEveryRange(t *testing.T) {
	t.Parallel()
	fields := []struct {
		bound assistantBound
		read  func(AssistantDeepWork) int
		set   func(*AssistantDeepWork, int)
		want  [3]int
	}{
		{deepTimeout, func(d AssistantDeepWork) int { return int(d.Timeout() / time.Second) },
			func(d *AssistantDeepWork, v int) { d.TimeoutSeconds = v }, [3]int{60, 1200, 1200}},
		{deepSteps, AssistantDeepWork.Steps, func(d *AssistantDeepWork, v int) { d.MaxSteps = v },
			[3]int{10, 400, 150}},
		{deepTokens, AssistantDeepWork.Tokens, func(d *AssistantDeepWork, v int) { d.MaxTokens = v },
			[3]int{10_000, 20_000_000, 3_000_000}},
	}
	if deepTimeout.max != AssistantTurnTimeoutCeiling {
		t.Errorf("a deep-work run may outlive the grant its turn holds: %d > %d",
			deepTimeout.max, AssistantTurnTimeoutCeiling)
	}
	for _, f := range fields {
		t.Run(f.bound.key, func(t *testing.T) {
			t.Parallel()
			if got := [3]int{f.bound.min, f.bound.max, f.bound.def}; got != f.want {
				t.Errorf("bounds = %v, want %v", got, f.want)
			}
			doc := func(v int) string {
				return deepCompany(deepEntry, fmt.Sprintf("    llm: deep\n    %s: %d\n", f.bound.key, v))
			}
			for _, bad := range []int{f.bound.min - 1, f.bound.max + 1} {
				if err := rejects(t, doc(bad), "assistant.deep_work."+f.bound.key); !errors.Is(err, ErrOutOfRange) {
					t.Errorf("%d: want ErrOutOfRange, got %v", bad, err)
				}
			}
			cfg := mustCompany(t, doc(f.bound.max))
			if got := f.read(*cfg.Assistant.DeepWork); got != f.bound.max {
				t.Errorf("%d reads as %d", f.bound.max, got)
			}
			var d AssistantDeepWork
			if got := f.read(d); got != f.bound.def {
				t.Errorf("zero reads as %d, want %d", got, f.bound.def)
			}
			f.set(&d, f.bound.max*2)
			if got := f.read(d); got != f.bound.max {
				t.Errorf("a stored %d reads as %d, want %d", f.bound.max*2, got, f.bound.max)
			}
		})
	}
}
