package boxhelpertest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// RelayWiring and RelayLog are where a case's relay reads its wiring and
// appends its log: the job's files under <home>/.crewlet, as the job script
// lays them out.
const (
	RelayWiring = "browser-relay.json"
	RelayLog    = "browser-relay.log"
)

// Relay is a job's relay a case started as the job script starts one: in
// the background of a parent sh that waits for it, its output appended to
// the job's log. PID is the relay's own, node's, which the subshell's exec
// makes $!; Script is its parent's.
type Relay struct {
	PID    int
	Script int
	Log    string

	t      testing.TB
	cmd    *exec.Cmd
	exited chan int
}

// Relay writes the job's wiring, url its route and job its launch, 0600 as
// the engine writes it, and starts the relay on it as the job script does,
// `( boxhelper.RelayScript ) < /dev/null >> browser-relay.log 2>&1 &`, under
// a parent sh that waits for it, in the box's environment and env after it.
// The case's end TERMs it and its parent.
func (b *Box) Relay(t testing.TB, url, job string, env ...string) *Relay {
	t.Helper()
	wiring := b.WriteWiring(t, browser.RelayWiring{V: browser.RelayWiringVersion, URL: url, Job: job})
	return b.StartRelay(t, wiring, env...)
}

// WriteWiring writes w as the job's wiring file, 0600, and answers its path.
func (b *Box) WriteWiring(t testing.TB, w any) string {
	t.Helper()
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(b.Home, ".crewlet", RelayWiring)
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// StartRelay starts the relay on the wiring at file as [Box.Relay] does.
func (b *Box) StartRelay(t testing.TB, file string, env ...string) *Relay {
	t.Helper()
	log := filepath.Join(b.Home, ".crewlet", RelayLog)
	script := "( " + boxhelper.RelayScript(b.Home, file) + " ) < /dev/null >> " + shellword.Quote(log) + " 2>&1 &\n" +
		"relay=$!\necho \"$relay\"\nwait \"$relay\"\necho \"$?\"\n"
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	cmd.Env = append(b.Environ(), env...)
	cmd.Dir = b.Home
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatalf("starting the relay's script: %v", err)
	}
	lines := bufio.NewScanner(out)
	if !lines.Scan() {
		t.Fatalf("the relay's script printed no pid: %v", lines.Err())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines.Text()))
	if err != nil {
		t.Fatalf("the relay's script printed %q for its pid", lines.Text())
	}
	r := &Relay{PID: pid, Script: cmd.Process.Pid, Log: log, t: t, cmd: cmd, exited: make(chan int, 1)}
	go func() {
		code := -1
		if lines.Scan() {
			if n, err := strconv.Atoi(strings.TrimSpace(lines.Text())); err == nil {
				code = n
			}
		}
		r.exited <- code
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(r.PID, syscall.SIGTERM)
		_ = syscall.Kill(r.Script, syscall.SIGKILL)
		procgrouptest.AwaitGone(t, r.PID, relayWithin)
	})
	return r
}

// Term sends the relay TERM, as the job script's end does.
func (r *Relay) Term() {
	r.t.Helper()
	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil {
		r.t.Fatalf("TERMing the relay: %v", err)
	}
}

// KillScript kills the relay's parent outright, as a job script killed by
// anything but its own end.
func (r *Relay) KillScript() {
	r.t.Helper()
	if err := syscall.Kill(r.Script, syscall.SIGKILL); err != nil {
		r.t.Fatalf("killing the relay's script: %v", err)
	}
}

// Exited is the relay's exit status as its parent's wait read it, failing
// the case on none within relayWithin.
func (r *Relay) Exited() int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), relayWithin)
	defer cancel()
	select {
	case code := <-r.exited:
		r.exited <- code
		return code
	case <-ctx.Done():
		r.t.Fatalf("the relay, pid %d, still runs %s on", r.PID, relayWithin)
		return 0
	}
}

// Gone waits for the relay's process to go, failing the case on one still
// running after relayWithin: for a relay whose parent is gone, which nobody
// here reaps.
func (r *Relay) Gone() {
	r.t.Helper()
	if !procgrouptest.AwaitGone(r.t, r.PID, relayWithin) {
		r.t.Fatalf("the relay, pid %d, still runs %s after its script went", r.PID, relayWithin)
	}
}

