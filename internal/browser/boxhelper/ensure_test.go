package boxhelper_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// readyLine is a ready line with every field it requires, each of its grammar.
const readyLine = "crewlet-browser-ensure/1 ready helper=0123456789abcdef pin=0.0.86 node=22.23.5 " +
	"chromium=157.0.8103.50 build=expected sandbox=required supervisor=started"

// ENSURE SPEAKS ONLY ITS CLOSED LINES: ParseEnsure reads the last non-empty
// line of the first 4 KiB, which must be the line's grammar whole. A ready
// line carries every field it requires, a node and a pin a prerelease's tag
// at most, and build, sandbox and supervisor each a value of its type; a
// withheld one any reason the box prints with any of its causes; an unknown
// key is ignored, and anything else is unreadable: bad_line, or no_line.
//
// Mutations, each red: accept a reason off the list, or one only the engine
// gives; read the first line, not the last; read past 4 KiB; let ready pass
// without build; a node grammar with no prerelease's tag.
func TestEnsureSpeaksOnlyItsClosedLines(t *testing.T) {
	t.Parallel()
	withheld := func(rest string) string { return "crewlet-browser-ensure/1 withheld " + rest }
	full := boxhelper.Ensure{Verdict: browser.EnsureReady, Helper: "0123456789abcdef", Pin: "0.0.86", Node: "22.23.5",
		Chromium: "157.0.8103.50", Build: browser.BuildExpected, Sandbox: browser.SandboxRequired,
		Supervisor: browser.SupervisorStarted}
	alpha, next := full, full
	alpha.Node, next.Pin = "27.0.0-alpha.0.1.1", "0.0.79-alpha-1787869812000"
	type row struct {
		name, stdout string
		want         boxhelper.Ensure
		unreadable   browser.EnsureCause // empty: read
	}
	rows := []row{
		{name: "ready", stdout: readyLine + "\n", want: full},
		{name: "ready after other output", stdout: "warming up\n" + readyLine + "\n\n", want: full},
		{name: "ready with a later build's key", stdout: readyLine + " frames=2\n", want: full},
		{name: "ready on a Node prerelease", stdout: strings.Replace(readyLine, "node=22.23.5",
			"node=27.0.0-alpha.0.1.1", 1), want: alpha},
		{name: "ready on a pin's prerelease", stdout: strings.Replace(readyLine, "pin=0.0.86",
			"pin=0.0.79-alpha-1787869812000", 1), want: next},
		{name: "withheld", stdout: withheld("reason=chromium_missing cause=not_found node=22.23.5\n"),
			want: boxhelper.Ensure{Verdict: browser.EnsureWithheld, Reason: browser.ReasonChromiumMissing,
				Cause: browser.CauseNotFound, Node: "22.23.5"}},
		{name: "the last line read, not the first", stdout: withheld("reason=node_missing cause=not_on_path\n") + readyLine,
			want: full},
		{name: "a line past the first 4 KiB is never read",
			stdout:     readyLine + "\n" + strings.Repeat("x", 5000) + "\n" + withheld("reason=node_missing cause=not_on_path"),
			unreadable: browser.CauseBadLine},
		{name: "no output", stdout: "", unreadable: browser.CauseNoLine},
		{name: "blank lines alone", stdout: "\n\n\n", unreadable: browser.CauseNoLine},
		{name: "ready without build", stdout: strings.Replace(readyLine, " build=expected", "", 1),
			unreadable: browser.CauseBadLine},
		{name: "ready carrying a reason", stdout: readyLine + " reason=chromium_missing", unreadable: browser.CauseBadLine},
		{name: "a build off its type", stdout: strings.Replace(readyLine, "build=expected", "build=trusted", 1),
			unreadable: browser.CauseBadLine},
		{name: "a sandbox off its type", stdout: strings.Replace(readyLine, "sandbox=required", "sandbox=off", 1),
			unreadable: browser.CauseBadLine},
		{name: "a start off its type", stdout: strings.Replace(readyLine, "supervisor=started", "supervisor=resumed", 1),
			unreadable: browser.CauseBadLine},
		{name: "withheld without a cause", stdout: withheld("reason=chromium_missing"), unreadable: browser.CauseBadLine},
		{name: "a reason off the list", stdout: withheld("reason=out_of_coffee cause=not_found"),
			unreadable: browser.CauseBadLine},
		{name: "a cause of another reason", stdout: withheld("reason=chromium_missing cause=apparmor"),
			unreadable: browser.CauseBadLine},
		{name: "a cause no reason takes any more", stdout: withheld("reason=playwright_missing cause=not_placed"),
			unreadable: browser.CauseBadLine},
		{name: "a known key off its grammar", stdout: strings.Replace(readyLine, "node=22.23.5", "node=22", 1),
			unreadable: browser.CauseBadLine},
		{name: "a node past 64 bytes", stdout: strings.Replace(readyLine, "node=22.23.5",
			"node=22.23.5-"+strings.Repeat("a", 57), 1), unreadable: browser.CauseBadLine},
		{name: "a key twice", stdout: readyLine + " pin=0.0.85", unreadable: browser.CauseBadLine},
		{name: "another version of the line", stdout: strings.Replace(readyLine, "ensure/1", "ensure/2", 1),
			unreadable: browser.CauseBadLine},
		{name: "a verdict off the list", stdout: strings.Replace(readyLine, " ready ", " maybe ", 1),
			unreadable: browser.CauseBadLine},
		{name: "two spaces", stdout: strings.Replace(readyLine, " pin", "  pin", 1), unreadable: browser.CauseBadLine},
		{name: "a carriage return", stdout: readyLine + "\r\n", unreadable: browser.CauseBadLine},
		{name: "a line past 512 bytes", stdout: readyLine + " later=" + strings.Repeat("y", 120) + " more=" +
			strings.Repeat("z", 120) + " most=" + strings.Repeat("w", 120), unreadable: browser.CauseBadLine},
	}
	for _, r := range browser.EnsureReasons() {
		for _, c := range r.Causes() {
			if r.Printed() {
				rows = append(rows, row{name: fmt.Sprintf("%s %s", r, c),
					stdout: withheld(fmt.Sprintf("reason=%s cause=%s", r, c)),
					want:   boxhelper.Ensure{Verdict: browser.EnsureWithheld, Reason: r, Cause: c}})
			}
		}
		// A REASON ONLY THE ENGINE GIVES, with each of its causes, or with a
		// cause a box's reason takes where it takes none, as a line must carry.
		if r.Printed() {
			continue
		}
		causes := r.Causes()
		if len(causes) == 0 {
			causes = []browser.EnsureCause{browser.CauseNotFound}
		}
		for _, c := range causes {
			rows = append(rows, row{name: fmt.Sprintf("the engine's %s %s", r, c),
				stdout: withheld(fmt.Sprintf("reason=%s cause=%s", r, c)), unreadable: browser.CauseBadLine})
		}
	}
	for _, tc := range rows {
		got, err := boxhelper.ParseEnsure(tc.stdout)
		var unreadable *boxhelper.UnreadableError
		switch {
		case tc.unreadable == "" && err != nil:
			t.Errorf("%s: %v, want %+v", tc.name, err, tc.want)
		case tc.unreadable == "" && got != tc.want:
			t.Errorf("%s: read %+v, want %+v", tc.name, got, tc.want)
		case tc.unreadable != "" && (!errors.As(err, &unreadable) || unreadable.Cause != tc.unreadable):
			t.Errorf("%s: read %+v (%v), want it unreadable as %s", tc.name, got, err, tc.unreadable)
		}
	}
}

