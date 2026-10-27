package coalesce

// NewBounded is [New] keeping at most bound refused records. The backlog cases
// fill and overflow a bound of a few records rather than [MaxPending]'s 4096,
// which they would reach one refused flush at a time, each flush copying the
// whole backlog.
func NewBounded[K comparable, B any](pub Publisher, r Records[K, B], bound int) *Ledger[K, B] {
	l := New(pub, r)
	l.bound = bound
	return l
}

// PendingBound is the most refused records l keeps.
func PendingBound[K comparable, B any](l *Ledger[K, B]) int { return l.bound }
