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
// # Why the org chart alone cannot say
//
// A human seat's contact map — its Slack member id, its Jira account, its
// GitHub login — is ORG-CHART content, and the registry was built from the org
// view and nothing else. So suspending somebody changed nothing here: their
// messages on every vendor surface went on being attributed to a colleague
// seat, a suspended lead's Slack DM to an agent was annotated "your lead says",
// and the only way to stop it was a chart record rewriting the seat's contact
// map — which is a second gesture, by a different person, on a different log,
// that an offboarding can forget.
//
// The identity directory is the other half of the answer: WHO holds the seat,
// and at what stage. A seat bound to somebody who may not act routes nowhere,
// with no chart record at all. That is what [Directory] and [Standing] carry.
//
// # A reading, not a live view
//
// A [Standing] is ONE READ of the directory, taken before a registry is built
// and applied to it whole — the same rule the org half follows. A registry
// never re-reads the directory itself: it answers for the org view and the
// reading it was built from, permanently, and a directory that moved is a NEW
// registry swapped in by the engine. A registry that patched itself from a
// second source would answer "whose standing is this?" with "one of two".
//
// # Chart-only is the zero value, and a real posture
//
// A node that does not run the identity domain — a seats-only satellite —
// holds an empty copy of it, and an empty copy read as "nobody holds any seat"
// is correct only by accident. So such a node supplies NO directory, and the
// zero [Standing] it gets withholds nothing: exactly the routing the chart
// declares, which is what every node did before the directory existed.

// Holder is one seat binding, as the identity directory states it.
//
// A FACT, NOT A VERDICT: whether that holder may be reached is this package's
// rule ([Standing]), stated once beside the registry it governs.
type Holder struct {
	// Seat is the handle the binding names — the handle the seat had WHEN
	// the person was bound to it, because nothing rewrites a binding when
	// the chart renames its seat. A [Standing] resolves it through the
	// organization it is applied to, exactly as the request path resolves
	// the same binding, rather than comparing it to a current handle.
	Seat string

	// Stage is the bound person's stage. Empty for a reservation — an
	// enrolment whose content record has not landed — and for a removal.
	Stage iam.Stage

	// Removed marks a seat whose holder was removed while holding it, and
	// that nobody has been bound to since that removal.
	Removed bool
}

// Directory is the identity estate as contact routing reads it.
//
// CONSUMER-DEFINED and one method wide, for the package-wide reason: the
// registry needs this one fact and nothing else the estate knows, and a seam
// this narrow is what lets a test hand it a directory with no store behind it.
// The engine satisfies it over internal/iamdomain's reader.
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
	// the seat for good, so a later holder's unbind hands it back to the
	// chart rather than to the leaver's tombstone.
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
// not.
//
// # An ALLOWLIST of the stages that route, like internal/iam's own
//
// ACTIVE routes, and so do INVITED, ENROLLING and a reservation's empty stage:
// each is somebody the company has put in the seat who has not finished
// signing up, the seat's contact map names them, and silencing a new hire
// until they complete an enrolment would read as a colleague who never answers.
// Those three mean "not yet"; SUSPENDED and RETIRED mean "no longer", and a
// removal is the permanent form of the second.
//
// A STAGE THIS BUILD CANNOT NAME WITHHOLDS. It is one a newer peer wrote during
// a rolling upgrade, and the two ways to be wrong are not symmetric: withheld,
// a person is unreachable through the engine for the minutes the upgrade takes;
// routed, the company's work can reach somebody a newer build had excluded.
func withholding(h Holder) (Withholding, bool) {
	if h.Removed {
		return WithheldRemoved, true
	}
	switch h.Stage {
	case iam.StageActive, iam.StageInvited, iam.StageEnrolling, "":
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
// # Keyed on the BINDING, resolved against an organization
//
// A binding names the handle its seat had when the person was bound, and
// nothing rewrites it when the chart renames the seat — a rename is survived
// through the seat's former handles, the way every other reference to a seat
// survives one. So a reading states what each binding says, and
// [Standing.Seats] is what turns that into seats: it resolves every binding
// through the organization a registry is being built from, LIVE HANDLES FIRST
// and former ones after, which is [org.Organization.Role]'s own order and
// therefore the seat the request path resolves the same binding to. Keyed on
// the raw handle instead, a suspended person whose seat had been renamed went
// on being attributed on every vendor surface, because the withholding named a
// handle no seat answered to any more.
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
// An unreadable directory normally carries the last reading forward, because
// what the node last knew is the best answer it has. The first registry a node
// builds has no last reading — and the two answers left are not symmetric.
// Chart-only would route every suspended, retired and removed holder's
// accounts for as long as the directory stayed unreadable, which is the
// direction a suspension exists to close; withholding every human seat makes
// the company's people briefly unreachable through the engine, which is the
// same trade a stage this build cannot name already makes. The periodic
// re-read replaces it the moment a read lands.
func Unread() Standing { return Standing{consulted: true, unread: true} }

// ReadStanding reads the directory once and applies the rule.
//
// A NIL DIRECTORY IS CHART-ONLY and no error: it is a node that does not run
// the identity domain, which is a posture rather than a fault. An error from
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

// Withheld is every BINDING that withholds its seat, by the handle it names,
// sorted. [Standing.Seats] is the same reading in terms of an organization's
// seats.
func (s Standing) Withheld() []string {
	return slices.Sorted(maps.Keys(s.withheld))
}

// Seats applies this reading to one organization: which of its seats are
// withheld and why, keyed on each seat's CURRENT handle.
//
// A binding that resolves to no seat in o withholds nothing here — there is no
// contact map for it to withdraw — and is the dangling residue the identity
// duties report. A binding naming a FORMER handle resolves to the seat that
// answers to it now, and a handle some other seat has since taken as its own
// resolves to that seat: the chart's stated residue for every reference to a
// seat, and the answer the request path gives the same person.
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
	current := seatAddresses(o)
	for bound, why := range s.withheld {
		if handle, ok := current[bound]; ok {
			out[handle] = harder(out[handle], why)
		}
	}
	return out
}

// seatAddresses maps every handle a seat in o answers to — its own, then each
// of its former ones — to its current handle.
//
// TWO PASSES, LIVE FIRST, and first wins within each: [org.Organization.Role]'s
// rule, as an index built once rather than a walk per binding. A live handle
// must never lose to some other seat's retired one, and one merged pass in org
// order would decide that by position.
func seatAddresses(o *org.Organization) map[string]string {
	out := map[string]string{}
	for role := range o.AllRoles() {
		if h := role.Handle(); h != "" {
			if _, taken := out[h]; !taken {
				out[h] = h
			}
		}
	}
	for role := range o.AllRoles() {
		h := role.Handle()
		if h == "" {
			continue
		}
		for _, former := range role.FormerHandles {
			if _, taken := out[former]; former != "" && !taken {
				out[former] = h
			}
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