// standIn is a directory holding an executable named node: a stand-in that
// answers -p with version, and does nothing else, so a prelude it passes
// prints nothing more.
func standIn(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -p ] && echo '" + version + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// prelude is what the ensure prelude prints with path its PATH: a line where
// it withholds, nothing where it hands the node the helper.
func prelude(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", boxhelper.EnsureScript("/nonexistent/home"))
	cmd.Env = []string{"PATH=" + path, "NODE_OPTIONS=--require=/nonexistent/mark.cjs"}
	out, err := cmd.Output()
	if err != nil && len(out) > 0 {
		t.Fatalf("the prelude printed %q and failed: %v", out, err)
	}
	return string(out)
}

// THE ENSURE PRELUDE REFUSES A NODE BELOW THE FLOOR: before Node is asked to
// load the helper, a box with no node on PATH is node_missing, and a node
// whose release is below MinNodeMajor, compared as a number, is node_too_old,
// naming the version, a prerelease's tag and all; one off the line's grammar
// (three numbers, then a tag of dot-joined identifiers, 64 bytes in all) is
// node_too_old naming none. Only a node at or above the floor is handed the
// helper, which these stand-ins are not: past the prelude, they print nothing.
//
// Mutations, each red: compare majors as strings (9 passes); refuse any tag
// (27's alpha is refused); take any text as a tag (22.0.0- passes).
func TestTheEnsurePreludeRefusesANodeBelowTheFloor(t *testing.T) {
	t.Parallel()
	floor := boxhelper.MinNodeMajor
	const tooOld = "crewlet-browser-ensure/1 withheld reason=node_too_old cause=below_floor"
	for _, tc := range []struct {
		name, path, want string
	}{
		{"no node on PATH", t.TempDir(), "crewlet-browser-ensure/1 withheld reason=node_missing cause=not_on_path\n"},
		{"Node 20", standIn(t, "20.11.1"), tooOld + " node=20.11.1\n"},
		{"Node 9, below the floor as a number and above it as a string", standIn(t, "9.11.0"),
			tooOld + " node=9.11.0\n"},
		{"Node 20's release candidate", standIn(t, "20.0.0-rc.1"), tooOld + " node=20.0.0-rc.1\n"},
		{"a version of two numbers", standIn(t, "22.1"), tooOld + "\n"},
		{"a version of four numbers", standIn(t, "22.1.0.4"), tooOld + "\n"},
		{"a major of four digits", standIn(t, "1234.0.0"), tooOld + "\n"},
		{"a tag of nothing", standIn(t, "22.0.0-"), tooOld + "\n"},
		{"a tag with an empty identifier", standIn(t, "22.0.0-alpha..1"), tooOld + "\n"},
		{"a tag of other characters", standIn(t, "22.0.0-alpha+1"), tooOld + "\n"},
		{"a version past 64 bytes", standIn(t, "22.0.0-"+strings.Repeat("a", 58)), tooOld + "\n"},
		{"garbage", standIn(t, "v22.0.0 and more"), tooOld + "\n"},
		{"nothing", standIn(t, ""), tooOld + "\n"},
		{"the floor", standIn(t, strconv.Itoa(floor)+".0.0"), ""},
		{"above the floor", standIn(t, strconv.Itoa(floor+4)+".12.1"), ""},
		{"a prerelease above the floor", standIn(t, "27.0.0-alpha.0.1.1"), ""},
	} {
		if got := prelude(t, tc.path); got != tc.want {
			t.Errorf("%s: the prelude printed %q, want %q", tc.name, got, tc.want)
		}
	}
	if !strings.HasPrefix(boxhelper.EnsureCommand("/home/user"), "sh -lc '") {
		t.Errorf("EnsureCommand is %q, want a login shell, as a job runs, so ensure finds the node a job would",
			boxhelper.EnsureCommand("/home/user"))
	}
}

