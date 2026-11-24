package executor

import (
	"strings"
	"testing"
)

// A SESSION'S NONCE IS SIX HEX DIGITS OF ITS OWN, THE SAME ON EVERY RUN OF IT:
// each run that continues a session names its servers as its first did, and
// two sessions' names differ.
//
// Mutation: draw the nonce at random, or ignore the session.
func TestASessionsNonceIsItsOwnOnEveryRun(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for i := range 64 {
		id := "0b5e0000-0000-4000-8000-0000000000" + string(rune('a'+i/16)) + string("0123456789abcdef"[i%16])
		n := sessionNonce(id)
		if len(n) != 6 || strings.Trim(n, "0123456789abcdef") != "" || n != sessionNonce(id) {
			t.Fatalf("session %s: nonce %q, want six hex digits, the same each run", id, n)
		}
		if other, ok := seen[n]; ok {
			t.Fatalf("sessions %s and %s share the nonce %s", other, id, n)
		}
		seen[n] = id
	}
}
