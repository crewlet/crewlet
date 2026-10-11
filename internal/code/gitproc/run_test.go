package gitproc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// NO GIT RUNS WITHOUT A DEADLINE, and the refusal starts nothing.
func TestNoGitRunsWithoutADeadline(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	before := len(calls())
	for _, d := range []time.Duration{0, -1} {
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassRead, Deadline: d})
		if !errors.Is(err, gitproc.ErrNoDeadline) {
			t.Errorf("Deadline %v: %v, want ErrNoDeadline", d, err)
		}
	}
	if got := len(calls()); got != before {
		t.Fatalf("%d git processes ran without a deadline", got-before)
	}
}

// holdBody is a stand-in body that marks "$2" as started and then holds until
// the file "$3" exists, so a case starts and ends each holding run itself.
const holdBody = `: > "$2"; while [ ! -e "$3" ]; do sleep 0.05; done`

// holder is one holding run.
type holder struct {
	started, release string
	done             <-chan outcome
}

// hold starts a holding run in class c and waits until its process runs.
func hold(t *testing.T, p *gitproc.Pool, c gitproc.Class) holder {
	t.Helper()
	dir := t.TempDir()
	h := holder{started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release")}
	h.done = start(t.Context(), p, gitproc.Cmd{Args: []string{"hold", h.started, h.release}, Class: c, Deadline: time.Minute})
	deadline := time.Now().Add(hang)
	for {
		if _, err := os.Stat(h.started); err == nil {
			return h
		}
		select {
		case o := <-h.done:
			t.Fatalf("a holding %s run ended before it held: %v", c, o.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("a holding %s run had not started after %v", c, hang)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// end releases a holding run and waits for its answer.
func (h holder) end(t *testing.T) outcome {
	t.Helper()
	if err := os.WriteFile(h.release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return await(t, h.done, "a released holder")
}

// quick is a run that ends at once.
func quick(c gitproc.Class) gitproc.Cmd {
	return gitproc.Cmd{Args: []string{"quick"}, Class: c, Deadline: time.Minute}
}

// SLOTS BOUND CONCURRENT GIT, in each waiting lane: a run past the lane waits,
// is refused ErrBusy after SlotWait naming the lane, takes a slot handed to it
// in arrival order, and answers its CALLER's cancellation as that rather than
// as a busy node.
func TestSlotsBoundConcurrentGit(t *testing.T) {
	t.Parallel()
	body := `case "$1" in hold) ` + holdBody + `;; esac`
	for _, tc := range []struct {
		class gitproc.Class
		size  int
	}{{gitproc.ClassTransfer, 2}, {gitproc.ClassRead, 1}} {
		t.Run(string(tc.class), func(t *testing.T) {
			t.Parallel()
			o := gitproc.Options{MaxProcesses: 1, MaxTransfers: 2}
			p := fakePool(t, body, o)
			gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
			holders := make([]holder, tc.size)
			for i := range holders {
				holders[i] = hold(t, p, tc.class)
			}
			res, err := p.Run(t.Context(), quick(tc.class))
			if !errors.Is(err, gitproc.ErrBusy) || res.Waited < 300*time.Millisecond ||
				!strings.Contains(err.Error(), fmt.Sprintf("%d %s slots", tc.size, tc.class)) {
				t.Fatalf("a run past a full lane: waited %v, %v; want ErrBusy after 300ms naming %d %s slots",
					res.Waited, err, tc.size, tc.class)
			}
			for _, h := range holders {
				if o := h.end(t); o.err != nil {
					t.Fatal(o.err)
				}
			}

			// A long SlotWait: a queued run takes the slot a holder hands on.
			p = fakePool(t, body, o)
			gitproc.SetTimings(p, hang, time.Second)
			for i := range holders {
				holders[i] = hold(t, p, tc.class)
			}
			queued := start(t.Context(), p, quick(tc.class))
			time.Sleep(200 * time.Millisecond) // long enough to be queued; a run admitted early passes too
			if o := holders[0].end(t); o.err != nil {
				t.Fatal(o.err)
			}
			if o := await(t, queued, "the queued run"); o.err != nil {
				t.Fatalf("the queued run, handed a slot: %v", o.err)
			}

			holders[0] = hold(t, p, tc.class) // the lane full again
			ctx, cancel := context.WithCancel(t.Context())
			waiting := start(ctx, p, quick(tc.class))
			time.Sleep(100 * time.Millisecond)
			cancel()
			if o := await(t, waiting, "the cancelled waiter"); !errors.Is(o.err, context.Canceled) || errors.Is(o.err, gitproc.ErrBusy) {
				t.Fatalf("a waiter whose caller went away: %v, want context.Canceled and not ErrBusy", o.err)
			}
			for _, h := range holders {
				if o := h.end(t); o.err != nil {
					t.Fatal(o.err)
				}
			}
		})
	}
}

// A READ IS NEVER QUEUED BEHIND TRANSFERS: slow clients holding every transfer
// slot cost the node no read.
func TestAReadIsNeverQueuedBehindTransfers(t *testing.T) {
	t.Parallel()
	p := fakePool(t, `case "$1" in hold) `+holdBody+`;; esac`, gitproc.Options{MaxProcesses: 1, MaxTransfers: 2})
	gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
	a, b := hold(t, p, gitproc.ClassTransfer), hold(t, p, gitproc.ClassTransfer)
	if _, err := p.Run(t.Context(), quick(gitproc.ClassRead)); err != nil {
		t.Fatalf("a read with every transfer slot held: %v", err)
	}
	a.end(t)
	b.end(t)
}

// MAINTENANCE IS NEVER REFUSED BEHIND TRANSFERS: the maintainer does not lose
// its turn because slow clients hold every transfer slot.
func TestMaintenanceIsNeverRefusedBehindTransfers(t *testing.T) {
	t.Parallel()
	p := fakePool(t, `case "$1" in hold) `+holdBody+`;; esac`, gitproc.Options{MaxProcesses: 1, MaxTransfers: 2})
	gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
	a, b := hold(t, p, gitproc.ClassTransfer), hold(t, p, gitproc.ClassTransfer)
	if _, err := p.Run(t.Context(), quick(gitproc.ClassMaintenance)); err != nil {
		t.Fatalf("maintenance with every transfer slot held: %v", err)
	}
	a.end(t)
	b.end(t)
}

// nested is a Stdin whose first Read waits until every transfer of the case
// has started, then runs a read through the same pool — what a receive-pack's
// hook does — and ends.
type nested struct {
	p       *gitproc.Pool
	ctx     context.Context
	barrier *sync.WaitGroup
	once    sync.Once
	err     error
}

func (n *nested) Read([]byte) (int, error) {
	n.once.Do(func() {
		n.barrier.Done()
		n.barrier.Wait()
		_, n.err = n.p.Run(n.ctx, gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	})
	if n.err != nil {
		return 0, n.err
	}
	return 0, io.EOF
}

// A TRANSFER WAITING ON A READ NEVER DEADLOCKS: every transfer slot held by a
// run whose input waits on a read, and every read still runs.
func TestATransferWaitingOnAReadNeverDeadlocks(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "cat >/dev/null", gitproc.Options{MaxProcesses: 1, MaxTransfers: 2})
	gitproc.SetTimings(p, time.Second, time.Second)
	var barrier sync.WaitGroup
	barrier.Add(2)
	var runs []<-chan outcome
	var readers []*nested
	for range 2 {
		n := &nested{p: p, ctx: t.Context(), barrier: &barrier}
		readers = append(readers, n)
		runs = append(runs, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdin: n, Class: gitproc.ClassTransfer, Deadline: time.Minute}))
	}
	for i, run := range runs {
		if o := await(t, run, "a transfer whose input runs a read"); o.err != nil || readers[i].err != nil {
			t.Fatalf("transfer %d: %v; its nested read: %v", i, o.err, readers[i].err)
		}
	}
}

// MAINTENANCE NEVER WAITS FOR A SLOT: half the cores, rounded up, a free slot
// or an immediate refusal — and a lane of its own.
func TestMaintenanceNeverWaitsForASlot(t *testing.T) {
	t.Parallel()
	body := `case "$1" in hold) ` + holdBody + `;; esac`
	p := fakePool(t, body, gitproc.Options{MaxProcesses: 4})
	gitproc.SetTimings(p, 10*time.Second, time.Second)
	a, b := hold(t, p, gitproc.ClassMaintenance), hold(t, p, gitproc.ClassMaintenance)
	res, err := p.Run(t.Context(), quick(gitproc.ClassMaintenance))
	if !errors.Is(err, gitproc.ErrBusy) || !strings.Contains(err.Error(), "at most 2 maintenance processes") {
		t.Fatalf("a third maintenance run on four cores: %v, want ErrBusy naming at most 2", err)
	}
	if res.Waited >= 5*time.Second {
		t.Fatalf("the refused maintenance run waited %v: it queued for a slot", res.Waited)
	}
	a.end(t)
	if _, err := p.Run(t.Context(), quick(gitproc.ClassMaintenance)); err != nil {
		t.Fatalf("maintenance after a holder ended: %v", err)
	}
	b.end(t)

	one := fakePool(t, body, gitproc.Options{MaxProcesses: 1})
	gitproc.SetTimings(one, 10*time.Second, time.Second)
	if _, err := one.Run(t.Context(), quick(gitproc.ClassMaintenance)); err != nil {
		t.Fatalf("maintenance on an empty one-core lane: %v", err)
	}
	c := hold(t, one, gitproc.ClassMaintenance)
	if _, err := one.Run(t.Context(), quick(gitproc.ClassMaintenance)); !errors.Is(err, gitproc.ErrBusy) {
		t.Fatalf("a second maintenance run on one core: %v, want ErrBusy", err)
	}
	c.end(t)

	four := fakePool(t, body, gitproc.Options{MaxProcesses: 4})
	gitproc.SetTimings(four, 10*time.Second, time.Second)
	var reads []holder
	for range 4 {
		reads = append(reads, hold(t, four, gitproc.ClassRead))
	}
	if _, err := four.Run(t.Context(), quick(gitproc.ClassMaintenance)); err != nil {
		t.Fatalf("maintenance with every read slot held: %v", err)
	}
	for _, r := range reads {
		r.end(t)
	}
}

// treeBody backgrounds a grandchild, records its pid and waits on it.
func treeBody(pids pidFile) string {
	return "sleep 600 & echo $! > " + pids.shell() + "; wait"
}

// A DEADLINE ENDS THE WHOLE TREE, the grandchild with the leader.
func TestADeadlineEndsTheWholeTree(t *testing.T) {
	t.Parallel()
	pids := newPIDFile(t)
	p := fakePool(t, treeBody(pids), gitproc.Options{})
	o, _, _, pid := pastItsDeadline(t, p, pids, 300*time.Millisecond)
	if !errors.Is(o.err, gitproc.ErrDeadline) || o.res.Exit != -1 {
		t.Fatalf("a run past its deadline: exit %d, %v; want ErrDeadline and a signal", o.res.Exit, o.err)
	}
	if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the grandchild %d outlived its run", pid)
	}
}

// A CANCELLED REQUEST KILLS THE WHOLE TREE: a client that hangs up takes
// everything its git started with it.
func TestACancelledRequestKillsTheWholeTree(t *testing.T) {
	t.Parallel()
	pids := newPIDFile(t)
	p := fakePool(t, treeBody(pids), gitproc.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	done := start(ctx, p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: time.Minute})
	pid := pids.read(t)
	cancel()
	if o := await(t, done, "a cancelled run"); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("a cancelled run: %v, want context.Canceled", o.err)
	}
	if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the grandchild %d outlived its cancelled run", pid)
	}
}

// A PROCESS THAT IGNORES SIGTERM IS KILLED KillGrace later — and Run returns.
func TestAProcessThatIgnoresTerminateIsKilled(t *testing.T) {
	t.Parallel()
	pids := newPIDFile(t)
	// The trap comes before the tree, so a run whose pid was written was
	// ignoring SIGTERM — leader and grandchild alike, since an ignored
	// signal stays ignored across exec — when its deadline fired.
	p := fakePool(t, "trap '' TERM; "+treeBody(pids), gitproc.Options{})
	gitproc.SetTimings(p, 10*time.Second, 300*time.Millisecond)
	o, deadline, took, pid := pastItsDeadline(t, p, pids, 200*time.Millisecond)
	if !errors.Is(o.err, gitproc.ErrDeadline) {
		t.Fatalf("a run ignoring SIGTERM: %v, want ErrDeadline", o.err)
	}
	if took-deadline >= 10*time.Second {
		t.Fatalf("Run took %v past its %v deadline to end a run with a 300ms grace", took-deadline, deadline)
	}
	if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the grandchild %d ignoring SIGTERM outlived its run", pid)
	}
}

