package cliprofile

import (
	"slices"
	"strings"
	"testing"
)

// SEATS KEEP THE WEB. A built-in loaded with no overrides, or with overrides
// that say nothing about web, runs the argv and seed files it always ran: the
// stance is something an entry opts into, never a default a release flips.
func TestSeatsKeepTheWebByDefault(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		builtin, _ := Builtin(name)
		if builtin.WebTools != "" {
			t.Errorf("%s: the built-in sets web_tools %q; the stance is an entry's to choose", name, builtin.WebTools)
		}
		if name == "custom" {
			continue
		}
		loaded, err := Load(name, map[string]any{"env": map[string]any{"CREWLET_PROBE": "1"}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if loaded.EffectiveWebTools() != WebToolsAllowed {
			t.Errorf("%s: stance %q, want allowed", name, loaded.EffectiveWebTools())
		}
		if !slices.Equal(loaded.CompleteArgs, builtin.CompleteArgs) || !slices.Equal(loaded.SeedFiles, builtin.SeedFiles) {
			t.Errorf("%s: an entry that said nothing about web runs a different command line", name)
		}
	}
}

// A DENIAL REPLACES WHAT IT NAMES AND NOTHING ELSE. Claude Code's denial is
// an argv and leaves its (absent) seed files alone; OpenCode's is a seeded
// policy and leaves its argv alone.
func TestAWebDenialReplacesOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	denied := map[string]any{"web_tools": "denied"}

	claude, err := Load("claude-code", denied)
	if err != nil {
		t.Fatal(err)
	}
	builtin, _ := Builtin("claude-code")
	if !slices.Equal(claude.CompleteArgs, builtin.WebDenial.CompleteArgs) {
		t.Errorf("claude-code argv = %q, want the denial's", claude.CompleteArgs)
	}

	opencode, err := Load("opencode", denied)
	if err != nil {
		t.Fatal(err)
	}
	seat, _ := Builtin("opencode")
	if !slices.Equal(opencode.CompleteArgs, seat.CompleteArgs) {
		t.Errorf("opencode argv moved: %q, want %q", opencode.CompleteArgs, seat.CompleteArgs)
	}
	if !slices.Equal(opencode.SeedFiles, seat.WebDenial.SeedFiles) {
		t.Errorf("opencode seed files = %+v, want the denial's", opencode.SeedFiles)
	}
}

// A STANCE NOTHING HONOURS IS REFUSED. `web_tools: denied` on a profile with
// no measured web_denial would run a CLI that still fetches under a
// configuration saying it cannot, and the assistant is admitted on exactly
// that saying.
func TestWebDeniedIsRefusedOnAProfileWithNoDenial(t *testing.T) {
	t.Parallel()
	_, err := Load("gemini-cli", map[string]any{"web_tools": "denied"})
	if err == nil || !strings.Contains(err.Error(), "no web_denial") {
		t.Fatalf("err = %v, want a refusal naming web_denial", err)
	}
	if !strings.Contains(err.Error(), "has not been measured") {
		t.Errorf("the refusal does not carry the profile's own note: %v", err)
	}
}

// AN OVERRIDE THE DENIAL WOULD DISCARD IS REFUSED, naming the field to edit
// instead: applied either way round, one of the two things the operator wrote
// would never run.
func TestAnOverrideTheWebDenialWouldDiscardIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{
		"web_tools":     "denied",
		"complete_args": []any{"-p", "--output-format", "json"},
	})
	if err == nil || !strings.Contains(err.Error(), "override web_denial.complete_args instead") {
		t.Fatalf("err = %v, want the refusal naming web_denial.complete_args", err)
	}
	_, err = Load("opencode", map[string]any{
		"web_tools":  "denied",
		"seed_files": []any{map[string]any{"path": ".crewlet-config/opencode.json", "in": "work", "content": "{}"}},
	})
	if err == nil || !strings.Contains(err.Error(), "override web_denial.seed_files instead") {
		t.Fatalf("err = %v, want the refusal naming web_denial.seed_files", err)
	}
	// The same complete_args beside a stance that keeps web on is an
	// ordinary override.
	if _, err := Load("claude-code", map[string]any{"complete_args": []any{"-p", "--output-format", "json"}}); err != nil {
		t.Errorf("an argv override with web allowed was refused: %v", err)
	}
}

// A BROKEN DENIAL IS REFUSED WHERE IT IS WRITTEN, not on the first entry that
// turns web off: a profile that keeps web on but carries a web_denial has the
// denial validated as the profile it would become.
func TestABrokenWebDenialIsRefusedWhereItIsWritten(t *testing.T) {
	t.Parallel()
	_, err := Load("opencode", map[string]any{"web_denial": map[string]any{
		"seed_files": []any{map[string]any{"path": "../escape.json", "in": "work", "content": "{}"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "with web_tools denied, seed_files[0].path") {
		t.Fatalf("err = %v, want the denial's seed path refused", err)
	}
}

// The stance and the denial are each held to their own shape.
func TestTheWebStanceIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"unknown stance": {map[string]any{"web_tools": "off"}, `web_tools "off" (want allowed or denied)`},
		"empty denial":   {map[string]any{"web_tools": "denied", "web_denial": map[string]any{}}, "web_denial sets neither"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// custom has no denial of its own, so the override is
			// the whole of it.
			overrides := map[string]any{"binary": "x", "complete_args": []any{"x"}, "output": "text"}
			for k, v := range tc.overrides {
				overrides[k] = v
			}
			_, err := Load("custom", overrides)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// A clone owns its denial: an entry's override of the denial must never
// reach the table every later entry reads.
func TestACloneOwnsItsWebDenial(t *testing.T) {
	t.Parallel()
	a, _ := Builtin("claude-code")
	a.WebDenial.CompleteArgs[0] = "--mutated"
	b, _ := Builtin("claude-code")
	if b.WebDenial.CompleteArgs[0] == "--mutated" {
		t.Fatal("a profile's web_denial is shared with the built-in table")
	}
	o, _ := Builtin("opencode")
	if o.WebDenial.CompleteArgs != nil {
		t.Errorf("an unset denial field came back set: %q", o.WebDenial.CompleteArgs)
	}
}
