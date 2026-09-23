package iam

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

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

	// OperatorID is the CREDENTIAL the write was made through, recorded
	// BESIDE the author rather than instead of it — the `operator_id`
	// column the tracker, the knowledge base, the chart and the identity
	// trail each carry. It is the principal's [Principal.Via] where it acts
	// through something other than itself (`pat:<id>` for a machine token
	// acting as its owner) and its login otherwise, so "what did this
	// credential do" is a question an audit answers without reasoning about
	// kinds — for a person bound to a seat too, whose author is the seat.
	OperatorID string
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
// THE NAME IS [RecordOwner]'s, degraded — one function decides which of a
// principal's names it acts under, and this one only adds the kind and the
// name for nobody.
//
// WHICH IS WHY EVERY PERSON ENROLS WITH A LOGIN. The unbound arm has nothing
// else to write, and a person enrolled by address alone was recorded as
// [AnonymousActor] beside every change they made; the identity directory now
// refuses an enrolment that names none, so the degradation above is reached
// only by a principal no enrolment produced.
//
// AND THE OPERATOR IS WHAT IT PRESENTED: [Actor.OperatorID] is the
// credential beside the name — the machine token a principal acts through
// ([Principal.Via]) or, where it is its own credential, its login. A token
// acting as its owner is the owner's AUTHORITY being exercised, so the author
// is the owner; the operator column is what keeps that write distinguishable
// from one the owner made themselves.
func ActorFor(p Principal) Actor {
	name := nameOr(RecordOwner(p))
	operator := p.Login
	if p.Via != "" {
		operator = p.Via
	}
	switch p.Kind {
	case KindSeat:
		return Actor{Name: name, Kind: ActorAgent, OperatorID: operator}
	case KindPerson:
		if p.Seat != "" {
			return Actor{Name: name, Kind: ActorHuman, OperatorID: operator}
		}
		return Actor{Name: name, Kind: ActorOperator, OperatorID: operator}
	case KindMachine:
		return Actor{Name: name, Kind: ActorOperator, OperatorID: operator}
	case KindEngine:
		return Actor{Name: name, Kind: ActorSystem, OperatorID: operator}
	default:
		return Actor{Name: AnonymousActor, Kind: ActorOperator, OperatorID: operator}
	}
}

// RecordOwner is the name a principal's OWN RECORD is kept under — their
// inbox, their pins, their priorities, their personal views — and the name
// every write they make is attributed to.
//
// # One function for the read and the write
//
// It exists because there were two. The tools wrote a caller's record under
// the name [ActorFor] gave them, and the query surface read it back under the
// principal's SEAT — which is the same value for a bound person and nothing at
// all for an unbound one. So a person the directory binds to no seat, and
// every unbound token, pinned a view, marked their inbox and saved a personal
// view under their LOGIN through their assistant, and their dashboard, asking
// for the record by seat, never showed any of it. A record that is written
// under one name and read under another is a record nobody has.
//
// # Which name
//
// A bound person's is their SEAT, because that is where the company's work
// finds them: an assignment, a mention and a lead's priority list are all
// addressed to the seat. An unbound person's and every machine's is their
// LOGIN — the colon or the dot in it keeps it out of the seat namespace, so
// it can never be read as somebody's seat. A seat's is its handle.
//
// EMPTY FOR NOBODY, and never [AnonymousActor]: a principal of no known kind,
// or one missing the name its kind is kept under, has no record, and an empty
// owner is what every personal rule refuses. A name here would be one a
// caller could be decided as — every such principal would share a record
// called "anonymous".
func RecordOwner(p Principal) string {
	switch p.Kind {
	case KindSeat:
		return p.Seat
	case KindPerson:
		if p.Seat != "" {
			return p.Seat
		}
		return p.Login
	case KindMachine, KindEngine:
		// A MACHINE'S LOGIN AND NEVER A SEAT. A bound token is re-kinded
		// as the person it acts as once its binding resolves
		// (internal/api/auth), so whatever is still a machine here acts as
		// the credential — and reading a seat off one would decide on one
		// record and write into another.
		return p.Login
	}
	return ""
}

