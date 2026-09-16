package cliagent

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// usageProfile is the shape a usage-file profile has: a report written where
// the profile asked, read back through the ordinary usage paths.
func usageProfile() map[string]any {
	return map[string]any{
		// The "--" is for the FAKE: it is this test binary, and Go's
		// flag package would refuse --usage-file as an unknown test flag.
		"complete_args":   []any{"-test.run=TestCLIAgentFakeCLI", "--"},
		"usage_file_args": []any{"--usage-file", "{usage_file}"},
		"usage": map[string]any{
			"input":       []any{[]any{"input_tokens"}},
			"output":      []any{[]any{"output_tokens"}},
			"cache_read":  []any{[]any{"cache_read_tokens"}},
			"cache_write": []any{[]any{"cache_write_tokens"}},
		},
	}
}

// A CLI CAN REPORT ITS TOKENS HONESTLY AND STILL NOT PUT THEM IN THE ANSWER.
//
// The counts a budget is charged against must come from the vendor wherever
// the vendor has them. An entry point whose whole contract is "the final
// response text and nothing else" leaves no envelope for a usage path to
// walk, so the report rides a file instead — and it is read back through the
// same [UsagePaths] every other profile uses.
func TestAUsageFileReportsRealTokensWhereStdoutCannot(t *testing.T) {
	p := fakeProvider(t, map[string]string{
		"FAKE_STDOUT": "the answer",
		"FAKE_USAGE_FILE": `{"input_tokens":1200,"output_tokens":340,` +
			`"cache_read_tokens":800,"cache_write_tokens":64,` +
			`"reasoning_tokens":120,"total_tokens":1540,"completed":true}`,
	}, usageProfile())
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "the answer" {
		t.Fatalf("Content = %q — the report must not disturb the reply", comp.Content)
	}
	// InputTokens is ALWAYS the full prompt count, cache included: the
	// contract's rule, and what keeps it a correct budget figure.
	if want := 1200 + 800 + 64; comp.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d (the prompt count plus both cache halves)",
			comp.InputTokens, want)
	}
	if comp.OutputTokens != 340 {
		t.Errorf("OutputTokens = %d, want the reported 340", comp.OutputTokens)
	}
	if comp.CacheRead != 800 || comp.CacheWrite != 64 {
		t.Errorf("cache read/write = %d/%d, want 800/64", comp.CacheRead, comp.CacheWrite)
	}
}

// A REPORT THAT WAS NEVER WRITTEN IS NOT A CALL THAT COST NOTHING.
//
// The vendor writes it "even when the run fails", so its absence says the run
// did not get that far — and failing a completion the model answered, over a
// count, would throw away work the operator paid for. The counts fall back to
// the estimate, which is where a CLI reporting nothing already is.
func TestAMissingUsageFileFallsBackToTheEstimate(t *testing.T) {
	p := fakeProvider(t, map[string]string{
		"FAKE_STDOUT": "the answer",
		// "-" tells the fake to write no report at all.
		"FAKE_USAGE_FILE": "-",
	}, usageProfile())
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.InputTokens == 0 || comp.OutputTokens == 0 {
		t.Errorf("a run whose report never arrived must still be estimated, got in=%d out=%d",
			comp.InputTokens, comp.OutputTokens)
	}
}

// A REPORT THIS PROFILE CANNOT READ IS DRIFT, NOT A ZERO-TOKEN CALL.
//
// A vendor that renamed its keys would otherwise charge every turn zero and
// look exactly like a profile reporting real usage — the silent half of the
// same failure `located` exists to make loud for the answer.
func TestAUsageFileThatSaysNothingThisProfileReadsIsStillEstimated(t *testing.T) {
	for name, report := range map[string]string{
		"every key renamed": `{"promptTokens":1200,"completionTokens":340}`,
		// HALF a report is the sharper case: charging a real output
		// count against a prompt this build read as zero is worse than
		// estimating both, because it looks like a turn that cost
		// almost nothing.
		"only the output count": `{"output_tokens":340}`,
		"only the input count":  `{"input_tokens":1200}`,
		"not JSON at all":       `run ended with Failed`,
	} {
		t.Run(name, func(t *testing.T) {
			p := fakeProvider(t, map[string]string{
				"FAKE_STDOUT": "the answer", "FAKE_USAGE_FILE": report,
			}, usageProfile())
			comp, err := ask(t, p, llm.Request{})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if comp.InputTokens == 0 || comp.OutputTokens == 0 {
				t.Errorf("a report this profile cannot read must fall back to the "+
					"estimate rather than charging zero, got in=%d out=%d",
					comp.InputTokens, comp.OutputTokens)
			}
		})
	}
}

