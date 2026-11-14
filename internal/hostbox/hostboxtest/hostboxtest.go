// Package hostboxtest is THE KILL MATRIX a writer of a shared login is
// certified by. The writer runs as a process of its own (the test binary
// re-executed), is killed at every step of the journaled writes its script
// makes ([hostbox.CredentialStep]), one process per step, and what it left is
// judged as the next reader reads it.
//
// AT A NAMED STEP, NEVER AT A RANDOM MOMENT. The three suites it serves —
// hostbox's own write, the harness's operator writes and a seat's carry, and a
// local box's teardown — each killed a writer looping its writes after a random
// sleep, counted the kills that found a journal standing, and failed when none
// did. On a loaded machine none did, and when one did the case certified only
// the instants chance had hit. Here the writer announces every step it reaches
// and parks at the one it was told, holding the lock as a real writer would,
// and the matrix kills it there: every step, in every run.
//
// THE PROTOCOL. [KillAtEveryStep] runs the writer once to its end, to learn its
// steps, then once per step, with that step's number in the writer's
// environment. The writer ([Writer]) announces on a pipe of its own, file
// descriptor 3 — never stdout, which the code it drives may write to — one JSON
// line an announcement: ready once its setup is done, each step its writes
// reach after that, and done at its end. At the step it was told, it announces
// the step as stopped and reads its stdin, whose write end the matrix holds; the
// matrix kills it with SIGKILL and checks that SIGKILL is what ended it. A
// writer whose matrix is gone reads the end of its stdin and exits 3 without
// returning into the write, so a parked writer never finishes a write behind a
// case's back: not procgrouptest.Hold, whose sleep ends, and not `select {}`,
// which the runtime can take for a deadlock.
//
// WHAT IS JUDGED. A case declares its script ([Script]): the login files it
// reads, and the logins of record, one the setup leaves and one for each write
// the script makes. Every step of the run to its end is replayed over them, so
// each step knows exactly which files the steps so far leave and whether a
// journal stands. A writer killed there must leave exactly that, and the next
// reader ([Run.Read]) must read the login the interrupted write committed and
// leave no journal standing. A run whose steps do not fit its script is refused
// before any kill: a step outside a write, a commit while a journal stands, a
// file written that its write's login does not hold, or removed that it holds,
// a write whose close leaves another login, and — the one sentence that
// catches a writer that writes a login a file at a time, and one that writes it
// outside the journal and so reaches no step at all — a script that makes other
// than one write for each login of record after the first.
//
// WHAT IS NOT KILLED: a reader finishing a write, which reports no steps
// (hostbox.CredentialSteps says why), and a write replacing one left standing,
// whose merge hostbox's TestAWriteReplacesOneLeftStanding holds.
package hostboxtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// Script is what a writer does after its setup, as the matrix judges it.
type Script struct {
	// Names are the login's files the matrix reads, by their names in the
	// shared directory. A file a write also makes outside them, such as the
	// harness's install record, is neither replayed nor judged.
	Names []string
	// Logins are the logins of record, each over Names: Logins[0] is what
	// the setup leaves, and Logins[w] what the script's write w commits. The
	// script makes exactly len(Logins)-1 writes, each finished before the
	// next begins.
	Logins []map[string]string
}

// Run is one process of a writer, made afresh for every run, over a directory
// of its own.
type Run struct {
	// Cmd is the writer: this test binary re-executed with the case's own
	// variables, not yet started, its Stdin, Stdout, Stderr and ExtraFiles
	// unset, since the matrix owns them.
	Cmd *exec.Cmd
	// Dir is the shared credential directory the writer writes.
	Dir string
	// Read reads the login as the next process reads it, the reader that
	// finishes a write left standing, over the script's Names.
	Read func(t *testing.T) map[string]string
}

// stopEnv hands a writer the step to park at, from 1, or 0 for none. Its
// presence is what tells [NewWriter] the matrix started the process.
const stopEnv = "CREWLET_HOSTBOXTEST_STOP"

