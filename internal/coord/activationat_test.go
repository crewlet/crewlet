package coord_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN ACTIVATION IS PUBLISHED LATER THAN THE ONE IT REPLACES, and still later
// once a node has stored its copy at the store's microseconds.
//
// Later by a few microseconds on the pointer would round to the same instant
// wherever a coarser copy is compared, and a node comparing its stored
// `activated_at` against the pointer's would read two activations as one — or
// the replaced one as the newer.
//
// Mutation: compare in nanoseconds, or keep the request, and a case fails.
func TestTheActivationPointersForwardRuleIsLaterWhereverItIsKept(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 14, 15, 9, 26, 500_000, time.UTC)
	for _, requested := range []time.Time{
		base.Add(-time.Hour), base, base.Add(300 * time.Microsecond),
		base.Add(999 * time.Microsecond),
	} {
		got := coord.ActivationAt(requested, base)
		stored := got.Truncate(time.Microsecond)
		if got.UnixMilli() <= base.UnixMilli() || !stored.After(base.Truncate(time.Microsecond)) {
			t.Errorf("an activation asking for %s after one at %s is published at %s "+
				"— no later than the replaced one where it is kept", requested, base, got)
		}
	}
	if later := base.Add(time.Second); !coord.ActivationAt(later, base).Equal(later) {
		t.Error("a genuinely later activation was moved")
	}
	if first := base; !coord.ActivationAt(first, time.Time{}).Equal(first) {
		t.Error("the first activation was moved, with nothing before it")
	}
}

// AN ACTIVATION IS SIZED AT ITS WIDEST, so a pointer the size passes is one
// every store takes whatever instant it decides.
//
// A store publishes an activation at the request's own instant or a
// millisecond past the one it replaces (ActivationAt), and an instant encodes
// longer the more of its fraction is set: a request on a whole second is
// "…:26Z", and the instant a store moves it to is "…:26.001Z". A caller
// holding a write to what one key holds before it commits (a config write)
// sizes the pointer with ActivationBytes, which every later instant the store
// may decide encodes no longer than.
//
// Mutation: size the pointer at the request's own instant, and the instant a
// store decides encodes longer than it was sized at.
func TestAnActivationIsSizedAtItsWidest(t *testing.T) {
	t.Parallel()
	request := coord.ActivationRequest{
		RevisionID: "00000000-0000-0000-0000-000000000000", Summary: "a summary",
		At: time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC),
		Origin: coord.RevisionOrigin{AuthorKind: "operator", Source: "api",
			CreatedAt: time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)},
	}
	payload := []byte(`{"__encrypted__":"enc:v1:k1:abcd"}`)
	sized, err := coord.ActivationBytes(request, payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, previous := range []time.Time{
		request.At, request.At.Add(123_456_789), request.At.Add(time.Second - time.Nanosecond),
	} {
		at := coord.ActivationAt(request.At, previous)
		stored, err := coord.ActivationValue(request, payload, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) > sized {
			t.Errorf("a pointer a store publishes at %s is %d bytes, past the %d it was sized at",
				at.Format(time.RFC3339Nano), len(stored), sized)
		}
	}
}

// AN ACTIVATION'S ORIGIN IS SIZED AT ITS WIDEST TOO, so a config write's dry
// run, which sizes the pointer its write would make, answers for that write.
//
// The write follows its dry run at a later instant, and the instant stamps the
// pointer's origin as well as the pointer: an origin on a whole second is
// "…:26Z", one a moment later "…:26.123456789Z". A pointer sized at the dry
// run's instant is one the write's own instant cannot lengthen.
//
// Mutation: size the origin at its own instant, and the write a moment after
// its dry run makes a pointer past the size the dry run passed.
func TestAnActivationsOriginIsSizedAtItsWidest(t *testing.T) {
	t.Parallel()
	checked := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	request := coord.ActivationRequest{
		RevisionID: "00000000-0000-0000-0000-000000000000", Summary: "a summary", At: checked,
		Origin: coord.RevisionOrigin{AuthorKind: "operator", Source: "api", CreatedAt: checked},
	}
	payload := []byte(`{"__encrypted__":"enc:v1:k1:abcd"}`)
	sized, err := coord.ActivationBytes(request, payload)
	if err != nil {
		t.Fatal(err)
	}
	written := checked.Add(123_456_789)
	request.At, request.Origin.CreatedAt = written, written
	stored, err := coord.ActivationValue(request, payload, written)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) > sized {
		t.Errorf("a pointer written at %s is %d bytes, past the %d its dry run at %s sized",
			written.Format(time.RFC3339Nano), len(stored), sized, checked.Format(time.RFC3339Nano))
	}

	// AND AN ORIGIN THAT NAMES NO INSTANT STILL ENCODES NONE: sized at the
	// widest, a zero instant would count an instant the pointer never carries.
	request.Origin.CreatedAt = time.Time{}
	sized, err = coord.ActivationBytes(request, payload)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = coord.ActivationValue(request, payload,
		written.Truncate(time.Second).Add(time.Second-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != sized {
		t.Errorf("a pointer whose origin names no instant is %d bytes at its widest, sized at %d",
			len(stored), sized)
	}
}