// A `text` profile normally cannot report real tokens — [extract] decodes no
// document in that mode, so declared paths are walked by nothing — and the
// usage FILE is the whole exception, because the CLI writes it to a path of
// its own. `crewlet llm doctor` must say so, or the profile that went to the
// trouble is the one reported as estimating.
func TestTheDoctorCallsAUsageFileWhatItIs(t *testing.T) {
	t.Parallel()
	hermes, ok := Builtin("hermes")
	if !ok {
		t.Fatal("no built-in hermes profile")
	}
	if hermes.output() != OutputText {
		t.Fatalf("output = %q — this case exists for the text profile that reads a "+
			"report anyway", hermes.output())
	}
	if !hermes.ReadsUsage() {
		t.Error("the doctor reports estimated tokens for a profile whose CLI writes " +
			"the real figures to --usage-file")
	}
	// And the rule it is an exception to still holds: a text profile with
	// no report to read estimates.
	copilot, _ := Builtin("copilot")
	if copilot.ReadsUsage() {
		t.Error("copilot has no usage report of any kind and must report estimated")
	}
}

// Neither half of the mechanism configures anything alone: a path with no
// placeholder writes the report nowhere, and a placeholder with no usage
// paths writes a report nothing reads — and both look exactly like a profile
// reporting real usage while the budget spends an estimate.
func TestAUsageFileWithNothingToSubstituteOrNothingToReadIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"no placeholder": {
			overrides: map[string]any{"usage_file_args": []any{"--usage-file"}},
			want:      "{usage_file}",
		},
		"no usage paths": {
			overrides: map[string]any{
				"usage_file_args": []any{"--usage-file", "{usage_file}"},
				"usage":           map[string]any{},
			},
			// The stricter rule this now falls under — both prompt
			// counts or none — is covered case by case in
			// markerscope_internal_test.go; declaring none at all is
			// still the plainest way to write a report nothing reads.
			want: "usage.input and usage.output",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// copilot, because it declares no usage paths of its own,
			// so the second case is not fighting a base profile.
			base := "copilot"
			if name == "no placeholder" {
				base = "claude-code"
			}
			_, err := Load(base, tc.overrides)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// THE HERMES PROFILE'S OWN CLAIMS, stated once so an edit that drops one has
// to argue with this rather than with nothing.
func TestTheHermesProfileDeniesItsToolsAndReadsItsRealTokens(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("hermes")
	if !ok {
		t.Fatal("no built-in hermes profile")
	}
	joined := strings.Join(p.CompleteArgs, " ")
	// --ignore-rules is the isolation: no AGENTS.md, SOUL.md,
	// .cursorrules, persistent memory or preloaded skills injected from a
	// host the engine does not control.
	if !strings.Contains(joined, "--ignore-rules") {
		t.Errorf("complete_args = %v, want --ignore-rules", p.CompleteArgs)
	}
	// NOT these two: both ignore ~/.hermes/config.yaml, which is where the
	// seeded toolset lives, so either would hand the run the full
	// hermes-cli tool surface back.
	for _, forbidden := range []string{"--ignore-user-config", "--safe-mode"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("complete_args carries %s, which ignores the seeded config.yaml "+
				"this profile's tool denial lives in", forbidden)
		}
	}
	// AND NOT --yolo, because `-z` already bypasses approvals — its own flag
	// help says so. There is no prompt for a headless run to wedge on, so
	// --yolo would widen what a run may do and buy nothing at all.
	if strings.Contains(joined, "--yolo") {
		t.Error("complete_args carries --yolo, but `-z` already auto-bypasses " +
			"approvals — this only widens what a run may do")
	}
	// `-p` selects a PROFILE on this CLI, not a prompt. `-z` is the
	// one-shot entry point and takes the prompt as its value, so it has to
	// be the last thing before it.
	if got := strings.Join(p.PromptArgs, " "); got != "-z" {
		t.Errorf("prompt_args = %v, want [-z]: `-p` on this CLI names a profile", p.PromptArgs)
	}
	if strings.Contains(joined, "-p ") || slices.Contains(p.CompleteArgs, "-p") {
		t.Error("complete_args carries -p, which selects a profile named after " +
			"whatever followed it")
	}
	if p.mode() != PromptArgv || p.output() != OutputText {
		t.Errorf("prompt_mode/output = %q/%q, want argv/text: `-z` is documented as "+
			"the final response text and nothing else", p.mode(), p.output())
	}

	// THE DENIAL IS ON ARGV. `--toolsets` is a TOP-LEVEL flag whose own help
	// says "Applies to -z/--oneshot", and passing it REPLACES the enabled set
	// for the invocation rather than merging with the config file's — the
	// CLI's own oneshot module states the rule ("Toolsets = explicit
	// --toolsets, else the user's `cli` toolsets"). `web` resolves to
	// web_search and web_extract and nothing else: no file, no terminal, no
	// code execution, no delegation.
	//
	// It reads like the seeded settings files this file's other profiles use,
	// and is deliberately not one: a flag fails at ARGUMENT PARSING when a
	// vendor renames it, where a settings key a vendor renamed is ignored and
	// the run quietly keeps every tool.
	if !allowsAfterFlag(p.CompleteArgs, "--toolsets", "web") {
		t.Errorf("complete_args = %v, want `--toolsets web`: without it every seat "+
			"runs the full hermes-cli surface — a shell, a file editor and code "+
			"execution on the engine host", p.CompleteArgs)
	}
	if len(p.SeedFiles) > 0 {
		t.Errorf("hermes seeds %d file(s), but its denial is on argv — a second, "+
			"weaker copy of one policy is one more thing to keep in step",
			len(p.SeedFiles))
	}

	// The counts come from the file, under the vendor's documented key
	// names — and NOT from reasoning_tokens, which is a subset of the
	// output count on every vendor that reports both.
	if len(p.UsageFileArgs) == 0 {
		t.Fatal("hermes declares no usage_file_args, so every turn's spend is an " +
			"estimate although the CLI writes the real figures")
	}
	for field, want := range map[string]string{
		PathList(p.Usage.Input):      "input_tokens",
		PathList(p.Usage.Output):     "output_tokens",
		PathList(p.Usage.CacheRead):  "cache_read_tokens",
		PathList(p.Usage.CacheWrite): "cache_write_tokens",
	} {
		if field != want {
			t.Errorf("usage path %q, want %q", field, want)
		}
	}
	if strings.Contains(strings.Join([]string{
		PathList(p.Usage.Input), PathList(p.Usage.Output),
	}, " "), "reasoning_tokens") {
		t.Error("reasoning_tokens is read, and it is a SUBSET of output_tokens — " +
			"adding it double-charges a thinking model's budget")
	}
}

