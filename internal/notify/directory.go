package notify

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// THE DIRECTORY: whether the person holding a human seat may still be reached
// through that seat's contact identities.
//
// The chart alone cannot say (its contact map ignores a suspension), so the
// identity directory adds WHO holds the seat and at what stage: a seat bound to
// somebody who may not act routes nowhere ([Directory], [Standing]).
// A [Standing] is ONE READ, applied to a registry whole and never re-read: a
// directory that moved is a NEW registry swapped in by the engine.
// Chart-only (the zero [Standing]) belongs to no running node, since every
// consuming node must know who may be reached (internal/estate floors a
// non-data node's read); it serves `crewlet validate` and tests.

// Holder is one seat binding, as the identity directory states it.
//
// A FACT, NOT A VERDICT: whether that holder may be reached is this package's
// rule ([Standing]), stated once beside the registry it governs.
type Holder struct {
	// Seat is the bound seat's HANDLE, which is its identity (ADR-0013): a
	// [Standing] finds the seat by it in the organization it is applied to,
	// exactly as the request path finds the same binding's seat.
	Seat string

	// Stage is the bound person's stage. Empty for a removal, which has no
	// row left to have one.
	Stage iam.Stage

	// Removed marks a seat whose holder was removed while holding it, and
	// that nobody has been bound to since that removal.
	Removed bool
}

// Directory is the identity estate as contact routing reads it.
//
// CONSUMER-DEFINED and one method wide, for the package-wide reason: the
// registry needs this one fact and nothing else the estate knows, and a seam
// this narrow is what lets a test hand it a directory with no store behind it
// — and lets the engine hand it either source: internal/iamdomain's reader on
// a data node, which holds the identity estate, or the estate client's
// identity read on a node without the data role, which asks a data node.
//
// AN ERROR IS THE UNKNOWN ARM and never "nobody holds anything": an empty
// answer is a real one, and reading an outage as it would hand every
// suspended person's seat back to the chart.
type Directory interface {
	SeatHolders(ctx context.Context) ([]Holder, error)
}

// Withholding is why a human seat's contact identities are not registered.
//
// A NAMED VALUE rather than a bool, because an operator reading why a person
// stopped being attributed on Slack needs to know which of the four it was —
// and the four have different remedies.
type Withholding string

const (
	// WithheldSuspended is a seat whose holder is suspended. Reinstating
	// them restores it on the same apply.
	WithheldSuspended Withholding = "suspended"

	// WithheldRetired is a seat whose holder has left and whose record is
	// kept.
	WithheldRetired Withholding = "retired"

	// WithheldRemoved is a seat somebody was removed from while holding it,
	// that nobody has been bound to since that removal. Binding its next
	// holder is what routes it again — and what ends the removal's say over
	// the seat for good, so a later holder's move off the seat, or a
	// service account's unbind, hands it back to the chart rather than to
	// the leaver's tombstone.
	WithheldRemoved Withholding = "removed"

	// WithheldUnrecognised is a seat whose holder is at a stage this build
	// cannot name — one a newer peer wrote.
	WithheldUnrecognised Withholding = "unrecognised_stage"

	// WithheldUnread is EVERY human seat on a node that has never managed
	// to read its directory. See [Unread].
	WithheldUnread Withholding = "directory_unread"
)

// withholdings is every [Withholding], in the order a seat with two holders
// reports them: the one that is hardest to undo first, so the reason an
// operator reads is the one that will still be true after they fix the other.
//
// [WithheldUnread] is last because it is never one holder's reason: it is the
// whole reading's, and it never meets another in one seat.
var withholdings = []Withholding{
	WithheldRemoved, WithheldRetired, WithheldSuspended, WithheldUnrecognised,
	WithheldUnread,
}

// Valid reports whether w is one of the five.
func (w Withholding) Valid() bool { return slices.Contains(withholdings, w) }

// harder is whichever of two reasons for one seat an operator should read —
// the one that is harder to undo. The empty reason loses to any other.
func harder(a, b Withholding) Withholding {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case slices.Index(withholdings, b) < slices.Index(withholdings, a):
		return b
	}
	return a
}

// withholding is the rule: whether one holder's seat may still route, and why
// not. An ALLOWLIST of the stages that route, like internal/iam's own.
//
// ACTIVE routes, and so do INVITED and ENROLLING ("not yet": silencing a new
// hire would read as a colleague who never answers). SUSPENDED and RETIRED
// mean "no longer"; a removal is the permanent form.
//
// A STAGE THIS BUILD CANNOT NAME WITHHOLDS (a newer peer wrote it, or none):
// withheld, a person is unreachable for the minutes a roll takes; routed, work
// can reach somebody a newer build had excluded.
func withholding(h Holder) (Withholding, bool) {
	if h.Removed {
		return WithheldRemoved, true
	}
	switch h.Stage {
	case iam.StageActive, iam.StageInvited, iam.StageEnrolling:
		return "", false
	case iam.StageSuspended:
		return WithheldSuspended, true
	case iam.StageRetired:
		return WithheldRetired, true
	}
	return WithheldUnrecognised, true
}