// blockingWriter accepts its first Write and blocks every later one until
// release closes.
type blockingWriter struct {
	mu      sync.Mutex
	writes  int
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	first := w.writes == 1
	w.mu.Unlock()
	if !first {
		<-w.release
	}
	return len(p), nil
}

// A STALLED TRANSFER IS KILLED: a process that has stopped, and a CLIENT that
// has stopped reading — killed with the client's Write still blocked, and
// answered only once that Write returns.
func TestAStalledTransferIsKilled(t *testing.T) {
	t.Parallel()
	t.Run("a silent process", func(t *testing.T) {
		t.Parallel()
		p := fakePool(t, "sleep 600", gitproc.Options{})
		o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassTransfer,
			Deadline: time.Minute, Stall: 300 * time.Millisecond}), "a silent run")
		if !errors.Is(o.err, gitproc.ErrStalled) {
			t.Fatalf("a silent run: %v, want ErrStalled", o.err)
		}
	})
	t.Run("a client that stops reading", func(t *testing.T) {
		t.Parallel()
		pids := newPIDFile(t)
		p := fakePool(t, "echo $$ > "+pids.shell()+"; yes", gitproc.Options{})
		w := &blockingWriter{release: make(chan struct{})}
		done := start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdout: w, Class: gitproc.ClassTransfer,
			Deadline: time.Minute, Stall: 300 * time.Millisecond})
		pid := pids.read(t)
		if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
			t.Fatalf("the stalled process %d was not killed while its client's Write blocked", pid)
		}
		select {
		case o := <-done:
			t.Fatalf("Run returned (%v) while the client's Write was still blocked", o.err)
		default:
		}
		close(w.release)
		if o := await(t, done, "a run whose client stopped reading"); !errors.Is(o.err, gitproc.ErrStalled) {
			t.Fatalf("a run whose client stopped reading: %v, want ErrStalled", o.err)
		}
	})
}

