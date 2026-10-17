package gitproc_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// loanBody is a stand-in whose receive-pack, past its options, writes its
// environment to its first path and holds until its second exists, and whose
// other commands hold the same way — so a case can read the pack.threads a
// receive-pack started with while it still runs, beside the reads it holds.
const loanBody = `case "$1" in
receive-pack) shift; while [ "${1#-}" != "$1" ]; do shift; done
	env > "$1.tmp"; mv "$1.tmp" "$1"; while [ ! -e "$2" ]; do sleep 0.05; done;;
hold) ` + holdBody + `;;
esac`

// receive is one holding receive-pack.
type receive struct {
	holder
	env string
}

// toExit is a Cmd.Indexed nobody closes: its receive-pack holds its loan to
// its exit, the latest a loan is ever repaid.
func toExit() <-chan struct{} { return make(chan struct{}) }

// startReceive starts a receive-pack that holds, and holds its loan, and
// waits until it runs.
func startReceive(t *testing.T, p *gitproc.Pool) receive {
	t.Helper()
	return startReceivePack(t, p, []string{"--stateless-rpc"}, toExit())
}

// startIndexing is startReceive with a Cmd.Indexed the case closes.
func startIndexing(t *testing.T, p *gitproc.Pool, indexed <-chan struct{}) receive {
	t.Helper()
	return startReceivePack(t, p, []string{"--stateless-rpc"}, indexed)
}

// startAdvertisement starts a push's advertisement — the receive-pack the
// host runs for a push's GET, as it runs it — that holds.
func startAdvertisement(t *testing.T, p *gitproc.Pool) receive {
	t.Helper()
	return startReceivePack(t, p, []string{"--stateless-rpc", "--advertise-refs"}, nil)
}

