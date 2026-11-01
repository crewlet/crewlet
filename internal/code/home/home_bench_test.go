package home_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
)

// The burst BenchmarkHomeLoop works, and what each step of it costs: a facts
// read's git holding a unit of the share, a record's round trip to its
// writer, and a seat's read beside them.
const (
	benchRepos      = 16
	benchFactsHold  = 20 * time.Millisecond
	benchRecordTrip = 5 * time.Millisecond
	benchSeatHold   = time.Millisecond
	benchSeatPause  = 2 * time.Millisecond
)

// sharedEstate is a burst's estate as a git node answers it: every git read
// holds a unit of the node's real share for benchFactsHold, taken as the
// read's own work (code.GitQuery.SeatWork), and every record takes
// benchRecordTrip.
type sharedEstate struct {
	*estate
	share *code.GitShare
}

func (s sharedEstate) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	release, err := s.share.Take(ctx, q.SeatWork())
	if err != nil {
		return code.GitAnswer{}, err
	}
	time.Sleep(benchFactsHold)
	release()
	return s.estate.GitRead(ctx, q)
}

func (s sharedEstate) Converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error) {
	time.Sleep(benchRecordTrip)
	return s.estate.Converse(ctx, w)
}

// BenchmarkHomeLoop is what decides home.Concurrency and home.OwnReads (M4's
// §2.5): a BURST — every one of benchRepos repositories of a node due at
// once, each a change owing its facts, as a node's first pass after a start
// finds them — worked by a real Loop, its facts reads each holding a unit of
// a real code.GitShare for benchFactsHold and each of its records taking
// benchRecordTrip; and beside it a SEAT READER taking a unit of the same
// share for benchSeatHold every benchSeatPause, as a node's seats' reads do,
// timing its own waits. Per share — one unit, a node of three cores or fewer;
// two, of four or five — and per turns at once (1, 2, 4), it reports
// `drain-ms`, the burst's time until every change's facts are recorded, the
// longest a repository of it waits for its turn, and `seat-wait-max-ms` and
// `seat-wait-p99-ms`, the seat reader's waits.
//
// A MODEL OF A NODE, not a node: the hold and the round trip are the
// stand-in's, so it ranks the values rather than timing a git node's git
// (BenchmarkRevisionFacts in internal/code/query times that). Run once
// (-benchtime 3x -count 1) on two cores, 2026-11-01:
//
//	units=1 turns=1  drain 493 ms  seat wait max 19.9 ms  p99 19.1 ms
//	units=1 turns=2  drain 348 ms  seat wait max 19.0 ms  p99 19.0 ms
//	units=1 turns=4  drain 348 ms  seat wait max 19.0 ms  p99 19.0 ms
//	units=2 turns=1  drain 494 ms  seat wait max 0.03 ms  p99 0.02 ms
//	units=2 turns=2  drain 332 ms  seat wait max 0.03 ms  p99 0.02 ms
//	units=2 turns=4  drain 333 ms  seat wait max 0.03 ms  p99 0.02 ms
//
// So two turns drain a burst as fast as four, and a seat waits behind one
// facts read at the most on a one-unit share and behind none on a wider one.
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

// burst is one burst on a node whose share holds units units, worked turns at
// once: how long it took to drain, and every wait of the seat reader beside it.
func burst(b *testing.B, units, turns int) (time.Duration, []time.Duration) {
	e := newEstate()
	homes := map[string]string{}
	repos := make([]string, benchRepos)
	for i := range repos {
		repos[i] = fmt.Sprintf("01920000-0000-7000-8000-%012x", i+1)
		homes[repos[i]] = "node-a"
		e.hold(repos[i], 1, fmt.Sprintf("%040x", i+1), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"),
			PatchID: oid("e"), Touched: []string{"a.go"}})
	}
	share := code.NewGitShare("node-a", 2*units, nil)
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
			case <-time.After(benchSeatPause):
			}
			began := time.Now()
			release, err := share.Take(context.Background(), code.SeatWorkRead)
			waited := time.Since(began)
			mu.Lock()
			waits = append(waits, waited)
			mu.Unlock()
			if err == nil {
				time.Sleep(benchSeatHold)
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
		time.Sleep(time.Millisecond)
	}
	drained := time.Since(began)
	close(done)
	seat.Wait()
	l.Stop(context.Background())
	return drained, waits
}
