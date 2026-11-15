package executor_test

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/words"
)

// The calls an OpenCode run's script makes to its run's own server, each
// bounded (crewlet#1088), over the fake opencode of opencode_script_test.go
// ([fakeOpenCode]), whose server takes the kinds FAKE_HANG names and never
// answers them.

// boundedTimes runs a case's script with every call to its server bounded at a
// second, the least its whole-second clock reads, and the login loop out of the
// way unless the case brings it back.
var boundedTimes = executor.OpenCodeTimes{Call: time.Second, SessionCall: time.Second, Sync: time.Hour}

// hungCalls reads the calls the case's server took and never answered: the
// kind of each, and the pid of the client that made it.
func hungCalls(t *testing.T, dir string) (kinds []string, pids []int) {
	t.Helper()
	for line := range strings.Lines(readIn(t, dir, "hung")) {
		kind, pid, ok := strings.Cut(strings.TrimSpace(line), " ")
		n, err := strconv.Atoi(pid)
		if !ok || err != nil {
			t.Fatalf("an unreadable record of a hung call: %q", line)
		}
		kinds, pids = append(kinds, kind), append(pids, n)
	}
	return kinds, pids
}

// runs reports whether pid is a process that runs, read from /proc: a zombie
// runs nothing, and is no leftover. False where there is no /proc to read, as
// on darwin, where the group's own check ([scriptJob.assertGroupGone]) stands.
func runs(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) //nolint:gosec // /proc
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	fields := strings.Fields(string(stat[end+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// assertHungEnded fails the case unless the server took a call of kind and
// never answered it, and every call it never answered is over once the script
// is: ended by the script's bound, and reaped, rather than left running in the
// job's group.
func assertHungEnded(t *testing.T, dir, kind string) {
	t.Helper()
	kinds, pids := hungCalls(t, dir)
	if !strings.Contains(" "+strings.Join(kinds, " ")+" ", " "+kind+" ") {
		t.Fatalf("the server never took a %s call to leave unanswered (its record: %q), so nothing was tested", kind,
			readIn(t, dir, "hung"))
	}
	for i, pid := range pids {
		if runs(pid) {
			t.Errorf("the %s call the server never answered (pid %d) still runs after the script ended", kinds[i], pid)
		}
	}
}

// A CALL ITS SERVER NEVER ANSWERS GOES AS A CALL TO A SERVER THAT IS GONE GOES,
// for every kind of call the script makes to its run's own server, under every
// sh a box may run it with. Unbounded, a server that took a call and never
// answered it held the script there until the job was stopped: the readiness
// loop past its own deadline, which it read only between calls; the usage
// before and after the run; the leftover steers; the export, and the capture
// with it; the pages that measure the session; the login's exports. Each is
// ended at its bound and reaped, the script goes on as it goes on where the
// server is gone, and nothing of the call is left in the job's group.
//
// Mutation: run any one of the calls unbounded, and its case waits out the
// fake's hour; end it and not reap it, and its pid is left.
func TestACallItsServerNeverAnswersGoesAsACallToAGoneServerGoes(t *testing.T) {
	t.Parallel()
	const (
		pending = `"time":{"created":1793072423854},"type":"user","payload":{"text":"x"},"delivery":"steer"`
		first   = "msg_30292d427a87e479be9f88cba42963ca"
		second  = "msg_05536338060eb18a87638c49f196ffcf"
		third   = "msg_8a2c0f4e9b1d7a6c3e5f0b2d4a6c8e0f"
	)
	inbox := `{"data":[{"id":"` + first + `",` + pending + `},{"id":"` + second + `",` + pending + `},{"id":"` +
		third + `",` + pending + `}]}`
	steered := sandbox.RunRequest{MCPServers: bridgeOnly, Steer: steerWiring}
	// THE READINESS CHECK, A CALL A POLL, is held to its own bound and to what
	// is left of the loop's: its first call left unanswered and every later one
	// answered, the run starts at the call's bound, long before the loop's; and
	// every call left unanswered, the loop ends at its own deadline, long
	// before a call's.
	readyAfterOne := boundedTimes
	readyAfterOne.MCP = time.Minute
	readyAtDeadline := boundedTimes
	readyAtDeadline.MCP, readyAtDeadline.Call = 2*time.Second, time.Minute
	for _, tc := range []struct {
		name, kind string
		times      executor.OpenCodeTimes
		env        []string
		opts       scriptOpts
		check      func(t *testing.T, run scriptRun, paths executor.Paths)
	}{
		{name: "mcp.list held to its own bound", kind: "mcp.list", times: readyAfterOne,
			env:  []string{"FAKE_HANG_ONCE=1"},
			opts: scriptOpts{req: sandbox.RunRequest{MCPServers: bridgeOnly}},
			check: func(t *testing.T, run scriptRun, _ executor.Paths) {
				if strings.Contains(run.stderr, "still connecting") || run.callIndex("run ") < 0 {
					t.Errorf("the readiness loop did not go on past a call at its bound to the run:\n%s\ncalls:\n%s",
						run.stderr, strings.Join(run.calls, "\n"))
				}
			}},
		{name: "mcp.list held to the loop's deadline", kind: "mcp.list", times: readyAtDeadline,
			opts: scriptOpts{req: sandbox.RunRequest{MCPServers: bridgeOnly}},
			check: func(t *testing.T, run scriptRun, _ executor.Paths) {
				if !strings.Contains(run.stderr, "an MCP server was still connecting after 2s; the run starts without it") ||
					run.callIndex("run ") < 0 {
					t.Errorf("the readiness loop did not end at its deadline and start the run:\n%s\ncalls:\n%s",
						run.stderr, strings.Join(run.calls, "\n"))
				}
			}},
		{name: "session.get", kind: "session.get", times: boundedTimes,
			opts: scriptOpts{session: &continuingOpenCode, held: []string{carriedOpenCode},
				req: sandbox.RunRequest{Resume: carriedOpenCode}},
			check: func(t *testing.T, run scriptRun, paths executor.Paths) {
				for _, event := range []string{`"type":"crewlet.usage_before"`, `"type":"crewlet.usage"`} {
					if strings.Contains(run.stdout, event) {
						t.Errorf("the stream holds %s from a call the server never answered:\n%s", event, run.stdout)
					}
				}
				assertCaptured(t, run, paths)
			}},
		{name: "session.inbox.list", kind: "session.inbox.list", times: boundedTimes, opts: scriptOpts{req: steered, env: []string{"FAKE_INBOX=" + inbox}},
			check: func(t *testing.T, run scriptRun, paths executor.Paths) {
				if n := countCalls(run.calls, "api --server http://127.0.0.1:4242 session.inbox.cancel"); n != 0 {
					t.Errorf("%d notes were cancelled after a listing the server never answered", n)
				}
				assertCaptured(t, run, paths)
			}},
		{name: "session.inbox.cancel", kind: "session.inbox.cancel", times: boundedTimes, opts: scriptOpts{req: steered, env: []string{"FAKE_INBOX=" + inbox}},
			check: func(t *testing.T, run scriptRun, paths executor.Paths) {
				if n := countCalls(run.calls, "api --server http://127.0.0.1:4242 session.inbox.cancel"); n != 1 {
					t.Errorf("%d cancels were asked, want 1: a server that answered none in its bound "+
						"is not asked again", n)
				}
				assertCaptured(t, run, paths)
			}},
		{name: "session.message.list", kind: "session.message.list", times: boundedTimes,
			opts: scriptOpts{pages: map[string]string{"first": openCodePage(openCodeSettled("msg_1", 1015, 200, 0))}},
			check: func(t *testing.T, run scriptRun, paths executor.Paths) {
				if strings.Contains(run.stdout, `"type":"crewlet.context"`) {
					t.Errorf("the stream holds a page the server never answered:\n%s", run.stdout)
				}
				assertCaptured(t, run, paths)
			}},
		{name: "session.export", kind: "session.export", times: boundedTimes,
			check: func(t *testing.T, run scriptRun, paths executor.Paths) {
				if !strings.Contains(run.stderr, "was not exported to be continued in another box") {
					t.Errorf("an export past its bound was not said:\n%s", run.stderr)
				}
				if _, err := os.Stat(paths.SessionCapture()); !os.IsNotExist(err) {
					t.Errorf("an export past its bound left a capture (%v)", err)
				}
			}},
		{name: "auth.export", kind: "auth.export", times: boundedTimes, opts: scriptOpts{login: seededLogin, env: []string{"FAKE_REFRESH=" + refreshedLogin}},
			check: func(t *testing.T, run scriptRun, _ executor.Paths) {
				if run.document != seededLogin {
					t.Errorf("the box's login is %q, want the one it was given: no export the server never "+
						"answered replaces it", run.document)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forEachShell(t, func(t *testing.T, sh boxShell) {
				o := tc.opts
				o.shell = sh
				times := tc.times
				o.times = &times
				o.env = append(append(append([]string{}, o.env...), tc.env...), "FAKE_HANG="+tc.kind)
				j := startScript(t, o)
				run := j.waitWithin(promptEnd, "a call its server never answered held the script")
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				tc.check(t, run, j.paths)
				assertHungEnded(t, run.dir, tc.kind)
				j.assertGroupGone()
			})
		})
	}
}

// assertCaptured fails the case unless the run's session was exported through
// its server and the capture moved into place: what a call past its bound
// before the export must not cost.
func assertCaptured(t *testing.T, run scriptRun, paths executor.Paths) {
	t.Helper()
	if run.callIndex("session export --server ") < 0 {
		t.Fatalf("the session was never exported:\n%s", strings.Join(run.calls, "\n"))
	}
	if _, err := os.Stat(paths.SessionCapture()); err != nil {
		t.Errorf("the session was not captured: %v\n%s", err, run.stderr)
	}
}

// AN EXPORT PAST ITS BOUND COSTS THE BOUND, NEVER THE SESSION. The script ends
// the export, moves no capture into place and stops the server as it goes, so
// the read that takes the session finds none and packs one on demand, beside a
// database no server holds any more: the same session, whole.
//
// Mutation: leave the ended export's archive in place, and the capture is a
// gzip of nothing; leave the server running, and the pack meets it.
func TestAnExportPastItsBoundLeavesTheSessionToThePackOnDemand(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, times: &boundedTimes, env: []string{"FAKE_HANG=session.export"}})
		run := j.waitWithin(promptEnd, "an export its server never answered held the script")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		assertHungEnded(t, run.dir, "session.export")
		_, session, _ := strings.Cut(run.calls[run.callIndex("run ")], "--session ")
		session, _, _ = strings.Cut(session, " ")
		dest := j.paths.SessionPackHeld("on-demand")
		pack := executor.OpenCode{}.Pack(j.paths, session, j.paths.SessionPackStaging("on-demand"), dest, false)
		cmd := exec.Command("sh", "-c", pack) //nolint:gosec // the pack under test
		cmd.Dir = run.dir
		cmd.Env = []string{"PATH=" + filepath.Join(run.dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + run.home,
			"FAKE_DIR=" + run.dir}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the pack on demand failed: %v\n%s", err, out)
		}
		if export := gunzipped(t, readFileBytes(t, dest)); !strings.Contains(string(export), `"id":"`+session+`"`) {
			t.Errorf("the pack on demand holds %q, want the session %s", export, session)
		}
		if exported := readIn(t, run.dir, "exported"); !strings.Contains(exported, session+" stopped") {
			t.Errorf("the pack on demand ran beside a server (exports: %q), want one the script had stopped", exported)
		}
	})
}

