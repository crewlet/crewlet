package gitproc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// BenchmarkTransferShare is A PERSON'S CLONE ON A TRANSFER LANE CI HAS FILLED:
// real upload-packs of the shared history (BenchmarkTransfer's) as the host
// runs a clone, each operation one person's clone, its wait for a slot and its
// run. Its arms decide ShareClass.Weight and ShareWindow (share.go); each loop
// below says what its arms show. The lane is as wide as the cores (the runs
// arms' unless shareSlotsEnv sets it), not the production four a core: this
// client reads as fast as the pipe allows, so at four a core every clone would
// be slowed by the others whatever the grant order, and a held slot is charged
// a whole core a second (occupancyRate).
// Reported: clone-p50-ms, clone-p99-ms and wait-p99-ms of the person's clones;
// busy, those refused past SlotWait; clones/s on the machine-alone arms; slots,
// run-held-max, run-share, pushes and push-busy on the runs arms.
func BenchmarkTransferShare(b *testing.B) {
	quiet(b)
	h := sharedHistory(b)
	width := runtime.NumCPU()
	person := gitproc.Share{Class: gitproc.SharePerson, Principal: "ceo"}
	machine := gitproc.Share{Class: gitproc.ShareMachine, Principal: "token:ci"}

	newLanePool := func(b *testing.B, fifo bool) *gitproc.Pool {
		p := realPool(b, gitproc.Options{MaxProcesses: width, MaxTransfers: width})
		if fifo {
			gitproc.SetArrivalOrder(p)
		}
		return p
	}

	// mix=idle: the person alone, the floor every arm is read against.
	// mix=machine64: sixty-four clones at once by ONE machine credential, each
	// begun again as it ends; the share grants the person the first slot
	// released. mix=machine64-fifo is its CONTROL, arrival order
	// (gitproc.SetArrivalOrder): the person waits behind every clone queued
	// first, and past SlotWait is answered busy (counted, never a failure).
	for _, arm := range []struct {
		name string
		fifo bool
		load bool
	}{
		{"mix=idle", false, false},
		{"mix=machine64", false, true},
		{"mix=machine64-fifo", true, true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			p := newLanePool(b, arm.fifo)
			if arm.load {
				stop := shareLoad(b, p, h, machine, 64, nil)
				defer stop()
			}
			personClones(b, p, h, person)
		})
	}

	// mix=machine-alone(-fifo): the machine's sixty-four with nobody else, an
	// operation sixty-four of its clones ending. A WORK-CONSERVING share leaves
	// the throughput equal with and without it: a weight decides who goes next,
	// never whether a slot is used.
	for _, arm := range []struct {
		name string
		fifo bool
	}{
		{"mix=machine-alone", false},
		{"mix=machine-alone-fifo", true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			p := newLanePool(b, arm.fifo)
			ended := make(chan struct{}, 64)
			stop := shareLoad(b, p, h, machine, 64, ended)
			defer stop()
			<-ended // the lane full and turning over before the timer starts
			at := time.Now()
			for b.Loop() {
				// AN OPERATION IS SIXTY-FOUR CLONES ENDING, the load's own
				// size: one clone's end apart is tens of milliseconds, and a
				// rate taken over a score of those is the scheduler's jitter.
				for range 64 {
					<-ended
				}
			}
			b.ReportMetric(float64(64*b.N)/time.Since(at).Seconds(), "clones/s") //fixedwait:measure reported as the benchmark's rate
		})
	}

	// decay=1m and decay=3m: the person's own burst of sixty-four clones, then,
	// on a clock moved on that much (gitproc.SetShareClock), mix=machine64. What
	// ShareWindow buys: a burst still counts a minute on, and three minutes on
	// (e^-3, 5 % left) the person waits as one who spent nothing does.
	for _, after := range []time.Duration{time.Minute, 3 * time.Minute} {
		b.Run(fmt.Sprintf("decay=%v", after), func(b *testing.B) {
			p := newLanePool(b, false)
			var offset atomic.Int64
			gitproc.SetShareClock(p, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
			// THE PERSON'S BURST: sixty-four clones of their own, the lane
			// theirs alone, charged to them.
			var burst sync.WaitGroup
			for range 64 {
				burst.Go(func() {
					if _, _, err := shareClone(b.Context(), p, h, person); err != nil {
						b.Error(err)
					}
				})
			}
			burst.Wait()
			offset.Add(int64(after))
			stop := shareLoad(b, p, h, machine, 64, nil)
			defer stop()
			personClones(b, p, h, person)
		})
	}

	// mix=runs/weights=P:M:R: THE CODING BOXES' CLASS AMONG THE OTHERS
	// (ADR-0093). The machine's sixty-four, then boxClones boxes (half paced at
	// objstore.MiBPace's floor; boxClone), the person's clones measured once the
	// boxes hold their cap (boxesHold), with a push every personPushEvery, on
	// weights P:M:R (gitproc.SetWeights): the search that decides the run's
	// weight. Pass: clone p99 within twice mix=idle's, no clone or push busy,
	// the boxes never past half the lane (run-held-max, via gitproc.TransferHeld;
	// run-share, as crewlet.code.git.share{share=run} reads). A two-slot lane
	// cannot tell the weights apart (twice idle is its floor under any), so run
	// it wider (shareSlotsEnv). mix=runs-uncapped is the CONTROL
	// (gitproc.SetRunUncapped): boxes hold the whole lane; the person is busy.
	for _, arm := range []struct {
		name     string
		weights  [3]int // person, machine, run
		uncapped bool
	}{
		{"mix=runs/weights=4:2:1", [3]int{4, 2, 1}, false},
		{"mix=runs/weights=8:4:1", [3]int{8, 4, 1}, false},
		{"mix=runs/weights=16:8:1", [3]int{16, 8, 1}, false},
		{"mix=runs-uncapped", [3]int{4, 2, 1}, true},
	} {
		b.Run(arm.name, func(b *testing.B) {
			lane := runsLaneWidth(b, width)
			p := realPool(b, gitproc.Options{MaxProcesses: lane, MaxTransfers: lane})
			b.ReportMetric(float64(lane), "slots")
			var offset atomic.Int64
			gitproc.SetShareClock(p, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
			gitproc.SetWeights(p, map[gitproc.ShareClass]int{
				gitproc.SharePerson: arm.weights[0], gitproc.ShareMachine: arm.weights[1], gitproc.ShareRun: arm.weights[2],
			})
			held := gitproc.ShareRun.Cap(lane)
			if arm.uncapped {
				gitproc.SetRunUncapped(p)
				held = lane
			}
			stopMachines := shareLoad(b, p, h, machine, 64, nil)
			defer stopMachines()
			// THE CI TOKEN'S MINUTE: the boxes arrive at a lane the machine has
			// held whole for a ShareWindow, so they win releases until their
			// spending reaches half the machine's: the burst of launches the
			// boxes' cap is for. On a lane held only a moment they would be
			// weighed back to one slot within a second, cap or none.
			offset.Add(int64(gitproc.ShareWindow))
			boxes := startBoxes(b, p, h, boxClones)
			defer boxes.stop(b)
			boxesHold(b, p, held)
			pushes := startPushes(b, p, person)
			defer pushes.stop(b)
			personClones(b, p, h, person)
			b.ReportMetric(float64(boxes.most.Load()), "run-held-max")
			usage := gitproc.TransferLane(p)
			run, all := usage.Usage(gitproc.ShareRun), 0.0
			for _, c := range gitproc.ShareClasses() {
				all += usage.Usage(c)
			}
			b.ReportMetric(run/all, "run-share")
			b.ReportMetric(float64(pushes.pushed.Load()), "pushes")
			b.ReportMetric(float64(pushes.busy.Load()), "push-busy")
		})
	}
}

// shareSlotsEnv names the runs arms' lane width in transfer slots, to search
// the run class's weight on a lane as wide as a production node's (four a
// core) or wider than this machine. The runs arms alone take it: their slow
// boxes hold half the slots at almost no CPU, so a lane of twice the cores
// runs the fast half at one a core, as every other arm runs. Wider, those
// clones share cores, a cost every grant order pays alike, which their
// clone-p99-ms then carries against mix=idle's.
const shareSlotsEnv = "CREWLET_BENCH_SHARE_SLOTS"

// runsLaneWidth is the runs arms' lane width: shareSlotsEnv's, or cores where
// it is unset. A width the transfer lane cannot be — under two slots, which
// leaves the boxes' half no slot the person's could be granted beside — fails
// b naming the variable.
func runsLaneWidth(b *testing.B, cores int) int {
	b.Helper()
	raw, set := os.LookupEnv(shareSlotsEnv)
	if !set {
		return cores
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2 {
		b.Fatalf("%s=%q: want a whole number of transfer slots, at least 2", shareSlotsEnv, raw)
	}
	return n
}

// boxClones is how many coding boxes the runs arms start, each a principal of
// its own: as many as the machine's clones, so every class has a crowd behind
// it at every release, and half of them slow.
const boxClones = 64

// boxesSteady is how long the boxes must hold the slots a runs arm lets them
// before the person's clones are measured: past a fast box's whole clone and
// a slow box's blobless fetch (each under a second here), so the slots are
// held by slow boxes in their checkout's fetch (minutes each), the steady
// state a burst of launches leaves a lane in.
const boxesSteady = 3 * time.Second

// boxesSteadyWait bounds the wait for that steady state: the boxes' first
// releases take seconds, and a lane that holds no such state in two minutes is
// one the arm cannot measure.
const boxesSteadyWait = 2 * time.Minute

// boxesHold waits until the boxes have held want of p's transfer slots for
// boxesSteady together, failing b past boxesSteadyWait.
func boxesHold(b *testing.B, p *gitproc.Pool, want int) {
	b.Helper()
	deadline := time.Now().Add(boxesSteadyWait)
	var since time.Time
	for {
		now := time.Now()
		switch held := gitproc.TransferHeld(p, gitproc.ShareRun); {
		case held < want:
			since = time.Time{}
		case since.IsZero():
			since = now
		case now.Sub(since) >= boxesSteady: //fixedwait:measure a benchmark's settling, which only says when its measuring starts
			return
		}
		if now.After(deadline) { //fixedwait:measure a poll's own deadline
			b.Fatalf("the boxes never held %d of the lane's %d slots for %v in %v", want,
				p.Slots(gitproc.ClassTransfer), boxesSteady, boxesSteadyWait)
		}
		time.Sleep(boxSample) //fixedwait:bench
	}
}

// personPushEvery is how often the runs arms' person pushes beside the
// clones they make one after another: a push a second, so most of their
// clones have one in flight beside them.
const personPushEvery = time.Second

// boxLoad is the runs arms' coding boxes, and the most transfer slots their
// class held at once, sampled every boxSample.
type boxLoad struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	failed atomic.Value
	most   atomic.Int64
}

// boxSample is how often a runs arm reads how many slots the boxes hold:
// well under a box's shortest transfer, so a slot taken past the cap is
// seen.
const boxSample = 5 * time.Millisecond

// startBoxes starts n coding boxes on p, each making boxClone of h's tip
// again and again under a principal of its own, every odd one reading at
// objstore.MiBPace's floor, until stop.
func startBoxes(b *testing.B, p *gitproc.Pool, h *history, n int) *boxLoad {
	b.Helper()
	blobs := strings.Fields(gittest.NewClient(b).Run(b, h.dir, "ls-tree", "-r", "--format=%(objectname)", h.tip))
	ctx, cancel := context.WithCancel(b.Context())
	l := &boxLoad{cancel: cancel}
	for i := range n {
		s := gitproc.Share{Class: gitproc.ShareRun, Principal: fmt.Sprintf("launch-%02d", i)}
		l.wg.Go(func() {
			for ctx.Err() == nil {
				err := boxClone(ctx, p, h, s, blobs, i%2 == 1)
				switch {
				case ctx.Err() != nil:
					return
				case errors.Is(err, gitproc.ErrBusy):
					// A box refused a slot clones again, as a launch's
					// setup step is retried: the class stays as contended.
				case err != nil:
					l.failed.CompareAndSwap(nil, err)
					return
				}
			}
		})
	}
	l.wg.Go(func() {
		tick := time.NewTicker(boxSample) //fixedwait:bench
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if held := int64(gitproc.TransferHeld(p, gitproc.ShareRun)); held > l.most.Load() {
					l.most.Store(held)
				}
			}
		}
	})
	return l
}

