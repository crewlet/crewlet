package codingagent_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// fakeOpenCode is an `opencode` that records what the run's script asks of it,
// in the order it asks, and answers as OpenCode 2.0.32 measured:
//
//   - `serve` prints where it listens and the password it minted, then serves
//     until it is stopped — and, given OPENCODE_PASSWORD or
//     OPENCODE_SERVER_PASSWORD, takes that as its password and prints none.
//     It KEEPS ITS PASSWORD BEFORE IT SAYS ANYTHING, as a server holds the one
//     it minted before printing it: kept after, a script that read the
//     printed lines on a loaded runner presented the password to a fake that
//     had not yet written down what to check it against, was refused, and
//     the case read the refused run as a login never refreshed;
//   - every call to the server is refused unless it presents the server's
//     password, read as 2.0.32's client reads it: OPENCODE_PASSWORD, and
//     OPENCODE_SERVER_PASSWORD only where that is unset;
//   - `api mcp.list` lists nothing at first — the workspace's servers start as
//     it is first asked about — then the bridge pending, then connected, or
//     always FAKE_MCP_LIST where that is set;
//   - `api session.get` prints a session's usage report on one line;
//   - `api session.inbox.list` prints FAKE_INBOX, or an empty inbox, and
//     `api session.inbox.cancel` records in `cancelled` the session, the item
//     and whether the server was still serving — `serving` or `stopped`;
//   - `auth import` keeps what it was given as its database, and `auth export`
//     prints that database, whether through the server or beside it — taking
//     FAKE_EXPORT_SLEEP seconds over it where that is set, the file
//     `exporting` saying it has begun;
//   - `run` keeps its stdin and the password it presented, puts FAKE_REFRESH
//     in the database where that is set — a login refreshed mid-run — and
//     then works for FAKE_RUN_SLEEP seconds, and until the case's directory holds
//     the file FAKE_RUN_UNTIL names, before it exits. Where only FAKE_RUN_SLEEP
//     is set, the run BECOMES the sleep (`exec`), because every case that sets
//     it ends the run by a signal, and a shell left in front of the sleep can
//     swallow one: the fake is a `#!/bin/sh` script, dash catches SIGINT in a
//     script file and holds one that lands while it forks a foreground
//     command until that command returns, so a group SIGINT sent the moment
//     `refreshed` appears could miss the sleep and wait out all sixty seconds
//     of it — the interrupt case failed so, now and then, under load. With no
//     shell left there is nothing to hold it.
//
// Every call also records, in `configured`, whether it was handed
// OPENCODE_CONFIG_CONTENT — `handed|<subcommand>` or `|<subcommand>` — and
// `serve` keeps the content it was handed, in `serve-config`; and, in
// `steered`, the CREWLET_STEER_SESSION it was handed — `<session>|<subcommand>`.
//
// FAKE_SERVE=listen-only is a server that says where it listens and never
// prints a password; FAKE_SERVE=silent one that says nothing at all; and
// FAKE_SERVE=password-first one that prints its password a second before it
// says where it listens.
const fakeOpenCode = `#!/bin/sh
d="$FAKE_DIR"
echo "$*" >> "$d/calls"
printf '%s|%s %s\n' "${OPENCODE_CONFIG_CONTENT+handed}" "$1" "$2" >> "$d/configured"
printf '%s|%s %s\n' "${CREWLET_STEER_SESSION-}" "$1" "$2" >> "$d/steered"
authed() {
  [ "${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}}" = "$(cat "$d/server-password" 2> /dev/null)" ] && return 0
  echo "Error: Server at http://127.0.0.1:4242 rejected the password" >&2; exit 1
}
case "$1" in
--version) echo "${FAKE_VERSION:-opencode v2.0.32}" ;;
auth)
  case "$2" in
  import) cat > "$d/imported"; cp "$d/imported" "$d/db" ;;
  export)
    [ "$3" != --server ] || authed
    if [ -n "${FAKE_EXPORT_SLEEP:-}" ]; then : > "$d/exporting"; sleep "$FAKE_EXPORT_SLEEP"; fi
    cat "$d/db" 2> /dev/null ;;
  esac ;;
serve)
  printf '%s|%s' "${OPENCODE_PASSWORD:-}" "${OPENCODE_SERVER_PASSWORD:-}" > "$d/serve-env"
  printf '%s' "${OPENCODE_CONFIG_CONTENT-}" > "$d/serve-config"
  echo $$ > "$d/serve.pid"
  [ "${FAKE_SERVE:-}" != silent ] || exec sleep 60
  if [ "${FAKE_SERVE:-}" = password-first ]; then
    printf '%s' pw-secret > "$d/server-password"
    echo "server password pw-secret"; sleep 1
    echo "server listening on http://127.0.0.1:4242"
    exec sleep 60
  fi
  pw=${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}} minted=
  if [ -z "$pw" ] && [ "${FAKE_SERVE:-}" != listen-only ]; then pw=pw-secret minted=1; fi
  printf '%s' "$pw" > "$d/server-password"
  echo "server listening on http://127.0.0.1:4242"
  [ -z "$minted" ] || echo "server password $pw"
  exec sleep 60 ;;
api)
  authed
  case "$4" in
  mcp.list)
    n=$(cat "$d/polls" 2> /dev/null || echo 0); n=$((n+1)); echo "$n" > "$d/polls"
    if [ -n "${FAKE_MCP_LIST:-}" ]; then printf '%s\n' "$FAKE_MCP_LIST"; exit 0; fi
    case "$n" in
    1) echo '{"data":[]}' ;;
    2) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"pending"}}]}' ;;
    *) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"connected"}}]}' ;;
    esac ;;
  session.get) echo '{"data":{"id":"'"${6#sessionID=}"'","cost":0.5,"tokens":{"input":10,"output":2,"reasoning":3,"cache":{"read":40,"write":1}}}}' ;;
  session.inbox.list)
    inbox=${FAKE_INBOX-}
    [ -n "$inbox" ] || inbox='{"data":[]}'
    printf '%s\n' "$inbox" ;;
  session.inbox.cancel)
    if kill -0 "$(cat "$d/serve.pid" 2> /dev/null)" 2> /dev/null; then served=serving; else served=stopped; fi
    echo "${6#sessionID=} ${8#inboxID=} $served" >> "$d/cancelled" ;;
  esac ;;
run)
  authed
  cat > "$d/brief"
  printf '%s' "${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}}" > "$d/run-password"
  if [ -n "${FAKE_REFRESH+set}" ]; then printf '%s' "$FAKE_REFRESH" > "$d/db"; fi
  : > "$d/refreshed"
  echo '{"type":"text","part":{"text":"done"}}'
  [ -z "${FAKE_RUN_SLEEP:-}" ] || [ -n "${FAKE_RUN_UNTIL:-}" ] || exec sleep "$FAKE_RUN_SLEEP"
  [ -z "${FAKE_RUN_SLEEP:-}" ] || sleep "$FAKE_RUN_SLEEP"
  [ -z "${FAKE_RUN_UNTIL:-}" ] || while [ ! -e "$d/$FAKE_RUN_UNTIL" ]; do sleep 0.05; done
  exit "${FAKE_RUN_EXIT:-0}" ;;
esac
`

