package home_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/home"
)

// The burst BenchmarkHomeLoop works, and what each step costs: a facts read's
// git holding a unit of the share, a record's round trip to its writer, and a
// seat's read beside them.
const (
	benchRepos      = 16
	benchFactsHold  = 20 * time.Millisecond
	benchRecordTrip = 5 * time.Millisecond
	benchSeatHold   = time.Millisecond
	benchSeatPause  = 2 * time.Millisecond
)

// sharedEstate is a burst's estate as a Gitlet node answers it: every git read
// holds a unit of the node's real share for benchFactsHold, as the read's own
// work (gitlet.GitQuery.SeatWork), and every record takes benchRecordTrip.
type sharedEstate struct {
	*estate
	share *gitlet.GitShare
}

func (s sharedEstate) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	release, err := s.share.Take(ctx, q.SeatWork())
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	time.Sleep(benchFactsHold) //fixedwait:simulated a facts read's git holding its unit of the share
	release()
	return s.estate.GitRead(ctx, q)
}

func (s sharedEstate) Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	time.Sleep(benchRecordTrip) //fixedwait:simulated a record's round trip to its writer
	return s.estate.Converse(ctx, w)
}

// BenchmarkHomeLoop is what decides home.Concurrency (whose doc holds its
// results) and home.OwnReads: a BURST — benchRepos repositories due at once,
// each a change owing its facts, as a node's first pass after a start finds
// them — worked by a real Loop on a real gitlet.GitShare, beside a SEAT
// READER taking a unit of the same share for benchSeatHold every
// benchSeatPause. Per share
// (one unit: a node of three cores or fewer; two: four or five) and per turns
// at once (1, 2, 4), it reports `drain-ms`, until every change's facts are
// recorded, and `seat-wait-max-ms` and `seat-wait-p99-ms`, the seat's waits.
// A MODEL OF A NODE: the hold and the round trip are stand-ins, so it ranks
// the values rather than timing git (BenchmarkRevisionFacts in
// internal/gitlet/query does that).
func BenchmarkHomeLoop(b *testing.B) {
	for _, units := range []int{1, 2} {
		for _, turns := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("units=%d/turns=%d", units, turns), func(b *testing.B) {
				var (
					drained time.Duration
					waits   []time.Duration
					runs    int
				)
				for b.Loop() {
					d, w := burst(b, units, turns)
					drained += d
					waits = append(waits, w...)
					runs++
				}
				slices.Sort(waits)
				b.ReportMetric(float64(drained.Milliseconds())/float64(runs), "drain-ms")
				b.ReportMetric(float64(waits[len(waits)-1].Microseconds())/1000, "seat-wait-max-ms")
				b.ReportMetric(float64(waits[len(waits)*99/100].Microseconds())/1000, "seat-wait-p99-ms")
			})
		}
	}
}

// burst is one burst on a share of units units, worked turns at once: how long
// it took to drain, and every wait of the seat reader beside it.
func burst(b *testing.B, units, turns int) (time.Duration, []time.Duration) {
	e := newEstate()
	homes := map[string]string{}
	repos := make([]string, benchRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("01920000-0000-7000-8000-%012x", i+1)
		homes[repos[i]] = "node-a"
		e.hold(repos[i], 1, fmt.Sprintf("%040x", i+1), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"),
			PatchID: oid("e"), Touched: []string{"a.go"}})
	}
	share := gitlet.NewGitShare("node-a", 2*units, nil)
	l, err := home.New(home.Options{Chart: noChart, Estate: sharedEstate{estate: e, share: share},
		Placement: asNode{placement: &placement{homes: homes}, node: "node-a"}, Nudges: newNudges(),
		Cache: unmerged{t: b}, Git: unmerged{t: b}, Chains: unqueued{t: b}, Tasks: unqueued{t: b},
		Share: share, NodeID: "node-a"})
	if err != nil {
		b.Fatal(err)
	}
	home.SetConcurrency(l, turns)

	done := make(chan struct{})
	var (
		mu    sync.Mutex
		waits []time.Duration
		seat  sync.WaitGroup
	)
	seat.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(benchSeatPause): //fixedwait:bench
			}
			began := time.Now()
			release, err := share.Take(context.Background(), gitlet.SeatWorkRead)
			waited := time.Since(began) //fixedwait:measure a benchmark reports this; no gate reads it
			mu.Lock()
			waits = append(waits, waited)
			mu.Unlock()
			if err == nil {
				time.Sleep(benchSeatHold) //fixedwait:bench
				release()
			}
		}
	})

	began := time.Now()
	l.Start()
	for {
		e.mu.Lock()
		left := 0
		for _, repo := range repos {
			left += len(e.due[repo])
		}
		e.mu.Unlock()
		if left == 0 {
			break
		}
		time.Sleep(time.Millisecond) //fixedwait:liveness
	}
	drained := time.Since(began) //fixedwait:measure a benchmark reports this; no gate reads it
	close(done)
	seat.Wait()
	l.Stop(context.Background())
	return drained, waits
}
