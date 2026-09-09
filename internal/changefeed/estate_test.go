package changefeed_test

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// estate is an [changefeed.Opener] and [changefeed.Records] of this fixture's
// own.
//
// # Why the tests do not open a real one
//
// Every case in this package is about the FEED's rules — the claim, the
// dedupe, the wake ids, which outcomes ack and which nak — and none of them is
// about an estate. The document families these tests were written against are
// gone; the two that replaced them are domain LOGS, which would need a broker
// per test to exercise a rule that has nothing to do with a broker.
//
// So the seam is filled directly, which is what the seam is for: `Opener` is
// one method and `Records` is two, and a fixture that implements them
// exercises the contract rather than one estate's satisfaction of it.
// queued is one record and the earliest instant it may be delivered.
type queued struct {
	rec changefeed.Record
	at  time.Time
}

type estate struct {
	mu      sync.Mutex
	pending []queued
	waiting chan struct{}
	acked   map[string]int
	naked   map[string]int
	group   string
	openErr error
	opens   int
}

// nakRedeliveries is how many times this fixture hands a naked record back.
//
// Eight, which is comfortably more than any case here needs and finite, so a
// record the feed can never handle fails the test by timing out rather than by
// spinning a goroutine for the life of the run.
const nakRedeliveries = 8

func newEstate() *estate {
	return &estate{
		waiting: make(chan struct{}, 1024),
		acked:   map[string]int{}, naked: map[string]int{},
	}
}

func (e *estate) Open(_ context.Context, group string) (changefeed.Records, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opens++
	e.group = group
	if e.openErr != nil {
		return nil, e.openErr
	}
	return e, nil
}

// deliver queues one record, exactly as an estate's own consumer would.
func (e *estate) deliver(rec changefeed.Record) {
	e.mu.Lock()
	e.pending = append(e.pending, queued{rec: rec})
	e.mu.Unlock()
	select {
	case e.waiting <- struct{}{}:
	default:
	}
}

func (e *estate) Next(ctx context.Context) (*changefeed.Message, error) {
	for {
		e.mu.Lock()
		// THE DELAY IS HONOURED, because the feed's whole retry
		// behaviour is built on it: a fixture that handed a naked record
		// straight back would burn its redelivery budget in microseconds
		// and the case that recovers a broker mid-retry would find
		// nothing left to redeliver.
		if len(e.pending) > 0 && !time.Now().Before(e.pending[0].at) {
			rec := e.pending[0].rec
			e.pending = e.pending[1:]
			e.mu.Unlock()
			return &changefeed.Message{
				Record: rec,
				Ack: func() error {
					e.mu.Lock()
					defer e.mu.Unlock()
					e.acked[rec.ID]++
					return nil
				},
				Nak: func(delay time.Duration) error {
					e.mu.Lock()
					defer e.mu.Unlock()
					e.naked[rec.ID]++
					// EVERY NAK COMES BACK, up to a cap, which
					// is what a real consumer does: a fixture
					// that redelivered once would turn "the
					// broker recovered and the retry landed"
					// into "the record vanished on its second
					// failure", and the cap is what stops a
					// permanently failing record spinning a
					// test forever rather than failing it.
					if e.naked[rec.ID] <= nakRedeliveries {
						e.pending = append(e.pending,
							queued{rec: rec, at: time.Now().Add(delay)})
						select {
						case e.waiting <- struct{}{}:
						default:
						}
					}
					return nil
				},
			}, nil
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.waiting:
		case <-time.After(2 * time.Millisecond):
			// A RECORD WHOSE DELAY HAS NOT ELAPSED is still
			// pending, and nothing will signal again for it — so
			// the wait has a floor rather than blocking on a
			// channel that has already been drained.
		}
	}
}

func (e *estate) Stop() error { return nil }

func (e *estate) acks(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.acked[id]
}

func (e *estate) naks(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.naked[id]
}