// scriptOpts is one run of a run's script against [fakeOpenCode].
type scriptOpts struct {
	req   sandbox.RunRequest
	login string
	env   []string
	// times, where set, runs the script at these waits
	// ([codingagent.OpenCodeCommandTimed]) rather than as Command builds it.
	times *codingagent.OpenCodeTimes
	// shell is the sh the script runs under; the zero value is the sh on
	// PATH, which is what Command names.
	shell boxShell
	// unconfigured starts the script with no run configuration where
	// [codingagent.Paths.OpenCodeConfig] names it; every other case finds
	// [scriptConfig] there, as WriteConfig would have left one.
	unconfigured bool
}

// scriptConfig is the run configuration a case's script finds, as
// [codingagent.OpenCode.WriteConfig] would have left it — with text a shell
// would expand, were it ever read as anything but a value.
const scriptConfig = "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n" +
	"  \"instructions\": [\"/box/$HOME/$(echo expanded)/`id`.md\"]\n}"

// scriptJob is the script running, in a process group of its own as the
// wrapper's job is, so a case can signal the whole of it as a teardown does.
type scriptJob struct {
	t              *testing.T
	dir, home      string
	paths          codingagent.Paths
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	exited         chan error
}

// scriptRun is what one execution of the script left.
type scriptRun struct {
	dir, home       string
	stdout, stderr  string
	code            int
	calls           []string
	document, brief string
}

// signalsDeliverable gives every script this file starts the default action
// for SIGHUP and SIGINT, however the test binary itself was started.
//
// A signal ignored when a process starts is ignored in everything it execs,
// and a non-interactive sh may not trap one it was started ignoring: POSIX
// says so, and dash, bash and busybox ash all comply. So a suite run under
// nohup (SIGHUP) or as a background job of a script (SIGINT) handed every
// script a signal it could neither trap nor die of, and the cases about that
// signal failed on how the runner was started rather than on the script:
// under nohup each hangup case waited out its whole bound.
//
// Go hands an ignored SIGHUP or SIGINT on to the processes it starts, and
// resets every signal it HANDLES to the default in them, so asking os/signal
// for the two is what gives them back. Only where they are ignored: a handler
// on one that is not would stop an interrupt ending the test binary.
var signalsDeliverable = sync.OnceFunc(func() {
	for _, sig := range []os.Signal{syscall.SIGHUP, syscall.SIGINT} {
		if signal.Ignored(sig) {
			signal.Notify(make(chan os.Signal, 1), sig)
		}
	}
})

// startScript starts the script under a real sh — o.shell, the sh on PATH
// where that is unset — the way the wrapper does: the brief on stdin, stdout
// the event stream.
func startScript(t *testing.T, o scriptOpts) *scriptJob {
	t.Helper()
	signalsDeliverable()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{home + "/.crewlet", bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeStandIn(t, filepath.Join(bin, "opencode"), fakeOpenCode)
	paths := codingagent.Paths{Home: home}
	if !o.unconfigured {
		if err := os.WriteFile(paths.OpenCodeConfig(), []byte(scriptConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if o.login != "" {
		if err := os.WriteFile(paths.CredentialDocument(), []byte(o.login), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := codingagent.OpenCode{}.Command(o.req, paths, "")
	if o.times != nil {
		command = codingagent.OpenCodeCommandTimed(o.req, paths, *o.times)
	}
	script := o.shell.prelude + scriptOf(t, command)
	argv := o.shell.argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}

	j := &scriptJob{t: t, dir: dir, home: home, paths: paths, exited: make(chan error, 1)}
	args := append(append([]string{}, argv[1:]...), "-c", script)
	j.cmd = exec.Command(argv[0], args...) //nolint:gosec // the script under test
	j.cmd.Dir = dir
	j.cmd.Env = append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + home, "FAKE_DIR=" + dir,
	}, o.env...)
	j.cmd.Stdin = strings.NewReader(`fix "the" flake`)
	j.cmd.Stdout, j.cmd.Stderr = &j.stdout, &j.stderr
	j.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := j.cmd.Start(); err != nil {
		t.Fatalf("starting the script: %v", err)
	}
	go func() { j.exited <- j.cmd.Wait() }()
	// Whatever a failed case left running goes with it.
	t.Cleanup(func() { _ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL) })
	return j
}

// runScript runs the script to its end.
func runScript(t *testing.T, o scriptOpts) scriptRun {
	t.Helper()
	return startScript(t, o).wait()
}

// startPieces runs body, pieces of a run's script put together by a case,
// under sh as [startScript] runs the whole: in a process group of its own,
// behind the shell's prelude, so the same waits and checks read it.
func startPieces(t *testing.T, sh boxShell, body string) *scriptJob {
	t.Helper()
	signalsDeliverable()
	dir := t.TempDir()
	argv := sh.argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	j := &scriptJob{t: t, dir: dir, home: dir, paths: codingagent.Paths{Home: dir}, exited: make(chan error, 1)}
	args := append(append([]string{}, argv[1:]...), "-c", sh.prelude+body)
	j.cmd = exec.Command(argv[0], args...) //nolint:gosec // the pieces under test
	j.cmd.Dir = dir
	j.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "FAKE_DIR=" + dir}
	j.cmd.Stdout, j.cmd.Stderr = &j.stdout, &j.stderr
	j.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := j.cmd.Start(); err != nil {
		t.Fatalf("starting the pieces: %v", err)
	}
	go func() { j.exited <- j.cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL) })
	return j
}

// signal sends sig to the script's whole process group, as a teardown does.
func (j *scriptJob) signal(sig syscall.Signal) {
	j.t.Helper()
	if err := syscall.Kill(-j.cmd.Process.Pid, sig); err != nil {
		j.t.Fatalf("signalling the script's group with %v: %v", sig, err)
	}
}

