package executor_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// What a stand-in relay ([relayNode]) records in its box's home, the directory
// above its wiring's: its pid, its arguments a line each, the wiring it was
// started on as it held, the wirings in the box as it started, and, written
// last, that it is up with its answer to TERM set. On TERM it records whether
// the process a case's harness recorded in harnessPID still ran, and whether
// the run's scenario had left scenarioDone.
const (
	relayPID     = "relay-pid"
	relayArgv    = "relay-argv"
	relayRead    = "relay-wiring"
	relayBeside  = "relay-beside"
	relayUp      = "relay-up"
	relayTerm    = "relay-term"
	harnessPID   = "harness-pid"
	scenarioDone = "scenario-done"
)

// relayFiles is the names of a box's three relay wirings, as [executor.Paths]
// places them in its work directory.
func relayFiles() []string {
	p := executor.Paths{Home: "/"}
	return []string{path.Base(p.BrowserRelay()), path.Base(p.BrowserRelayContinued()), path.Base(p.BrowserRelayFresh())}
}

// relayNode is a `node` for a box whose job's browser relay is a case's
// subject: a stand-in for the relay, dispatched on its arguments as
// boxhelper.RelayScript runs it, `<helper> relay --wiring <file>`, recording
// what it was started on ([relayPID] and the rest) and staying up until TERM,
// which it answers by recording its moment and leaving, or ignores. Any other
// arguments run was, what the box's node was, "" for none: Kimi Code's driver
// and Qwen Code's helper are stand-ins of their own, which a stand-in that
// did not dispatch would run as the relay.
func relayNode(was string, ignoreTerm bool) string {
	onTerm := `trap 'p=$(cat "$h/` + harnessPID + `" 2> /dev/null); if [ -n "$p" ] && kill -0 "$p" 2> /dev/null; ` +
		`then s=alive; else s=gone; fi; if [ -e "$h/` + scenarioDone + `" ]; then d=done; else d=undone; fi; ` +
		`echo "harness $s scenario $d" > "$h/` + relayTerm + `"; exit 0' TERM`
	if ignoreTerm {
		onTerm = `trap '' TERM`
	}
	other := `echo 'crewlet test: no node beside the stand-in relay' >&2; exit 127`
	if was != "" {
		other = `exec ` + shellword.Quote(was) + ` "$@"`
	}
	return `#!/bin/sh
if [ "$2" = relay ] && [ "$3" = --wiring ]; then
	w=$(dirname "$4"); h=$(dirname "$w")
	` + onTerm + `
	printf '%s\n' "$$" > "$h/` + relayPID + `"
	for a; do printf '%s\n' "$a"; done > "$h/` + relayArgv + `"
	cat "$4" > "$h/` + relayRead + `"
	for f in ` + strings.Join(relayFiles(), " ") + `; do [ ! -e "$w/$f" ] || echo "$f"; done > "$h/` + relayBeside + `"
	: > "$h/` + relayUp + `"
	while :; do sleep 0.05; done
fi
` + other + "\n"
}

// relayIn makes box's node a [relayNode] over what it was: the box's own
// stand-in where it has one, else the machine's node, if any.
func relayIn(t *testing.T, box sandbox.Sandbox, ignoreTerm bool) {
	t.Helper()
	bin := executor.PathsFor(box).BinDir()
	was := ""
	if own, err := box.ReadFile(t.Context(), bin+"/node"); err == nil && len(own) > 0 {
		was = bin + "/node-of-the-box"
		writeBoxStandIn(t, box, was, string(own))
	} else if node, err := exec.LookPath("node"); err == nil {
		was = node
	}
	writeBoxStandIn(t, box, bin+"/node", relayNode(was, ignoreTerm))
}

// relayRecord is what box's stand-in relay recorded under name, "" for nothing.
func relayRecord(t *testing.T, box sandbox.Sandbox, name string) string {
	t.Helper()
	return boxFile(t, box, box.Home()+"/"+name)
}

// relayForget removes what box's stand-in relay recorded, so a case's next
// run in the box is judged by its own relay alone: its harness waits for
// [relayUp] ([awaitRelayUp]), which a relay of a run before would have left.
func relayForget(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	q := shellword.Quote
	cmd := "cd " + q(box.Home()) + " && rm -f " + strings.Join([]string{relayPID, relayArgv, relayRead, relayBeside,
		relayUp, relayTerm}, " ")
	if res, err := box.Exec(t.Context(), cmd, sandbox.ExecOptions{}); err != nil || res.ExitCode != 0 {
		t.Fatalf("forgetting the stand-in relay's records: %+v %v", res, err)
	}
}

