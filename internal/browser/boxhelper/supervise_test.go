package boxhelper_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
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
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// lockHolder is the pid profile's SingletonLock names on this host.
func lockHolder(t *testing.T, profile string) int {
	t.Helper()
	target, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		t.Fatalf("the profile holds no SingletonLock: %v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(target, host+"-"))
	if err != nil {
		t.Fatalf("SingletonLock names %q, no process of this host, %s", target, host)
	}
	return pid
}

// A STALE CHROMIUM HOLDING THE PROFILE IS KILLED BEFORE A LAUNCH: a Chromium
// left running on the box's profile, as a supervisor that died outright
// leaves one, is killed with its process group before the supervisor
// launches, so the box's browser starts rather than finding its profile held.
//
// Mutation: skip the stale kill (the launch reads as profile_held).
func TestAStaleChromiumHoldingTheProfileIsKilledBeforeALaunch(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		profile := filepath.Join(box.Folder, "profile")
		chromiumtest.LaunchPort(t, env.Chrome, "--user-data-dir="+profile)
		orphan := lockHolder(t, profile)
		if !slices.Contains(boxhelpertest.Argv(t, orphan, env.Chrome), "--user-data-dir="+profile) {
			t.Fatalf("SingletonLock names pid %d, which holds no --user-data-dir=%s", orphan, profile)
		}
		got := box.Ensure(t)
		if got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v, want ready on a profile the orphan held", got)
		}
		if !procgrouptest.AwaitGone(t, orphan, goneWithin) {
			t.Errorf("the orphan, pid %d, still runs", orphan)
		}
		if s := box.Supervisor(t); s.Chromium == nil || s.Chromium.PID == orphan || lockHolder(t, profile) != s.Chromium.PID {
			t.Errorf("the supervisor records %+v, and the profile's lock %d, want its own browser holding the profile",
				s.Chromium, lockHolder(t, profile))
		}
	})
}

// A PROFILE HELD ON ANOTHER HOST IS NEVER TAKEN OVER: a SingletonLock naming
// another host is left alone, whatever process of this host its pid names
// (here one carrying the profile's switch, as a Chromium on it would), and
// the launch that finds the profile held says so: profile_held.
//
// Mutation: kill on any lock (the process the pid names here dies).
func TestAProfileHeldOnAnotherHostIsNeverTakenOver(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		profile := filepath.Join(box.Folder, "profile")
		if err := os.MkdirAll(profile, 0o700); err != nil {
			t.Fatal(err)
		}
		decoy := exec.CommandContext(t.Context(), "/bin/sh", "-c", "read -r line", "sh", "--user-data-dir="+profile)
		procgroup.Set(decoy)
		stdin, err := decoy.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := decoy.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdin.Close()
			_ = decoy.Wait()
		})
		lock := "another-host.invalid-" + strconv.Itoa(decoy.Process.Pid)
		if err := os.Symlink(lock, filepath.Join(profile, "SingletonLock")); err != nil {
			t.Fatal(err)
		}
		got := box.Ensure(t)
		if got.Verdict != browser.EnsureWithheld || got.Reason != browser.ReasonChromiumStartFailed ||
			got.Cause != browser.CauseProfileHeld {
			t.Errorf("ensure answered %+v, want withheld chromium_start_failed profile_held", got)
		}
		if running, err := procgrouptest.Running(decoy.Process.Pid); err != nil || !running {
			t.Errorf("pid %d, which a lock of another host names, was killed (%v)", decoy.Process.Pid, err)
		}
	})
}

// event is one entry a supervisor logged, as the probe reports it.
type event struct {
	Kind string `json:"kind"`
	PID  int    `json:"pid"`
	Sent bool   `json:"sent"`
	Hook string `json:"hook"`
}

// supervisedAnswer is the probe's answer about the supervisor it runs.
type supervisedAnswer struct {
	State    string         `json:"state"`
	Events   []event        `json:"events"`
	Reports  []string       `json:"reports"`
	Attached []attachedCall `json:"attached"`
	Killed   int            `json:"killed"`
	PID      int            `json:"pid"`
	Frozen   int            `json:"frozen"`
	Pages    int            `json:"pages"`
	Refused  string         `json:"refused"`
}

