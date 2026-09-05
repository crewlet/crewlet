package org

import (
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/google/uuid"
)

// Organization is the whole company: a flexible hierarchy of units, the
// seats inside them, and the org-wide facts every seat reads.
//
// Seats live at two levels. Inside a unit they are scoped to it for MCP
// credential inheritance and lead auto-management. At the root they are
// org-wide — a CEO, a cross-cutting advisor, the founder's own human seat —
// with no unit affiliation, participating in the management hierarchy like
// anyone else.
//
// # Build it, wire it, then treat it as frozen
//
// [Organization.Normalize] MUTATES the tree — it moves seats, cascades
// leads and rewrites manages lists — and everything afterwards only reads.
// A live company is served by many goroutines at once, so an org that is
// published and then edited in place is a data race with no owner. Hot
// reload builds a NEW org and swaps the pointer; it never edits the one
// turns are running against.
type Organization struct {
	Name     string   `yaml:"name" json:"name"`
	Mission  string   `yaml:"mission,omitempty" json:"mission,omitempty"`
	Vision   string   `yaml:"vision,omitempty" json:"vision,omitempty"`
	Policies []string `yaml:"policies,omitempty" json:"policies,omitempty"`

	// Roles are the seats that belong to no unit.
	Roles []*Role `yaml:"roles,omitempty" json:"roles,omitempty"`
	Units []*Unit `yaml:"units,omitempty" json:"units,omitempty"`

	// TokenBudget is the org-wide cap; 0 is unlimited. It lives on the
	// domain model for the same reason the per-seat cap does: the API
	// serves the org from here, and a cap that existed only in the config
	// layer could not be shown beside the meter that enforces it.
	TokenBudget int `yaml:"token_budget,omitempty" json:"token_budget,omitempty"`

	// KnowledgeScope is the org-wide knowledge READ scope — the only
	// thing that narrows a knowledge search. Empty means unscoped, bounded
	// by whatever the backend's own ACLs allow.
	//
	// Deliberately org-wide rather than per-seat: a unit's own space is an
	// identity (where it writes, where its page activity routes), and
	// letting an identity double as a read scope is how an agent ends up
	// unable to read the page it was told to follow.
	KnowledgeScope []string `yaml:"scope,omitempty" json:"scope,omitempty"`
}

// AllRoles iterates every seat in the company: root seats first, then each
// unit's subtree. Stopping early stops the walk, which is what the lookups
// below rely on; callers wanting a slice use slices.Collect.
func (o *Organization) AllRoles() iter.Seq[*Role] {
	return func(yield func(*Role) bool) {
		for _, r := range o.Roles {
			if !yield(r) {
				return
			}
		}
		for _, u := range o.Units {
			for r := range u.AllRoles() {
				if !yield(r) {
					return
				}
			}
		}
	}
}

// AllUnits iterates every unit, depth-first, parents before children.
func (o *Organization) AllUnits() iter.Seq[*Unit] {
	return func(yield func(*Unit) bool) {
		for _, u := range o.Units {
			for d := range u.AllUnits() {
				if !yield(d) {
					return
				}
			}
		}
	}
}

