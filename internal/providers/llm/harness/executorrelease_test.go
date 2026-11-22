package harness

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A RELEASE IS READ BY ITS LAST THREE NUMBERS, as the run's script reads it:
// a vendor's prefix is passed over, and a version naming no three numbers is
// taken for a current release, never refused.
//
// Mutation: compare the numbers as text, or refuse an unread version, and a
// case goes red.
func TestAReleaseIsHeldToAFloorByItsNumbers(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"codex-cli 0.171.0": true, "codex-cli 0.172.3": true, "1.0.0": true, "codex-cli 0.170.99": false,
		"0.99.0": false, "codex-cli dev": true, "": true,
	} {
		if got := releaseAtLeast(version, "0.171.0"); got != want {
			t.Errorf("releaseAtLeast(%q, 0.171.0) = %v, want %v", version, got, want)
		}
	}
}

// codexEntry is a codex entry whose binary is a stand-in exiting probe for
// its executor probe (`app-server --help`).
func codexEntry(t *testing.T, probe int) *Provider {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n[ \"$1\" = app-server ] && exit " + strconv.Itoa(probe) + "\necho 'codex-cli 0.171.0'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // the case's own stand-in
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{Key: "sub", Harness: "codex", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{"binary": bin}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// A DIRECT BOX'S CODEX IS HELD TO ITS RUNNER'S FLOOR AND PROBE, since a direct
// box runs this host's own; a box in another cell runs its image's, which its
// run's script holds to the floor, so there it is a line and no problem. A
// harness whose runner states neither reports nothing here.
//
// Mutation: drop the floor's or the probe's check, or raise either for a
// remote cell, and a case goes red.
func TestADirectBoxsCodexIsHeldToItsRunnersFloorAndProbe(t *testing.T) {
	t.Parallel()
	direct := DiagnoseOptions{ExecutorSeats: []string{"swe"}, ExecutorHarnesses: []string{"codex"}, Cell: directCell}
	for name, tc := range map[string]struct {
		probe   int
		version string
		opts    DiagnoseOptions
		problem string
	}{
		"at the floor, with an app-server": {0, "codex-cli 0.171.0", direct, ""},
		"below the floor":                  {0, "codex-cli 0.170.2", direct, "drives 0.171.0 or later"},
		"no app-server":                    {1, "codex-cli 0.171.0", direct, "app-server --help` exited 1"},
		"a remote cell": {1, "codex-cli 0.150.0", DiagnoseOptions{ExecutorSeats: []string{"swe"},
			ExecutorHarnesses: []string{"codex"}, Cell: "e2b"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			line, problem := codexEntry(t, tc.probe).executorRelease(t.Context(), tc.opts, tc.version)
			if line == "" {
				t.Error("the executor check says nothing of the release")
			}
			if (tc.problem == "" && problem != "") || (tc.problem != "" && !strings.Contains(problem, tc.problem)) {
				t.Errorf("the problem is %q, want one saying %q", problem, tc.problem)
			}
		})
	}
	claude := doctorEntry(t, "claude-code")
	if line, problem := claude.executorRelease(t.Context(), DiagnoseOptions{ExecutorSeats: []string{"swe"},
		ExecutorHarnesses: []string{"claude-code"}, Cell: directCell}, "2.1.310"); line != "" || problem != "" {
		t.Errorf("a runner stating no floor reports %q, %q", line, problem)
	}
}
