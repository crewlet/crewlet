package auxspend

import "log/slog"

// NewLedgerBounded is [NewLedger] keeping at most bound refused records and
// logging to logger. The backlog cases fill and overflow a bound of a few
// records rather than [MaxPending]'s 4096, which they reached one refused
// flush at a time, each flush copying the whole backlog — and logging a
// warning apiece into the suite's output.
func NewLedgerBounded(pub Publisher, bound int, logger *slog.Logger) *Ledger {
	l := NewLedger(pub)
	l.maxPending = bound
	l.logger = logger
	return l
}

// PendingBound is the most refused records l keeps.
func PendingBound(l *Ledger) int { return l.maxPending }