// trickle is a Stdin giving one byte every interval for n bytes.
type trickle struct {
	n        int
	interval time.Duration
}

func (r *trickle) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.interval)
	r.n--
	p[0] = 'x'
	return 1, nil
}

// A MOVING TRANSFER IS NOT STALLED — whichever of its three streams moves:
// stdout, stderr only (a receive-pack relaying its hook's keepalives to a
// client without side-band), or stdin.
func TestAMovingTransferIsNotStalled(t *testing.T) {
	t.Parallel()
	loop := func(redirect string) string {
		return "i=0; while [ $i -lt 30 ]; do echo x" + redirect + "; sleep 0.1; i=$((i+1)); done"
	}
	for name, tc := range map[string]struct {
		body  string
		stdin io.Reader
	}{
		"stdout": {body: loop("")},
		"stderr": {body: loop(" >&2")},
		"stdin":  {body: "cat >/dev/null", stdin: &trickle{n: 30, interval: 100 * time.Millisecond}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakePool(t, tc.body, gitproc.Options{})
			var out bytes.Buffer
			o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdin: tc.stdin, Stdout: &out,
				Class: gitproc.ClassTransfer, Deadline: time.Minute, Stall: 2 * time.Second}), "a moving run")
			if o.err != nil {
				t.Fatalf("a run moving on %s every 100ms with a 2s stall: %v", name, o.err)
			}
			if name == "stdout" && strings.Count(out.String(), "x\n") != 30 {
				t.Fatalf("stdout carried %q", out.String())
			}
		})
	}
}

