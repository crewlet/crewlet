// Package chart is the company's own org chart: its units, the seats that hold
// it, and the structure the engine executes.
//
// # What it is for, and why it is a LOG rather than a document
//
// The chart a founder draws IS the execution graph — knowledge, delegation,
// routing and escalation all derive from where a seat sits — and until now it
// lived as a nested object inside the Tier B company document, rewritten whole
// by whoever wrote the document last. That shape settles one question badly
// and three not at all:
//
//   - A WRITE IS THE WHOLE DOCUMENT. Adding one seat re-states every other,
//     so two people editing two different teams collide on a revision neither
//     of them touched, and the loser's change is not refused — it is
//     overwritten by a value the winner never looked at.
//   - THERE IS NO PER-OBJECT ARBITRATION. A document has one version, so the
//     unit of contention is the company rather than the object, and a
//     reconcile that wanted to touch one team had to take the whole chart.
//   - A CHANGE HAS NO RECORD. The document's revisions say what the chart
//     BECAME and never what happened: who moved, out of which unit, into
//     which, at whose hand. A reorganisation is exactly the change a company
//     most needs an audit of.
//
// So the chart becomes the state log's FOURTH DOMAIN, on exactly the terms
// [internal/tracker] and [internal/pages] are its first and third: every change
// is ONE RECORD on an ordered stream, arbitrated at the broker on the subject
// of the object it changes and applied into N identical SQL copies with the
// checkpoint in the same transaction as the rows. That shape is ADR-0002 — the
// stream is the write-ahead log, these SQL tables are derived from it — and
// [internal/statelog] is the record's authority. What is particular to an org
// chart is below.
//
// # The one thing an org chart has that neither of the others does
//
// STRUCTURE IS AN OBJECT IN ITS OWN RIGHT. A task belongs to a project and a
// page to a container, and in both cases containment is a FIELD on the thing
// contained: two writers moving two tasks into one project do not have to
// agree about anything. An org chart's containment is the thing itself — the
// tree is what every derived answer is read out of — and a move touches two
// parents and every ancestor above them. Two moves that both reparent through
// one common ancestor can each be locally valid and jointly produce a CYCLE,
// which no node can see from the subject it arbitrated on.
//
// So the domain has TWO kinds of subject rather than one:
//
//   - THE STRUCTURE arbitrates on [KindTree], one subject for the whole chart.
//     Every reparent, every placement, every removal and every import contends
//     there, and exactly one wins. That is a deliberate serialisation of the
//     rarest write this company makes — a reorganisation — bought to make a
//     cycle impossible rather than detectable.
//   - THE CONTENT arbitrates per object, on [KindUnit] and [KindSeat]. A
//     backstory, a goal, a model chain, a channel, a purpose: facts about ONE
//     object that no other object's validity depends on. Two leads editing two
//     seats never contend, which is the ordinary case and the one that has to
//     stay cheap.
//
// A reader must not assume the two are ordered against each other per object.
// They are ordered by the LOG, which is total — a content record and a
// structural record are applied in the order the broker committed them, and
// neither carries an expectation on the other's subject. What that buys is
// that a seat edited while a reorganisation is in flight does not lose either
// write; what it costs is that a content record for a seat the structure is
// about to remove still applies, and is then removed. The applier reconciles
// that in one direction only: STRUCTURE WINS, because the structure is what
// every derived answer is read out of.
//
// # What a reader must not assume
//
//   - NOT THAT A KEY IS AN IDENTITY. A unit's key and a seat's handle are
//     ADDRESSES people type, and both can be reassigned — see [KindRekey] and
//     `former_keys_json` — and a key that moved goes on resolving to the object
//     that used to hold it until something else claims it. The IDENTITY is the
//     address the object was CREATED under (`origin_key`, `origin_handle`,
//     frozen by the first rename), and it is the one address never issued
//     twice: it resolves to its object however many renames ago it was
//     retired, no rename and no creation may take it, and a removal tombstones
//     it beside the address the object held. Everything durable a seat owns is
//     keyed on the id derived from it (ADR-0019) and every binding the
//     identity directory holds names a seat by it (ADR-0020), so a second
//     object created under it would share the first one's mailbox, diary and
//     people. See [identityHolder].
//   - NOT THAT THE CHART IS THE ORGANISATION. [internal/org] builds the
//     runtime tree a turn reads — normalised, with lead inheritance and
//     manages-expansion applied. This package holds what was AUTHORED. Every
//     derivation the org model performs is a function of these rows and is not
//     stored beside them, because a derived value written down is a second
//     answer that drifts.
//   - NOT THAT A DANGLING REFERENCE IS AN ERROR. A `manages:` entry naming a
//     seat nobody has added yet is kept as written, for the reason the
//     organisation model gives: a chart is built in pieces and every
//     intermediate state is applied by every node. The edge tables store what
//     was authored, and resolution happens where the tree is read.
package chart