// Standing is one reading of the identity directory: which bindings withhold
// their seat's contact identities, and why. See the package notes above.
//
// # Keyed on the BINDING, found in an organization by HANDLE
//
// A binding names its seat by the seat's handle, which is immutable
// (ADR-0013). So a reading states what each binding says, and [Standing.Seats]
// is what turns that into seats: it finds every binding's seat in the
// organization a registry is being built from by that handle, which is the
// seat the request path finds for the same binding.
//
// ITS ZERO VALUE IS CHART-ONLY: nothing consulted, nothing withheld.
type Standing struct {
	consulted bool
	// unread is a node that has never read its directory — see [Unread].
	unread bool
	// withheld is keyed on the handle each BINDING names.
	withheld map[string]Withholding
}

// StandingOf applies the rule to one reading of the directory.
//
// A seat with more than one holder — a duplicate binding, which only a restore
// can produce and a duty reports — is withheld if ANY of them may not be
// reached: the contact map is one, and there is no way to say which of two
// people it belongs to.
func StandingOf(holders []Holder) Standing {
	s := Standing{consulted: true, withheld: map[string]Withholding{}}
	for _, h := range holders {
		if h.Seat == "" {
			continue
		}
		why, withheld := withholding(h)
		if !withheld {
			continue
		}
		s.withheld[h.Seat] = harder(s.withheld[h.Seat], why)
	}
	return s
}

// Unread is the reading of a node that has NEVER managed to read its
// directory: every human seat is withheld, as [WithheldUnread].
//
// # Fail closed, and only when there is nothing to carry
//
// An unreadable directory normally carries the last reading forward. The
// first registry a node builds has none, and the two answers left are
// asymmetric: chart-only would route every suspended, retired and removed
// holder's accounts while the directory stays unreadable, the direction a
// suspension exists to close; withholding every human seat only makes people
// briefly unreachable, the trade an unnameable stage already makes. The
// periodic re-read replaces it once a read lands.
func Unread() Standing { return Standing{consulted: true, unread: true} }

// ReadStanding reads the directory once and applies the rule.
//
// A NIL DIRECTORY IS CHART-ONLY and no error: it is an engine wired with no
// directory at all, which is a posture rather than a fault. An error from
// the directory is returned as the unknown arm — the caller decides what an
// unreadable directory means for the registry it is building, and the one
// answer this function must never give for it is "nobody is withheld".
func ReadStanding(ctx context.Context, dir Directory) (Standing, error) {
	if dir == nil {
		return Standing{}, nil
	}
	holders, err := dir.SeatHolders(ctx)
	if err != nil {
		return Standing{}, fmt.Errorf("notify: read the identity directory: %w", err)
	}
	return StandingOf(holders), nil
}

// Consulted reports whether this reading came from a directory at all, as
// opposed to the chart-only zero value.
func (s Standing) Consulted() bool { return s.consulted }

// Unread reports whether this is [Unread]'s fail-closed reading.
func (s Standing) Unread() bool { return s.unread }

// Withheld is every BINDING that withholds its seat, by the seat identity it
// names, sorted. [Standing.Seats] is the same reading in terms of an organization's
// seats.
func (s Standing) Withheld() []string {
	return slices.Sorted(maps.Keys(s.withheld))
}

// Seats applies this reading to one organization: which of its seats are
// withheld and why, keyed on each seat's handle.
//
// A binding whose handle names no seat in o withholds nothing here — there is
// no contact map for it to withdraw — and is the dangling residue the identity
// duties report.
func (s Standing) Seats(o *org.Organization) map[string]Withholding {
	out := map[string]Withholding{}
	if o == nil {
		return out
	}
	if s.unread {
		for role := range o.AllRoles() {
			if role.IsHuman() && role.Handle() != "" {
				out[role.Handle()] = WithheldUnread
			}
		}
		return out
	}
	if len(s.withheld) == 0 {
		return out
	}
	for bound, why := range s.withheld {
		if o.Role(bound) != nil {
			out[bound] = harder(out[bound], why)
		}
	}
	return out
}

// Equal reports whether two readings withhold exactly the same bindings for
// the same reasons — which is what lets a directory trigger that found nothing
// moved skip a rebuild rather than swap in an identical registry.
func (s Standing) Equal(o Standing) bool {
	return s.consulted == o.consulted && s.unread == o.unread &&
		maps.Equal(s.withheld, o.withheld)
}
