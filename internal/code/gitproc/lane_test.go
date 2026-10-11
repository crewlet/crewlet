package gitproc_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// loanBody is a stand-in whose receive-pack writes its environment to "$2"
// and holds until "$3" exists, and whose other commands hold the same way —
// so a case can read the pack.threads a receive-pack started with while it
// still runs, beside the reads it holds.
const loanBody = `case "$1" in
receive-pack) env > "$2.tmp"; mv "$2.tmp" "$2"; while [ ! -e "$3" ]; do sleep 0.05; done;;
hold) ` + holdBody + `;;
esac`

// receive is one holding receive-pack.
type receive struct {
	holder
	env string
}

// startReceive starts a receive-pack that holds, and waits until it runs.
func startReceive(t *testing.T, p *gitproc.Pool) receive {
	t.Helper()
	return startIndexing(t, p, nil)
}

// startIndexing is startReceive with a Cmd.Indexed the case closes.
func startIndexing(t *testing.T, p *gitproc.Pool, indexed <-chan struct{}) receive {
	t.Helper()
	dir := t.TempDir()
	r := receive{holder: holder{started: filepath.Join(dir, "env"), release: filepath.Join(dir, "release")}}
	r.env = r.started
	r.done = start(t.Context(), p, gitproc.Cmd{Args: []string{"receive-pack", r.env, r.release}, Class: gitproc.ClassTransfer,
		Deadline: time.Minute, Indexed: indexed})
	deadline := time.Now().Add(hang)
	for {
		if _, err := os.Stat(r.env); err == nil {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("a receive-pack had not started after %v", hang)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// threads is the pack.threads the receive-pack started with.
func (r receive) threads(t *testing.T) int {
	t.Helper()
	body, err := os.ReadFile(r.env)
	if err != nil {
		t.Fatal(err)
	}
	return packThreads(t, envOf(body))
}

func packThreads(t *testing.T, env map[string]string) int {
	t.Helper()
	found := -1
	for _, kv := range configOf(t, env) {
		if kv.Key != "pack.threads" {
			continue
		}
		if found >= 0 {
			t.Fatalf("pack.threads is set twice: %v", configOf(t, env))
		}
		n, err := strconv.Atoi(kv.Value)
		if err != nil {
			t.Fatalf("pack.threads %q", kv.Value)
		}
		found = n
	}
	if found < 0 {
		t.Fatal("no pack.threads in the process's configuration")
	}
	return found
}

// notOversubscribed fails t when the read lane's held slots and loans claim
// more cores than it has — or, with any loan out, claim them all: a loan
// always leaves the core its borrower's own thread runs on. (Every case
// calling it holds its reads BEFORE the loans; a read arriving after a loan
// is admitted whatever the loans, which is TestALoanNeverMakesAReadWait.)
func notOversubscribed(t *testing.T, p *gitproc.Pool) {
	t.Helper()
	held, lent := gitproc.ReadLane(p)
	size := int64(p.Slots(gitproc.ClassRead))
	if held+lent > size || (lent > 0 && held+lent >= size) {
		t.Fatalf("the read lane claims %d held and %d lent of %d cores", held, lent, size)
	}
}

// A RECEIVE-PACK IS LENT THE READ LANE'S FREE CORES LESS ITS OWN, at most
// three: on an idle four-core node its index-pack gets four threads — one a
// core — beside held reads it gets what they leave less the core its own
// thread needs, and on a one-core node or a full lane it runs on one core
// exactly as before.
func TestAReceivePackIsLentFreeCores(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cores, reads, want int
	}{
		{cores: 8, reads: 0, want: 1 + gitproc.MaxLentCores},
		{cores: 4, reads: 0, want: 4},
		{cores: 4, reads: 2, want: 2},
		{cores: 4, reads: 3, want: 1},
		{cores: 4, reads: 4, want: 1},
		{cores: 2, reads: 0, want: 2},
		{cores: 1, reads: 0, want: 1},
		{cores: 1, reads: 1, want: 1},
	} {
		p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: tc.cores})
		var reads []holder
		for range tc.reads {
			reads = append(reads, hold(t, p, gitproc.ClassRead))
		}
		r := startReceive(t, p)
		if got := r.threads(t); got != tc.want {
			t.Errorf("%d cores, %d reads held: a receive-pack started with %d threads, want %d", tc.cores, tc.reads, got, tc.want)
		}
		notOversubscribed(t, p)
		r.end(t)
		for _, h := range reads {
			h.end(t)
		}
		if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
			t.Fatalf("after every run ended the read lane holds %d and lends %d", held, lent)
		}
	}
}