// stop ends the boxes and waits for every one, failing b if a box's clone
// failed for anything but a slot it was refused.
func (l *boxLoad) stop(b *testing.B) {
	l.cancel()
	l.wg.Wait()
	if err, _ := l.failed.Load().(error); err != nil {
		b.Fatalf("a coding box's clone failed: %v", err)
	}
}

// boxClone is one coding box's clone of h's tip as the coding recipe makes it
// through the host: a blobless fetch of the tip — every commit and tree —
// then the checkout's fetch of the tip's every file, blobs, each an
// upload-pack of the transfer lane charged to s, served as a cache serves a
// partial clone; slow reads both at objstore.MiBPace's floor.
func boxClone(ctx context.Context, p *gitproc.Pool, h *history, s gitproc.Share, blobs []string, slow bool) error {
	head := pkt("command=fetch\n") + "0001" + pkt("ofs-delta\n") + pkt("no-progress\n")
	var lazy strings.Builder
	lazy.WriteString(head)
	for _, oid := range blobs {
		lazy.WriteString(pkt("want " + oid + "\n"))
	}
	lazy.WriteString(pkt("done\n") + "0000")
	for _, request := range []string{
		head + pkt("filter blob:none\n") + pkt("want "+h.tip+"\n") + pkt("done\n") + "0000",
		lazy.String(),
	} {
		out := io.Discard
		if slow {
			out = &paced{ctx: ctx, at: time.Now()}
		}
		if _, err := p.Run(ctx, gitproc.Cmd{
			Dir: h.dir, Args: []string{"upload-pack", "--stateless-rpc", h.dir}, Protocol: "version=2",
			Stdin: strings.NewReader(request), Stdout: out,
			Config: []gitproc.KV{
				{Key: "uploadpack.allowFilter", Value: "true"},
				{Key: "uploadpackfilter.allow", Value: "false"},
				{Key: "uploadpackfilter.blob:none.allow", Value: "true"},
			},
			Class: gitproc.ClassTransfer, Share: s,
			Deadline: gitproc.UploadPackDeadline(h.packBytes), Stall: gitproc.TransferStall,
		}); err != nil {
			return err
		}
	}
	return nil
}

