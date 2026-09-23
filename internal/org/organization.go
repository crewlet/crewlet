package org

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"slices"
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

// Role returns the seat with this HANDLE from anywhere in the company, or
// nil. Root seats are checked first.
//
// THE HANDLE IS WHAT A DOCUMENT REFERENCES a seat by — a unit's `lead:`,
// every `manages:` entry — and it is the identity every layer outside the
// org model already addresses the seat by. A seat's NAME is prose a founder
// edits, so resolving one by name made every reference to it break on a
// rename, and made two derivations of "which seat is this" that disagreed
// wherever an operator declared a handle.
//
// A RETIRED HANDLE RESOLVES TOO, and only after every live one has missed.
// The chart moves the STRUCTURE a rename touches — a seat's unit, a unit's
// children, who is recorded as leading what — but it deliberately does not
// rewrite the authored text of a `manages:` list, because that is a document
// somebody wrote and the next apply would put the old spelling straight back.
// So the alias is what keeps that entry pointing at the person it named. The
// order is the rule rather than an optimisation: a live handle must never
// lose to some other seat's retired one, which is exactly what one merged
// pass over both would allow.
func (o *Organization) Role(handle string) *Role {
	if handle == "" {
		return nil
	}
	for r := range o.AllRoles() {
		if r.Handle() == handle {
			return r
		}
	}
	for r := range o.AllRoles() {
		if slices.Contains(r.FormerHandles, handle) {
			return r
		}
	}
	return nil
}