// LOANS NEVER CLAIM A CORE TWICE: concurrent receive-packs are lent what the
// earlier loans left, and the lane's held slots and loans never exceed its
// size.
func TestLoansNeverExceedTheFreeCores(t *testing.T) {
	t.Parallel()
	p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: 8})
	var receives []receive
	for _, want := range []int{4, 4, 2, 1} {
		r := startReceive(t, p)
		receives = append(receives, r)
		if got := r.threads(t); got != want {
			t.Errorf("receive-pack %d started with %d threads, want %d", len(receives), got, want)
		}
		notOversubscribed(t, p)
	}
	receives[0].end(t)
	r := startReceive(t, p) // the first one's three cores are free again
	if got := r.threads(t); got != 4 {
		t.Errorf("after the first loan was repaid a receive-pack started with %d threads, want 4", got)
	}
	notOversubscribed(t, p)
	for _, r := range append(receives[1:], r) {
		r.end(t)
	}
}

// A LOAN NEVER MAKES A READ WAIT — neither reads that arrive after it, which
// may take every core it was sized by, nor the receive-pack's own hook reads,
// which would otherwise queue for cores their own receive-pack holds idle.
// Every slot wait here is 300 ms, so a read that waited is ErrBusy.
func TestALoanNeverMakesAReadWait(t *testing.T) {
	t.Parallel()
	p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: 2})
	gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
	r := startReceive(t, p)
	if got := r.threads(t); got != 2 {
		t.Fatalf("an idle two-core node: a receive-pack started with %d threads, want 2", got)
	}
	a, b := hold(t, p, gitproc.ClassRead), hold(t, p, gitproc.ClassRead) // hold fails on ErrBusy
	a.end(t)
	b.end(t)
	r.end(t)

	// The receive-pack's own read — what its proc-receive hook does — run
	// while its loan is out and another read holds the core the loan left.
	hookPool := fakePool(t, `case "$1" in receive-pack) cat >/dev/null;; hold) `+holdBody+`;; esac`, gitproc.Options{MaxProcesses: 2})
	gitproc.SetTimings(hookPool, 300*time.Millisecond, time.Second)
	var barrier sync.WaitGroup
	barrier.Add(2)
	hook := &nested{p: hookPool, ctx: t.Context(), barrier: &barrier}
	push := start(t.Context(), hookPool, gitproc.Cmd{Args: []string{"receive-pack"}, Stdin: hook, Class: gitproc.ClassTransfer, Deadline: time.Minute})
	awaitLent(t, hookPool, 1)
	other := hold(t, hookPool, gitproc.ClassRead)
	barrier.Done() // the hook's read now runs, beside the loan and the other read
	if o := await(t, push, "a receive-pack whose hook reads"); o.err != nil || hook.err != nil {
		t.Fatalf("a receive-pack lent a core: %v; its own read beside another: %v", o.err, hook.err)
	}
	other.end(t)
}

// awaitLent waits, up to the hang detector, until the read lane lends want
// cores: a loan is made once the run is admitted, a moment after it begins.
func awaitLent(t *testing.T, p *gitproc.Pool, want int64) {
	t.Helper()
	deadline := time.Now().Add(hang)
	for {
		_, lent := gitproc.ReadLane(p)
		if lent == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read lane lends %d cores after %v, want %d", lent, hang, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A LOAN IS REPAID ONCE ITS INDEX-PACK IS DONE, not when its receive-pack
// exits: closing Cmd.Indexed hands the cores back while the push runs on
// through its hook, its upload and the wait for its record, so the next push
// is lent them — and the lent-cores gauge says so at each step.
func TestALoanIsRepaidOnceItsIndexPackIsDone(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: 4, Metrics: rec})
	lentGauge := func() float64 {
		for _, s := range rec.Read() {
			if s.Name == metrics.CodeGitLentCores {
				return s.Value
			}
		}
		return -1
	}

	indexed := make(chan struct{})
	first := startIndexing(t, p, indexed)
	if got := first.threads(t); got != 4 {
		t.Fatalf("an idle four-core node: a receive-pack started with %d threads, want 4", got)
	}
	if got := lentGauge(); got != 3 {
		t.Fatalf("with one push lent three cores the gauge reads %v", got)
	}
	behind := startReceive(t, p)
	if got := behind.threads(t); got != 1 {
		t.Fatalf("a push beside a loan of three cores of four started with %d threads, want 1", got)
	}

	close(indexed) // the first push's index-pack is done; it runs on
	// The run's watcher repays the loan and Sets the gauge only after the
	// lane has the cores back, so it is the GAUGE that is polled: the lane
	// read after it follows the whole repayment rather than racing its
	// second half.
	awaitGauge(t, rec, metrics.CodeGitLentCores, "", 0)
	if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
		t.Fatalf("with the loan repaid the read lane holds %d and lends %d", held, lent)
	}
	next := startReceive(t, p)
	if got := next.threads(t); got != 4 {
		t.Fatalf("a push after the first one's index-pack finished started with %d threads, want 4", got)
	}
	for _, r := range []receive{first, behind, next} {
		if o := r.end(t); o.err != nil {
			t.Fatal(o.err)
		}
	}
	if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
		t.Fatalf("after every push ended the read lane holds %d and lends %d", held, lent)
	}
}

