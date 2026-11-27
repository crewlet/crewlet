package executor_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
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

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// fakeOpenCode is an `opencode` answering as OpenCode 2 does, recording each call in
// FAKE_DIR. `serve` writes its password down before printing it, as a real
// server does: written after, a loaded runner's script presented it first and
// was refused. A server's end is read off `serve.alive`, which only it holds
// open and the kernel closes as it exits ([serverPipe]), never its pid, which
// kill -0 finds until reaped (#1164); `up` reads the pid's state as
// internal/procgroup does, since sh cannot ask a pipe without blocking. Pages
// print byte for byte, as the budget cases measure them; config.get, one
// document declaring both bridge names.
// FAKE_RUN_SLEEP alone `exec`s the sleep: dash holds a SIGINT landing as it
// forks a foreground command, so a shell in front could miss a group SIGINT.
// Exports are slowed by a sleep, not a packer stand-in busybox would bypass.
const fakeOpenCode = `#!/bin/sh
d="$FAKE_DIR"
echo "$*" >> "$d/calls"
printf '%s|%s|%s %s\n' "${OPENCODE_DISABLE_MODELS_FETCH-}" "${OPENCODE_DISABLE_AUTOUPDATE-}" "$1" "$2" >> "$d/pinned"
printf '%s|%s %s\n' "${OPENCODE_CONFIG_CONTENT+handed}" "$1" "$2" >> "$d/configured"
printf '%s|%s %s\n' "${CREWLET_RUN_SESSION-}" "$1" "$2" >> "$d/session-handed"
authed() {
  [ "${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}}" = "$(cat "$d/server-password" 2> /dev/null)" ] && return 0
  echo "Error: Server at http://127.0.0.1:4242 rejected the password" >&2; exit 1
}
hangs() {
  case " ${FAKE_HANG-} " in *" $1 "*) ;; *) return 0;; esac
  if [ -n "${FAKE_HANG_ONCE:-}" ]; then [ ! -e "$d/hung-$1" ] || return 0; : > "$d/hung-$1"; fi
  echo "$1 $$" >> "$d/hung"
  [ -z "${FAKE_HANG_DEAF:-}" ] || trap '' TERM
  exec sleep 3600
}
hold() { mkdir -p "$d/sessions" && : > "$d/sessions/$1"; }
held() { [ -n "$1" ] && [ -e "$d/sessions/$1" ]; }
serving() { : > "$d/serve-settled"; exec sleep 60; }
up() {
  p=$(cat "$d/serve.pid" 2> /dev/null); [ -n "$p" ] || return 1
  if [ -r /proc/self/stat ]; then set -- $(sed 's/.*) //' "/proc/$p/stat" 2> /dev/null); else set -- $(ps -o stat= -p "$p" 2> /dev/null); fi
  case "${1-}" in '' | [Xx]*) return 1 ;; Z*) [ "${18:-1}" -gt 1 ] ;; esac
}
case "$1" in
debug) cat "$d/agents.json" 2> /dev/null || echo '[]' ;;
--version)
  [ -z "${FAKE_VERSION_FAIL:-}" ] || { echo "$FAKE_VERSION_FAIL" >&2; exit 3; }
  echo "${FAKE_VERSION:-opencode v2.3.2}" ;;
auth)
  case "$2" in
  import) f=; for a; do f=$a; done; if [ -f "$f" ]; then cat "$f"; else cat; fi > "$d/imported"; cp "$d/imported" "$d/db" ;;
  export)
    [ "$3" != --server ] || { authed; hangs auth.export; }
    if [ -n "${FAKE_EXPORT_SLEEP:-}" ]; then : > "$d/exporting"; sleep "$FAKE_EXPORT_SLEEP"; fi
    cat "$d/db" 2> /dev/null ;;
  esac ;;
serve)
  printf '%s|%s' "${OPENCODE_PASSWORD:-}" "${OPENCODE_SERVER_PASSWORD:-}" > "$d/serve-env"
  printf '%s' "${OPENCODE_CONFIG_CONTENT-}" > "$d/serve-config"
  if [ -n "${FAKE_VALUES:-}" ]; then
    cat "$FAKE_VALUES/v1" > "$d/served-value" 2> /dev/null
    if [ -e "$FAKE_VALUES-fresh" ]; then echo yes; else echo no; fi > "$d/served-beside-other"
  fi
  exec 9> "$d/serve.alive"
  echo $$ > "$d/serve.pid"
  [ "${FAKE_SERVE:-}" != silent ] || serving
  if [ "${FAKE_SERVE:-}" = password-first ]; then
    printf '%s' pw-secret > "$d/server-password"
    echo "server password pw-secret"; sleep 1 9>&-
    echo "server listening on http://127.0.0.1:4242"
    serving
  fi
  pw=${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}} minted=
  if [ -z "$pw" ] && [ "${FAKE_SERVE:-}" != listen-only ]; then pw=pw-secret minted=1; fi
  printf '%s' "$pw" > "$d/server-password"
  echo "server listening on http://127.0.0.1:4242"
  [ -z "$minted" ] || echo "server password $pw"
  serving ;;
api)
  if [ "$2" = --standalone ]; then shift 2; else authed; shift 3; hangs "$1"; fi
  case "$1" in
  credential.list)
    [ -e "$d/creds" ] || for id in ${FAKE_CREDS-}; do echo "$id"; done > "$d/creds"
    printf '{"data":[' ; sep=
    while IFS= read -r id; do printf '%s{"id":"%s","integrationID":"anthropic","label":"key","active":true,"value":{"type":"key","key":"k"}}' "$sep" "$id"; sep=,; done 2> /dev/null < "$d/creds"
    echo ']}' ;;
  credential.remove)
    id=${3#credentialID=}; echo "$id" >> "$d/removed"
    [ "$id" = "${FAKE_STUCK_CRED-}" ] || { grep -vxF "$id" "$d/creds" > "$d/creds.next"; mv "$d/creds.next" "$d/creds"; } ;;
  config.get)
    docs=${FAKE_DOCUMENTS-}
    [ -n "$docs" ] || docs='[{"type":"document","info":{"mcp":{"servers":{"crewlet":{"type":"remote"},"crewlet-bridge":{"type":"remote"}}}}}]'
    printf '%s\n' "$docs" ;;
  mcp.list)
    n=$(cat "$d/polls" 2> /dev/null || echo 0); n=$((n+1)); echo "$n" > "$d/polls"
    if [ -n "${FAKE_MCP_LIST:-}" ]; then printf '%s\n' "$FAKE_MCP_LIST"; exit 0; fi
    case "$n" in
    1) echo '{"data":[]}' ;;
    2) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"pending"}}]}' ;;
    *) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"connected"}}]}' ;;
    esac ;;
  session.get)
    held "${3#sessionID=}" || { echo "Error: Session not found: ${3#sessionID=}" >&2; exit 1; }
    echo '{"data":{"id":"'"${3#sessionID=}"'","cost":0.5,"tokens":{"input":10,"output":2,"reasoning":3,"cache":{"read":40,"write":1}}}}' ;;
  session.inbox.list)
    inbox=${FAKE_INBOX-}
    [ -n "$inbox" ] || inbox='{"data":[]}'
    printf '%s\n' "$inbox" ;;
  session.inbox.cancel)
    if up; then served=serving; else served=stopped; fi
    echo "${3#sessionID=} ${5#inboxID=} $served" >> "$d/cancelled" ;;
  session.interrupt)
    if up; then served=serving; else served=stopped; fi
    echo "${3#sessionID=} $served" >> "$d/interrupted"
    echo '{"interrupted":true}' ;;
  session.message.list)
    shift; echo "$*" >> "$d/listed"
    if [ -n "${FAKE_LIST_UNTIL:-}" ]; then : > "$d/listing"; while [ ! -e "$d/$FAKE_LIST_UNTIL" ]; do sleep 0.05; done; fi
    cur=; for a; do case "$a" in cursor=*) cur=${a#cursor=};; esac; done
    [ ! -e "$d/pages/${cur:-first}" ] || cat "$d/pages/${cur:-first}" ;;
  *) echo "Error: Operation not found: $1" >&2; exit 1 ;;
  esac ;;
session)
  case "$2" in
  export)
    if [ "$3" = --standalone ]; then id=$4; else [ "$3" != --server ] || { authed; hangs session.export; }; id=$5; fi
    if up; then echo "$id serving" >> "$d/exported"; else echo "$id stopped" >> "$d/exported"; fi
    held "$id" || { echo "Error: Session not found: $id" >&2; exit 1; }
    if [ -n "${FAKE_SESSION_EXPORT_SLEEP:-}" ]; then : > "$d/packing"; sleep "$FAKE_SESSION_EXPORT_SLEEP"; fi
    if [ -e "$d/export-fails" ] || [ -n "${FAKE_EXPORT_FAILS:-}" ]; then printf '{"info":{"id":"%s"' "$id"; echo "Error: the export broke off" >&2; exit 1; fi
    if [ -n "${FAKE_SESSION_EXPORT_BYTES:-}" ]; then dd if=/dev/null of=/dev/stdout bs=1 seek="$FAKE_SESSION_EXPORT_BYTES" 2> /dev/null; exit 0; fi
    if [ -e "$d/export-doc" ]; then cat "$d/export-doc"; exit 0; fi
    if [ -n "${FAKE_SESSION_EXPORT:-}" ]; then printf '%s\n' "$FAKE_SESSION_EXPORT"; else
      msgs=$(cat "$d/export-messages" 2> /dev/null) || msgs='[{"id":"msg_1","type":"user","text":"fix the flake"}]'
      printf '{"info":{"id":"%s","title":"crewlet"},"messages":%s}\n' "$id" "$msgs"; fi ;;
  import)
    shift 2; dir=
    while [ $# -gt 1 ]; do case "$1" in --directory) dir=$2; shift 2;; *) shift;; esac; done
    printf '%s' "$dir" > "$d/import-directory"; cp "$1" "$d/imported-session"
    sid=$(sed -n 's/^{"info":{"id":"\([^"]*\)".*/\1/p' "$1")
    case "${FAKE_IMPORT:-}" in
    409) echo "Session already exists" >&2 ;;
    empty) ;;
    *) hold "$sid"; echo "Imported session: $sid" ;;
    esac ;;
  delete)
    shift 2
    while [ $# -gt 1 ]; do shift; done
    [ "${FAKE_DELETE:-}" != fail ] || { echo "Error: database is locked" >&2; exit 1; }
    held "$1" || { echo "Error: Session not found: $1" >&2; exit 1; }
    rm -f "$d/sessions/$1"; echo "Session $1 deleted" ;;
  esac ;;
run)
  authed
  sid= prev=; for a; do [ "$prev" != --session ] || sid=$a; prev=$a; done; hold "$sid"
  cat > "$d/brief"
  cp "$d/creds" "$d/run-creds" 2> /dev/null
  printf '%s' "${OPENCODE_PASSWORD:-${OPENCODE_SERVER_PASSWORD:-}}" > "$d/run-password"
  if [ -n "${FAKE_REFRESH+set}" ]; then printf '%s' "$FAKE_REFRESH" > "$d/db"; fi
  [ -z "${FAKE_RUN_TERM_EXIT:-}" ] || trap 'exit "$FAKE_RUN_TERM_EXIT"' TERM
  : > "$d/refreshed"
  if [ -n "${FAKE_RUN_MESSAGE:-}" ]; then
    echo '{"type":"text","part":{"messageID":"'"$FAKE_RUN_MESSAGE"'","text":"done"}}'
  else echo '{"type":"text","part":{"text":"done"}}'; fi
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
	// times, where set, replaces Command's waits ([executor.OpenCodeCommandTimed]).
	times *executor.OpenCodeTimes
	// shell is the sh the script runs under; zero is the sh on PATH, as Command names.
	shell boxShell
	// clocked runs the script's waits on the case's clock ([caseClock]), which
	// stands at [clockStart] until the case moves it ([scriptJob.listenRunsOut]).
	clocked bool
	// unconfigured leaves no [executor.Paths.OpenCodeConfig]; every other case
	// finds [scriptConfig] there, as WriteConfig would have left it.
	unconfigured bool
	// session, where set, is the session the run works in, rather than the
	// one [executor.OpenCode.Session] names for req.
	session *executor.Session
	// held is every session the box's database holds as the case starts.
	held []string
	// noDatabase leaves the held sessions' database file out.
	noDatabase bool
	// carried, where set, is a session export carried into the box, gzipped at
	// [executor.Paths.SessionCarried] as a run's start writes it.
	carried string
	// ownsCarried keeps the carried export's digest among the box's own
	// ([executor.Paths.SessionDigests]), as a box that packed or restored it does.
	ownsCarried bool
	// members, past one, gzips the carried export as that many members, as a
	// session kept with its payloads apart is spliced back ([inMembers]).
	members int
	// brokenDigests, where set, is the shell command ([brokenDigestLists]) that
	// makes the box's digest list one that keeps no line, before the script starts.
	brokenDigests string
	// freshBrief, where set, is the brief a session of the run's own is handed,
	// at [executor.Paths.FreshBrief] ([sandbox.RunRequest.FreshBrief]).
	freshBrief string
	// values and freshValues, where set, are the run's first configuration value
	// under the continued session's bridge token and under a fresh session's, at
	// [executor.Paths.OpenCodeValues] and [executor.Paths.OpenCodeValuesFresh]
	// ([sandbox.RunRequest.FreshWiring]); the fake's server records which it
	// started on, and whether the other was still in the box.
	values, freshValues string
	// pages is the fake's `session.message.list` answer by cursor, "first" for none.
	pages map[string]string
	// standIns is a stand-in of the case's own by its name, beside the fake on
	// the script's PATH ([relayNode]'s `node`, say).
	standIns map[string]string
	// inWorkDir is a file the run's start would have written, by its name
	// under [executor.Paths.WorkDir] (a relay's wirings, say).
	inWorkDir map[string]string
}

// scriptConfig is the run configuration a case's script finds, as
// [executor.OpenCode.WriteConfig] would have left it, holding text a shell
// would expand, to catch any read of it as other than a value.
const scriptConfig = "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n" +
	"  \"instructions\": [\"/box/$HOME/$(echo expanded)/`id`.md\"]\n}"

// scriptJob is the script running, in a process group of its own as the
// wrapper's job is, so a case can signal the whole of it as a teardown does.
type scriptJob struct {
	t              *testing.T
	dir, home      string
	paths          executor.Paths
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	exited         chan error
	// alive is the read end of the pipe the fake's server holds for as long
	// as it runs ([serverPipe]); nil for pieces, which start no server.
	alive *os.File
}

// scriptRun is what one execution of the script left.
type scriptRun struct {
	dir, home       string
	stdout, stderr  string
	code            int
	calls           []string
	document, brief string
	// alive is its job's [scriptJob.alive].
	alive *os.File
}

// signalsDeliverable gives every script this file starts the default action
// for SIGHUP and SIGINT, however the test binary was started: a signal ignored
// at start stays ignored across exec, and a non-interactive sh may not trap one
// (POSIX; dash, bash and busybox ash comply), so under nohup or as a background
// job the cases about that signal failed on how the runner was started.
// Go resets every signal it HANDLES to the default in what it starts, so asking
// os/signal for the two gives them back; only where ignored, since a handler on
// one that is not would stop an interrupt ending the test binary.
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
	for name, script := range o.standIns {
		writeStandIn(t, filepath.Join(bin, name), script)
	}
	alive := serverPipe(t, dir)
	paths := executor.Paths{Home: home}
	for name, body := range o.inWorkDir {
		if err := os.WriteFile(filepath.Join(paths.WorkDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	session := executor.OpenCode{}.Session(o.req)
	if o.session != nil {
		session = *o.session
	}
	for _, id := range o.held {
		if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sessions", id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// THE DATABASE THE HELD SESSIONS ARE IN, where OpenCode keeps it: a
	// re-import bounds its memory by the file's size ([openCodeRenew]).
	if len(o.held) > 0 && !o.noDatabase {
		db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
		if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(db, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if o.carried != "" {
		packed := gzipped(t, o.carried)
		if o.members > 1 {
			packed = inMembers(t, packed, o.members)
		}
		if err := os.WriteFile(paths.SessionCarried(), packed, 0o600); err != nil {
			t.Fatal(err)
		}
		if o.ownsCarried {
			keepDigest(t, paths, session.ID, packed)
		}
	}
	if o.brokenDigests != "" {
		breakDigestList(t, paths.SessionDigests(), o.brokenDigests)
	}
	if o.freshBrief != "" {
		if err := os.WriteFile(paths.FreshBrief(), []byte(o.freshBrief), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for cursor, page := range o.pages {
		if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pages", cursor), []byte(page), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var env []string
	for dir, value := range map[string]string{paths.OpenCodeValues(): o.values,
		paths.OpenCodeValuesFresh(): o.freshValues} {
		if value == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		env = []string{"FAKE_VALUES=" + paths.OpenCodeValues()}
	}
	command := executor.OpenCode{}.Command(o.req, session, paths, "")
	if o.times != nil || o.clocked {
		var times executor.OpenCodeTimes
		if o.times != nil {
			times = *o.times
		}
		if o.clocked {
			writeClock(t, dir, clockStart)
			times.Clock = caseClock
		}
		command = executor.OpenCodeCommandTimed(o.req, session, paths, times)
	}
	script := o.shell.prelude + scriptOf(t, command)
	argv := o.shell.argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}

	j := &scriptJob{t: t, dir: dir, home: home, paths: paths, exited: make(chan error, 1), alive: alive}
	args := append(append([]string{}, argv[1:]...), "-c", script)
	j.cmd = exec.Command(argv[0], args...) //nolint:gosec // the script under test
	j.cmd.Dir = dir
	j.cmd.Env = append(append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + home, "FAKE_DIR=" + dir,
	}, env...), o.env...)
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

// serverPipe makes the pipe the fake's server holds open for writing while it
// runs (`serve.alive`, [fakeOpenCode]) and holds its read end for the case
// ([assertServerStopped]). It is opened before the script starts, O_NONBLOCK:
// an open for writing waits for a reader, so the server would hang on a pipe
// nobody held, and a plain read open waits for a server that may never start.
func serverPipe(t *testing.T, dir string) *os.File {
	t.Helper()
	path := filepath.Join(dir, "serve.alive")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("making the server's pipe: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // under the test's own dir
	if err != nil {
		t.Fatalf("opening the server's pipe: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// caseClock is the clock a clocked case's script reads: the time the case last
// set in `clock`, each reading recorded in `clock-reads`. Read with cat, never a
// `date` stand-in, which a busybox that prefers its own applets would pass by.
const caseClock = `t=$(cat "$FAKE_DIR/clock"); echo "$t" >> "$FAKE_DIR/clock-reads"; echo "$t"`

// clockStart is what a clocked case's clock reads until the case moves it.
const clockStart = 1000

func writeClock(t *testing.T, dir string, now int) {
	t.Helper()
	next := filepath.Join(dir, "clock.next")
	if err := os.WriteFile(next, []byte(strconv.Itoa(now)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Moved into place whole, so no reading sees half a write.
	if err := os.Rename(next, filepath.Join(dir, "clock")); err != nil {
		t.Fatal(err)
	}
}

// listenRunsOut runs a clocked script's listen wait out once the script has
// read its deadline and its server has printed all it ever will, so the script
// reports the server's last word, never how far a loaded runner got the fake.
func (j *scriptJob) listenRunsOut() {
	j.t.Helper()
	j.await("the script to read its listen deadline", func() bool {
		return readIn(j.t, j.dir, "clock-reads") != ""
	})
	j.await("the server to print all it ever will", func() bool {
		_, err := os.Stat(filepath.Join(j.dir, "serve-settled"))
		return err == nil
	})
	writeClock(j.t, j.dir, clockStart+3600)
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
	j := &scriptJob{t: t, dir: dir, home: dir, paths: executor.Paths{Home: dir}, exited: make(chan error, 1)}
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

// signalScript sends sig to the script ALONE, by the pid it published as its
// end protocol's ([executor.Paths.JobScript]), as the engine's end request
// reaches it: the case fails where the script published none, or a pid that is
// not its own.
func (j *scriptJob) signalScript(sig syscall.Signal) {
	j.t.Helper()
	raw, err := os.ReadFile(j.paths.JobScript())
	if err != nil {
		j.t.Fatalf("the script published no end protocol: %v", err)
	}
	if want := "1 " + strconv.Itoa(j.cmd.Process.Pid) + "\n"; string(raw) != want {
		j.t.Fatalf("the script published %q, want %q: its own pid under protocol 1", raw, want)
	}
	if err := syscall.Kill(j.cmd.Process.Pid, sig); err != nil {
		j.t.Fatalf("signalling the script with %v: %v", sig, err)
	}
}

// signal sends sig to the script's whole process group, as a teardown does.
func (j *scriptJob) signal(sig syscall.Signal) {
	j.t.Helper()
	if err := syscall.Kill(-j.cmd.Process.Pid, sig); err != nil {
		j.t.Fatalf("signalling the script's group with %v: %v", sig, err)
	}
}

// await polls until cond holds, failing after ten seconds (the script's steps
// take milliseconds against the fake), or AT ONCE when the script exits
// without it: nothing an exited script left will change, and waiting out the
// bound would report a script stuck at its first line as merely slow.
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
	run := scriptRun{dir: j.dir, home: j.home, stdout: j.stdout.String(), stderr: j.stderr.String(), alive: j.alive}
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

// assertGroupGone fails the case if anything of the script's process group
// still runs five seconds after it exited: the script stops and waits for all
// it started, so this waits out only the kernel's bookkeeping. A ZOMBIE IS NOT
// A LEFTOVER: one whose parent the group's signal also killed may never be
// reaped where the first process reaps nothing, and it runs nothing.
func (j *scriptJob) assertGroupGone() {
	j.t.Helper()
	pgid := j.cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		live, readable := liveInGroup(j.t, pgid)
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

// liveInGroup lists the processes of group pgid that can still run, each read
// as internal/procgroup reads one (a zombie runs nothing, but one whose main
// thread alone has exited runs on), over a listing of /proc: false where there
// is none to list, as on darwin. A record the kernel will not give fails the
// case, since a group must not read as gone on "unknown".
func liveInGroup(t *testing.T, pgid int) ([]string, bool) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var live []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		proc, found, err := procgroup.Inspect(pid)
		if err != nil {
			t.Fatalf("reading the kernel's record of pid %d: %v", pid, err)
		}
		if !found || proc.Group != pgid || proc.Zombie {
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
// machine has, once per binary: the sh on PATH, which the runner names; dash
// (Debian, Ubuntu); bash --posix (Fedora, RHEL, Arch); and busybox ash (Alpine,
// which OpenCode 2's musl builds are for). AND ALWAYS the sh on PATH with
// [busyboxKill], so a machine without busybox, as most CI runners are, still
// runs the cases busybox's kill tripped the script on.
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

// loginFree reports whether name, in a box's work directory, is a file a run's
// script leaves there that holds no login: its configuration ([scriptConfig]),
// its session record, export and export digests, its end protocol's pid and
// end records, and its checkouts' directory ([Paths.OpenCodeCheckouts]).
func loginFree(p executor.Paths, name string) bool {
	for _, path := range []string{p.OpenCodeConfig(), p.SessionRecord(), p.SessionCapture(), p.SessionDigests(),
		p.JobScript(), p.JobEnded(), p.OpenCodeCheckouts()} {
		if name == filepath.Base(path) {
			return true
		}
	}
	return false
}

var bridgeOnly = map[string]sandbox.MCPServer{
	"crewlet-bridge": {Name: "crewlet-bridge", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp/x"},
}

const (
	seededLogin    = `[{"id":"cred_1","key":"old"}]`
	refreshedLogin = `[{"id":"cred_1","key":"new"}]`
)

// promptEnd bounds a run's end against the fake at [stretchedStop]: an end
// takes about a second, one that waits out a stop of nothing the whole minute,
// so the bound sits far from both, past a loaded runner's reach.
const promptEnd = 15 * time.Second

// stretchedStop is the stop wait a case about an end not waiting runs the
// script at: long enough that waiting it out once is unmistakable.
const stretchedStop = time.Minute

// THE BRIEF IS SENT ONLY ONCE THE SEAT'S TOOLS EXIST: OpenCode 2 fixes a step's
// tools as it starts, so a brief sent before the MCP servers connect gets a
// first step with none (as `--standalone` would). The script waits until every
// declared server has left `pending`, through an empty answer and a pending
// one, then runs the brief verbatim on stdin against its own server.
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

// ONLY THE SERVER IS HANDED THE CONFIGURATION: WriteConfig's file, as
// OPENCODE_CONFIG_CONTENT, verbatim with `$` and backquotes unexpanded. The
// login's import and export, the run's client and its `api` calls get none.
//
// Mutation: export the variable for the whole script, and every command has it.
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

// ONLY THE SERVER IS HANDED THE RUN'S SESSION, wired to notes or not: the
// notes plugin admits notes to the session CREWLET_RUN_SESSION names, the one
// the client is pointed at with --session, and the checkouts plugin hands
// checkouts in it alone.
//
// Mutation: export the variable for the whole script, or hand it the server
// only for a run wired to notes.
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
			for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "session-handed")), "\n") {
				handed, call, _ := strings.Cut(line, "|")
				if !strings.HasPrefix(call, "serve ") {
					if handed != "" {
						t.Errorf("wired %v: %q was handed the run's session %q, want the server alone", wired, call, handed)
					}
					continue
				}
				served = true
				if handed != session {
					t.Errorf("wired %v: the server was handed the session %q, want the one the run names, %q",
						wired, handed, session)
				}
			}
			if !served {
				t.Errorf("wired %v: the server never started", wired)
			}
		}
	})
}

// inboxItem is one pending item of a session's inbox, as a case lists it.
type inboxItem struct{ id, text, delivery string }

// inboxOf is `session.inbox.list`'s answer holding items, each OpenCode 2.3.2's
// own item for a steer the notes plugin admitted (testdata/opencode/2.3.2/
// inbox-list.json) with its id, text and delivery replaced.
func inboxOf(t *testing.T, items ...inboxItem) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode", "2.3.2", "inbox-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil || len(captured.Data) != 1 {
		t.Fatalf("the captured inbox is %s, want one item: %v", raw, err)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		item := maps.Clone(captured.Data[0])
		payload, _ := item["payload"].(map[string]any)
		payload = maps.Clone(payload)
		payload["text"] = it.text
		item["id"], item["payload"], item["delivery"] = it.id, payload, it.delivery
		out = append(out, item)
	}
	answer, err := json.Marshal(map[string]any{"data": out})
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}

// NOTHING THE NOTES PLUGIN ADMITTED IS LEFT PENDING IN THE SESSION. A note
// admitted as the session settles waits on the run's record, and would also
// stay in the session's inbox, which a run carried on in the session hands its
// model beside the continuation brief's copy. So after the run and before its
// server stops, the script cancels every inbox item of the plugin's shape and
// nothing else (an id OpenCode minted, an id inside a note's text). A run wired
// to no notes asks nothing. The listing is OpenCode 2.3.2's own ([inboxOf]).
//
// Mutation: drop the cancel, run it after the server stops, or match any
// `msg_` id, and this goes red.
func TestARunCancelsTheNotesItsSessionNeverDelivered(t *testing.T) {
	t.Parallel()
	const (
		ours    = "msg_30292d427a87e479be9f88cba42963ca"
		oursToo = "msg_05536338060eb18a87638c49f196ffcf"
		theirs  = "msg_01JB8V9Q4XG2W7M3R5T6Y8Z0AB"
		inText  = "msg_00000000000000000000000000000000"
	)
	inbox := inboxOf(t, inboxItem{ours, `use "id":"` + inText + `" here`, "steer"},
		inboxItem{oursToo, "second", "steer"}, inboxItem{theirs, "queued", "queue"})
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

// A KILLED JOB'S PENDING NOTES NEVER REACH THE NEXT RUN IN ITS BOX'S COPY. A
// run killed mid-way never cancels what its notes plugin admitted, and the
// box's copy keeps those steers pending, which OpenCode would hand a run
// continuing that copy ahead of its brief while the brief retells them. So a
// run continuing its box's own copy cancels every steer of the plugin's shape
// before its server starts, through a private server, whether or not it is
// wired to notes; a run starting a new session asks nothing. The listing is
// OpenCode 2.3.2's own ([inboxOf]).
//
// Mutation: drop the gate's cancel, run it after the server starts, or match
// any `msg_` id, and this goes red.
func TestAKilledJobsPendingNotesNeverReachTheNextRunInItsCopy(t *testing.T) {
	t.Parallel()
	const (
		leftover = "msg_8a2c0f4e9b1d7a6c3e5f0b2d4a6c8e0f"
		minted   = "msg_01JB8V9Q4XG2W7M3R5T6Y8Z0AB"
	)
	inbox := inboxOf(t, inboxItem{leftover, "Target the release branch.", "steer"},
		inboxItem{minted, "queued", "queue"})
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: []string{carriedOpenCode},
			req: sandbox.RunRequest{Resume: carriedOpenCode}, env: []string{"FAKE_INBOX=" + inbox}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		listed := run.callIndex("api --standalone session.inbox.list")
		if listed < 0 || listed > run.callIndex("serve ") ||
			!strings.Contains(run.calls[listed], "sessionID="+carriedOpenCode) {
			t.Fatalf("the box's copy's inbox was not listed before the server started:\n%s",
				strings.Join(run.calls, "\n"))
		}
		cancelled := strings.Fields(strings.TrimSpace(readIn(t, run.dir, "cancelled")))
		if len(cancelled) != 3 || cancelled[0] != carriedOpenCode || cancelled[1] != leftover ||
			cancelled[2] != "stopped" {
			t.Errorf("cancelled %v, want the killed job's note %s alone, before any server", cancelled, leftover)
		}

		fresh := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_INBOX=" + inbox}})
		if fresh.code != 0 || slices.ContainsFunc(fresh.calls, func(c string) bool {
			return strings.Contains(c, "session.inbox")
		}) {
			t.Errorf("a run starting a new session asked about an inbox (exit %d):\n%s", fresh.code,
				strings.Join(fresh.calls, "\n"))
		}
	})
}

// A RUN WITH NO CONFIGURATION STARTS NOTHING: its server would run on whatever
// the image and checkout hold, without the run's posture or servers, so it is
// refused before any login import or server start, naming the missing file.
//
// Mutation: drop the check, and the server starts on an empty configuration.
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

// A SERVER NAMED THE WAY OPENCODE NAMES IT: readiness matches the `mcp.list`
// answer as text, so a name is spelled as JSON.stringify spells it; spelled by
// encoding/json, which escapes `&`, `<`, `>`, U+2028 and U+2029, such a server
// never matches and the run waits out the bound. The answer is OpenCode 2's
// own from `opencode api mcp.list`, only its scratch directory replaced.
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
			times: &executor.OpenCodeTimes{MCP: 3 * time.Second, Sync: time.Hour},
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

// A SERVER THAT WOULD SHADOW THE ENGINE'S TOOLS ENDS THE RUN BEFORE IT STARTS.
// OpenCode names a tool `<server>_<tool>`, rewriting all but letters, digits,
// `_` and `-` to `_`, and keeps one tool per name, so an image's or a
// repository's `crewlet_x` or `crewlet.x` with tool `y` takes the engine's
// `x_y`. The readiness loop refuses such a run, naming the server, before
// `opencode run`, wherever it is listed; `crewlet-x` and `crewletx` start.
// Names are held only to the prefixes of servers the run offers directly,
// never one behind Code Mode. Under every sh.
//
// Mutation: leave the check out of the loop, take `-` for a character
// OpenCode rewrites, or hold every server's name rather than those offered
// directly, and a row goes red.
func TestOpenCodeServersThatShadowTheEnginesToolsAreRefused(t *testing.T) {
	t.Parallel()
	engine := sandbox.MCPServer{Name: "crewlet", Command: "false", AlwaysLoaded: true}
	seat := sandbox.MCPServer{Name: "github", Command: "false"}
	listed := func(names ...string) string {
		entries := make([]string, len(names))
		for i, name := range names {
			entries[i] = `{"name":"` + name + `","status":{"status":"connected"}}`
		}
		return `{"location":{"directory":"/home/crewlet/workspace"},"data":[` + strings.Join(entries, ",") + `]}`
	}
	for _, tc := range []struct {
		name    string
		servers map[string]sandbox.MCPServer
		list    string
		refused string
	}{
		{"an underscore after the engine's name", map[string]sandbox.MCPServer{"crewlet": engine},
			listed("crewlet", "crewlet_x"), "crewlet_x"},
		{"listed before the engine's own", map[string]sandbox.MCPServer{"crewlet": engine},
			listed("crewlet_docs", "crewlet"), "crewlet_docs"},
		{"a character OpenCode writes as an underscore", map[string]sandbox.MCPServer{"crewlet": engine},
			listed("crewlet", "crewlet.x"), "crewlet.x"},
		{"a hyphen, which OpenCode keeps", map[string]sandbox.MCPServer{"crewlet": engine},
			listed("crewlet", "crewlet-x"), ""},
		{"no separator at all", map[string]sandbox.MCPServer{"crewlet": engine},
			listed("crewlet", "crewletx"), ""},
		{"beside a server behind Code Mode", map[string]sandbox.MCPServer{"crewlet": engine, "github": seat},
			listed("crewlet", "github", "github_x"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forEachShell(t, func(t *testing.T, sh boxShell) {
				run := runScript(t, scriptOpts{
					shell: sh,
					req:   sandbox.RunRequest{MCPServers: tc.servers},
					env:   []string{"FAKE_MCP_LIST=" + tc.list},
					times: &executor.OpenCodeTimes{MCP: 3 * time.Second, Sync: time.Hour},
				})
				if tc.refused == "" {
					if run.code != 0 || strings.Contains(run.stderr, "declares an MCP server") {
						t.Fatalf("a server that keeps a name of its own refused the run (exit %d):\n%s",
							run.code, run.stderr)
					}
					if run.callIndex("run ") < 0 {
						t.Errorf("the run never started:\n%s", strings.Join(run.calls, "\n"))
					}
					return
				}
				if run.code == 0 {
					t.Fatalf("the run started beside %s, whose tools take the engine's names:\n%s",
						tc.refused, strings.Join(run.calls, "\n"))
				}
				if want := "declares an MCP server named " + tc.refused + ","; !strings.Contains(run.stderr, want) {
					t.Errorf("the refusal does not name the server (%q):\n%s", want, run.stderr)
				}
				if run.callIndex("run ") >= 0 {
					t.Errorf("opencode run started before the refusal:\n%s", strings.Join(run.calls, "\n"))
				}
			})
		})
	}
}

// AN OPENCODE RUN WHOSE BRIDGE /etc/opencode REPLACES IS REFUSED BEFORE ITS
// BRIEF: config.get lists the run's own document and /etc's, each declaring
// `crewlet` (measured on 2.3.2, where /etc's took the bridge's place with
// mcp.list as ever), so the script ends before `opencode run`, under every sh.
//
// Mutation: leave the check over config.get out of the script.
func TestAnOpenCodeRunWhoseBridgeEtcReplacesIsRefusedBeforeItsBrief(t *testing.T) {
	t.Parallel()
	engine := sandbox.MCPServer{Name: "crewlet", Command: "false", AlwaysLoaded: true}
	docs := `[{"type":"document","info":{"mcp":{"servers":{"crewlet":{"type":"remote"}}}}},` +
		`{"type":"document","path":"/etc/opencode/opencode.json","info":{"mcp":{"servers":{"crewlet":{"type":"local"}}}}}]`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, req: sandbox.RunRequest{MCPServers: map[string]sandbox.MCPServer{"crewlet": engine}},
			env:   []string{`FAKE_MCP_LIST={"data":[{"name":"crewlet","status":{"status":"connected"}}]}`, "FAKE_DOCUMENTS=" + docs},
			times: &executor.OpenCodeTimes{MCP: 3 * time.Second, Sync: time.Hour}})
		if run.code == 0 || run.callIndex("run ") >= 0 {
			t.Fatalf("the run started (exit %d):\n%s", run.code, strings.Join(run.calls, "\n"))
		}
		if !strings.Contains(run.stderr, "declare the MCP server 'crewlet' 2 times") {
			t.Errorf("the refusal does not say the bridge is declared twice:\n%s", run.stderr)
		}
	})
}

// JSON.stringify's spelling, to the byte: `"`, `\` and the control characters
// escaped (five by their short forms, the rest as lowercase \u hex) and the
// rest literal, DEL and U+2029 included. A byte that is not UTF-8 is U+FFFD
// written as itself: the name OpenCode reads from encoding/json's config.
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
		if got := executor.JSONStringify(in); got != want {
			t.Errorf("JSONStringify(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE SERVER IS THE RUN'S AND ENDS WITH IT: the done marker is written only
// once the script exits, so a server left running holds nothing open, and one
// never stopped would keep the box's bridge connection and credentials alive.
func TestTheRunsServerIsStoppedWithIt(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh})
		assertServerStopped(t, run)
	})
}

