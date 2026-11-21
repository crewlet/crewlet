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
// NO CREDENTIAL IS EVER RECORDED UNDER IT, which keeps an unidentified write
// apart from a real credential's in an audit row. The reason is the name
// grammars: every login carries a separator ([ValidLogin] a dot,
// [ValidMachineHandle] a colon) and this name has neither; and [ActorFor]
// records a Tier A token under its WHOLE login, `token:<id>` ([TokenLogin]),
// so a token with id `anonymous` writes as `token:anonymous`. A SEAT handle is
// one segment and may be spelled this way, so the name is never read alone:
// the [Actor.Kind] beside it says whether a seat wrote it.
//
// One copy, here; every other package imports this leaf.
const AnonymousActor = "anonymous"

// Actor is how a principal is recorded on a row it authors.
//
// ITS WIRE KEYS ARE THE API'S SNAKE CASE (`name`, `kind`, `operator_id`), set
// here once, because every surface that answers a row's author (the artifact
// reads, the audit feed, a record's payload) encodes this struct as it is. A
// struct with no tags answered `Name`, `Kind` and `OperatorID`, the only
// PascalCase keys in the API, and every consumer had to spell them that way.
type Actor struct {
	// Name is the author, bare. NO PREFIX: internal/api/opsmcp used to
	// record an operator as "operator:" + id while a work commit recorded
	// the id alone, and the audit feed — the one screen that reads both
	// histories — showed one person as two people three rows apart. The
	// kind is already a column, so the prefix was a second encoding of a
	// fact the row carries, and what it bought was that a reader
	// filtering on a name matched half of what somebody did.
	Name string `json:"name"`

	// Kind is which of the four this author is.
	Kind ActorKind `json:"kind"`

	// OperatorID is the CREDENTIAL the write was made through, recorded
	// BESIDE the author rather than instead of it — the `operator_id`
	// column the tracker, the knowledge base, the chart and the identity
	// trail each carry. It is the principal's [Principal.Via] where it acts
	// through something other than itself (`pat:<id>` for a machine token
	// acting as its owner, `session:<lineage>` for a browser session) and
	// its login otherwise, so "what did this credential do" is a question
	// an audit answers without reasoning about kinds — for a person bound
	// to a seat too, whose author is the seat.
	OperatorID string `json:"operator_id,omitempty"`
}

// ActorFor is how a principal is written down.
//
// TOTAL OVER FOUR CASES: every principal, even of an unknown [Kind], comes back
// as one of [ActorKinds]; it runs as a row is written, with nowhere to return an
// error. The DECLARED kind decides and only the NAME degrades to
// [AnonymousActor]; an unreadable kind becomes an operator, the least it claims.
//
// A person is bound to a seat (ADR-0032) and acts as it; the unbound arm, the
// credential under its login, is a MACHINE's, so every principal enrols with a
// login. The name is [RecordOwner]'s, degraded. [Actor.OperatorID] is
// [Principal.Via], else the login: a token acting as its owner makes the owner
// the author, and this column keeps that write distinguishable.
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

// ActorApps is what a write records of its principal's apps beside [ActorFor]'s
// author: the apps it uses ([AppsOf]) where it is an OPERATOR, else nothing (a
// seat's record names the seat; the running node reads its holder).
//
// ON THE RECORD, because a turn the write wakes is worked for that credential
// (ADR-0109) on a node that may not have taken the write, which bounds the turn
// by these apps AND its own resolution of the credential, so a token narrowed
// on one node never acts on another's wider grants.
func ActorApps(p Principal) []Grant {
	if ActorFor(p).Kind != ActorOperator {
		return nil
	}
	return AppsOf(p.Grants)
}

// RecordOwner is the name a principal's OWN RECORD (inbox, pins, priorities,
// personal views) is kept under, and every write they make is attributed to.
//
// ONE FUNCTION FOR THE READ AND THE WRITE: a record written under one name and
// read under another is a record nobody has. A person's name is their SEAT
// (ADR-0032), where assignments, mentions and priorities are addressed; an
// unbound machine's is its LOGIN, whose colon keeps it out of the seat
// namespace; a seat's is its handle.
//
// EMPTY FOR NOBODY, never [AnonymousActor]: a principal of no known kind, or
// missing its name, has no record, and an empty owner is what every personal
// rule refuses; a name would make all of them share one "anonymous" record.
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
// BOTH, because a person bound to a seat is known by two names and the
// authority table's own-record rule admits either. Comparing against
// [RecordOwner] alone reads a bound person's login as SOMEBODY ELSE's (a
// colleague lookup, or a second record); against the login alone it misses the
// seat. Either name reaches the one record, under [RecordOwner].
//
// NOBODY NAMES NOTHING: an empty name, or a principal with no record, is never
// self.
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
	// one, as it does every person, and the login itself when it binds them
	// to none (a service account).
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
var ErrNoHolder = errors.New("nobody has that login")

// ErrHolderUnseated is a login whose holder the identity directory binds to a
// seat the chart no longer holds: their record was kept under that seat, and
// the seat is gone, so there is no name this node can say it is under now.
//
// NOT [ErrNoHolder], and not the login's own record: somebody holds it, and
// their record is not under the login — answering the login would write a
// second record nobody reads, which is the defect [OwnerOf] exists to close.
var ErrHolderUnseated = errors.New("that login's seat is no longer " +
	"in the org chart")

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
// says about somebody else's login: [OwnerOf] asks it BEFORE the directory, and
// nil lets the directory be asked.
//
// The authority comes first because the directory's answers differ by login
// (nobody holds it, its holder's seat moved, an undecodable record): decided
// after the lookup, a caller with no authority could read which logins exist
// and whose seat each holds off the difference. So the surface decides first
// what it can without the record, and only a caller the record could still
// admit reaches the directory. internal/authz holds the rule
// ([authz.Object.Unresolved]); this package only carries its answer.
type MayLook func(ctx context.Context, login string) error

// LookRefused is [OwnerOf]'s answer when its [MayLook] refused the lookup: the
// gate's own error, UNCHANGED in Err, so a caller tells its own authority
// answer from anything the directory said and answers it exactly as it answers
// that decision everywhere else.
type LookRefused struct{ Err error }

func (r *LookRefused) Error() string { return r.Err.Error() }
func (r *LookRefused) Unwrap() error { return r.Err }

// OwnerOf is the record a name addresses when THIS caller names it, for reads
// and writes alike. Somebody ELSE's record must not be addressed by whatever
// was typed: `jane.doe` for the seat `jane` would touch a record no screen of
// hers reads, or land via the fuzzy resolver on a lookalike seat.
//
//   - NO NAME, or either of the caller's own: [RecordOwner], maybe empty.
//   - A LOGIN is looked up through [Holders] ([ErrNoHolder], [ErrHolderUnseated]
//     or an unknown error), never read literally, and only once look has let it
//     be; else the answer is look's refusal as a [LookRefused].
//   - ANYTHING ELSE comes back UNCHANGED and UNSETTLED, the chart's to resolve.
//     SETTLED is true only when this function established the owner.
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
