package iam

import "slices"

// ActorKind is who wrote something, in the vocabulary three durable stores
// already hold: the tracker's `author_kind` column, the knowledge base's, and
// the notice rows derived from both.
//
// RESTATED HERE ON PURPOSE, and the wire strings are byte-identical to the
// ones those tables already contain, so the day tracker and pages take their
// author kinds from this package nothing migrates. The alternative was
// importing one of them, which would end the leaf property this package exists
// to have — see the package doc — and a leaf that imports a store is a leaf
// until the first person needs it somewhere a store cannot be opened.
type ActorKind string

const (
	// ActorAgent is a seat, acting inside a turn.
	ActorAgent ActorKind = "agent"

	// ActorHuman is a person, through the dashboard or a chat surface.
	ActorHuman ActorKind = "human"

	// ActorOperator is a credential acting on the company's behalf, under
	// its own name and never a seat's. A tracker whose author field is
	// chosen by the writer is not an audit trail, which is why this is a
	// kind of its own rather than a second spelling of [ActorHuman].
	ActorOperator ActorKind = "operator"

	// ActorSystem is the engine itself — a duty's repair, a chart apply.
	ActorSystem ActorKind = "system"
)

// ActorKinds are the four.
var ActorKinds = []ActorKind{ActorAgent, ActorHuman, ActorOperator, ActorSystem}

// Valid reports whether an actor kind off the wire is one this build knows.
func (a ActorKind) Valid() bool { return slices.Contains(ActorKinds, a) }

// AnonymousActor is the name recorded for an actor this build cannot name.
//
// internal/config REFUSES IT AS A TOKEN ID, which is what makes it
// unclaimable: a write made with nobody identified can then never be confused
// in an audit row with a real credential's, and a reader filtering on the name
// gets one of the two rather than both. It is spelled out here rather than
// imported, for the leaf property's sake — config imports this package, not
// the other way round — and the two must stay the same string.
const AnonymousActor = "anonymous"

// Actor is how a principal is recorded on a row it authors.
type Actor struct {
	// Name is the author, bare. NO PREFIX: internal/api/opsmcp used to
	// record an operator as "operator:" + id while a work commit recorded
	// the id alone, and the audit feed — the one screen that reads both
	// histories — showed one person as two people three rows apart. The
	// kind is already a column, so the prefix was a second encoding of a
	// fact the row carries, and what it bought was that a reader
	// filtering on a name matched half of what somebody did.
	Name string

	// Kind is which of the four this author is.
	Kind ActorKind
}

// ActorFor is how a principal is written down.
//
// TOTAL OVER FOUR CASES, WITH NO FIFTH — every principal, including a
// malformed one and one carrying a [Kind] this build has never heard of, comes
// back as one of [ActorKinds]. There is nowhere for this to return an error
// to: it is called at the moment a row is written, and a write that fails
// because the author could not be classified is a decision somebody made that
// the engine then threw away.
//
// ONE DEGRADATION RULE, stated once: the kind a principal DECLARES decides the
// actor kind, and only the NAME degrades. A seat principal missing its handle
// is still an agent's write — pretending it was the engine's, or an operator's,
// would put somebody else's name on it — so the kind stands and the name falls
// back to [AnonymousActor]. The one exception is a kind this build cannot
// read at all, which becomes an operator: that claims neither a seat nor the
// engine, which is the least this can claim and still be total.
//
// A PERSON SPLITS ON THEIR SEAT, and that split is the whole point of the
// seat binding the identity directory holds: bound, they act as themselves and
// their work lands under their own seat handle; unbound — an operator who is
// not in the org chart, a pipeline, an automation — they act as the
// credential, under its login. Both are ordinary.
//
// WHICH IS WHY EVERY PERSON ENROLS WITH A LOGIN. The unbound arm has nothing
// else to write, and a person enrolled by address alone was recorded as
// [AnonymousActor] beside every change they made; the identity directory now
// refuses an enrolment that names none, so the degradation above is reached
// only by a principal no enrolment produced.
func ActorFor(p Principal) Actor {
	switch p.Kind {
	case KindSeat:
		return Actor{Name: nameOr(p.Seat), Kind: ActorAgent}
	case KindPerson:
		if p.Seat != "" {
			return Actor{Name: p.Seat, Kind: ActorHuman}
		}
		return Actor{Name: nameOr(p.Login), Kind: ActorOperator}
	case KindMachine:
		return Actor{Name: nameOr(p.Login), Kind: ActorOperator}
	case KindEngine:
		return Actor{Name: nameOr(p.Login), Kind: ActorSystem}
	default:
		return Actor{Name: AnonymousActor, Kind: ActorOperator}
	}
}

// nameOr is the degradation rule's one half: a name, or the name for nobody.
func nameOr(name string) string {
	if name == "" {
		return AnonymousActor
	}
	return name
}
