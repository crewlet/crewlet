package org

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"strings"

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

	// ConfluenceSpaces is the org-wide knowledge READ scope — the only
	// thing that narrows a knowledge search. Empty means unscoped, bounded
	// by whatever the backend's own ACLs allow.
	//
	// Deliberately org-wide rather than per-seat: a unit's Confluence space
	// is an identity (where it writes, where its webhooks route), and
	// letting an identity double as a read scope is how an agent ends up
	// unable to read the page it was told to follow.
	ConfluenceSpaces []string `yaml:"confluence_spaces,omitempty" json:"confluence_spaces,omitempty"`
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
//  3. A unit's MCP credentials are inherited by its DIRECT AGENT members,
//     whose own values win per variable. Human seats inherit none.
//  4. A unit lead AUTO-MANAGES any direct member that no direct member of
//     the same unit already manages.
//  5. A manages entry naming a UNIT expands to the seats in it.
//
// Steps 4 and 5 both read manages through ONE resolver ([managesIndex]),
// built once after step 1 has settled who sits where. Auto-management runs
// before the expansion rewrites the lists, so it has to see each entry the
// way the expansion will leave it: a direct member that manages its own unit
// (or an ancestor) by name shields the members that reference reaches,
// exactly as if it had listed them one by one. Reading the entry as written
// instead let the lead claim those members too, giving each a second
// manager, and claim the member itself when the reference reached the lead,
// which is a two-seat management cycle.
//
// It is idempotent (running it twice changes nothing) because live config
// management re-applies whole revisions and a second pass must not compound
// what the first derived.
func (o *Organization) Normalize() {
	o.attachRootSeats()
	for _, u := range o.Units {
		propagateDownward(u, "", "")
	}
	o.inheritMCPEnv()
	index := o.managesIndex()
	o.autoManageByLead(index)
	o.expandManages(index)
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
//
// What each unit wrote is recorded first, in [Unit.DeclaredLead] and
// [Unit.DeclaredChannel], and the effective values are computed from that
// record on every pass, which is what makes a second pass land on the same
// tree as the first.
func propagateDownward(u *Unit, parentLead, parentChannel string) {
	if u.Type == "" {
		u.Type = UnitTypeTeam
	}
	if !u.declared {
		u.DeclaredLead, u.DeclaredChannel = u.Lead, u.Channel
		u.declared = true
	}
	u.Lead = cmp.Or(u.DeclaredLead, parentLead)
	u.Channel = cmp.Or(u.DeclaredChannel, parentChannel)
	for _, c := range u.Children {
		propagateDownward(c, u.Lead, u.Channel)
	}
}

// inheritMCPEnv layers each unit's tool credentials under its DIRECT AGENT
// members' own.
//
// One level, deliberately: a unit declares what its own team shares, and a
// child unit that needs the same credentials declares them too. Cascading
// them would hand a division's credentials to every seat beneath it, which
// is the opposite of the per-seat identity these exist to give.
//
// Agents only, because a human seat runs no tools and is REFUSED an mcp_env
// of its own. Layering the unit's block under a human member put a field on
// that seat its author never wrote, and validation then rejected the whole
// company with an error pointing at it: a human lead in a unit that shares a
// tracker token could not be declared at all.
func (o *Organization) inheritMCPEnv() {
	for u := range o.AllUnits() {
		if len(u.MCPEnv) == 0 {
			continue
		}
		for _, r := range u.Roles {
			if !r.IsAgent() {
				continue
			}
			r.MCPEnv = r.MCPEnv.WithDefaults(u.MCPEnv)
		}
	}
}

// managesIndex is how a manages entry resolves to seats: the one reading
// both [Organization.autoManageByLead] and [Organization.expandManages] use.
//
// Built once, after [Organization.attachRootSeats] and before either step,
// because neither step moves a seat between units, so the membership it
// captures is the membership both of them see.
type managesIndex struct {
	// seats is every seat name in the company.
	seats map[string]struct{}
	// unitSeats is each unit's seat names, descendants included, in
	// [Unit.AllRoles] order. The FIRST unit carrying a name owns it, the
	// same answer [Organization.Unit] gives: a stored revision can still
	// hold two units of one name, and an expansion that read the last one
	// while every lookup read the first would manage one team while
	// reporting another.
	unitSeats map[string][]string
}

func (o *Organization) managesIndex() managesIndex {
	index := managesIndex{
		seats:     make(map[string]struct{}),
		unitSeats: make(map[string][]string),
	}
	for r := range o.AllRoles() {
		index.seats[r.Name] = struct{}{}
	}
	for u := range o.AllUnits() {
		if _, claimed := index.unitSeats[u.Name]; claimed {
			continue
		}
		names := make([]string, 0, len(u.Roles))
		for r := range u.AllRoles() {
			names = append(names, r.Name)
		}
		index.unitSeats[u.Name] = names
	}
	return index
}

// resolve returns the names one manages entry of manager stands for.
//
// A name that is BOTH a seat and a unit stays a seat reference. The seat is
// the more specific reading, and an operator who named a person means that
// person; expanding it would silently hand them a whole team.
//
// A unit name stands for every seat in that unit's subtree except manager
// itself: a seat inside the unit it manages does not manage itself.
//
// An entry matching neither stands for itself, verbatim. Live config
// management bootstraps an org in pieces, so a manages entry naming a seat
// that has not arrived yet is ordinary, and dropping it would quietly
// rewrite the chart the operator wrote. [Organization.DanglingRefs] is what
// reports it.
func (x managesIndex) resolve(manager, entry string) []string {
	if _, isSeat := x.seats[entry]; isSeat {
		return []string{entry}
	}
	members, isUnit := x.unitSeats[entry]
	if !isUnit {
		return []string{entry}
	}
	out := make([]string, 0, len(members))
	for _, name := range members {
		if name != manager {
			out = append(out, name)
		}
	}
	return out
}

// managed is the set of names r's manages entries resolve to.
func (x managesIndex) managed(r *Role) map[string]struct{} {
	out := make(map[string]struct{}, len(r.Manages))
	for _, entry := range r.Manages {
		for _, name := range x.resolve(r.Name, entry) {
			out[name] = struct{}{}
		}
	}
	return out
}

// autoManageByLead gives a unit lead a manages entry for every direct
// member that no direct member of the same unit already manages.
//
// This is what makes a lead's roster complete without an operator listing
// every report twice. Three guards keep it from claiming what it should
// not: a member another member already manages keeps that manager, a member
// the lead already lists is not listed twice, and a member that manages the
// LEAD is never claimed. That last one would build a two-seat cycle out of
// a perfectly reasonable chart, where a tech lead reports to a VP who leads
// the unit the tech lead sits in.
//
// Every guard reads manages RESOLVED ([managesIndex.resolve]), so an entry
// naming a unit counts for each seat it reaches. A member that manages its
// own unit (or an ancestor) by name shields the members that reference
// reaches, and is itself never claimed when the reference reaches the lead.
//
// THE SHIELD IS THE UNIT'S OWN DIRECT MEMBERS' manages, never the whole
// company's, and the narrow scope is the point. Management is stored on the
// manager, so a root CEO managing a division by name lists every seat in
// it; an org-wide shield would therefore strip every team lead beneath that
// division of the roster auto-management exists to fill. The consequence is
// deliberate and visible: a seat reached both by an outside manager's unit
// reference and by its own lead's auto-management has two managers, and
// [Organization.Manager] answers with the first in [Organization.AllRoles]
// order, which puts a root seat first.
func (o *Organization) autoManageByLead(index managesIndex) {
	for u := range o.AllUnits() {
		if u.Lead == "" || len(u.Roles) == 0 {
			continue
		}
		lead := o.EffectiveLead(u)
		if lead == nil {
			continue
		}

		shielded := make(map[string]struct{})
		managedBy := make(map[*Role]map[string]struct{}, len(u.Roles))
		for _, r := range u.Roles {
			managedBy[r] = index.managed(r)
			for name := range managedBy[r] {
				shielded[name] = struct{}{}
			}
		}
		byLead := index.managed(lead)

		for _, r := range u.Roles {
			if r.Name == u.Lead {
				continue
			}
			if _, taken := shielded[r.Name]; taken {
				continue
			}
			if _, already := byLead[r.Name]; already {
				continue
			}
			if _, managesLead := managedBy[r][u.Lead]; managesLead {
				continue
			}
			lead.Manages = append(lead.Manages, r.Name)
			byLead[r.Name] = struct{}{}
		}
	}
}

// expandManages replaces each manages entry with the seats it resolves to,
// so an operator can write one team name instead of five people. See
// [managesIndex.resolve] for the reading, and [Organization.Normalize] for
// why auto-management shares it.
func (o *Organization) expandManages(index managesIndex) {
	for r := range o.AllRoles() {
		if len(r.Manages) == 0 {
			continue
		}
		expanded := make([]string, 0, len(r.Manages))
		seen := make(map[string]struct{}, len(r.Manages))
		for _, entry := range r.Manages {
			for _, name := range index.resolve(r.Name, entry) {
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
	// RefLead is a unit whose own lead names no seat in the org.
	RefLead RefKind = "lead"
	// RefUnit is a root seat whose unit: names no unit in the org.
	RefUnit RefKind = "unit"
	// RefManages is a manages entry naming neither a seat nor a unit.
	RefManages RefKind = "manages"
	// RefGitLabAccessLevel is a key of
	// integrations.gitlab.provisioning.access_levels naming no seat's
	// handle. The organization carries no integrations, so
	// [Organization.DanglingRefs] never reports it: the config layer does
	// (config.Company.DanglingRefs). The kind is declared here so the whole
	// vocabulary of dangling references, and its rendering in
	// [DanglingRef.Message], lives in one place.
	RefGitLabAccessLevel RefKind = "gitlab_access_level"
)

// DanglingRef is a name that resolved to nothing.
type DanglingRef struct {
	Kind RefKind
	// From is what carries the reference: the unit (for a lead), the seat
	// (for a unit reference or a manages entry), or the document path of
	// the map (for a GitLab access level).
	From string
	// To is the name that resolved to nothing.
	To string
}

// Message renders the reference for an operator: what names what, what the
// engine does with it meanwhile, and the two ways to resolve it.
func (d DanglingRef) Message() string {
	switch d.Kind {
	case RefLead:
		return fmt.Sprintf("unit %q names lead %q, which is no seat, so the unit "+
			"and every descendant inheriting its lead run with no lead. "+
			"Correct the lead or add a seat with that name", d.From, d.To)
	case RefUnit:
		return fmt.Sprintf("seat %q names unit %q, which does not exist, so the "+
			"seat stays at the root. Correct its unit or add a unit with that name",
			d.From, d.To)
	case RefManages:
		return fmt.Sprintf("seat %q manages %q, which is neither a seat nor a "+
			"unit, so the entry manages nobody. Correct the entry or add a seat "+
			"or unit with that name", d.From, d.To)
	case RefGitLabAccessLevel:
		return fmt.Sprintf("%s names handle %q, which no seat has, so a seat "+
			"added later with that handle would be given this access level. "+
			"Remove the entry or correct the handle", d.From, d.To)
	default:
		return fmt.Sprintf("%s reference %q on %q resolves to nothing", d.Kind, d.To, d.From)
	}
}

// DanglingRefs reports the soft references [Organization.Normalize] could
// not resolve, in the order an author reads the document: root seats' unit
// references, then each unit's own lead, then every seat's manages entries.
// It assumes Normalize has run, like every other accessor.
//
// These are NOT validation errors, deliberately. Live config management
// bootstraps an org in pieces (a unit is allowed to land before the seat
// that leads it, and the engine applies every intermediate revision), so
// rejecting a partially-wired org would make per-entity bootstrap
// impossible. Every reader already treats a dangling reference as absent.
//
// They are worth a WARNING though: once the org is fully wired this list is
// empty, and an entry that persists across revisions is a misspelling
// nothing else will ever report. Each node logs them, through
// config.Company.DanglingRefs, as org_dangling_reference once per epoch it
// applies.
//
// WHAT WAS WRITTEN, ONCE. A lead is reported on the unit that declares it
// ([Unit.DeclaredLead]), never on the descendants that inherited it: they
// wrote nothing, and naming them sent an operator to fix units whose authors
// had nothing to fix. A manages entry is reported when it names neither a
// seat nor a unit. One naming a unit with no seats resolves to nobody, but it
// is not a misspelling, so it is not reported.
func (o *Organization) DanglingRefs() []DanglingRef {
	seats := make(map[string]struct{})
	for r := range o.AllRoles() {
		seats[r.Name] = struct{}{}
	}
	units := make(map[string]struct{})
	for u := range o.AllUnits() {
		units[u.Name] = struct{}{}
	}

	var out []DanglingRef
	for _, r := range o.Roles {
		if _, found := units[r.UnitRef]; r.UnitRef != "" && !found {
			out = append(out, DanglingRef{Kind: RefUnit, From: r.Name, To: r.UnitRef})
		}
	}
	for u := range o.AllUnits() {
		if _, found := seats[u.DeclaredLead]; u.DeclaredLead != "" && !found {
			out = append(out, DanglingRef{Kind: RefLead, From: u.Name, To: u.DeclaredLead})
		}
	}
	for r := range o.AllRoles() {
		for _, entry := range r.Manages {
			_, isSeat := seats[entry]
			_, isUnit := units[entry]
			if !isSeat && !isUnit {
				out = append(out, DanglingRef{Kind: RefManages, From: r.Name, To: entry})
			}
		}
	}
	return out
}

// ---- validation ------------------------------------------------------ //

// Validate reports every RUNNABLE rule the company breaks, joined. It assumes
// [Organization.Normalize] has run: the checks that need a fully wired
// hierarchy, an inherited lead or a moved seat, cannot see it otherwise.
//
// # Two classes of rule
//
// RUNNABLE rules are what a running company depends on: a seat with no
// handle owns no inbox, two seats on one handle share one, a lead-targeted
// schedule on a human lead never fires. This method holds them, and nothing
// may run a company that breaks one.
//
// ADMISSION rules were added after companies already existed, and a company
// that breaks one still runs exactly as it did before the rule: two units
// called "Platform" resolve references to the first of them today and did
// yesterday. [Organization.ValidateAdmission] holds them. A document somebody
// submits is refused for breaking one, while a STORED revision that breaks one
// is applied with a warning, because refusing it would take a running company
// down on upgrade (or on the older half of a rolling one) over a rule its
// author never saw. The config layer decides which class a caller runs.
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

// ValidateAdmission reports every ADMISSION rule the company breaks, joined:
// duplicate seat names and duplicate unit names. See the class note above
// [Organization.Validate]. It assumes [Organization.Normalize] has run, so a
// root seat moved into its unit is counted once, where it now sits.
func (o *Organization) ValidateAdmission() error {
	return errors.Join(o.validateSeatNames(), o.validateUnitNames())
}

// validateHandles enforces org-wide handle uniqueness.
//
// The handle is the canonical seat identity (inbox topic, party resolution,
// external-id registration), so a collision makes one seat silently
// unreachable. Two agents would share an inbox; an agent colliding with a
// human would absorb that person's inbound activity. Fatal either way.
//
// An EMPTY handle is skipped: a seat that derives none is already refused
// by [Role.Validate], and counting every such seat as a collision with the
// others reported one mistake two or three times over.
func (o *Organization) validateHandles() error {
	groups := groupBy(o.placedSeats(), func(s placedSeat) string { return s.role.Handle() })
	var errs []error
	for _, g := range groups {
		errs = append(errs, fmt.Errorf(
			"%w %q: %d seats derive it (%s). The handle is the canonical seat "+
				"identity, naming its inbox, its agent id and its external "+
				"accounts, so give each of these seats a distinct name or an "+
				"explicit handle",
			ErrDuplicateHandle, g.key, len(g.members), describeSeats(g.members, true)))
	}
	return errors.Join(errs...)
}

// validateSeatNames enforces org-wide seat name uniqueness.
//
// Compared as the EXACT string, because that is how [Organization.Role]
// resolves a lead or a manages entry: two names differing only in case or
// spacing are distinct references there, so they are distinct here. A name
// that is empty or blank is skipped, since [Role.Validate] already refuses
// it.
func (o *Organization) validateSeatNames() error {
	groups := groupBy(o.placedSeats(), func(s placedSeat) string {
		if strings.TrimSpace(s.role.Name) == "" {
			return ""
		}
		return s.role.Name
	})
	var errs []error
	for _, g := range groups {
		errs = append(errs, fmt.Errorf(
			"%w %q: %d seats carry it (%s). A unit's lead and every manages "+
				"entry name exactly one seat, and resolve to the first seat of "+
				"that name, so give each of these seats its own name",
			ErrDuplicateSeatName, g.key, len(g.members), describeSeats(g.members, false)))
	}
	return errors.Join(errs...)
}

// validateUnitNames enforces unit name uniqueness across the WHOLE tree, not
// among siblings: every reference to a unit (a manages entry, a root seat's
// unit reference) searches the entire tree and takes the first match.
//
// Compared as the exact string, which is the unit's identity key in the
// config layer and what [Organization.Unit] matches on. An empty or blank
// name is skipped, since [Unit.Validate] already refuses it.
func (o *Organization) validateUnitNames() error {
	groups := groupBy(o.placedUnits(), func(u placedUnit) string {
		if strings.TrimSpace(u.unit.Name) == "" {
			return ""
		}
		return u.unit.Name
	})
	var errs []error
	for _, g := range groups {
		places := make([]string, len(g.members))
		for i, m := range g.members {
			places[i] = m.place
		}
		errs = append(errs, fmt.Errorf(
			"%w %q: %d units carry it (%s). A manages entry and a seat's unit "+
				"reference name exactly one unit, and resolve to the first unit "+
				"of that name, so give each of these units its own name",
			ErrDuplicateUnitName, g.key, len(g.members), strings.Join(places, "; ")))
	}
	return errors.Join(errs...)
}

// placedSeat is a seat and where it sits, in words an operator can find in
// their document.
type placedSeat struct {
	role  *Role
	place string
}

// placedUnit is a unit and where it sits.
type placedUnit struct {
	unit  *Unit
	place string
}

// placedSeats lists every seat in [Organization.AllRoles] order with its
// place: "at the root", or the unit it is a direct member of.
func (o *Organization) placedSeats() []placedSeat {
	var out []placedSeat
	for _, r := range o.Roles {
		out = append(out, placedSeat{role: r, place: "at the root"})
	}
	for u := range o.AllUnits() {
		for _, r := range u.Roles {
			out = append(out, placedSeat{role: r, place: fmt.Sprintf("in unit %q", u.Name)})
		}
	}
	return out
}

// placedUnits lists every unit depth-first, parents before children, with
// its place: "at the top level", or the unit it is a child of.
func (o *Organization) placedUnits() []placedUnit {
	var out []placedUnit
	var walk func(u *Unit, place string)
	walk = func(u *Unit, place string) {
		out = append(out, placedUnit{unit: u, place: place})
		for _, c := range u.Children {
			walk(c, fmt.Sprintf("under unit %q", u.Name))
		}
	}
	for _, u := range o.Units {
		walk(u, "at the top level")
	}
	return out
}

// duplicateGroup is every member sharing one key, in the order they were
// met.
type duplicateGroup[T any] struct {
	key     string
	members []T
}

// groupBy returns the keys carried by more than one member, each with all of
// its members, in the order each key was first met. ONE GROUP PER KEY, so a
// name used three times is one message naming all three rather than two
// pairwise ones. An empty key is never a group: it is how a caller skips a
// member whose missing identity another rule already reports.
func groupBy[T any](members []T, key func(T) string) []duplicateGroup[T] {
	index := make(map[string]int)
	var all []duplicateGroup[T]
	for _, m := range members {
		k := key(m)
		if k == "" {
			continue
		}
		i, seen := index[k]
		if !seen {
			i = len(all)
			index[k] = i
			all = append(all, duplicateGroup[T]{key: k})
		}
		all[i].members = append(all[i].members, m)
	}
	var out []duplicateGroup[T]
	for _, g := range all {
		if len(g.members) > 1 {
			out = append(out, g)
		}
	}
	return out
}

// describeSeats renders colliding seats for one grouped message. byName says
// what tells them apart: their names when they share a handle, their handles
// when they share a name.
func describeSeats(seats []placedSeat, byName bool) string {
	parts := make([]string, len(seats))
	for i, s := range seats {
		if byName {
			parts[i] = fmt.Sprintf("seat %q %s", s.role.Name, s.place)
		} else {
			parts[i] = fmt.Sprintf("handle %q %s", s.role.Handle(), s.place)
		}
	}
	return strings.Join(parts, "; ")
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
