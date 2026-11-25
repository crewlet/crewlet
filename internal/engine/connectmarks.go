package engine

import (
	"context"
	"log/slog"
	"sync"
)

// connectMarks is every job whose harness listed its bridge's tools on this
// node while its row does not say so yet ([bridgeLedger.connected]), by run.
// The write is made off the list, which a loaded store's write (one to three
// seconds in a fleet) held past Grok Build's two-second handshake wait, and is
// retried on a store's error ([appendRetryWaits]). Until the row says so, a
// collection here settles it on its completion, never a timer, and makes a
// write whose every try failed itself ([connectMarks.settle], sandbox's
// ConnectSettled); the seat leaving this node waits for its runs' writes and
// tries a failed one again before the lease goes, since the next holder
// collects them ([connectMarks.release]). A node that dies before its write
// lands leaves its run read as one that never connected.
type connectMarks struct {
	mu   sync.Mutex
	runs map[string]*connectMark
}

// connectMark is one job's connection not yet on its row.
type connectMark struct {
	seat string
	// done is closed once the write's tries are over.
	done chan struct{}
	// failed is the last try's error where none landed; set before done closes.
	failed error
	// write is the write, made again by the seat's release.
	write func(ctx context.Context) error
}

func newConnectMarks() *connectMarks { return &connectMarks{runs: map[string]*connectMark{}} }

// record holds runID's connection, on seat, until write lands: tried at once
// and after each of [appendRetryWaits] on an error, beside the caller.
func (m *connectMarks) record(base context.Context, runID, seat string, write func(context.Context) error) {
	if m == nil {
		_ = write(base)
		return
	}
	mark := &connectMark{seat: seat, done: make(chan struct{}), write: write}
	m.mu.Lock()
	m.runs[runID] = mark
	m.mu.Unlock()
	go func() {
		var err error
		for attempt := 1; ; attempt++ {
			if err = write(base); err == nil || attempt >= appendAttempts ||
				!sleepCtx(base, appendRetryWaits[attempt-1]) {
				break
			}
		}
		m.mu.Lock()
		if err == nil && m.runs[runID] == mark {
			delete(m.runs, runID)
		}
		mark.failed = err
		m.mu.Unlock()
		close(mark.done)
	}()
}

// settle waits for runID's write under way here, on its completion or ctx's
// end (ctx's error), and answers whether this node saw the harness connect
// with no write of it landed: the caller then makes it. The answer goes with
// what it says, so the next settle asks the store alone.
func (m *connectMarks) settle(ctx context.Context, runID string) (unrecorded bool, err error) {
	if m == nil {
		return false, nil
	}
	m.mu.Lock()
	mark := m.runs[runID]
	m.mu.Unlock()
	if mark == nil {
		return false, nil
	}
	select {
	case <-mark.done:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs[runID] != mark {
		return false, nil
	}
	delete(m.runs, runID)
	return mark.failed != nil, nil
}

// release waits for every write under way of seat's runs here, on its
// completion or ctx's end, tries each failed one once more under ctx, and
// forgets them all: the seat's next holder collects its runs.
func (m *connectMarks) release(ctx context.Context, seat string, log *slog.Logger) {
	if m == nil {
		return
	}
	m.mu.Lock()
	marks := map[string]*connectMark{}
	for runID, mark := range m.runs {
		if mark.seat == seat {
			marks[runID] = mark
			delete(m.runs, runID)
		}
	}
	m.mu.Unlock()
	for runID, mark := range marks {
		select {
		case <-mark.done:
		case <-ctx.Done():
			log.WarnContext(ctx, "executor_bridge_connect_unrecorded", "run_id", runID, "seat", seat,
				"error", ctx.Err().Error(), "detail", "the seat left this node before the record that the "+
					"run's harness connected its bridge was written: its collection reads it as a run whose "+
					"harness never did, and fails it")
			continue
		}
		if mark.failed == nil {
			continue
		}
		if err := mark.write(ctx); err != nil {
			log.WarnContext(ctx, "executor_bridge_connect_unrecorded", "run_id", runID, "seat", seat,
				"error", err.Error(), "detail", "the seat left this node with the record that the run's "+
					"harness connected its bridge unwritten: its collection reads it as a run whose harness "+
					"never did, and fails it")
		}
	}
}
