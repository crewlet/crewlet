package statelog_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE IDENTITY FOR EVERY DOMAIN THAT TAKES A KEY, and the field only one of
// them fills adds nothing where it is empty. The hoist is safe only if the
// wiki's ids are what they were — its keys name no tool — and a key that names
// a tool still carries it.
func TestACallKeysIdentityAddsOnlyWhatItCarries(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		key  statelog.CallKey
		want []string
	}{
		"a bare key: the namespace, the seed and the parts": {
			key:  statelog.CallKey{Seed: " turn-7 "},
			want: []string{"ns", "turn-7", "save", "p1"},
		},
		"a tool, after the seed and before the parts": {
			key:  statelog.CallKey{Seed: "turn-7", Tool: " post_message "},
			want: []string{"ns", "turn-7", "post_message", "save", "p1"},
		},
		"the arguments and the count, last": {
			key:  statelog.CallKey{Seed: "turn-7", Args: "d1", Repeat: 2},
			want: []string{"ns", "turn-7", "save", "p1", "args:d1", "repeat:2"},
		},
	} {
		if got := c.key.Identity("ns", "save", "p1"); !slices.Equal(got, c.want) {
			t.Errorf("%s: Identity = %q, want %q", name, got, c.want)
		}
	}
}

// A KEY'S OPERATION CARRIES THE UNIT OF WORK'S INSTANT, never the call's, so
// the publisher reads when the work began when it asks whether its ledger can
// vouch for a retry — and two repetitions of one call derive one id.
func TestACallKeysOperationCarriesItsSeedsInstant(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	key := statelog.CallKey{Seed: "turn-7", Since: since, Tool: "post_message"}
	first, again := key.DeriveOpID("ns", "post", "room"), key.DeriveOpID("ns", "post", "room")
	if first != again {
		t.Fatalf("one call derived %q and then %q", first, again)
	}
	at, ok := statelog.OpMintedAt(first)
	if !ok || !at.Equal(since) {
		t.Errorf("the operation carries %v (%v), want the seed's own %v", at, ok, since)
	}
	if other := key.DeriveOpID("ns", "post", "another room"); other == first {
		t.Errorf("two different writes under one key derived one operation %q", first)
	}
	if !(statelog.CallKey{Seed: "  "}).Empty() || key.Empty() {
		t.Error("Empty does not tell a key that names no unit of work from one that does")
	}
}