// await polls until cond holds, failing the case after ten seconds — the
// script's own steps take milliseconds against the fake — or AT ONCE when the
// script has exited without it holding: nothing an exited script left behind
// will change, and a case that waited out its bound reported a script that
// stopped at its first line as one that was slow.
func (j *scriptJob) await(what string, cond func() bool) {
	j.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		select {
		case err := <-j.exited:
			j.exited <- err
			if cond() {
				return
			}
			j.t.Fatalf("the script exited (%v) before %s; calls:\n%s\nstderr:\n%s", err, what,
				strings.Join(j.calls(), "\n"), j.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			j.t.Fatalf("waited 10s for %s; calls:\n%s\nstderr:\n%s", what,
				strings.Join(j.calls(), "\n"), j.stderrSoFar())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stderrSoFar is the script's stderr, read only once it has exited — the
// buffer is written by exec's copier until then.
func (j *scriptJob) stderrSoFar() string {
	select {
	case err := <-j.exited:
		j.exited <- err
		return j.stderr.String()
	default:
		return "(still running)"
	}
}

func (j *scriptJob) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(j.dir, "calls")) //nolint:gosec // under the test's own dir
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (j *scriptJob) document() string {
	doc, _ := os.ReadFile(j.paths.CredentialDocument())
	return string(doc)
}

// wait waits for the script to exit — thirty seconds at most, past which it is
// hung rather than slow — and reads what it left.
func (j *scriptJob) wait() scriptRun {
	j.t.Helper()
	return j.waitWithin(30*time.Second, "it is hung rather than slow")
}

// waitWithin waits for the script to exit, failing the case — and saying why
// so long is wrong — if it has not after limit, and reads what it left.
func (j *scriptJob) waitWithin(limit time.Duration, why string) scriptRun {
	j.t.Helper()
	var err error
	select {
	case err = <-j.exited:
		j.exited <- err
	case <-time.After(limit):
		j.signal(syscall.SIGKILL)
		j.t.Fatalf("the script had not exited after %s, and %s; calls:\n%s", limit, why,
			strings.Join(j.calls(), "\n"))
	}
	run := scriptRun{dir: j.dir, home: j.home, stdout: j.stdout.String(), stderr: j.stderr.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		run.code = exit.ExitCode()
	} else if err != nil {
		j.t.Fatalf("running the script: %v", err)
	}
	run.calls = j.calls()
	run.document = j.document()
	brief, _ := os.ReadFile(filepath.Join(j.dir, "brief")) //nolint:gosec // under the test's own dir
	run.brief = string(brief)
	return run
}

// assertGroupGone fails the case if anything of the script's process group is
// still running five seconds after the script exited — and nothing should be:
// the script stops what it started, and waits for it, before it exits, so
// what this waits out is only the kernel's own bookkeeping.
//
// A ZOMBIE IS NOT A LEFTOVER. A process the group's signal killed whose parent
// it killed too is reaped by whatever reaps orphans, which on a machine whose
// first process does not may be never; it runs nothing, so where /proc says
// every process left in the group is one, the group is gone.
func (j *scriptJob) assertGroupGone() {
	j.t.Helper()
	pgid := j.cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		live, readable := liveInGroup(pgid)
		if readable && len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			j.t.Fatalf("the script's process group (%d) is still running 5s after it exited "+
				"(kill: %v):\n%s", pgid, err, strings.Join(live, "\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveInGroup lists the processes of group pgid that are not zombies, read from
// /proc — false where there is no /proc to read, as on darwin.
func liveInGroup(pgid int) ([]string, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var live []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")) //nolint:gosec // /proc
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp …, comm in parentheses and free to
		// hold spaces, so the fields are counted from its closing one.
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 3 || fields[2] != strconv.Itoa(pgid) || fields[0] == "Z" {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline")) //nolint:gosec // /proc
		live = append(live, e.Name()+" "+strings.ReplaceAll(string(cmdline), "\x00", " "))
	}
	return live, true
}

// callIndex is where the first call starting with prefix falls, or -1.
func (r scriptRun) callIndex(prefix string) int {
	for i, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func readIn(t *testing.T, dir, name string) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // under the test's own dir
	return string(raw)
}

// boxShell is one sh a box may run the script under.
type boxShell struct {
	name string
	// argv is the shell and its flags, before -c; empty is the sh on PATH.
	argv []string
	// prelude is shell text the script is run behind.
	prelude string
}

// busyboxKill is busybox ash's `kill`, on any other sh: an operand that is
// empty answered with success and nothing signalled — what busybox 1.35 does
// with both `kill ""` and `kill -0 ""`, where dash and bash refuse it.
const busyboxKill = `kill() { for a; do [ -n "$a" ] || return 0; done; command kill "$@"; }` + "\n"

// boxShells is every sh a box's image may run the script under that this
// machine has: the sh on PATH, which is what the runner names; dash, Debian's
// and Ubuntu's sh; bash in POSIX mode, which is what it is as Fedora's, RHEL's
// and Arch's sh; and busybox ash, Alpine's — the image OpenCode 2's musl builds
// are for. Each only where it is installed, and once however many names reach
// the same binary.
//
// AND ALWAYS the sh on PATH with busybox ash's kill ([busyboxKill]), because
// that is the behaviour of busybox's the script has tripped on, and a machine
// without busybox — most CI runners — would otherwise never run the cases that
// caught it.
func boxShells(t *testing.T) []boxShell {
	t.Helper()
	candidates := []boxShell{
		{name: "sh"},
		{name: "dash", argv: []string{"dash"}},
		{name: "bash-posix", argv: []string{"bash", "--posix"}},
		{name: "busybox", argv: []string{"busybox", "sh"}},
	}
	seen := map[string]bool{}
	var shells []boxShell
	for _, c := range candidates {
		argv := c.argv
		if len(argv) == 0 {
			argv = []string{"sh"}
		}
		path, err := exec.LookPath(argv[0])
		if err != nil {
			if c.name == "sh" {
				t.Fatalf("no sh on PATH to run the script under: %v", err)
			}
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		key := strings.Join(append([]string{path}, argv[1:]...), " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		shells = append(shells, c)
	}
	return append(shells, boxShell{name: "sh-with-busybox-kill", prelude: busyboxKill})
}

// forEachShell runs test under every sh in [boxShells], in parallel.
func forEachShell(t *testing.T, test func(t *testing.T, sh boxShell)) {
	t.Helper()
	for _, sh := range boxShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			test(t, sh)
		})
	}
}

var bridgeOnly = map[string]sandbox.MCPServer{
	"crewlet-bridge": {Name: "crewlet-bridge", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp/x"},
}

const (
	seededLogin    = `[{"id":"cred_1","key":"old"}]`
	refreshedLogin = `[{"id":"cred_1","key":"new"}]`
)

// promptEnd is how long a run's script may take to end against the fake in a
// case that stretches its stop wait to [stretchedStop]: an end there takes
// about a second, and one that waited out a stop of nothing takes the whole
// minute — so the bound sits far from both, where a loaded runner cannot
// carry a prompt end past it.
const promptEnd = 15 * time.Second

// stretchedStop is the stop wait a case about an end not waiting runs the
// script at: long enough that waiting it out once is unmistakable.
const stretchedStop = time.Minute

// THE BRIEF IS SENT ONLY ONCE THE SEAT'S TOOLS EXIST. OpenCode 2 fixes a step's
// tools as the step starts, so a brief sent before the run's MCP servers have
// connected gets a first step with none of them — which is what `--standalone`
// does. The script waits until every declared server has left `pending`, across
// an answer that lists nothing and one that lists it pending, and only then
// runs the brief, verbatim on stdin, against the server it started.
func TestTheBriefWaitsForTheSeatsTools(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, req: sandbox.RunRequest{MCPServers: bridgeOnly}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if polls := strings.TrimSpace(readIn(t, run.dir, "polls")); polls != "3" {
			t.Errorf("mcp.list was asked %s times, want 3 — through the empty and the pending "+
				"answers to the connected one", polls)
		}
		runAt := run.callIndex("run ")
		if runAt < 0 || runAt < run.callIndex("api --server http://127.0.0.1:4242 mcp.list") {
			t.Fatalf("the brief was not run after the servers were asked about:\n%s",
				strings.Join(run.calls, "\n"))
		}
		if !strings.HasPrefix(run.calls[runAt], "run --server http://127.0.0.1:4242 --format json --title crewlet --session ses_") {
			t.Errorf("the run = %q, want it against the script's own server", run.calls[runAt])
		}
		if run.brief != `fix "the" flake` {
			t.Errorf("the run read %q, want the brief verbatim", run.brief)
		}
		if pw := readIn(t, run.dir, "run-password"); pw != "pw-secret" {
			t.Errorf("the run was handed %q, want the server's password", pw)
		}
		if _, err := os.Stat(filepath.Join(run.home, ".crewlet", "opencode-server")); !os.IsNotExist(err) {
			t.Errorf("the server's password outlived its read (%v)", err)
		}
	})
}

// ONLY THE SERVER IS HANDED THE CONFIGURATION. The run's configuration is the
// content of the file WriteConfig left, handed to `opencode serve` as
// OPENCODE_CONFIG_CONTENT — verbatim, its `$` and backquotes never expanded —
// and to no other command the script runs: the login's import and export, the
// run's own client and its `api` calls are each started without it.
//
// Mutation: export the variable for the whole script, and every other command
// is handed it.
func TestOnlyTheServerIsHandedTheConfiguration(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, login: seededLogin,
			req: sandbox.RunRequest{MCPServers: bridgeOnly}, env: []string{"FAKE_REFRESH=" + refreshedLogin}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if got := readIn(t, run.dir, "serve-config"); got != scriptConfig {
			t.Errorf("the server was handed %q, want the run's configuration verbatim %q", got, scriptConfig)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "configured")), "\n") {
			handed, call, _ := strings.Cut(line, "|")
			seen[call] = true
			if (handed == "handed") != strings.HasPrefix(call, "serve ") {
				t.Errorf("%q was handed the configuration: %t, want it on the server alone", call, handed == "handed")
			}
		}
		for _, call := range []string{"serve --hostname", "auth import", "auth export", "run --server", "api --server"} {
			if !seen[call] {
				t.Errorf("the script never ran %q, so what it was handed went unasked: %v", call, seen)
			}
		}
	})
}

