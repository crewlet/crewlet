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

	// ID is this unit's STABLE IDENTITY, and the reason it exists is that
	// a name is not one.
	//
	// A unit's name is what people read — in a prompt, on a board, in a
	// channel topic — so it is renamed for the reasons prose is renamed,
	// and everything keyed on it moves when it does. An id is chosen once
	// and never read by anybody, so what is keyed on it survives the
	// rename.
	//
	// OPTIONAL, and absent means exactly today's behaviour: [Unit.Key]
	// falls back to the name. Adding one to a unit that already has work
	// filed against it does not rewrite those rows and does not have to —
	// a filter on a unit matches the SET of its id and its name, which is
	// [Organization.UnitByRef]'s promise and is where every durable unit
	// reference in the engine is resolved.
	//
	// IT DOES NOT STOP A RENAME RE-ONBOARDING THE SEATS BENEATH IT. That
	// is a different mechanism: onboarding turns on the unit's NAME, which
	// is what an agent reads as its team, so changing the name changes the
	// context those seats were introduced with, id or no id.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`

	Purpose string `yaml:"purpose,omitempty" json:"purpose,omitempty"`

	// Lead names the seat that leads this unit: routing work within it,
	// acting as its single point of contact, and auto-managing any direct
	// member that no direct member of this unit already manages (see
	// [Organization.Normalize] for how a unit reference counts).
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

	// DeclaredLead and DeclaredChannel are what this unit itself WROTE,
	// recorded by [Organization.Normalize] before the cascade fills Lead and
	// Channel with an ancestor's value. Empty means the unit named none.
	//
	// They mirror [Role.DeclaredHandle]: every reader resolves through the
	// effective field, and the authored half exists for the questions the
	// effective value can no longer answer once the cascade has run. Two
	// such questions exist. A dangling-reference report has to name the unit
	// that wrote a misspelled lead ONCE, rather than every descendant that
	// inherited it and whose author wrote nothing; and a chart has to tell
	// an inherited lead from a unit naming the same seat itself, which read
	// identically in Lead.
	//
	// Not part of the wire form: the authored values are `lead` and
	// `channel` in the document, and these are derived from them. A caller
	// building a Unit sets Lead and Channel.
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
	// product's: the same key names a native project and a Jira one, and a
	// company that switches backends keeps the org chart it wrote. The
	// fields were `jira_project` and `confluence_space`, which was the
	// `slack_channel` mistake — a chat channel is a channel whoever hosts
	// it — made twice.
	Project string `yaml:"project,omitempty" json:"project,omitempty"`
	Space   string `yaml:"space,omitempty" json:"space,omitempty"`

	KnowledgeRefs []string `yaml:"knowledge_refs,omitempty" json:"knowledge_refs,omitempty"`

	// MCPEnv is the tool credentials this unit's DIRECT AGENT members share.
	// Inherited by those members with their own values winning; see
	// [Organization.Normalize] for why it stops at one level and skips
	// human seats.
	MCPEnv MCPEnv `yaml:"mcp_env,omitempty" json:"mcp_env,omitempty"`

	Roles    []*Role `yaml:"roles,omitempty" json:"roles,omitempty"`
	Children []*Unit `yaml:"children,omitempty" json:"children,omitempty"`

	// Schedules is this unit's recurring work. NOT inherited by child
	// units: a standup that fanned out to every descendant of a division
	// would wake the whole company.
	Schedules []Schedule `yaml:"schedules,omitempty" json:"schedules,omitempty"`
}

// Key is this unit's identity: its id when it has one, its name otherwise.
//
// EVERYTHING DURABLE KEYS ON THIS and everything a person reads keys on
// [Unit.Name]. The two are the same string on a company that set no ids,
// which is what makes the field optional rather than a migration.
//
// Which is also why nothing durable may compare against it directly. A row
// written before an id was added holds the name and a row written after it
// holds the id, so the two spellings coexist for the life of the deployment
// and a reference is resolved back through [Organization.UnitByRef] — which
// matches this OR the name — rather than equated with this.
func (u *Unit) Key() string {
	if u == nil {
		return ""
	}
	if id := strings.TrimSpace(u.ID); id != "" {
		return id
	}
	return strings.TrimSpace(u.Name)
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