// Unit returns the unit answering to this KEY from anywhere in the tree, or
// nil.
//
// THE KEY, for the reason [Organization.Role] resolves a handle: a unit's
// name is what people read and therefore what gets renamed, while its key
// ([Unit.Key], its id) is chosen once. A `manages:` entry naming a unit and a
// root seat's `unit:` both resolve here, so a team can be renamed without
// anything that points at it going dark.
func (o *Organization) Unit(key string) *Unit {
	if key == "" {
		return nil
	}
	for u := range o.AllUnits() {
		if u.Key() == key {
			return u
		}
	}
	// A RETIRED KEY, after every live one has missed — see
	// [Organization.Role], and here it is what keeps a root seat's `unit:`
	// and a `manages:` entry naming a renamed team resolving to that team
	// rather than silently placing the seat at the root.
	for u := range o.AllUnits() {
		if slices.Contains(u.FormerKeys, key) {
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
	// FROM THE ORIGIN HANDLE, NEVER THE CURRENT ONE. Everything durable a
	// seat owns is keyed on this id — its mailbox and consumer group, its
	// seat lease, its diary and episodes, its schedule ledger — so deriving
	// it from an address a founder retypes made every rename a brand-new
	// seat standing in an empty office. See [Role.Origin].
	return DeriveAgentID(o.Name, r.Origin())
}

// AgentSeatByHandle returns the AGENT seat with this handle, or nil.
//
// It never returns a human seat even though handles are unique across both
// kinds: this lookup's callers publish to an inbox, and a human seat has
// none.
func (o *Organization) AgentSeatByHandle(handle string) *Role {
	// THROUGH [Organization.Role], so a retired handle resolves here on the
	// same terms it resolves everywhere else. A second walk with its own
	// alias rule is how one lookup starts answering a question differently
	// from the lookup beside it.
	if r := o.Role(handle); r != nil && r.IsAgent() {
		return r
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
//
// [Organization.Role] IS this lookup — the handle is how the document itself
// names a seat — so this is a name for the caller's question rather than a
// second walk that could answer differently.
func (o *Organization) SeatByHandle(handle string) *Role { return o.Role(handle) }

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
//  1. A root seat naming a unit KEY moves into it. A seat added through the
//     per-entity config API arrives at the root with a unit: reference; a
//     seat that stayed there would miss the unit's MCP credentials and be
//     invisible to the unit lead, so the move has to precede both.
//  2. Lead and Slack channel CASCADE from a unit to any child that sets
//     none, to any depth.
//  3. A unit's MCP credentials are inherited by its DIRECT AGENT members,
//     whose own values win per variable. Human seats inherit none.
//  4. A unit lead AUTO-MANAGES any direct member that no direct member of
//     the same unit already manages.
//  5. A manages entry naming a UNIT KEY expands to the handles in it.
//
// Steps 4 and 5 both read manages through ONE resolver ([managesIndex]),
// built once after step 1 has settled who sits where. Auto-management runs
// before the expansion rewrites the lists, so it has to see each entry the
// way the expansion will leave it: a direct member that manages its own unit
// (or an ancestor) by key shields the members that reference reaches,
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

// attachRootSeats moves each root seat carrying a unit: reference into the
// unit answering to that KEY. A reference naming no unit leaves the seat at
// the root — see [Organization.DanglingRefs].
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
	// seats maps every address a seat answers to — its handle and each of
	// its retired ones — to the handle it answers to NOW.
	//
	// THE VALUE IS THE CURRENT HANDLE, not a presence marker, and that is
	// what makes a rename keep a roster intact. [Organization.expandManages]
	// rewrites every entry through this map, so a `manages:` list naming
	// somebody by the handle they had last quarter lands as the handle they
	// have today — which is what [Organization.Manager] reads, and it
	// compares against a live handle rather than resolving anything.
	//
	// A LIVE HANDLE ALWAYS WINS over another seat's retired one, for
	// [Organization.Role]'s reason: the live pass is written first and the
	// alias pass may not overwrite it.
	seats map[string]string
	// unitSeats is each unit KEY's seat handles, descendants included, in
	// [Unit.AllRoles] order, under the unit's current key and each retired
	// one. The FIRST unit answering to a key owns it, the same answer
	// [Organization.Unit] gives: a stored revision can still hold two units
	// on one key, and an expansion that read the last one while every lookup
	// read the first would manage one team while reporting another.
	unitSeats map[string][]string
}

func (o *Organization) managesIndex() managesIndex {
	index := managesIndex{
		seats:     make(map[string]string),
		unitSeats: make(map[string][]string),
	}
	for r := range o.AllRoles() {
		index.seats[r.Handle()] = r.Handle()
	}
	for r := range o.AllRoles() {
		for _, was := range r.FormerHandles {
			if _, live := index.seats[was]; !live {
				index.seats[was] = r.Handle()
			}
		}
	}
	claim := func(key string, u *Unit) {
		if key == "" {
			return
		}
		if _, claimed := index.unitSeats[key]; claimed {
			return
		}
		handles := make([]string, 0, len(u.Roles))
		for r := range u.AllRoles() {
			handles = append(handles, r.Handle())
		}
		index.unitSeats[key] = handles
	}
	for u := range o.AllUnits() {
		claim(u.Key(), u)
	}
	for u := range o.AllUnits() {
		for _, was := range u.FormerKeys {
			claim(was, u)
		}
	}
	return index
}

// resolve returns the HANDLES one manages entry of manager stands for.
// manager is the managing seat's own handle.
//
// A SEAT REFERENCE IS ANSWERED WITH THE SEAT'S CURRENT HANDLE, which is how
// an entry naming a renamed colleague keeps managing them: every reader of a
// normalised manages list compares against live handles.
//
// A token that is BOTH a seat handle and a unit key stays a seat reference.
// The seat is the more specific reading, and an operator who named a person
// means that person; expanding it would silently hand them a whole team.
//
// A unit key stands for every seat in that unit's subtree except manager
// itself: a seat inside the unit it manages does not manage itself.
//
// An entry matching neither stands for itself, verbatim. Live config
// management bootstraps an org in pieces, so a manages entry naming a seat
// that has not arrived yet is ordinary, and dropping it would quietly
// rewrite the chart the operator wrote. [Organization.DanglingRefs] is what
// reports it.
func (x managesIndex) resolve(manager, entry string) []string {
	if handle, isSeat := x.seats[entry]; isSeat {
		return []string{handle}
	}
	members, isUnit := x.unitSeats[entry]
	if !isUnit {
		return []string{entry}
	}
	out := make([]string, 0, len(members))
	for _, handle := range members {
		if handle != manager {
			out = append(out, handle)
		}
	}
	return out
}

// managed is the set of handles r's manages entries resolve to.
func (x managesIndex) managed(r *Role) map[string]struct{} {
	out := make(map[string]struct{}, len(r.Manages))
	for _, entry := range r.Manages {
		for _, handle := range x.resolve(r.Handle(), entry) {
			out[handle] = struct{}{}
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
// own unit (or an ancestor) by key shields the members that reference
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
	// THE LEAD'S OWN MANAGED SET IS COMPUTED ONCE PER LEAD, not once per
	// unit, and that is a correctness-preserving fix to a quadratic.
	//
	// This loop only ever APPENDS to a lead's Manages, and it records each
	// append in the set below as it goes — so the set is already exact at
	// the top of every later iteration, and recomputing it was pure waste.
	// The waste was not small: lead inheritance makes a root seat the
	// EFFECTIVE lead of every unit beneath it, and its Manages grows to
	// hold every seat in the company, so the recomputation was
	// O(units x seats). Measured on a fan-shaped 20,000-seat company it
	// was 95% of the derivation's allocation — 970 MB and 1.2 seconds,
	// against 60 ms with this map.
	//
	// KEYED ON THE ROLE POINTER rather than the handle, because that is
	// what identifies the seat whose slice is being appended to; two seats
	// cannot share a handle in a valid company, but a lookup that assumed
	// so would silently merge them in one that is not.
	byLeadCache := map[*Role]map[string]struct{}{}

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
			for handle := range managedBy[r] {
				shielded[handle] = struct{}{}
			}
		}
		byLead, cached := byLeadCache[lead]
		if !cached {
			byLead = index.managed(lead)
			byLeadCache[lead] = byLead
		}

		for _, r := range u.Roles {
			handle := r.Handle()
			if handle == u.Lead {
				continue
			}
			if _, taken := shielded[handle]; taken {
				continue
			}
			if _, already := byLead[handle]; already {
				continue
			}
			if _, managesLead := managedBy[r][u.Lead]; managesLead {
				continue
			}
			lead.Manages = append(lead.Manages, handle)
			lead.AutoManaged = append(lead.AutoManaged, handle)
			byLead[handle] = struct{}{}
		}
	}
}

// expandManages replaces each manages entry with the seat HANDLES it
// resolves to, so an operator can write one unit key instead of five people.
// See [managesIndex.resolve] for the reading, and [Organization.Normalize]
// for why auto-management shares it.
//
// Afterwards every entry of every manages list is a handle, which is what
// [Organization.Manager] and [Organization.Reports] read.
func (o *Organization) expandManages(index managesIndex) {
	for r := range o.AllRoles() {
		if len(r.Manages) == 0 {
			continue
		}
		expanded := make([]string, 0, len(r.Manages))
		seen := make(map[string]struct{}, len(r.Manages))
		for _, entry := range r.Manages {
			for _, handle := range index.resolve(r.Handle(), entry) {
				if _, dup := seen[handle]; dup {
					continue
				}
				expanded = append(expanded, handle)
				seen[handle] = struct{}{}
			}
		}
		r.Manages = expanded
	}
}

// ---- soft references ------------------------------------------------- //

// RefKind names which soft reference failed to resolve.
type RefKind string

const (
	// RefLead is a unit whose own lead is no seat's handle.
	RefLead RefKind = "lead"
	// RefUnit is a root seat whose unit: is no unit's key.
	RefUnit RefKind = "unit"
	// RefManages is a manages entry that is neither a seat's handle nor a
	// unit's key.
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
	// From is what carries the reference, by the DISPLAY name a person
	// reads: the unit (for a lead), the seat (for a unit reference or a
	// manages entry), or the document path of the map (for a GitLab access
	// level).
	From string
	// To is the handle or key, as written, that resolved to nothing.
	To string
	// Seat and Unit are the entity carrying the reference, when it is one:
	// the seat for a unit reference or a manages entry, the unit for a lead.
	// From names it for a reader, and a name does not say which entity when
	// two share it, so a caller placing the reference in a document locates
	// it by these instead, as it does a validation error (see [SeatError]).
	Seat *Role
	Unit *Unit
}

// Message renders the reference for an operator: what names what, what the
// engine does with it meanwhile, and the two ways to resolve it.
func (d DanglingRef) Message() string {
	switch d.Kind {
	case RefLead:
		return fmt.Sprintf("unit %q names lead %q, which is no seat's handle, so "+
			"the unit and every descendant inheriting its lead run with no lead. "+
			"Correct the handle or add a seat that derives it", d.From, d.To)
	case RefUnit:
		return fmt.Sprintf("seat %q names unit %q, which is no unit's key, so the "+
			"seat stays at the root. Correct the key or add a unit with that id",
			d.From, d.To)
	case RefManages:
		return fmt.Sprintf("seat %q manages %q, which is neither a seat's handle "+
			"nor a unit's key, so the entry manages nobody. Correct the entry or "+
			"add a seat or unit answering to it", d.From, d.To)
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
// had nothing to fix. A manages entry is reported when it is neither a seat's
// handle nor a unit's key. One naming a unit with no seats resolves to
// nobody, but it is not a misspelling, so it is not reported.
//
// EVERY REFERENCE IS RESOLVED THE WAY THE ENGINE RESOLVES IT: seats by
// handle, units by key, each including the addresses it used to answer to.
// Checking a different identity here would report a misspelling the engine
// does not have, or miss the one it does.
func (o *Organization) DanglingRefs() []DanglingRef {
	// A SEAT ANSWERS TO ITS RETIRED HANDLES TOO, because
	// [Organization.Role] does and this report may not disagree with it: a
	// unit whose `lead:` names somebody by the handle they had last quarter
	// HAS that lead, so calling it a misspelling would send an operator to
	// fix a reference that works.
	//
	// THE UNIT SET CARRIES NO ALIASES, and the asymmetry is not an
	// oversight. Only two references name a unit and neither can reach this
	// report holding a retired key: a `unit:` that resolves — an alias
	// included — has already moved its seat into that unit, so the seat is
	// no longer among the root seats this checks; and a `manages:` entry is
	// read after [Organization.expandManages] has rewritten it, which
	// resolves aliases into member handles. Adding them would be a second
	// rule nothing exercises.
	seats := make(map[string]struct{})
	for r := range o.AllRoles() {
		seats[r.Handle()] = struct{}{}
		for _, was := range r.FormerHandles {
			seats[was] = struct{}{}
		}
	}
	units := make(map[string]struct{})
	for u := range o.AllUnits() {
		units[u.Key()] = struct{}{}
	}

	var out []DanglingRef
	for _, r := range o.Roles {
		if _, found := units[r.UnitRef]; r.UnitRef != "" && !found {
			out = append(out, DanglingRef{Kind: RefUnit, From: r.Name, To: r.UnitRef, Seat: r})
		}
	}
	for u := range o.AllUnits() {
		if _, found := seats[u.DeclaredLead]; u.DeclaredLead != "" && !found {
			out = append(out, DanglingRef{Kind: RefLead, From: u.Name, To: u.DeclaredLead, Unit: u})
		}
	}
	for r := range o.AllRoles() {
		for _, entry := range r.Manages {
			_, isSeat := seats[entry]
			_, isUnit := units[entry]
			if !isSeat && !isUnit {
				out = append(out, DanglingRef{Kind: RefManages, From: r.Name, To: entry, Seat: r})
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
	if err := o.validateContactIdentities(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateAdmission reports every ADMISSION rule the company breaks, joined:
// duplicate seat names, two units answering to one key (a duplicate name, or
// an id that is another unit's key), and a unit reference on a seat declared
// inside a unit of a different key. See the class note above [Organization.Validate].
// It assumes [Organization.Normalize] has run, so a root seat moved into its
// unit is counted once, where it now sits.
func (o *Organization) ValidateAdmission() error {
	return errors.Join(o.validateSeatNames(), o.validateUnitKeys(),
		o.validateUnitRefs())
}

// validateUnitRefs refuses a `unit:` reference on a seat that sits inside a
// unit the reference does not key. See [ErrMisplacedUnitRef].
//
// Read after normalization, which is what makes one comparison enough: a
// root seat whose reference resolved now sits in the unit it keys, so its
// reference matches, and one whose reference resolved to nothing is still at
// the root, where [Organization.DanglingRefs] reports it. Every other member
// carrying a reference that differs from its unit's key was declared there.
// Compared as the exact string a reference resolves by.
func (o *Organization) validateUnitRefs() error {
	var errs []error
	for u := range o.AllUnits() {
		for _, r := range u.Roles {
			if r.UnitRef == "" || r.UnitRef == u.Key() {
				continue
			}
			errs = append(errs, &SeatError{Seat: r, Field: []any{"unit"}, Err: fmt.Errorf(
				"role %q: %w: it is declared in unit %q (key %q), and `unit: %s` places "+
					"only a seat declared at the root, so here it moves nothing. Remove "+
					"the reference, or declare the seat in the unit keyed %q or at the root",
				r.Name, ErrMisplacedUnitRef, u.Name, u.Key(), r.UnitRef, r.UnitRef)})
		}
	}
	return errors.Join(errs...)
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
		errs = append(errs, &DuplicateError{
			Kind: DuplicateHandle, Key: g.key, Seats: seatsOf(g.members),
			Err: fmt.Errorf(
				"%w %q: %d seats derive it (%s). The handle is the canonical seat "+
					"identity, naming its inbox, its agent id and its external "+
					"accounts, so give each of these seats a distinct name or an "+
					"explicit handle",
				ErrDuplicateHandle, g.key, len(g.members), describeSeats(g.members, true)),
		})
	}
	return errors.Join(errs...)
}

// validateSeatNames enforces org-wide seat name uniqueness.
//
// A seat's name is DISPLAY, and this rule is about what reads it. Nothing in
// the document resolves a seat by name any more — a unit's `lead:` and every
// `manages:` entry are handles — so a duplicate no longer makes a seat
// unreachable through a reference. What it still does is make the seat
// unaddressable by the one party that types prose: a model reaching a
// colleague names what it remembers, and the colleague lookup answers an
// exact role-name match with one seat or an honest list. Two seats of one
// name are permanently that list, on every ask, every mention and every
// roster row a lead reads.
//
// Compared as the EXACT string, because that is how an exact role-name match
// is made: two names differing only in case or spacing are distinct there, so
// they are distinct here. A unit key is folded instead (see validateUnitKeys
// below), and the two rules are not in disagreement: a seat's identity is its
// HANDLE, held unique by a runnable rule, so nothing is ever filed or
// referenced under a seat's name, and a pair differing only in case derives
// ONE handle unless it declares two, which is where that rule reports it. A
// unit derives nothing of the sort: its name IS its key wherever it declares
// no id. A name that is empty or blank is skipped, since [Role.Validate]
// already refuses it.
func (o *Organization) validateSeatNames() error {
	groups := groupBy(o.placedSeats(), func(s placedSeat) string {
		if strings.TrimSpace(s.role.Name) == "" {
			return ""
		}
		return s.role.Name
	})
	var errs []error
	for _, g := range groups {
		errs = append(errs, &DuplicateError{
			Kind: DuplicateSeatName, Key: g.key, Seats: seatsOf(g.members),
			Err: fmt.Errorf(
				"%w %q: %d seats carry it (%s). A colleague named in prose is "+
					"resolved by this name, so an agent asking for it is offered "+
					"both of them every time; give each of these seats its own name",
				ErrDuplicateSeatName, g.key, len(g.members), describeSeats(g.members, false)),
		})
	}
	return errors.Join(errs...)
}

// foldUnitKey is THE fold a unit key is claimed and compared under, and the
// only one: what a key means is decided here, and every question asked about
// a group afterwards (which of its members carry the name, which answers by
// an id) has to be asked in the same terms or the group and the message it
// produces disagree.
//
// Not [strings.EqualFold] at those questions, which is close enough to read
// as the same rule and is not: it folds by [unicode.SimpleFold], while this
// folds by [unicode.ToLower], and the two part over characters that are real
// in a team name. "İstanbul" and "Istanbul" are ONE key here, so the pair is
// refused, and EqualFold calls them different, so the unit written second
// used to lose the duplicate NAME sentinel and be described as answering by
// an id it never declared.
func foldUnitKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validateUnitKeys enforces chart-wide uniqueness of a unit's identity: ONE
// message per key, naming every unit that answers to it.
//
// # What a collision costs
//
// [Unit.Key] is what work, routing and pages are filed under, and
// [Organization.Unit] resolves a name to the FIRST unit carrying it. So two
// units answering to one key do not conflict loudly: one of them simply
// receives the other's work, for ever, and the chart looks correct.
//
// # Why an id may not collide with another unit's NAME either
//
// Key falls back to the name, so a company that gives one unit the id
// "platform" while another is NAMED "Platform" has exactly the collision
// above. It arrives by a door nobody is watching: adding an id that is
// already some other unit's name.
//
// # EVERY KEY IS FOLDED, names and ids alike
//
// A name is prose, and "Platform" and "platform" are one team. Two units a
// reader cannot tell apart are refused here, naming both and where each one
// sits, rather than admitted so that the first person to write the case they
// remember files one team's work, routing and pages under the other for
// ever.
//
// That a reference resolves a name EXACTLY ([Organization.Unit], [Unit.Child]
// and the manages index all match the string as written) is what makes the
// collision QUIET, not what makes it safe: both spellings resolve, each to a
// different team, and the chart looks correct either way. A reference that
// resolves is the symptom, so it is no argument for admitting the pair.
//
// Folding is also what makes the two vocabularies comparable. An id is a
// lowercase key by rule while a name is written however a person writes it,
// so an exact comparison would let `id: platform` sit beside `name: Platform`
// unreported, which is the cross-check above. This package validates no shape
// of its own and so may not lean on the config layer narrowing an id: an
// `id: Platform` is folded here like every other key.
//
// The seat name rule beside this one does NOT fold, and the two are not in
// disagreement. A seat's key is its HANDLE, which is unique by a runnable
// rule, so a seat name is never what work is filed under; two seats named
// "Dev" and "dev" that declare no handle derive one and are refused by the
// handle rule (validateHandles above) before this class is reached. A unit
// derives no second identity, so its name IS its key wherever it declares no
// id, and this rule is the only thing between that pair and a silent
// misfiling.
//
// THE FIRST SPELLING MET OWNS THE GROUP, and is what the message is reported
// in: an operator searches their document for what they wrote, not for a form
// nothing in it contains.
//
// # One rule, two sentinels
//
// A collision between two NAMES wraps [ErrDuplicateUnitName] as well as
// [ErrDuplicateUnit], because a name IS the key on a unit that declares no
// id: two rules reporting it put one mistake in front of an operator twice,
// and in front of the builder twice for each unit carrying it.
//
// An ADMISSION rule (see the class note above [Organization.Validate]), like
// the duplicate seat name beside it. Companies holding two units of one name
// were admitted before the rule and run exactly as they did, so a submitted
// document is refused for it while a stored revision is applied with a
// warning. An id collision cannot reach a stored revision that predates the
// field at all, so nothing is grandfathered by the class that had a choice.
func (o *Organization) validateUnitKeys() error {
	units := o.placedUnits()
	// byKey indexes a group by the FOLDED text every member of it answers
	// to, a name and an id alike, which is what makes "Platform",
	// "platform" and `id: platform` one group rather than three near
	// misses nobody is told about.
	byKey := make(map[string]int, len(units))
	var all []duplicateGroup[placedUnit]

	// group returns the group indexed under folded, starting one reported
	// in the spelling that key was first met in. A GROUP KEEPS ITS FIRST
	// SPELLING, so a chart carrying both cases of a name is reported in the
	// one written first rather than in whichever the walk reached last.
	group := func(folded, spelling string) int {
		i, seen := byKey[folded]
		if !seen {
			i = len(all)
			byKey[folded] = i
			all = append(all, duplicateGroup[placedUnit]{key: spelling})
		}
		return i
	}
	// join adds a unit to a group it is not already in. POINTER IDENTITY,
	// not the key's text: a unit whose id equals its own name claims the
	// same key twice and collides with nobody.
	join := func(i int, by placedUnit) {
		for _, m := range all[i].members {
			if m.unit == by.unit {
				return
			}
		}
		all[i].members = append(all[i].members, by)
	}

	// EVERY NAME IS CLAIMED BEFORE ANY ID, so a group is reported in a name
	// wherever one carries the key and an id colliding with a name is named
	// after it in the message: the id is the field somebody just added, and
	// the one they can change without renaming a team. A blank name is
	// skipped rather than claimed, since [Unit.Validate] already reports it
	// and an empty key would group every nameless unit together.
	for _, c := range units {
		name := strings.TrimSpace(c.unit.Name)
		if name == "" {
			continue
		}
		join(group(foldUnitKey(name), name), c)
	}
	for _, c := range units {
		id := strings.TrimSpace(c.unit.ID)
		if id == "" {
			continue
		}
		join(group(foldUnitKey(id), id), c)
	}

	var errs []error
	for _, g := range all {
		if len(g.members) < 2 {
			continue
		}
		carriers := make([]*Unit, len(g.members))
		named := 0
		for i, m := range g.members {
			carriers[i] = m.unit
			// FOLDED BY foldUnitKey, the way the key was claimed. A
			// unit named "platform" in a group reported as "Platform"
			// answers by its NAME, and measuring that any other way
			// would both count it as an id carrier in the message and
			// drop the duplicate name sentinel from a pair that is
			// exactly that.
			if foldUnitKey(m.unit.Name) == foldUnitKey(g.key) {
				named++
			}
		}
		// A key two units carry as their NAME is a duplicate name as well,
		// and the error carries both sentinels so a caller branching on
		// either one means this collision. A key that arrived through an id
		// carries the key sentinel alone: no name is duplicated, and saying
		// one is sends an operator to rename a team that is named once.
		key := fmt.Errorf("%w %q", ErrDuplicateUnit, g.key)
		if named > 1 {
			key = fmt.Errorf("%w %q (a %w)", ErrDuplicateUnit, g.key, ErrDuplicateUnitName)
		}
		errs = append(errs, &DuplicateError{
			Kind: DuplicateUnitName, Key: g.key, Units: carriers,
			Err: fmt.Errorf(
				"%w: %d units answer to it (%s). A unit's key is its id when it "+
					"declares one and its name otherwise, and a manages entry or a "+
					"seat's unit reference resolves to the first unit answering to "+
					"it, so one team's work, routing and pages are filed under "+
					"another. Give each of these units its own id",
				key, len(g.members), describeUnits(g.members, g.key)),
		})
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

// seatsOf is the seats of a group, in the order they were met.
func seatsOf(placed []placedSeat) []*Role {
	out := make([]*Role, len(placed))
	for i, s := range placed {
		out[i] = s.role
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

// describeUnits renders colliding units for one grouped message: each unit's
// name and where it sits, because two units answering to one key differ in
// their place when the key is a shared name and in their name when it is an
// id that is another unit's.
//
// A unit that does not answer to the key by its name answers by its id, and
// the id is named: it is the field that collided, the one an operator just
// added, and the one they can change without renaming a team.
//
// Asked through foldUnitKey, the way the key was claimed, and never through
// [strings.EqualFold]: the two agree on the cases anyone writes by hand and
// part over characters that are real in a team name, so a unit named
// "Istanbul" in a group reported as "İstanbul" would be described as
// answering by an id it never declared, reading `(id "")`.
func describeUnits(units []placedUnit, key string) string {
	parts := make([]string, len(units))
	for i, u := range units {
		parts[i] = fmt.Sprintf("unit %q %s", u.unit.Name, u.place)
		if foldUnitKey(u.unit.Name) != foldUnitKey(key) {
			parts[i] += fmt.Sprintf(" (id %q)", strings.TrimSpace(u.unit.ID))
		}
	}
	return strings.Join(parts, "; ")
}

// validateContactIdentities enforces chart-wide uniqueness of every external
// account a seat is reachable at: ONE message per identity, naming every seat
// that claims it.
//
// # What a collision costs
//
// An identity is how an inbound message finds a person, and the two consumers
// resolve a duplicate OPPOSITE WAYS. [notify.Registry.ReconcileHumanContacts]
// keys a map on (transport, id), so the last seat in chart order wins and the
// first silently stops being reachable, while every walk of the chart answers
// the first. So a duplicated `slack_user_id` sends one person's wakes to
// another while an inbound message resolves back to the first, and neither
// seat looks wrong.
//
// It is checked on the DECLARED values rather than the resolved ones for the
// reason [HumanContact.Identities] gives: validation runs where the config is
// read, which is not always where the environment that resolves a ${VAR}
// lives, and two seats sharing a literal is the collision somebody typed. Two
// seats pointing at one ${VAR} is the same mistake spelled once, so it is
// caught here too — the reference is compared as its own text.
//
// # Compared as the values SIT
//
// [HumanContact.Normalize] has already trimmed every value and folded the
// ones whose field is canonically lowercase, leaving a ${VAR} reference
// exempt. So the text stored on the contact IS the text each consumer routes
// on, and comparing it as it sits is what makes this rule agree with them:
// folding here as well would refuse `slack_user_id` values differing only in
// case, which the registry keys apart and no inbound payload confuses, and it
// would call `${FOO}` and `${foo}` one reference when the environment they
// resolve from does not.
//
// Per FIELD, not per transport: Jira and Confluence share `atlassian_account_id`
// and reporting one collision twice would read as two problems.
//
// A RUNNABLE rule (see the class note above [Organization.Validate]), unlike
// the duplicate seat and unit names: an identity is what a message is ROUTED
// by, so a company carrying a collision is not running as its author reads it
// — one of the two people is already unreachable, today, and no later
// revision makes the mail they never received arrive.
func (o *Organization) validateContactIdentities() error {
	seats := o.placedSeats()
	var errs []error
	for _, f := range contactFields {
		// One group per value of THIS field. A seat with no contact, and a
		// field it left empty, key to "" and are skipped by groupBy: an
		// identity that is missing is not a collision, the rule every
		// duplicate here follows (see validateHandles above).
		for _, g := range groupBy(seats, func(s placedSeat) string {
			if s.role.Contact == nil {
				return ""
			}
			return strings.TrimSpace(*f.value(s.role.Contact))
		}) {
			errs = append(errs, &DuplicateError{
				Kind: DuplicateIdentity, Key: g.key, Seats: seatsOf(g.members),
				Err: fmt.Errorf(
					"%w %s=%q: %d seats claim it (%s). An inbound message finds "+
						"whichever seat a reader resolved first and notification "+
						"registration takes the other, so all but one of these "+
						"seats silently stops being reachable. Give each of them "+
						"its own account",
					ErrDuplicateIdentity, f.key, g.key, len(g.members),
					describeSeats(g.members, true)),
			})
		}
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
		for i, s := range u.Schedules {
			if !s.IsEnabled() || !s.TargetsLead() {
				continue
			}
			errs = append(errs, &UnitError{Unit: u, Field: []any{"schedules", i}, Err: fmt.Errorf(
				"unit %q: schedule %q: %w: it targets the unit lead, but the effective lead %q is a human seat — define the schedule on an agent seat instead",
				u.Name, s.Name, ErrUnrunnableSchedule, lead.Name)})
		}
	}
	return errors.Join(errs...)
}