import (
	"fmt"
	"slices"
	"strings"
)

// The content caps, in bytes. Refused at the edge naming the field, never
// silently cut: a backstory truncated mid-sentence is a personality with a
// missing half, and a purpose cut to fit is a team whose reason for existing
// stops mid-clause.
const (
	// MaxKey bounds a unit's key and a seat's handle.
	//
	// SIXTY-FOUR. It is the tightest cap here and it has the most reasons:
	// a key is a SUBJECT TOKEN on [KindRekey], so the broker keeps it in a
	// per-member index for the life of the deployment; it is a SEGMENT of
	// every scope path an object's records are filed under; and it is what
	// a person types into a `manages:` entry. A handle is a slug derived
	// from a name, so sixty-four is already several times the longest one
	// any company has written.
	MaxKey = 64

	// MaxName bounds a unit's or a seat's display name.
	//
	// Two hundred and fifty-six, the same as a page title's and for the
	// same reason: it has to fit on a line, in a breadcrumb and in a
	// roster row.
	MaxName = 256

	// MaxProse bounds one free-text field on an object — a purpose, a
	// backstory, a goal, one responsibility, one behavioural guideline.
	//
	// SIXTEEN KIBIBYTES. A backstory is the largest of them and it is
	// rendered into every system prompt that seat's turns build, so the
	// cap is a TOKEN BUDGET before it is a storage one: 16 KiB is roughly
	// four thousand tokens, which is already a tenth of a modest context
	// window spent on one seat's biography. A founder who needs more is
	// describing a knowledge-base page rather than a seat.
	MaxProse = 16 << 10

	// MaxList bounds a repeated prose field — responsibilities, goals,
	// behavioural guidelines, policies.
	//
	// THIRTY-TWO ENTRIES, because every one of them is rendered into a
	// prompt on every turn and a list past that is a document.
	MaxList = 32

	// MaxManages bounds one seat's authored `manages:` list.
	//
	// SIXTY-FOUR, which is this design's universal fan-out batch and also
	// the [MaxScopeTerms] cap a structural record is bounded by. A seat
	// managing more than sixty-four things directly is a chart that wants a
	// unit key, which is what a `manages:` entry expands through — one
	// entry reaching a whole department costs one row here.
	MaxManages = 64

	// MaxFormerKeys bounds how many retired keys one object carries.
	//
	// SIXTEEN. A former key exists so a reference somebody already typed
	// goes on resolving, and the sixteenth rename of one unit is a chart
	// nobody can follow anyway. It is a bound on a column read WHOLE, so
	// the cost of the cap is one array on a row rather than an index.
	MaxFormerKeys = 16

	// MaxEmail bounds a seat's address.
	//
	// Three hundred and twenty: the longest address RFC 5321 permits, 64
	// for the local part and 255 for the domain with the `@` between them.
	// A cap chosen from the standard rather than from taste, because this
	// value is matched against what a vendor's webhook sends.
	MaxEmail = 320
)

// ObjectRef names one object in the chart, in the vocabulary a scope term and
// a history row both use.
//
// A PAIR RATHER THAN A STRING, because the two halves are read separately
// everywhere they are used — the kind picks the table and the id picks the row
// — and a composed string would be parsed back apart at every one of them.
type ObjectRef struct {
	Kind ObjectKind `json:"k"`
	ID   string     `json:"i"`
}

// SeatKind is what holds a seat.
//
// ITS OWN TYPE rather than [org.RoleKind], and the reason is the direction
// each is read in: org's is the runtime model's, built from a validated
// document on a node that already accepted it, where this one comes OFF THE
// WIRE from a peer that may be a newer build. An unknown value here has to be
// a value rather than a panic, which is what Valid is for.
type SeatKind string

