package engine

import (
	"context"
	"sync"
)

// connectMarks is every job whose harness's bridge connection this node is
// still writing to its row, by run ([bridgeLedger.connected]). The write is
// made OFF the harness's list, which a row write under load (one to three
// seconds in a fleet) would hold back from its first model request, and past
// Grok Build's two-second wait for the handshake; and the collection waits for
// a write under way here before it reads the row
// ([sandbox.CoordinatorOptions.ConnectSettled]), so a run that listed is never
// read as one that did not.
type connectMarks struct {
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func newConnectMarks() *connectMarks { return &connectMarks{inflight: map[string]chan struct{}{}} }

// begin records runID's write under way, and returns what ends it.
func (m *connectMarks) begin(runID string) (end func()) {
	if m == nil {
		return func() {}
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.inflight[runID] = done
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.inflight[runID] == done {
			delete(m.inflight, runID)
		}
		m.mu.Unlock()
		close(done)
	}
}

// settle waits for runID's write under way on this node, if there is one, or
// for ctx; the write is bounded by its own budget ([bridgeLedger.recordConnected]).
func (m *connectMarks) settle(ctx context.Context, runID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	done := m.inflight[runID]
	m.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}