// NOTHING A RUN STARTED OUTLIVES IT: a backgrounded child is swept after the
// leader is reaped.
func TestNothingARunStartedOutlivesIt(t *testing.T) {
	t.Parallel()
	pids := newPIDFile(t)
	p := fakePool(t, "sleep 600 >/dev/null 2>&1 & echo $! > "+pids.shell()+"; exit 0", gitproc.Options{})
	if _, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if pid := pids.read(t); !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the backgrounded %d outlived the run that started it", pid)
	}
}

// A STRAGGLER HOLDING STDOUT DOES NOT HOLD RUN: the sweep ends it, which closes
// the pipes — the drain watch never has to.
func TestAStragglerHoldingStdoutDoesNotHoldRun(t *testing.T) {
	t.Parallel()
	pids := newPIDFile(t)
	var logs logBuffer
	p := fakePool(t, "sleep 600 & echo $! > "+pids.shell()+"; exit 0", gitproc.Options{Logger: logs.logger()})
	gitproc.SetTimings(p, 10*time.Second, 300*time.Millisecond)
	began := time.Now()
	o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: time.Minute}), "a run with a straggler")
	if o.err != nil {
		t.Fatal(o.err)
	}
	if took := time.Since(began); took >= 10*time.Second {
		t.Fatalf("Run took %v with a straggler holding its stdout", took)
	}
	if pid := pids.read(t); !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the straggler %d outlived the run", pid)
	}
	if logs.lines("code_git_straggler_killed") != 1 {
		t.Errorf("the sweep did not log the straggler it killed:\n%s", logs.String())
	}
	if n := logs.lines("code_git_pipe_held"); n != 0 {
		t.Errorf("the drain watch released %d pipes the sweep should have closed:\n%s", n, logs.String())
	}
}