// pair is a reason and a cause the box prints.
type pair struct {
	Reason browser.EnsureReason
	Cause  browser.EnsureCause
}

// THE HELPER PRINTS THE REASONS THE ENGINE READS: every reason and cause the
// helper's modules print (common.mjs's PRINTS), with the two the ensure
// prelude prints before Node loads them, read off its own lines, are exactly
// the printed part of internal/browser's table, both ways: no module prints a
// pair the engine refuses as bad_line, and the engine waits on no pair no
// box prints. A copy a second language cannot avoid gets a gate (ADR-0008).
//
// Mutations, each red: a typo in PRINTS; a Go cause no module prints.
func TestTheHelperPrintsTheReasonsTheEngineReads(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		var got struct {
			Prints map[browser.EnsureReason][]browser.EnsureCause `json:"prints"`
		}
		boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t).Do(map[string]string{"op": "prints"}, &got)
		printed := map[pair]bool{}
		for r, causes := range got.Prints {
			for _, c := range causes {
				printed[pair{r, c}] = true
			}
		}
		for _, path := range []string{t.TempDir(), standIn(t, "9.0.0")} {
			line, err := boxhelper.ParseEnsure(prelude(t, path))
			if err != nil {
				t.Fatalf("the prelude's line: %v", err)
			}
			printed[pair{line.Reason, line.Cause}] = true
		}
		table := map[pair]bool{}
		for _, r := range browser.EnsureReasons() {
			for _, c := range r.Causes() {
				if r.Printed() {
					table[pair{r, c}] = true
				}
			}
		}
		for p := range printed {
			if !table[p] {
				t.Errorf("the helper prints %s %s, which internal/browser's table does not give a box", p.Reason, p.Cause)
			}
		}
		for p := range table {
			if !printed[p] {
				t.Errorf("internal/browser's table gives a box %s %s, which nothing the helper runs prints",
					p.Reason, p.Cause)
			}
		}
	})
}

// ENSURE NAMES EACH REASON IT CAN TELL: whatever stops a box's browser,
// ensure prints one line naming the first check that failed and the cause it
// could tell, under its own deadline: a folder that is a link to one, or too
// deep for its sockets; a Node below config.json's floor; no npm, lock files
// that are not the lock, or no registry and an empty cache; no Chromium
// anywhere it looks; a supervisor that dies before a word, or says none in
// its wait. Stand-ins stand in for a Chromium, which none of these reaches; a
// launch's causes are TestPlaywrightsLaunchFailuresStillReadAsTheirReasons's,
// and a box's that no case can provoke are the probe's.
//
// Mutations, each red: read ENOENT as anything but npm_missing; drop
// ensure's own node check (the floor row reads chromium_missing).
func TestEnsureNamesEachReasonItCanTell(t *testing.T) {
	t.Parallel()
	const hang = "exec /bin/cat <&3 >/dev/null\n"
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			cache   boxhelpertest.Cache
			chrome  string         // a stand-in's body, on PATH as chromium
			config  map[string]any // over the engine's
			arrange func(t *testing.T, box *boxhelpertest.Box) (home string)
			reason  browser.EnsureReason
			cause   browser.EnsureCause
		}{
			{name: "a link in the folder's place", arrange: func(t *testing.T, box *boxhelpertest.Box) string {
				moved := filepath.Join(filepath.Dir(box.Folder), "elsewhere")
				rename(t, box.Folder, moved)
				if err := os.Symlink(moved, box.Folder); err != nil {
					t.Fatal(err)
				}
				return box.Home
			}, reason: browser.ReasonFolderUnusable, cause: browser.CauseNotDirectory},
			{name: "a home too deep for its sockets", arrange: func(t *testing.T, box *boxhelpertest.Box) string {
				deep := filepath.Join(box.Home, strings.Repeat("d", 80))
				if err := os.Mkdir(deep, 0o700); err != nil {
					t.Fatal(err)
				}
				rename(t, filepath.Dir(box.Folder), filepath.Join(deep, ".crewlet"))
				return deep
			}, reason: browser.ReasonFolderUnusable, cause: browser.CausePathTooLong},
			{name: "a floor above the box's Node", config: map[string]any{"node_floor": 999},
				reason: browser.ReasonNodeTooOld, cause: browser.CauseBelowFloor},
			{name: "no npm", arrange: func(t *testing.T, box *boxhelpertest.Box) string {
				if err := os.Remove(filepath.Join(box.Home, "bin", "npm")); err != nil {
					t.Fatal(err)
				}
				return box.Home
			}, reason: browser.ReasonPlaywrightMissing, cause: browser.CauseNPMMissing},
			{name: "lock files that are not the lock", arrange: func(t *testing.T, box *boxhelpertest.Box) string {
				appendTo(t, filepath.Join(box.Folder, "tree-package-lock.json"), "\n")
				return box.Home
			}, reason: browser.ReasonPlaywrightMissing, cause: browser.CauseDigestMismatch},
			{name: "no registry and an empty cache", cache: boxhelpertest.CacheNone,
				reason: browser.ReasonPlaywrightMissing, cause: browser.CauseRegistryUnreachable},
			{name: "no Chromium", reason: browser.ReasonChromiumMissing, cause: browser.CauseNotFound},
			{name: "a supervisor that dies before a word", chrome: hang,
				arrange: func(t *testing.T, box *boxhelpertest.Box) string {
					file := filepath.Join(box.Folder, "supervise.mjs")
					if err := os.WriteFile(file, []byte("this is not a module\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					return box.Home
				}, reason: browser.ReasonSupervisorFailed, cause: browser.CauseExited},
			{name: "a supervisor with no word in its wait", chrome: hang,
				config: map[string]any{"supervisor_wait_ms": 1000},
				reason: browser.ReasonSupervisorFailed, cause: browser.CauseTimeout},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium,
					Cache: tc.cache, Config: tc.config})
				if tc.chrome != "" {
					standInChromium(t, box, tc.chrome)
				}
				home := box.Home
				if tc.arrange != nil {
					home = tc.arrange(t, box)
				}
				withheldAs(t, box, home, tc.reason, tc.cause)
			})
		}
	})
}

