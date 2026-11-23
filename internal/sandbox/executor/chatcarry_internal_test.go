package executor

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// THE ENGINE, THE START GATE AND THE SESSION GATE READ ONE RELEASE OFF ONE
// PRINT. For each `--version` print, read by the script's own [releaseRead]
// and run with sh: the engine continues a 0.70.0 session in the box
// ([releaseOf], [GeminiCLI.Continues]) exactly where the session gate's rv
// ([bareReleaseShell], [chatSessionGate]) is 0.70.0; a print the engine reads
// as 0.70.0 passes the start gate ([releaseGate]); older, pre-release and
// unreadable prints are refused there, newer ones run on sessions of their own.
//
// Mutations, each red: judge the start gate on v's leading digits, not its
// first digit-led word; strip no leading v, or no closing period, in rv; leave
// the carriage return in v; drop the pre-release check; judge the minor
// whatever the major.
func TestTheEngineAndBothGatesReadOneReleaseOffOnePrint(t *testing.T) {
	t.Parallel()
	script := releaseRead(`printf '%s' "$P"`) + "\n" +
		"( " + releaseGate("Gemini CLI", geminiRelease) + "\n) 2> /dev/null && g=starts || g=refused\n" +
		bareReleaseShell("rv") + `printf '%s %s\n' "$g" "$rv"`
	for _, tc := range []struct {
		print             string
		starts, continues bool
	}{
		{"0.70.0", true, true},
		{"0.70.0\r\n", true, true},
		{"v0.70.0", true, true},
		{"Gemini CLI 0.70.0", true, true},
		{" 0.70.0", true, true},
		{"0.70.0 (abc)", true, true},
		{"0.70.0.", true, true},
		{"0.71.0", true, false},
		{"Gemini CLI 0.71.0", true, false},
		{"0.70.0-nightly", false, false},
		{"0.70.1-rc.1", true, false},
		{"0.69.9", false, false},
		{"1.0.0", true, false},
		{"gemini nightly", false, false},
		{"", false, false},
	} {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script) //nolint:gosec // the script under test
		cmd.Env = append(os.Environ(), "P="+tc.print)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q: the script: %v", tc.print, err)
		}
		gate, rv, _ := strings.Cut(strings.TrimSuffix(string(out), "\n"), " ")
		release := releaseOf(tc.print)
		engine, _ := GeminiCLI{}.Continues(release, geminiRelease)
		if engine != tc.continues || (rv == geminiRelease) != engine {
			t.Errorf("%q: the engine reads %q and continues %v, the session gate reads %q; want both %v",
				tc.print, release, engine, rv, tc.continues)
		}
		if starts := gate == "starts"; starts != tc.starts || (release == geminiRelease && !starts) {
			t.Errorf("%q: the start gate %s it, want starts %v (the engine reads %q)", tc.print, gate,
				tc.starts, release)
		}
	}
}