// slowWriter sleeps in every Write and counts what it was given.
type slowWriter struct {
	delay time.Duration
	n     int64
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.n += int64(len(p))
	return len(p), nil
}

// A SLOW READER RECEIVES EVERY BYTE, though the process exited long before the
// last Write — a Write slower than KillGrace, so a watch that ignored where
// the copier was blocked would cut it.
func TestASlowReaderReceivesEveryByte(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "head -c 1048576 /dev/zero; exit 0", gitproc.Options{})
	gitproc.SetTimings(p, 10*time.Second, 300*time.Millisecond)
	w := &slowWriter{delay: 500 * time.Millisecond}
	o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdout: w, Class: gitproc.ClassTransfer, Deadline: time.Minute}), "a run with a slow reader")
	if o.err != nil || w.n != 1<<20 {
		t.Fatalf("a slow reader got %d of %d bytes: %v", w.n, 1<<20, o.err)
	}
}

// AN ESCAPED DESCENDANT DOES NOT HOLD RUN: a process that left the group (out
// of the sweep's reach) and holds stdout and stderr open has both pipes closed
// by the drain watch once they sit empty for KillGrace — and says so.
func TestAnEscapedDescendantDoesNotHoldRun(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "escaped")
	pids := newPIDFile(t)
	var logs logBuffer
	p := fakePool(t, escapeEnv+"="+quote(ready)+" "+quote(self)+" & echo $! > "+pids.shell()+
		"; while [ ! -e "+quote(ready)+" ]; do sleep 0.01; done; exit 0", gitproc.Options{Logger: logs.logger()})
	gitproc.SetTimings(p, 10*time.Second, 300*time.Millisecond)
	done := start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: time.Minute})
	pid := pids.read(t)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if o := await(t, done, "a run whose descendant escaped its group"); o.err != nil {
		t.Fatal(o.err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if logs.lines("code_git_pipe_held", "stream="+stream) != 1 {
			t.Errorf("the drain watch did not report releasing %s:\n%s", stream, logs.String())
		}
	}
}