// ONLY THE SERVER IS HANDED THE RUN'S SESSION, and only for a run wired to its
// notes: the notes plugin the server loads admits notes to the session the
// run's script names, which it reads from CREWLET_STEER_SESSION — the same id
// the run's client is pointed at with --session. No other command the script
// runs is handed it, and a run wired to no notes hands nobody it.
//
// Mutation: export the variable for the whole script, or hand it the run's
// server whatever its wiring.
func TestOnlyTheServerIsHandedTheRunsSession(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, wired := range []bool{true, false} {
			req := sandbox.RunRequest{MCPServers: bridgeOnly}
			if wired {
				req.Steer = steerWiring
			}
			run := runScript(t, scriptOpts{shell: sh, login: seededLogin, req: req})
			if run.code != 0 {
				t.Fatalf("wired %v: exit %d:\n%s", wired, run.code, run.stderr)
			}
			at := run.callIndex("run --server ")
			if at < 0 {
				t.Fatalf("wired %v: the brief was never run:\n%s", wired, strings.Join(run.calls, "\n"))
			}
			_, session, _ := strings.Cut(run.calls[at], "--session ")
			session, _, _ = strings.Cut(session, " ")
			served := false
			for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "steered")), "\n") {
				handed, call, _ := strings.Cut(line, "|")
				if !strings.HasPrefix(call, "serve ") {
					if handed != "" {
						t.Errorf("wired %v: %q was handed the run's session %q, want the server alone", wired, call, handed)
					}
					continue
				}
				served = true
				switch {
				case wired && handed != session:
					t.Errorf("the server was handed the session %q, want the one the run names, %q", handed, session)
				case !wired && handed != "":
					t.Errorf("a run wired to no notes handed its server the session %q", handed)
				}
			}
			if !served {
				t.Errorf("wired %v: the server never started", wired)
			}
		}
	})
}