// announceFD is the writer's end of the matrix's pipe: the first of
// [exec.Cmd.ExtraFiles], which the writer has as descriptor 3.
const announceFD = 3

// KillAtEveryStep certifies a writer by killing it at every step its script
// reaches, one process per step, each in a parallel subtest of its own, and
// returns the steps, for a case that holds them to a sequence of its own.
//
// newRun makes each process: once for the run to the end, with the case's t,
// and once for each kill, with that kill's subtest's.
func KillAtEveryStep(t *testing.T, script Script, newRun func(t *testing.T) Run) []Stop {
	t.Helper()
	checkScript(t, script)
	run := newRun(t)
	p := start(t, run, 0)
	announced, readErr := p.readToEnd()
	if err := p.wait(); err != nil || readErr != nil {
		t.Fatalf("the writer's run to its end failed (%v, %v): %s", err, readErr, p.output())
	}
	stops, err := number(script, announced)
	if err != nil {
		t.Fatalf("the writer's run to its end does not fit its script: %v", err)
	}
	files, standing := look(t, run.Dir, script.Names)
	if last := script.Logins[len(script.Logins)-1]; !maps.Equal(files, last) || standing {
		t.Fatalf("the writer's run to its end left %v (a journal standing: %t), not its last login %v",
			files, standing, last)
	}
	for i, stop := range stops {
		t.Run(subtest(stop), func(t *testing.T) {
			t.Parallel()
			killAt(t, script, stops[:i+1], newRun(t))
		})
	}
	return stops
}

// killAt runs a writer told to park at the last of reached, the steps up to
// it, kills it there, and judges what it left.
func killAt(t *testing.T, script Script, reached []Stop, run Run) {
	t.Helper()
	stop := reached[len(reached)-1]
	p := start(t, run, stop.Index)
	if err := p.readTo(reached); err != nil {
		t.Fatalf("killing at %v: %v: %s", stop, err, p.output())
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing at %v: %v", stop, err)
	}
	waitErr := p.wait()
	if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() ||
		status.Signal() != syscall.SIGKILL {
		t.Fatalf("parked at %v, the writer ended (%v) other than by the kill, so it was not killed there: %s",
			stop, waitErr, p.output())
	}
	files, standing := look(t, run.Dir, script.Names)
	read := run.Read(t)
	_, after := look(t, run.Dir, nil)
	for _, problem := range judge(script, stop, files, standing, read, after) {
		t.Error(problem)
	}
}

// subtest names a kill's subtest by its stop: its place, its write, its step
// and its file.
func subtest(stop Stop) string {
	name := fmt.Sprintf("%d-write%d-%s", stop.Index, stop.Write, stop.Step)
	if stop.Name != "" {
		name += "-" + stop.Name
	}
	return name
}

// checkScript refuses a script the matrix cannot judge: no names to read, a
// name twice, no write to kill, or a login holding a file outside its names.
func checkScript(t *testing.T, script Script) {
	t.Helper()
	if len(script.Names) == 0 || len(script.Logins) < 2 {
		t.Fatalf("hostboxtest: a script names the files it reads and at least two logins of record, "+
			"the setup's and one write's; this one names %d files and %d logins",
			len(script.Names), len(script.Logins))
	}
	names := map[string]bool{}
	for _, name := range script.Names {
		if names[name] {
			t.Fatalf("hostboxtest: the script names %q twice", name)
		}
		names[name] = true
	}
	for i, login := range script.Logins {
		for name := range login {
			if !names[name] {
				t.Fatalf("hostboxtest: login %d holds %q, which is none of the script's names %v",
					i, name, script.Names)
			}
		}
	}
}

// process is one writer the matrix started.
type process struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// pipe is the matrix's end of the announcements, read through lines.
	pipe  *os.File
	lines *json.Decoder
	out   *syncBuffer

	waited  bool
	waitErr error
}

