package coordtest

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// listingsUnderChurn is the least number of reads a churn case takes while
// its writers run.
//
// Sized from the failure it certifies against rather than from a budget:
// measured on the embedded KV before its listings were certified, 23% of
// ListLive calls against three tight renewers came back without a lease that
// was live throughout, so a backend with that defect fails this many with
// certainty, and one without it has to be right every time.
const listingsUnderChurn = 200

// churn is a set of writers, each rewriting its own record in a tight loop
// until stopped. What it collects it reports from the test goroutine — never
// a Fatal off it, which TestNoFatalOffTheTestGoroutine holds.
//
// A write that FAILS ends its writer: the churn cases assert about records
// that exist throughout, and a failed renew is a lease that may not be live.
type churn struct {
	wg       sync.WaitGroup
	done     chan struct{}
	mu       sync.Mutex
	writes   int64
	failures []error
}

// startChurn starts one writer per key.
func startChurn(keys []string, write func(key string) error) *churn {
	c := &churn{done: make(chan struct{})}
	for _, key := range keys {
		c.wg.Go(func() {
			for {
				select {
				case <-c.done:
					return
				default:
				}
				err := write(key)
				c.mu.Lock()
				if err != nil {
					c.failures = append(c.failures, fmt.Errorf("%s: %w", key, err))
					c.mu.Unlock()
					return
				}
				c.writes++
				c.mu.Unlock()
			}
		})
	}
	return c
}

// landed is how many writes have completed so far.
func (c *churn) landed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// readThrough calls read at least listingsUnderChurn times, and then for as
// long as no write has landed since the first call. It answers how many
// writes landed while it read, and up to five of the answers read called
// wrong; read reports an answer as wrong by describing it, and ends the loop
// early by saying stop.
//
// THE COUNT IS A FLOOR, NOT THE LOOP, because a backend that answers a read
// in a microsecond finishes any fixed number of them before a writer has been
// scheduled at all — measured on the memory twin, where two hundred reads saw
// no write — and a churn case whose reads raced nothing certifies nothing. It
// yields between reads for the same reason. ctx bounds it, so a backend whose
// writes never land is reported as that rather than as a hang.
func (c *churn) readThrough(ctx context.Context, read func() (wrong string, stop bool)) (raced int64, wrong []string) {
	start := c.landed()
	for reads := 1; ; reads++ {
		answer, stop := read()
		if answer != "" && len(wrong) < 5 {
			wrong = append(wrong, answer)
		}
		if stop || ctx.Err() != nil || (reads >= listingsUnderChurn && c.landed() > start) {
			break
		}
		runtime.Gosched()
	}
	return c.landed() - start, wrong
}

// stop ends every writer and reports every failure.
func (c *churn) stop() []error {
	close(c.done)
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}

// verdict reports a churn case from the test goroutine: a writer that failed
// first, since its record may not have existed throughout and the reads then
// prove nothing; then any read that was not whole; then a run whose reads no
// write raced.
func (c *churn) verdict(t testingT, reads string, raced int64, wrong []string) {
	t.Helper()
	for _, err := range c.stop() {
		t.Errorf("a writer stopped, so its record was not rewritten throughout: %v", err)
	}
	if t.Failed() {
		return
	}
	if len(wrong) > 0 {
		t.Fatalf("%s answered without a record rewritten throughout it (%d writes "+
			"landed while it read); first answers: %v", reads, raced, wrong)
	}
	if raced == 0 {
		t.Fatalf("no write landed while %s read, so nothing here raced a read", reads)
	}
}

// testingT is the slice of *testing.T a verdict reports through.
type testingT interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Failed() bool
}