// attachedCall is one call the probe's recording attachment was told, with
// what the supervisor showed then: its state and whether browser.sock was
// there; at attach, its boot; for a browser, whether the supervisor serves it
// and its pages; and a call's job, action and answer's text.
type attachedCall struct {
	Hook    string `json:"hook"`
	State   string `json:"state"`
	Socket  bool   `json:"socket"`
	Boot    string `json:"boot"`
	Current bool   `json:"current"`
	Pages   int    `json:"pages"`
	Job     string `json:"job"`
	Action  string `json:"action"`
	Text    string `json:"text"`
}

// calls is every recorded call of hook, in the order it was told.
func (a supervisedAnswer) calls(hook string) []attachedCall {
	var out []attachedCall
	for _, c := range a.Attached {
		if c.Hook == hook {
			out = append(out, c)
		}
	}
	return out
}

// kinds is how many events of kind the answer holds.
func (a supervisedAnswer) kinds(kind string) int {
	n := 0
	for _, e := range a.Events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// supervised starts a probe running the supervisor of box's launch in its own
// process, on the probe's clock, its profile's lock read through fail's file
// system, handed the probe's attachments live names: the launch box's ensure
// wrote, its own supervisor stopped first, so the probe's holds the profile.
// It answers the probe and the supervisor's first answer, ready or failed. Its
// browser's group is killed, frozen or not, once the case ends.
func supervised(t *testing.T, box *boxhelpertest.Box, rejectFirst bool, fail *fault, live ...string) (
	*boxhelpertest.Probe, supervisedAnswer) {
	t.Helper()
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	box.Stop(t)
	probe := box.Probe(t)
	var got supervisedAnswer
	probe.Do(map[string]any{"op": "supervise", "launch": filepath.Join(box.Folder, "launch.json"),
		"reject_first": rejectFirst, "fail": fail, "live": live}, &got)
	t.Cleanup(func() {
		if s := box.Supervisor(t); s.Chromium != nil && s.Chromium.PID > 0 {
			_ = procgroup.Kill(s.Chromium.PID)
			procgrouptest.AwaitGone(t, s.Chromium.PID, goneWithin)
		}
	})
	return probe, got
}

// supervising is supervised's probe once its supervisor's first launch is
// ready, as a case of what follows a launch needs it.
func supervising(t *testing.T, box *boxhelpertest.Box, rejectFirst bool) (*boxhelpertest.Probe, supervisedAnswer) {
	t.Helper()
	probe, got := supervised(t, box, rejectFirst, nil)
	if got.State != "ready" || !slices.Equal(got.Reports, []string{"ready " + box.Supervisor(t).Chromium.Version}) {
		t.Fatalf("the probe's supervisor answered %+v, want ready", got)
	}
	return probe, got
}

// onProfile fails t unless pid is a browser running on box's profile.
func onProfile(t *testing.T, box *boxhelpertest.Box, pid int) {
	t.Helper()
	profile := filepath.Join(box.Folder, "profile")
	if lockHolder(t, profile) != pid || !slices.Contains(boxhelpertest.Argv(t, pid, ""), "--user-data-dir="+profile) {
		t.Errorf("pid %d does not hold the box's profile", pid)
	}
}

// A CRASHED BROWSER COMES BACK ON ITS PROFILE THREE TIMES IN TEN MINUTES: a
// browser killed from under the supervisor is launched again on the same
// profile, at most three times in any ten minutes of the supervisor's clock;
// the fourth crash inside them crashes it out, and the browser it serves
// then is a refusal saying when it comes back. Relaunches the window has left
// behind no longer count. On the probe's clock, so ten minutes pass at once,
// on each Node's own timers and child processes.
//
// Mutations, each red: measure the window from the last relaunch, not the
// first it holds; count the fourth crash as a relaunch.
func TestACrashedBrowserComesBackOnItsProfileThreeTimesInTenMinutes(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Run("the fourth crash in the window crashes out", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			probe, _ := supervising(t, box, false)
			for i := 1; i <= 3; i++ {
				var got supervisedAnswer
				probe.Do(map[string]any{"op": "kill_chromium"}, &got)
				if got.State != "ready" || got.PID == got.Killed || got.PID == 0 {
					t.Fatalf("crash %d: the supervisor answered %+v, want the browser launched again", i, got)
				}
				onProfile(t, box, got.PID)
			}
			var got supervisedAnswer
			probe.Do(map[string]any{"op": "kill_chromium"}, &got)
			if got.State != "crashed_out" {
				t.Fatalf("the fourth crash: the supervisor answered %+v, want it crashed out", got)
			}
			probe.Do(map[string]any{"op": "context"}, &got)
			if !strings.Contains(got.Refused, "starts again when this box's next job starts") {
				t.Errorf("a crashed-out browser is served as %+v, want a refusal saying when it comes back", got)
			}
			if s := box.Supervisor(t); s.State != "crashed_out" || len(s.Relaunches) != 3 {
				t.Errorf("supervisor.json records %+v, want crashed_out after three relaunches", s)
			}
		})
		t.Run("relaunches the window has left behind no longer count", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			probe, _ := supervising(t, box, false)
			for i, advance := range []int{0, 400_000, 400_000, 100_000} {
				var moved struct{ Now int64 }
				probe.Do(map[string]any{"op": "advance", "ms": advance}, &moved)
				var got supervisedAnswer
				probe.Do(map[string]any{"op": "kill_chromium"}, &got)
				if got.State != "ready" {
					t.Fatalf("crash %d, %d ms on: the supervisor answered %+v, want the browser launched again", i+1,
						advance, got)
				}
			}
		})
	})
}

