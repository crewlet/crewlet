//go:build unix

package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// fakeDaemonPID is where the fake keeps its daemon, relative to the home:
// the home_daemon.pid_file the cases declare.
const fakeDaemonPID = ".daemon/gateway.pid"

// fakeHomeDaemon plays a harness whose calls run in a daemon it keeps per home,
// as Hermes Agent 0.23.4's gateway: the first call starts it in a session of
// its own with that call's key, and a call that finds it running is served by
// it, on the key it started with. Its stop command (`daemon-stop`) kills the
// daemon its pid file names and removes the file. Each daemon started is
// logged to FAKE_DAEMON_LOG, one pid a line.
//
// `gateway stop` is Hermes's own stop, as 0.23.4 answers it: while the daemon
// still runs a turn (a `.turn` file beside its pid file) it refuses, exit 1,
// unless `--yes` is passed.
func fakeHomeDaemon() {
	pidFile := filepath.Join(os.Getenv("HOME"), os.Getenv("FAKE_HOME_DAEMON"))
	keyFile := pidFile + ".key"
	turnFile := pidFile + ".turn"
	running := func() (int, bool) {
		raw, err := os.ReadFile(pidFile) //nolint:gosec // the fake's own home
		if err != nil {
			return 0, false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		return pid, err == nil && syscall.Kill(pid, 0) == nil
	}
	hermesStop := slices.Contains(os.Args, "gateway") && slices.Contains(os.Args, "stop")
	if hermesStop || slices.Contains(os.Args, "daemon-stop") {
		if _, err := os.Stat(turnFile); hermesStop && err == nil && !slices.Contains(os.Args, "--yes") {
			fmt.Println("✗ Refusing to stop the gateway: 1 live session(s) would be interrupted.")
			os.Exit(1)
		}
		if pid, ok := running(); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = os.Remove(pidFile)
		_ = os.Remove(keyFile)
		_ = os.Remove(turnFile)
		return
	}
	if _, ok := running(); ok {
		key, _ := os.ReadFile(keyFile) //nolint:gosec // the fake's own home
		fmt.Print(string(key))
		return
	}
	child := exec.Command(os.Args[0], "-test.run=TestFakeHarness") //nolint:gosec // this test binary
	child.Env = append(os.Environ(), "FAKE_DAEMON_CHILD=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness could not start its daemon:", err)
		os.Exit(1)
	}
	key := os.Getenv("FAKE_VENDOR_API_KEY")
	err := errors.Join(os.MkdirAll(filepath.Dir(pidFile), 0o700),
		os.WriteFile(keyFile, []byte(key), 0o600),
		os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600))
	if log := os.Getenv("FAKE_DAEMON_LOG"); log != "" && err == nil {
		f, openErr := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the case's own file
		err = openErr
		if openErr == nil {
			_, err = fmt.Fprintln(f, child.Process.Pid)
			err = errors.Join(err, f.Close())
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake harness could not record its daemon:", err)
		os.Exit(1)
	}
	fmt.Print(key)
}

// fakeDaemonChild is the daemon: it holds nothing open and does nothing until it
// is killed ([procgrouptest.Hold]).
func fakeDaemonChild() { procgrouptest.Hold() }

// daemonProvider is an entry on dir, signed in with key, whose harness keeps a
// daemon per home ([fakeHomeDaemon]) declared as the profile's home_daemon.
func daemonProvider(t *testing.T, dir, key, log string) *Provider {
	t.Helper()
	return daemonProviderStopping(t, dir, key, log, []any{"daemon-stop"})
}

