package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// THE GROK PROFILE'S ARGV MUST PARSE, and only the real binary can say so: on
// both system-prompt channels, with the transcript in a file, and every tool
// name it passes known to the harness.
//
// `-p` takes the prompt as its VALUE; with it in complete_args the model flag
// became the prompt and every call exited on "a value is required for
// '--single <PROMPT>'". The fake harness accepts any argv and a unit test over
// the slice only restates the profile, so nothing else could catch it. A name
// --tools does not know makes the harness drop the whole allowlist with a warning.
//
// Skipped unless a `grok` is on PATH, and asserts on ARGUMENT PARSING alone: the
// harness stopping for want of a credential proves the flags were understood.
func TestTheGrokProfileArgvParsesAgainstTheRealHarness(t *testing.T) {
	t.Parallel()
	binary, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("no grok on PATH")
	}
	p, ok := harnessprofile.Builtin("grok")
	if !ok {
		t.Fatal("no built-in grok profile")
	}
	// IS THIS xAI'S GROK, and not the npm package `grok@0.0.4`, which would be
	// argued with about flags it has never heard of. Judged on the SHAPE of the
	// version, never its MAJOR: `HasPrefix(out, "grok 1.")` goes quiet the day
	// xAI ships 2.0, with a skip message saying the opposite of what happened.
	// THE PROBE RUNS UNDER THE SAME FILTERED ENVIRONMENT as the call below: a bare
	// exec.Command leaves cmd.Env nil, which hands the child THIS PROCESS'S WHOLE
	// ENVIRONMENT, every key and token, before any identity check had run.
	probeDir := t.TempDir()
	probe := exec.CommandContext(t.Context(), binary, "--version")
	probe.Dir = probeDir
	probe.Env = vendorHarnessEnv(p, probeDir, nil)
	out, err := probe.Output()
	version := strings.TrimSpace(string(out))
	// THE BUILD ID IS THE PROVENANCE, not the major number and not a bare
	// semver. `grok 1.` was a time bomb — the day xAI ships 2.0 the right harness
	// is on PATH and this goes quiet. But a bare `^grok \d+\.\d+\.\d+`
	// is worse in the other direction: the npm impostor is grok@0.0.4, which
	// that shape ADMITS while the old major check excluded it. xAI's own
	// prints its commit in parentheses — `grok 1.0.30 (04b7ffed98c6)` — and
	// requiring that rejects both a future major going quiet and a same-named
	// package being argued with about flags it has never heard of.
	if err != nil || !regexp.MustCompile(`^grok \d+\.\d+\.\d+ \(`).MatchString(version) {
		t.Skipf("this is not xAI's own grok (%q) — the npm package of the same "+
			"name is a different program", version)
	}
	t.Logf("against %s", version)

	for field, channel := range map[string]harnessprofile.Channel{
		"system_prompt":        p.SystemPrompt,
		"append_system_prompt": p.AppendSystemPrompt,
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			// ONE directory for both the home and the working directory,
			// and the working directory matters as much as the
			// environment: a vendor harness reads AGENTS.md / CLAUDE.md from
			// its cwd, and this repository has both. Left in the checkout,
			// the probe answers with whatever the tree happens to contain
			// rather than about the shipped profile.
			dir := t.TempDir()
			prompt := filepath.Join(dir, "crewlet-prompt.txt")
			if err := os.WriteFile(prompt, []byte("## user\n\nsay hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// In the order a call assembles them: the profile's own
			// flags, the model, the channel, the prompt flag last.
			args := append([]string(nil), p.CompleteArgs...)
			for _, a := range p.ModelArgs {
				args = append(args, strings.ReplaceAll(a, "{model}", "grok-4-latest"))
			}
			if len(channel.Args) == 0 {
				t.Fatalf("the grok profile declares no %s", field)
			}
			for _, a := range channel.Args {
				args = append(args, strings.ReplaceAll(a, "{system}", "You are Agent CTO."))
			}
			for _, a := range p.PromptArgs {
				args = append(args, strings.ReplaceAll(a, "{file}", prompt))
			}

			cmd := exec.Command(binary, args...) //nolint:gosec // args come from the shipped profile
			cmd.Dir = dir
			cmd.Env = vendorHarnessEnv(p, dir, nil)
			combined, _ := cmd.CombinedOutput()
			if strings.Contains(string(combined), "names no tool") {
				t.Fatalf("the harness does not know a tool name the profile passes — an unknown "+
					"--tools entry drops the whole allowlist:\nargs: %v\n%s", args, combined)
			}
			assertArgvReachedAuth(t, args, string(combined))
		})
	}
}

