package org

import (
	"errors"
	"fmt"
	"iter"
	"strings"
)

// UnitType labels what kind of grouping a unit is. It is informational —
// nothing in the engine behaves differently for a squad than for a
// department — so any string is accepted and these are the well-known ones.
type UnitType string

// The well-known unit types. A founder is free to invent others.
const (
	UnitTypeDivision   UnitType = "division"
	UnitTypeDepartment UnitType = "department"
	UnitTypeGroup      UnitType = "group"
	UnitTypeTeam       UnitType = "team"
	UnitTypeSquad      UnitType = "squad"
	UnitTypePod        UnitType = "pod"
	UnitTypeGuild      UnitType = "guild"
	UnitTypeChapter    UnitType = "chapter"
	UnitTypeUnit       UnitType = "unit"
)

// Unit is a grouping in the hierarchy that nests to any depth: a
// division holding departments holding teams, a flat squad, a pod. A unit
// holds seats directly, child units, or both.
//
// Used by pointer: normalisation writes inherited leads and channels into
// it, and callers compare units by identity.
type Unit struct {
	// Name is DISPLAY ONLY: what a person reads in a prompt, on a board, in
	// a channel topic. Nothing resolves a unit by it — see [Unit.ID].
	Name string   `yaml:"name" json:"name"`
	Type UnitType `yaml:"type,omitempty" json:"type,omitempty"`

	// ID is this unit's STABLE IDENTITY: a name, renamed as prose is, is not
	// one. An id is chosen once and IMMUTABLE (ADR-0013's rule for a seat's
	// handle), so what is keyed on it survives a rename; a document that
	// changes it has removed one unit and created another.
	//
	// IT IS WHAT REFERENCES IT: a `manages:` entry and a root seat's `unit:`
	// both resolve by [Unit.Key], which is this id. The config layer REQUIRES
	// one on every authored unit and mints it from the name at import
	// (config.MintUnitID); the fallback in [Unit.Key] is for a tree built in Go.
	//
	// IT DOES NOT STOP A RENAME RE-ONBOARDING THE SEATS BENEATH IT: onboarding
	// turns on the unit's NAME, which an agent reads as its team.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`

	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty"`

	// Lead is the HANDLE of the seat that leads this unit: routing its work,
	// its single point of contact, auto-managing any direct member no direct
	// member already manages (see [Organization.Normalize]).
	//
	// THE HANDLE, never the display name: it is the identity every other layer
	// addresses the seat by, so a unit's lead and the seat the engine runs
	// cannot come apart.
	//
	// It may resolve to a seat in this unit, a descendant or, after
	// inheritance, an ancestor; a unit with no lead inherits its parent's.
	// Read it through [Unit.LeadRole] or [Organization.EffectiveLead]; a
	// reference resolving to nothing reads as no lead.
	Lead string `yaml:"lead,omitempty" json:"lead,omitempty"`

	Goals []string `yaml:"goals,omitempty" json:"goals,omitempty"`

	// Channel is where this unit talks. Inherited by child units that
	// do not set their own.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`

	// DeclaredLead and DeclaredChannel are what this unit itself WROTE,
	// recorded by [Organization.Normalize] before the cascade fills Lead and
	// Channel with an ancestor's value. Empty means the unit named none.
	//
	// They mirror [Role.DeclaredHandle]: readers resolve through the effective
	// field, and the authored half answers what the cascade erased: a
	// dangling-reference report names the unit that wrote a misspelled lead
	// ONCE, not every descendant that inherited it, and a chart tells an
	// inherited lead from a unit naming the same seat itself.
	//
	// Not part of the wire form: they derive from `lead` and `channel`. A
	// caller building a Unit sets Lead and Channel.
	DeclaredLead    string `yaml:"-" json:"-"`
	DeclaredChannel string `yaml:"-" json:"-"`

	// declared reports that DeclaredLead and DeclaredChannel have been
	// recorded. It is what keeps Normalize idempotent: after the cascade an
	// inherited Lead and an authored one are the same string, so a second
	// pass that recorded again would promote every inherited lead to a
	// declared one.
	declared bool

	// Project and Space are the unit's tracker and knowledge IDENTITY:
	// inbound activity with no better recipient routes to the unit lead,
	// and this is where the team files work and writes pages. Neither is an
	// MCP credential, and neither scopes what anyone can READ — read scope
	// is org-wide.
	//
	// VENDOR-NEUTRAL, because the identity is the company's rather than a
	// product's: the same key names a Worklet project and a Jira one, and a
	// company that switches backends keeps the org chart it wrote.
	Project string `yaml:"project,omitempty" json:"project,omitempty"`
	Space   string `yaml:"space,omitempty" json:"space,omitempty"`

	KnowledgeRefs []string `yaml:"knowledge_refs,omitempty" json:"knowledge_refs,omitempty"`

	// MCPEnv is the tool credentials this unit's DIRECT AGENT members share.
	// Inherited by those members with their own values winning; see
	// [Organization.Normalize] for why it stops at one level and skips
	// human seats.
	MCPEnv MCPEnv `yaml:"mcp_env,omitempty" json:"mcp_env,omitempty"`

	// MCPLimits is what this unit's lead holds MCP servers to for every
	// agent seat beneath the unit, at any depth ([MCPLimits]); each seat is
	// held to the strictest of its own and every enclosing unit's
	// ([Role.Limits]).
	MCPLimits MCPLimits `yaml:"mcp_limits,omitempty" json:"mcp_limits,omitempty"`

	Roles    []*Role `yaml:"roles,omitempty" json:"roles,omitempty"`
	Children []*Unit `yaml:"children,omitempty" json:"children,omitempty"`

	// Schedules is this unit's recurring work. NOT inherited by child
	// units: a standup that fanned out to every descendant of a division
	// would wake the whole company.
	Schedules []Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty"`
}

