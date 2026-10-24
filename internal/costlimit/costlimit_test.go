package costlimit_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/costlimit"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

var rates = costlimit.Rates{PrincipalRate: 2, PrincipalBurst: 4, ScopeRate: 5, ScopeBurst: 6}

func TestOnePrincipalCannotTakeMoreThanItsBucket(t *testing.T) {
	c := &clock{at: time.Unix(1_700_000_000, 0)}
	l := costlimit.New(rates, c.now)
	for i := range 4 {
		if r := l.Take("s", "ana", 1); r != nil {
			t.Fatalf("charge %d refused within the burst: %+v", i, r)
		}
	}
	r := l.Take("s", "ana", 1)
	if r == nil || r.Scope != costlimit.ScopePrincipal {
		t.Fatalf("the fifth charge = %+v, want a refusal of the principal's bucket", r)
	}
	if r.RetryAfter != 500*time.Millisecond {
		t.Fatalf("RetryAfter = %s, want 500ms at two a second", r.RetryAfter)
	}
	c.at = c.at.Add(500 * time.Millisecond)
	if r := l.Take("s", "ana", 1); r != nil {
		t.Fatalf("a refilled bucket refused: %+v", r)
	}
}

func TestEverybodyTogetherCannotTakeMoreThanTheScope(t *testing.T) {
	c := &clock{at: time.Unix(1_700_000_000, 0)}
	l := costlimit.New(rates, c.now)
	for i, who := range []string{"a", "b", "c", "d", "e", "f"} {
		if r := l.Take("s", who, 1); r != nil {
			t.Fatalf("charge %d refused within the scope's burst: %+v", i, r)
		}
	}
	r := l.Take("s", "g", 1)
	if r == nil || r.Scope != costlimit.ScopeShared {
		t.Fatalf("the seventh principal's charge = %+v, want the scope's refusal", r)
	}
	if r := l.Take("other", "g", 1); r != nil {
		t.Fatalf("another scope paid into the first one's bucket: %+v", r)
	}
}

func TestARefusedChargeTakesNothingFromEitherBucket(t *testing.T) {
	c := &clock{at: time.Unix(1_700_000_000, 0)}
	l := costlimit.New(rates, c.now)
	if r := l.Take("s", "ana", 4); r != nil {
		t.Fatalf("a whole bucket refused when full: %+v", r)
	}
	// The principal is empty; its refusal must not drain the scope.
	if r := l.Take("s", "ana", 1); r == nil {
		t.Fatal("an empty principal bucket admitted a charge")
	}
	if r := l.Take("s", "bo", 2); r != nil {
		t.Fatalf("the scope lost what a refused charge never paid: %+v", r)
	}
}

func TestAChargeHeavierThanABucketWeighsAWholeBucket(t *testing.T) {
	c := &clock{at: time.Unix(1_700_000_000, 0)}
	l := costlimit.New(rates, c.now)
	if r := l.Take("s", "ana", 100); r != nil {
		t.Fatalf("a heavy charge on a full bucket refused for ever: %+v", r)
	}
	if r := l.Take("s", "ana", 1); r == nil {
		t.Fatal("a heavy charge left tokens behind")
	}
}

func TestAZeroRateIsRefusedAtWiring(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a zero rate was accepted: that bucket never refills")
		}
	}()
	costlimit.New(costlimit.Rates{PrincipalRate: 0, PrincipalBurst: 1, ScopeRate: 1, ScopeBurst: 1}, nil)
}