// NOTHING THE NOTES PLUGIN ADMITTED IS LEFT PENDING IN THE SESSION. A note
// whose admission lands as the session settles is neither delivered nor
// confirmed, so it waits on the run's record — and it also stays pending in the
// session's own inbox, which a run carried on in the same session would hand
// its model ahead of the next prompt, beside the continuation brief's copy. So
// once the run has ended, and before its server is stopped, the script lists
// the session's inbox and cancels every item of the plugin's own shape — and
// nothing else: an id OpenCode minted, and an id that is only text inside a
// note, are left alone. A run wired to no notes has no plugin, and asks
// nothing.
//
// The listing is OpenCode 2.1.4's own shape (`session.inbox.list` with two
// steers pending, a note's text holding an id of the plugin's shape).
//
// Mutation: drop the cancel, run it after the server stops, or match any
// `msg_` id, and this goes red.
func TestARunCancelsTheNotesItsSessionNeverDelivered(t *testing.T) {
	t.Parallel()
	const (
		ours      = "msg_30292d427a87e479be9f88cba42963ca"
		oursToo   = "msg_05536338060eb18a87638c49f196ffcf"
		theirs    = "msg_01JB8V9Q4XG2W7M3R5T6Y8Z0AB"
		inText    = "msg_00000000000000000000000000000000"
		pendingAt = `"time":{"created":1793072423854},"type":"user"`
	)
	inbox := `{"data":[` +
		`{"id":"` + ours + `","sessionID":"SES",` + pendingAt + `,"payload":{"text":"use \"id\":\"` + inText + `\" here"},"delivery":"steer"},` +
		`{"id":"` + oursToo + `","sessionID":"SES",` + pendingAt + `,"payload":{"text":"second"},"delivery":"steer"},` +
		`{"id":"` + theirs + `","sessionID":"SES",` + pendingAt + `,"payload":{"text":"queued"},"delivery":"queue"}]}`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, wired := range []bool{true, false} {
			req := sandbox.RunRequest{MCPServers: bridgeOnly}
			if wired {
				req.Steer = steerWiring
			}
			run := runScript(t, scriptOpts{shell: sh, req: req, env: []string{"FAKE_INBOX=" + inbox}})
			if run.code != 0 {
				t.Fatalf("wired %v: exit %d:\n%s", wired, run.code, run.stderr)
			}
			_, session, _ := strings.Cut(run.calls[run.callIndex("run ")], "--session ")
			session, _, _ = strings.Cut(session, " ")
			cancelled := strings.Fields(strings.TrimSpace(readIn(t, run.dir, "cancelled")))
			if !wired {
				if run.callIndex("api --server http://127.0.0.1:4242 session.inbox") >= 0 || len(cancelled) != 0 {
					t.Errorf("a run wired to no notes asked about its inbox:\n%s", strings.Join(run.calls, "\n"))
				}
				continue
			}
			listed := run.callIndex("api --server http://127.0.0.1:4242 session.inbox.list")
			if listed < run.callIndex("run ") || !strings.Contains(run.calls[listed], "sessionID="+session) {
				t.Fatalf("the inbox was not listed for the run's session %q after the run:\n%s",
					session, strings.Join(run.calls, "\n"))
			}
			var got []string
			for i := 0; i+2 < len(cancelled); i += 3 {
				if cancelled[i] != session || cancelled[i+2] != "serving" {
					t.Errorf("cancelled %s of session %s with the server %s, want the run's session %s "+
						"while its server serves", cancelled[i+1], cancelled[i], cancelled[i+2], session)
				}
				got = append(got, cancelled[i+1])
			}
			slices.Sort(got)
			if want := []string{oursToo, ours}; !slices.Equal(got, want) {
				t.Errorf("cancelled %v, want the plugin's two pending notes %v and nothing else", got, want)
			}
		}
	})
}

// A RUN WITH NO CONFIGURATION STARTS NOTHING. Handed none, the server would run
// on whatever the image and the checkout hold, with neither the run's posture
// nor its servers; so a run whose configuration is missing is refused before
// any login is imported or any server started, naming the file it looked for.
//
// Mutation: drop the check and the server starts, handed an empty
// configuration.
func TestARunWithNoConfigurationStartsNothing(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		job := startScript(t, scriptOpts{shell: sh, login: seededLogin, unconfigured: true})
		run := job.wait()
		if run.code != 1 {
			t.Errorf("exit %d, want 1", run.code)
		}
		if want := "no OpenCode configuration at " + job.paths.OpenCodeConfig(); !strings.Contains(run.stderr, want) {
			t.Errorf("stderr = %q, want it to say %q", run.stderr, want)
		}
		if run.callIndex("serve ") >= 0 || run.callIndex("auth ") >= 0 {
			t.Errorf("a run with no configuration ran:\n%s", strings.Join(run.calls, "\n"))
		}
	})
}

// A SERVER NAMED THE WAY OPENCODE NAMES IT. The readiness check matches the
// `mcp.list` answer as text, so each name has to be spelled as OpenCode's
// JSON.stringify spells it — and encoding/json spells `&`, `<`, `>`, U+2028
// and U+2029 otherwise, so a server named with any of them never matched and
// every run waited out the whole bound for one that had settled at once.
//
// The answer is OpenCode 2.0.32's own, recorded from `opencode api mcp.list`
// with two servers that exit at once — only the location's directory, a
// scratch path, is replaced.
//
// Mutation: spell the names with json.Marshal, and the run waits the bound
// and says a server was still connecting.
func TestTheReadinessCheckSpellsANameAsOpenCodeDoes(t *testing.T) {
	t.Parallel()
	const recorded = "{\"location\":{\"directory\":\"/home/crewlet/workspace\"},\"data\":[" +
		"{\"name\":\"quote\\\"back\\\\slash\\ttab\",\"status\":{\"status\":\"failed\",\"error\":\"Connection closed: MCP server process exited with code 1\"}}," +
		"{\"name\":\"R&D <x> tools\u2028\",\"status\":{\"status\":\"failed\",\"error\":\"Connection closed: MCP server process exited with code 1\"}}]}"
	servers := map[string]sandbox.MCPServer{
		"R&D <x> tools\u2028":     {Name: "R&D <x> tools\u2028", Command: "false"},
		"quote\"back\\slash\ttab": {Name: "quote\"back\\slash\ttab", Command: "false"},
	}
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{
			shell: sh,
			req:   sandbox.RunRequest{MCPServers: servers},
			env:   []string{"FAKE_MCP_LIST=" + recorded},
			times: &codingagent.OpenCodeTimes{MCP: 3 * time.Second, Sync: time.Hour},
		})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if strings.Contains(run.stderr, "still connecting") {
			t.Errorf("the run waited for servers OpenCode had already settled:\n%s", run.stderr)
		}
		if polls := strings.TrimSpace(readIn(t, run.dir, "polls")); polls != "1" {
			t.Errorf("mcp.list was asked %s times, want once: the first answer settles both", polls)
		}
	})
}

// JSON.stringify's spelling, to the byte: `"`, `\` and the control characters
// escaped — five by their short forms, the rest as \u and lowercase hex — and
// everything else literal, DEL and U+2029 included. A byte that is not UTF-8
// is U+FFFD written as itself, which is the name OpenCode read from the config
// encoding/json wrote it into.
func TestANameIsSpelledAsJSONStringifySpellsIt(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a&b<c>d":        `"a&b<c>d"`,
		"x\u2029y":       "\"x\u2029y\"",
		"q\"b\\s":        `"q\"b\\s"`,
		"\b\f\n\r\t":     `"\b\f\n\r\t"`,
		"\x01\x1f\x7f":   "\"\\u0001\\u001f\x7f\"",
		"bad\xffbyte":    "\"bad\ufffdbyte\"",
		"Ünï 🙂":          `"Ünï 🙂"`,
		"plain-name_1.0": `"plain-name_1.0"`,
	} {
		if got := codingagent.JSONStringify(in); got != want {
			t.Errorf("JSONStringify(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE SERVER IS THE RUN'S AND ENDS WITH IT: the done marker is written only
// once the script exits, so a server left running would hold nothing open,
// and one the script never stopped would keep the box's bridge connection
// and its credentials alive past the run.
func TestTheRunsServerIsStoppedWithIt(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh})
		assertServerStopped(t, run.dir)
	})
}

func assertServerStopped(t *testing.T, dir string) {
	t.Helper()
	raw := readIn(t, dir, "serve.pid")
	if raw == "" {
		t.Fatal("no server was started")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the server (pid %d) outlived the run", pid)
	}
}