// NamesSelf reports whether a name somebody typed or a path carried is one of
// this principal's OWN — the name their record is kept under, or the login
// they signed in with.
//
// BOTH, because a person bound to a seat is known by two names and either one
// is them: the authority table's own-record rule admits the login as readily
// as the seat. A surface that compared against [RecordOwner] alone read a
// bound person's login as SOMEBODY ELSE's name — resolving it through the
// colleague lookup, or writing a second record under it — while one that
// compared against the login alone missed the seat. What a caller naming
// themselves reaches is always the one record, under [RecordOwner].
//
// NOBODY NAMES NOTHING: an empty name, or a principal with no record, is
// never self.
func NamesSelf(p Principal, name string) bool {
	owner := RecordOwner(p)
	if name == "" || owner == "" {
		return false
	}
	return name == owner || name == p.Login
}

// NamesLogin reports whether a name has a LOGIN's shape — a person's dotted
// login or a machine's coloned handle — which no seat handle can ever have.
//
// It is the question a surface asks before it lets a name anywhere near the
// chart: the colleague resolver matches what a model types against seats'
// handles, names and addresses, and `jane.doe` resembles the seat `jane`
// ("Jane Doe") closely enough to land on it. A login is never a seat, so it is
// never resolved against the chart — it is looked up in the identity
// directory, or it names nothing.
func NamesLogin(name string) bool { return ValidLogin(name) || ValidMachineHandle(name) }

// Holders is the one read [OwnerOf] needs that this package cannot make: whose
// record somebody else's login names.
//
// DECLARED HERE, by its one caller, and implemented over the identity
// directory and the chart by internal/engine — this package holds values and
// reads nothing, which is why the read is a seam rather than a query.
type Holders interface {
	// HolderRecord answers the name the record of whoever holds login is
	// kept under: [RecordOwner] of the principal they act as — their seat,
	// as the chart knows it NOW, when the identity directory binds them to
	// one, and the login itself when it binds them to none.
	//
	// THREE-VALUED. [ErrNoHolder] is a login nobody holds;
	// [ErrHolderUnseated] is a holder bound to a seat the chart no longer
	// holds; ANY OTHER ERROR IS UNKNOWN — this node could not say — and a
	// caller answers it as 503, never as either of the first two, because
	// "nobody" read off a directory that could not be read is a guess about
	// whose record to write.
	HolderRecord(ctx context.Context, login string) (string, error)
}

// ErrNoHolder is a login nobody holds — no person, no machine, or only the
// reservation an unfinished enrolment leaves — so no record is kept under it.
var ErrNoHolder = errors.New("iam: nobody holds that login")

// ErrHolderUnseated is a login whose holder the identity directory binds to a
// seat the chart no longer holds: their record was kept under that seat, and
// the seat is gone, so there is no name this node can say it is under now.
//
// NOT [ErrNoHolder], and not the login's own record: somebody holds it, and
// their record is not under the login — answering the login would write a
// second record nobody reads, which is the defect [OwnerOf] exists to close.
var ErrHolderUnseated = errors.New("iam: that login's holder is bound to a " +
	"seat the chart no longer holds")

// errNoHolders is what [OwnerOf] answers for a login it was given no way to
// look up. UNKNOWN, like every error but the two sentinels: a surface wired
// without the directory cannot say whose record a login names, and reading the
// login literally is the answer that wrote a bound person's inbox marks where
// nothing of theirs reads them.
var errNoHolders = errors.New("iam: no identity directory is wired to say " +
	"whose record a login names")

// errNoLook is what [OwnerOf] answers for somebody else's login when it was
// given no [MayLook] to decide first. UNKNOWN for [errNoHolders]' reason, and
// never "go ahead": a surface that wired no gate would otherwise hand the
// directory's answer to whoever asked.
var errNoLook = errors.New("iam: no authority decision is wired to say whether " +
	"this caller may look a login up")