// Role returns the seat with this name from anywhere in the company, or
// nil. Root seats are checked first.
func (o *Organization) Role(name string) *Role {
	for r := range o.AllRoles() {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Unit returns the unit with this name from anywhere in the tree, or nil.
func (o *Organization) Unit(name string) *Unit {
	for u := range o.AllUnits() {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// ---- seat identity -------------------------------------------------- //
//
// An agent seat's runtime identity is DERIVED from the org, never looked up
// in a process: a UUIDv5 over (org name, handle), so every node computes
// the same id for the same seat with no database and no running instance.
// These three are that derivation and its inverse, in one place, so routing
// can answer "which seat is this event for?" without asking whether the
// seat happens to be running locally.

// AgentIDFor returns the derived id for a seat, reporting false for a human
// seat and for a seat or org too unnamed to derive from.
//
// Human seats are addressable but never spawned, so they have no agent id
// at all — not a zero one. A caller that treated uuid.Nil as an id would
// give every human in the company the same one.
func (o *Organization) AgentIDFor(r *Role) (uuid.UUID, bool) {
	if r == nil || !r.IsAgent() {
		return uuid.Nil, false
	}
	return DeriveAgentID(o.Name, r.Handle())
}

// AgentSeatByHandle returns the AGENT seat with this handle, or nil.
//
// It never returns a human seat even though handles are unique across both
// kinds: this lookup's callers publish to an inbox, and a human seat has
// none.
func (o *Organization) AgentSeatByHandle(handle string) *Role {
	if handle == "" {
		return nil
	}
	for r := range o.AllRoles() {
		if r.IsAgent() && r.Handle() == handle {
			return r
		}
	}
	return nil
}

// SeatByHandle returns the seat with this handle, of EITHER kind, or nil.
//
// The counterpart to [Organization.AgentSeatByHandle], and the distinction is
// the caller's purpose rather than a convenience. That one answers "who can I
// publish an inbox event to", so it must never return a human seat — a human
// has no inbox and the publish would be dropped. This one answers "is this
// handle somebody in the company", which is what a MENTION asks: a person
// named on a work item is notified through the contact transports their seat
// declares, exactly like a person named in a chat message, and a resolver
// that skipped them would silently drop every mention of a human colleague.
//
// Handles are unique across both kinds, so there is no ambiguity to resolve.
func (o *Organization) SeatByHandle(handle string) *Role {
	if handle == "" {
		return nil
	}
	for r := range o.AllRoles() {
		if r.Handle() == handle {
			return r
		}
	}
	return nil
}

// AgentSeatByID returns the agent seat whose derived id is id — the inverse
// of AgentIDFor.
//
// Linear in seat count with a hash per seat, which is why it is the
// FALLBACK on routing paths that also carry a name or a handle.
func (o *Organization) AgentSeatByID(id uuid.UUID) *Role {
	if id == uuid.Nil || o.Name == "" {
		return nil
	}
	for r := range o.AllRoles() {
		if got, ok := o.AgentIDFor(r); ok && got == id {
			return r
		}
	}
	return nil
}

// ---- normalisation --------------------------------------------------- //

// Normalize applies the derivations the org chart implies but nobody writes
// out. The config layer calls it once after loading and before
// [Organization.Validate]; every accessor and every hierarchy walk assumes
// it has run.
//
// In order, because the order is load-bearing:
//
//  1. A root seat naming a unit MOVES into it. A seat added through the
//     per-entity config API arrives at the root with a unit: reference; a
//     seat that stayed there would miss the unit's MCP credentials and be
//     invisible to the unit lead, so the move has to precede both.
//  2. Lead and Slack channel CASCADE from a unit to any child that sets
//     none, to any depth.
//  3. A unit's MCP credentials are inherited by its DIRECT members, whose
//     own values win per variable.
//  4. A unit lead AUTO-MANAGES any direct member nobody else manages.
//  5. A manages entry naming a UNIT expands to the seats in it.
//
// Steps 4 and 5 are in that order for a reason: auto-management reads
// manages as written, so a unit reference there still shields its members
// from being claimed by the lead as well.
//
// It is idempotent — running it twice changes nothing — because live config
// management re-applies whole revisions and a second pass must not compound
// what the first derived.
func (o *Organization) Normalize() {
	o.attachRootSeats()
	for _, u := range o.Units {
		propagateDownward(u, "", "")
	}
	o.inheritMCPEnv()
	o.autoManageByLead()
	o.expandManages()
	for r := range o.AllRoles() {
		r.Contact.Normalize()
	}
}

// attachRootSeats moves each root seat carrying a unit: reference into that
// unit's members. A reference naming no unit leaves the seat at the root —
// see [Organization.DanglingRefs].
func (o *Organization) attachRootSeats() {
	if len(o.Units) == 0 || len(o.Roles) == 0 {
		return
	}
	kept := o.Roles[:0]
	for _, r := range o.Roles {
		target := o.Unit(r.UnitRef)
		if r.UnitRef == "" || target == nil {
			kept = append(kept, r)
			continue
		}
		target.Roles = append(target.Roles, r)
	}
	o.Roles = kept
}

// propagateDownward cascades the lead and the Slack channel into children
// that declare none, and fills in the default unit type.
//
// A child inherits what its parent RESOLVED to, not what the parent
// literally declared, so a lead set on a division reaches a team three
// levels down through units that named nothing themselves.
func propagateDownward(u *Unit, parentLead, parentChannel string) {
	if u.Type == "" {
		u.Type = UnitTypeTeam
	}
	if u.Lead == "" {
		u.Lead = parentLead
	}
	if u.Channel == "" {
		u.Channel = parentChannel
	}
	for _, c := range u.Children {
		propagateDownward(c, u.Lead, u.Channel)
	}
}

// inheritMCPEnv layers each unit's tool credentials under its DIRECT
// members' own.
//
// One level, deliberately: a unit declares what its own team shares, and a
// child unit that needs the same credentials declares them too. Cascading
// them would hand a division's credentials to every seat beneath it, which
// is the opposite of the per-seat identity these exist to give.
func (o *Organization) inheritMCPEnv() {
	for u := range o.AllUnits() {
		if len(u.MCPEnv) == 0 {
			continue
		}
		for _, r := range u.Roles {
			r.MCPEnv = r.MCPEnv.WithDefaults(u.MCPEnv)
		}
	}
}

// autoManageByLead gives a unit lead a manages entry for every direct
// member nobody else in the unit manages.
//
// This is what makes a lead's roster complete without an operator listing
// every report twice. Three guards keep it from claiming what it should
// not: a member another member already manages keeps that manager, a member
// the lead already lists is not listed twice, and a member that manages the
// LEAD is never claimed — that one would build a two-role cycle out of a
// perfectly reasonable chart, where a tech lead reports to a VP who leads
// the unit the tech lead sits in.
func (o *Organization) autoManageByLead() {
	for u := range o.AllUnits() {
		if u.Lead == "" || len(u.Roles) == 0 {
			continue
		}
		lead := o.EffectiveLead(u)
		if lead == nil {
			continue
		}

		managed := make(map[string]struct{})
		for _, r := range u.Roles {
			for _, name := range r.Manages {
				managed[name] = struct{}{}
			}
		}
		byLead := make(map[string]struct{}, len(lead.Manages))
		for _, name := range lead.Manages {
			byLead[name] = struct{}{}
		}

		for _, r := range u.Roles {
			if r.Name == u.Lead {
				continue
			}
			if _, taken := managed[r.Name]; taken {
				continue
			}
			if _, already := byLead[r.Name]; already {
				continue
			}
			if slices.Contains(r.Manages, u.Lead) {
				continue
			}
			lead.Manages = append(lead.Manages, r.Name)
			byLead[r.Name] = struct{}{}
		}
	}
}

// expandManages replaces a manages entry naming a UNIT with the seats in
// that unit, descendants included, so an operator can write one team name
// instead of five people.
//
// A name that is BOTH a seat and a unit stays a seat reference. The seat is
// the more specific reading, and an operator who named a person means that
// person; expanding it would silently hand them a whole team.
//
// An entry matching neither is kept verbatim. Live config management
// bootstraps an org in pieces, so a manages entry naming a seat that has
// not arrived yet is ordinary — dropping it would quietly rewrite the chart
// the operator wrote.
func (o *Organization) expandManages() {
	seats := make(map[string]struct{})
	for r := range o.AllRoles() {
		seats[r.Name] = struct{}{}
	}
	unitSeats := make(map[string][]string)
	for u := range o.AllUnits() {
		names := make([]string, 0, len(u.Roles))
		for r := range u.AllRoles() {
			names = append(names, r.Name)
		}
		unitSeats[u.Name] = names
	}

	for r := range o.AllRoles() {
		if len(r.Manages) == 0 {
			continue
		}
		expanded := make([]string, 0, len(r.Manages))
		seen := make(map[string]struct{}, len(r.Manages))
		for _, entry := range r.Manages {
			if _, isSeat := seats[entry]; isSeat {
				expanded = append(expanded, entry)
				seen[entry] = struct{}{}
				continue
			}
			members, isUnit := unitSeats[entry]
			if !isUnit {
				expanded = append(expanded, entry)
				seen[entry] = struct{}{}
				continue
			}
			for _, name := range members {
				// A seat inside the unit it manages does not manage
				// itself.
				if name == r.Name {
					continue
				}
				if _, dup := seen[name]; dup {
					continue
				}
				expanded = append(expanded, name)
				seen[name] = struct{}{}
			}
		}
		r.Manages = expanded
	}
}

// ---- soft references ------------------------------------------------- //

// RefKind names which soft reference failed to resolve.
type RefKind string

const (
	// RefLead is a unit whose lead names no seat in the org.
	RefLead RefKind = "lead"
	// RefUnit is a root seat whose unit: names no unit in the org.
	RefUnit RefKind = "unit"
)

// DanglingRef is a name that resolved to nothing.
type DanglingRef struct {
	Kind RefKind
	// From is the unit (for a lead) or the seat (for a unit reference)
	// that carries the reference.
	From string
	// To is the name that resolved to nothing.
	To string
}

// DanglingRefs reports the soft references [Organization.Normalize] could
// not resolve.
//
// These are NOT validation errors, deliberately. Live config management
// bootstraps an org in pieces — a unit is allowed to land before the seat
// that leads it, and the engine applies every intermediate revision —
// so rejecting a partially-wired org would make per-entity bootstrap
// impossible. Every reader already treats a dangling reference as absent.
//
// They are worth a WARNING though: once the org is fully wired this list is
// empty, and an entry that persists across revisions is a misspelling
// nothing else will ever report. The config layer logs them.
func (o *Organization) DanglingRefs() []DanglingRef {
	var out []DanglingRef
	for _, r := range o.Roles {
		if r.UnitRef != "" && o.Unit(r.UnitRef) == nil {
			out = append(out, DanglingRef{Kind: RefUnit, From: r.Name, To: r.UnitRef})
		}
	}
	for u := range o.AllUnits() {
		if u.Lead != "" && o.Role(u.Lead) == nil {
			out = append(out, DanglingRef{Kind: RefLead, From: u.Name, To: u.Lead})
		}
	}
	return out
}

// ---- validation ------------------------------------------------------ //

// Validate reports every rule the company breaks, joined. It assumes
// [Organization.Normalize] has run: the checks that need a fully wired
// hierarchy — an inherited lead, a moved seat — cannot see it otherwise.
func (o *Organization) Validate() error {
	var errs []error
	for _, r := range o.Roles {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, u := range o.Units {
		if err := u.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := o.validateHandles(); err != nil {
		errs = append(errs, err)
	}
	if err := o.validateLeadSchedules(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// validateHandles enforces org-wide handle uniqueness.
//
// The handle is the canonical seat identity — inbox topic, party
// resolution, external-id registration — so a collision makes one seat
// silently unreachable. Two agents would share an inbox; an agent colliding
// with a human would absorb that person's inbound activity. Fatal either
// way.
func (o *Organization) validateHandles() error {
	var errs []error
	owner := make(map[string]string)
	for r := range o.AllRoles() {
		h := r.Handle()
		if first, dup := owner[h]; dup {
			errs = append(errs, fmt.Errorf(
				"%w %q: roles %q and %q — the handle is the canonical seat identity and must be unique",
				ErrDuplicateHandle, h, first, r.Name))
			continue
		}
		owner[h] = r.Name
	}
	return errors.Join(errs...)
}

// validateLeadSchedules rejects an enabled lead-targeted schedule whose
// effective lead is a human seat: humans run no turns, so it could never
// fire, and no later revision fixes it without editing one of the two
// entities. A disabled one is fine — it is config an operator is holding.
func (o *Organization) validateLeadSchedules() error {
	var errs []error
	for u := range o.AllUnits() {
		lead := o.EffectiveLead(u)
		if lead == nil || !lead.IsHuman() {
			continue
		}
		for _, s := range u.Schedules {
			if !s.IsEnabled() || !s.TargetsLead() {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"unit %q: schedule %q: %w: it targets the unit lead, but the effective lead %q is a human seat — define the schedule on an agent seat instead",
				u.Name, s.Name, ErrUnrunnableSchedule, lead.Name))
		}
	}
	return errors.Join(errs...)
}
