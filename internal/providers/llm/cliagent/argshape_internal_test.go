package cliagent

import (
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
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
// alone — the CLI stops for want of a credential, which is exactly far enough
// to prove the flags were understood.
func TestTheGrokProfileArgvParsesAgainstTheRealCLI(t *testing.T) {
	binary, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("no grok on PATH")
	}
	p, ok := Builtin("grok")
	if !ok {
		t.Fatal("no built-in grok profile")
	}
	// IS THIS xAI'S GROK, and not the npm package of the same name — which is
	// real, is `grok@0.0.4` ("do you grok it?"), and would otherwise be argued
	// with about flags it has never heard of.
	//
	// ON THE SHAPE OF THE VERSION, never on its MAJOR. This read
	// `HasPrefix(out, "grok 1.")`, which is a silent time bomb: the day xAI
	// ships 2.0 the right CLI is on PATH, this case goes quiet, and the skip
	// message says the opposite of what happened — the one shape "a skip is
	// not a pass" exists to catch. The impostor is distinguished by its
	// OUTPUT FORM, which is what was actually meant.
	out, err := exec.Command(binary, "--version").Output()
	version := strings.TrimSpace(string(out))
	if err != nil || !regexp.MustCompile(`^grok \d+\.\d+\.\d+`).MatchString(version) {
		t.Skipf("this is not xAI's own grok (%q) — the npm package of the same "+
			"name is a different program", version)
	}
	t.Logf("against %s", version)

	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "grok-4-latest"))
	}
	for _, a := range p.SystemPromptArgs {
		args = append(args, strings.ReplaceAll(a, "{system}", "You are Agent CTO."))
	}
	args = append(args, p.PromptArgs...)
	args = append(args, "say hello")

	cmd := exec.Command(binary, args...) //nolint:gosec // args come from the shipped profile
	cmd.Env = vendorCLIEnv(t.TempDir(), nil)
	combined, _ := cmd.CombinedOutput()
	assertArgvReachedAuth(t, args, string(combined))
}

// vendorCLIEnv is the environment a real-CLI case runs a vendor binary in.
//
// THE PRODUCTION ALLOWLIST, not os.Environ(). This case built its child
// environment as `append(os.Environ(), "HOME="+t.TempDir())`, which is wrong
// twice. It does not resemble what the engine actually hands a CLI — buildEnv
// composes hostAllowlist plus the isolation, and an argv test running under a
// different environment than production is testing a different call. And a
// fresh HOME isolates a credential FILE while doing nothing about an exported
// one, so on any machine with a vendor key in the environment this case
// signed in and `say hello` became a real, billed completion — on a test whose
// own comment promises it asserts "on ARGUMENT PARSING alone".
//
// Callers also set cmd.Dir to that same fresh directory. A vendor CLI reads
// AGENTS.md / CLAUDE.md from its working directory, and this repository has
// both — so a probe left in the checkout is answering with whatever the tree
// happens to contain rather than about the shipped profile.
func vendorCLIEnv(home string, extra map[string]string) []string {
	env := map[string]string{}
	for _, name := range hostAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
	env["HOME"] = home
	maps.Copy(env, extra)

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// assertArgvReachedAuth fails unless a vendor CLI got past argument parsing.
//
// REACHING AUTHENTICATION IS THE PROOF, and it is checked FIRST. A CLI that
// asks for a credential has already parsed every flag before it, so nothing
// else needs deciding — and testing it this way round is what makes the case
// able to fail at all. Both real-CLI cases were written as a list of argument
// error strings that must be ABSENT, which is a test that passes when the CLI
// prints nothing, when it prints something new, and when the vendor reworded
// its parser: the muse case's five sentinels are clap's vocabulary, and muse's
// own `exec` parser is not clap.
//
// The auth markers are deliberately broad and case-folded. What is being
// proved is "it got as far as credentials", not any vendor's wording, and a
// narrow match would turn a reworded message into a failure that reads like an
// argv bug.
func assertArgvReachedAuth(t *testing.T, args []string, got string) {
	t.Helper()

	lower := strings.ToLower(got)
	for _, marker := range []string{
		"signed in", "sign in", "log in", "login",
		"credential", "api key", "api_key", "apikey",
		"authenticate", "unauthorized", "not authorized",
	} {
		if strings.Contains(lower, marker) {
			return
		}
	}
	for _, refusal := range []string{
		"a value is required", "unexpected argument",
		"invalid value", "unrecognized", "unknown option",
		"unknown flag", "unknown argument", "error: unknown",
	} {
		if strings.Contains(lower, refusal) {
			t.Fatalf("the profile's argv does not parse (%q):\nargs: %v\n%s",
				refusal, args, got)
		}
	}
	t.Fatalf("the CLI neither asked for a credential nor refused the argv, so "+
		"this proves nothing about the profile. Either it now runs without "+
		"one — in which case this case is spending a plan and must stop — or "+
		"it stopped somewhere new and the profile needs re-reading against "+
		"it:\nargs: %v\n%s", args, got)
}