// daemonProviderStopping is [daemonProvider] with stop as its home_daemon's
// stop_args.
func daemonProviderStopping(t *testing.T, dir, key, log string, stop []any) *Provider {
	t.Helper()
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir,
		Overrides: map[string]any{
			"binary":        os.Args[0],
			"complete_args": []any{"-test.run=TestFakeHarness"},
			"model_args":    []any{},
			"output":        "text",
			"prompt_mode":   "stdin",
			"api_key_env":   "FAKE_VENDOR_API_KEY",
			"home_daemon":   map[string]any{"pid_file": fakeDaemonPID, "stop_args": stop},
		},
		Timeout: 20 * time.Second, MaxConcurrent: 2,
		Env:  fakeChildEnv(map[string]string{"FAKE_HOME_DAEMON": fakeDaemonPID, "FAKE_DAEMON_LOG": log}),
		Auth: Auth{Mode: AuthAPIKey, APIKey: key},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// loggedDaemons reads the pids the fake logged.
func loggedDaemons(t *testing.T, log string) []int {
	t.Helper()
	raw, err := os.ReadFile(log) //nolint:gosec // the case's own file
	if err != nil {
		t.Fatalf("no daemon was started: %v", err)
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("daemon log line %q", line)
		}
		pids = append(pids, pid)
	}
	return pids
}

// assertDaemonsGone holds every daemon the fake logged to having ended: one
// the engine stopped was killed, so it is gone once its new parent reaps it.
// One still running is killed, so a failing case leaves none behind.
func assertDaemonsGone(t *testing.T, log string) {
	t.Helper()
	for _, pid := range loggedDaemons(t, log) {
		if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("a daemon (pid %d) outlived the call that started it", pid)
		}
	}
}

// A HARNESS'S DAEMON ENDS WITH THE CALL THAT STARTED IT. Hermes Agent 0.23.4
// runs each `-z` turn in a gateway it starts per home in a session of its own,
// so it outlives the call's process group, keeps the call's key in its
// environment and serves the home's next call on it. The engine runs the
// profile's stop command as the call releases the home, so each call starts a
// daemon of its own: two calls on two keys each go out on their own.
//
// Mutation: drop the stop from Release, and the last call's daemon outlives
// it; drop it from the generation's open too, and the second call goes out on
// the first key.
func TestAHarnessDaemonEndsWithItsCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	log := filepath.Join(t.TempDir(), "daemons")
	for _, key := range []string{"sk-first", "sk-second"} {
		comp, err := ask(t, daemonProvider(t, dir, key, log), llm.Request{})
		if err != nil {
			t.Fatalf("call on %s: %v", key, err)
		}
		if comp.Content != key {
			t.Errorf("the call on %s went out on %q", key, comp.Content)
		}
	}
	assertDaemonsGone(t, log)
}

// A DAEMON AN ENGINE THAT DIED LEFT IS STOPPED BEFORE THE NEXT CALL OPENS ITS
// HOME. Nothing released the home it ran in, so it still records itself there
// and still holds the dead engine's call's key: the next call must not be
// served by it.
//
// Mutation: drop the stop from the generation's open, and the call goes out on
// the stale key and the daemon lives on.
func TestADaemonAnEngineLeftIsStoppedBeforeTheNextCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	log := filepath.Join(t.TempDir(), "daemons")
	p := daemonProvider(t, dir, "sk-fresh", log)
	startStaleDaemon(t, p, log)

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "sk-fresh" {
		t.Errorf("the call went out on %q, want its own sk-fresh", comp.Content)
	}
	assertDaemonsGone(t, log)
}

