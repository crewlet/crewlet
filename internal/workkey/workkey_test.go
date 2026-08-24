package workkey

import (
	"context"
	"testing"
)

// TestDeriveIsOrderIndependent is the property the whole design rests on:
// two nodes handed the same batch in different orders must derive the same
// key, or the ledger records the duplicate it exists to collapse.
func TestDeriveIsOrderIndependent(t *testing.T) {
	t.Parallel()
	a := Derive([]string{"evt-b", "evt-a", "evt-c"})
	b := Derive([]string{"evt-c", "evt-b", "evt-a"})
	if a != b {
		t.Errorf("order changed the key: %q vs %q", a, b)
	}
	if len(a) != keyChars {
		t.Errorf("key length = %d, want %d", len(a), keyChars)
	}
}

func TestDeriveDeduplicates(t *testing.T) {
	t.Parallel()
	if one, two := Derive([]string{"evt-a"}), Derive([]string{"evt-a", "evt-a"}); one != two {
		t.Errorf("duplicate id changed the key: %q vs %q", one, two)
	}
}

// TestDeriveDistinguishesSets guards the other direction: a partially
// overlapping redelivery must NOT collide with the batch it overlaps, or the
// ledger would skip constituents that were never worked.
func TestDeriveDistinguishesSets(t *testing.T) {
	t.Parallel()
	ab := Derive([]string{"evt-a", "evt-b"})
	abc := Derive([]string{"evt-a", "evt-b", "evt-c"})
	if ab == abc {
		t.Error("different trigger sets derived the same key")
	}
}

// TestDeriveEmptyIsUnconstrained pins the honest default: a turn with no
// ledgerable trigger has no cross-node duplicate to collapse, and must not
// receive a hash of nothing that a second such turn would also derive.
func TestDeriveEmptyIsUnconstrained(t *testing.T) {
	t.Parallel()
	for _, in := range [][]string{nil, {}, {""}, {"  ", ""}} {
		if got := Derive(in); got != "" {
			t.Errorf("Derive(%q) = %q, want empty", in, got)
		}
	}
}

func TestDeriveIgnoresBlankIDs(t *testing.T) {
	t.Parallel()
	if with, without := Derive([]string{"evt-a", ""}), Derive([]string{"evt-a"}); with != without {
		t.Errorf("a blank id changed the key: %q vs %q", with, without)
	}
}

func TestContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := With(context.Background(), "abc123")
	if got := From(ctx); got != "abc123" {
		t.Errorf("From = %q, want abc123", got)
	}
	if got := From(context.Background()); got != "" {
		t.Errorf("unbound context yielded %q, want empty", got)
	}
	//nolint:staticcheck // deliberately asserting the nil-context guard
	if got := From(nil); got != "" {
		t.Errorf("nil context yielded %q, want empty", got)
	}
}
