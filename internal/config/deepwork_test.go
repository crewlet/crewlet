package config

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// deepEntry is a Claude Code entry deep work may run on.
const deepEntry = "    deep:\n      agent: claude-code\n      model: sonnet\n"

// deepCompany is a company running the assistant on its usual entry, with
// deep work's block lines appended under assistant.deep_work.
func deepCompany(entries, block string) string {
	return assistantCompany(assistantEntry+entries, "  enabled: true\n  llm: [assistant]\n  deep_work:\n"+block)
}

// DEEP WORK RUNS ONLY ON A CLAUDE CODE ENTRY, and is handed out from one
// place, which refuses what admission refuses: another runner's CLI, or one
// with no runner at all, would meet a host confinement measured against
// Claude Code's alone.
func TestDeepWorkIsHandedOutOnlyForAClaudeCodeEntry(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, deepCompany(deepEntry, "    llm: deep\n"))
	key, entry, err := cfg.AssistantDeepWork()
	if err != nil || key != "deep" || entry.AgentName() != string(CLIClaudeCode) {
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
		"no runner": {"    deep:\n      agent: codex\n      model: gpt-5\n",
			"    llm: deep\n", "assistant.deep_work.llm"},
		"another runner": {"    deep:\n      agent: opencode\n      model: x/y\n",
			"    llm: deep\n", "assistant.deep_work.llm"},
		"unconfigured e2b": {"    deep:\n      agent: claude-code\n      run_in: e2b\n",
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

// DEEP WORK RUNS IN A REMOTE OR CONTAINED CELL WHENEVER ONE IS CONFIGURED, and
// on the engine host only when none is: the entry's own run_in first, then
// e2b, then a container with an image, then the host. Mutation: prefer the
// host and the e2b case reads direct.
func TestDeepWorkPrefersAConfiguredSandbox(t *testing.T) {
	t.Parallel()
	e2b := "  sandbox:\n    e2b:\n      api_key: ${E2B_API_KEY}\n"
	container := "  sandbox:\n    local: {image: crewlet/agent:1}\n"
	for name, c := range map[string]struct {
		sandbox, entry string
		want           Placement
	}{
		"no catalogue":         {"", deepEntry, PlacementDirect},
		"e2b configured":       {e2b, deepEntry, PlacementE2B},
		"container with image": {container, deepEntry, PlacementContainer},
		"the entry's own":      {e2b, deepEntry + "      run_in: direct\n", PlacementDirect},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := "name: Acme\nproviders:\n" + c.sandbox + "  llm:\n" + assistantEntry + c.entry +
				"assistant:\n  enabled: true\n  llm: [assistant]\n  deep_work:\n    llm: deep\n"
			cfg := mustCompany(t, doc)
			got, ok := cfg.DeepWorkPlacement()
			if !ok || got != c.want {
				t.Fatalf("DeepWorkPlacement = %q, %v; want %q", got, ok, c.want)
			}
			if c.want != PlacementDirect {
				if _, reached := cfg.SandboxPlacements()[c.want]; !reached {
					t.Errorf("deep work's %s cell is not reached, so its backend is never built", c.want)
				}
			}
		})
	}
}
