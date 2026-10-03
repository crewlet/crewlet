package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
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

// NOTHING IN FLIGHT IS HELD ONCE IT RESOLVES.
//
// An admitted attempt is counted apart from the bounded memory of failures,
// and what keeps that count from growing for the life of the process is that
// every way a ticket resolves — a failure, a success, a release — takes its
// attempt back out of it. Mutation: leave the attempt counted on any one of
// the three and an entry outlives the request it stood for.
func TestNothingInFlightIsHeldOnceItResolves(t *testing.T) {
	t.Parallel()
	th := NewThrottle(ThrottleDeps{Sleep: func(context.Context, time.Duration) {}})
	resolve := []func(*Ticket){
		func(k *Ticket) { k.Fail() },
		func(k *Ticket) { k.Succeed() },
		func(k *Ticket) { k.Release() },
	}
	var tickets []*Ticket
	for i := range 30 {
		ticket, err := th.Admit(t.Context(), Attempt{Source: "203.0.113.7",
			Subject: fmt.Sprintf("name.%d", i%10)})
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, ticket)
	}
	for i, ticket := range tickets {
		resolve[i%len(resolve)](ticket)
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.pending) != 0 {
		t.Errorf("with every attempt resolved, %d pairs still count attempts "+
			"in flight", len(th.pending))
	}
}
