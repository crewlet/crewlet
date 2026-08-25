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
	Name string   `yaml:"name" json:"name"`
	Type UnitType `yaml:"type,omitempty" json:"type,omitempty"`

	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty"`

	// Lead names the seat that leads this unit — routing work within it,
	// acting as its single point of contact, and auto-managing any direct
	// member nobody else manages.
	//
	// The name may resolve to a seat in this unit, in a descendant, or —
	// after inheritance — in an ancestor. A unit with no lead of its own
	// inherits its parent's, cascading to any depth. Read the resolved seat
	// through [Unit.LeadRole] or [Organization.EffectiveLead]; a
	// reference that resolves to nothing reads as no lead.
	Lead string `yaml:"lead,omitempty" json:"lead,omitempty"`

	Goals []string `yaml:"goals,omitempty" json:"goals,omitempty"`

	// Channel is where this unit talks. Inherited by child units that
	// do not set their own.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`

	// JiraProject, ConfluenceSpace and PlaneProject are the unit's
	// integration IDENTITY: inbound activity with no better recipient
	// routes to the unit lead, and this is where the team files work and
	// writes pages. None of them is an MCP credential, and none of them
	// scopes what anyone can READ — read scope is org-wide.
	JiraProject     string `yaml:"jira_project,omitempty" json:"jira_project,omitempty"`
	ConfluenceSpace string `yaml:"confluence_space,omitempty" json:"confluence_space,omitempty"`
	PlaneProject    string `yaml:"plane_project,omitempty" json:"plane_project,omitempty"`

	KnowledgeRefs []string `yaml:"knowledge_refs,omitempty" json:"knowledge_refs,omitempty"`

	// MCPEnv is the tool credentials this unit's DIRECT members share.
	// Inherited by those members with their own values winning; see
	// [Organization.Normalize] for why it stops at one level.
	MCPEnv MCPEnv `yaml:"mcp_env,omitempty" json:"mcp_env,omitempty"`

	Roles    []*Role `yaml:"roles,omitempty" json:"roles,omitempty"`
	Children []*Unit `yaml:"children,omitempty" json:"children,omitempty"`

	// Schedules is this unit's recurring work. NOT inherited by child
	// units: a standup that fanned out to every descendant of a division
	// would wake the whole company.
	Schedules []Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty"`
}

// Role returns the direct member with this name, or nil.
func (u *Unit) Role(name string) *Role {
	for _, r := range u.Roles {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Child returns the direct child unit with this name, or nil.
func (u *Unit) Child(name string) *Unit {
	for _, c := range u.Children {
		if c.Name == name {
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

// FindRole returns the seat with this name anywhere in this subtree, or
// nil.
func (u *Unit) FindRole(name string) *Role {
	for r := range u.AllRoles() {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// FindUnit returns this unit or the descendant with this name, or nil.
func (u *Unit) FindUnit(name string) *Unit {
	for d := range u.AllUnits() {
		if d.Name == name {
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

// IsLedBy reports whether this unit designates r as its lead.
func (u *Unit) IsLedBy(r *Role) bool { return u.Lead != "" && u.Lead == r.Name }

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
	name := strings.TrimSpace(u.Name)
	if name == "" {
		errs = append(errs, fmt.Errorf("unit: %w", ErrMissingName))
	}

	owner := fmt.Sprintf("unit %q", name)
	if err := validateSchedules(owner, u.Schedules); err != nil {
		errs = append(errs, err)
	}
	for _, s := range u.Schedules {
		// A fan-out with nothing to fan out to can never fire. Failing at
		// load beats a schedule that silently no-ops every minute it is due.
		if s.IsEnabled() && !s.TargetsLead() && !u.hasDirectAgent() {
			errs = append(errs, fmt.Errorf(
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
