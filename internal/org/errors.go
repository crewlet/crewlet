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
	// nameless unit can be neither a lead scope nor a manages target.
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
	// A seat is referenced BY NAME: a unit's lead and every manages entry
	// resolve to the first seat of that name, so a second one is silently
	// unreachable through either. Two seats can differ in handle and still
	// collide here, which is why the handle rule does not cover it.
	//
	// An ADMISSION rule (see [Organization.ValidateAdmission]): refused on
	// a document somebody submits, reported as a warning on a stored
	// revision that predates it.
	ErrDuplicateSeatName = errors.New("duplicate seat name")

	// ErrDuplicateUnitName reports two units carrying one name, anywhere in
	// the tree.
	//
	// A unit is referenced BY NAME: a manages entry naming it expands to the
	// first unit of that name, a root seat's unit reference moves the seat
	// into it, and a masked credential is restored against it. Two teams
	// called "Platform" under different departments read as distinct on
	// every screen while each of those resolves one of them.
	//
	// An ADMISSION rule, like [ErrDuplicateSeatName].
	ErrDuplicateUnitName = errors.New("duplicate unit name")

	// ErrHumanSeatField reports a runtime-only field set on a human seat.
	// Human seats are addressable but never spawned, so an LLM key or a
	// budget on one is dead config at best and misleading at worst.
	ErrHumanSeatField = errors.New("agent-only field set on a human seat")

	// ErrAgentSeatField reports a human-only field set on an agent seat —
	// almost always a missing `kind: human`.
	ErrAgentSeatField = errors.New("human-only field set on an agent seat")

	// ErrNoContact reports a human seat with no external identity. Such a
	// seat is inert: visible in the chart and impossible for any agent to
	// mention or reach.
	ErrNoContact = errors.New("human seat needs at least one contact identity")

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
	// DuplicateUnitName is two or more units carrying one name.
	DuplicateUnitName DuplicateKind = "unit_name"
)

// Valid reports whether k is one of the kinds this build reports.
func (k DuplicateKind) Valid() bool {
	switch k {
	case DuplicateHandle, DuplicateSeatName, DuplicateUnitName:
		return true
	}
	return false
}

// DuplicateError is one identity carried by more than one entity: ONE error
// per duplicated key, naming every entity that carries it, because a name
// used three times is one mistake rather than two pairwise ones.
//
// It holds EVERY entity, so a caller placing problems in a document can put
// one beside each of them. Seats is set for a handle or a seat name, Units
// for a unit name.
type DuplicateError struct {
	Kind DuplicateKind
	// Key is the shared handle or name.
	Key   string
	Seats []*Role
	Units []*Unit
	// Err is the grouped message, wrapping ErrDuplicateHandle,
	// ErrDuplicateSeatName or ErrDuplicateUnitName.
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
