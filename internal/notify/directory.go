package notify

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
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
	// Seat is the handle the binding names.
	Seat string

	// Stage is the bound person's stage. Empty for a reservation — an
	// enrolment whose content record has not landed — and for a removal.
	Stage iam.Stage

	// Removed marks a seat whose holder was removed while holding it, and
	// that nobody has been bound to since.
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
	// that nobody has been bound to since. Binding its next holder is what
	// routes it again.
	WithheldRemoved Withholding = "removed"

	// WithheldUnrecognised is a seat whose holder is at a stage this build
	// cannot name — one a newer peer wrote.
	WithheldUnrecognised Withholding = "unrecognised_stage"
)

// withholdings is every [Withholding], in the order a seat with two holders
// reports them: the one that is hardest to undo first, so the reason an
// operator reads is the one that will still be true after they fix the other.
var withholdings = []Withholding{
	WithheldRemoved, WithheldRetired, WithheldSuspended, WithheldUnrecognised,
}

// Valid reports whether w is one of the four.
func (w Withholding) Valid() bool { return slices.Contains(withholdings, w) }

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

// Standing is one reading of the identity directory: which human seats'
// contact identities are withheld, and why. See the package notes above.
//
// ITS ZERO VALUE IS CHART-ONLY: nothing consulted, nothing withheld.
type Standing struct {
	consulted bool
	withheld  map[string]Withholding
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
		if held, ok := s.withheld[h.Seat]; ok &&
			slices.Index(withholdings, held) <= slices.Index(withholdings, why) {
			continue
		}
		s.withheld[h.Seat] = why
	}
	return s
}

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

// Withholds reports whether a seat's contact identities are withheld, and why.
func (s Standing) Withholds(handle string) (Withholding, bool) {
	why, ok := s.withheld[handle]
	return why, ok
}

// Withheld is every withheld seat, sorted.
func (s Standing) Withheld() []string {
	return slices.Sorted(maps.Keys(s.withheld))
}

// Equal reports whether two readings withhold exactly the same seats for the
// same reasons — which is what lets a directory trigger that found nothing
// moved skip a rebuild rather than swap in an identical registry.
func (s Standing) Equal(o Standing) bool {
	return s.consulted == o.consulted && maps.Equal(s.withheld, o.withheld)
}