// Key is this unit's identity: its id when it has one, its name otherwise.
//
// EVERYTHING KEYS ON THIS — every durable row, and every reference in the
// document itself ([Organization.Unit], a `manages:` entry, a root seat's
// `unit:`) — while everything a person reads keys on [Unit.Name]. The
// fallback is what keeps a tree built in Go addressable; an authored document
// always carries an id, which the config layer requires and mints.
func (u *Unit) Key() string {
	if u == nil {
		return ""
	}
	if id := strings.TrimSpace(u.ID); id != "" {
		return id
	}
	return strings.TrimSpace(u.Name)
}

// Role returns the direct member with this HANDLE, or nil.
func (u *Unit) Role(handle string) *Role {
	if handle == "" {
		return nil
	}
	for _, r := range u.Roles {
		if r.Handle() == handle {
			return r
		}
	}
	return nil
}

// Child returns the direct child unit answering to this KEY, or nil.
func (u *Unit) Child(key string) *Unit {
	if key == "" {
		return nil
	}
	for _, c := range u.Children {
		if c.Key() == key {
			return c
		}
	}
	return nil
}

// AllRoles iterates this unit's seats and every descendant's, direct
// members first. Stopping early stops the walk.
func (u *Unit) AllRoles() iter.Seq[*Role] {
	return func(yield func(*Role) bool) {
		for _, r := range u.Roles {
			if !yield(r) {
				return
			}
		}
		for _, c := range u.Children {
			for r := range c.AllRoles() {
				if !yield(r) {
					return
				}
			}
		}
	}
}

// AllUnits iterates this unit and every descendant, depth-first, parents
// before children.
func (u *Unit) AllUnits() iter.Seq[*Unit] {
	return func(yield func(*Unit) bool) {
		if !yield(u) {
			return
		}
		for _, c := range u.Children {
			for d := range c.AllUnits() {
				if !yield(d) {
					return
				}
			}
		}
	}
}

// FindRole returns the seat with this HANDLE anywhere in this subtree, or
// nil.
func (u *Unit) FindRole(handle string) *Role {
	if handle == "" {
		return nil
	}
	for r := range u.AllRoles() {
		if r.Handle() == handle {
			return r
		}
	}
	return nil
}

// FindUnit returns this unit or the descendant answering to this KEY, or
// nil.
func (u *Unit) FindUnit(key string) *Unit {
	if key == "" {
		return nil
	}
	for d := range u.AllUnits() {
		if d.Key() == key {
			return d
		}
	}
	return nil
}

// LeadRole resolves the lead within this subtree: direct members first,
// then descendants. It returns nil when the unit has no lead, or when the
// lead was inherited and therefore lives in an ancestor — use
// [Organization.EffectiveLead] to resolve that case too.
func (u *Unit) LeadRole() *Role {
	if u.Lead == "" {
		return nil
	}
	if r := u.Role(u.Lead); r != nil {
		return r
	}
	for _, c := range u.Children {
		if r := c.FindRole(u.Lead); r != nil {
			return r
		}
	}
	return nil
}

// IsLedBy reports whether this unit designates r as its lead, by r's
// handle — the identity [Unit.Lead] holds.
func (u *Unit) IsLedBy(r *Role) bool { return u.Lead != "" && u.Lead == r.Handle() }

// hasDirectAgent reports whether any DIRECT member is an agent seat — the
// question a fan-out schedule asks, since it never reaches descendants and
// humans run no turns.
func (u *Unit) hasDirectAgent() bool {
	for _, r := range u.Roles {
		if r.IsAgent() {
			return true
		}
	}
	return false
}

// Validate reports every rule this unit, its direct members and its
// descendants break, joined.
//
// Lead resolvability is deliberately NOT checked here. Live config
// management bootstraps an org in pieces — a unit can legitimately land
// before the seat that leads it — and every reader already treats a
// dangling lead as no lead. See [Organization.DanglingRefs] for what to
// report instead.
func (u *Unit) Validate() error {
	var errs []error
	add := func(field []any, err error) {
		errs = append(errs, &UnitError{Unit: u, Field: field, Err: err})
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		add([]any{"name"}, fmt.Errorf("unit: %w", ErrMissingName))
	}

	owner := fmt.Sprintf("unit %q", name)
	for _, f := range validateSchedules(owner, u.Schedules) {
		add(f.field, f.err)
	}
	for i, s := range u.Schedules {
		// A fan-out with nothing to fan out to can never fire. Failing at
		// load beats a schedule that silently no-ops every minute it is due.
		if s.IsEnabled() && !s.TargetsLead() && !u.hasDirectAgent() {
			add([]any{"schedules", i}, fmt.Errorf(
				"%s: schedule %q: %w: target each fans out to direct agent members only — never descendants, never human seats — and this unit has none",
				owner, s.Name, ErrUnrunnableSchedule))
		}
	}

	for _, r := range u.Roles {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, c := range u.Children {
		if err := c.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
