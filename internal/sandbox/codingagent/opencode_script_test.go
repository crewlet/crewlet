package codingagent_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// fakeOpenCode is an `opencode` that records what the run's script asks of it,
// in the order it asks, and answers as OpenCode 2.0.32 measured:
//
//   - `serve` prints where it listens and its password, then serves until it
//     is stopped;
//   - `api mcp.list` lists nothing at first — the workspace's servers start as
//     it is first asked about — then the bridge pending, then connected;
//   - `api session.get` prints a session's usage report on one line;
//   - `auth import` keeps what it was given, `auth export` prints FAKE_EXPORT;
//   - `run` keeps its stdin and the server password it was handed.
const fakeOpenCode = `#!/bin/sh
d="$FAKE_DIR"
echo "$*" >> "$d/calls"
case "$1" in
--version) echo "${FAKE_VERSION:-opencode v2.0.32}" ;;
auth)
  case "$2" in
  import) cat > "$d/imported" ;;
  export) printf '%s' "$FAKE_EXPORT" ;;
  esac ;;
serve)
  echo "server listening on http://127.0.0.1:4242"
  echo "server password pw-secret"
  echo $$ > "$d/serve.pid"
  exec sleep 60 ;;
api)
  case "$4" in
  mcp.list)
    n=$(cat "$d/polls" 2> /dev/null || echo 0); n=$((n+1)); echo "$n" > "$d/polls"
    case "$n" in
    1) echo '{"data":[]}' ;;
    2) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"pending"}}]}' ;;
    *) echo '{"data":[{"name":"crewlet-bridge","status":{"status":"connected"}}]}' ;;
    esac ;;
  session.get) echo '{"data":{"id":"'"${6#sessionID=}"'","cost":0.5,"tokens":{"input":10,"output":2,"reasoning":3,"cache":{"read":40,"write":1}}}}' ;;
  esac ;;
run)
  cat > "$d/brief"
  printf '%s' "$OPENCODE_SERVER_PASSWORD" > "$d/run-password"
  echo '{"type":"text","part":{"text":"done"}}'
  exit "${FAKE_RUN_EXIT:-0}" ;;
esac
`

// scriptRun is one execution of a run's script against [fakeOpenCode].
type scriptRun struct {
	dir, home       string
	stdout, stderr  string
	code            int
	calls           []string
	document, brief string
}