// readFileBytes is a file of the case's own, whole.
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // under the test's own dir
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// AN END REQUEST ENDS A CALL BEFORE THE RUN AT ONCE. A request that comes while
// the run waits for its MCP servers, or for what its continued session had
// spent, starts no run, and leaves the run's own tail to export the session:
// so the call it finds in flight is ended then, never waited out to its bound.
// Only that first call hangs: the tail's calls of the same kind, which an end
// request never ends, are answered.
//
// Mutation: bound the readiness loop's call or the usage before the run, but
// never end it on a request, and the case waits out the bound's minute.
func TestAnEndRequestEndsACallBeforeTheRunAtOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind string
		opts scriptOpts
	}{
		"the readiness loop's": {kind: "mcp.list", opts: scriptOpts{req: sandbox.RunRequest{MCPServers: bridgeOnly}}},
		"the usage before the run": {kind: "session.get", opts: scriptOpts{session: &continuingOpenCode,
			held: []string{carriedOpenCode}, req: sandbox.RunRequest{Resume: carriedOpenCode}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forEachShell(t, func(t *testing.T, sh boxShell) {
				o := tc.opts
				o.shell = sh
				o.times = &executor.OpenCodeTimes{Call: time.Minute, MCP: time.Minute, Sync: time.Hour}
				o.env = []string{"FAKE_HANG=" + tc.kind, "FAKE_HANG_ONCE=1"}
				j := startScript(t, o)
				j.await("the server to take a call it never answers", func() bool {
					return strings.Contains(readIn(t, j.dir, "hung"), tc.kind+" ")
				})
				j.signalScript(syscall.SIGUSR1)
				run := j.waitWithin(promptEnd, "an end request waited out a call it should have ended")
				if run.callIndex("run ") >= 0 {
					t.Errorf("a request before the run started one:\n%s", strings.Join(run.calls, "\n"))
				}
				assertHungEnded(t, run.dir, tc.kind)
				j.assertGroupGone()
			})
		})
	}
}

