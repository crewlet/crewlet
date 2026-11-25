package executor

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CHECKOUT SPEC REACHES THE HELPER IN THE SHAPE IT READS: Codex's slot of
// names and its skill directories are the lists they always were, byte for
// byte; Claude Code's rules a tree slot after its names, naming its form, and
// a recursive skill directory, the objects internal/notehelper's
// instructionSlot and skillDirSlot read.
//
// Mutation: write a slot of names or a plain skill directory as an object, or
// drop the tree's form or scope or the directory's recursive, and a case goes
// red.
func TestACheckoutSpecReachesTheHelperInTheShapeItReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		spec             checkoutSpec
		instructions, sd string
	}{
		{"codex", codexCheckouts(1), `[["AGENTS.override.md","AGENTS.md"]]`, `[".codex/skills",".agents/skills"]`},
		{"claude", claudeCheckouts, `[["CLAUDE.md"],[".claude/CLAUDE.md"],["CLAUDE.local.md"],` +
			`{"tree":".claude/rules","suffix":".md","form":"claude","scoped":{"keys":["paths"],"whole":["**"]}}]`,
			`[".claude/skills"]`},
		{"a recursive directory", checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"AGENTS.md"}}},
			SkillDirs: []checkoutSkillDir{{Dir: ".pi/skills", Recursive: true}, {Dir: ".agents/skills"}}},
			`[["AGENTS.md"]]`, `[{"dir":".pi/skills","recursive":true},".agents/skills"]`},
	}
	for _, c := range cases {
		var launch struct {
			Instructions json.RawMessage `json:"instructions"`
			SkillDirs    json.RawMessage `json:"skillDirs"`
		}
		if err := json.Unmarshal(checkoutHome{}.launchConfig(c.spec, nil), &launch); err != nil {
			t.Fatal(err)
		}
		if got := string(launch.Instructions); got != c.instructions {
			t.Errorf("%s: the launch config's instructions are %s, want %s", c.name, got, c.instructions)
		}
		if got := string(launch.SkillDirs); got != c.sd {
			t.Errorf("%s: the launch config's skill directories are %s, want %s", c.name, got, c.sd)
		}
	}
}