// A RUN THAT STARTED NOTHING TO STOP ENDS AT ONCE, under every sh a box can
// have. The script's stops are bounded waits for a process to go, and a stop of
// a process that was never started — the login exporter of a run given no
// login, and both of them at an exit before the server listens — must not wait
// at all. It did, under busybox ash, whose `kill ""` and `kill -0 ""` succeed:
// five seconds at the end of every run given no login.
//
// Each case stretches the stop wait to a minute, so a run that waited out one
// stop of nothing cannot pass for a prompt one on a loaded runner.
//
// Mutation: drop end's test for an empty pid, and the busybox-kill shell waits
// out the minute in both cases.
func TestARunThatStartedNothingToStopEndsAtOnce(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		t.Run("no-login", func(t *testing.T) {
			t.Parallel()
			run := startScript(t, scriptOpts{shell: sh, times: &codingagent.OpenCodeTimes{Stop: stretchedStop}}).
				waitWithin(promptEnd, "a run given no login waited to stop an exporter it never started")
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
		})
		t.Run("server-never-listens", func(t *testing.T) {
			t.Parallel()
			run := startScript(t, scriptOpts{
				shell: sh, env: []string{"FAKE_SERVE=silent"},
				times: &codingagent.OpenCodeTimes{Listen: time.Second, Stop: stretchedStop},
			}).waitWithin(promptEnd, "an early exit waited to stop an exporter it never started")
			if run.code != 1 {
				t.Fatalf("exit %d, want the refusal's 1:\n%s", run.code, run.stderr)
			}
			assertServerStopped(t, run.dir)
		})
	})
}

// THE SERVER'S PASSWORD IS ITS OWN. `opencode serve` takes a password from
// OPENCODE_PASSWORD or OPENCODE_SERVER_PASSWORD and then prints none, so a box
// environment carrying either — an operator's role.sandbox.env, an image built
// for OpenCode — had the script wait out the whole bound and fail saying the
// server never listened. The server is started with neither, and the run's
// own clients present the password it minted.
//
// Mutation: drop the unset, and the run fails with no password printed.
func TestARunsServerNeverTakesAnOutsidePassword(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{
			shell: sh,
			env:   []string{"OPENCODE_SERVER_PASSWORD=operator-chosen", "OPENCODE_PASSWORD=operator-other"},
			times: &codingagent.OpenCodeTimes{Listen: 5 * time.Second, Sync: time.Hour},
		})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if env := readIn(t, run.dir, "serve-env"); env != "|" {
			t.Errorf("the server was started with OPENCODE_PASSWORD|OPENCODE_SERVER_PASSWORD = %q, want neither", env)
		}
		if pw := readIn(t, run.dir, "run-password"); pw != "pw-secret" {
			t.Errorf("the run presented %q, want the password its server minted", pw)
		}
	})
}

// A SERVER THAT NEVER SAYS ALL IT MUST IS NAMED FOR WHAT IT LEFT OUT — where it
// listens, or its password — rather than every such failure reading as one
// that never listened.
func TestAServerThatNeverPrintsWhatTheRunNeedsIsNamed(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for serve, want := range map[string]string{
			"listen-only": "opencode serve listened but printed no server password within 1s",
			"silent":      "opencode serve did not say where it listens within 1s",
		} {
			t.Run(serve, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{
					shell: sh,
					env:   []string{"FAKE_SERVE=" + serve},
					times: &codingagent.OpenCodeTimes{Listen: time.Second, Sync: time.Hour},
				})
				if run.code == 0 {
					t.Fatal("the run went on without what it needs from its server")
				}
				if !strings.Contains(run.stderr, want) {
					t.Errorf("stderr = %q, want %q", run.stderr, want)
				}
				if run.callIndex("run ") >= 0 {
					t.Errorf("the brief was run anyway:\n%s", strings.Join(run.calls, "\n"))
				}
				assertServerStopped(t, run.dir)
			})
		}
	})
}

// THE SCRIPT WAITS FOR BOTH LINES, whichever its server prints first: a run
// sent on once the password was there and the address was not would be sent
// to no server at all.
//
// Mutation: wait for the password line alone, and the brief goes to an empty
// address.
func TestTheRunWaitsForTheServersAddressAndItsPassword(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_SERVE=password-first"}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if at := run.callIndex("run "); at < 0 ||
			!strings.HasPrefix(run.calls[at], "run --server http://127.0.0.1:4242 ") {
			t.Errorf("the brief was not sent to the server's address:\n%s", strings.Join(run.calls, "\n"))
		}
	})
}

// THE RUN'S EXIT IS THE RUN'S, and its usage follows it: the session's report,
// asked of the run's own session, as the stream's last line — which the
// decoder reads as the run's tokens and price.
func TestTheScriptReportsTheSessionsUsageAndKeepsTheRunsExit(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_RUN_EXIT=3"}})
		if run.code != 3 {
			t.Errorf("exit %d, want the run's own 3:\n%s", run.code, run.stderr)
		}
		runCall := run.calls[run.callIndex("run ")]
		_, session, _ := strings.Cut(runCall, "--session ")
		session, _, _ = strings.Cut(session, " ")
		if !strings.Contains(run.calls[run.callIndex("api --server http://127.0.0.1:4242 session.get")],
			"sessionID="+session) {
			t.Errorf("the usage was not asked of the run's own session %q:\n%s",
				session, strings.Join(run.calls, "\n"))
		}
		res := codingagent.OpenCode{}.Parse(run.stdout)
		if res.InputTokens != 51 || res.OutputTokens != 5 || res.CostUSD != 0.5 || res.SessionID != session {
			t.Errorf("result = %+v, want the session's report read: in 51 out 5 cost 0.5 session %s",
				res, session)
		}
	})
}

// A BOX GIVEN A LOGIN IMPORTS IT BEFORE THE RUN AND WRITES THE REFRESHED ONE
// BACK AFTER — the export, over the document the box was seeded with, which is
// what the box's teardown copies back to the shared login. Never an export
// that holds no login over it, and never a document where the box was given
// none.
func TestABoxsLoginIsImportedAndTheRefreshedOneWrittenBack(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, login: seededLogin, env: []string{"FAKE_REFRESH=" + refreshedLogin}})
		if imported := readIn(t, run.dir, "imported"); imported != seededLogin {
			t.Errorf("imported %q, want the login the box was given", imported)
		}
		if run.callIndex("auth import --standalone") > run.callIndex("serve ") {
			t.Errorf("the login was imported after the server started:\n%s", strings.Join(run.calls, "\n"))
		}
		if run.document != refreshedLogin {
			t.Errorf("the box's login is %q, want the refreshed export", run.document)
		}

		run = runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_REFRESH=" + `[{"id":"cred_1","key":"minted"}]`}})
		if run.document != "" || run.callIndex("auth ") >= 0 {
			t.Errorf("a box given no login touched one: document %q, calls\n%s",
				run.document, strings.Join(run.calls, "\n"))
		}
	})
}

