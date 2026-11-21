package coord_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN ACTIVATION IS PUBLISHED LATER THAN THE ONE IT REPLACES, and still later
// once a node has stored its copy at the store's microseconds: a few
// microseconds later on the pointer would round to the same instant in a
// coarser copy, and a node comparing its stored `activated_at` with the
// pointer's would read two activations as one, or the replaced one as newer.
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
// every store takes whatever instant it decides. A store publishes at the
// request's instant or a millisecond past the one it replaces (ActivationAt),
// and an instant encodes longer the more of its fraction is set ("…:26Z" against
// "…:26.001Z"); a caller holding a write to one key's size (a config write)
// sizes the pointer with ActivationBytes, which no later instant encodes past.
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
	sized, err := coord.ActivationBytes(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, previous := range []time.Time{
		request.At, request.At.Add(123_456_789), request.At.Add(time.Second - time.Nanosecond),
	} {
		at := coord.ActivationAt(request.At, previous)
		stored, err := coord.ActivationValue(request, widest, at)
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
// run answers for the write: the write follows at a later instant, which stamps
// the origin as well as the pointer ("…:26Z" against "…:26.123456789Z").
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
	sized, err := coord.ActivationBytes(request)
	if err != nil {
		t.Fatal(err)
	}
	written := checked.Add(123_456_789)
	request.At, request.Origin.CreatedAt = written, written
	stored, err := coord.ActivationValue(request, widest, written)
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
	sized, err = coord.ActivationBytes(request)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = coord.ActivationValue(request, widest,
		written.Truncate(time.Second).Add(time.Second-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != sized {
		t.Errorf("a pointer whose origin names no instant is %d bytes at its widest, sized at %d",
			len(stored), sized)
	}
}

// widest names a body at what one key holds: the longest reference a pointer
// carries, which ActivationBytes sizes every pointer with.
var widest = coord.BodyRef{Bytes: coord.MaxValueBytes, SHA256: strings.Repeat("a", 64)}

// A SUMMARY IS MEASURED AS THE POINTER ENCODES IT: a control byte is six
// bytes there, a quote or a backslash two, and HTML is not escaped. Held at
// coord.MaxSummaryBytes, refused a byte past as coord.ErrSummaryTooLong.
//
// Mutation: measure the summary raw, and the control bytes fit when encoded
// they do not.
func TestASummaryIsMeasuredAsThePointerEncodesIt(t *testing.T) {
	t.Parallel()
	for summary, want := range map[string]int{
		"plain":      5,
		"a<b && c>d": 10,
		`say "hi"\`:  12,
		"\x01\n":     8,
		"caf\u00e9":  5,
	} {
		if got := coord.SummaryBytes(summary); got != want {
			t.Errorf("SummaryBytes(%q) = %d, want %d", summary, got, want)
		}
	}
	if err := coord.HoldSummary(strings.Repeat("s", coord.MaxSummaryBytes)); err != nil {
		t.Errorf("a summary at the bound was refused: %v", err)
	}
	escaped := strings.Repeat("\x01", coord.MaxSummaryBytes/6+1)
	if err := coord.HoldSummary(escaped); !errors.Is(err, coord.ErrSummaryTooLong) {
		t.Errorf("a summary of %d control bytes, %d encoded, = %v, want coord.ErrSummaryTooLong",
			len(escaped), coord.SummaryBytes(escaped), err)
	}
}

// A POINTER WITH A SUMMARY AT ITS BOUND AND A FULL ORIGIN FITS ITS OWN BOUND,
// so a write the summary bound admits is one both backends take: the pointer
// bound is the backstop no valid writer reaches.
//
// Mutation: raise coord.MaxSummaryBytes to the pointer's bound, and it does
// not fit.
func TestAPointerWithASummaryAtItsBoundFits(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	size, err := coord.ActivationBytes(coord.ActivationRequest{
		RevisionID: "00000000-0000-0000-0000-000000000000", At: at, Restore: true,
		Summary: strings.Repeat("s", coord.MaxSummaryBytes),
		Origin: coord.RevisionOrigin{Author: strings.Repeat("a", 256), AuthorKind: "operator",
			OperatorID: "pat:" + strings.Repeat("0", 36), Source: "api", CreatedAt: at},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.HoldPointer(size); err != nil {
		t.Errorf("a pointer of %d bytes with its summary at the bound was refused: %v", size, err)
	}
	if err := coord.HoldPointer(coord.MaxPointerBytes + 1); !errors.Is(err, coord.ErrValueTooLarge) {
		t.Errorf("a pointer a byte past its bound = %v, want coord.ErrValueTooLarge", err)
	}
}