// relayWiring is a relay's wiring, as the run's start writes it.
func relayWiring(t *testing.T, w sandbox.BrowserWiring) string {
	t.Helper()
	body, err := json.Marshal(browser.RelayWiring{V: browser.RelayWiringVersion, URL: w.URL, Job: w.Job})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// relayGone fails the case unless the relay box's stand-in recorded is gone:
// a job's relay never outlives its job.
func relayGone(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(relayRecord(t, box, relayPID)))
	if err != nil {
		t.Fatalf("the stand-in relay recorded no pid: %v", err)
	}
	if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Errorf("the job's relay, pid %d, outlived its job", pid)
	}
}

// awaitRelayUp is a stand-in harness's wait for its job's relay to be up,
// where its box holds a wiring the relay starts on: a relay ended before its
// shell set its trap would record nothing.
const awaitRelayUp = `for f in "$HOME"/.crewlet/browser-relay*.json; do [ -e "$f" ] || continue; i=0; ` +
	`while [ ! -e "$HOME/` + relayUp + `" ] && [ "$i" -lt 400 ]; do sleep 0.05; i=$((i+1)); done; break; done` + "\n"

// A RELAY STARTS ON THE WIRING ITS GATE CHOSE, under every sh: a run of one
// session's on its own wiring; a continuing run's on the continued session's
// where its gate took it up (k=yes) and on the job's other token's where it
// started one of its own, the other removed before the relay starts; none
// where the chosen wiring is missing, or there is none, never another. The
// relay's pid is $relay, and the stop ends it.
//
// Mutations, each red: fall back to the continued wiring where the fresh one
// is missing; leave the other wiring in the box; start the relay on the
// continued wiring whatever k says.
func TestARelayStartsOnTheWiringItsGateChose(t *testing.T) {
	t.Parallel()
	one, continued, fresh := relayFiles()[0], relayFiles()[1], relayFiles()[2]
	for name, tc := range map[string]struct {
		k     string
		files []string
		want  string
	}{
		"a run of one session":                          {k: "no", files: []string{one}, want: one},
		"a run of one session whose gate said yes":      {k: "yes", files: []string{one}, want: one},
		"a continuing run that took its session up":     {k: "yes", files: []string{continued, fresh}, want: continued},
		"a continuing run that started its own":         {k: "no", files: []string{continued, fresh}, want: fresh},
		"its own started, with its wiring missing":      {k: "no", files: []string{continued}},
		"its session taken up, with its wiring missing": {k: "yes", files: []string{fresh}},
		"a run with no wiring":                          {k: "no"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			forEachShell(t, func(t *testing.T, sh boxShell) {
				home, bin := t.TempDir(), t.TempDir()
				p := executor.Paths{Home: home}
				if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, f := range tc.files {
					if err := os.WriteFile(filepath.Join(p.WorkDir(), f), []byte(`{"v":1,"url":"`+f+`"}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				writeStandIn(t, filepath.Join(bin, "node"), relayNode("", false))
				start, stop := executor.RelayStep(p)
				q := shellword.Quote
				run := startPieces(t, sh, "PATH="+q(bin)+`:"$PATH"`+"\n"+executor.EndFunction(5*time.Second)+
					"k="+tc.k+"\n"+executor.RelayPrologue()+start+
					`printf '%s\n' "$relay" > `+q(filepath.Join(home, "relay-var"))+"\n"+
					`[ -z "$relay" ] || { i=0; while [ ! -e `+q(filepath.Join(home, relayUp))+
					` ] && [ "$i" -lt 400 ]; do sleep 0.05; i=$((i+1)); done; }`+"\n"+
					`ls -A `+q(p.WorkDir())+` | grep '\.json$' > `+q(filepath.Join(home, "left"))+"\n"+stop).wait()
				if run.code != 0 {
					t.Fatalf("the step exited %d:\n%s", run.code, run.stderr)
				}
				relay := strings.TrimSpace(readIn(t, home, "relay-var"))
				left := strings.Fields(readIn(t, home, "left"))
				if tc.want == "" {
					if relay != "" || readIn(t, home, relayUp) != "" {
						t.Errorf("a relay started (pid %q) where its wiring %s was in the box", relay, tc.files)
					}
					if len(left) != 0 {
						t.Errorf("the step left %s in the box, where it starts no relay", left)
					}
					return
				}
				if got := readIn(t, home, relayRead); got != `{"v":1,"url":"`+tc.want+`"}` {
					t.Errorf("the relay started on %q, want %s", got, tc.want)
				}
				if beside := strings.Fields(readIn(t, home, relayBeside)); !slices.Equal(beside, []string{tc.want}) {
					t.Errorf("the relay started beside %s, want its own wiring alone", beside)
				}
				if !slices.Equal(left, []string{tc.want}) {
					t.Errorf("the step left %s in the box, want %s alone", left, tc.want)
				}
				if pid := strings.TrimSpace(readIn(t, home, relayPID)); pid != relay {
					t.Errorf("$relay is %q and the relay's pid %q: the job's script would end another process", relay, pid)
				}
				if got := readIn(t, home, relayTerm); got == "" {
					t.Error("the step's stop did not TERM the relay")
				}
			})
		})
	}
}

// relayWaiting is harness, a stand-in, waiting for its job's relay to be up
// before it ends on its result ([awaitRelayUp]).
func relayWaiting(harness string) string {
	return strings.Replace(harness, `printf '{"type":"result"`, awaitRelayUp+`printf '{"type":"result"`, 1)
}

// tokenOf is the bridge token a wiring's URL names.
func tokenOf(t *testing.T, wiring string) string {
	t.Helper()
	var w browser.RelayWiring
	if err := json.Unmarshal([]byte(wiring), &w); err != nil {
		t.Fatalf("the relay's wiring %q is no wiring: %v", wiring, err)
	}
	rest, ok := strings.CutSuffix(w.URL, "/browser")
	if !ok {
		t.Fatalf("the relay's wiring names %q, which is no browser route", w.URL)
	}
	return rest[strings.LastIndex(rest, "/")+1:]
}

// A RELAY STARTS ON THE TOKEN OF THE SESSION ITS BOX STARTED, AND ITS BOX HOLDS
// NO OTHER. A continuing run is handed a relay's wiring under both of the
// job's tokens; its relay starts on the one its harness's tools are wired to,
// the continued session's where the box held it and the other's where it
// started one of its own, the other wiring gone first: Claude Code's gate and
// OpenCode's, under every sh, the two whose cases drive a continued session.
//
// Mutation: start the relay on the continued wiring whatever the gate chose,
// or leave the other wiring in the box, and a row goes red.
func TestARelayStartsOnTheTokenOfTheSessionItsBoxStarted(t *testing.T) {
	t.Parallel()
	job := uuid.NewString()
	routes := func(token string) sandbox.BrowserWiring {
		return sandbox.BrowserWiring{URL: "https://engine.example.com/mcp/" + token + "/browser", Job: job}
	}
	t.Run("claude code", func(t *testing.T) {
		t.Parallel()
		runner, box := realBox(t)
		relayIn(t, box, false)
		writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", relayWaiting(wiredHarness("2.1.310")))
		_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", MCPServers: bridgeAt("first-job")})
		for _, tc := range []struct{ name, resume, want string }{
			{name: "its session held", resume: first.SessionID, want: "continued-token"},
			{name: "a session it cannot take up", resume: uuid.NewString(), want: "fresh-token"},
		} {
			relayForget(t, box)
			runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on where you stopped",
				FreshBrief: "the task, from the top", Resume: tc.resume, MCPServers: bridgeAt("continued-token"),
				Browser: routes("continued-token"), FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token"),
					Browser: routes("fresh-token")}})
			if seen := boxFile(t, box, box.Home()+"/mcp-seen"); !strings.Contains(seen, tc.want) {
				t.Fatalf("%s: the harness started on %s, want the %s", tc.name, seen, tc.want)
			}
			if got := tokenOf(t, relayRecord(t, box, relayRead)); got != tc.want {
				t.Errorf("%s: the relay started on the %s, and the harness on the %s", tc.name, got, tc.want)
			}
			if beside := strings.Fields(relayRecord(t, box, relayBeside)); len(beside) != 1 {
				t.Errorf("%s: the relay started beside the wirings %s, want its own alone", tc.name, beside)
			}
		}
	})
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		node, _ := exec.LookPath("node")
		forEachShell(t, func(t *testing.T, sh boxShell) {
			for name, tc := range map[string]struct {
				held []string
				want string
			}{
				"the session held":    {held: []string{carriedOpenCode}, want: "continued-token"},
				"nothing to continue": {want: "fresh-token"},
			} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					files := relayFiles()
					run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: tc.held,
						freshBrief: "the task, from the top", values: "continued-token", freshValues: "fresh-token",
						req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly,
							FreshWiring: &sandbox.Wiring{MCPServers: bridgeOnly}},
						env:      []string{"FAKE_RUN_UNTIL=home/" + relayUp},
						standIns: map[string]string{"node": relayNode(node, false)},
						inWorkDir: map[string]string{files[1]: relayWiring(t, routes("continued-token")),
							files[2]: relayWiring(t, routes("fresh-token"))}})
					if run.code != 0 {
						t.Fatalf("exit %d:\n%s", run.code, run.stderr)
					}
					if got := readIn(t, run.dir, "served-value"); got != tc.want {
						t.Fatalf("the run's server started on %q, want %q", got, tc.want)
					}
					if got := tokenOf(t, readIn(t, run.home, relayRead)); got != tc.want {
						t.Errorf("the relay started on the %s, and the server on the %s", got, tc.want)
					}
					if beside := strings.Fields(readIn(t, run.home, relayBeside)); len(beside) != 1 {
						t.Errorf("the relay started beside the wirings %s, want its own alone", beside)
					}
				})
			}
		})
	})
}

// relayScenario is a scenario for [creditBoxes] whose subject is the job's
// relay: once the run's relay step has run, leaving one of the continuing
// run's two wirings (none is there before the run's start writes them, and
// both until the step), it waits for the relay to be up, so the harness
// running it never ends before the relay can answer its TERM; then it records
// that it is done.
var relayScenario = `n=0; for f in "$HOME"/.crewlet/browser-relay*.json; do [ ! -e "$f" ] || n=$((n+1)); done` + "\n" +
	`if [ "$n" = 1 ]; then i=0; while [ ! -e "$HOME/` + relayUp + `" ] && [ "$i" -lt 400 ]; do sleep 0.05; ` +
	`i=$((i+1)); done; fi` + "\n" + `: > "$HOME/` + scenarioDone + `"` + "\n"

// relayScenarioIn is, by runner, how its box runs [relayScenario] in the very
// process its script starts beside the relay, where its [creditBoxes] box runs
// its scenario in one started before (Codex's app-server, ahead of `codex
// exec`).
var relayScenarioIn = map[string]func(t *testing.T, b sandbox.Sandbox){
	executor.CodexName: func(t *testing.T, b sandbox.Sandbox) {
		bin := executor.PathsFor(b).BinDir() + "/codex"
		standIn, err := b.ReadFile(t.Context(), bin)
		if err != nil || !strings.Contains(string(standIn), "\nexec)\n") {
			t.Fatalf("the codex stand-in has no exec branch (%v)", err)
		}
		writeBoxStandIn(t, b, bin, strings.Replace(string(standIn), "\nexec)\n", "\nexec)\n  "+scenarioRun, 1))
	},
}

// EVERY RUNNER'S JOB RUNS ITS RELAY BESIDE ITS HARNESS. Each registered
// runner's whole run in a real box ([creditBoxes]), continuing a session its
// box does not hold, so every gate falls back to a session of its own and the
// relay must start on the job's other token's wiring, alone in the box, its
// command line naming the file and no token. Its script ends it once the
// harness's work is done, and it is gone when the job is over. The box's node
// dispatches on `relay --wiring` ([relayNode]); that the continued half
// starts on its own wiring is held on the two runners whose cases drive a
// continued session (TestARelayStartsOnTheTokenOfTheSessionItsBoxStarted).
//
// Mutations, each red: drop the step from launchWatched, or from Gemini CLI's
// script; drop its stop; a runner without a box in creditBoxes is named.
func TestEveryRunnersJobRunsItsRelayBesideItsHarness(t *testing.T) {
	t.Parallel()
	for _, name := range executor.Names() {
		boxed, ok := creditBoxes[name]
		if !ok {
			t.Errorf("runner %s has no box in creditBoxes, so nothing shows its job runs its browser relay", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := boxed(t)
			relayIn(t, b, false)
			if in, ok := relayScenarioIn[name]; ok {
				in(t, b)
			}
			if err := b.WriteFile(t.Context(), b.Home()+"/scenario.sh", []byte(relayScenario)); err != nil {
				t.Fatalf("write the scenario: %v", err)
			}
			job := uuid.NewString()
			wired := func(token string) (map[string]sandbox.MCPServer, sandbox.BrowserWiring) {
				return map[string]sandbox.MCPServer{"crewlet-bridge": {Name: "crewlet-bridge",
						Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/" + token, AlwaysLoaded: true}},
					sandbox.BrowserWiring{URL: "http://127.0.0.1:7/mcp/" + token + "/browser", Job: job}
			}
			servers, continued := wired("continued-token")
			freshServers, fresh := wired("fresh-token")
			handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "carry on where you stopped",
				FreshBrief: "the task, from the top", Resume: uuid.NewString(), MCPServers: servers, Browser: continued,
				FreshWiring: &sandbox.Wiring{MCPServers: freshServers, Browser: fresh}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			awaitOver(t, runner, b, handle, time.Minute, "a stand-in that does one thing takes seconds")
			report := "\n--- harness\n" + boxFile(t, b, executor.PathsFor(b).Err()) + "--- relay's log\n" +
				boxFile(t, b, executor.PathsFor(b).BrowserRelayLog())
			argv := strings.Split(strings.TrimSuffix(relayRecord(t, b, relayArgv), "\n"), "\n")
			want := []string{path.Join(boxhelper.Folder(b.Home()), "helper.mjs"), "relay", "--wiring",
				executor.PathsFor(b).BrowserRelayFresh()}
			if !slices.Equal(argv, want) {
				t.Fatalf("the job's relay ran as %q, want %q:%s", argv, want, report)
			}
			if got, want := relayRecord(t, b, relayRead), relayWiring(t, fresh); got != want {
				t.Errorf("the relay read the wiring %s, want the session of its own's %s", got, want)
			}
			if beside := strings.Fields(relayRecord(t, b, relayBeside)); !slices.Equal(beside, relayFiles()[2:]) {
				t.Errorf("the relay started beside the wirings %s, want its own alone", beside)
			}
			for _, a := range argv {
				if strings.Contains(a, "-token") {
					t.Errorf("the relay's command line names a token: %q", argv)
				}
			}
			if got := relayRecord(t, b, relayTerm); !strings.HasSuffix(got, "scenario done\n") {
				t.Errorf("the relay was ended with %q, want its TERM once the harness's work was done:%s", got, report)
			}
			relayGone(t, b)
		})
	}
}

// relayedHarness is a stand-in `claude` of 2.1.310 for a case about how its
// job's relay ends ([relayNode]): started on its brief, it records its pid
// ([harnessPID]) and waits for the relay to be up; then it ends on its result
// or, working, stays until TERM, which it answers only once the relay has
// recorded its own, so a relay TERMed first finds it still running.
func relayedHarness(working bool) string {
	wait := func(name string) string {
		return `i=0; while [ ! -e "$HOME/` + name + `" ] && [ "$i" -lt 400 ]; do sleep 0.05; i=$((i+1)); done`
	}
	work := `echo "$$" > "$HOME/` + harnessPID + `"` + "\n" + wait(relayUp) + "\n"
	if working {
		work += `trap '` + wait(relayTerm) + `; exit 143' TERM` + "\n" +
			`echo working > "$HOME/` + workingMarker + `"` + "\n" +
			"while :; do sleep 0.05; done\n"
	}
	return strings.Replace(sessionHarness("2.1.310"), `printf '{"type":"result"`, work+`printf '{"type":"result"`, 1)
}

// startRelayed starts a run in box with a browser relay's wiring, as a launch
// whose browser relays its screen hands it.
func startRelayed(t *testing.T, runner *executor.Runner, box sandbox.Sandbox) sandbox.RunHandle {
	t.Helper()
	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "begin",
		Browser: sandbox.BrowserWiring{URL: "http://127.0.0.1:7/mcp/run-token/browser", Job: uuid.NewString()}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle
}

// THE RELAY IS ENDED WITH ITS JOB, under every sh: once its harness exits, the
// script TERMs it and waits for it; an end request TERMs it before anything
// else, while the harness still runs, so it goes inside the harness's own
// grace; a request before the harness starts none; and one that ignores TERM
// is killed at the script's grace, here a third of a second, as the runners'
// own cases shorten theirs. In none does it outlive its job.
//
// Mutations, each red: TERM the relay after `end "$harness"`; drop `end
// "$relay"`; start the relay before the check for an early request.
func TestTheRelayIsEndedWithItsJob(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		t.Run("its harness's exit", func(t *testing.T) {
			t.Parallel()
			runner, box := realBoxUnder(t, sh)
			relayIn(t, box, false)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", relayedHarness(false))
			awaitOver(t, runner, box, startRelayed(t, runner, box), 15*time.Second, "the relay's end takes a moment")
			if got := relayRecord(t, box, relayTerm); !strings.HasPrefix(got, "harness gone ") {
				t.Errorf("the relay was ended with %q, want its TERM once its harness had gone", got)
			}
			relayGone(t, box)
		})
		t.Run("an end request", func(t *testing.T) {
			t.Parallel()
			runner, box := realBoxUnder(t, sh)
			relayIn(t, box, false)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", relayedHarness(true))
			handle := startRelayed(t, runner, box)
			harnessWorking(t, box)
			askEnd(t, runner, box, handle)
			awaitOver(t, runner, box, handle, 15*time.Second, "the end gives its harness five seconds")
			if ended := boxFile(t, box, executor.PathsFor(box).JobEnded()); ended != "asked\n" {
				t.Errorf("the job's end is recorded as %q, want asked", ended)
			}
			if got := relayRecord(t, box, relayTerm); !strings.HasPrefix(got, "harness alive ") {
				t.Errorf("the relay was ended with %q, want its TERM while its harness still ran", got)
			}
			relayGone(t, box)
		})
		t.Run("a request before its harness", func(t *testing.T) {
			t.Parallel()
			runner, box := realBoxUnder(t, sh)
			relayIn(t, box, false)
			p := executor.PathsFor(box)
			asked, endAsked := box.Home()+"/release-asked", box.Home()+"/end-asked"
			writeBoxStandIn(t, box, p.BinDir()+"/claude", strings.Replace(relayedHarness(false),
				`if [ "$1" = --version ]; then echo`, `if [ "$1" = --version ]; then : > `+shellword.Quote(asked)+
					`; i=0; while [ ! -e `+shellword.Quote(endAsked)+` ] && [ "$i" -lt 400 ]; do sleep 0.05; `+
					`i=$((i+1)); done; echo`, 1))
			handle := startRelayed(t, runner, box)
			boxFileAppears(t, box, p.JobScript(), "the script to publish its pid")
			waitForFile(t, asked, "the script to ask its harness's release")
			askEnd(t, runner, box, handle)
			if err := box.WriteFile(t.Context(), endAsked, []byte("asked")); err != nil {
				t.Fatal(err)
			}
			awaitOver(t, runner, box, handle, 15*time.Second, "nothing starts")
			if ended := boxFile(t, box, p.JobEnded()); ended != "before\n" {
				t.Errorf("the job's end is recorded as %q, want before", ended)
			}
			if up := relayRecord(t, box, relayUp); up != "" || relayRecord(t, box, relayPID) != "" {
				t.Errorf("a relay started for a job asked to end before its harness started")
			}
		})
		t.Run("a relay that ignores TERM", func(t *testing.T) {
			t.Parallel()
			_, box := realBoxUnder(t, sh)
			runner := executor.New(timedClaude{stop: 300 * time.Millisecond})
			relayIn(t, box, true)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", relayedHarness(false))
			awaitOver(t, runner, box, startRelayed(t, runner, box), 15*time.Second,
				"a relay ignoring TERM is given a third of a second here")
			relayGone(t, box)
		})
	})
}

// A RELAY THAT LEFT ON ITS OWN IS SIGNALLED NO MORE, under every sh: once the
// script has reaped a relay that exited by itself, its pid is any new
// process's to take, so where $relay names another process (here one the
// kernel started a clock tick or more after it, as it starts one that takes a
// pid again), neither an end request's TERM nor the stop signals it, and
// each finds the relay gone and forgets it.
//
// Mutations, each red: TERM $relay by its pid alone on an end request; end it
// by its pid alone at the stop.
func TestARelayThatLeftOnItsOwnIsSignalledNoMore(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		home, bin := t.TempDir(), t.TempDir()
		p := executor.Paths{Home: home}
		if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.BrowserRelay(), []byte(`{"v":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		writeStandIn(t, filepath.Join(bin, "node"), "#!/bin/sh\nexit 0\n")
		start, stop := executor.RelayStep(p)
		q := shellword.Quote
		// gone is the relay started, left and reaped, and $relay then naming
		// a process that is not it, started in a later tick of the clock the
		// kernel's start times count, whose survival the step after it
		// reports.
		gone := start + `[ -z "$relay" ] || wait "$relay"` + "\n" + `while :; do sleep 30 & other=$!; ` +
			`[ "$(relay=$other; relayborn)" = "$relayat" ] || break; kill "$other"; wait "$other"; done; ` +
			"relay=$other\n"
		// said is how the other process ended: by the KILL it sends now, where
		// nothing signalled it before, and with $relay forgotten.
		said := `kill -9 "$other" 2> /dev/null; wait "$other"; c=$?; if [ "$c" = 137 ]; then ` +
			`printf 'alive %s\n' "${relay:-none}"; else printf 'ended %s\n' "$c"; fi >> ` +
			q(filepath.Join(home, "said")) + "\n"
		run := startPieces(t, sh, "PATH="+q(bin)+`:"$PATH"`+"\n"+executor.EndFunction(time.Second)+
			"k=no harness=\n"+executor.RelayPrologue()+executor.EndHarness("")+
			gone+"endharness\n"+said+gone+stop+said).wait()
		if run.code != 0 {
			t.Fatalf("the pieces exited %d:\n%s", run.code, run.stderr)
		}
		if got := readIn(t, home, "said"); got != "alive none\nalive none\n" {
			t.Errorf("with $relay naming another process, the end request's TERM and the stop left it %q, want "+
				"alive and $relay forgotten after each", got)
		}
	})
}

// A RUN HANDED A BROWSER RELAY'S WIRING UNDER ONE OF ITS TOKENS ALONE IS
// REFUSED before anything is written: the session its box starts could be the
// one with none, and its screen would never leave the box.
//
// Mutation: drop the check, and the run starts.
func TestARunHandedAFreshRelayWiringWithoutItsOwnIsRefused(t *testing.T) {
	t.Parallel()
	w := sandbox.BrowserWiring{URL: "http://127.0.0.1:7/mcp/fresh-token/browser", Job: uuid.NewString()}
	for name, req := range map[string]sandbox.RunRequest{
		"the fresh token's alone": {Brief: "b", Resume: uuid.NewString(),
			FreshWiring: &sandbox.Wiring{Browser: w}},
		"the continued token's alone": {Brief: "b", Resume: uuid.NewString(), Browser: w,
			FreshWiring: &sandbox.Wiring{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)
			_, err := executor.NewClaudeCode().Start(t.Context(), b, req)
			if err == nil || !strings.Contains(err.Error(), "under one of its two bridge tokens alone") {
				t.Fatalf("Start = %v, want the run refused for its relay's one wiring", err)
			}
			if started := b.Background(); len(started) != 0 {
				t.Errorf("the refused run started %q", started)
			}
		})
	}
}

// THE RELAY'S FILES ARE CLEARED BEFORE EVERY RUN: its three wirings and its
// log, so a run with no browser starts no relay on a session another job's
// end closed, and a relay's log is its own run's.
//
// Mutation: leave any of the four out of ClearArtifacts, and it outlives it.
func TestTheRelaysFilesAreClearedBeforeEveryRun(t *testing.T) {
	t.Parallel()
	_, b := realBox(t)
	p := executor.PathsFor(b)
	files := []string{p.BrowserRelay(), p.BrowserRelayContinued(), p.BrowserRelayFresh(), p.BrowserRelayLog()}
	for _, f := range files {
		if err := b.WriteFile(t.Context(), f, []byte("a previous run's")); err != nil {
			t.Fatal(err)
		}
	}
	if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), b); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	for _, f := range files {
		if boxHas(t, b, f) {
			t.Errorf("%s outlived the clear", f)
		}
	}
}

// THE RELAY'S WIRINGS NAME THE ENGINE HOST AS THE BOX REACHES IT: a container
// box's relay dials from inside the box, where 127.0.0.1 is its own loopback,
// so both of a continuing run's wirings and a single session's are written
// with the engine host by its alias, the job and the version as handed. A
// browser runs only in a container box, so this is its relay's one path.
//
// Mutation: drop either rewrite from Start, and a row goes red alone.
func TestTheRelaysWiringsNameTheEngineHostAsTheBoxReachesIt(t *testing.T) {
	t.Parallel()
	job := uuid.NewString()
	at := func(host, token string) sandbox.BrowserWiring {
		return sandbox.BrowserWiring{URL: "http://" + host + ":8000/mcp/" + token + "/browser", Job: job}
	}
	p := executor.Paths{Home: sandbox.DefaultHome}
	for name, tc := range map[string]struct {
		req  sandbox.RunRequest
		want map[string]string
	}{
		"a run of one session": {req: sandbox.RunRequest{Brief: "b", Browser: at("127.0.0.1", "run-token")},
			want: map[string]string{p.BrowserRelay(): "run-token"}},
		"a continuing run": {req: sandbox.RunRequest{Brief: "b", Resume: uuid.NewString(),
			Browser: at("127.0.0.1", "continued-token"), FreshWiring: &sandbox.Wiring{Browser: at("127.0.0.1", "fresh-token")}},
			want: map[string]string{p.BrowserRelayContinued(): "continued-token", p.BrowserRelayFresh(): "fresh-token"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := containerLike{sandbox.NewFakeSandbox("box-1").CapReads(boxReadCap)}
			if err := runner.Install(t.Context(), b); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if executor.PathsFor(b) != p {
				t.Fatalf("the box's paths are %+v, want %+v", executor.PathsFor(b), p)
			}
			handed := tc.req.Browser.URL
			if _, err := runner.Start(t.Context(), b, tc.req); err != nil {
				t.Fatalf("Start: %v", err)
			}
			for _, file := range []string{p.BrowserRelay(), p.BrowserRelayContinued(), p.BrowserRelayFresh()} {
				body, err := b.ReadFile(t.Context(), file)
				token, wanted := tc.want[file]
				switch {
				case !wanted:
					if err == nil && len(body) > 0 {
						t.Errorf("%s was written for a run it is not for: %s", file, body)
					}
				case err != nil:
					t.Errorf("reading %s: %v", file, err)
				case string(body) != relayWiring(t, at("host.docker.internal", token)):
					t.Errorf("%s holds %s, want the engine host by its alias under the %s", file, body, token)
				}
			}
			if tc.req.Browser.URL != handed {
				t.Errorf("the caller's wiring was rewritten: %q", tc.req.Browser.URL)
			}
		})
	}
}

// THE JOB'S RELAY DIALS THE BRIDGE ON THE TOKEN ITS BOX CHOSE: the real relay,
// the box helper as a launch writes it and the floor's node or newer, started
// by the run's own script beside a Claude Code stand-in, whose gate took its
// session up or started one of its own. Its every request reaches the bridge
// under the token of the session its box started, and with no supervisor in
// the box its check-in says so. The relay loads no Playwright tree: the box
// holds none.
//
// Mutation: swap the wirings in the step, and a row's first request names the
// other token.
func TestTheJobsRelayDialsTheBridgeOnTheTokenItsBoxChose(t *testing.T) {
	t.Parallel()
	node := boxhelpertest.Node(t)
	for _, tc := range []struct {
		name string
		held bool
		want string
	}{
		{name: "its session held", held: true, want: "continued-token"},
		{name: "a session of its own", want: "fresh-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			p := executor.PathsFor(box)
			if err := boxhelper.Write(t.Context(), box, box.Home(),
				boxhelper.Settings{Sandbox: browser.SandboxWaived}); err != nil {
				t.Fatalf("writing the box helper: %v", err)
			}
			writeBoxStandIn(t, box, p.BinDir()+"/node", "#!/bin/sh\nexec "+shellword.Quote(node)+` "$@"`+"\n")
			reached := box.Home() + "/bridge-reached"
			writeBoxStandIn(t, box, p.BinDir()+"/claude", strings.Replace(wiredHarness("2.1.310"),
				`printf '{"type":"result"`, `for f in "$HOME"/.crewlet/browser-relay*.json; do [ -e "$f" ] || continue; `+
					`i=0; while [ ! -e `+shellword.Quote(reached)+` ] && [ "$i" -lt 1200 ]; do sleep 0.05; `+
					`i=$((i+1)); done; break; done`+"\n"+`printf '{"type":"result"`, 1))
			resume := uuid.NewString()
			if tc.held {
				_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", MCPServers: bridgeAt("first-job")})
				resume = first.SessionID
			}
			bridge := boxhelpertest.NewFakeBridge(t)
			job := uuid.NewString()
			at := func(token string) sandbox.BrowserWiring {
				return sandbox.BrowserWiring{URL: strings.Replace(bridge.URL(), bridge.Token(), token, 1), Job: job}
			}
			handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "carry on where you stopped",
				FreshBrief: "the task, from the top", Resume: resume, MCPServers: bridgeAt("continued-token"),
				Browser: at("continued-token"), FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token"),
					Browser: at("fresh-token")}})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			first := bridge.Await("the relay's first request", 0, func(boxhelpertest.Request) bool { return true })
			checkin := bridge.Await("the relay's check-in", 0, boxhelpertest.Verb("checkin"))
			if err = box.WriteFile(t.Context(), reached, []byte("reached")); err != nil {
				t.Fatal(err)
			}
			awaitOver(t, runner, box, handle, time.Minute, "the relay leaves within two seconds of its TERM")
			if first.Token != tc.want {
				t.Errorf("the relay's first request named the %s, want the %s", first.Token, tc.want)
			}
			for _, r := range bridge.Requests() {
				if r.Token != tc.want {
					t.Errorf("the relay's %s %s named the %s, want the %s alone", r.Method, r.Verb, r.Token, tc.want)
				}
			}
			var said struct {
				Job        string                     `json:"job"`
				Supervisor browser.SupervisorPresence `json:"supervisor"`
			}
			if err = json.Unmarshal(checkin.Body, &said); err != nil || said.Job != job ||
				said.Supervisor != browser.SupervisorAbsent {
				t.Errorf("the relay checked in with %s (%v), want its job and its supervisor absent", checkin.Body, err)
			}
		})
	}
}