// withheldAs fails t unless ensure in box, run for home, withholds its
// browser as reason and cause.
func withheldAs(t *testing.T, box *boxhelpertest.Box, home string, reason browser.EnsureReason,
	cause browser.EnsureCause) {
	t.Helper()
	stdout, stderr, err := box.RunEnsure(t.Context(), home)
	got, parseErr := boxhelper.ParseEnsure(stdout)
	if err != nil || parseErr != nil || got.Verdict != browser.EnsureWithheld || got.Reason != reason ||
		got.Cause != cause {
		t.Errorf("ensure answered %q (%v, %v), want withheld %s %s; its stderr:\n%s", stdout, err, parseErr, reason,
			cause, stderr)
	}
}

// setEnsureDeadline writes ms as the box's ensure_deadline_ms, which each
// ensure reads from config.json as it starts.
func setEnsureDeadline(t *testing.T, box *boxhelpertest.Box, ms any) {
	t.Helper()
	config := box.Config(t)
	config["ensure_deadline_ms"] = ms
	box.SetConfig(t, config)
}

// rename renames from to to, failing t.
func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// appendTo appends text to file, failing t.
func appendTo(t *testing.T, file, text string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ENSURE'S DEADLINE ANSWERS FOR THE STEP IT CAUGHT: ensure's own deadline,
// passed while npm installs, is playwright_missing timeout, its npm killed
// with its group and no tree published, and the next ensure leaves pw/
// holding the tree alone; passed while the supervisor launches, it is
// supervisor_failed timeout. The deadline (2 s) is each row's subject, every
// bound it races set past the case; the install row's next ensure, which
// installs for real, runs under the deadline the box ships with.
//
// Mutations, each red: answer supervisor_failed at every deadline; leave npm
// running at the deadline (the stand-in outlives the case's wait).
func TestEnsuresDeadlineAnswersForTheStepItCaught(t *testing.T) {
	t.Parallel()
	const hang = "exec /bin/cat <&3 >/dev/null\n"
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		t.Run("during the install", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium})
			shipped := box.Config(t)["ensure_deadline_ms"]
			setEnsureDeadline(t, box, 2000)
			// It stops by itself if a failed case leaves it.
			box.StandInNPM(t, "11.0.0", `echo $$ >"$HOME/npm.pid"`+"\nexec sleep 600")
			withheldAs(t, box, box.Home, browser.ReasonPlaywrightMissing, browser.CauseTimeout)
			raw, err := os.ReadFile(filepath.Join(box.Home, "npm.pid"))
			if err != nil {
				t.Fatalf("the stand-in npm never ran: %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			stopAtEnd(t, pid)
			if !procgrouptest.AwaitGone(t, pid, goneWithin) {
				t.Errorf("npm, pid %d, outlived ensure's deadline by %s", pid, goneWithin)
			}
			for _, name := range inPW(t, box) {
				if nameOf(t, box).MatchString(name) {
					t.Errorf("pw/%s is published after an install ensure's deadline ended", name)
				}
			}
			relink(t, box, env)
			setEnsureDeadline(t, box, shipped)
			withheldAs(t, box, box.Home, browser.ReasonChromiumMissing, browser.CauseNotFound)
			if left := inPW(t, box); len(left) != 1 || !nameOf(t, box).MatchString(left[0]) {
				t.Errorf("pw/ holds %q after the next ensure, want the tree alone", left)
			}
		})
		t.Run("during the supervisor's launch", func(t *testing.T) {
			t.Parallel()
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium,
				Config: map[string]any{"ensure_deadline_ms": 2000, "supervisor_wait_ms": 3_600_000,
					"launch_timeout_ms": 3_600_000}})
			// The tree is in place, so the deadline finds the supervisor.
			ready(t, box, install(box.Probe(t), box, nil), true)
			standInChromium(t, box, hang)
			withheldAs(t, box, box.Home, browser.ReasonSupervisorFailed, browser.CauseTimeout)
		})
	})
}

