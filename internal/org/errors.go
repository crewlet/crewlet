package org

import "errors"

// The sentinels a config layer branches on. Everything Validate reports
// wraps one of these with the name of the seat or unit at fault, so an
// operator's error line says both what rule broke and where, and carries the
// entity itself in a [SeatError], a [UnitError] or a [DuplicateError], so a
// caller can say where in the document without matching on a name.
//
// They are deliberately few. A sentinel per message would make the set
// unreadable without making anything decidable: nothing branches on the
// difference between a bad cron field count and a bad timezone, and both
// have the same fix.
var (
	// ErrMissingName reports a seat or unit with no name.
	//
	// A nameless seat derives no handle, and therefore no agent id and no
	// inbox — it is in the chart and unreachable from everywhere else. A
	// nameless unit has nothing to render and nothing to mint a key from.
	ErrMissingName = errors.New("name must not be empty")

	// ErrUnknownKind reports a seat whose kind is neither agent nor human.
	// It is rejected rather than defaulted: the two kinds differ in whether
	// the seat is ever spawned, so guessing is the wrong move.
	ErrUnknownKind = errors.New("unknown seat kind")

	// ErrInvalidHandle reports an explicit handle outside [a-z0-9][a-z0-9-]*.
	//
	// Handles flow into inbox topic names, plus-addressed emails and
	// external-id registration, which rejects a malformed handle at engine
	// start. Failing at config time with the slugified suggestion beats
	// failing during boot.
	ErrInvalidHandle = errors.New("handle must match [a-z0-9][a-z0-9-]*")

	// ErrDuplicateHandle reports two seats resolving to one handle.
	//
	// The handle is the canonical seat identity, so a collision makes one
	// seat silently unreachable: two agents would share an inbox topic, and
	// an agent colliding with a human would take over the person's inbound
	// activity attribution.
	ErrDuplicateHandle = errors.New("duplicate handle")

	// ErrDuplicateSeatName reports two seats carrying one name.
	//
	// A seat's name is DISPLAY — the document references a seat by handle —
	// and this is about what reads prose. A model addressing a colleague
	// types the name it remembers, and the colleague lookup answers an exact
	// role-name match with one seat or an honest list; two seats of one name
	// are permanently that list, on every ask and every roster row. Two
	// seats can differ in handle and still collide here, which is why the
	// handle rule does not cover it.
	//
	// An ADMISSION rule (see [Organization.ValidateAdmission]): refused on
	// a document somebody submits, reported as a warning on a stored
	// revision that predates it.
	ErrDuplicateSeatName = errors.New("duplicate seat name")

	// ErrDuplicateUnitName reports two units carrying one name, anywhere in
	// the tree.
	//
	// A unit is referenced BY KEY, and a name IS the key on a unit that
	// declares no id: a manages entry keying it expands to the first unit
	// answering to it, a root seat's unit reference moves the seat into it,
	// and a masked credential is restored against it. Two teams called
	// "Platform" under different departments read as distinct on every
	// screen while each of those resolves one of them.
	//
	// Compared FOLDED, unlike a seat name: a name is prose, "Platform" and
	// "platform" are one team, and a reader who cannot tell two units apart
	// files one team's work under the other the first time they write the
	// case they remember. Every reference does resolve a unit key as
	// written, which is what makes that collision quiet rather than what
	// makes it safe. A seat name is compared exactly because a seat's
	// identity is its handle, which is unique by a runnable rule and is
	// what every reference resolves; a unit's name IS its key wherever it
	// declares no id.
	//
	// The rule that reports this measures a name against other units' IDS
	// under the same fold, because an id is a lowercase key by rule while a
	// name is prose. Where the id is what collided, an id is what the
	// resulting error names, and it carries [ErrDuplicateUnit] alone.
	//
	// An ADMISSION rule, like [ErrDuplicateSeatName], and never reported
	// alone: a name is also a unit's key when it declares no id, so the
	// error that carries this wraps [ErrDuplicateUnit] as well.
	ErrDuplicateUnitName = errors.New("duplicate unit name")

	// ErrDuplicateUnit reports two units answering to one key.
	//
	// A unit's key is what work, routing and pages are filed under, so a
	// collision sends one team's work to whichever unit a reader resolved
	// first, and it arrives by a door nobody watches: an id may collide
	// with another unit's NAME, and a name with another name that differs
	// from it only in case, as readily as an id with an id.
	//
	// Reported for a duplicate NAME too, in the same error as
	// [ErrDuplicateUnitName]: on a unit that declares no id the name IS the
	// key, and a caller branching on either sentinel means the same
	// collision. Where the id is what collided, this is the only sentinel
	// the error carries.
	ErrDuplicateUnit = errors.New("duplicate unit key")

	// ErrDuplicateIdentity reports two seats claiming one external account.
	//
	// A contact identity is how an inbound message finds a person, and two
	// seats declaring one id resolve DIFFERENTLY depending on who is
	// asking: notification registration keys a map on the identity, so the
	// LAST seat in chart order silently takes it, while every lookup that
	// walks the chart answers the FIRST. The person who lost the race keeps
	// a correct-looking config and stops receiving their own mail, while
	// an inbound message from them still resolves to the seat that lost.
	//
	// A RUNNABLE rule (see the class note above [Organization.Validate]),
	// like the duplicate handle it is the contact-field twin of: an
	// identity is what an inbound message is routed by, so a company
	// carrying a collision is not running as its author reads it — one of
	// the two people is already unreachable. Reported as a
	// [DuplicateError] of kind [DuplicateIdentity], naming every seat that
	// claims the account.
	ErrDuplicateIdentity = errors.New("duplicate contact identity")

	// ErrMisplacedUnitRef reports a seat declared inside a unit whose `unit:`
	// reference keys a different unit.
	//
	// The reference PLACES a seat declared at the root: normalization moves
	// such a seat into the unit it names. A seat already declared inside a
	// unit is never moved, so on it the reference reads as a placement and
	// does nothing, and the seat stays where it was written while its author
	// believes it sits elsewhere. Repeating the enclosing unit's own key
	// says nothing wrong and is accepted.
	//
	// An ADMISSION rule, like [ErrDuplicateSeatName]: nothing refused the
	// reference before, and a stored company carrying one runs as it did.
	ErrMisplacedUnitRef = errors.New("unit reference on a seat inside another unit")

	// ErrHumanSeatField reports a runtime-only field set on a human seat.
	// Human seats are addressable but never spawned, so an LLM key or a
	// budget on one is dead config at best and misleading at worst.
	ErrHumanSeatField = errors.New("agent-only field set on a human seat")

	// ErrAgentSeatField reports a human-only field set on an agent seat —
	// almost always a missing `kind: human`.
	ErrAgentSeatField = errors.New("human-only field set on an agent seat")

	// ErrEmbeddedEnvRef reports a contact value that embeds a ${VAR}
	// reference inside a longer string. Resolution would substitute it and
	// register a truncated identity that no webhook payload can ever match,
	// so the half-formed case fails loudly instead.
	ErrEmbeddedEnvRef = errors.New("value embeds a ${VAR} reference")

	// ErrInvalidSchedule reports a schedule that cannot be evaluated: an
	// empty name or task, a cron expression without five fields, an unknown
	// timezone, a non-positive timeout, an unknown target, or a duplicate
	// name within one role or unit.
	ErrInvalidSchedule = errors.New("invalid schedule")

	// ErrUnrunnableSchedule reports a schedule nothing could ever run — a
	// unit fan-out with no direct agent members, or a lead-targeted
	// schedule whose effective lead is a human seat. Both are permanent:
	// no later revision fixes them without editing one of the two entities,
	// which is why they are errors rather than a runtime no-op.
	ErrUnrunnableSchedule = errors.New("schedule has no runner")

	// ErrProviderKeysShape reports an llm field that is neither a string
	// nor a list of strings.
	ErrProviderKeysShape = errors.New("llm keys must be a string or a list of strings")
)

