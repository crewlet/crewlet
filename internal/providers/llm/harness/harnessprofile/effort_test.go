package harnessprofile

import (
	"slices"
	"strings"
	"testing"
)

// CLAUDE CODE'S LEVELS ARE ITS MODEL'S: the models its 2.1.312 binary runs with
// no effort, or without xhigh or max, are named in full; an alias follows the
// harness's latest models and takes every level.
func TestClaudeCodesLevelsFollowItsModel(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("claude-code")
	if !ok {
		t.Fatal("no claude-code profile")
	}
	all := []string{"low", "medium", "high", "xhigh", "max"}
	for _, tc := range []struct {
		model    string
		want     []string
		narrowed bool
	}{
		{"opus", all, false},
		{"claude-opus-5-5", all, false},
		{"claude-sonnet-5-5[1m]", all, false},
		{"claude-haiku-4-5", nil, true},
		{"claude-haiku-4-5-20251001", nil, true},
		{"claude-sonnet-4-5@20250929", nil, true},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", nil, true},
		{"claude-3-7-sonnet-20250219", nil, true},
		{"claude-opus-4-1", nil, true},
		{"claude-opus-4-20250514", nil, true},
		{"claude-opus-4-5-20251101", []string{"low", "medium", "high"}, true},
		{"claude-opus-4-6", []string{"low", "medium", "high", "max"}, true},
		{"claude-sonnet-4-6[1m]", []string{"low", "medium", "high", "max"}, true},
	} {
		got, narrowed := p.EffortLevels(tc.model)
		if !slices.Equal(got, tc.want) || narrowed != tc.narrowed {
			t.Errorf("%s: levels %v (narrowed %v), want %v (%v)", tc.model, got, narrowed, tc.want, tc.narrowed)
		}
	}
}

// A REFUSAL SAYS WHAT THE HARNESS TAKES, and a harness with no setting why it has none.
func TestAnEffortProblemSaysWhatTheHarnessTakes(t *testing.T) {
	t.Parallel()
	claude, _ := Builtin("claude-code")
	cursor, _ := Builtin("cursor-agent")
	for _, tc := range []struct {
		p                  Profile
		model, level, want string
	}{
		{claude, "sonnet", "high", ""},
		{claude, "sonnet", "ultra", `takes "low", "medium", "high", "xhigh" or "max", not "ultra"`},
		{claude, "claude-haiku-4-5", "low", "runs claude-haiku-4-5 with no reasoning effort setting"},
		{claude, "claude-opus-4-6", "xhigh", `on claude-opus-4-6, not "xhigh"`},
		{cursor, "gpt-5.5", "high", "as part of the model id"},
	} {
		got := tc.p.EffortProblem("the harness", tc.model, tc.level)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s %s: problem %q, want one containing %q", tc.model, tc.level, got, tc.want)
		}
	}
}

// AN EFFORT THE ENGINE COULD NOT PASS DOES NOT LOAD: no placeholder, no levels,
// a model rule naming a level the harness lacks, a variable the engine owns.
func TestAnEffortTheEngineCannotPassIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		effort map[string]any
		want   string
	}{
		{"no placeholder", map[string]any{"args": []any{"--effort", "high"}, "levels": []any{"high"}}, "carries no {effort}"},
		{"no levels", map[string]any{"args": []any{"--effort", "{effort}"}, "levels": []any{}}, "effort.levels is empty"},
		{"a level twice", map[string]any{"args": []any{"--effort", "{effort}"}, "levels": []any{"high", "high"}}, "twice"},
		{"a rule's level", map[string]any{"args": []any{"--effort", "{effort}"}, "levels": []any{"high"},
			"models": []any{map[string]any{"match": "x.*", "levels": []any{"low"}}}}, "which effort.levels does not"},
		{"a bad rule", map[string]any{"args": []any{"--effort", "{effort}"}, "levels": []any{"high"},
			"models": []any{map[string]any{"match": "(", "levels": []any{"high"}}}}, "not a regular expression"},
		{"an isolation variable", map[string]any{"env": map[string]any{"HOME": "{effort}"}, "levels": []any{"high"}}, "which the engine sets itself"},
	} {
		_, err := Load("claude-code", map[string]any{"effort": tc.effort})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Load = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	if _, err := Load("cursor-agent", map[string]any{
		"effort": map[string]any{"args": []any{"--think", "{effort}"}, "levels": []any{"low", "high"}}}); err != nil {
		t.Errorf("an operator's own effort: %v", err)
	}
}

// EVERY SHIPPED EFFORT LOADS, and a harness without one says why.
func TestEveryShippedEffortLoads(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		if name == "custom" {
			continue // ships no defaults to load
		}
		p, err := Load(name, nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !p.TakesEffort() && p.EffortNote == "" {
			t.Errorf("%s has no effort and no effort_note saying why", name)
		}
	}
}