// ONE ENSURE AT A TIME STARTS ONE SUPERVISOR: two ensures started at once in
// one box take its lock in turn, so one installs the tree and starts the
// supervisor and the other finds it running, and the box runs that one
// supervisor, on each Node a box may bring, whose own exclusive create and
// watch the lock rests on.
//
// Mutation: drop the lock (both start one, or one restarts the other's).
func TestOneEnsureAtATimeStartsOneSupervisor(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		type answer struct {
			stdout, stderr string
			err            error
		}
		answers := make(chan answer, 2)
		for range 2 {
			go func() {
				stdout, stderr, err := box.RunEnsure(t.Context(), box.Home)
				answers <- answer{stdout, stderr, err}
			}()
		}
		var starts []string
		for range 2 {
			a := <-answers
			got, err := boxhelper.ParseEnsure(a.stdout)
			if a.err != nil || err != nil || got.Verdict != browser.EnsureReady {
				t.Fatalf("ensure answered %q (%v, %v); its stderr:\n%s", a.stdout, a.err, err, a.stderr)
			}
			starts = append(starts, string(got.Supervisor))
		}
		slices.Sort(starts)
		if !slices.Equal(starts, []string{"found", "started"}) {
			t.Errorf("two ensures at once answered %q, want one started and one found", starts)
		}
		if got := box.Ensure(t); got.Supervisor != browser.SupervisorFound {
			t.Errorf("a third ensure answered %+v, want the one supervisor found", got)
		}
	})
}

