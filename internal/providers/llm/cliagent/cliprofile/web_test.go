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

// A WEB DENIAL IS THE SEATS' ARGV WITH THE WEB TAKEN OUT, AND NOTHING ELSE.
// A denial restates the whole command line, so a confinement flag added to the
// seats' argv and not to the denial would be dropped from every entry that
// turns its web off, the assistant's among them. Each built-in denial is held
// to its seats' argv with exactly its web arguments changed.
func TestAWebDenialKeepsEverySeatArgumentButTheWeb(t *testing.T) {
	t.Parallel()
	webOff := map[string]func([]string) []string{
		"codex": func(seat []string) []string {
			out := slices.Clone(seat)
			out[slices.Index(out, `web_search="live"`)] = `web_search="disabled"`
			return out
		},
		"claude-code": func(seat []string) []string {
			var out []string
			for i := 0; i < len(seat); i++ {
				switch {
				case seat[i] == "--tools":
					out = append(out, "--tools", "")
					i++
				case seat[i] == "--allowedTools":
					i += 2
				default:
					out = append(out, seat[i])
				}
			}
			return append(out, "WebFetch", "WebSearch")
		},
		"pi": slices.Clone[[]string],
	}
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if p.WebDenial == nil || p.WebDenial.CompleteArgs == nil {
			continue
		}
		off, ok := webOff[name]
		if !ok {
			t.Errorf("%s: its web denial restates the argv, and this test does not say how the two may differ", name)
			continue
		}
		if want := off(p.CompleteArgs); !slices.Equal(p.WebDenial.CompleteArgs, want) {
			t.Errorf("%s: web_denial.complete_args = %q\nwant the seats' argv with the web taken out: %q",
				name, p.WebDenial.CompleteArgs, want)
		}
	}
}

// AND A SEEDED DENIAL IS THE SEATS' POLICY WITH ITS WEB RULES TAKEN OUT.
// OpenCode turns its web off in the seeded policy file rather than on argv, so
// its denial restates that file, and is held to it the same way: every other
// setting the seats' file carries is in the denial's, byte for byte.
func TestASeededWebDenialKeepsEverySeatSettingButTheWeb(t *testing.T) {
	t.Parallel()
	const allows = `,{"action":"webfetch","resource":"*","effect":"allow"},{"action":"websearch","resource":"*","effect":"allow"}`
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if p.WebDenial == nil || p.WebDenial.SeedFiles == nil {
			continue
		}
		if name != "opencode" {
			t.Errorf("%s: its web denial restates its seed files, and this test does not say how the two may differ", name)
			continue
		}
		if len(p.SeedFiles) != len(p.WebDenial.SeedFiles) {
			t.Fatalf("%s: %d seat files, %d denied ones", name, len(p.SeedFiles), len(p.WebDenial.SeedFiles))
		}
		for i, seat := range p.SeedFiles {
			denied := p.WebDenial.SeedFiles[i]
			if !strings.Contains(seat.Content, allows) {
				t.Fatalf("%s: the seats' policy no longer allows the web as this test reads it:\n%s", name, seat.Content)
			}
			if want := strings.Replace(seat.Content, allows, "", 1); denied.Path != seat.Path ||
				denied.In != seat.In || denied.Content != want {
				t.Errorf("%s: the denied %s is\n%s\nwant the seats' policy without its web rules:\n%s",
					name, denied.Path, denied.Content, want)
			}
		}
	}
}

// A RUN WITH TOOLS IS DENIED THE WEB TOO. A profile whose mcp block runs its
// own argv or writes its own settings hands a run with tools those in place of
// the profile's, so a denial covers them: Codex's run with tools turns its
// search off, OpenCode's run policy drops the two web actions, and a denial
// that replaces the argv while the run with tools keeps its own is refused.
//
// Mutation: drop the mcp block from withWebDenied, or the coverage check from
// validateWebStance, and this is red.
func TestAWebDenialCoversARunWithTools(t *testing.T) {
	t.Parallel()
	codex, err := Load("codex", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatal(err)
	}
	if args := codex.MCP.CompleteArgs; !slices.Contains(args, `web_search="disabled"`) ||
		slices.Contains(args, `web_search="live"`) {
		t.Errorf("codex's run with tools keeps the web: %q", args)
	}
	opencode, err := Load("opencode", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range opencode.MCP.Seed {
		lowered := strings.ToLower(seed.Content)
		if strings.Contains(lowered, "webfetch") || strings.Contains(lowered, "websearch") {
			t.Errorf("opencode's run with tools still allows the web in %s:\n%s", seed.Path, seed.Content)
		}
		if !strings.Contains(seed.Content, `"crewlet_*"`) {
			t.Errorf("opencode's denied run policy lost the crewlet tools in %s", seed.Path)
		}
	}
	_, err = Load("codex", map[string]any{"web_tools": "denied",
		"web_denial": map[string]any{"mcp": nil}})
	if err == nil || !strings.Contains(err.Error(), "mcp.complete_args") {
		t.Errorf("a denial that leaves a run with tools its own argv loaded: %v", err)
	}
}

// NO TOOL OF ITS OWN STILL HOLDS WITH THE ENGINE'S SERVER IN THE RUN: the call's
// own argv turns every built-in tool off, and the mcp block's allow list may
// name only the engine's tools, so an override that allows anything else, or
// brings an allow list into the call's own arguments, is not off.
//
// Mutation: drop mcpAllowProblem from ToolsOffProblem and the widened allow
// list passes.
func TestToolsOffAdmitsTheEnginesToolsAlone(t *testing.T) {
	t.Parallel()
	denied := map[string]any{"web_tools": "denied"}
	claude, err := Load("claude-code", denied)
	if err != nil {
		t.Fatal(err)
	}
	if problem := claude.ToolsOffProblem(); problem != "" {
		t.Fatalf("the built-in denied argv is not off: %s", problem)
	}
	widened, err := Load("claude-code", map[string]any{"web_tools": "denied",
		"mcp": map[string]any{"allow_args": []any{"--allowedTools", "mcp__crewlet__*", "Bash"}}})
	if err != nil {
		t.Fatal(err)
	}
	if problem := widened.ToolsOffProblem(); !strings.Contains(problem, `"Bash"`) {
		t.Errorf("an allow list naming Bash passed as off: %q", problem)
	}
	noMCP := claude.Clone()
	noMCP.MCP = nil
	if problem := noMCP.ToolsOffProblem(); !strings.Contains(problem, "no mcp block") {
		t.Errorf("a profile the engine's tools cannot reach passed as off: %q", problem)
	}
}
