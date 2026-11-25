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

// leaseNameCases hold every backend to [coord.MaxLeaseNameBytes] on each name
// a lease's record holds, as stored: a lease naming each at it beside a Meta
// at its bound is taken whole, the widest record the class must hold, and one
// naming any past it, a byte JSON writes as six counted as six, is refused as
// too large before anything is written.
//
// Mutation: drop the owner's HoldLeaseName from a backend's validate, and its
// past case is granted.
var leaseNameCases = []testCase{
	{"a_lease_naming_each_name_at_its_bound_is_taken_whole_and_one_past_is_refused", func(h *harness) {
		name := func(n int) string { return strings.Repeat("n", n) }
		resource := coord.NodeResource(name(coord.MaxLeaseNameBytes - len(coord.NodeResource(""))))
		widest := coord.AcquireOptions{Owner: name(coord.MaxLeaseNameBytes), TTL: LongTTL, Ungated: true,
			Preferred: name(coord.MaxLeaseNameBytes), Meta: metaOf(h, coord.MaxLeaseMetaBytes)}
		h.claim(resource, widest)
		if got := h.mustHold(resource, widest.Owner); got.Preferred != widest.Preferred {
			h.t.Fatalf("the widest lease reads back preferring %q, want %q", got.Preferred, widest.Preferred)
		}

		escaped := strings.Repeat("\x01", coord.MaxLeaseNameBytes/6+1)
		for what, c := range map[string]struct {
			resource string
			opts     coord.AcquireOptions
		}{
			"resource": {resource + "n", coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL, Ungated: true}},
			"owner":    {coord.NodeResource("n2"), coord.AcquireOptions{Owner: widest.Owner + "n", TTL: LongTTL, Ungated: true}},
			"escaped owner": {coord.NodeResource("n2"),
				coord.AcquireOptions{Owner: escaped, TTL: LongTTL, Ungated: true}},
			"preferred node": {coord.NodeResource("n2"), coord.AcquireOptions{Owner: "n2:a", TTL: LongTTL,
				Ungated: true, Preferred: widest.Preferred + "n"}},
		} {
			lease, _, err := h.b.TryAcquire(h.ctx, c.resource, c.opts)
			if !errors.Is(err, coord.ErrValueTooLarge) || errors.Is(err, coord.ErrUnavailable) ||
				!strings.Contains(err.Error(), strconv.Itoa(coord.MaxLeaseNameBytes)) {
				h.t.Errorf("a claim whose %s is past its bound = %v, want coord.ErrValueTooLarge naming %d",
					what, err, coord.MaxLeaseNameBytes)
			}
			if lease != nil {
				h.t.Errorf("a claim whose %s is past its bound was granted", what)
			}
		}
		h.mustBeUnheld(coord.NodeResource("n2"))
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