// start starts run's writer told to park at stop, and ends it with the case
// whatever happens to the case.
func start(t *testing.T, run Run, stop int) *process {
	t.Helper()
	cmd := run.Cmd
	if cmd == nil || run.Dir == "" || run.Read == nil {
		t.Fatalf("hostboxtest: a Run names its writer, its directory and its reader")
	}
	if cmd.Process != nil || cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil ||
		len(cmd.ExtraFiles) != 0 {
		t.Fatalf("hostboxtest: a Run's Cmd is the writer not yet started, with its Stdin, Stdout, " +
			"Stderr and ExtraFiles left to the matrix")
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	// The race runtime's exit sleep would cost every writer that runs to its
	// end a second of nothing. Appended, so they win over what env holds.
	cmd.Env = append(slices.Clip(env), stopEnv+"="+strconv.Itoa(stop),
		procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	pipe, end, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{end}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Start()
	// The writer has its own copy now: with the matrix's closed, the pipe
	// reads its end once the writer is gone.
	_ = end.Close()
	if err != nil {
		_ = pipe.Close()
		t.Fatalf("starting the writer: %v", err)
	}
	p := &process{cmd: cmd, stdin: stdin, pipe: pipe, lines: json.NewDecoder(pipe), out: out}
	p.lines.DisallowUnknownFields()
	t.Cleanup(p.end)
	return p
}

// next reads the writer's next announcement; io.EOF once it is gone.
func (p *process) next() (announcement, error) {
	var a announcement
	err := p.lines.Decode(&a)
	return a, err
}

// readToEnd reads every announcement of a writer told to park nowhere.
func (p *process) readToEnd() ([]announcement, error) {
	var all []announcement
	for {
		a, err := p.next()
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return all, fmt.Errorf("reading the writer's announcements: %w", err)
		}
		all = append(all, a)
	}
}

// readTo reads the announcements of a writer told to park at the last of
// reached, and refuses any that is not ready and then reached, step for step,
// as the run to the end reported them, the last one stopped.
func (p *process) readTo(reached []Stop) error {
	a, err := p.next()
	if err != nil {
		return fmt.Errorf("the writer ended (%w) before it was ready", err)
	}
	if a.Kind != kindReady {
		return fmt.Errorf("the writer announced %+v where it is ready", a)
	}
	for i, want := range reached {
		a, err := p.next()
		if err != nil {
			return fmt.Errorf("the writer ended (%w) before %v", err, want)
		}
		if a.Kind != kindStep || a.Index != want.Index || a.Step != want.Step || a.Name != want.Name ||
			a.Stopped != (i == len(reached)-1) {
			return fmt.Errorf("the writer announced %+v where its run to the end reached %v", a, want)
		}
	}
	return nil
}

// wait reaps the writer, once: its stdin closed first, so a writer still
// parked ends rather than hold the wait.
func (p *process) wait() error {
	if !p.waited {
		_ = p.stdin.Close()
		p.waitErr = p.cmd.Wait()
		p.waited = true
	}
	return p.waitErr
}

// end kills and reaps a writer the case left running, and closes the pipe.
func (p *process) end() {
	if !p.waited {
		_ = p.cmd.Process.Kill()
		_ = p.wait()
	}
	_ = p.pipe.Close()
}

// output is what the writer wrote to stdout and stderr, for a failure.
func (p *process) output() string { return p.out.String() }

// syncBuffer is a writer's stdout and stderr, written by os/exec's copying
// goroutine and read by the case.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// look reads what a writer left in dir: the content of each of names that is
// a regular file there, and whether a journal stands beside it. Anything else
// at a name fails the case: no writer leaves it.
func look(t *testing.T, dir string, names []string) (map[string]string, bool) {
	t.Helper()
	files := map[string]string{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file: %v", path, info.Mode())
		}
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the case's own directory
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	_, err := os.Lstat(hostbox.CredentialJournal(dir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files, err == nil
}