// exportsHoldingNoLogin are what an export can print that is no login to keep:
// the empty array OpenCode prints for a database with nothing in it (with and
// without whitespace), nothing at all, an array cut short, and text that is
// not the JSON an export prints.
var exportsHoldingNoLogin = map[string]string{
	"empty array":       "[]",
	"spaced empty":      " [ ]\n",
	"nothing":           "",
	"cut short":         `[{"id":"cred_1","key":"n`,
	"not an export":     "Imported 1 credential\n",
	"an object, not []": `{"id":"cred_1"}`,
}

// No export holding no login replaces the box's, at the run's end: the
// teardown writes what the document holds back over the shared login.
func TestAnExportHoldingNoLoginNeverReplacesTheBoxs(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, export := range exportsHoldingNoLogin {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{shell: sh, login: seededLogin, env: []string{"FAKE_REFRESH=" + export}})
				if run.document != seededLogin {
					t.Errorf("the box's login became %q", run.document)
				}
			})
		}
	})
}

// A LOGIN REFRESHED MID-RUN SURVIVES THE RUN BEING KILLED. Every production
// teardown of a run in flight is a SIGKILL of its group or a `docker rm -f`,
// in which nothing in the box runs again, and a login OpenCode refreshed lives
// in its database until it is exported — so it is exported while the run
// works, through the run's own server, and what a kill finds is the document
// the last export left.
//
// Mutation: export only when the run ends, and the document is still the seed
// when the run is killed.
func TestALoginRefreshedMidRunSurvivesTheRunBeingKilled(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{
			shell: sh,
			login: seededLogin,
			env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_RUN_SLEEP=60"},
			times: &codingagent.OpenCodeTimes{Sync: 200 * time.Millisecond},
		})
		j.await("the refreshed login exported mid-run", func() bool { return j.document() == refreshedLogin })
		if countCalls(j.calls(), "auth export --server http://127.0.0.1:4242") == 0 {
			t.Errorf("the login was not exported through the run's own server:\n%s", strings.Join(j.calls(), "\n"))
		}
		if _, err := os.Stat(filepath.Join(j.dir, "refreshed")); err != nil {
			t.Fatalf("the run had not refreshed the login yet: %v", err)
		}
		j.signal(syscall.SIGKILL)
		run := j.wait()
		if run.code != -1 {
			t.Fatalf("exit %d, want the kill to have ended the run", run.code)
		}
		if run.document != refreshedLogin {
			t.Errorf("after the kill the box's login is %q, want the refreshed one", run.document)
		}
	})
}

// The loop holds its exports to the same rule as the last one: an export that
// holds no login never replaces the box's, however often it comes round.
func TestTheLoginLoopNeverWritesAnExportHoldingNoLogin(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, export := range exportsHoldingNoLogin {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				j := startScript(t, scriptOpts{
					shell: sh,
					login: seededLogin,
					env:   []string{"FAKE_REFRESH=" + export, "FAKE_RUN_SLEEP=60"},
					times: &codingagent.OpenCodeTimes{Sync: 100 * time.Millisecond},
				})
				j.await("three exports after the refresh", func() bool {
					refreshed := false
					exports := 0
					for _, c := range j.calls() {
						if strings.HasPrefix(c, "run ") {
							refreshed = true
						} else if refreshed && strings.HasPrefix(c, "auth export --server") {
							exports++
						}
					}
					return exports >= 3
				})
				j.signal(syscall.SIGKILL)
				if run := j.wait(); run.document != seededLogin {
					t.Errorf("the box's login became %q", run.document)
				}
			})
		}
	})
}

// NOTHING OF A RUN OUTLIVES IT. The login exporter is stopped, and waited for,
// before the run's last export — or the two would race to rename, and the
// exporter would go on exporting against a dead server, in the job's group,
// until the box was torn down — and it reaps its own sleep before it goes, or
// that sleep would be left in the group for up to an interval. Both at an
// exporter asleep when the run ends and at one that may be part way through an
// export.
//
// Mutations: drop the final unsync, and the exporter is still running after
// the script; drop the exporter's TERM trap, and its sleep is.
func TestNothingOfARunOutlivesIt(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, sync := range map[string]time.Duration{
			"exporter-asleep":    time.Hour,
			"exporter-exporting": 50 * time.Millisecond,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				j := startScript(t, scriptOpts{
					shell: sh, login: seededLogin,
					env:   []string{"FAKE_REFRESH=" + refreshedLogin},
					times: &codingagent.OpenCodeTimes{Sync: sync},
				})
				run := j.wait()
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				if run.document != refreshedLogin {
					t.Errorf("the box's login is %q, want the refreshed export", run.document)
				}
				j.assertGroupGone()
			})
		}
	})
}

// THE EXPORTER SIGNALS NO PID IT HAS ALREADY REAPED. It is stopped by a
// SIGTERM that may arrive while it is exporting — an interval after it last
// reaped its sleep — and the trap that ends it kills the sleep it is waiting
// on. So it forgets a sleep's pid the moment it has reaped it: kept, that trap
// signalled a pid the system was free to have handed to another process by
// then, which in a `direct` box, sharing the host's pids, is any process of
// the engine's user.
//
// Every kill the script makes and every sleep it starts and sees end are
// recorded, by functions standing in for both, and the exporter is stopped
// part way through an export.
//
// Mutation: keep the sleep's pid after its wait, and the trap kills it.
func TestTheLoginExporterSignalsNoPidItHasReaped(t *testing.T) {
	t.Parallel()
	const recorder = `kill() { echo "kill $*" >> "$FAKE_DIR/pids"; command kill "$@"; }
sleep() { __p=$(exec sh -c 'echo $PPID'); echo "start $__p" >> "$FAKE_DIR/pids"; command sleep "$@"; echo "end $__p" >> "$FAKE_DIR/pids"; }
`
	j := startScript(t, scriptOpts{
		shell: boxShell{name: "sh-recording-pids", prelude: recorder},
		login: seededLogin,
		env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_EXPORT_SLEEP=1", "FAKE_RUN_UNTIL=released"},
		times: &codingagent.OpenCodeTimes{Sync: 10 * time.Millisecond},
	})
	j.await("the exporter part way through an export", func() bool {
		_, err := os.Stat(filepath.Join(j.dir, "exporting"))
		return err == nil
	})
	if err := os.WriteFile(filepath.Join(j.dir, "released"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := j.wait()
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	script := strconv.Itoa(j.cmd.Process.Pid)
	ended := map[string]bool{}
	reaped := 0
	for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "pids")), "\n") {
		// THE PID IS THE LAST WORD, after any signal a kill names: the
		// sleep is ended with `kill -9`.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("an unreadable record %q", line)
		}
		verb, operand := fields[0], fields[len(fields)-1]
		switch {
		case verb == "end":
			ended[operand] = true
			if operand != script {
				reaped++
			}
		case verb == "kill" && ended[operand]:
			t.Errorf("the script killed %s after that sleep had ended and been reaped", operand)
		}
	}
	if reaped == 0 {
		t.Fatal("the exporter reaped no sleep before it was stopped, so nothing was tested")
	}
}