// A STOPPED SUPERVISOR TAKES ITS CHROMIUM WITH IT: a supervisor sent TERM
// closes its browser, removes its socket and records itself stopped before it
// exits, leaving no Chromium running, even one that never answers the close,
// which it kills with its group once the grace has passed: a frozen browser
// notices no closed pipe, and would outlive it. On the supervisor's own clock
// that kill comes at the grace, not before. One sent TERM while its first
// launch is under way stops once the launch settles, serving nothing.
//
// Mutations, each red: skip the kill (no KILL recorded sent); keep listening
// on the socket; report a launch's failure that comes after the stop (ensure
// reads the browser's timeout, and the record says failed).
func TestAStoppedSupervisorTakesItsChromiumWithIt(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, frozen := range []bool{false, true} {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
			if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
				t.Fatalf("ensure answered %+v", got)
			}
			s := box.Supervisor(t)
			if frozen {
				if err := procgroup.Stop(s.Chromium.PID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = procgroup.Kill(s.Chromium.PID) })
			}
			if err := procgroup.Terminate(s.PID); err != nil {
				t.Fatal(err)
			}
			if !procgrouptest.AwaitGone(t, s.PID, goneWithin) || !procgrouptest.AwaitGone(t, s.Chromium.PID, goneWithin) {
				t.Fatalf("frozen %t: the supervisor, pid %d, or its browser, pid %d, outlived a TERM", frozen, s.PID,
					s.Chromium.PID)
			}
			if _, err := os.Lstat(filepath.Join(box.Folder, "browser.sock")); !os.IsNotExist(err) {
				t.Errorf("frozen %t: browser.sock outlives the supervisor (%v)", frozen, err)
			}
			if got := box.Supervisor(t); got.State != "stopped" {
				t.Errorf("frozen %t: supervisor.json records %+v, want stopped", frozen, got)
			}
		}
	})
	t.Run("a TERM while its browser launches", func(t *testing.T) {
		boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{
				Route: boxhelpertest.NoChromium, Config: map[string]any{"launch_timeout_ms": 2000}})
			// The stand-in is launched by the supervisor itself, so its parent
			// is the supervisor, sent TERM while this launch is under way.
			standInChromium(t, box, "kill -TERM \"$PPID\"\nexec /bin/cat <&3 >/dev/null\n")
			got := box.Ensure(t)
			s := box.Supervisor(t)
			if got.Reason != browser.ReasonSupervisorFailed || got.Cause != browser.CauseExited || s.State != "stopped" {
				t.Errorf("ensure answered %+v and the supervisor records %+v, want it stopped once its launch "+
					"settled, having said nothing", got, s)
			}
			if !procgrouptest.AwaitGone(t, s.PID, goneWithin) {
				t.Errorf("the supervisor, pid %d, outlived its TERM", s.PID)
			}
			if _, err := os.Lstat(filepath.Join(box.Folder, "browser.sock")); !os.IsNotExist(err) {
				t.Errorf("a supervisor stopped as it launched left browser.sock (%v)", err)
			}
		})
	})
	t.Run("the kill at the grace, on the supervisor's clock", func(t *testing.T) {
		boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			probe, _ := supervising(t, box, false)
			var frozen supervisedAnswer
			probe.Do(map[string]any{"op": "freeze_chromium"}, &frozen)
			var got supervisedAnswer
			probe.Do(map[string]any{"op": "stop"}, &got)
			var moved struct{ Now int64 }
			probe.Do(map[string]any{"op": "advance", "ms": 4_999}, &moved)
			probe.Do(map[string]any{"op": "events"}, &got)
			if got.kinds("browser_killed") != 0 {
				t.Fatalf("the browser was killed before the grace ran out: %+v", got.Events)
			}
			probe.Do(map[string]any{"op": "advance", "ms": 1}, &moved)
			probe.Do(map[string]any{"op": "stopped"}, &got)
			killed := slices.ContainsFunc(got.Events, func(e event) bool {
				return e.Kind == "browser_killed" && e.PID == frozen.Frozen && e.Sent
			})
			if got.State != "stopped" || got.kinds("browser_killed") != 1 || !killed {
				t.Errorf("the supervisor answered %+v, want it stopped, a KILL sent to the frozen browser's group", got)
			}
			if !procgrouptest.AwaitGone(t, frozen.Frozen, goneWithin) {
				t.Errorf("the frozen browser, pid %d, outlived the stop", frozen.Frozen)
			}
			if _, err := os.Lstat(filepath.Join(box.Folder, "browser.sock")); !os.IsNotExist(err) {
				t.Errorf("browser.sock outlives the supervisor (%v)", err)
			}
		})
	})
}