// assertServerStopped fails the case if the run's server outlived the run: if
// its pipe ([serverPipe]) still has a writer. The kernel closes an exiting
// process's descriptors, so none left is a server gone, reaped or not, where
// kill(pid, 0) succeeds until it is reaped (#1164). ONE READ, WHICH NEVER
// WAITS: end of file is no writer, EAGAIN a writer still there; a read waiting
// for end of file would pass a server that outlived the run once the fake's
// own sixty seconds end it ([fakeOpenCode]).
func assertServerStopped(t *testing.T, run scriptRun) {
	t.Helper()
	raw := readIn(t, run.dir, "serve.pid")
	if raw == "" {
		t.Fatal("no server was started")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := run.alive.SyscallConn()
	if err != nil {
		t.Fatalf("reading the server's pipe: %v", err)
	}
	var n int
	var readErr error
	if err := conn.Read(func(fd uintptr) bool {
		n, readErr = syscall.Read(int(fd), make([]byte, 1))
		return true // answered as it stands: never wait for the pipe to change
	}); err != nil {
		t.Fatalf("reading the server's pipe: %v", err)
	}
	switch {
	case errors.Is(readErr, syscall.EAGAIN):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the server (pid %d) outlived the run: it still holds its pipe", pid)
	case readErr != nil:
		t.Fatalf("reading the server's pipe: %v", readErr)
	case n != 0:
		t.Fatalf("the server's pipe held %d byte, which nothing writes", n)
	}
}

// A RUN THAT STARTED NOTHING TO STOP ENDS AT ONCE, under every sh. A stop is
// a bounded wait for a process to go, and one never started (a run given no
// login's exporter; both, at an exit before the server listens) must not be
// waited for, though busybox ash's `kill ""` and `kill -0 ""` succeed. Each
// case stretches the stop wait to a minute, so one waited out cannot pass.
//
// Mutation: drop end's test for an empty pid, and the busybox-kill shell waits
// out the minute in both cases.
func TestARunThatStartedNothingToStopEndsAtOnce(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		t.Run("no-login", func(t *testing.T) {
			t.Parallel()
			run := startScript(t, scriptOpts{shell: sh, times: &executor.OpenCodeTimes{Stop: stretchedStop}}).
				waitWithin(promptEnd, "a run given no login waited to stop an exporter it never started")
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
		})
		t.Run("server-never-listens", func(t *testing.T) {
			t.Parallel()
			j := startScript(t, scriptOpts{
				shell: sh, env: []string{"FAKE_SERVE=silent"}, clocked: true,
				times: &executor.OpenCodeTimes{Listen: time.Second, Stop: stretchedStop},
			})
			j.listenRunsOut()
			run := j.waitWithin(promptEnd, "an early exit waited to stop an exporter it never started")
			if run.code != 1 {
				t.Fatalf("exit %d, want the refusal's 1:\n%s", run.code, run.stderr)
			}
			assertServerStopped(t, run)
		})
	})
}

