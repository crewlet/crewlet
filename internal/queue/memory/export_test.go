package memory

// WithLingerClock builds a client whose linger windows are timed on clock
// rather than on the wall clock ([lingerClock]).
//
// It exists for the conformance suite, and only this package's tests can name
// it: it is the twin's answer to Capabilities.LingerClock, which builds a queue
// on a clock the case moves, so a case ends a window after a verb rather than
// racing one.
func WithLingerClock(clock lingerClock) Option {
	return func(q *Queue) { q.clock = clock }
}
