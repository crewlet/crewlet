package a2a_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// mintedID opens one channel through the DEFAULT minter and returns its id.
//
// Every other case in this suite injects Options.NewID to pin the id, which
// is what a deterministic assertion about topics and payloads needs — and it
// is also why the real minter had no coverage at all: leaving NewID unset is
// the only way to execute it.
func mintedID(t *testing.T) string {
	t.Helper()
	svc, err := a2a.New(a2a.NewCoordStore(memory.NewFleet()), &recorder{}, a2a.Options{
		Directory: dir{"bob": true},
		Now:       func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return id
}

// The invariant: a minted channel id carries a WHOLE uuid.
//
// The id is the channel's store key — Reply resolves an answer against it and
// speaks to whoever the record names — and the coordination store's Create
// ignores an id that already exists, so a collision is not an error anywhere:
// it silently routes one seat's answer onto another seat's ask. This minted
// `uuid.New().String()[:12]` once, which is eleven hex digits and 44 random
// bits — measured, as the minter's doc says, against the channels the store
// holds AT ONCE, a set an hour's idle timeout and a week's retention bound
// rather than the company's lifetime volume. Small odds, and the wrong
// quantity to be weighing: nothing detects the misroute either way.
//
// Version and variant are asserted, not just the length, because they are
// what make "122 random bits" true: a 36-character string of the right shape
// carrying no randomness would satisfy a length check alone.
func TestAMintedChannelIDCarriesAWholeUUID(t *testing.T) {
	t.Parallel()
	id := mintedID(t)

	rest, ok := strings.CutPrefix(id, "a2a-")
	if !ok {
		t.Fatalf("channel id %q has no a2a- prefix, so nothing says which kind of id it is", id)
	}
	// The CANONICAL dashed form, pinned by length and by a round trip through
	// String(): uuid.Parse also accepts the 32-character undashed spelling, a
	// urn: form and a braced one, so a bare parse would certify any of four
	// spellings for a value that is a STORE KEY and a log field — where one
	// id written two ways is two keys and two conversations.
	if len(rest) != 36 {
		t.Fatalf("channel id %q carries %d characters after the prefix, want the 36 of a dashed uuid", id, len(rest))
	}
	parsed, err := uuid.Parse(rest)
	if err != nil {
		t.Fatalf("channel id %q is not a uuid: %v", id, err)
	}
	if parsed.String() != rest {
		t.Errorf("channel id %q is not the canonical spelling of %s", rest, parsed)
	}
	if got := parsed.Version(); got != 4 {
		t.Errorf("uuid version = %v, want 4 — the 122 random bits the collision argument rests on", got)
	}
	if got := parsed.Variant(); got != uuid.RFC4122 {
		t.Errorf("uuid variant = %v, want RFC4122", got)
	}
}

// The invariant: a minted channel id is a URL segment exactly as it stands.
//
// The dashboard's object routes give a channel its own page at
// `#/activity/a2a/<id>` and hand the id to that path WHOLE, so what the
// minter returns travels in a fragment and in a path segment with nothing in
// between to encode it. That route is the minter's own argument for spending
// the extra 24 bytes — a URL that carries the id whole is why the 36-character
// form costs nothing — and a claim about a route that nothing checks is the
// defect this case exists to prevent.
//
// RFC 3986's unreserved set is the bar rather than url.PathEscape's, because
// PathEscape leaves the sub-delims ($&+,;=) and ':@' alone: it would certify a
// prefix carrying a character a router splits on or a reader has to
// percent-decode. PathEscape is asserted too, as the weaker restatement in the
// vocabulary the standard library uses.
func TestAMintedChannelIDNeedsNoURLEscaping(t *testing.T) {
	t.Parallel()
	id := mintedID(t)
	for i, r := range id {
		unreserved := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' || r == '~'
		if !unreserved {
			t.Fatalf("channel id %q carries %q at byte %d, which RFC 3986 does not leave "+
				"unreserved: the #/activity/a2a/<id> route would have to encode or split it", id, r, i)
		}
	}
	if escaped := url.PathEscape(id); escaped != id {
		t.Errorf("url.PathEscape(%q) = %q, so the id is not a path segment as it stands", id, escaped)
	}
}

// The counterfactual for the whole-uuid case: a minter that always answered
// the same value would pass every shape assertion there.
func TestTwoAsksGetDifferentChannelIDs(t *testing.T) {
	t.Parallel()
	if first, second := mintedID(t), mintedID(t); first == second {
		t.Fatalf("two asks both opened channel %q, so one answer can reach the other's ask", first)
	}
}
