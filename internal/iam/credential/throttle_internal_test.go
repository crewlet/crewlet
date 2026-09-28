package credential

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// THE THROTTLE NEVER HOLDS WHAT WAS TYPED.
//
// A password typed into the login box is what lands in the subject often
// enough to matter, and the throttle holds a pair for a whole window. Held in
// the clear, or hashed without a key, it is recoverable from this process's
// memory against the company's own roster in one pass. Mutation: key a pair on
// the typed value, or on its unkeyed SHA-256, and a held key carries it.
func TestTheThrottleNeverHoldsWhatWasTyped(t *testing.T) {
	t.Parallel()
	th := NewThrottle(ThrottleDeps{})
	typed := "correct horse battery staple"
	ticket, err := th.Admit(t.Context(), Attempt{Source: "203.0.113.7", Subject: typed})
	if err != nil {
		t.Fatal(err)
	}
	ticket.Fail()

	unkeyed := sha256.Sum256([]byte(typed))
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.pairs.len() != 1 {
		t.Fatalf("the throttle holds %d pairs for one failed attempt", th.pairs.len())
	}
	for key := range th.pairs.byKey {
		if strings.Contains(key, "correct") || strings.Contains(key, "203.0.113.7") ||
			strings.Contains(key, hex.EncodeToString(unkeyed[:8])) {
			t.Errorf("the throttle holds %q, which carries what was typed or "+
				"where from", key)
		}
	}
}
