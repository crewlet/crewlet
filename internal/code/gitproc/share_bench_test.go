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

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// BenchmarkTransferShare is A PERSON'S CLONE ON A TRANSFER LANE CI HAS FILLED:
// real upload-packs of the shared history (BenchmarkTransfer's, about 19 MB)
// as the host runs a clone — protocol v2, the transfer lane, upload-pack's
// deadline and the stall watchdog — each operation one person's clone, its
// wait for a slot and its run. What decides ShareClass.Weight and
// ShareWindow (share.go) is in these arms:
//
//   - mix=idle: the person alone. The floor every other arm is read against.
//   - mix=machine64: sixty-four clones at once by ONE machine credential, every one
//     of them begun again the moment it ends, while the person clones. The
//     share grants the person the first slot released.
//   - mix=machine64-fifo: the same with the lane granting in arrival order
//     (gitproc.SetArrivalOrder) — the CONTROL, the lane before the share: the
//     person waits behind every clone queued before them, and past SlotWait is
//     answered busy (counted, never a failure of the arm).
//   - mix=machine-alone and mix=machine-alone-fifo: the machine's sixty-four
//     with nobody else, each operation sixty-four of its clones ending — the lane's
//     throughput with and without the share, which a WORK-CONSERVING share
//     leaves equal: a weight decides who goes next, never whether a slot is
//     used.
//   - decay=1m and decay=3m: the person's own burst first — sixty-four clones
//     of their own — then, on a clock moved on by that much
//     (gitproc.SetShareClock), the machine's sixty-four and the person's
//     clones as in mix=machine64. What ShareWindow buys: a burst still counts
//     against its principal a minute on, and three minutes on (e^-3, 5 %
//     left) the person waits as one who spent nothing does.
//   - mix=runs/weights=P:M:R: THE CODING BOXES' CLASS AMONG THE OTHERS
//     (ADR-0063) — the machine's sixty-four, a minute into holding the lane,
//     then boxClones coding boxes arriving, each a principal of its own (its
//     launch) making the coding recipe's two transfers again and again
//     (boxClone: a blobless fetch of the tip, then its checkout's fetch of
//     the tip's every file), half of them read at objstore.MiBPace's floor,
//     so a slow box holds its slot for minutes; the person's clones measured
//     once the boxes have held what the lane lets them for boxesSteady
//     (boxesHold), with a push of theirs every personPushEvery beside them —
//     every class contending for every release,
//     on the lane weighing the classes P:M:R (gitproc.SetWeights): 4:2:1,
//     ShareClass.Weight's, then 8:4:1 and 16:8:1, the search the weights are
//     decided by. Its pass rule: the person's clone p99 within twice
//     mix=idle's, none of their clones or pushes answered busy, and the boxes
//     never holding more than half the lane (run-held-max, sampled through
//     gitproc.TransferHeld, and run-share, the run class's part of what the
//     lane spent lately, which crewlet.code.git.share{share=run} reads).
//   - mix=runs-uncapped: the same at 4:2:1 with the boxes' cap lifted
//     (gitproc.SetRunUncapped), measured once the boxes hold every slot — the
//     CONTROL, the share before ShareClass.Cap: slow boxes holding the whole
//     lane for minutes, and the person answered busy.
//
// The runs arms' lane is as wide as the cores too, unless shareSlotsEnv names
// a width of its own (runsLaneWidth). ON A LANE OF TWO — the runner's, where
// these arms were first measured — the boxes' half is one slot and a person's
// clone shares the other with the machine's: it waits behind at most one
// machine clone, which takes as long as its own, so its p99 is about twice
// its idle one whatever the weights, which keep persons at twice the machines
// in every candidate. That lane CANNOT TELL THE CANDIDATES APART: one count
// of five hundred clones read 4:2:1 at 1.97 times idle (83.0 ms against
// 42.2) and 8:4:1 at 2.03, an earlier count of a hundred 4:2:1 at 2.04 and
// 8:4:1 at 1.96 — which weight passes flips from one count to the next — so
// ShareClass.Weight's run 1 stays the judgment it was, and the search is to
// be run on a lane wide enough for the weights to decide who is granted a
// release, not the lane's width.
//
// THE LANE IS AS WIDE AS THE MACHINE'S CORES (git.max_transfers = cores) —
// the runs arms' unless shareSlotsEnv sets theirs — not the production four
// a core: the stand-in client here reads as fast as the
// pipe allows, so every upload-pack is bound by its CPU, and at four a core
// every running clone would be slowed fourfold by the others whichever was
// granted first — a cost no grant order changes, while the lane four a core
// is sized for clients that pace their transfers themselves. At one a core
// the slots, and nothing else, are what a clone waits for, and a held slot is
// charged a whole core a second (occupancyRate), as on every lane no wider
// than its cores.
//
// Reported per arm: the person's clone p50 and p99 and the wait for its slot
// p99, in milliseconds (clone-p50-ms, clone-p99-ms, wait-p99-ms); busy, the
// person's clones refused for a slot that stayed held past SlotWait; on the
// machine-alone arms, clones/s; and on the runs arms slots, the lane's width,
// run-held-max, run-share, pushes and push-busy.
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
			b.ReportMetric(float64(64*b.N)/time.Since(at).Seconds(), "clones/s")
		})
	}

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
			// THE CI TOKEN'S MINUTE: the boxes arrive at a lane the machine's
			// clones have held whole for a ShareWindow — the clock moved on by
			// one, as the decay arms move it — so they win its releases until
			// their own spending catches up with half the machine's: the burst
			// of launches on a busy node that the boxes' cap is for. Arriving at
			// a lane the machine had held a moment, they would be weighed
			// back to a slot of their own within a second, cap or none.
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