// A HUNG INTERRUPT KEEPS NO END FROM ENDING. The end request interrupts the
// session through the run's server first, so the step in flight settles; a
// server that never answers that call has the step left as a server that is
// gone leaves it, and the run's client is ended within the grace all the same,
// and the tail runs: the session exported, the script gone.
//
// Mutation: run the interrupt unbounded, and the end waits on the server until
// the case's bound.
func TestAHungInterruptKeepsNoEndFromEnding(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, times: &boundedTimes,
			env: []string{"FAKE_HANG=session.interrupt", "FAKE_RUN_UNTIL=never"}})
		j.await("the run to start", func() bool {
			_, err := os.Stat(filepath.Join(j.dir, "refreshed"))
			return err == nil
		})
		j.signalScript(syscall.SIGUSR1)
		run := j.waitWithin(promptEnd, "an end waited on an interrupt its server never answered")
		if got := strings.TrimSpace(readIn(t, filepath.Dir(j.paths.JobEnded()), filepath.Base(j.paths.JobEnded()))); got != "asked" {
			t.Errorf("the job's end was recorded as %q, want asked: its client ended within the grace", got)
		}
		assertCaptured(t, run, j.paths)
		assertHungEnded(t, run.dir, "session.interrupt")
		j.assertGroupGone()
	})
}