// paced is a client reading what it is sent at objstore.MiBPace's floor — a
// mebibyte in thirty seconds, the slowest a transfer waits for — so its
// transfer holds its slot as long as the slowest box's does.
type paced struct {
	ctx context.Context
	at  time.Time
	n   int64
}

func (w *paced) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	due := w.at.Add(time.Duration(float64(w.n) / objstore.MiB * float64(objstore.MiBPace)))
	select {
	//fixedwait:simulated a client reading at objstore.MiBPace's floor
	case <-time.After(time.Until(due)): //fixedwait:measure the remaining time only paces the client
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

// pushLoad is the runs arms' person pushing beside their clones.
type pushLoad struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	failed atomic.Value
	pushed atomic.Int64
	busy   atomic.Int64
}

// startPushes has s push a commit to a branch of its own every
// personPushEvery — receive-pack in the transfer lane, as the host runs a
// person's push — into a repository of its own, until stop: pushed counts
// those that landed, busy those refused a slot.
func startPushes(b *testing.B, p *gitproc.Pool, s gitproc.Share) *pushLoad {
	b.Helper()
	c := gittest.NewClient(b)
	wt := c.Init(b)
	commit := c.Commit(b, wt, map[string]string{"pushed.txt": "a person's push\n"}, "a person's push")
	pack, stderr, err := c.Try(b, wt, strings.NewReader(commit+"\n"), "pack-objects", "--stdout", "--revs", "-q")
	if err != nil {
		b.Fatalf("packing a person's push: %v\n%s", err, stderr)
	}
	into := gittest.Empty(b)
	ctx, cancel := context.WithCancel(b.Context())
	l := &pushLoad{cancel: cancel}
	l.wg.Go(func() {
		tick := time.NewTicker(personPushEvery) //fixedwait:bench
		defer tick.Stop()
		indexed := make(chan struct{})
		close(indexed)
		for n := 0; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			request := pkt(fmt.Sprintf("%s %s refs/heads/push-%d\x00report-status\n", strings.Repeat("0", 40),
				commit, n)) + "0000" + pack
			_, err := p.Run(ctx, gitproc.Cmd{
				Dir: into, Args: []string{"receive-pack", "--stateless-rpc", into},
				Stdin: strings.NewReader(request), Stdout: io.Discard,
				Class: gitproc.ClassTransfer, Share: s, Deadline: gitproc.ReadDeadline, Indexed: indexed,
			})
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, gitproc.ErrBusy):
				l.busy.Add(1)
			case err != nil:
				l.failed.CompareAndSwap(nil, err)
				return
			default:
				l.pushed.Add(1)
			}
		}
	})
	return l
}