// runScript executes the script OpenCode's Command builds, under a real sh,
// the way the wrapper does: the brief on stdin, stdout the event stream.
func runScript(t *testing.T, req sandbox.RunRequest, login string, env ...string) scriptRun {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{home + "/.crewlet", bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fakeOpenCode), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := codingagent.Paths{Home: home}
	if login != "" {
		if err := os.WriteFile(paths.CredentialDocument(), []byte(login), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := scriptOf(t, codingagent.OpenCode{}.Command(req, paths, ""))

	cmd := exec.Command("sh", "-c", script) //nolint:gosec // the script under test
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + home, "FAKE_DIR=" + dir,
	}, env...)
	cmd.Stdin = strings.NewReader(`fix "the" flake`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := scriptRun{dir: dir, home: home, stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		run.code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running the script: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // under the test's own dir
	run.calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	doc, _ := os.ReadFile(paths.CredentialDocument())
	run.document = string(doc)
	brief, _ := os.ReadFile(filepath.Join(dir, "brief")) //nolint:gosec // under the test's own dir
	run.brief = string(brief)
	return run
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

var bridgeOnly = map[string]sandbox.MCPServer{
	"crewlet-bridge": {Name: "crewlet-bridge", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp/x"},
}

// THE BRIEF IS SENT ONLY ONCE THE SEAT'S TOOLS EXIST. OpenCode 2 fixes a step's
// tools as the step starts, so a brief sent before the run's MCP servers have
// connected gets a first step with none of them — which is what `--standalone`
// does. The script waits until every declared server has left `pending`, across
// an answer that lists nothing and one that lists it pending, and only then
// runs the brief, verbatim on stdin, against the server it started.
func TestTheBriefWaitsForTheSeatsTools(t *testing.T) {
	t.Parallel()
	run := runScript(t, sandbox.RunRequest{MCPServers: bridgeOnly}, "")
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	polls, _ := os.ReadFile(filepath.Join(run.dir, "polls")) //nolint:gosec // under the test's own dir
	if strings.TrimSpace(string(polls)) != "3" {
		t.Errorf("mcp.list was asked %s times, want 3 — through the empty and the pending "+
			"answers to the connected one", strings.TrimSpace(string(polls)))
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
	pw, _ := os.ReadFile(filepath.Join(run.dir, "run-password")) //nolint:gosec // under the test's own dir
	if string(pw) != "pw-secret" {
		t.Errorf("the run was handed %q, want the server's password", pw)
	}
	if _, err := os.Stat(filepath.Join(run.home, ".crewlet", "opencode-server")); !os.IsNotExist(err) {
		t.Errorf("the server's password outlived its read (%v)", err)
	}
}

// THE SERVER IS THE RUN'S AND ENDS WITH IT: the done marker is written only
// once the script exits, so a server left running would hold nothing open,
// and one the script never stopped would keep the box's bridge connection
// and its credentials alive past the run.
func TestTheRunsServerIsStoppedWithIt(t *testing.T) {
	t.Parallel()
	run := runScript(t, sandbox.RunRequest{}, "")
	raw, err := os.ReadFile(filepath.Join(run.dir, "serve.pid")) //nolint:gosec // under the test's own dir
	if err != nil {
		t.Fatalf("no server was started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the server (pid %d) outlived the run", pid)
	}
}

// THE RUN'S EXIT IS THE RUN'S, and its usage follows it: the session's report,
// asked of the run's own session, as the stream's last line — which the
// decoder reads as the run's tokens and price.
func TestTheScriptReportsTheSessionsUsageAndKeepsTheRunsExit(t *testing.T) {
	t.Parallel()
	run := runScript(t, sandbox.RunRequest{}, "", "FAKE_RUN_EXIT=3")
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
}

// A BOX GIVEN A LOGIN IMPORTS IT BEFORE THE RUN AND WRITES THE REFRESHED ONE
// BACK AFTER — the export, over the document the box was seeded with, which is
// what the box's teardown copies back to the shared login. Never an export of
// nothing over it, and never a document where the box was given none.
func TestABoxsLoginIsImportedAndTheRefreshedOneWrittenBack(t *testing.T) {
	t.Parallel()
	seeded := `[{"id":"cred_1","key":"old"}]`

	run := runScript(t, sandbox.RunRequest{}, seeded, `FAKE_EXPORT=[{"id":"cred_1","key":"new"}]`)
	imported, _ := os.ReadFile(filepath.Join(run.dir, "imported")) //nolint:gosec // under the test's own dir
	if string(imported) != seeded {
		t.Errorf("imported %q, want the login the box was given", imported)
	}
	if run.callIndex("auth import --standalone") > run.callIndex("serve ") {
		t.Errorf("the login was imported after the server started:\n%s", strings.Join(run.calls, "\n"))
	}
	if run.document != `[{"id":"cred_1","key":"new"}]` {
		t.Errorf("the box's login is %q, want the refreshed export", run.document)
	}

	run = runScript(t, sandbox.RunRequest{}, seeded, "FAKE_EXPORT= [ ]\n")
	if run.document != seeded {
		t.Errorf("an export of nothing replaced the box's login: %q", run.document)
	}

	run = runScript(t, sandbox.RunRequest{}, "", `FAKE_EXPORT=[{"id":"cred_1","key":"minted"}]`)
	if run.document != "" || run.callIndex("auth ") >= 0 {
		t.Errorf("a box given no login touched one: document %q, calls\n%s",
			run.document, strings.Join(run.calls, "\n"))
	}
}

// 1.x CANNOT RUN THIS SCRIPT, and says so in its own words rather than leaving
// the usage text 1.x prints for an unknown flag as the run's failure.
func TestOpenCodeOneIsRefusedByName(t *testing.T) {
	t.Parallel()
	run := runScript(t, sandbox.RunRequest{}, "", "FAKE_VERSION=1.18.35")
	if run.code == 0 {
		t.Fatal("a 1.x box ran")
	}
	if !strings.Contains(run.stderr, "OpenCode 1.18.35") || !strings.Contains(run.stderr, "@opencode/cli") {
		t.Errorf("the refusal does not name the version and the package:\n%s", run.stderr)
	}
	if run.callIndex("serve ") >= 0 || run.callIndex("run ") >= 0 {
		t.Errorf("a 1.x box was driven anyway:\n%s", strings.Join(run.calls, "\n"))
	}
}