// THE SERVER'S PASSWORD IS ITS OWN. `opencode serve` takes one from
// OPENCODE_PASSWORD or OPENCODE_SERVER_PASSWORD and then prints none, so a box
// environment carrying either (role.sandbox.env, an OpenCode image) would leave
// the script waiting for one. The server starts with neither, and the run's
// clients present the one it minted.
//
// Mutation: drop the unset, and the run fails with no password printed.
func TestARunsServerNeverTakesAnOutsidePassword(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{
			shell: sh,
			env:   []string{"OPENCODE_SERVER_PASSWORD=operator-chosen", "OPENCODE_PASSWORD=operator-other"},
			times: &executor.OpenCodeTimes{Listen: 5 * time.Second, Sync: time.Hour},
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
				j := startScript(t, scriptOpts{
					shell: sh, env: []string{"FAKE_SERVE=" + serve}, clocked: true,
					times: &executor.OpenCodeTimes{Listen: time.Second, Sync: time.Hour},
				})
				j.listenRunsOut()
				run := j.wait()
				if run.code == 0 {
					t.Fatal("the run went on without what it needs from its server")
				}
				if !strings.Contains(run.stderr, want) {
					t.Errorf("stderr = %q, want %q", run.stderr, want)
				}
				if run.callIndex("run ") >= 0 {
					t.Errorf("the brief was run anyway:\n%s", strings.Join(run.calls, "\n"))
				}
				assertServerStopped(t, run)
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
// asked once the run has ended, as a line of its own type the decoder reads as
// the run's tokens and price. Not the stream's last line: the message list's
// pages follow it.
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
		res := executor.OpenCode{}.Parse([]byte(run.stdout))
		if res.InputTokens != 51 || res.OutputTokens != 5 || res.CostUSD != 0.5 || res.SessionID != session {
			t.Errorf("result = %+v, want the session's report read: in 51 out 5 cost 0.5 session %s",
				res, session)
		}
	})
}

