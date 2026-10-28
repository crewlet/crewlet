//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// thisEngineOOMScoreAdj is the test process's own oom_score_adj: it is the
// engine every box this suite starts descends from.
func thisEngineOOMScoreAdj(t *testing.T) int {
	t.Helper()
	engine, err := readOOMScoreAdj(selfOOMScoreAdjFile)
	if err != nil {
		t.Fatalf("this host does not report the engine's oom_score_adj: %v", err)
	}
	return engine
}

// requireAheadOfTheEngine fails t unless reported — a box process's
// oom_score_adj as the kernel gave it — is what a box runs at beside this
// engine, and above the engine's own wherever the ceiling leaves room.
func requireAheadOfTheEngine(t *testing.T, what, reported string) {
	t.Helper()
	engine := thisEngineOOMScoreAdj(t)
	got, err := strconv.Atoi(strings.TrimSpace(reported))
	if err != nil {
		t.Fatalf("%s reported oom_score_adj %q: %v", what, reported, err)
	}
	if want := boxOOMScoreAdj(engine); got != want {
		t.Fatalf("%s runs at oom_score_adj %d beside an engine at %d, want %d", what, got, engine, want)
	}
	if engine < oomScoreAdjMax && got <= engine {
		t.Fatalf("%s runs at oom_score_adj %d beside an engine at %d: the kernel can take the engine first",
			what, got, engine)
	}
}

// Every process a direct box runs goes before its engine when memory runs
// out: an exec or setup command and what it starts, and the detached job —
// the process the box records, and what it starts.
func TestEveryProcessADirectBoxRunsGoesBeforeItsEngine(t *testing.T) {
	t.Parallel()
	box := mustCreate(t, newDirect(t), Spec{})

	res, err := box.Exec(t.Context(), "cat /proc/self/oom_score_adj", ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec: %v (exit %d, stderr %q)", err, res.ExitCode, res.Stderr)
	}
	requireAheadOfTheEngine(t, "a command an exec started", res.Stdout)

	pid, err := box.StartBackground(t.Context(),
		"cat /proc/self/oom_score_adj > adj.tmp && mv adj.tmp adj && exec sleep 60", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	reported := filepath.Join(box.Home(), WorkspaceSubdir, "adj")
	var child []byte
	waitFor(t, 5*time.Second, func() bool {
		child, err = os.ReadFile(reported)
		return err == nil
	}, "the job never reported its oom_score_adj")
	requireAheadOfTheEngine(t, "a command the detached job started", string(child))

	// The job's own process — its leader, by the pid its record names — read
	// from the kernel while it sleeps.
	leader, err := os.ReadFile(filepath.Join("/proc", pid, "oom_score_adj"))
	if err != nil {
		t.Fatalf("reading the job's oom_score_adj: %v", err)
	}
	requireAheadOfTheEngine(t, "the detached job", string(leader))
}

// A confined box is raised BEFORE its confinement starts: Landlock leaves a
// confined box nothing to write but its home, /proc included, so a raise
// made inside it would fail. The helper here only reports what it was started
// at and becomes the command, as the real ones do once they have confined it.
func TestAConfinedBoxIsRaisedBeforeItsConfinementStarts(t *testing.T) {
	t.Parallel()
	box := mustCreate(t, newDirect(t), Spec{})
	direct, ok := box.(*directBox)
	if !ok {
		t.Fatalf("a direct backend minted a %T", box)
	}
	dir := t.TempDir()
	reported := filepath.Join(dir, "helper-adj")
	helper := filepath.Join(dir, "confine-helper")
	script := "#!/bin/sh\ncat /proc/self/oom_score_adj > '" + reported + "'\n" +
		"while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	direct.confine = &boxConfine{
		host:  &HostConfinement{Mechanism: MechanismLandlock, helper: helper},
		rules: confineRules{write: direct.Home()},
	}

	res, err := direct.Exec(t.Context(), "echo confined-ran", ExecOptions{})
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "confined-ran" {
		t.Fatalf("Exec: %v (exit %d, stdout %q, stderr %q)", err, res.ExitCode, res.Stdout, res.Stderr)
	}
	raw, err := os.ReadFile(reported)
	if err != nil {
		t.Fatalf("the confinement's helper never ran: %v", err)
	}
	requireAheadOfTheEngine(t, "a confined box's confinement helper", string(raw))
}

// A container box is started ahead of its engine — the runtime gives every
// `exec` into it the container's value — and an operator's own
// --oom-score-adj in run_args still wins, spliced after the backend's.
func TestAContainerBoxGoesBeforeItsEngineUnlessItsOperatorSaysOtherwise(t *testing.T) {
	t.Parallel()
	score, ok := boxOOMScore()
	if !ok {
		t.Fatal("this host does not report the engine's oom_score_adj")
	}
	requireAheadOfTheEngine(t, "a container box", strconv.Itoa(score))
	l := &Local{
		opts: LocalOptions{
			Placement: Container, Image: "img",
			RunArgs: []string{"--oom-score-adj", "-5"},
		},
		cli: containerCLI{path: fakeRuntime(t, "docker", dockerRootlessFormat, `[name=seccomp]`, 0)},
	}
	argv := l.runArgv(t.Context(), boxLayout{id: "b", root: t.TempDir()}, "crewlet-sbx-b")

	ours := slices.Index(argv, "--oom-score-adj")
	if ours < 0 || argv[ours+1] != strconv.Itoa(score) {
		t.Fatalf("argv = %v, want --oom-score-adj %d", argv, score)
	}
	theirs := slices.Index(argv[ours+2:], "--oom-score-adj")
	if theirs < 0 || argv[ours+2+theirs+1] != "-5" {
		t.Fatalf("argv = %v; the operator's --oom-score-adj must come after the backend's, "+
			"where the runtime takes the last, or it cannot override", argv)
	}
}