// stop ends the pushes and waits for the one in flight, failing b if a push
// failed for anything but a slot it was refused.
func (l *pushLoad) stop(b *testing.B) {
	l.cancel()
	l.wg.Wait()
	if err, _ := l.failed.Load().(error); err != nil {
		b.Fatalf("a person's push failed: %v", err)
	}
}

// shareClone is one clone of h's tip, as the host serves a person's: an
// upload-pack v2 fetch in the transfer lane, charged to s, into a writer that
// only counts. It answers how long the clone took from asking for its slot to
// its end, and how long of that it waited for the slot.
func shareClone(ctx context.Context, p *gitproc.Pool, h *history, s gitproc.Share) (took, waited time.Duration, err error) {
	request := pkt("command=fetch\n") + "0001" + pkt("ofs-delta\n") + pkt("no-progress\n") +
		pkt("want "+h.tip+"\n") + pkt("done\n") + "0000"
	var sent counter
	at := time.Now()
	res, err := p.Run(ctx, gitproc.Cmd{
		Dir: h.dir, Args: []string{"upload-pack", "--stateless-rpc", h.dir}, Protocol: "version=2",
		Stdin: strings.NewReader(request), Stdout: &sent,
		Class: gitproc.ClassTransfer, Share: s,
		Deadline: gitproc.UploadPackDeadline(h.packBytes), Stall: gitproc.TransferStall,
	})
	took = time.Since(at) //fixedwait:measure the clone's time, which the benchmark reports
	if err == nil && sent.n.Load() < h.packBytes/2 {
		err = fmt.Errorf("upload-pack sent %d bytes of a history whose pack is %d", sent.n.Load(), h.packBytes)
	}
	return took, res.Waited, err
}