// AN EXPORTER STOPPED AS IT STARTS GOES AT ONCE, under every sh a box can
// have, and leaves nothing behind. It is forked while the script traps
// SIGTERM, and a SIGTERM that reaches it before it has run lands on the
// script's handler, which dash clears as the subshell begins: the stop is
// dropped, so unsync tells it again until it goes.
//
// TWENTY EXPORTERS, ONE AFTER ANOTHER, each stopped straight after its fork,
// because whether one stop is lost depends on whether the fork ran first: one
// at a time, every stop is lost under dash, but with this case's shells
// running beside each other a fork often runs first, so a single exporter
// stopped once passed two runs in three with the repeat removed. Twenty make
// a lost stop all but certain under dash on every run.
//
// The stop wait is stretched to a minute, so a stop that was lost and waited
// out cannot pass for a prompt one.
//
// Mutation: tell the exporter once (drop unsync's second operand), and the
// dash cases wait out the minute.
func TestAnExporterStoppedAsItStartsGoesAtOnce(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startPieces(t, sh, "trap 'exit 143' TERM\nkeep() { :; }\n"+
			codingagent.OpenCodeStops(stretchedStop)+"n=0\nwhile [ \"$n\" -lt 20 ]; do\n"+
			codingagent.OpenCodeExporter(time.Hour, "sync")+" &\nxpid=$!\nunsync\n"+
			"n=$((n+1))\ndone\n")
		run := j.waitWithin(promptEnd, "a stop sent as the exporter started was lost and waited out")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		j.assertGroupGone()
	})
}

// A STOP AS THE EXPORTER STARTS ITS SLEEP LEAVES NO SLEEP. Between starting a
// sleep in the background and holding its pid the exporter has nothing it
// could kill, so a SIGTERM there is kept until it does. Acted on at once, the
// trap found no pid and left the sleep in the job's group for the rest of its
// interval. No signal from outside can be timed into that window, so the
// exporter here signals itself in it, once, from inside its own text.
//
// Mutation: act on the stop at once (drop the trap's test of w), and the
// hour's sleep is still running after the exporter has gone.
func TestAStopAsTheExporterStartsItsSleepLeavesNoSleep(t *testing.T) {
	t.Parallel()
	exporter := codingagent.OpenCodeExporter(time.Hour, "sync")
	for _, cut := range []struct{ old, new string }{
		{"( ", `( me=$(exec sh -c 'echo $PPID'); `},
		{"sleep 3600 & ", `sleep 3600 & [ -z "$me" ] || { kill -TERM "$me"; me=; }; `},
	} {
		if n := strings.Count(exporter, cut.old); n != 1 {
			t.Fatalf("the exporter holds %q %d times, want once:\n%s", cut.old, n, exporter)
		}
		exporter = strings.Replace(exporter, cut.old, cut.new, 1)
	}
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startPieces(t, sh, "keep() { :; }\n"+exporter+" &\nwait $!\n")
		run := j.waitWithin(promptEnd, "an exporter that stopped itself did not go")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		j.assertGroupGone()
	})
}

// A SIGNALLED RUN EXPORTS ITS LOGIN BEFORE IT GOES, beside a server that is
// going with the group, and exits with the conventional status — PROMPTLY, and
// leaving nothing behind. A plain sh runs no EXIT trap on a signal it does not
// trap, so a run stopped this way left the export to an end it never reached;
// and under busybox ash, whose `kill ""` succeeds, the trap's exit ran the
// EXIT trap's stops again over processes already gone and waited each out, so
// a run told to stop took ten seconds and was killed before it could exit 143.
// The stop wait is stretched to a minute here so that cannot pass unseen.
//
// Mutations: drop the signal traps, and the refreshed login is lost; drop
// end's test for an empty pid and quit's clearing of the EXIT trap together,
// and the busybox-kill shell waits out two minutes.
func TestASignalledRunExportsItsLoginBeforeItGoes(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, c := range []struct {
			sig  syscall.Signal
			code int
		}{
			{syscall.SIGTERM, 143},
			{syscall.SIGINT, 130},
			{syscall.SIGHUP, 129},
		} {
			t.Run(c.sig.String(), func(t *testing.T) {
				t.Parallel()
				j := startScript(t, scriptOpts{
					shell: sh,
					login: seededLogin,
					env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_RUN_SLEEP=60"},
					// A sync that never comes round, so only the signal's
					// export can have written the document.
					times: &codingagent.OpenCodeTimes{Sync: time.Hour, Stop: stretchedStop},
				})
				j.await("the run to refresh the login", func() bool {
					_, err := os.Stat(filepath.Join(j.dir, "refreshed"))
					return err == nil
				})
				if doc := j.document(); doc != seededLogin {
					t.Fatalf("the document moved before the signal: %q", doc)
				}
				j.signal(c.sig)
				run := j.waitWithin(promptEnd, "a signalled run waited to stop what it had already stopped")
				if run.code != c.code {
					t.Errorf("exit %d, want %d:\n%s", run.code, c.code, run.stderr)
				}
				if run.document != refreshedLogin {
					t.Errorf("after %v the box's login is %q, want the refreshed one", c.sig, run.document)
				}
				if last := run.calls[len(run.calls)-1]; last != "auth export --standalone" {
					t.Errorf("the last call was %q, want the export beside the going server", last)
				}
				assertServerStopped(t, run.dir)
				j.assertGroupGone()
			})
		}
	})
}

// 1.x CANNOT RUN THIS SCRIPT, and says so in its own words rather than leaving
// the usage text 1.x prints for an unknown flag as the run's failure.
func TestOpenCodeOneIsRefusedByName(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_VERSION=1.18.35"}})
		if run.code == 0 {
			t.Fatal("a 1.x box ran")
		}
		if !strings.Contains(run.stderr, "OpenCode 1.18.35") || !strings.Contains(run.stderr, "@opencode/cli") {
			t.Errorf("the refusal does not name the version and the package:\n%s", run.stderr)
		}
		if run.callIndex("serve ") >= 0 || run.callIndex("run ") >= 0 {
			t.Errorf("a 1.x box was driven anyway:\n%s", strings.Join(run.calls, "\n"))
		}
	})
}