// SeatError is a rule one seat breaks, carrying the seat itself.
//
// # Why the seat and not its name
//
// Because a name does not say which seat. The errors a person makes while
// building a chart are exactly the ones that make names ambiguous: two seats
// called "Software Engineer", a seat whose name slugifies to nothing, a seat
// with no name at all. A caller that has to put the problem back where it was
// written (the config layer mapping it to a path in the document) can only do
// that by identity, so the error holds the pointer the organization was built
// from, and the config layer keeps a map from that pointer to where the seat
// was authored.
//
// It renders exactly as the wrapped error does: the text is what an operator
// reads, and the pointer is for the caller that has to locate it.
type SeatError struct {
	// Seat is the seat the rule is about, as it sits in the organization
	// that was validated.
	Seat *Role
	// Field is where in the seat the rule is about, as segments of the
	// authored document: field names as strings, list indexes as ints
	// ({"schedules", 1, "cron"}). Empty when the rule is about the seat as a
	// whole, or about several of its fields at once.
	Field []any
	// Err is the rule broken, wrapping one of this package's sentinels.
	Err error
}

func (e *SeatError) Error() string { return e.Err.Error() }

// Unwrap exposes the sentinel, so errors.Is keeps answering.
func (e *SeatError) Unwrap() error { return e.Err }