// A CALL DEAF TO ITS END IS KILLED, AND REAPED. A client that ignores the
// SIGTERM its bound ends it with is given the grace every process of the run's
// own is given, then SIGKILL, and is waited for, so it is no more left in the
// job's group than one that went at once.
//
// Mutation: end the call with SIGTERM alone, and the deaf client still runs.
func TestACallDeafToItsEndIsKilledAndReaped(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		times := boundedTimes
		times.Stop = time.Second
		j := startScript(t, scriptOpts{shell: sh, times: &times,
			env: []string{"FAKE_HANG=session.get", "FAKE_HANG_DEAF=1"}})
		run := j.waitWithin(promptEnd, "a call deaf to its end held the script")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		assertHungEnded(t, run.dir, "session.get")
		j.assertGroupGone()
	})
}

// A LOGIN EXPORT IN FLIGHT AS THE LOOP IS STOPPED IS ENDED BEFORE THE LOOP GOES.
// The loop's export is a bounded call, so the loop is between its polls, not in
// a foreground command of the export's own, when the run's tail stops it:
// gone then without the call, it left the export running in the job's group.
//
// And a teardown's signal, which reaches every process of the job, ends the
// last export at once: its subshell says the call is over as it goes.
//
// Mutation: drop unbound from the loop's trap, and the loop's export outlives
// it; drop the subshell's trap, and the teardown waits out two graces.
func TestALoginExportInFlightAsTheLoopStopsIsEnded(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		times := boundedTimes
		times.Sync, times.Call = 100*time.Millisecond, time.Minute
		j := startScript(t, scriptOpts{shell: sh, login: seededLogin, times: &times,
			env: []string{"FAKE_HANG=auth.export", "FAKE_RUN_UNTIL=released"}})
		j.await("the loop's export to be taken and never answered", func() bool {
			return strings.Contains(readIn(t, j.dir, "hung"), "auth.export ")
		})
		if err := os.WriteFile(filepath.Join(j.dir, "released"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// The loop is stopped before the last export starts, so by the time the
		// server takes that one, the loop's must be over: read then, before the
		// teardown below signals the whole group, which would end it anyway.
		j.await("the last export to be taken too", func() bool {
			return strings.Count(readIn(t, j.dir, "hung"), "auth.export ") >= 2
		})
		if _, pids := hungCalls(t, j.dir); runs(pids[0]) {
			t.Errorf("the loop's export (pid %d) still runs after the loop was stopped", pids[0])
		}
		// The last export, through the same server, waits out its own minute
		// unless the case ends the script first: a teardown, which ends every
		// call in flight on its way.
		j.signal(syscall.SIGTERM)
		// THE TEARDOWN'S SIGNAL REACHES THE CALL'S SUBSHELL TOO, which says the
		// call is over as it goes: the script's own stop of the call then
		// finds it over at once, and spends none of the grace it gives a call
		// that does not go (two of them, a TERM's and a KILL's).
		run := j.waitWithin(sandbox.JobStopGrace+2*time.Second,
			"a teardown spent its graces on a call the group's signal had already ended")
		if run.document != seededLogin {
			t.Errorf("the box's login is %q, want the one it was given", run.document)
		}
		assertHungEnded(t, run.dir, "auth.export")
		j.assertGroupGone()
	})
}

