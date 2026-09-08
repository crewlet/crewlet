package cliagent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// THE GROK PROFILE'S ARGV MUST PARSE, and only the real binary can say so.
//
// `-p` takes the prompt as its VALUE. With it in complete_args, the model flag
// that follows became the prompt and the real prompt became a stray
// positional, so every call exited on "a value is required for
// '--single <PROMPT>'". Nothing in this repository could catch that: the fake
// CLI accepts any argv, and a unit test over the slice only re-states the
// profile back to itself.
//
// Skipped unless a `grok` is on PATH, and it asserts on ARGUMENT PARSING
// alone — the CLI stops at "Not signed in" without credentials, which is
// exactly far enough to prove the flags were understood.
func TestTheGrokProfileArgvParsesAgainstTheRealCLI(t *testing.T) {
	binary, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("no grok on PATH")
	}
	p, ok := Builtin("grok")
	if !ok {
		t.Fatal("no built-in grok profile")
	}
	out, err := exec.Command(binary, "--version").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "grok 1.") {
		t.Skipf("this is not xAI's own grok (%q) — the npm package of the same "+
			"name is a different program", strings.TrimSpace(string(out)))
	}

	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "grok-4-latest"))
	}
	for _, a := range p.SystemPromptArgs {
		args = append(args, strings.ReplaceAll(a, "{system}", "You are Agent CTO."))
	}
	args = append(args, p.PromptArgs...)
	args = append(args, "say hello")

	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	combined, _ := cmd.CombinedOutput()
	got := string(combined)

	// clap reports an argument problem before it reaches authentication.
	for _, refusal := range []string{
		"a value is required",
		"unexpected argument",
		"invalid value",
		"unrecognized",
		"error: ",
	} {
		if strings.Contains(got, refusal) {
			t.Fatalf("the profile's argv does not parse (%q):\nargs: %v\n%s",
				refusal, args, got)
		}
	}
	if !strings.Contains(got, "Not signed in") {
		t.Logf("the CLI got past argument parsing but said something unexpected:\n%s", got)
	}
}
