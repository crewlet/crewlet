package cliagent

import (
	"os/exec"
	"path/filepath"
	"regexp"
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
	// THE PROBE RUNS UNDER THE SAME FILTERED ENVIRONMENT as the call below.
	// It was a bare exec.Command, which leaves cmd.Env nil — and a nil Env
	// means os/exec hands the child THIS PROCESS'S WHOLE ENVIRONMENT. So the
	// one step whose entire purpose is deciding whether the binary on PATH is
	// even the right program was handing an unidentified executable every key
	// and token the environment carried, before any identity check had run.
	probeDir := t.TempDir()
	probe := exec.CommandContext(t.Context(), binary, "--version")
	probe.Dir = probeDir
	probe.Env = vendorCLIEnv(p, probeDir, nil)
	out, err := probe.Output()
	version := strings.TrimSpace(string(out))
	// THE BUILD ID IS THE PROVENANCE, not the major number and not a bare
	// semver. `grok 1.` was a time bomb — the day xAI ships 2.0 the right CLI
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

	args := append([]string(nil), p.CompleteArgs...)
	for _, a := range p.ModelArgs {
		args = append(args, strings.ReplaceAll(a, "{model}", "grok-4-latest"))
	}
	for _, a := range p.SystemPromptArgs {
		args = append(args, strings.ReplaceAll(a, "{system}", "You are Agent CTO."))
	}
	args = append(args, p.PromptArgs...)
	args = append(args, "say hello")

	// ONE directory for both the home and the working directory, and the
	// working directory matters as much as the environment: a vendor CLI
	// reads AGENTS.md / CLAUDE.md from its cwd, and this repository has both.
	// Left in the checkout, the probe answers with whatever the tree happens
	// to contain rather than about the shipped profile — the kimi, pi and
	// hermes cases were already right about this where this one was not.
	dir := t.TempDir()
	cmd := exec.Command(binary, args...) //nolint:gosec // args come from the shipped profile
	cmd.Dir = dir
	cmd.Env = vendorCLIEnv(p, dir, nil)
	combined, _ := cmd.CombinedOutput()
	assertArgvReachedAuth(t, args, string(combined))
}

// vendorCLIEnv is the environment a real-CLI case runs a vendor binary in.
//
// IT IS buildEnv — production's own, with a Checkout rooted at the case's
// temporary directory and a ZERO Auth. Not an approximation of it, and that is
// the whole point: an argv test running under a different environment than
// production is testing a different call, so the only environment that cannot
// drift from the real one is the real one.
//
// This started as `append(os.Environ(), "HOME="+t.TempDir())`, which was wrong
// twice — it resembled nothing the engine hands a CLI, and a fresh HOME
// isolates a credential FILE while doing nothing about an exported one, so on
// any machine with a vendor key in the environment the case signed in and
// `say hello` became a real, billed completion. Re-composing hostAllowlist and
// HOME by hand fixed that and left the SAME shape of bug one layer in: each
// case then named the one or two profile variables it remembered, so a profile
// declaring a third had a test running a call the engine never makes. kimi-code
// declares KIMI_CODE_NO_AUTO_UPDATE, KIMI_CODE_BACKGROUND_PRINT_BACKGROUND_MODE
// and KIMI_LOOP_MAX_ATTEMPTS_PER_STEP; the case carried none of them, so it
// could trigger the vendor's updater mid-run and wait out its default retry
// policy in full. Deriving the whole environment is what ends that class.
//
// A ZERO Auth is what keeps the promise the cases make. buildEnv's default
// branch sets no token (auth.Token is empty) and DELETES p.APIKeyEnv outright,
// so the child reaches authentication and stops — which is exactly what
// assertArgvReachedAuth reads as proof, and it is why these cases can be run on
// a machine that is signed in to the vendor.
//
// Callers also set cmd.Dir to that same fresh directory. A vendor CLI reads
// AGENTS.md / CLAUDE.md from its working directory, and this repository has
// both — so a probe left in the checkout is answering with whatever the tree
// happens to contain rather than about the shipped profile.
func vendorCLIEnv(p Profile, home string, extra map[string]string) []string {
	// Cache beside the home rather than in it, as Workspace.Acquire lays it
	// out; Work is unread by buildEnv and set for the same reason.
	c := &Checkout{
		Home:  home,
		Cache: filepath.Join(home, "cache"),
		Work:  home,
	}
	return buildEnv(p, c, extra, Auth{})
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
	t.Fatalf("the CLI neither asked for a credential nor refused the argv, so "+
		"this proves nothing about the profile. Either it now runs without "+
		"one — in which case this case is spending a plan and must stop — or "+
		"it stopped somewhere new and the profile needs re-reading against "+
		"it:\nargs: %v\n%s", args, got)
}

// EVERY BUILT-IN PROFILE'S ENVIRONMENT REACHES THE CHILD, over the whole
// table rather than over the profiles that happen to have a real-CLI case.
//
// This is the guard the hand-written helper did not have, and its absence is
// why the drift lasted. The real-CLI cases SKIP wherever the vendor binary is
// not installed, which is every CI runner and nearly every workstation — so
// the code path that composes their environment was compiled and never run,
// and a profile gaining a variable nobody copied across produced no failure
// anywhere. This case runs unconditionally, needs no binary, and fails the
// moment vendorCLIEnv stops deriving what buildEnv derives.
//
// Driven off BuiltinNames rather than a list, so a profile added tomorrow is
// covered by existing code — the alternative is a list that has to be edited
// by whoever adds the profile, which is the same shape of omission one level
// up.
func TestVendorCLIEnvCarriesEveryProfilesOwnEnvironment(t *testing.T) {
	t.Parallel()

	for _, name := range BuiltinNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, ok := Builtin(name)
			if !ok {
				t.Fatalf("Builtin(%q) missing from a table BuiltinNames just listed", name)
			}

			home := t.TempDir()
			got := map[string]string{}
			for _, kv := range vendorCLIEnv(p, home, nil) {
				k, v, found := strings.Cut(kv, "=")
				if !found {
					t.Fatalf("environment entry %q is not KEY=VALUE", kv)
				}
				got[k] = v
			}

			// The fixed environment: an auto-update switch, an offline flag,
			// a retry cap. Each one stops the CLI doing something a test must
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
						"case's own home, or the CLI writes to the real one", k, got[k], want)
				}
			}

			// A zero Auth signs nothing in. The api-key variable is deleted
			// outright by buildEnv's default branch, which is what lets these
			// cases run on a machine already logged in to the vendor.
			if p.APIKeyEnv != "" {
				if _, present := got[p.APIKeyEnv]; present {
					t.Errorf("%s reached the child; a real-CLI case must reach authentication "+
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