// openCodeCalls is every place on line where a command word calls OpenCode:
// the text before each, and the text after it to the line's end. Every one,
// since a line may hold two (the leftover steers' listing and their cancels).
func openCodeCalls(line string) (before, after []string) {
	const word = "opencode "
	for at := 0; ; {
		i := strings.Index(line[at:], word)
		if i < 0 {
			return before, after
		}
		i += at
		if i == 0 || !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-", rune(line[i-1])) {
			before, after = append(before, line[:i]), append(after, line[i+len(word):])
		}
		at = i + len(word)
	}
}

// EVERY CALL TO THE RUN'S SERVER IS BOUNDED, in the script as a run renders it:
// every `opencode` a line of it runs against the run's own server, `--server
// "$url"`, is the bounded helper's, but the harness itself, which the end
// protocol bounds; and no answer of one is read by a command substitution,
// which would run the call in a subshell an end request never reaches. A
// call added to the script without the helper fails here before any server
// can hold it.
//
// Mutation: call any of the script's server calls as it was called before
// crewlet#1088, and its line is named here.
func TestEveryCallToTheRunsServerIsBounded(t *testing.T) {
	t.Parallel()
	session := executor.OpenCode{}.Session(sandbox.RunRequest{Resume: carriedOpenCode})
	script := executor.OpenCode{}.Command(sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly,
		Steer: steerWiring}, session, executor.Paths{Home: "/box"}, "")
	bounded := regexp.MustCompile(`bounded [a-z]+ (?:[0-9]+|"\$[a-z]+") [e-] $`)
	var seen int
	for _, line := range strings.Split(scriptOf(t, script), "\n") {
		befores, afters := openCodeCalls(line)
		for i, rest := range afters {
			if !strings.Contains(firstCommand(rest), `--server "$url"`) || strings.HasPrefix(rest, "run ") {
				continue
			}
			seen++
			if !bounded.MatchString(befores[i]) {
				t.Errorf("a call to the run's server is not bounded:\n%s", line)
			}
			if strings.HasSuffix(befores[i], "$(") {
				t.Errorf("a call to the run's server is read by a command substitution:\n%s", line)
			}
		}
		if strings.Contains(line, "keep ") && strings.Contains(line, `--server "$url"`) &&
			!regexp.MustCompile(`keep (sync|last) `).MatchString(line) {
			t.Errorf("a login export through the run's server goes past keep's bound:\n%s", line)
		}
	}
	if seen < 8 {
		t.Errorf("only %d calls to the run's server were found in the script, so the scan read nothing it should", seen)
	}
}

