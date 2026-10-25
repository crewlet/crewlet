package hostbox

// SetSettleAttempts scales how many execs Settle tries, so the refusal of a
// file held open for writing is reached in milliseconds rather than the five
// seconds production waits. It returns the previous value for t.Cleanup.
func SetSettleAttempts(n int) (previous int) {
	previous, settleAttempts = settleAttempts, n
	return previous
}