// startReceivePack starts a holding receive-pack in the transfer lane with
// the options and Cmd.Indexed given, and waits until it runs.
func startReceivePack(t *testing.T, p *gitproc.Pool, options []string, indexed <-chan struct{}) receive {
	t.Helper()
	dir := t.TempDir()
	r := receive{holder: holder{started: filepath.Join(dir, "env"), release: filepath.Join(dir, "release")}}
	r.env = r.started
	args := append(append([]string{"receive-pack"}, options...), r.env, r.release)
	r.done = start(t.Context(), p, gitproc.Cmd{Args: args, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: indexed})
	deadline := time.Now().Add(hang)
	for {
		if _, err := os.Stat(r.env); err == nil {
			return r
		}
		select {
		case o := <-r.done:
			t.Fatalf("%v ended before it ran: %v", args[:1+len(options)], o.err)
		default:
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

// FREE IS WHAT A RUN COULD TAKE WITHOUT WAITING: a lane's slots less those
// held and, in the read lane, those lent to a push's receive-pack — so a
// caller sizing independent reads by it, as a push's force check does, takes
// only the cores nothing else claims and never queues behind itself.
func TestFreeIsWhatNoRunAndNoLoanClaims(t *testing.T) {
	t.Parallel()
	p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: 4, MaxTransfers: 2})
	if got := p.Free(gitproc.ClassRead); got != 4 {
		t.Fatalf("an idle four-core pool has %d read slots free, want 4", got)
	}
	h := hold(t, p, gitproc.ClassRead)
	if got := p.Free(gitproc.ClassRead); got != 3 {
		t.Errorf("with one read held %d read slots are free, want 3", got)
	}
	r := startReceive(t, p) // lent the two cores the read and its own thread leave
	if got := p.Free(gitproc.ClassRead); got != 1 {
		t.Errorf("with one read held and two cores lent %d read slots are free, want 1", got)
	}
	if got := p.Free(gitproc.ClassTransfer); got != 1 {
		t.Errorf("with one receive-pack running %d of two transfer slots are free, want 1", got)
	}
	if got := p.Free(gitproc.ClassMaintenance); got != 2 {
		t.Errorf("an idle maintenance lane of two has %d free", got)
	}
	r.end(t)
	h.end(t)
	if got := p.Free(gitproc.ClassRead); got != 4 {
		t.Errorf("after every run ended %d read slots are free, want 4", got)
	}
	if got := p.Free("install"); got != 0 {
		t.Errorf("a class the pool does not have has %d slots free", got)
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
	push := start(t.Context(), hookPool, gitproc.Cmd{Args: []string{"receive-pack"}, Stdin: hook, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit()})
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
	rec := newRecorder(t)
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

// ONLY A RECEIVE-PACK THAT RECEIVES A PACK, IN THE TRANSFER LANE, IS LENT:
// upload-pack's pack-objects holds its delta window per thread, a push's
// advertisement — by either full name of its option, wherever it stands —
// starts no index-pack, and a read or maintenance run is a core by
// definition. Each runs without a Cmd.Indexed and starts at one thread on an
// idle eight-core node, where the push run last — the control — is lent three
// cores.
func TestOnlyAReceivePackIsLent(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "exec env", gitproc.Options{MaxProcesses: 8})
	for _, tc := range []struct {
		cmd  gitproc.Cmd
		want int
	}{
		{cmd: gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack", "--stateless-rpc", "--advertise-refs", "/r"}, Class: gitproc.ClassTransfer}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack", "--stateless-rpc", "--http-backend-info-refs", "/r"}, Class: gitproc.ClassTransfer}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack", "/r", "--advertise-refs"}, Class: gitproc.ClassTransfer}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassRead}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassMaintenance}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"index-pack"}, Class: gitproc.ClassRead}, want: 1},
		{cmd: gitproc.Cmd{Args: []string{"receive-pack", "--stateless-rpc", "/r"}, Class: gitproc.ClassTransfer, Indexed: toExit()},
			want: 1 + gitproc.MaxLentCores},
	} {
		tc.cmd.Deadline = time.Minute
		res, err := p.Run(t.Context(), tc.cmd)
		if err != nil {
			t.Fatal(err)
		}
		if got := packThreads(t, envOf(res.Stdout)); got != tc.want {
			t.Errorf("git %v in the %s lane started with %d threads, want %d", tc.cmd.Args, tc.cmd.Class, got, tc.want)
		}
	}
}

// A PUSH'S ADVERTISEMENT IS NEVER LENT. Every push a client makes sends a GET
// before its POST, and the host answers it with receive-pack
// --advertise-refs, which writes the refs and exits: no pack arrives, no
// index-pack runs, no hook reaches a relay. So it carries no Cmd.Indexed,
// starts at one thread and claims no core — and a push started beside it, as
// the next client's POST is, is lent everything an idle node lends, where a
// lent advertisement would hold three cores of four for as long as its client
// takes to read the refs.
func TestAnAdvertisementIsNeverLent(t *testing.T) {
	t.Parallel()
	p := fakePool(t, loanBody, gitproc.Options{MaxProcesses: 4})
	adv := startAdvertisement(t, p)
	if got := adv.threads(t); got != 1 {
		t.Fatalf("a push's advertisement started with %d threads, want 1", got)
	}
	if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
		t.Fatalf("with an advertisement running the read lane holds %d and lends %d", held, lent)
	}
	push := startReceive(t, p)
	if got := push.threads(t); got != 4 {
		t.Fatalf("a push beside an advertisement on an idle four-core node started with %d threads, want 4", got)
	}
	for _, r := range []receive{adv, push} {
		if o := r.end(t); o.err != nil {
			t.Fatal(o.err)
		}
	}
}