// piHomeFiles is Pi 1.6.0's home instruction file, the first of these that
// exists in its agent directory (measured, rows hd1-hd5).
var piHomeFiles = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// candidateHome is a checkoutHome under a box home whose instruction file is
// the first of names in agent, with the engine's note helper for this machine
// and Codex's spec, its working directory holding one checkout, repo.
func candidateHome(t *testing.T, names []string) (h checkoutHome, agent, work string) {
	t.Helper()
	home := t.TempDir()
	agent, work = filepath.Join(home, "agent"), filepath.Join(home, "workspace")
	finishedClone(t, filepath.Join(work, "repo", ".git"))
	if err := os.WriteFile(filepath.Join(work, "repo", "AGENTS.md"), []byte("REPO\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	h = checkoutHome{state: home + "/state", helper: home + "/helper", launch: home + "/launch.json",
		held: home + "/held", sections: home + "/sections", skills: home + "/skills", copied: home + "/copied"}
	for _, name := range names {
		h.agents = append(h.agents, filepath.Join(agent, name))
	}
	bin, err := noteHelperBinary(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(h.helper, bin, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(h.launch, h.launchConfig(codexCheckouts(codexProjectDocMaxBytes), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	return h, agent, real
}

// runShell runs script in dir, failing the case where it fails.
func runShell(t *testing.T, dir, script string) {
	t.Helper()
	for _, tool := range codexRepoTools {
		prereqtest.OnPath(t, tool, "the case runs the box's script", "install "+tool)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	cmd := exec.Command(sh, "-c", "cd "+shellword.Quote(dir)+" || exit 1\n"+script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
}

// homeFiles is every file in dir, by name, with its text.
func homeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		files[e.Name()] = readHome(t, filepath.Join(dir, e.Name()))
	}
	return files
}

// THE CHECKOUTS' SECTIONS GO INTO THE HOME FILE THE HARNESS READS, the first
// of its candidates that exists, else the first, each row a measured case of
// Pi 1.6.0's agent directory (hd1-hd5, exact names); every other candidate is
// untouched, and the put-back leaves the home as it was: the held file's own
// text back, or the file the run made removed. A run killed before its
// put-back is put back by the next run's gate, which then holds the home's
// own again, never the killed run's.
//
// Mutation: always write the first candidate, choose by any case, put back a
// fixed candidate, or keep a made file, and a row goes red.
func TestTheSectionsGoIntoTheHomeFileTheHarnessReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, into string
		home       map[string]string
	}{
		{"hd1 the override over AGENTS.md", "AGENTS.override.md",
			map[string]string{"AGENTS.override.md": "OVERRIDE\n", "AGENTS.md": "AGENTS\n"}},
		{"hd2 AGENTS.md over CLAUDE.md", "AGENTS.md", map[string]string{"AGENTS.md": "AGENTS\n", "CLAUDE.md": "CLAUDE\n"}},
		{"hd3 CLAUDE.md alone", "CLAUDE.md", map[string]string{"CLAUDE.md": "CLAUDE\n"}},
		{"hd4 AGENTS.MD alone", "AGENTS.MD", map[string]string{"AGENTS.MD": "UPPER\n"}},
		{"hd5 no exact name: the first made", "AGENTS.override.md",
			map[string]string{"agents.md": "lower\n", "Claude.md": "mixed\n", "Agents.override.md": "mixed\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, agent, work := candidateHome(t, piHomeFiles)
			for name, body := range c.home {
				if err := os.WriteFile(filepath.Join(agent, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			section := "for `repo` with root " + work + "/repo\n\nREPO\n"
			for _, kill := range []bool{false, true} {
				runShell(t, work, h.gate())
				if kill { // the killed run's gate is followed by the next run's
					runShell(t, work, h.gate())
				}
				want := maps.Clone(c.home)
				want[c.into] = section
				if own, ok := c.home[c.into]; ok {
					want[c.into] = own + "\n\n" + section
				}
				if got := homeFiles(t, agent); !maps.Equal(got, want) {
					t.Errorf("killed %v: the home holds %q, want %q", kill, got, want)
				}
				runShell(t, work, h.restore()+" && "+h.restore())
				if got := homeFiles(t, agent); !maps.Equal(got, c.home) {
					t.Errorf("killed %v: after the put-back the home holds %q, want %q", kill, got, c.home)
				}
				for _, held := range []string{h.held, h.held + heldNone, h.held + heldLive} {
					if _, err := os.Lstat(held); err == nil {
						t.Errorf("killed %v: %s is left after the put-back", kill, held)
					}
				}
			}
		})
	}
}

// ONE CANDIDATE KEEPS THE SCRIPT IT ALWAYS HAD: Codex's home file is the only
// one it reads, so its gate and put-back name nothing more.
//
// Mutation: name the held file of one candidate, and the script goes red.
func TestOneHomeFileCandidateNamesNoHeldFile(t *testing.T) {
	t.Parallel()
	h := codexCheckoutHome(Paths{Home: "/home/box"})
	if strings.Contains(h.gate()+h.restore(), heldLive) {
		t.Errorf("Codex's gate or put-back names the held file:\n%s\n%s", h.gate(), h.restore())
	}
	if got, want := h.putBack(), putBackHome("/home/box/.codex/AGENTS.md", h.held); got != want {
		t.Errorf("Codex's put-back is\n%s\nwant\n%s", got, want)
	}
}

// A HOLD CUT SHORT NEVER REPLACES THE HOME'S FILE: where the gate is killed
// mid-copy of the home's instruction file (a full disk ends it as surely),
// the put-back the clear runs leaves the person's file whole and nothing
// held, for Codex's one candidate and Pi's several alike.
//
// Mutation: copy straight into the held file, or leave the part behind, and
// a row goes red.
func TestAHoldCutShortNeverReplacesTheHomesFile(t *testing.T) {
	t.Parallel()
	for name, names := range map[string][]string{"codex": {"AGENTS.md"}, "pi": piHomeFiles} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, agent, work := candidateHome(t, names)
			const own = "THE PERSON'S OWN INSTRUCTIONS\n"
			if err := os.WriteFile(filepath.Join(agent, "AGENTS.md"), []byte(own), 0o600); err != nil {
				t.Fatal(err)
			}
			// A cp that writes part of the file, then dies with its script.
			bin := t.TempDir()
			cut := "#!/bin/sh\nhead -c 5 \"$1\" > \"$2\"\nkill -KILL \"$PPID\"\n"
			if err := os.WriteFile(filepath.Join(bin, "cp"), []byte(cut), 0o700); err != nil { //nolint:gosec // the case's own tool
				t.Fatal(err)
			}
			sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
			cmd := exec.Command(sh, "-c", "cd "+shellword.Quote(work)+" || exit 1\n"+h.gate())
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("the gate ran to its end past a cp that killed it:\n%s", out)
			}
			runShell(t, work, h.restore())
			if got := homeFiles(t, agent); !maps.Equal(got, map[string]string{"AGENTS.md": own}) {
				t.Errorf("after the put-back the home holds %q, want its own file whole", got)
			}
			for _, held := range []string{h.held, h.held + heldNone, h.held + heldLive, h.held + heldPart} {
				if _, err := os.Lstat(held); err == nil {
					t.Errorf("%s is left after the put-back", held)
				}
			}
		})
	}
}

// A RUN IS MARKED ENDED ONLY ONCE ITS FIRINGS LET GO: a firing holds the
// state's lock for all its work, and the end's restore waits on that lock
// before it writes the marker, so what a firing still running at the end
// keeps for the run's end (here a line it appends) is printed by the end's
// next step, never left behind it.
//
// Mutation: write the marker without the lock, and the late line goes
// unprinted.
func TestARunIsMarkedEndedOnlyOnceItsFiringsLetGo(t *testing.T) {
	t.Parallel()
	prereqtest.OnPath(t, "flock", "the run's end waits on the firings' lock with it", "install util-linux")
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	h, _, work := candidateHome(t, []string{"AGENTS.md"})
	if err := os.MkdirAll(h.state, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(h.state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	cmd := exec.Command(sh, "-c", "cd "+shellword.Quote(work)+" || exit 1\n"+everyStep(h.restore(), h.saySkipped()))
	cmd.Stderr = &said
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ended, waited := false, error(nil)
	giveUp := time.After(10 * time.Second) //fixedwait:liveness
	for !ended && !waitingOnLock(t, h.state) {
		select {
		case waited = <-done:
			ended = true
		case <-giveUp:
			t.Fatal("the end neither waited on the firings' lock nor finished within 10s")
		case <-time.After(20 * time.Millisecond): //fixedwait:liveness
		}
	}
	f, err := os.OpenFile(filepath.Join(h.state, "skipped"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("notehelper: LATE\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if !ended {
		waited = <-done
	}
	if waited != nil || !strings.Contains(said.String(), "notehelper: LATE") {
		t.Errorf("the end (%v) printed %q, want the line the running firing kept", waited, said.String())
	}
}

// waitingOnLock reports whether a flock(1) on state's lock is running.
func waitingOnLock(t *testing.T, state string) bool {
	t.Helper()
	procs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		raw, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline")) //nolint:gosec // the box's own process table
		if err == nil && bytes.Contains(raw, []byte("flock\x00")) && bytes.Contains(raw, []byte(state+"/lock")) {
			return true
		}
	}
	return false
}

// CODEX'S AND PI'S CHECKOUT FORMS ARE THE ONES THEY WERE, BESIDE HERMES'S
// GATED ONE: their launch configs carry no gate or budget, their gates hold
// the home's instruction file, run checkout-launch on no input and write the
// sections into that file, and their restores put it back; Hermes's carries
// its gate and budget, peeks and asks Hermes before checkout-launch, and
// holds, writes and puts back no home file.
//
// Mutation: write `gated` or `budget` for every harness, run the gate's
// command for one without it, or hold a home file Hermes has none of, and a
// case goes red.
func TestCodexAndPiCheckoutFormsCarryNoGateBesideHermess(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/u"}
	keys := func(h checkoutHome, spec checkoutSpec) []string {
		var config map[string]any
		if err := json.Unmarshal(h.launchConfig(spec, []string{"repo"}), &config); err != nil {
			t.Fatal(err)
		}
		return slices.Sorted(maps.Keys(config))
	}
	plain := []string{"agents", "copied", "first", "importInstructions", "instructions", "limit", "nestedSkillsOnly", "reserved",
		"skillDirs", "skills", "state"}
	for name, c := range map[string]struct {
		home  checkoutHome
		spec  checkoutSpec
		gated bool
	}{
		"codex":  {codexCheckoutHome(paths), codexCheckouts(codexProjectDocMaxBytes), false},
		"pi":     {piCheckoutHome(paths), piCheckouts(), false},
		"hermes": {hermesLaunchHome(paths, "m"), hermesCheckouts(paths), true},
	} {
		want := plain
		if c.gated {
			want = slices.Sorted(slices.Values(append(slices.Clone(plain), "budget", "gated")))
		}
		if got := keys(c.home, c.spec); !slices.Equal(got, want) {
			t.Errorf("%s: the launch config's keys are %q, want %q", name, got, want)
		}
		gate, restore := c.home.gate(), c.home.restore()
		for what, held := range map[string]bool{
			"the home file held":           strings.Contains(gate, `cp "$ra" "$rha`),
			"the sections written into it": strings.Contains(gate, `> "$ra"`),
			"the home file put back":       c.home.held != "" && strings.Contains(restore, shellword.Quote(c.home.held)),
			"checkout-launch on no input":  strings.Contains(gate, "-adapter checkout-launch -config "+shellword.Quote(c.home.launch)+" < /dev/null"),
		} {
			if held == c.gated {
				t.Errorf("%s: %s: %v", name, what, held)
			}
		}
		if peeks := strings.Contains(gate, "-adapter hermes-peek"); peeks != c.gated {
			t.Errorf("%s: the gate peeks: %v", name, peeks)
		}
	}
}