// regenerated drops the licence lines from the box's lock, as an npm that
// writes none regenerates it under the same pin, and names the edited lock
// in config, as the engine would; it answers the tree's new name.
func regenerated(t *testing.T, box *boxhelpertest.Box, config map[string]any) string {
	t.Helper()
	lockFile := filepath.Join(box.Folder, "tree-package-lock.json")
	lock, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := regexp.MustCompile(`(?m)^\s*"license": "[^"]*",\n`).ReplaceAll(lock, nil)
	if len(edited) == len(lock) {
		t.Fatal("the lock holds no licence line to drop")
	}
	if err := os.WriteFile(lockFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(box.Folder, "tree-package.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, l := sha256.Sum256(manifest), sha256.Sum256(edited)
	digest := sha256.Sum256([]byte(hex.EncodeToString(m[:]) + "\n" + hex.EncodeToString(l[:]) + "\n"))
	config["pin_digest"] = hex.EncodeToString(digest[:])[:12]
	return fmt.Sprintf("%v-%v-l", config["pin"], config["pin_digest"])
}

// ENSURE FINDS A LIVE SUPERVISOR AND RESTARTS A STALE ONE: a second ensure
// finds the supervisor the first started, the same process; one whose launch
// changed in anything it runs under (a viewport, a lock regenerated under the
// same pin, a hello's bound, the server's bounds, a variable of its
// environment, the sandbox), whose helper changed, or whose browser crashed
// out is stopped and another started; and once the box's one supervisor runs
// on this tree, every other entry of pw/ is swept, never one a live
// supervisor runs on.
//
// Mutations, each red: digest the launch's old fields alone (the lock row is
// found, its tree swept from under it); leave the environment out (the TZ row).
func TestEnsureFindsALiveSupervisorAndRestartsAStaleOne(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		pw := filepath.Join(box.Folder, "pw")
		for _, leftover := range []string{"0.0.1-000000000000-l1", "0.0.86-0123456789ab-l1.Xy3kQ1"} {
			if err := os.MkdirAll(filepath.Join(pw, leftover, "node_modules"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if got := box.Ensure(t); got.Supervisor != browser.SupervisorStarted {
			t.Fatalf("the first ensure answered %+v, want a supervisor started", got)
		}
		if left := inPW(t, box); len(left) != 1 || !nameOf(t, box).MatchString(left[0]) {
			t.Errorf("pw/ holds %q after a ready ensure, want this lock's tree alone", left)
		}
		first := box.Supervisor(t).PID
		if got := box.Ensure(t); got.Supervisor != browser.SupervisorFound || box.Supervisor(t).PID != first {
			t.Fatalf("a second ensure answered %+v with supervisor %d, want %d found", got, box.Supervisor(t).PID, first)
		}
		var tree string // the regenerated lock's tree, once that row has run
		for _, r := range []struct {
			name   string
			change func(config map[string]any) // nil: the record crashed out
			extra  []string
		}{
			{"another viewport", func(config map[string]any) {
				config["viewport"] = map[string]any{"width": 1024, "height": 768}
			}, nil},
			{"another helper", func(config map[string]any) { config["helper"] = "fedcba9876543210" }, nil},
			{"a browser that crashed out", nil, nil},
			{"a lock regenerated under the same pin", func(config map[string]any) {
				tree = regenerated(t, box, config)
			}, nil},
			{"another hello bound", func(config map[string]any) { config["hello_ms"] = 3000 }, nil},
			{"another server bound", func(config map[string]any) {
				config["mcp"].(map[string]any)["action_ms"] = 6000
			}, nil},
			{"another time zone", func(map[string]any) {}, []string{"TZ=UTC"}},
		} {
			before := box.Supervisor(t).PID
			if r.change != nil {
				config := box.Config(t)
				r.change(config)
				box.SetConfig(t, config)
			} else {
				record := filepath.Join(box.Folder, "supervisor.json")
				raw, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, []byte(strings.Replace(string(raw), `"state":"ready"`,
					`"state":"crashed_out"`, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := box.Ensure(t, r.extra...)
			after := box.Supervisor(t).PID
			if got.Supervisor != browser.SupervisorRestarted || after == before {
				t.Errorf("%s: ensure answered %+v with supervisor %d, before it %d, want another restarted", r.name,
					got, after, before)
			}
			if running, err := procgrouptest.Running(before); err != nil || running {
				t.Errorf("%s: the supervisor it replaced, pid %d, still runs (%v)", r.name, before, err)
			}
		}
		if left := inPW(t, box); len(left) != 1 || !strings.HasPrefix(left[0], tree) {
			t.Errorf("pw/ holds %q, want the regenerated lock's tree alone, %s<layout>", left, tree)
		}
		before := box.Supervisor(t).PID
		config := box.Config(t)
		config["sandbox"] = "required"
		box.SetConfig(t, config)
		got := box.Ensure(t, "TZ=UTC")
		if boxhelpertest.SandboxRuns(t) {
			if got.Supervisor != browser.SupervisorRestarted || got.Sandbox != browser.SandboxRequired {
				t.Errorf("another sandbox: ensure answered %+v, want a supervisor restarted under required", got)
			}
		} else if got.Reason != browser.ReasonChromiumSandboxUnavailable {
			t.Errorf("another sandbox, on a host that cannot start it: ensure answered %+v, want it withheld", got)
		}
		if running, err := procgrouptest.Running(before); err != nil || running {
			t.Errorf("another sandbox: the supervisor it replaced, pid %d, still runs (%v)", before, err)
		}
	})
}

// mode fails t unless path's permission bits are want.
func mode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s is %v, want %v: the box's user's alone", path, got, want)
	}
}

// closed fails t unless nothing the supervisor or its browser made in box's
// folder, the profile included, is open to the box's other users.
func closed(t *testing.T, box *boxhelpertest.Box) {
	t.Helper()
	for _, dir := range []string{"profile", "output", "artifacts"} {
		err := filepath.WalkDir(filepath.Join(box.Folder, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.Type()&fs.ModeSymlink != 0 {
				return err
			}
			if info, err := d.Info(); err == nil && info.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is %v, open to the box's other users", path, info.Mode())
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}
}

// THE BOX'S FILES AND SOCKETS ARE ITS USER'S ALONE: whatever modes the
// box's upload left the engine's files at (0644 here, in 0755 directories),
// once ensure has run the folder is 0700, every file directly in it 0600, the
// socket 0600, and nothing the supervisor or its browser made, the profile
// included, is open to the box's other users; nor is it from a supervisor a
// looser process starts (the probe, at umask 022) on a fresh profile, since
// ensure's install has set 077 before it starts one. On each Node, whose
// umask and chmod the helper's modes are.
//
// Mutation: skip the supervisor's umask (the probe's profile is the world's
// to read).
func TestTheBoxsFilesAndSocketsAreItsUsersAlone(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
		if info, err := os.Stat(filepath.Join(box.Folder, "config.json")); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("the engine's config.json is %v (%v) before ensure, and this case needs the upload's loose modes",
				info.Mode(), err)
		}
		if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v", got)
		}
		mode(t, box.Folder, 0o700)
		entries, err := os.ReadDir(box.Folder)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			switch file := filepath.Join(box.Folder, e.Name()); {
			case e.Type().IsRegular():
				mode(t, file, 0o600)
			case e.Type()&fs.ModeSocket != 0:
				mode(t, file, 0o600)
			}
		}
		if info, err := os.Lstat(filepath.Join(box.Folder, "browser.sock")); err != nil || info.Mode()&fs.ModeSocket == 0 {
			t.Errorf("browser.sock is %v (%v), want the supervisor's socket", info, err)
		}
		closed(t, box)

		box.Stop(t)
		for _, dir := range []string{"profile", "output", "artifacts"} {
			if err := os.RemoveAll(filepath.Join(box.Folder, dir)); err != nil {
				t.Fatal(err)
			}
		}
		var got supervisedAnswer
		box.Probe(t).Do(map[string]any{"op": "supervise", "launch": filepath.Join(box.Folder, "launch.json"),
			"umask": 0o022}, &got)
		t.Cleanup(func() {
			if s := box.Supervisor(t); s.Chromium != nil && s.Chromium.PID > 0 {
				_ = procgroup.Kill(s.Chromium.PID)
				procgrouptest.AwaitGone(t, s.Chromium.PID, goneWithin)
			}
		})
		if got.State != "ready" {
			t.Fatalf("the probe's supervisor answered %+v, want ready", got)
		}
		closed(t, box)
	})
}

// THE BOX'S ENVIRONMENT CANNOT CHANGE HOW ITS BROWSER RUNS: ensure run under
// the variables that change how Playwright launches (headed, legacy
// screenshots, no library check, a Selenium hub, protocol logs to a file,
// code loaded into every node) answers ready; its browser runs the listed
// command line; its supervisor holds exactly the variables the box set of the
// engine's one list for every child, which config.json carries whole, and
// Playwright's browsers directory; and nothing it was handed ran or logged,
// in npm's node either, on each Node's own spawn.
//
// Mutations, each red: pass ensure's environment through; a deny-list of
// the PLAYWRIGHT_ and PW_ prefixes in place of the allowlist (PWDEBUG passes);
// a list of the helper's own that leaves NO_PROXY out.
func TestTheBoxsEnvironmentCannotChangeHowItsBrowserRuns(t *testing.T) {
	config := map[string]any{}
	if err := json.Unmarshal(writtenConfig(t), &config); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range config["supervisor_env"].([]any) {
		names = append(names, n.(string))
	}
	if want := slices.Concat(hostbox.PassthroughEnv, hostbox.HostUserEnv); !slices.Equal(names, want) {
		t.Errorf("config.json's supervisor_env is %q, want hostbox.PassthroughEnv then hostbox.HostUserEnv's names, %q",
			names, want)
	}
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		dir := t.TempDir()
		marker, debugLog := filepath.Join(dir, "marked"), filepath.Join(dir, "debug.log")
		module := filepath.Join(dir, "mark.cjs")
		if err := os.WriteFile(module, []byte("require('fs').writeFileSync("+strconv.Quote(marker)+", 'loaded')\n"),
			0o644); err != nil {
			t.Fatal(err)
		}
		got := box.Ensure(t, "PWDEBUG=1", "PLAYWRIGHT_LEGACY_SCREENSHOT=1", "PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1",
			"SELENIUM_REMOTE_URL=http://127.0.0.1:1", "DEBUG=pw:*", "DEBUG_FILE="+debugLog,
			"NODE_OPTIONS=--require="+module, "LANG=C.UTF-8", "TZ=UTC", "NO_PROXY=example.com")
		if got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v, want ready", got)
		}
		if argv, want := browserArgv(t, box), commandLineFor(box, browser.SandboxWaived); !slices.Equal(argv, want) {
			t.Errorf("the browser runs\n%q\nwant\n%q", argv, want)
		}
		gotNames, err := boxhelpertest.EnvironNames(box.Supervisor(t).PID)
		if errors.Is(err, boxhelpertest.ErrNoEnviron) {
			t.Logf("%v: the supervisor's environment is read on Linux alone", err)
		} else if err != nil {
			t.Fatal(err)
		} else if want := []string{"HOME", "LANG", "NO_PROXY", "PATH", "PLAYWRIGHT_BROWSERS_PATH", "TZ"}; !slices.Equal(
			slices.Sorted(slices.Values(gotNames)), want) {
			t.Errorf("the supervisor's environment holds %q, want exactly %q", gotNames, want)
		}
		for _, file := range []string{marker, debugLog} {
			if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s exists (%v): the box's environment reached a node of the helper's", file, err)
			}
		}
	})
}

