package coordtest

import (
	"errors"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// leaseMetaCases hold every backend to ONE bound on what a lease carries
// beside itself, [coord.MaxLeaseMetaBytes], and to one answer past it,
// [coord.ErrValueTooLarge]: refused before anything is written, never as an
// outage, and the resource left exactly as it was. A holder re-sends its Meta
// on every renewal, so the bound is what bounds the renewal; a twin that
// stored a larger one would certify a presence the store refuses.
var leaseMetaCases = []testCase{
	{"a_claim_whose_meta_is_past_what_a_lease_carries_is_refused_and_takes_nothing", func(h *harness) {
		node := coord.NodeResource("n1")
		past := metaOf(h, coord.MaxLeaseMetaBytes+1)
		lease, refused, err := h.b.TryAcquire(h.ctx, node, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: past})
		h.metaTooLarge("a claim", err)
		if lease != nil || refused != "" {
			h.t.Fatalf("a claim past the bound answered (%+v, %q) beside its error", lease, refused)
		}
		h.mustBeUnheld(node)

		// THE HOLDER'S OWN RE-CLAIM, which is how a presence renews: past
		// the bound it is refused, and the lease it holds is untouched —
		// still held, at its epoch, carrying what it carried.
		small := map[string]any{"roles": []any{"seats"}}
		held := h.claim(node, coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: small})
		_, _, err = h.b.TryAcquire(h.ctx, node, coord.AcquireOptions{
			Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: past})
		h.metaTooLarge("a holder's re-claim", err)
		after := h.mustHold(node, "n1:a")
		if after.Epoch != held.Epoch {
			h.t.Fatalf("a re-claim refused for its meta moved the epoch %d -> %d", held.Epoch, after.Epoch)
		}
		h.requireSameValues("the meta after a refused re-claim", small, after.Meta)
	}},

	{"a_claim_whose_meta_is_exactly_what_a_lease_carries_is_taken_whole", func(h *harness) {
		node := coord.NodeResource("n1")
		at := metaOf(h, coord.MaxLeaseMetaBytes)
		h.claim(node, coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL, Ungated: true, Meta: at})
		h.requireSameValues("a meta at the bound, read back", at, h.mustHold(node, "n1:a").Meta)
	}},
}

// metaOf is a Meta that encodes, as the store encodes a lease's, to exactly
// size bytes — measured by [coord.LeaseMetaBytes] rather than assumed, so a
// case named for the bound is at it.
func metaOf(h *harness, size int) map[string]any {
	h.t.Helper()
	const frame = `{"pad":""}`
	meta := map[string]any{"pad": strings.Repeat("x", size-len(frame))}
	if got, err := coord.LeaseMetaBytes(meta); err != nil || got != size {
		h.t.Fatalf("the case's meta encodes to %d bytes (%v), want %d", got, err, size)
	}
	return meta
}

// metaTooLarge fails the case unless err refuses a Meta past what a lease
// carries, naming the bound, and reads as nothing an outage or a landed write
// would.
func (h *harness) metaTooLarge(what string, err error) {
	h.t.Helper()
	switch {
	case !errors.Is(err, coord.ErrValueTooLarge):
		h.t.Fatalf("%s whose meta is past what a lease carries = %v, want coord.ErrValueTooLarge",
			what, err)
	case errors.Is(err, coord.ErrUnavailable), errors.Is(err, coord.ErrWriteUnknown):
		h.t.Fatalf("%s whose meta is past what a lease carries = %v: a refusal made before "+
			"anything was sent read as an outage", what, err)
	case !strings.Contains(err.Error(), strconv.Itoa(coord.MaxLeaseMetaBytes)):
		h.t.Fatalf("%s whose meta is past what a lease carries = %v, which does not name the "+
			"bound, %d", what, err, coord.MaxLeaseMetaBytes)
	}
}