// A BOX GIVEN A LOGIN IMPORTS IT BEFORE THE RUN AND WRITES THE REFRESHED ONE
// BACK AFTER: the export, over the seeded document the teardown copies back to
// the shared login. Never an export holding no login, and no document where
// the box was given none.
func TestABoxsLoginIsImportedAndTheRefreshedOneWrittenBack(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, login: seededLogin, env: []string{"FAKE_REFRESH=" + refreshedLogin}})
		if imported := readIn(t, run.dir, "imported"); imported != seededLogin {
			t.Errorf("imported %q, want the login the box was given", imported)
		}
		if imp := run.callIndex("auth import --server"); imp < run.callIndex("serve ") || imp > run.callIndex("run ") {
			t.Errorf("the login was not imported through the run's server before its client started:\n%s",
				strings.Join(run.calls, "\n"))
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

// exportsHoldingNoLogin are what an export can print that is no login to keep.
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

// A LOGIN REFRESHED MID-RUN SURVIVES THE RUN BEING KILLED. A teardown of a run
// in flight is a group SIGKILL or `docker rm -f`, after which nothing in the
// box runs, and a refreshed login lives in OpenCode's database until exported;
// so it is exported through the run's server while the run works.
//
// Mutation: export only when the run ends, and a kill leaves the seed.
func TestALoginRefreshedMidRunSurvivesTheRunBeingKilled(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{
			shell: sh,
			login: seededLogin,
			env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_RUN_SLEEP=60"},
			times: &executor.OpenCodeTimes{Sync: 200 * time.Millisecond},
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
					times: &executor.OpenCodeTimes{Sync: 100 * time.Millisecond},
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
// before the last export (else the two race to rename, and it keeps exporting
// against a dead server in the job's group until teardown), and it reaps its
// own sleep first. Both with the exporter asleep and part way through one.
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
					times: &executor.OpenCodeTimes{Sync: sync},
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

// THE EXPORTER SIGNALS NO PID IT HAS ALREADY REAPED. The SIGTERM that stops it
// may land mid-export, an interval after it last reaped its sleep, and its
// trap kills the sleep it waits on; so it forgets a sleep's pid once reaped,
// or the trap could signal a reused pid: in a `direct` box, any process of the
// engine's user. Stand-ins record every kill and sleep, and the exporter is
// stopped mid-export. A shell's tenth-of-a-second polls (end's, a bounded
// call's) are left out: one in the exporter's shell is recorded under that
// shell's pid, which the stop signals by design.
//
// Mutation: keep the sleep's pid after its wait, and the trap kills it.
func TestTheLoginExporterSignalsNoPidItHasReaped(t *testing.T) {
	t.Parallel()
	const recorder = `kill() { echo "kill $*" >> "$FAKE_DIR/pids"; command kill "$@"; }
sleep() { [ "$1" != 0.1 ] || { command sleep "$@"; return; }; __p=$(exec sh -c 'echo $PPID'); echo "start $__p" >> "$FAKE_DIR/pids"; command sleep "$@"; echo "end $__p" >> "$FAKE_DIR/pids"; }
`
	j := startScript(t, scriptOpts{
		shell: boxShell{name: "sh-recording-pids", prelude: recorder},
		login: seededLogin,
		env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_EXPORT_SLEEP=1", "FAKE_RUN_UNTIL=released"},
		times: &executor.OpenCodeTimes{Sync: 10 * time.Millisecond},
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
		// THE PID IS THE LAST WORD, after any signal: the sleep is ended by `kill -9`.
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

// AN EXPORTER STOPPED AS IT STARTS GOES AT ONCE, under every sh, leaving
// nothing. It is forked while the script traps SIGTERM, and a SIGTERM before
// it runs lands on the script's handler, which dash clears as the subshell
// begins: the stop is dropped, so unsync tells it again until it goes. TWENTY
// EXPORTERS, each stopped straight after its fork: a stop is lost only if it
// beats the fork's first run, which shells running side by side make rarer,
// and twenty make a loss all but certain under dash. The stop wait is a
// minute, so a lost stop waited out cannot pass for a prompt one.
//
// Mutation: tell the exporter once (drop unsync's second operand), and the
// dash cases wait out the minute.
func TestAnExporterStoppedAsItStartsGoesAtOnce(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startPieces(t, sh, "trap 'exit 143' TERM\nkeep() { :; }\nunbound() { :; }\n"+
			executor.OpenCodeStops(stretchedStop)+"n=0\nwhile [ \"$n\" -lt 20 ]; do\n"+
			executor.OpenCodeExporter(time.Hour, "sync")+" &\nxpid=$!\nunsync\n"+
			"n=$((n+1))\ndone\n")
		run := j.waitWithin(promptEnd, "a stop sent as the exporter started was lost and waited out")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		j.assertGroupGone()
	})
}

// A STOP AS THE EXPORTER STARTS ITS SLEEP LEAVES NO SLEEP. Between starting a
// sleep and holding its pid the exporter has nothing to kill, so a SIGTERM
// there is kept until it has; acted on at once, the trap would leave the sleep
// in the job's group for its interval. No outside signal can be timed into
// that window, so the exporter here signals itself in it, once.
//
// Mutation: act on the stop at once (drop the trap's test of w), and the
// hour's sleep is still running after the exporter has gone.
func TestAStopAsTheExporterStartsItsSleepLeavesNoSleep(t *testing.T) {
	t.Parallel()
	exporter := executor.OpenCodeExporter(time.Hour, "sync")
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
		j := startPieces(t, sh, "keep() { :; }\nunbound() { :; }\n"+exporter+" &\nwait $!\n")
		run := j.waitWithin(promptEnd, "an exporter that stopped itself did not go")
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		j.assertGroupGone()
	})
}

// A STOPPED EXPORT LEAVES NO COPY OF THE LOGIN. Stopped mid-export, the
// exporter acts on the stop as the export ends, before keep moves it over the
// document, and drops the file it was writing: the login stays only in the
// document, the one place a teardown writes back.
//
// Mutation: leave the exporter's file when it goes, and the box keeps a
// second copy beside the document.
func TestAStoppedExportLeavesNoCopyOfTheLogin(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{
			shell: sh,
			login: seededLogin,
			env:   []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_EXPORT_SLEEP=1", "FAKE_RUN_UNTIL=released"},
			times: &executor.OpenCodeTimes{Sync: 10 * time.Millisecond},
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
		if run.document != refreshedLogin {
			t.Errorf("the box's login is %q, want the refreshed export", run.document)
		}
		// Besides the document, only a run's own artefacts ([loginFree]).
		doc := j.paths.CredentialDocument()
		entries, err := os.ReadDir(filepath.Dir(doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != filepath.Base(doc) && !loginFree(j.paths, e.Name()) {
				t.Errorf("the run left %s beside the box's login", e.Name())
			}
		}
	})
}

// AN EXPORT THE STOP OUTLASTS LEAVES NO COPY EITHER: end kills an exporter
// whose export outlasts the stop's bound, which runs no trap, so the file it
// was writing is unsync's to drop.
//
// Mutation: leave the drop to the exporter's own trap, and the box keeps the
// copy beside the document.
func TestAnExportTheStopOutlastsLeavesNoCopy(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{
			shell: sh,
			login: seededLogin,
			env: []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_EXPORT_SLEEP=5",
				"FAKE_RUN_UNTIL=released"},
			times: &executor.OpenCodeTimes{Sync: 10 * time.Millisecond,
				Stop: 300 * time.Millisecond},
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
		// Besides the document, only a run's own artefacts ([loginFree]).
		doc := j.paths.CredentialDocument()
		entries, err := os.ReadDir(filepath.Dir(doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != filepath.Base(doc) && !loginFree(j.paths, e.Name()) {
				t.Errorf("the run left %s beside the box's login", e.Name())
			}
		}
	})
}

// A SIGNALLED RUN EXPORTS ITS LOGIN BEFORE IT GOES, beside a server going with
// the group, and exits with the conventional status PROMPTLY, leaving nothing.
// A plain sh runs no EXIT trap on a signal it does not trap, so the signals
// are trapped; and under busybox ash, whose `kill ""` succeeds, the EXIT
// trap's stops must not run again over processes already gone. The stop wait
// is a minute, so waiting one out cannot pass unseen.
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
					// A sync that never comes round: only the signal's export writes the document.
					times: &executor.OpenCodeTimes{Sync: time.Hour, Stop: stretchedStop},
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
				assertServerStopped(t, run)
				j.assertGroupGone()
			})
		}
	})
}