// A REJECTION BETWEEN JOBS LEAVES THE SUPERVISOR RUNNING: a promise rejected
// with nobody to handle it, when no model is connected and Playwright MCP
// listens for none, or raised as the supervisor starts, before any of
// Playwright is loaded, is logged, and the supervisor and its browser run on
// and serve, under each Node's own rejection handling.
//
// Mutations, each red: install the policy after Playwright loads (the
// rejection raised at the start ends the probe); install none.
func TestARejectionBetweenJobsLeavesTheSupervisorRunning(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
		probe, _ := supervising(t, box, true)
		var none struct{}
		probe.Do(map[string]any{"op": "reject"}, &none)
		var got supervisedAnswer
		probe.Do(map[string]any{"op": "context"}, &got)
		if got.Pages != 1 || got.Refused != "" {
			t.Errorf("after a rejection nobody handled, the browser serves %+v, want its one page", got)
		}
		probe.Do(map[string]any{"op": "events"}, &got)
		if n := got.kinds("unhandled_rejection"); n != 2 {
			t.Errorf("the supervisor logged %d unhandled rejections, want both: %+v", n, got.Events)
		}
	})
}

// dialed is one connection to a box's browser.sock, read a line at a time.
type dialed struct {
	conn  net.Conn
	lines *bufio.Reader
}

func dial(t *testing.T, box *boxhelpertest.Box) *dialed {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(box.Folder, "browser.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(goneWithin)); err != nil {
		t.Fatal(err)
	}
	return &dialed{conn: conn, lines: bufio.NewReader(conn)}
}

// say writes v as one line, and answers the line that comes back: for a
// request, the answer carrying its id, the server's notifications passed over.
func (d *dialed) say(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	d.write(t, lineOf(t, v))
	return d.answer(t, v["id"])
}

// lineOf is v as one line, its newline included.
func lineOf(t *testing.T, v any) []byte {
	t.Helper()
	line, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// write writes raw to the connection, in one write.
func (d *dialed) write(t *testing.T, raw []byte) {
	t.Helper()
	if _, err := d.conn.Write(raw); err != nil {
		t.Fatal(err)
	}
}

// answer is the next line that comes back, or, given a request's id, the
// answer carrying it, the server's notifications passed over.
func (d *dialed) answer(t *testing.T, id any) map[string]any {
	t.Helper()
	for {
		line, err := d.lines.ReadBytes('\n')
		if err != nil {
			t.Fatalf("awaiting the answer to id %v: %v", id, err)
		}
		var got map[string]any
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if id == nil || got["id"] == id || fmt.Sprint(got["id"]) == fmt.Sprint(id) {
			return got
		}
	}
}

// ended fails t unless the connection ends with nothing more coming back.
func (d *dialed) ended(t *testing.T) {
	t.Helper()
	line, err := d.lines.ReadBytes('\n')
	switch {
	case err == nil:
		t.Fatalf("the connection carried %q, and was to end", line)
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatalf("the connection is still open %s on", goneWithin)
	}
}

// helloOf is the hello a shim of box's helper says for job.
func helloOf(t *testing.T, box *boxhelpertest.Box, job string) map[string]any {
	t.Helper()
	config := box.Config(t)
	return map[string]any{"crewlet": "browser-mcp", "v": config["protocol"], "helper": config["helper"], "job": job,
		"root": "file://" + box.Workspace}
}

// initialize is a harness's initialize, id, declaring no capability.
func initialize(id int) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "boxhelper-suite", "version": "1"}}}
}

