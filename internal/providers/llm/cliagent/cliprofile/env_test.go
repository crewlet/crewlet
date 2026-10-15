package cliprofile

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE ENGINE OWNS THE ISOLATION AND THE PER-CALL CHANNEL, AND NOTHING ELSE.
//
// What EngineEnv names is what the backend removes from every lower layer and
// what config refuses an operator to write, so it must hold every variable a
// seat's state could be relocated by — the base layout and the vendor's own —
// plus the system-prompt channel, and no variable a profile legitimately
// configures.
func TestTheEngineOwnsTheIsolationAndTheSystemPromptChannel(t *testing.T) {
	t.Parallel()
	base := []string{"HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}
	for agent, extra := range map[string][]string{
		"claude-code": {"CLAUDE_CONFIG_DIR"},
		"codex":       {"CODEX_HOME"},
		"gemini-cli":  {"GEMINI_SYSTEM_MD"},
		"muse-code":   nil,
	} {
		p, ok := Builtin(agent)
		if !ok {
			t.Fatalf("no built-in %s profile", agent)
		}
		want := slices.Sorted(slices.Values(append(append([]string(nil), base...), extra...)))
		if got := p.EngineEnv(); !slices.Equal(got, want) {
			t.Errorf("%s: EngineEnv() = %v, want %v", agent, got, want)
		}
		for name := range p.Env {
			if p.EngineSets(name) != "" {
				t.Errorf("%s: its own env variable %s is claimed by the engine", agent, name)
			}
		}
	}
}

// Every isolation variable points inside the seat home but the cache, which
// sits beside it, and a relocation variable naming a base one cannot move it.
func TestTheSeatEnvironmentStaysInTheSeatHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "state", "seats", "ana", "home")
	cache := filepath.Join(string(filepath.Separator), "state", "seats", "ana", "cache")
	p := Profile{ConfigEnv: map[string]string{"VENDOR_DIR": ".vendor", "XDG_CACHE_HOME": ".cache"}}
	env := p.SeatEnv(home, cache)
	for name, value := range env {
		if name == "XDG_CACHE_HOME" {
			if value != cache {
				t.Errorf("XDG_CACHE_HOME = %q, want the cache beside the home, %q", value, cache)
			}
			continue
		}
		if value != home && !strings.HasPrefix(value, home+string(filepath.Separator)) {
			t.Errorf("%s = %q, which is not inside the seat home %q", name, value, home)
		}
	}
	if env["VENDOR_DIR"] != filepath.Join(home, ".vendor") {
		t.Errorf("VENDOR_DIR = %q, want the relocation under the home", env["VENDOR_DIR"])
	}
}

// A profile naming an engine variable in a layer that cannot reach the CLI is
// told so, naming the field and the variable.
func TestAProfileMayNotNameAVariableTheEngineSets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		agent     string
		overrides map[string]any
		field     string
		variable  string
	}{
		{"env HOME", "claude-code", map[string]any{"env": map[string]any{"HOME": "/srv"}}, "env", "HOME"},
		{"env relocation", "claude-code",
			map[string]any{"env": map[string]any{"CLAUDE_CONFIG_DIR": "/srv"}}, "env", "CLAUDE_CONFIG_DIR"},
		{"passthrough HOME", "codex", map[string]any{"passthrough_env": []any{"HOME"}}, "passthrough_env", "HOME"},
		{"env system prompt", "qwen-code",
			map[string]any{"env": map[string]any{"QWEN_SYSTEM_MD": "/srv/p.md"}}, "env", "QWEN_SYSTEM_MD"},
		{"token_env", "claude-code", map[string]any{"token_env": "XDG_STATE_HOME"}, "token_env", "XDG_STATE_HOME"},
		{"api_key_env", "claude-code", map[string]any{"api_key_env": "TMPDIR"}, "api_key_env", "TMPDIR"},
	} {
		p, err := Load(tc.agent, tc.overrides)
		if err != nil {
			t.Errorf("%s: the runnable rules refused a profile that runs: %v", tc.name, err)
			continue
		}
		err = p.ValidateEnv(tc.agent)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		for _, want := range []string{tc.field + " names " + tc.variable, "engine sets"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error does not say %q: %v", tc.name, want, err)
			}
		}
	}
}

// No shipped profile names one: the table is held to the rule it is judged by.
func TestNoShippedProfileNamesAVariableTheEngineSets(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if err := p.ValidateEnv(name); err != nil {
			t.Error(err)
		}
	}
}

// A relocation variable is part of the isolation, so the runnable rules keep
// it in the seat home and off the base variables.
func TestARelocationVariableMustStayInTheSeatHome(t *testing.T) {
	t.Parallel()
	for name, configEnv := range map[string]map[string]any{
		"a climb":            {"CLAUDE_CONFIG_DIR": "../../shared"},
		"an absolute path":   {"CLAUDE_CONFIG_DIR": "/srv/shared"},
		"HOME":               {"HOME": ".home"},
		"an XDG directory":   {"XDG_DATA_HOME": ".data"},
		"the temp directory": {"TMPDIR": ".tmp"},
	} {
		if _, err := Load("claude-code", map[string]any{"config_env": configEnv}); err == nil {
			t.Errorf("%s: config_env %v loaded", name, configEnv)
		}
	}
	if _, err := Load("claude-code", map[string]any{"config_env": map[string]any{"VENDOR_DIR": "nested/dir"}}); err != nil {
		t.Errorf("a relocation inside the home was refused: %v", err)
	}
}