// AN OPENCODE THAT IS THERE BUT FAILS IS NOT CALLED MISSING: a failing
// `opencode --version` is reported with its own stderr, the run's transcript
// when nothing else was written, never as "no opencode on the box PATH".
//
// Mutation: answer a failing --version with the PATH sentence, or drop its
// stderr, and the first row goes red; answer absence with the failure's, and
// the second does.
func TestAnOpenCodeThatFailsIsToldFromOneThatIsMissing(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		failing := runScript(t, scriptOpts{shell: sh,
			env: []string{"FAKE_VERSION_FAIL=opencode: libstdc++ not found"}})
		if failing.code == 0 || strings.Contains(failing.stderr, "no opencode on the box PATH") ||
			!strings.Contains(failing.stderr, "opencode --version failed with exit 3") ||
			!strings.Contains(failing.stderr, "libstdc++ not found") {
			t.Errorf("an opencode that failed --version said (exit %d):\n%s", failing.code,
				failing.stderr)
		}
		missing := runScript(t, scriptOpts{shell: sh, env: []string{"PATH=/usr/bin:/bin"}})
		if missing.code != 127 || !strings.Contains(missing.stderr, "no opencode on the box PATH") ||
			strings.Contains(missing.stderr, "--version failed") {
			t.Errorf("a box with no opencode said (exit %d):\n%s", missing.code, missing.stderr)
		}
	})
}