// writtenConfig is config.json as the engine writes it for a box, required.
func writtenConfig(t *testing.T) []byte {
	t.Helper()
	w := &memoryBox{}
	if err := boxhelper.Write(t.Context(), w, "/home/user", boxhelper.Settings{Sandbox: browser.SandboxRequired}); err != nil {
		t.Fatal(err)
	}
	return w.files["/home/user/.crewlet/browser-box/config.json"]
}

// memoryBox is a box that keeps what is written into it.
type memoryBox struct{ files map[string][]byte }

func (m *memoryBox) WriteFile(_ context.Context, path string, content []byte) error {
	if m.files == nil {
		m.files = map[string][]byte{}
	}
	m.files[path] = content
	return nil
}

// refusal is the probe's answer to a step that may withhold: ok, or why.
type refusal struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Cause  string `json:"cause"`
	Holder string `json:"holder"`
}

// A BOX WHOSE FOLDER IS NOT ITS USER'S ALONE SAYS WHY: ensure's hold of its
// folder answers folder_unusable not_owned for a folder another user owns, and
// mode where it cannot hold the folder or a file in it to the box user's
// modes, and holds a folder of its own: causes no case can provoke on a
// machine it does not own, so the probe stages them, as the kernel reports a
// foreign owner and refuses a chmod. A link in the folder's place and a path
// too long for its sockets are TestEnsureNamesEachReasonItCanTell's, on disk.
//
// Mutation: read a foreign owner as mode.
func TestABoxWhoseFolderIsNotItsUsersAloneSaysWhy(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		for _, tc := range []struct {
			name  string
			fail  *fault
			cause string
		}{
			{"a folder another user owns", &fault{Method: "lstat", UID: 4242}, "not_owned"},
			{"a folder it cannot hold", &fault{Method: "chmod", Errno: "EPERM"}, "mode"},
			{"a file in it it cannot hold", &fault{Method: "chmod", Errno: "EPERM", After: 1}, "mode"},
			{"its own", nil, ""},
		} {
			var got refusal
			probe.Do(struct {
				Op     string `json:"op"`
				Folder string `json:"folder"`
				Fail   *fault `json:"fail,omitempty"`
			}{"folder", box.Folder, tc.fail}, &got)
			want := refusal{OK: true}
			if tc.cause != "" {
				want = refusal{Reason: "folder_unusable", Cause: tc.cause}
			}
			if got != want {
				t.Errorf("%s: ensure's hold answered %+v, want %+v", tc.name, got, want)
			}
		}
	})
}