// UnitError is a rule one unit breaks, carrying the unit itself. See
// [SeatError] for why a pointer rather than a name: two units can share a
// name, and a unit with no name has none to share.
type UnitError struct {
	// Unit is the unit the rule is about.
	Unit *Unit
	// Field is where in the unit, as on [SeatError.Field].
	Field []any
	// Err is the rule broken, wrapping one of this package's sentinels.
	Err error
}

func (e *UnitError) Error() string { return e.Err.Error() }

// Unwrap exposes the sentinel.
func (e *UnitError) Unwrap() error { return e.Err }

// DuplicateKind names which identity several entities share.
type DuplicateKind string

const (
	// DuplicateHandle is two or more seats deriving one handle.
	DuplicateHandle DuplicateKind = "handle"
	// DuplicateSeatName is two or more seats carrying one name.
	DuplicateSeatName DuplicateKind = "seat_name"
	// DuplicateUnitName is two or more units answering to one key: a name
	// they share, or an id that is another unit's key. Named for the
	// sentinel it reports under, and for the field a caller places it at,
	// which is the name either way.
	DuplicateUnitName DuplicateKind = "unit_name"
	// DuplicateIdentity is two or more seats claiming one external account:
	// one value of one contact field. Not named for the field that carried
	// it — every one of them is the same collision, and a kind per
	// transport would make a caller enumerate the vendor table to ask "is
	// this a duplicate identity".
	DuplicateIdentity DuplicateKind = "identity"
)

// Valid reports whether k is one of the kinds this build reports.
func (k DuplicateKind) Valid() bool {
	switch k {
	case DuplicateHandle, DuplicateSeatName, DuplicateUnitName, DuplicateIdentity:
		return true
	}
	return false
}

// DuplicateError is one identity carried by more than one entity: ONE error
// per duplicated key, naming every entity that carries it, because a name
// used three times is one mistake rather than two pairwise ones.
//
// It holds EVERY entity, so a caller placing problems in a document can put
// one beside each of them. Seats is set for a handle, a seat name or a
// contact identity, Units for a unit key.
type DuplicateError struct {
	Kind DuplicateKind
	// Key is the shared handle, seat name, unit key or contact identity,
	// and for a unit it is the spelling the key was first met in: a unit
	// key is matched folded, a name and an id alike, and an operator
	// searches their document for what they wrote rather than for a form
	// nothing in it contains. An identity is the value as it sits after
	// [Organization.Normalize], which is the text every consumer routes on.
	Key   string
	Seats []*Role
	Units []*Unit
	// Err is the grouped message, wrapping ErrDuplicateHandle,
	// ErrDuplicateSeatName, ErrDuplicateIdentity, or
	// ErrDuplicateUnitName and ErrDuplicateUnit together.
	Err error
}

func (e *DuplicateError) Error() string { return e.Err.Error() }

// Unwrap exposes the sentinel.
func (e *DuplicateError) Unwrap() error { return e.Err }

// fieldError is a rule broken at one field of a seat or unit, before the
// owner the field belongs to is attached: a schedule and a contact are
// validated without knowing whose they are, and their owner wraps each one
// in a [SeatError] or a [UnitError].
type fieldError struct {
	field []any
	err   error
}

// at prefixes the error's field with the segments of the element it sits in.
func (f fieldError) at(prefix ...any) fieldError {
	return fieldError{field: append(append([]any(nil), prefix...), f.field...), err: f.err}
}

// joinFieldErrors renders a list of field errors as one joined error, for the
// exported validators that report without an owner.
func joinFieldErrors(faults []fieldError) error {
	errs := make([]error, len(faults))
	for i, f := range faults {
		errs[i] = f.err
	}
	return errors.Join(errs...)
}
