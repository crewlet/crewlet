package schedule

// Rows is every row the twin holds, in no particular order: what a test reads
// to see what a claim recorded.
//
// TEST-ONLY, because nothing in the engine lists a ledger (see [Ledger]): the
// schedules' history is the usage domain's, derived from the SQL backend's
// table. The contract suite reads each backend through a hook its own test
// supplies, and this is the twin's.
func (m *MemoryLedger) Rows() []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Run, 0, len(m.claim))
	for _, run := range m.claim {
		out = append(out, run)
	}
	return out
}