// firstCommand is text up to the end of its first simple command: the next
// `;`, `&&`, `||`, `|` or `)` outside quotes, near enough for a line of the
// script, whose quoted words hold none of them.
func firstCommand(text string) string {
	quote := byte(0)
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '|' || c == '&' || c == ')':
			return text[:i]
		}
	}
	return text
}

// AN ENDED RUN'S PATH FITS THE ENGINE'S WAIT. The engine waits
// [sandbox.JobEndWait] for a job its end request reached before it kills the
// job and packs its session on demand; every call on the script's way out at
// its bound, and every process it stops at the grace, still end inside it, so
// the script's own export is never cut short by the kill. And the way out the
// sum counts is the script's own: every call the rendered script makes to its
// server that no end request ends — a `bounded` call whose ENDS is `-` — is one
// the sum holds, and the sum holds no other, so a call added to the way out is
// counted or this goes red.
//
// Mutation: raise any of the calls' bounds past what the wait holds, add a call
// to the sum, or add one to the script's way out alone, and this goes red.
func TestAnEndedRunsPathFitsTheEnginesWait(t *testing.T) {
	t.Parallel()
	if path := executor.OpenCodeEndPath(); path > sandbox.JobEndWait {
		t.Errorf("an ended OpenCode run's path takes up to %s, past the %s the engine waits for an ended job",
			path, sandbox.JobEndWait)
	}
	session := executor.OpenCode{}.Session(sandbox.RunRequest{Resume: carriedOpenCode})
	script := scriptOf(t, executor.OpenCode{}.Command(sandbox.RunRequest{Resume: carriedOpenCode,
		MCPServers: bridgeOnly, Steer: steerWiring}, session, executor.Paths{Home: "/box"}, ""))
	unended := map[string]bool{}
	for _, m := range regexp.MustCompile(`bounded ("\$[a-z]+"|[a-z]+) (?:[0-9]+|"\$[a-z]+") - `).FindAllStringSubmatch(script, -1) {
		unended[m[1]] = true
	}
	got := slices.Sorted(maps.Keys(unended))
	if want := executor.OpenCodeEndCallTags(); !slices.Equal(got, want) {
		t.Errorf("the script's calls no end request ends are %s, and the end path sums %s: each call on the "+
			"way out is one the sum must hold", words.And(got), words.And(want))
	}
}
