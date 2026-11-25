//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// THE CASE NEEDS A TERMINAL, and a test process has none to lend a child: it
// opens a pseudo-terminal and starts this binary as the import in a session of
// its own whose controlling terminal is that pty — the shape a person running
// crewlet in a shell has. Opening a pty is platform code (unlocking it and
// naming its other end are ioctls whose numbers and shapes differ), and this
// file holds Linux's, where every CI runner is; what the case asserts — job
// control and process groups — is POSIX, the same on darwin.

// AN IMPORT ASKS FOR THE SOURCE'S CREDENTIAL ON THE TERMINAL. A source that
// asks for a user name and a password no credential helper holds — a private
// repository on a forge, a first use of a helper — has git prompt on the
// terminal, and the person answers it: git's prompt is read from the
// controlling terminal, which a process group of its own reads from as a
// BACKGROUND group, and the kernel stopped git (SIGTTIN) at its first read,
// so the import waited on a stopped git until somebody pressed Ctrl-C. The
// person's answer reaches the source, and the source's refusal of it ends the
// import with git's own words.
//
// Mutation: start the source's git in a process group of its own, and the
// prompt is never answered: git is left stopped.
func TestImportAsksForTheSourcesCredentialOnTheTerminal(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	offered := make(chan string, 8)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok {
			select {
			case offered <- user + ":" + pass:
			default:
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="a private repository"`)
		http.Error(w, "a credential, please", http.StatusUnauthorized)
	}))
	t.Cleanup(source.Close)

	master, tty := openPTY(t)
	cmd := exec.Command(os.Args[0], "gitlet", "import", source.URL+"/acme/web.git", "web", "-local", "-api", h.base,
		"-git", hermeticGit(t))
	cmd.Env = append(promptingEnv(), apiTokenEnv+"="+h.token, mainProbeEnv+"=1",
		procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// A SESSION OF ITS OWN, the pty its controlling terminal: the session
	// leader's group is the terminal's foreground group, as a shell makes a
	// command it runs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the import: %v", err)
	}
	_ = tty.Close()
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	session := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-session, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
		}
	})
	screen := readTerminal(master)

	answer := func(prompt, reply string) {
		t.Helper()
		if !screen.waitFor(prompt, 30*time.Second) {
			t.Fatalf("no %q on the terminal in 30 s — stopped in the import's session: %v\nthe terminal:\n%s",
				prompt, stoppedIn(session), screen.String())
		}
		if _, err := master.WriteString(reply + "\n"); err != nil {
			t.Fatalf("answer %q: %v", prompt, err)
		}
	}
	answer("Username for '", "importer")
	answer("Password for '", "s3cret")
	select {
	case got := <-offered:
		if got != "importer:s3cret" {
			t.Errorf("the source was offered %q, want what the person typed", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the source was offered no credential in 30 s — stopped in the import's session: %v\n"+
			"the terminal:\n%s", stoppedIn(session), screen.String())
	}
	select {
	case <-exited:
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			t.Errorf("an import the source refused exited %v, want a failure", waitErr)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the import did not end in 60 s — stopped in its session: %v\nthe terminal:\n%s",
			stoppedIn(session), screen.String())
	}
	if !screen.waitFor("Authentication failed", 10*time.Second) {
		t.Errorf("the import did not end in git's words:\n%s", screen.String())
	}
}

// promptingEnv is this process's environment less the node's token, which the
// case hands the command itself, and the probe's switch, which it sets. What would
// answer git's prompt in the person's place or forbid it — GIT_ASKPASS,
// SSH_ASKPASS, GIT_TERMINAL_PROMPT — is not in it: TestMain removed every git
// variable before any case ran ([scrubGitEnv]).
func promptingEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case apiTokenEnv, mainProbeEnv:
			continue
		}
		env = append(env, kv)
	}
	return env
}

// openPTY opens a pseudo-terminal: its master end, which the case reads and
// types into, and the terminal itself, for the child.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open a pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	fd := int(master.Fd())
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock the pseudo-terminal: %v", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("name the pseudo-terminal: %v", err)
	}
	tty, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open the terminal end: %v", err)
	}
	return master, tty
}

// terminalScreen is everything written to a pty, as the person at it saw it.
type terminalScreen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// readTerminal reads master until the terminal's last writer is gone.
func readTerminal(master *os.File) *terminalScreen {
	s := &terminalScreen{}
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := master.Read(chunk)
			s.mu.Lock()
			s.buf.Write(chunk[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *terminalScreen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor reports whether the screen shows text within d.
func (s *terminalScreen) waitFor(text string, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		if strings.Contains(s.String(), text) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// stoppedIn names every process of session the kernel holds stopped, by pid
// and command: what a prompt read from a background group leaves behind. Read
// thread by thread ([heldStopped]), since a stat line holds its main thread's
// state alone.
func stoppedIn(session int) []string {
	var stopped []string
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, path := range stats {
		// pid (comm) state ppid pgrp session …: the session is field 6.
		raw, fields := statFields(path)
		if len(fields) < 4 || fields[3] != strconv.Itoa(session) || !heldStopped(filepath.Dir(path)) {
			continue
		}
		stopped = append(stopped, fmt.Sprintf("%s%s", strings.Fields(string(raw))[0],
			raw[bytes.IndexByte(raw, '('):bytes.LastIndexByte(raw, ')')+1]))
	}
	return stopped
}

// heldStopped reports whether the process whose /proc entry is dir is held
// stopped: a thread of it stopped (T, or t under a tracer) and each other
// stopped too or exited (Z, X or x, as internal/procgroup reads a state). A
// group stop reaches each thread separately, and a main thread can have exited
// while the others stop.
func heldStopped(dir string) bool {
	tasks, _ := filepath.Glob(filepath.Join(dir, "task", "[0-9]*", "stat"))
	held := false
	for _, path := range tasks {
		_, fields := statFields(path)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "T", "t":
			held = true
		case "Z", "X", "x":
			// Exited: it runs nothing.
		default:
			return false
		}
	}
	return held
}

// statFields is the stat line at path and its fields after the command,
// counted from its LAST closing parenthesis, since the command is the
// process's own to name; none where it cannot be read.
func statFields(path string) ([]byte, []string) {
	raw, err := os.ReadFile(path) //nolint:gosec // the kernel's record of a process
	if err != nil {
		return nil, nil
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return nil, nil
	}
	return raw, strings.Fields(string(raw[end+1:]))
}

// THE TERMINAL CASE NAMES A PROCESS HELD STOPPED BY EACH THREAD OF IT, when it
// says what its session left stopped: a process whose main thread alone has
// exited, its stat line reading Z, is named once a stop reaches the threads
// that run on.
//
// Mutation: read the process's own stat line, its main thread's state, and
// the process is never named.
func TestTheTerminalCaseNamesAProcessHeldStoppedByEachThread(t *testing.T) {
	t.Parallel()
	pid := procgrouptest.StartExitedMainThread(t, &syscall.SysProcAttr{Setsid: true})
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stopping the process: %v", err)
	}
	named := func() bool {
		return slices.ContainsFunc(stoppedIn(pid), func(s string) bool { return strings.HasPrefix(s, strconv.Itoa(pid)+"(") })
	}
	deadline := time.Now().Add(time.Minute)
	for !named() {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the case fails on it
			t.Fatalf("a process whose main thread alone has exited, pid %d, stopped, is never named: %v", pid,
				stoppedIn(pid))
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}