// initialized is a harness's notifications/initialized, as one line.
const initialized = `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"

// tabs is a call of browser_tabs, id, listing the tabs.
func tabs(id int) map[string]any {
	return tabsCall(id, map[string]any{"action": "list"})
}

// tabsCall is a call of browser_tabs, id, with arguments.
func tabsCall(id int, arguments map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "browser_tabs", "arguments": arguments}}
}

// THE SUPERVISOR SERVES ITS BROWSER TO ONE CONNECTION THAT SAYS ITS HELLO: a
// connection to browser.sock whose hello names another helper is refused as
// helper_mismatch, and one off the hello's shape as bad_hello, each closed;
// one naming this helper is answered ok and served Playwright MCP over the
// box's own browser, whose tab it lists, on each Node's own sockets and each
// pin's browser; the shim's side is its own.
//
// Mutation: accept any helper's hello.
func TestTheSupervisorServesItsBrowserToAConnectionThatSaysItsHello(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v", got)
		}
		const job = "0b7c9c2e-4a51-4a8e-9a43-3c1d2f5e6a7b"
		another := helloOf(t, box, job)
		another["helper"] = "fedcba9876543210"
		noID := helloOf(t, box, "the job")
		for _, tc := range []struct {
			name    string
			hello   map[string]any
			refused string
		}{
			{"another helper's", another, "helper_mismatch"},
			{"a job that is no id", noID, "bad_hello"},
		} {
			if got := dial(t, box).say(t, tc.hello); got["ok"] != false || got["refused"] != tc.refused {
				t.Errorf("%s hello was answered %v, want refused as %s", tc.name, got, tc.refused)
			}
		}
		d := dial(t, box)
		if got := d.say(t, helloOf(t, box, job)); got["ok"] != true {
			t.Fatalf("this helper's hello was answered %v, want ok", got)
		}
		if got := d.say(t, initialize(1)); got["result"] == nil {
			t.Fatalf("initialize was answered %v", got)
		}
		d.write(t, []byte(initialized))
		got := d.say(t, tabs(2))
		result, _ := got["result"].(map[string]any)
		if result == nil || result["isError"] == true || !strings.Contains(fmt.Sprint(result["content"]), "about:blank") {
			t.Errorf("browser_tabs was answered %v, want the box's browser's one blank tab", got)
		}
	})
}

// A BROWSER THAT DIES DURING ITS LAUNCH IS NOTICED: a browser that ends while
// the supervisor reads its launch is never served: killed as the profile's
// lock is read, or, under a required sandbox, as its command line is read
// last (a launcher starts it on any host, and the probe answers that read as
// a sandboxed browser's command line), Playwright hearing of it before the
// read returns. A first launch fails as chromium_start_failed exited, and a
// relaunch is one of the relaunches the window counts, the browser crashing
// out once they run out, never ready on a dead context.
//
// Mutations, each red: attach the listeners after the lock's read, and look
// at the browser no more after it (the lock's rows read ready); look at it
// before its command line is read (the required row reads ready).
func TestABrowserThatDiesDuringItsLaunchIsNoticed(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Run("a first launch", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			// The first read is the stale kill's, before the launch.
			_, got := supervised(t, box, false, &fault{Method: "readlink", After: 1, KillHolder: true})
			if got.State != "failed" || !slices.Equal(got.Reports, []string{"failed chromium_start_failed exited"}) {
				t.Errorf("a browser that died as its launch was read was answered %+v, want it failed as exited", got)
			}
		})
		t.Run("a relaunch", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			// The first launch's two reads pass; every relaunch's browser dies.
			probe, first := supervised(t, box, false, &fault{Method: "readlink", After: 2, KillHolder: true})
			if first.State != "ready" {
				t.Fatalf("the first launch was answered %+v, want ready", first)
			}
			var got supervisedAnswer
			probe.Do(map[string]any{"op": "kill_chromium"}, &got)
			if got.State != "crashed_out" || got.kinds("relaunch_failed") != 3 {
				t.Errorf("relaunches whose browsers died as they were read were answered %+v, want three failed and "+
					"the browser crashed out", got)
			}
		})
		t.Run("under a required sandbox, as its command line is read", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium,
				Sandbox: browser.SandboxRequired})
			launcher(t, box, env)
			if got := box.Ensure(t); got.Reason != browser.ReasonChromiumSandboxUnavailable ||
				got.Cause != browser.CauseLauncher {
				t.Fatalf("ensure answered %+v, want withheld chromium_sandbox_unavailable launcher", got)
			}
			nothingOnProfile(t, box)
			box.Stop(t)
			var got supervisedAnswer
			box.Probe(t).Do(map[string]any{"op": "supervise", "launch": filepath.Join(box.Folder, "launch.json"),
				"fail": &fault{Method: "readFile", KillReading: true}}, &got)
			if got.State != "failed" || !slices.Equal(got.Reports, []string{"failed chromium_start_failed exited"}) {
				t.Errorf("a browser that died as its command line was read was answered %+v, want it failed as "+
					"exited", got)
			}
			nothingOnProfile(t, box)
		})
	})
}

// A STATE THE SUPERVISOR CANNOT RECORD COSTS ONLY THAT RECORD: a write of
// supervisor.json that fails (ENOSPC, once, as the browser starts again) is
// logged and costs nothing after it: the browser back, then crashed out,
// which the next ensure restarts, then stopped are each written, and the stop
// ends. One relaunch a window, so the second crash crashes out.
//
// Mutations, each red: chain a write on the one before only (the later states
// are never written); let the failed write throw out of setState (its state
// is never told, nor the failure logged).
func TestAStateTheSupervisorCannotRecordCostsOnlyThatRecord(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath,
			Config: map[string]any{"relaunch": map[string]any{"max": 1}}})
		// The first launch's two records land; the relaunch's first fails.
		probe, first := supervised(t, box, false, &fault{Method: "writeFile", Errno: "ENOSPC", After: 2, Times: 1})
		if first.State != "ready" {
			t.Fatalf("the first launch was answered %+v, want ready", first)
		}
		var got supervisedAnswer
		probe.Do(map[string]any{"op": "kill_chromium"}, &got)
		if got.State != "ready" || got.kinds("record_failed") != 1 {
			t.Fatalf("a relaunch whose first record failed was answered %+v, want ready, the failure logged once", got)
		}
		if s := box.Supervisor(t); s.State != "ready" || s.Chromium == nil || s.Chromium.PID != got.PID {
			t.Errorf("supervisor.json records %+v after the relaunch, want ready on pid %d", s, got.PID)
		}
		probe.Do(map[string]any{"op": "kill_chromium"}, &got)
		if s := box.Supervisor(t); got.State != "crashed_out" || s.State != "crashed_out" {
			t.Errorf("the second crash was answered %+v and supervisor.json records %+v, want crashed_out, which "+
				"the next ensure restarts", got, s)
		}
		probe.Do(map[string]any{"op": "stop"}, &got)
		probe.Do(map[string]any{"op": "stopped"}, &got)
		if s := box.Supervisor(t); got.State != "stopped" || s.State != "stopped" {
			t.Errorf("the stop was answered %+v and supervisor.json records %+v, want stopped", got, s)
		}
	})
}

// THE SUPERVISOR OPENS WHAT ATTACHES BEFORE IT SAYS READY AND CLOSES IT FIRST:
// an attachment is attached once browser.sock listens, the supervisor still
// starting, and told the browser served, then ready; each model connection's
// job as it comes and goes; each browser_tabs new, select and close the
// server answered without error, with its answer, never a list or an error; a
// relaunch's browser and states; and, on a stop, closed while browser.sock is
// there. One failing to attach or in its hooks is logged, costing the browser
// and its fellows nothing.
//
// Mutations, each red: attach after ready; close it last; tell a list; tell
// no tabs; let an attach's error out of the start.
func TestTheSupervisorOpensWhatAttachesBeforeItSaysReadyAndClosesItFirst(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Run("told in order", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			probe, got := supervised(t, box, false, nil, "record")
			want := []attachedCall{
				{Hook: "attach", State: "starting", Socket: true, Boot: box.Supervisor(t).Boot},
				{Hook: "launched", Current: true, Pages: 1},
				{Hook: "state", State: "ready"},
			}
			if got.State != "ready" || !slices.Equal(got.Attached, want) {
				t.Fatalf("the supervisor answered %s with its attachment told %+v, want ready, told %+v", got.State,
					got.Attached, want)
			}
			told := len(attachedTabs(t, box, probe).Attached)
			probe.Do(map[string]any{"op": "kill_chromium"}, &got)
			// The record of restarting and the relaunch race, so either may
			// be told first; ready is told once the browser has been.
			relaunched := got.Attached[told:]
			restarting := attachedCall{Hook: "state", State: "restarting"}
			launched := attachedCall{Hook: "launched", Current: true, Pages: 1}
			ready := attachedCall{Hook: "state", State: "ready"}
			if got.State != "ready" || len(relaunched) != 3 || relaunched[2] != ready ||
				!slices.Contains(relaunched[:2], restarting) || !slices.Contains(relaunched[:2], launched) {
				t.Errorf("a relaunch told the attachment %+v, want restarting and its browser, then ready", relaunched)
			}
			probe.Do(map[string]any{"op": "stop"}, &got)
			probe.Do(map[string]any{"op": "stopped"}, &got)
			if closed := got.calls("close"); len(closed) != 1 || closed[0].State != "ready" || !closed[0].Socket {
				t.Errorf("the stop closed the attachment as %+v, want once, while ready and browser.sock there", closed)
			}
			if last := got.Attached[len(got.Attached)-1]; last.Hook != "close" {
				t.Errorf("the attachment was told %+v after it was closed", last)
			}
		})
		t.Run("one that fails", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
			probe, got := supervised(t, box, false, nil, "fail", "fail_hooks", "record")
			if got.State != "ready" || len(got.calls("state")) != 1 || len(got.calls("launched")) != 1 {
				t.Fatalf("beside attachments that fail, the supervisor answered %s and told %+v, want ready, told "+
					"its browser and its state", got.State, got.Attached)
			}
			probe.Do(map[string]any{"op": "context"}, &got)
			if got.Pages != 1 || got.Refused != "" {
				t.Errorf("beside attachments that fail, the browser serves %+v, want its one page", got)
			}
			probe.Do(map[string]any{"op": "stop"}, &got)
			probe.Do(map[string]any{"op": "stopped"}, &got)
			failed := map[string]int{}
			for _, e := range got.Events {
				if e.Kind == "attachment_failed" {
					failed[e.Hook]++
				}
			}
			wantFailed := map[string]int{"attach": 1, "onLaunched": 1, "onState": 1, "close": 1}
			if got.State != "stopped" || len(got.calls("close")) != 1 || !maps.Equal(failed, wantFailed) {
				t.Errorf("the supervisor stopped as %s, its fellow closed %d times, and logged %v failed, want "+
					"stopped, closed once, and %v", got.State, len(got.calls("close")), failed, wantFailed)
			}
		})
	})
}

// A STOP AS THE SUPERVISOR BEGINS SERVING NEVER SAYS READY: a stop that comes
// while browser.sock opens and what attaches is attached, here from an
// attachment that stops the supervisor as it attaches, waits for that step to
// settle, closes every attachment while browser.sock is there, removes the
// socket and ends stopped, never ready and nothing reported to ensure.
//
// Mutations, each red: say ready after a stop that came as it began serving;
// close before what is attaching has attached.
func TestAStopAsTheSupervisorBeginsServingNeverSaysReady(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
		probe, _ := supervised(t, box, false, nil, "record", "stop")
		var got supervisedAnswer
		probe.Do(map[string]any{"op": "stopped"}, &got)
		if got.State != "stopped" || len(got.Reports) != 0 || len(got.calls("state")) != 0 {
			t.Errorf("a supervisor stopped as it began serving answered %s, reported %q and told %+v, want "+
				"stopped, never ready", got.State, got.Reports, got.Attached)
		}
		if closed := got.calls("close"); len(closed) != 2 || !closed[0].Socket || !closed[1].Socket {
			t.Errorf("a stop as they attached closed %+v, want both attachments, while browser.sock was there",
				closed)
		}
		if _, err := os.Lstat(filepath.Join(box.Folder, "browser.sock")); !os.IsNotExist(err) {
			t.Errorf("a supervisor stopped as it began serving left browser.sock (%v)", err)
		}
	})
}

// attachedTabs drives a model connection to box's probe's supervisor through
// browser.sock and holds what its recording attachment is told: the
// connection's job, and each tab call that moved the current tab, with the
// tabs its answer lists, and nothing else, then the connection gone. It
// answers the probe's answer once that is told.
func attachedTabs(t *testing.T, box *boxhelpertest.Box, probe *boxhelpertest.Probe) supervisedAnswer {
	t.Helper()
	d := dial(t, box)
	if got := d.say(t, helloOf(t, box, jobA)); got["ok"] != true {
		t.Fatalf("the hello was answered %v, want ok", got)
	}
	if got := d.say(t, initialize(1)); got["result"] == nil {
		t.Fatalf("initialize was answered %v", got)
	}
	d.write(t, []byte(initialized))
	for i, arguments := range []map[string]any{
		{"action": "new"}, {"action": "select", "index": 0}, {"action": "list"}, {"action": "select", "index": 9},
		{"action": "close", "index": 1},
	} {
		if got := d.say(t, tabsCall(2+i, arguments)); got["result"] == nil {
			t.Fatalf("browser_tabs %v was answered %v", arguments, got)
		}
	}
	if err := d.conn.Close(); err != nil {
		t.Fatal(err)
	}
	var got supervisedAnswer
	probe.Do(map[string]any{"op": "attached", "until": map[string]any{"hook": "model", "job": nil}}, &got)
	if models := got.calls("model"); !slices.Equal(models, []attachedCall{{Hook: "model", Job: jobA}, {Hook: "model"}}) {
		t.Errorf("the attachment was told the model connections %+v, want %s's, then none", models, jobA)
	}
	told := got.calls("tabs")
	for i, want := range []struct{ action, current, gone string }{
		{"new", "- 1: (current)", "- 0: (current)"},
		{"select", "- 0: (current)", "- 1: (current)"},
		{"close", "- 0: (current)", "- 1:"},
	} {
		if i >= len(told) {
			t.Errorf("the attachment was told %d tab calls, want new, select and close: %+v", len(told), told)
			break
		}
		c := told[i]
		if c.Job != jobA || c.Action != want.action || !strings.Contains(c.Text, want.current) ||
			strings.Contains(c.Text, want.gone) {
			t.Errorf("tab call %d was told as %+v, want %s's %s, its answer listing %q and no %q", i, c, jobA,
				want.action, want.current, want.gone)
		}
	}
	if len(told) > 3 {
		t.Errorf("the attachment was told %d tab calls, want new, select and close alone: %+v", len(told), told)
	}
	return got
}

// THE SUPERVISOR'S BOOT OUTLIVES A RELAUNCH: supervisor.json's boot is sixteen
// hex digits drawn as a supervisor starts, the same once its browser is
// killed and launched again, and another for the next supervisor, so what is
// numbered under it survives a crash and starts again with a new supervisor.
// Newest alone: the boot is the supervisor's own, which no Node or pin moves.
//
// Mutations, each red: draw the boot at each launch; one boot for every
// supervisor.
func TestTheSupervisorsBootOutlivesARelaunch(t *testing.T) {
	box := boxhelpertest.Start(t, boxhelpertest.Newest(t), boxhelpertest.Options{Route: boxhelpertest.OnPath})
	probe, _ := supervising(t, box, false)
	boot := box.Supervisor(t).Boot
	sixteen := regexp.MustCompile(`^[0-9a-f]{16}$`)
	if !sixteen.MatchString(boot) {
		t.Fatalf("supervisor.json's boot is %q, want sixteen hex digits", boot)
	}
	var got supervisedAnswer
	probe.Do(map[string]any{"op": "kill_chromium"}, &got)
	if s := box.Supervisor(t); got.State != "ready" || s.Boot != boot {
		t.Errorf("after a relaunch the supervisor answered %s and its boot is %q, want ready under %q", got.State,
			s.Boot, boot)
	}
	probe.Do(map[string]any{"op": "stop"}, &got)
	probe.Do(map[string]any{"op": "stopped"}, &got)
	if ensured := box.Ensure(t); ensured.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v, want ready under a new supervisor", ensured)
	}
	if next := box.Supervisor(t).Boot; next == boot || !sixteen.MatchString(next) {
		t.Errorf("the next supervisor's boot is %q, and the last's %q: want sixteen hex digits drawn anew", next, boot)
	}
}
