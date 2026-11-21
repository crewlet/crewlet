package boxhelpertest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// probeScript is the probe, beside boxhelper's tests, from the module's root.
var probeScript = filepath.Join("internal", "browser", "boxhelper", "boxhelper_probe.mjs")

// probeTimeout bounds one wait on the probe: its longest command, an install
// a case bounds or a failing registry ends, settles in a few seconds, so a
// minute is a hang, never a slow machine.
const probeTimeout = time.Minute

// probeLine is the longest answer the probe may give: far past any it gives.
const probeLine = 16 << 20

// Probe is the probe running under a box's node against the box's modules,
// answering one command at a time ([Probe.Do]).
type Probe struct {
	t       testing.TB
	stdin   io.WriteCloser
	answers <-chan []byte
	stderr  *tail
}

// Probe starts the probe under the box's node, importing the modules from the
// box's folder, in the box's environment and extra after it, which an install
// hands npm whole; t's end ends it.
func (b *Box) Probe(t testing.TB, extra ...string) *Probe {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), b.env.Node, filepath.Join(sourcetree.Root(t), probeScript), b.Folder)
	cmd.Env = append(b.Environ(), extra...)
	cmd.WaitDelay = probeTimeout
	stderr := &tail{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the probe under %s: %v", b.env.Node, err)
	}
	answers := make(chan []byte, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		lines := bufio.NewScanner(stdout)
		lines.Buffer(make([]byte, 0, 64<<10), probeLine)
		for lines.Scan() {
			answers <- bytes.Clone(lines.Bytes())
		}
		close(answers)
		// Every read of stdout is done, so Wait may close it.
		_ = cmd.Wait()
	}()
	// t's context ends as its cleanup begins, which kills the probe; this
	// drains what it answered unasked and waits for it to be reaped.
	t.Cleanup(func() {
		_ = stdin.Close()
		for range answers {
		}
		<-exited
	})
	return &Probe{t: t, stdin: stdin, answers: answers, stderr: stderr}
}

// Do sends command, a value whose op names one of the probe's commands, and
// decodes the probe's answer into answer: the case fails on an answer of
// another op, an error the probe reports, or none within probeTimeout.
func (p *Probe) Do(command, answer any) {
	p.t.Helper()
	op := p.send(command)
	ctx, cancel := context.WithTimeout(p.t.Context(), probeTimeout)
	defer cancel()
	var got []byte
	select {
	case a, ok := <-p.answers:
		if !ok {
			p.t.Fatalf("the probe ended before it answered %s; its stderr:\n%s", op, p.stderr)
		}
		got = a
	case <-ctx.Done():
		p.t.Fatalf("the probe did not answer %s within %s; its stderr:\n%s", op, probeTimeout, p.stderr)
	}
	var head struct {
		Op      string `json:"op"`
		Message string `json:"message"`
	}
	switch err := json.Unmarshal(got, &head); {
	case err != nil:
		p.t.Fatalf("the probe answered %s with %q, which is not JSON: %v", op, got, err)
	case head.Op == "error":
		p.t.Fatalf("the probe failed %s: %s", op, head.Message)
	case head.Op != op:
		p.t.Fatalf("the probe answered %s with %s", op, got)
	}
	if err := json.Unmarshal(got, answer); err != nil {
		p.t.Fatalf("the probe's answer to %s, %s: %v", op, got, err)
	}
}

// Killed sends command, one the probe is killed by part way, as a box's
// helper may be, and waits for it to end unanswered: the case fails on an
// answer, or on a probe still running after probeTimeout. The probe answers
// nothing more; a case goes on with another.
func (p *Probe) Killed(command any) {
	p.t.Helper()
	op := p.send(command)
	ctx, cancel := context.WithTimeout(p.t.Context(), probeTimeout)
	defer cancel()
	select {
	case a, ok := <-p.answers:
		if ok {
			p.t.Fatalf("the probe answered %s with %s, and was to be killed by it", op, a)
		}
	case <-ctx.Done():
		p.t.Fatalf("the probe was not killed by %s within %s; its stderr:\n%s", op, probeTimeout, p.stderr)
	}
}

// send writes command to the probe and answers its op.
func (p *Probe) send(command any) string {
	p.t.Helper()
	line, err := json.Marshal(command)
	if err != nil {
		p.t.Fatal(err)
	}
	var sent struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(line, &sent); err != nil || sent.Op == "" {
		p.t.Fatalf("a probe command names its op: %s", line)
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatalf("sending the probe %s: %v; its stderr:\n%s", sent.Op, err, p.stderr)
	}
	return sent.Op
}

// tail keeps the last bytes the probe wrote to its stderr, for a case that
// fails to say why.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

// tailBytes is how much of the probe's stderr a failure carries: a stack or
// two, where its reason is.
const tailBytes = 8 << 10

func (w *tail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - tailBytes; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	return len(p), nil
}

func (w *tail) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
