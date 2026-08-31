package cliagent

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// Config accepts a closed set of agent names; this package ships the profiles
// for them. A name accepted by validation with no profile behind it is a
// company that passes `crewlet validate` and fails at its first turn, which is
// the worst possible place to learn about it.
func TestEveryConfiguredAgentNameHasAProfile(t *testing.T) {
	t.Parallel()
	// Compared as plain strings: config's set is typed (CLIAgentName) so a
	// document cannot name one it does not define, while this package keys
	// its profile registry by string because it does not import config.
	// The two lists still have to be the same list.
	shipped := BuiltinNames()
	accepted := make([]string, len(config.CLIAgentNames))
	for i, name := range config.CLIAgentNames {
		accepted[i] = string(name)
	}
	for _, name := range accepted {
		if !slices.Contains(shipped, name) {
			t.Errorf("config accepts cli.agent %q but no profile ships for it", name)
		}
	}
	for _, name := range shipped {
		if !slices.Contains(accepted, name) {
			t.Errorf("profile %q ships but config refuses it as cli.agent", name)
		}
	}
}

// Every shipped profile must be usable as-is. A profile that needs an
// override to work at all is one an operator discovers is broken by running
// it, which defeats the point of shipping it.
func TestEveryShippedProfileLoadsWithNoOverrides(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		if name == "custom" {
			// `custom` ships nothing on purpose, and its emptiness is
			// asserted by TestCustomShipsNothingAndSaysWhatIsMissing.
			continue
		}
		if _, err := Load(name, nil); err != nil {
			t.Errorf("Load(%q): %v", name, err)
		}
	}
}

// The `custom` profile must fail with a message naming what to set, not with
// a nil-pointer or a silent success that produces an empty command line.
func TestCustomShipsNothingAndSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	_, err := Load("custom", nil)
	if err == nil {
		t.Fatal("Load(custom) succeeded with no overrides")
	}
	for _, want := range []string{"binary", "cli.overrides"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A fully declared custom profile must work with no built-in help at all —
// this is the escape hatch for an operator's own wrapper or a self-hosted
// gateway CLI.
func TestCustomWorksWhenFullyDeclared(t *testing.T) {
	t.Parallel()
	p, err := Load("custom", map[string]any{
		"binary":        "my-gateway-llm",
		"complete_args": []any{"--json"},
		"output":        "json",
		"text_paths":    []any{[]any{"answer"}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Binary != "my-gateway-llm" {
		t.Errorf("Binary = %q", p.Binary)
	}
}

// The rule the docs promise: maps merge key-wise, lists replace wholesale.
// An element-wise list merge would produce an argv neither side wrote, and
// the failure would look like a vendor bug rather than a config one.
func TestOverridesReplaceListsWholesale(t *testing.T) {
	t.Parallel()
	base, _ := Builtin("claude-code")
	if len(base.CompleteArgs) < 3 {
		t.Fatalf("the built-in profile has too few args to make this test meaningful: %v", base.CompleteArgs)
	}
	p, err := Load("claude-code", map[string]any{
		"complete_args": []any{"-p", "--json"},
		"env":           map[string]any{"EXTRA": "1"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(p.CompleteArgs, " "); got != "-p --json" {
		t.Errorf("complete_args = %q, want the override verbatim", got)
	}
	if p.Env["EXTRA"] != "1" {
		t.Errorf("env override lost: %v", p.Env)
	}
	// Untouched fields survive.
	if p.Binary != base.Binary {
		t.Errorf("binary = %q, want the built-in %q", p.Binary, base.Binary)
	}
}

// A typo in an override must fail validation, not be ignored until the first
// turn. This is the whole reason the merge round-trips through YAML with
// KnownFields rather than reflecting field by field.
func TestAnOverrideTypoIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{"complete_arg": []any{"-p"}})
	if err == nil {
		t.Fatal("a misspelled override field was accepted")
	}
	if !strings.Contains(err.Error(), "complete_arg") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// passthrough_env is forwarded BEFORE auth.mode is consulted, so a credential
// named there reaches every seat whatever the mode says — the exact
// metered-bill-on-a-flat-rate-plan failure auth.mode exists to prevent.
func TestAProfileMayNotPassThroughACredential(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "MY_SECRET", "DB_PASSWORD"} {
		_, err := Load("claude-code", map[string]any{"passthrough_env": []any{name}})
		if err == nil {
			t.Errorf("passthrough_env accepted %q", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for %q does not name it: %v", name, err)
		}
	}
	// And a genuine non-secret is still allowed, or the check is useless.
	if _, err := Load("claude-code", map[string]any{
		"passthrough_env": []any{"GOOGLE_CLOUD_PROJECT"},
	}); err != nil {
		t.Errorf("a non-credential passthrough was refused: %v", err)
	}
}

// Overriding one entry's profile must not rewrite the table every later
// entry reads — a shared map value would make the second provider inherit
// the first one's overrides.
func TestOverridesDoNotLeakIntoTheBuiltinTable(t *testing.T) {
	t.Parallel()
	if _, err := Load("codex", map[string]any{"binary": "/opt/custom/codex"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	fresh, err := Load("codex", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fresh.Binary == "/opt/custom/codex" {
		t.Error("one entry's override leaked into the built-in table")
	}
}

// A CLI with no model flag cannot honour a model, and silently ignoring one
// would run every phase on the CLI's default while the config said otherwise.
func TestAProfileWithoutAModelFlagRefusesAModel(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Key: "sub", Model: "sonnet", Agent: "custom", StateDir: t.TempDir(),
		Timeout: 1, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "x", "complete_args": []any{"-p"},
			"output": "text", "model_args": []any{},
		},
	})
	if err == nil {
		t.Fatal("a model was accepted by a profile with no model flag")
	}
	if !strings.Contains(err.Error(), "model_args") {
		t.Errorf("error %q does not name the field to declare", err)
	}
}