// MayLook decides whether THIS caller may learn what the identity directory
// says about somebody else's login — asked by [OwnerOf] BEFORE the directory
// is, and answering nil to let it be asked or the caller's own refusal to stop
// it.
//
// # Why the authority comes first
//
// The directory's answers differ by login: nobody holds this one, that one's
// holder is bound to a seat the chart has moved, a third is covered by a record
// this node cannot decode and so cannot be said at all. Decided after the
// lookup, those were answers every caller could tell apart — a login nobody
// holds refused on the name as typed, a held one this node could not resolve
// answered 503 naming the seat it was bound to — so a caller with no authority
// over anybody read which logins exist, and whose seat each holds, off the
// difference. So the surface decides first whatever it can decide without the
// record (the admin grant, a verb only a record's owner may take, a caller who
// leads nobody), and only a caller the record could still admit reaches the
// directory at all. internal/authz holds the rule as [authz.Object.Unresolved];
// this is the seam its answer reaches [OwnerOf] through, because this package
// holds values and decides nothing.
type MayLook func(ctx context.Context, login string) error

// LookRefused is [OwnerOf]'s answer when its [MayLook] refused the lookup: the
// gate's own error, UNCHANGED in Err, so a caller tells its own authority
// answer from anything the directory said and answers it exactly as it answers
// that decision everywhere else.
type LookRefused struct{ Err error }

func (r *LookRefused) Error() string { return r.Err.Error() }
func (r *LookRefused) Unwrap() error { return r.Err }

// OwnerOf is the record a name addresses when THIS caller names it — the name
// a surface reads a personal record under and writes one under, from a tool's
// argument, a question's parameter or a route's path alike.
//
// # One function for every name, and for the read and the write
//
// [RecordOwner] made a caller's OWN record one name for the read and the
// write, and [NamesSelf] made either of their names reach it. Somebody ELSE's
// was still read and written under whatever was typed: an administrator's
// `jane.doe` for a person the directory binds to the seat `jane` read an empty
// inbox and wrote pins and inbox marks into a record under the login that no
// screen of hers reads, and `set_priorities` sent the login through the fuzzy
// colleague resolver, which set the priorities of whichever seat it resembled.
// So:
//
//   - NO NAME, or either of the caller's own, is the caller's own record —
//     [RecordOwner], which may be empty for a principal with none.
//   - A LOGIN is looked up: its holder's record, through [Holders] —
//     [ErrNoHolder], [ErrHolderUnseated] or an unknown error otherwise. A
//     login is never a seat, and never read literally. And it is looked up
//     only once look has let it be: a caller the record could never admit is
//     answered look's own refusal, as a [LookRefused], and the directory is
//     never asked — see [MayLook].
//   - ANYTHING ELSE is returned UNCHANGED and UNSETTLED: a seat's handle, or
//     words a model typed. That is the chart's to resolve, and this package
//     holds no chart — a surface resolves it exactly, or against the
//     colleague lookup, as its verb calls for.
//
// SETTLED says which: true when the answer is a record owner this function
// established, false when it is the name handed back for the chart.
func OwnerOf(ctx context.Context, p Principal, name string, holders Holders,
	look MayLook) (owner string, settled bool, err error) {

	switch {
	case name == "", NamesSelf(p, name):
		return RecordOwner(p), true, nil
	case !NamesLogin(name):
		return name, false, nil
	case look == nil:
		return "", false, fmt.Errorf("%w: %q", errNoLook, name)
	}
	// THE AUTHORITY BEFORE THE DIRECTORY — see [MayLook] — and before the
	// directory's own absence too: a caller who may not look is refused the
	// same way whether or not this surface could have looked.
	if refused := look(ctx, name); refused != nil {
		return "", false, &LookRefused{Err: refused}
	}
	if holders == nil {
		return "", false, fmt.Errorf("%w: %q", errNoHolders, name)
	}
	owner, err = holders.HolderRecord(ctx, name)
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
}

// nameOr is the degradation rule's one half: a name, or the name for nobody.
func nameOr(name string) string {
	if name == "" {
		return AnonymousActor
	}
	return name
}