// AN OPENCODE OLDER THAN THE RELEASE SUPPORTED IS REFUSED BY NAME at the run's
// start, before anything is driven, saying the release it runs and the one
// supported; a newer one runs, and so does one whose release does not read.
//
// Mutation: compare the release as text, refuse a newer one, or drop the
// check, and this goes red.
func TestAnOpenCodeOlderThanTheReleaseSupportedIsRefusedByName(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, older := range []string{"opencode v2.3.1", "opencode v2.3.0", "opencode v2.2.99", "opencode v1.99.0",
			"opencode v2.0.0"} {
			run := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_VERSION=" + older}})
			if run.code == 0 || !strings.Contains(run.stderr, "OpenCode "+strings.TrimPrefix(older, "opencode v")) ||
				!strings.Contains(run.stderr, "supports OpenCode 2.3.2, its newest") {
				t.Errorf("a box printing %q ran (exit %d) or was not told the release:\n%s", older, run.code, run.stderr)
			}
			if run.callIndex("serve ") >= 0 || run.callIndex("run ") >= 0 {
				t.Errorf("a box printing %q was driven anyway:\n%s", older, strings.Join(run.calls, "\n"))
			}
		}
		for _, runs := range []string{"opencode v2.3.2", "opencode v2.3.3", "opencode v2.10.0", "opencode v3.0.0",
			"opencode nightly"} {
			run := runScript(t, scriptOpts{shell: sh, env: []string{"FAKE_VERSION=" + runs}})
			if strings.Contains(run.stderr, "supports OpenCode") || run.callIndex("serve ") < 0 {
				t.Errorf("a box printing %q was refused or not driven:\n%s", runs, run.stderr)
			}
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

// AN ENDED OPENCODE RUN EXPORTS ITS SESSION BEFORE ITS SERVER STOPS. The end
// request reaches the script alone, by its published pid: it interrupts the
// session through its server (settling the step in flight, so the export
// holds it), ends the client, records the end, then, while the server still
// serves, cancels the undelivered notes, asks usage, exports the session and
// the login, and only then stops the server. The run exits as its client did.
//
// Mutations: drop the interrupt, and no step is settled before the export;
// stop the server before the export, and the export is recorded beside a
// stopped server.
func TestAnEndedOpenCodeRunExportsItsSessionBeforeItsServerStops(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{
			shell: sh,
			login: seededLogin,
			req:   sandbox.RunRequest{Steer: steerWiring},
			// A CLIENT THAT EXITS 7 however it is ended, interrupt or SIGTERM, so its
			// status is known whichever lands first.
			env: []string{"FAKE_REFRESH=" + refreshedLogin, "FAKE_RUN_UNTIL=interrupted",
				"FAKE_RUN_EXIT=7", "FAKE_RUN_TERM_EXIT=7"},
			// A sync that never comes round: only the end's export writes the document.
			times: &executor.OpenCodeTimes{Sync: time.Hour},
		})
		j.await("the run to start", func() bool {
			_, err := os.Stat(filepath.Join(j.dir, "refreshed"))
			return err == nil
		})
		j.signalScript(syscall.SIGUSR1)
		run := j.waitWithin(promptEnd, "an ended run waited on something its end had already stopped")
		if run.code != 7 {
			t.Errorf("exit %d, want the client's own 7:\n%s", run.code, run.stderr)
		}
		if ended := readIn(t, run.home+"/.crewlet", "job-ended"); ended != "asked\n" {
			t.Errorf("the job's end is recorded as %q, want asked", ended)
		}
		interrupt := run.callIndex("api --server http://127.0.0.1:4242 session.interrupt ")
		inbox := run.callIndex("api --server http://127.0.0.1:4242 session.inbox.list ")
		usage := run.callIndex("api --server http://127.0.0.1:4242 session.get ")
		exported := run.callIndex("session export --server ")
		login := run.callIndex("auth export --server ")
		if interrupt < 0 || inbox < interrupt || usage < inbox || exported < usage || login < exported {
			t.Fatalf("want the session interrupted, its leftover notes listed, its usage asked, it exported "+
				"and the login exported, in that order:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := readIn(t, run.dir, "interrupted"); !strings.HasSuffix(got, " serving\n") {
			t.Errorf("the session was interrupted beside a server recorded %q", got)
		}
		if got := readIn(t, run.dir, "exported"); !strings.HasSuffix(got, " serving\n") {
			t.Errorf("the session was exported beside a server recorded %q, want serving", got)
		}
		if run.document != refreshedLogin {
			t.Errorf("after the end the box's login is %q, want the refreshed one", run.document)
		}
		if _, err := os.Stat(j.paths.SessionCapture()); err != nil {
			t.Errorf("the ended run left no capture: %v\n%s", err, run.stderr)
		}
		assertServerStopped(t, run)
		j.assertGroupGone()
	})
}

