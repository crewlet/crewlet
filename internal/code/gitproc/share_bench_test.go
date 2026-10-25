package gitproc_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
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
//
// THE LANE IS AS WIDE AS THE MACHINE'S CORES (git.max_transfers = cores), not
// the production four a core: the stand-in client here reads as fast as the
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
// person's clones refused for a slot that stayed held past SlotWait; and on
// the machine-alone arms, clones/s.
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