// A RECEIVE-PACK IS READ AS GIT READS IT: every form the pool takes for a
// push's advertisement, the real git answers with the refs and nothing more,
// and every form it takes for a push, the real git answers by waiting for the
// commands a push sends (here, finding none: "the remote end hung up") and
// writing no advertisement — the host's own two calls first among them. So
// whether a receive-pack is lent cores, and must carry Cmd.Indexed, is
// decided by what git will do with it rather than by a spelling the pool
// hoped for.
func TestAReceivePackIsReadAsGitReadsIt(t *testing.T) {
	t.Parallel()
	p := realPool(t, gitproc.Options{})
	repo := gittest.Empty(t)
	for _, args := range [][]string{
		{"--stateless-rpc", "--advertise-refs", repo}, // the host's advertisement
		{"--stateless-rpc", "--http-backend-info-refs", repo},
		{repo, "--advertise-refs", "--stateless-rpc"},
		{"--advertise-refs", repo},
	} {
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: repo, Args: append([]string{"receive-pack"}, args...),
			Class: gitproc.ClassTransfer, Deadline: time.Minute})
		if err != nil || !strings.Contains(string(res.Stdout), "capabilities^{}") {
			t.Errorf("git receive-pack %v, taken for an advertisement: %q, stderr %q, %v; want the refs advertised",
				args, res.Stdout, res.Stderr, err)
		}
	}
	for _, args := range [][]string{
		{"--stateless-rpc", repo}, // the host's push
		{repo, "--stateless-rpc"},
	} {
		res, err := p.Run(t.Context(), gitproc.Cmd{Dir: repo, Args: append([]string{"receive-pack"}, args...),
			Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit(), Answers: []int{128}})
		if err != nil || len(res.Stdout) != 0 || !strings.Contains(res.Stderr, "the remote end hung up") {
			t.Errorf("git receive-pack %v, taken for a push: %q, stderr %q, %v; want it waiting for a push's commands, advertising nothing",
				args, res.Stdout, res.Stderr, err)
		}
	}
}

// EVERY LOAN IS REPAID, on every way a receive-pack can end: its exit, its
// deadline, its caller going away, its input or output failing, and a process
// that never started — each with a Cmd.Indexed that never closes, as a push
// whose pack is refused before its hook runs has. Each case proves first that
// a loan WAS made (the gauge reached three on an idle four-core node), so a
// run refused before its admission, which lends nothing and so leaves nothing
// to repay, can never pass for one that repaid.
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
				return gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: 200 * time.Millisecond, Indexed: toExit()}
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
				return gitproc.Cmd{Args: []string{"receive-pack"}, Stdin: &failingReader{}, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit()}
			},
			want: func(err error) bool { return errors.Is(err, gitproc.ErrInput) },
		},
		"its output": {
			body: "yes",
			cmd: func(context.Context) gitproc.Cmd {
				return gitproc.Cmd{Args: []string{"receive-pack"}, Stdout: failingWriter{}, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit()}
			},
			want: func(err error) bool { return errors.Is(err, gitproc.ErrOutput) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t)
			p := fakePool(t, tc.body, gitproc.Options{MaxProcesses: 4, Metrics: rec})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit()}
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
			lentAndRepaid(t, p, rec)
		})
	}

	t.Run("a process that never started", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder(t)
		fake := gittest.Fake(t, fakeVersion, "exit 0")
		p := newPool(t, gitproc.Options{Path: fake, MaxProcesses: 4, Metrics: rec})
		if err := os.Remove(fake); err != nil {
			t.Fatal(err)
		}
		_, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"receive-pack"}, Class: gitproc.ClassTransfer, Deadline: time.Minute, Indexed: toExit()})
		// The START's failure, and only that: a refusal (ErrInvalid) comes
		// before any loan, and would prove nothing about repaying one.
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, gitproc.ErrInvalid) || !strings.Contains(err.Error(), "starting git receive-pack") {
			t.Fatalf("a receive-pack whose binary is gone answered %v, want its start's failure", err)
		}
		lentAndRepaid(t, p, rec)
	})
}

