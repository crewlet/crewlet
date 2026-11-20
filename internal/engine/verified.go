package engine

import (
	"sync"
	"time"
)

// VerifiedLinger is how long a model's verified answer stands for its own:
// past it, a model none of whose calls answered reads untested again.
const VerifiedLinger = time.Hour

// verifiedAnswers is, for each model entry, when a real run of its harness
// last answered on a test of its credential: the wizard's test of a model
// being added, or a check that passed. Held by this node, in memory.
type verifiedAnswers struct {
	mu sync.Mutex
	at map[string]time.Time
}

// Verified records that the entry key's credential answered a real run of its
// harness at `at`, on a test rather than on one of the model's own calls.
//
// A MODEL SAVED AFTER ITS TEST PASSED READ "NOT TESTED": the test runs in a
// fresh home, so the model's own sign-in never hears it, and the list counted
// a model that had just answered as never having answered. Its state takes
// this answer as its last until its own calls say more, for [VerifiedLinger].
func (e *Engine) Verified(key string, at time.Time) {
	v := &e.verified
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.at == nil {
		v.at = map[string]time.Time{}
	}
	if at.After(v.at[key]) {
		v.at[key] = at.UTC()
	}
}

// verifiedAt is the entry's verified answer, zero where none stands now.
func (e *Engine) verifiedAt(key string, now time.Time) time.Time {
	v := &e.verified
	v.mu.Lock()
	defer v.mu.Unlock()
	at := v.at[key]
	if at.IsZero() || now.Sub(at) > VerifiedLinger {
		return time.Time{}
	}
	return at
}