// The seat kinds. Two, and the difference is whether anything runs: an agent
// seat has an inbox, a turn loop and an LLM chain; a human seat participates in
// the same hierarchy and is addressable only.
const (
	SeatAgent SeatKind = "agent"
	SeatHuman SeatKind = "human"
)

// SeatKinds is every kind.
func SeatKinds() []SeatKind { return []SeatKind{SeatAgent, SeatHuman} }

// Valid reports whether k is a kind this build serves.
func (k SeatKind) Valid() bool { return slices.Contains(SeatKinds(), k) }

// AuthorKind is who made a change, on the same three values the tracker and the
// knowledge base use and for the same reasons.
type AuthorKind string

// The author kinds.
const (
	AuthorAgent    AuthorKind = "agent"
	AuthorHuman    AuthorKind = "human"
	AuthorOperator AuthorKind = "operator"
)

// AuthorKinds is every kind.
func AuthorKinds() []AuthorKind { return []AuthorKind{AuthorAgent, AuthorHuman, AuthorOperator} }

// Valid reports whether k is a kind this build serves.
func (k AuthorKind) Valid() bool { return slices.Contains(AuthorKinds(), k) }

// ChangeKind is what happened to the chart, in the vocabulary a history row
// renders.
type ChangeKind string

// The change kinds.
//
// A REORGANISATION IS THE CHANGE A COMPANY MOST NEEDS AN AUDIT OF, and these
// are what make one readable: "moved" and "placed" are different events to the
// person reading them, and collapsing both into "changed" would make the one
// screen this domain exists to serve say nothing.
const (
	ChangeCreated  ChangeKind = "created"
	ChangeEdited   ChangeKind = "edited"
	ChangeMoved    ChangeKind = "moved"
	ChangeLed      ChangeKind = "led"
	ChangeManages  ChangeKind = "manages"
	ChangeRekeyed  ChangeKind = "rekeyed"
	ChangeRemoved  ChangeKind = "removed"
	ChangeImported ChangeKind = "imported"
)

// ChangeKinds is every kind.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{ChangeCreated, ChangeEdited, ChangeMoved, ChangeLed,
		ChangeManages, ChangeRekeyed, ChangeRemoved, ChangeImported}
}

// Valid reports whether k is a kind this build serves.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds(), k) }

// RootUnit is the unit a seat names when it sits at the ORG ROOT rather than
// inside a team.
//
// A REAL NAME rather than an empty one, so "the seats above every unit" is a
// term a writer can state and a probe can match, and so no path in the scope
// alphabet has a hole in the middle of it. It is also why a root seat's records
// file under one path instead of scattering: a CEO and a founder are two seats
// in one place, and a deferral on either has to block the other.
//
// It is NOT a unit: nothing creates a row for it, `chart_units` never holds it,
// and a `manages:` entry naming it resolves to nothing. It exists in the scope
// alphabet and nowhere else.
const RootUnit = "root"

// NormalizeKey is the canonical form of a unit key or a seat handle: the
// ADDRESS an object is reached by, arbitrated on and filed under.
//
// FOLDED, because the organisation model already compares unit keys folded —
// "a name is prose and a reader who cannot tell two teams apart files one
// team's work under the other" — and a second answer here would make one
// address two.
//
// AND WHITESPACE BECOMES A HYPHEN, which is the half a case fold alone does not
// cover and the half this domain cannot do without. A unit's key is minted from
// its display name when the document declares no `id:`, so `Product Team` is a
// key a company can be running on today — and here a key is a BROKER SUBJECT
// TOKEN and a SCOPE PATH SEGMENT, neither of which may contain a space. The two
// alternatives were both worse: refusing the space would refuse a company that
// is valid, and escaping it would mean a second alphabet that has to agree with
// this one for ever.
//
// What it costs is that `Product Team` and `product-team` are one address.
// That is the same collision the organisation model already refuses a document
// for, one fold wider, and the refusal names both units and where each sits.
func NormalizeKey(key string) string {
	return strings.ToLower(strings.Join(strings.Fields(key), "-"))
}

// ErrInvalid reports a value this domain refuses.
var ErrInvalid = fmt.Errorf("chart: invalid")

// invalid builds a refusal naming the field and what to do.
func invalid(field, why string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(why, args...))
}