// ONLY A RECEIVE-PACK IN THE TRANSFER LANE IS LENT: upload-pack's
// pack-objects holds its delta window per thread, and a read or maintenance
// run is a core by definition.
func TestOnlyAReceivePackIsLent(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "exec env", gitproc.Options{MaxProcesses: 8})
	for _, c := range []gitproc.Cmd{
		{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer},
		{Args: []string{"receive-pack"}, Class: gitproc.ClassRead},
		{Args: []string{"receive-pack"}, Class: gitproc.ClassMaintenance},
		{Args: []string{"index-pack"}, Class: gitproc.ClassRead},
	} {
		c.Deadline = time.Minute
		res, err := p.Run(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got := packThreads(t, envOf(res.Stdout)); got != 1 {
			t.Errorf("%s in the %s lane started with %d threads, want 1", c.Args[0], c.Class, got)
		}
	}
}

// EVERY LOAN IS REPAID, on every way a receive-pack can end: its exit, its
// deadline, its caller going away, its input or output failing, and a process
// that never started.
func TestEveryLoanIsRepaid(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		cmd  func(ctx context.Context) gitproc.Cmd
		ends func(ctx context.Context, cancel context.CancelFunc)
		want func(error) bool
	}{
		"its exit": {
			body: "exit 0",
			want: func(err error) bool { return err == nil },
		},
		"a failure": {
			body: "exit 1",
			want: func(err error) bool { return errors.Is(err, gitproc.ErrFailed) },
		},
		"its deadline": {
			body: "sleep 600",
			cmd: func(context.Context) gitproc.Cmd {
				return gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: 200 * time.Millisecond}
			},
			want: func(err error) bool { return errors.Is(err, gitproc.ErrDeadline) },
		},
		"its caller": {
			body: "sleep 600",
			ends: func(_ context.Context, cancel context.CancelFunc) { time.AfterFunc(200*time.Millisecond, cancel) },
			want: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		"its input": {
			body: "cat >/dev/null",
			cmd: func(context.Context) gitproc.Cmd {
				return gitproc.Cmd{Args: []string{"receive-pack"}, Stdin: &failingReader{}, Class: gitproc.ClassTransfer, Deadline: time.Minute}
			},
			want: func(err error) bool { return errors.Is(err, gitproc.ErrInput) },
		},
		"its output": {
			body: "yes",
			cmd: func(context.Context) gitproc.Cmd {
				return gitproc.Cmd{Args: []string{"receive-pack"}, Stdout: failingWriter{}, Class: gitproc.ClassTransfer, Deadline: time.Minute}
			},
			want: func(err error) bool { return errors.Is(err, gitproc.ErrOutput) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakePool(t, tc.body, gitproc.Options{MaxProcesses: 4})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: time.Minute}
			if tc.cmd != nil {
				c = tc.cmd(ctx)
			}
			if tc.ends != nil {
				tc.ends(ctx, cancel)
			}
			o := await(t, start(ctx, p, c), "a receive-pack")
			if !tc.want(o.err) {
				t.Fatalf("the receive-pack answered %v", o.err)
			}
			if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
				t.Fatalf("after it ended the read lane holds %d and lends %d", held, lent)
			}
		})
	}

	t.Run("a process that never started", func(t *testing.T) {
		t.Parallel()
		fake := gittest.Fake(t, fakeVersion, "exit 0")
		p := newPool(t, gitproc.Options{Path: fake, MaxProcesses: 4})
		if err := os.Remove(fake); err != nil {
			t.Fatal(err)
		}
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: time.Minute})
		if err == nil {
			t.Fatal("a receive-pack whose binary is gone started")
		}
		if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
			t.Fatalf("after it failed to start the read lane holds %d and lends %d", held, lent)
		}
	})
}