// logEvery paces a wait on the relay's log, which the relay announces to
// nobody: the case waits on the line it names, never on the time.
const logEvery = 20 * time.Millisecond

// AwaitLog waits for the relay's log to hold a line naming kind, failing the
// case on none within relayWithin.
func (r *Relay) AwaitLog(what, kind string) {
	r.t.Helper()
	deadline := time.Now().Add(relayWithin)
	for !strings.Contains(r.LogText(), " "+kind+" ") {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the case fails on it
			r.t.Fatalf("the relay's log holds no %s, %s on: %q", what, relayWithin, r.LogText())
		}
		time.Sleep(logEvery) //fixedwait:liveness
	}
}

// LogText is the relay's log so far.
func (r *Relay) LogText() string {
	r.t.Helper()
	raw, err := os.ReadFile(r.Log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	return string(raw)
}

// RunHelper runs the box's helper with args under the box's node, in its
// environment and env after it, and answers its exit status and stderr: for a
// case whose subject is an invocation the helper refuses.
func (b *Box) RunHelper(t testing.TB, args []string, env ...string) (code int, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), relayWithin)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(b.Home, "bin", "node"),
		append([]string{filepath.Join(b.Folder, "helper.mjs")}, args...)...)
	cmd.Env = append(b.Environ(), env...)
	cmd.Dir = b.Home
	var errOut strings.Builder
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running the helper: %v", err)
	}
	return code, errOut.String()
}

// StandIn plays the supervisor's side of a box's relay.sock for a case whose
// subject is the relay: it listens where the supervisor would, takes each
// connection a relay makes ([StandIn.Accept]), and hands it to the case to
// answer, read and write as the supervisor would.
type StandIn struct {
	t     testing.TB
	ln    net.Listener
	conns chan *RelaySock
}

// StandIn listens on the box's relay.sock in the supervisor's place, its
// stale path removed first; t's end closes it.
func (b *Box) StandIn(t testing.TB) *StandIn {
	t.Helper()
	socket := filepath.Join(b.Folder, "relay.sock")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("listening on the box's relay.sock: %v", err)
	}
	s := &StandIn{t: t, ln: ln, conns: make(chan *RelaySock, 16)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(s.conns)
				return
			}
			s.conns <- serveConn(t, conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// Accept is the next connection a relay made, failing the case on none
// within relayWithin.
func (s *StandIn) Accept(what string) *RelaySock {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), relayWithin)
	defer cancel()
	select {
	case c, ok := <-s.conns:
		if !ok {
			s.t.Fatalf("relay.sock closed before %s", what)
		}
		return c
	case <-ctx.Done():
		s.t.Fatalf("no relay connected for %s within %s", what, relayWithin)
		return nil
	}
}

// Close stops listening, so the next relay to connect finds no supervisor.
func (s *StandIn) Close() {
	_ = s.ln.Close()
}

// Answer reads the relay's hello on c and answers it as the supervisor of
// helper does under boot, and answers the hello's job.
func (r *RelaySock) Answer(helper, boot string) string {
	r.t.Helper()
	var hello struct {
		Job string `json:"job"`
	}
	r.Next("hello", "the relay's hello", func(Record) bool { return true }).Decode(r.t, &hello)
	r.Send(map[string]any{"t": "hello", "v": 1, "helper": helper, "boot": boot,
		"chromium": map[string]any{"version": "157.0.8103.50", "major": 157, "build": "expected", "sandbox": "waived"}})
	return hello.Job
}

// SendFrame writes a frame record of the supervisor's: header, and jpeg as
// its payload.
func (r *RelaySock) SendFrame(header map[string]any, jpeg []byte) {
	r.t.Helper()
	h := map[string]any{}
	for k, v := range header {
		h[k] = v
	}
	h["bytes"] = len(jpeg)
	r.wmu.Lock()
	defer r.wmu.Unlock()
	body, err := json.Marshal(h)
	if err != nil {
		r.t.Fatal(err)
	}
	record := append(append(lengthOf(body), body...), jpeg...)
	if _, err := r.conn.Write(record); err != nil {
		r.t.Fatalf("writing a frame to relay.sock: %v", err)
	}
}