// vendorHarnessEnv is the environment a real-harness case runs a vendor binary in.
//
// IT IS buildEnv, production's own, with a Checkout rooted at the case's
// temporary directory and a ZERO Auth: an argv test under another environment
// tests a different call. A hand-composed one drifted: a fresh HOME isolates a
// credential FILE but not an exported key (a billed completion), and a profile
// declaring a variable no case remembered ran a call the engine never makes.
//
// A ZERO Auth sets no token and buildEnv DELETES p.APIKeyEnv, so the child stops
// at authentication (assertArgvReachedAuth's proof) even on a signed-in machine.
// Callers set cmd.Dir to that fresh directory: a harness reads AGENTS.md /
// CLAUDE.md from its working directory.
func vendorHarnessEnv(p harnessprofile.Profile, home string, extra map[string]string) []string {
	// Cache beside the home rather than in it, as Workspace.Acquire lays it
	// out; Work is unread by buildEnv and set for the same reason.
	c := &Checkout{
		Home:  home,
		Cache: filepath.Join(home, "cache"),
		Work:  home,
	}
	return buildEnv(p, c, extra, Auth{})
}

// assertArgvReachedAuth fails unless a vendor harness got past argument parsing.
//
// REACHING AUTHENTICATION IS THE PROOF, checked FIRST: a harness asking for a
// credential has parsed every flag before it. Checking for argument-error
// strings that must be ABSENT cannot fail: it passes when the harness prints
// nothing, something new, or a reworded parser message (muse's `exec` parser is
// not clap, whose vocabulary the sentinels are).
//
// The auth markers are deliberately broad and case-folded: what is proved is "it
// got as far as credentials", not any vendor's wording, and a narrow match would
// turn a reworded message into a failure that reads like an argv bug.
func assertArgvReachedAuth(t *testing.T, args []string, got string) {
	t.Helper()

	lower := strings.ToLower(got)
	// REFUSALS FIRST. The auth markers used to be checked first, on the
	// reasoning that reaching authentication proves the flags parsed — but a
	// PARSER ERROR can quote the flag it choked on, and the flags these
	// profiles pass are named after credentials. "unexpected argument
	// --credential" contains "credential", so a malformed profile was read as
	// proof that it worked. The refusal is the stronger signal and is decided
	// before anything can be mistaken for success.
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
	for _, marker := range []string{
		"signed in", "sign in", "log in", "login",
		"credential", "api key", "api_key", "apikey",
		"authenticate", "unauthorized", "not authorized",
	} {
		if strings.Contains(lower, marker) {
			return
		}
	}
	t.Fatalf("the harness neither asked for a credential nor refused the argv, so "+
		"this proves nothing about the profile. Either it now runs without "+
		"one — in which case this case is spending a plan and must stop — or "+
		"it stopped somewhere new and the profile needs re-reading against "+
		"it:\nargs: %v\n%s", args, got)
}

// EVERY BUILT-IN PROFILE'S ENVIRONMENT REACHES THE CHILD, over the whole table,
// not the profiles that happen to have a real-harness case. Those cases SKIP
// wherever the vendor binary is absent (every CI runner), so the code that
// composes their environment was never run, and a profile gaining a variable
// nobody copied produced no failure. This case runs unconditionally, needs no
// binary, and fails when vendorHarnessEnv stops deriving what buildEnv derives.
//
// Driven off BuiltinNames, not a list a profile's author must remember to edit.
func TestVendorHarnessEnvCarriesEveryProfilesOwnEnvironment(t *testing.T) {
	t.Parallel()

	for _, name := range harnessprofile.BuiltinNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, ok := harnessprofile.Builtin(name)
			if !ok {
				t.Fatalf("Builtin(%q) missing from a table BuiltinNames just listed", name)
			}

			home := t.TempDir()
			got := map[string]string{}
			for _, kv := range vendorHarnessEnv(p, home, nil) {
				k, v, found := strings.Cut(kv, "=")
				if !found {
					t.Fatalf("environment entry %q is not KEY=VALUE", kv)
				}
				got[k] = v
			}

			// The fixed environment: an auto-update switch, an offline flag,
			// a retry cap. Each one stops the harness doing something a test must
			// not provoke, so a missing one is a test that makes a call the
			// engine never makes.
			for k, want := range p.Env {
				if got[k] != want {
					t.Errorf("%s = %q, want %q — the profile declares it and buildEnv sets it, "+
						"so the test environment must carry it too", k, got[k], want)
				}
			}

			// The relocation variables, joined onto the home exactly as
			// buildEnv joins them. These were re-stated per case with the
			// path spelled out by hand, so profiles.yaml and the test each
			// held a copy of where a vendor keeps its state.
			for k, rel := range p.ConfigEnv {
				if want := filepath.Join(home, rel); got[k] != want {
					t.Errorf("%s = %q, want %q — the relocation must point inside the "+
						"case's own home, or the harness writes to the real one", k, got[k], want)
				}
			}

			// A zero Auth signs nothing in. The api-key variable is deleted
			// outright by buildEnv's default branch, which is what lets these
			// cases run on a machine already logged in to the vendor.
			if p.APIKeyEnv != "" {
				if _, present := got[p.APIKeyEnv]; present {
					t.Errorf("%s reached the child; a real-harness case must reach authentication "+
						"and stop, never spend a plan", p.APIKeyEnv)
				}
			}

			// HOME is the isolation everything else hangs off.
			if got["HOME"] != home {
				t.Errorf("HOME = %q, want %q", got["HOME"], home)
			}
		})
	}
}
