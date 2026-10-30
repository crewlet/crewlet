package seataccess

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// AsksStepUp is the one rule every write that sets an agent's access is held
// to (D10): whether moving one seat's lane from before to after needs a fresh
// step-up from a person, as a raise to an administration grant does. The
// Agents tab's save, its `keep` action and every authored write of the
// company document ask it.
//
// FAIL CLOSED. Where the vendor's offer for the lane could not be read
// ([Options.Unread]), which of its levels administers is unknown, so a turn-on
// or a change of level is asked a step-up as a raise would be; turning off and
// changing nothing never are. Where the vendor's offer was read, it is
// [NeedsStepUp], the vendor's own facts.
//
// A LANE MANAGED OUTSIDE CREWLET IS NEVER A RAISE HERE: the engine grants
// nothing on it ([ManagedExternal]), so its setting hands out no access.
func AsksStepUp(opts Options, before, after Desired) bool {
	if opts.Managed == ManagedExternal {
		return false
	}
	if NeedsStepUp(opts, before, after) {
		return true
	}
	if !opts.Unread || !after.Enabled {
		return false
	}
	before, after = before.Normalized(), after.Normalized()
	return !before.Enabled || before.Level != after.Level
}

// Raise is one agent seat's lane a write raises to an administration grant,
// or could not be told not to ([AsksStepUp]).
type Raise struct {
	Kind   integration.Kind `json:"kind"`
	Handle string           `json:"handle"`
	Lane   Lane             `json:"lane"`
}

// Offers is what each of one integration's lanes offers, for [RaisesBetween]:
// read once per integration, and only for an integration whose seats the write
// changes.
type Offers func(kind integration.Kind) map[Lane]Options

// RaisesBetween is every agent seat's lane that moving the company document
// from prior to next raises ([AsksStepUp]): a seat new to next is compared with
// a seat that held nothing, and a lane whose setting does not move is never
// one. prior is nil for a company's first document.
//
// THE OFFER IS READ LAZILY, only for an integration whose seats the write
// changes, since reading one asks the vendor.
func RaisesBetween(prior, next *config.Company, offers Offers) []Raise {
	held := map[string]*config.Role{}
	for _, seat := range SeatsOf(prior) {
		held[seat.Handle] = seat.Role
	}
	var out []Raise
	for _, kind := range Kinds() {
		var offer map[Lane]Options
		for _, seat := range SeatsOf(next) {
			for _, lane := range LanesOf(kind) {
				after, _ := DesiredOf(seat.Role, lane)
				before, _ := DesiredOf(held[seat.Handle], lane)
				before, after = before.Normalized(), after.Normalized()
				if before.Enabled == after.Enabled && before.SameAccess(after) {
					continue
				}
				if offer == nil {
					offer = offers(kind)
				}
				if AsksStepUp(offer[lane], before, after) {
					out = append(out, Raise{Kind: kind, Handle: seat.Handle, Lane: lane})
				}
			}
		}
	}
	return out
}

// UnreadOffers is every lane of kind as an offer nobody could read
// ([Options.Unread] wherever the lane grants by level), with the static facts
// its vendor's catalog holds where this build registers one: what
// [RaisesBetween] judges by when no vendor is asked, failing closed.
// external is a company whose credentials for kind are handed in from outside
// Crewlet ([config.Company.SeatCredentialMode]).
func UnreadOffers(kind integration.Kind, external bool) map[Lane]Options {
	vendor, registered := Lookup(kind)
	out := map[Lane]Options{}
	for _, lane := range LanesOf(kind) {
		o := Options{Lane: lane, Unread: true}
		if registered {
			if spec, ok := vendor.Catalog().Lane(lane); ok {
				o.Permissions, o.Unread = spec.Permissions, spec.Levels
			}
		}
		if external {
			o.Managed, o.Unread = ManagedExternal, false
		}
		out[lane] = o
	}
	return out
}