// blockedReader blocks its first Read until release closes, then ends.
type blockedReader struct{ release chan struct{} }

func (r *blockedReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// RUN RETURNS ONLY AFTER ITS INPUT ENDS — and still kills on time: the process
// is reaped long before the caller's Read returns, and Run waits for that Read
// rather than leave a goroutine inside it.
func TestRunReturnsOnlyAfterItsInputEnds(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "sleep 600", gitproc.Options{})
	in := &blockedReader{release: make(chan struct{})}
	began := time.Now()
	time.AfterFunc(3*time.Second, func() { close(in.release) })
	o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdin: in, Class: gitproc.ClassTransfer,
		Deadline: time.Minute, Stall: 200 * time.Millisecond}), "a run whose input blocks")
	returned := time.Since(began)
	if !errors.Is(o.err, gitproc.ErrStalled) {
		t.Fatalf("a stalled run with a blocked input: %v, want ErrStalled", o.err)
	}
	if o.res.Took >= 3*time.Second {
		t.Errorf("the process was reaped after %v: it was not killed before its input ended", o.res.Took)
	}
	if returned < 3*time.Second {
		t.Errorf("Run returned after %v, before its input's Read did", returned)
	}
}

var errBoom = errors.New("boom")

// failingReader gives some bytes, then fails with err (errBoom when nil).
type failingReader struct {
	given bool
	err   error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.given {
		r.given = true
		return copy(p, "0123456789"), nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, errBoom
}

// silentReader answers every Read with nothing and no error.
type silentReader struct{}

func (silentReader) Read([]byte) (int, error) { return 0, nil }

// THE CALLER SEES ITS OWN INPUT ERROR — and only io.EOF ITSELF is the end of
// what git was given (objstore.Fill's rule): a body cut short answers
// io.ErrUnexpectedEOF, and a reader wrapping io.EOF is saying something
// io.EOF does not, so each is the caller's failure, and a reader answering
// nothing a hundred times running is stuck (io.ErrNoProgress, bufio's
// judgement) rather than spun on — never a whole input handed to git.
func TestTheCallerSeesItsOwnInputError(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "cat >/dev/null", gitproc.Options{})
	for name, tc := range map[string]struct {
		stdin io.Reader
		want  error
	}{
		"a failure":               {stdin: &failingReader{}, want: errBoom},
		"a body cut short":        {stdin: &failingReader{err: io.ErrUnexpectedEOF}, want: io.ErrUnexpectedEOF},
		"a wrapped end of file":   {stdin: &failingReader{err: fmt.Errorf("the client hung up: %w", io.EOF)}, want: io.EOF},
		"a reader giving nothing": {stdin: silentReader{}, want: io.ErrNoProgress},
	} {
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Stdin: tc.stdin, Class: gitproc.ClassTransfer, Deadline: time.Minute})
		if !errors.Is(err, gitproc.ErrInput) || !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want ErrInput wrapping %v", name, err, tc.want)
		}
	}
	if _, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x"}, Stdin: strings.NewReader("0123456789"), Class: gitproc.ClassTransfer, Deadline: time.Minute}); err != nil {
		t.Fatalf("a reader ending in io.EOF itself: %v, want the whole input given", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBoom }

// THE CALLER SEES ITS OWN OUTPUT ERROR — and the run ends at once: the pipe is
// closed under git, which would otherwise block on a full pipe until its
// deadline.
func TestTheCallerSeesItsOwnOutputError(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "yes", gitproc.Options{})
	o := await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Stdout: failingWriter{}, Class: gitproc.ClassTransfer, Deadline: time.Minute}),
		"a run whose output failed")
	if !errors.Is(o.err, gitproc.ErrOutput) || !errors.Is(o.err, errBoom) {
		t.Fatalf("a run whose output failed: %v, want ErrOutput wrapping the writer's error", o.err)
	}
	if o.res.Took >= 30*time.Second {
		t.Fatalf("yes ran %v after its reader failed", o.res.Took)
	}
}