// shareSlotsEnv names the runs arms' lane width, in transfer slots, where it
// is set: how the run class's weight is searched on a lane as wide as a
// production node's (four slots a core) or wider than the machine the
// benchmark runs on.
//
// THE RUNS ARMS ALONE take it, since what fills their lane is mostly paced:
// at the boxes' half held, the slow boxes hold their slots reading thirty
// seconds a mebibyte and spend almost no CPU, so a lane of twice the cores
// runs the other half — the machine's and the person's clones, which read as
// fast as the pipe allows — at one a core, as every other arm runs its
// whole lane. Wider than that, those clones share cores, a cost every grant
// order pays alike; the clone-p99-ms the arm reports against mix=idle's,
// which runs on the cores' own width, then carries it, and the comparison is
// made against an idle clone on the same machine.
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
// held by slow boxes in their checkout's fetch — minutes each, the steady
// state a burst of launches leaves a lane in — and never by a box about to
// release one.
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
		case now.Sub(since) >= boxesSteady:
			return
		}
		if now.After(deadline) {
			b.Fatalf("the boxes never held %d of the lane's %d slots for %v in %v", want,
				p.Slots(gitproc.ClassTransfer), boxesSteady, boxesSteadyWait)
		}
		time.Sleep(boxSample)
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
		tick := time.NewTicker(boxSample)
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
		var out io.Writer = io.Discard
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
	case <-time.After(time.Until(due)):
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
		tick := time.NewTicker(personPushEvery)
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
	took = time.Since(at)
	if err == nil && sent.n.Load() < h.packBytes/2 {
		err = fmt.Errorf("upload-pack sent %d bytes of a history whose pack is %d", sent.n.Load(), h.packBytes)
	}
	return took, res.Waited, err
}

// shareLoad keeps n clones by s running on p, each begun again as it ends,
// until the answered stop is called — which waits for every one to end. Each
// clone that ends is announced on ended when it is not nil (and the reader
// keeps up: it is a buffered channel a reader drains). It returns once the
// lane is full and every one of the n has asked for a slot, so the arm that
// measures beside it measures a lane already contended.
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
		if time.Now().After(deadline) {
			b.Fatalf("%d clones left %d of the lane's %d slots free after a minute", n, free,
				p.Slots(gitproc.ClassTransfer))
		}
		time.Sleep(10 * time.Millisecond)
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