// A STALE LOCK IS TAKEN OVER BY ONE ENSURE AT A TIME: two ensures that judge
// one stale lock at once, ordered as two processes can be (both read it
// before either removes it, and the second would remove it only after the
// first has taken the lock), never both hold it: one takes it, and the other
// waits on it until it is given back, then takes it. On the probe's clock,
// two lock steps of the probe's own, a holder live where it is the probe.
//
// Mutation: take a stale lock over with no ensure.lock.takeover (the second
// removes the first's lock, and both take it).
func TestAStaleLockIsTakenOverByOneEnsureAtATime(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		// A lock this test binary holds, a process running no ensure.
		held := fmt.Sprintf(`{"pid":%d,"started_ms":%d}`, os.Getpid(), time.Now().UnixMilli())
		if err := os.WriteFile(filepath.Join(box.Folder, "ensure.lock"), []byte(held), 0o600); err != nil {
			t.Fatal(err)
		}
		var got struct{ Order []string }
		box.Probe(t).Do(map[string]any{"op": "takeover", "folder": box.Folder}, &got)
		first, other := "B", "C"
		if len(got.Order) > 0 && got.Order[0] == "C took" {
			first, other = "C", "B"
		}
		if want := []string{first + " took", first + " gave it back", other + " took"}; !slices.Equal(got.Order, want) {
			t.Errorf("two ensures that judged one lock stale went %q, want %q: one holder at a time", got.Order, want)
		}
	})
}

// A LOCK STEP THAT CANNOT WATCH OR WRITE STILL ANSWERS: ensure's lock step
// takes a free lock, and takes over one no ensure holds, though the kernel
// refuses its watch of the folder (inotify's limits, which a box busy with
// the model's own watchers meets), waking on its recheck alone; and a lock it
// cannot write or remove is folder_unusable, no_space for a full disk and
// unwritable for a refused path, never an error that leaves ensure no line.
// On the probe's clock, which no row advances: none waits.
//
// Mutations, each red: rethrow the refused watch; rethrow a failed write.
func TestALockStepThatCannotWatchOrWriteStillAnswers(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		lock := filepath.Join(box.Folder, "ensure.lock")
		// leftBy is a lock this test binary holds, a process running no ensure.
		leftBy := fmt.Sprintf(`{"pid":%d,"started_ms":%d}`, os.Getpid(), time.Now().UnixMilli())
		for _, tc := range []struct {
			name  string
			held  string // what ensure.lock holds first, "" for none
			watch string // the errno the folder's watch is refused with
			fail  *fault
			cause string // "" takes the lock
		}{
			{name: "a free lock, its watch refused", watch: "EMFILE"},
			{name: "a lock no ensure holds, its watch refused", held: leftBy, watch: "ENOSPC"},
			{name: "a disk that fills", fail: &fault{Method: "writeFile", Errno: "ENOSPC"}, cause: "no_space"},
			{name: "a read-only file system", fail: &fault{Method: "writeFile", Errno: "EROFS"}, cause: "unwritable"},
			{name: "a lock it may not remove", held: leftBy, fail: &fault{Method: "rm", Errno: "EACCES"},
				cause: "unwritable"},
		} {
			if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if tc.held != "" {
				if err := os.WriteFile(lock, []byte(tc.held), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var got refusal
			probe.Do(struct {
				Op     string `json:"op"`
				Folder string `json:"folder"`
				Fail   *fault `json:"fail,omitempty"`
				Watch  string `json:"watch,omitempty"`
			}{"lock", box.Folder, tc.fail, tc.watch}, &got)
			if tc.cause != "" {
				if got != (refusal{Reason: "folder_unusable", Cause: tc.cause}) {
					t.Errorf("%s: the lock step answered %+v, want folder_unusable %s", tc.name, got, tc.cause)
				}
				continue
			}
			var holder struct {
				PID int `json:"pid"`
			}
			if err := json.Unmarshal([]byte(got.Holder), &holder); !got.OK || err != nil || holder.PID == os.Getpid() {
				t.Errorf("%s: the lock step answered %+v, want the lock taken, naming the probe", tc.name, got)
			}
		}
	})
}