// A CAPTURE PAST ITS BOUND IS AN ERROR, never a short capture returned whole.
func TestACapturePastItsBoundIsAnError(t *testing.T) {
	t.Parallel()
	p := fakePool(t, `head -c "$2" /dev/zero`, gitproc.Options{})
	gitproc.SetCaptureMax(p, 64)
	_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x", "100"}, Class: gitproc.ClassRead, Deadline: time.Minute})
	if !errors.Is(err, gitproc.ErrOutput) || !errors.Is(err, gitproc.ErrCaptureFull) {
		t.Fatalf("100 bytes into a 64-byte capture: %v, want ErrOutput and ErrCaptureFull", err)
	}
	var out bytes.Buffer
	if _, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x", "100"}, Stdout: &out, Class: gitproc.ClassRead, Deadline: time.Minute}); err != nil || out.Len() != 100 {
		t.Fatalf("100 bytes into a caller's Stdout: %d, %v", out.Len(), err)
	}
	res, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"x", "64"}, Class: gitproc.ClassRead, Deadline: time.Minute})
	if err != nil || len(res.Stdout) != 64 {
		t.Fatalf("64 bytes into a 64-byte capture: %d, %v", len(res.Stdout), err)
	}
}

// AN ANSWER IS NOT A FAILURE: merge-base --is-ancestor's "no" is exit 1.
func TestAnAnswerIsNotAFailure(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	wt := c.Init(t)
	base := c.Commit(t, wt, map[string]string{"a": "1\n"}, "base")
	tip := c.Commit(t, wt, map[string]string{"a": "2\n"}, "tip")
	bare := gittest.Empty(t)
	gittest.Pack(t, c, bare, wt, tip)
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": tip})
	p := realPool(t, gitproc.Options{})
	cmd := gitproc.Cmd{Dir: bare, Args: []string{"merge-base", "--is-ancestor", tip, base}, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline, Answers: []int{1}}
	res, err := p.Run(t.Context(), cmd)
	if err != nil || res.Exit != 1 {
		t.Fatalf("is the tip an ancestor of its base, as an answer: exit %d, %v", res.Exit, err)
	}
	cmd.Answers = nil
	_, err = p.Run(t.Context(), cmd)
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) || !errors.Is(err, gitproc.ErrFailed) || exit.Command != "merge-base" {
		t.Fatalf("the same without the answer: %v, want an ExitError of kind ErrFailed", err)
	}
}

// THE GAUGES COUNT RUNNING PROCESSES, and every admission — refused or not —
// is one wait observation.
func TestTheGaugesCountRunningProcesses(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	p := fakePool(t, `case "$1" in hold) `+holdBody+`;; esac`, gitproc.Options{MaxTransfers: 2, Metrics: rec})
	gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
	a, b := hold(t, p, gitproc.ClassTransfer), hold(t, p, gitproc.ClassTransfer)
	awaitGauge(t, rec, metrics.CodeGitProcesses, "transfer", 2)
	if _, err := p.Run(t.Context(), quick(gitproc.ClassTransfer)); !errors.Is(err, gitproc.ErrBusy) {
		t.Fatalf("a third transfer: %v, want ErrBusy", err)
	}
	a.end(t)
	b.end(t)
	if got := reading(rec, metrics.CodeGitProcesses, "transfer").Value; got != 0 {
		t.Fatalf("with every transfer ended the gauge reads %v", got)
	}
	if got := reading(rec, metrics.CodeGitWait, "transfer").Count; got != 3 {
		t.Fatalf("%d transfer wait observations, want 3 (two admitted, one refused)", got)
	}
}

// reading is one series of the recorder.
func reading(rec *metrics.Recorder, name, class string) metrics.Snapshot {
	for _, s := range rec.Read() {
		if s.Name == name && s.Attrs["class"] == class {
			return s
		}
	}
	return metrics.Snapshot{}
}
