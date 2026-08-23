package org

import "errors"

// The sentinels a config layer branches on. Everything Validate reports
// wraps one of these with the name of the seat or unit at fault, so an
// operator's error line says both what rule broke and where.
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