// shareLoad keeps n clones by s running on p, each begun again as it ends,
// until the answered stop, which waits for every one. Each clone that ends is
// announced on ended unless it is nil (a buffered channel its reader drains).
// It returns once all n have asked for a slot and the lane is full, so the arm
// beside it measures a lane already contended.
func shareLoad(b *testing.B, p *gitproc.Pool, h *history, s gitproc.Share, n int, ended chan<- struct{}) (stop func()) {
	b.Helper()
	ctx, cancel := context.WithCancel(b.Context())
	var (
		wg     sync.WaitGroup
		asked  sync.WaitGroup
		failed atomic.Value
	)
	asked.Add(n)
	for range n {
		wg.Go(func() {
			first := true
			for ctx.Err() == nil {
				if first {
					asked.Done()
					first = false
				}
				_, _, err := shareClone(ctx, p, h, s)
				switch {
				case ctx.Err() != nil:
					return
				case errors.Is(err, gitproc.ErrBusy):
					// A machine clone refused a slot is begun again, as CI
					// retries one: the lane stays as contended.
				case err != nil:
					failed.CompareAndSwap(nil, err)
					return
				}
				if ended != nil {
					select {
					case ended <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}
	asked.Wait()
	deadline := time.Now().Add(time.Minute)
	for free := p.Free(gitproc.ClassTransfer); free > 0; free = p.Free(gitproc.ClassTransfer) {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline
			b.Fatalf("%d clones left %d of the lane's %d slots free after a minute", n, free,
				p.Slots(gitproc.ClassTransfer))
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:bench
	}
	return func() {
		cancel()
		wg.Wait()
		if err, _ := failed.Load().(error); err != nil {
			b.Fatalf("a machine clone failed: %v", err)
		}
	}
}

// personClones is the measured loop: one person's clone an operation, with
// its percentiles and the clones refused busy reported when it ends.
func personClones(b *testing.B, p *gitproc.Pool, h *history, s gitproc.Share) {
	b.Helper()
	var took, waited []time.Duration
	busy := 0
	for b.Loop() {
		clone, wait, err := shareClone(b.Context(), p, h, s)
		if errors.Is(err, gitproc.ErrBusy) {
			busy++
			continue
		}
		if err != nil {
			b.Fatal(err)
		}
		took = append(took, clone)
		waited = append(waited, wait)
	}
	slices.Sort(took)
	slices.Sort(waited)
	b.ReportMetric(nearestRank(took, 0.5), "clone-p50-ms")
	b.ReportMetric(nearestRank(took, 0.99), "clone-p99-ms")
	b.ReportMetric(nearestRank(waited, 0.99), "wait-p99-ms")
	b.ReportMetric(float64(busy), "busy")
}

// nearestRank is the nearest-rank q-quantile of sorted, in milliseconds; 0
// for none.
func nearestRank(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := max(int(math.Ceil(q*float64(len(sorted)))), 1)
	return float64(sorted[rank-1]) / float64(time.Millisecond)
}