// AN OPENCODE RUN ASKED TO END AS ITS SERVER STARTS GOES AT ONCE: a request
// while the script waits for its server's address starts no run, records the
// job ended before its harness started, and the script goes (its EXIT trap
// stopping the server) rather than waiting out the listen bound.
//
// Mutation: drop the request's check in the listen loop, and the script waits
// out the whole minute.
func TestAnOpenCodeRunAskedToEndAsItsServerStartsGoesAtOnce(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		j := startScript(t, scriptOpts{shell: sh, env: []string{"FAKE_SERVE=silent"}})
		j.await("the server to start", func() bool {
			_, err := os.Stat(filepath.Join(j.dir, "serve.pid"))
			return err == nil
		})
		j.signalScript(syscall.SIGUSR1)
		run := j.waitWithin(promptEnd, "a run asked to end waited for a server it would never use")
		if run.code != 143 {
			t.Errorf("exit %d, want 143:\n%s", run.code, run.stderr)
		}
		if ended := readIn(t, run.home+"/.crewlet", "job-ended"); ended != "before\n" {
			t.Errorf("the job's end is recorded as %q, want before", ended)
		}
		if run.callIndex("run ") >= 0 {
			t.Errorf("a run asked to end before its server listened started one:\n%s",
				strings.Join(run.calls, "\n"))
		}
		if !strings.Contains(readIn(t, run.home+"/.crewlet", "session"), "id ses_") {
			t.Error("the run asked to end before its server listened recorded no session")
		}
		assertServerStopped(t, run)
		j.assertGroupGone()
	})
}