// newRecorder is a metrics recorder of the case's own.
func newRecorder(t *testing.T) *metrics.Recorder {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// lentAndRepaid fails t unless, on a pool of four idle cores whose one run
// has returned, a loan of three cores was made — the lent-cores gauge's peak
// over the window — and every core is back: the gauge reads zero now and the
// read lane holds and lends nothing. Run repays before it returns, so neither
// reading is polled.
func lentAndRepaid(t *testing.T, p *gitproc.Pool, rec *metrics.Recorder) {
	t.Helper()
	peak := -1.0
	for _, s := range rec.ReadWindow() {
		if s.Name == metrics.CodeGitLentCores {
			peak = s.Value
		}
	}
	if peak != gitproc.MaxLentCores {
		t.Fatalf("the lent-cores gauge peaked at %v, want %d: no loan was made, so its repayment proves nothing", peak, gitproc.MaxLentCores)
	}
	if now := reading(rec, metrics.CodeGitLentCores, ""); now.Name != metrics.CodeGitLentCores || now.Value != 0 {
		t.Fatalf("after the run returned the lent-cores gauge reads %v, want 0", now.Value)
	}
	if held, lent := gitproc.ReadLane(p); held != 0 || lent != 0 {
		t.Fatalf("after the run returned the read lane holds %d and lends %d", held, lent)
	}
}

// A HELD READ SLOT IS A READ'S. Pool.HoldRead admits in-process work as a
// read's run is admitted: with the read lane's one slot held, a read run
// waits and is refused ErrBusy after SlotWait; a hold released hands its slot
// to the run queued first, ahead of a hold asked after it; a hold its caller
// gives up waiting for is that, never a busy node; and the running gauge
// counts a hold while it is held. So work a request waits on — the cache's
// hash of a pack a hint names — runs on a core the read lane counts, never
// beside every read on cores it counts already.
//
// Mutations: HoldRead takes no slot — the run is admitted at once; a hold
// jumps the queue (a waiter queued at the front) — the run queued before it
// is refused busy.
func TestHoldReadIsAReadSlot(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	p := fakePool(t, "", gitproc.Options{MaxProcesses: 1, Metrics: rec})
	gitproc.SetTimings(p, 300*time.Millisecond, time.Second)
	if p.Slots(gitproc.ClassRead) != 1 {
		t.Fatalf("the read lane has %d slots, want 1", p.Slots(gitproc.ClassRead))
	}

	release, err := p.HoldRead(t.Context())
	if err != nil {
		t.Fatalf("a hold of an idle lane: %v", err)
	}
	awaitGauge(t, rec, metrics.CodeGitProcesses, string(gitproc.ClassRead), 1)
	res, err := p.Run(t.Context(), quick(gitproc.ClassRead))
	if !errors.Is(err, gitproc.ErrBusy) || res.Waited < 300*time.Millisecond {
		t.Fatalf("a read past a held slot: waited %v, %v; want ErrBusy after 300ms", res.Waited, err)
	}
	if _, err := p.HoldRead(t.Context()); !errors.Is(err, gitproc.ErrBusy) {
		t.Fatalf("a second hold of a held lane: %v, want ErrBusy", err)
	}
	release()
	release() // once, however often it is called
	if held, _ := gitproc.ReadLane(p); held != 0 {
		t.Fatalf("after the hold's release the read lane holds %d slots", held)
	}
	awaitGauge(t, rec, metrics.CodeGitProcesses, string(gitproc.ClassRead), 0)

	// In arrival order: a run queued behind a hold takes its slot ahead of a
	// hold asked after it.
	gitproc.SetTimings(p, hang, time.Second)
	first, err := p.HoldRead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	queued := start(t.Context(), p, quick(gitproc.ClassRead))
	time.Sleep(100 * time.Millisecond) // long enough to be queued; a run admitted early fails above
	second := make(chan func(), 1)
	go func() {
		r, err := p.HoldRead(t.Context())
		if err != nil {
			t.Errorf("the hold asked after the queued run: %v", err)
			r = func() {}
		}
		second <- r
	}()
	time.Sleep(100 * time.Millisecond)
	first()
	if o := await(t, queued, "the run queued ahead of a hold"); o.err != nil {
		t.Fatalf("the queued run, handed the slot: %v", o.err)
	}
	select {
	case r := <-second:
		r()
	case <-time.After(hang):
		t.Fatalf("the hold asked after the run was never given the slot")
	}

	// A caller that gives up waiting is that, never a busy node.
	held, err := p.HoldRead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := p.HoldRead(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, gitproc.ErrBusy) {
		t.Fatalf("a hold whose caller went away: %v, want context.Canceled and not ErrBusy", err)
	}
}