// THE HERMES PROFILE'S ARGV MUST PARSE, and only the real binary can say so.
//
// Its per-run overrides are documented AFTER the prompt (`hermes -z "…"
// --model X`) while this backend puts every flag BEFORE it, because `-z`
// takes the prompt as its value and nothing may come between them. Whether
// that order parses is a fact about the vendor's parser, not about this
// profile, and only the installed CLI can answer it.
//
// Skipped unless a `hermes` is on PATH.
func TestTheHermesProfileArgvParsesAgainstTheRealCLI(t *testing.T) {
	binary, err := exec.LookPath("hermes")
	if err != nil {
		t.Skip("no hermes on PATH")
	}
	p, ok := Builtin("hermes")
	if !ok {
		t.Fatal("no built-in hermes profile")
	}
	dir := t.TempDir()
	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "openrouter/auto"))
	}
	for _, a := range p.UsageFileArgs {
		args = append(args, strings.ReplaceAll(a, "{usage_file}", dir+"/usage.json"))
	}
	args = append(args, p.PromptArgs...)
	args = append(args, "say hello")

	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	// HERMES_HOME is the profile's config_env; see vendorCLIEnv.
	cmd.Env = vendorCLIEnv(p, dir, nil)
	combined, _ := cmd.CombinedOutput()
	assertArgvReachedAuth(t, args, string(combined))
}