// A SECOND END REQUEST LEAVES THE PACK WHOLE: once a run's end is under way
// the script ignores the request, so one that comes while it packs neither
// kills the script nor cuts its archive, and the run's exit is unchanged.
//
// Mutation: put the request back to its default once the end is under way
// rather than ignoring it, and the second request kills the script mid-pack.
func TestASecondEndRequestLeavesThePackWhole(t *testing.T) {
	t.Parallel()
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		forEachShell(t, func(t *testing.T, sh boxShell) {
			j := startScript(t, scriptOpts{shell: sh,
				env: []string{"FAKE_RUN_UNTIL=interrupted", "FAKE_RUN_EXIT=7", "FAKE_RUN_TERM_EXIT=7",
					"FAKE_SESSION_EXPORT_SLEEP=1"}})
			j.await("the run to start", func() bool {
				_, err := os.Stat(filepath.Join(j.dir, "refreshed"))
				return err == nil
			})
			j.signalScript(syscall.SIGUSR1)
			j.await("the pack to begin", func() bool {
				_, err := os.Stat(filepath.Join(j.dir, "packing"))
				return err == nil
			})
			j.signalScript(syscall.SIGUSR1)
			run := j.waitWithin(promptEnd, "a second request held the run up")
			if run.code != 7 {
				t.Errorf("exit %d, want the client's own 7:\n%s", run.code, run.stderr)
			}
			raw, err := os.ReadFile(j.paths.SessionCapture())
			if err != nil {
				t.Fatalf("the run left no capture: %v\n%s", err, run.stderr)
			}
			if doc := string(gunzipped(t, raw)); !strings.HasPrefix(doc, `{"info":{"id":"ses_`) {
				t.Errorf("the capture is not the session's whole export: %q", doc)
			}
		})
	})
}
