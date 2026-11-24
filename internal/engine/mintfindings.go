package engine

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/integration"
)

// Every reading of what an integration minting a credential last recorded
// ([Engine.mintingStates]) runs on a path that keeps the read term: the
// beat's MCP rows, the boot's and an apply's shared servers, a seat's
// children and a run's launch. So it reads each status by its key and never a
// findings record (ADR-0175, "The integrations bucket declares its classes"),
// whose crossing at its bound would move each of those windows. Where a person
// owes a finding the judgement needs, the findings are the copy held here,
// under the digest the status names, and a newer record is read OFF the
// caller's path on this holder's own context; that reading judges the kind
// as it cannot say yet ([integration.State.FindingsReading]), no failure, and
// the next (the beat's, a renew period later) has it. A read that failed is
// said as one, until a later read of that digest lands.

// mintFindings holds the findings records [Engine.mintingStates] judges by,
// each under its kind and the digest its status named.
type mintFindings struct {
	mu sync.Mutex
	// held is the last record read per kind, and fetching the kinds a read
	// is in flight for.
	held     map[integration.Kind]heldFindings
	fetching map[integration.Kind]bool
	// failed is the digest of each kind whose last read failed.
	failed map[integration.Kind]string
	// ctx is the reads' own, which stop cancels; wg counts the reads in
	// flight, which stop waits for. stopped refuses a read once stop began.
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
	// fetched, where set, is told each read's end (tests).
	fetched func(integration.Kind)
}

// heldFindings is one record as read, under the digest it was named by.
type heldFindings struct {
	sha256   string
	findings []integration.Finding
}

// readFindings reads the findings a state's status names.
type readFindings func(ctx context.Context, state integration.State) (integration.State, error)

// attach is state with the findings its status names, where this holder has
// that record, and read as unread where it does not, with a read of it by
// read started off the caller's path: still being read, unless the last read
// of that record failed.
func (m *mintFindings) attach(state integration.State, read readFindings) integration.State {
	rec := state.FindingsRecord
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.held[state.Kind]; ok && held.sha256 == rec.SHA256 {
		state.Findings = held.findings
		return state
	}
	if !m.stopped && !m.fetching[state.Kind] {
		if m.ctx == nil {
			m.ctx, m.cancel = context.WithCancel(context.Background())
		}
		if m.fetching == nil {
			m.fetching = map[integration.Kind]bool{}
		}
		m.fetching[state.Kind] = true
		m.wg.Add(1)
		go m.fetch(m.ctx, state, read)
	}
	state.FindingsUnread = true
	state.FindingsReading = m.failed[state.Kind] != rec.SHA256
	return state
}

// fetch reads state's findings and holds them under its digest, where it
// read the record its status names.
func (m *mintFindings) fetch(ctx context.Context, state integration.State, read readFindings) {
	defer m.wg.Done()
	got, err := read(ctx, state)
	m.mu.Lock()
	delete(m.fetching, state.Kind)
	switch {
	case err == nil && !got.FindingsUnread:
		if m.held == nil {
			m.held = map[integration.Kind]heldFindings{}
		}
		m.held[state.Kind] = heldFindings{sha256: state.FindingsRecord.SHA256, findings: got.Findings}
		delete(m.failed, state.Kind)
	case ctx.Err() == nil:
		if m.failed == nil {
			m.failed = map[integration.Kind]string{}
		}
		m.failed[state.Kind] = state.FindingsRecord.SHA256
	}
	told := m.fetched
	m.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "integration_findings_unread", "integration", state.Kind.String(),
			"error", err, "detail", "a credential it mints is judged as last judged until a later read")
	}
	if told != nil {
		told(state.Kind)
	}
}

// stop cancels the reads in flight and waits for them; none starts after.
func (m *mintFindings) stop() {
	m.mu.Lock()
	m.stopped = true
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}