// startStaleDaemon starts, in p's key home, the daemon a call of an engine that
// died mid-call left there, run as that call ran it, and returns the home.
func startStaleDaemon(t *testing.T, p *Provider, log string) string {
	t.Helper()
	home := filepath.Join(p.ws.KeySeatDir(llm.SeatOf(t.Context()), p.Key()), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := exec.Command(os.Args[0], "-test.run=TestFakeHarness") //nolint:gosec // this test binary
	stale.Dir = home
	stale.Env = append(os.Environ(), "HOME="+home, helperEnv+"=1", "FAKE_HOME_DAEMON="+fakeDaemonPID,
		"FAKE_VENDOR_API_KEY=sk-stale", "FAKE_DAEMON_LOG="+log)
	if out, err := stale.CombinedOutput(); err != nil || string(out) != "sk-stale" {
		t.Fatalf("starting the stale daemon: %v: %s", err, out)
	}
	return home
}

// A DAEMON STILL RUNNING A TURN IS STOPPED TOO. A call cut short (at its
// timeout, or by an engine that died) leaves its turn running in Hermes Agent
// 0.23.4's gateway, and a plain `hermes gateway stop` refuses a live turn with
// exit 1, so the home would stay held by the old call's daemon. The built-in
// profile's own stop command stops it, and the next call goes out on its key.
//
// Mutation: drop --yes from the hermes profile's home_daemon.stop_args, and
// the next call fails on the refused stop.
func TestADaemonStillRunningATurnIsStoppedBeforeTheNextCall(t *testing.T) {
	t.Parallel()
	hermes, ok := harnessprofile.Builtin("hermes")
	if !ok || hermes.HomeDaemon == nil {
		t.Fatal("the built-in hermes profile declares no home_daemon")
	}
	stop := make([]any, 0, len(hermes.HomeDaemon.StopArgs))
	for _, arg := range hermes.HomeDaemon.StopArgs {
		stop = append(stop, arg)
	}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	log := filepath.Join(t.TempDir(), "daemons")
	p := daemonProviderStopping(t, dir, "sk-fresh", log, stop)
	home := startStaleDaemon(t, p, log)
	if err := os.WriteFile(filepath.Join(home, fakeDaemonPID+".turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "sk-fresh" {
		t.Errorf("the call went out on %q, want its own sk-fresh", comp.Content)
	}
	assertDaemonsGone(t, log)
}

// fifo makes a named pipe in a directory of the case's own, for a fake to signal
// through without a clock ([runFakeHarness]'s FAKE_STARTED and FAKE_WAIT_FOR).
func fifo(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A FALLBACK ENTRY NEVER GOES OUT ON THE KEY OF THE ENTRY BEFORE IT. A harness
// may keep what a key signed it in to in its home (Cursor Agent writes the key
// into its auth.json and prefers it to the variable), and a key home is emptied
// only when its generation closes, so while a call of the first entry is in
// flight, a call of a second entry for the same seat in the same home started
// on the first entry's key. Each entry has a key home of its own per seat.
//
// Mutation: key a key home by the seat alone, and the second entry's call
// starts on the session the first entry's call wrote down.
func TestAFallbackEntryNeverGoesOutOnThePreviousEntrysKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	started, release := fifo(t, "started"), fifo(t, "release")
	entry := func(key, apiKey string, extra map[string]string) *Provider {
		env := map[string]string{"FAKE_KEY_SESSION": ".vendor/auth.json"}
		for k, v := range extra {
			env[k] = v
		}
		p, err := New(Config{
			Key: key, Harness: "custom", StateDir: dir,
			Overrides: map[string]any{
				"binary":           os.Args[0],
				"complete_args":    []any{"-test.run=TestFakeHarness"},
				"model_args":       []any{},
				"output":           "text",
				"prompt_mode":      "stdin",
				"api_key_env":      "FAKE_VENDOR_API_KEY",
				"credential_paths": []any{".vendor/auth.json"},
			},
			Timeout: time.Minute, MaxConcurrent: 2, Env: fakeChildEnv(env),
			Auth: Auth{Mode: AuthAPIKey, APIKey: apiKey},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}
	held := entry("first", "sk-first", map[string]string{"FAKE_STARTED": started, "FAKE_WAIT_FOR": release})
	first := entry("first", "sk-first", nil)
	second := entry("second", "sk-second", nil)

	// A call of the first entry stays in flight, holding its home's generation open.
	done := make(chan error, 1)
	go func() {
		_, err := ask(t, held, llm.Request{})
		done <- err
	}()
	if _, err := os.ReadFile(started); err != nil { //nolint:gosec // the case's own FIFO
		t.Fatal(err)
	}
	// Another of its calls writes down what its key signed it in to.
	if comp, err := ask(t, first, llm.Request{}); err != nil || comp.Content != "nothing" {
		t.Fatalf("the first entry's call = %v, %v; want it to start on nothing", comp, err)
	}
	comp, err := ask(t, second, llm.Request{})
	if err != nil {
		t.Fatalf("the second entry's call: %v", err)
	}
	if comp.Content != "nothing" {
		t.Errorf("the second entry's call started on %q, want nothing of the first entry's", comp.Content)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the held call: %v", err)
	}
}
