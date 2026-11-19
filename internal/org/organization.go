package org

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Organization is the whole company: a flexible hierarchy of units, the
// seats inside them, and the org-wide facts every seat reads.
//
// Seats inside a unit are scoped to it for MCP credential inheritance and lead
// auto-management; root seats are org-wide, with no unit.
//
// # Build it, wire it, then treat it as frozen
//
// [Organization.Normalize] MUTATES the tree and everything afterwards only
// reads: many goroutines serve a live company, so editing a published org is a
// data race with no owner. Hot reload builds a NEW org and swaps the pointer.
type Organization struct {
	Name     string   `yaml:"name" json:"name"`
	Mission  string   `yaml:"mission,omitempty" json:"mission,omitempty"`
	Vision   string   `yaml:"vision,omitempty" json:"vision,omitempty"`
	Policies []string `yaml:"policies,omitempty" json:"policies,omitempty"`

	// Roles are the seats that belong to no unit.
	Roles []*Role `yaml:"roles,omitempty" json:"roles,omitempty"`
	Units []*Unit `yaml:"units,omitempty" json:"units,omitempty"`

	// TokenBudget is the org-wide ceiling per calendar window; a window it
	// does not name is uncapped. It lives on the domain model for the same
	// reason the per-seat ceilings do: the API serves the org from here,
	// and a cap that existed only in the config layer could not be shown
	// beside the meter that enforces it.
	TokenBudget TokenCeilings `yaml:"token_budget,omitempty" json:"token_budget,omitempty"`

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
// THE HANDLE IS WHAT A DOCUMENT REFERENCES a seat by (a unit's `lead:`, every
// `manages:` entry) and what every layer outside the org model addresses it by.
// A NAME is prose a founder edits: resolving by it broke references on rename
// and disagreed with declared handles. A handle is IMMUTABLE (ADR-0013), so one
// no seat answers to names nobody: there are no aliases.
func (o *Organization) Role(handle string) *Role {
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

// Unit returns the unit answering to this KEY from anywhere in the tree, or
// nil.
//
// THE KEY, as [Organization.Role] takes a handle: a name gets renamed, the key
// ([Unit.Key]) is chosen once. EXACT, because a reference inside the chart is
// one the chart wrote; one read back from a row or tool argument goes through
// [Organization.UnitByRef].
//
// AN ID BEFORE A NAME STANDING IN FOR ONE. A unit with no id is keyed on its
// name, so one unit's name can equal another's id. Admission refuses that pair
// but a stored revision may carry one; the id is durable and wins over walk
// order.
func (o *Organization) Unit(key string) *Unit {
	if key == "" {
		return nil
	}
	return o.unitByKey(func(k string) bool { return k == key })
}

// NormalizeKey is the canonical form of a unit key: the fold every unit
// reference is resolved and every pair of keys is compared under.
//
// FOLDED, because keys arrive from places nobody spells carefully (a model, a
// query string, a pasted URL). WHITESPACE BECOMES A HYPHEN, which a case fold
// does not cover: a key is minted from the display name when no `id:` is
// declared ([config.MintIdentities]), so `Product Team` and `product-team` are
// one key, which admission refuses two units for
// ([Organization.validateUnitKeys]).
func NormalizeKey(key string) string {
	return strings.ToLower(strings.Join(strings.Fields(key), "-"))
}

// UnitByRef resolves a DURABLE unit reference (read back from a row, a filter
// or a tool argument) to the unit it names.
//
// THE KEY FIRST, exactly ([Organization.Unit]): stored rows hold an immutable
// key (ADR-0013). Then under [NormalizeKey], the fold admission claims keys
// under, never [strings.EqualFold]. A NAME ONLY WHEN IT IS ONE UNIT'S, and
// every key is tried before any name.
//
// A REFERENCE NAMING NOTHING IS nil, never an error or a guess: a stored unit
// may name a team the chart no longer has, so the caller decides (a write
// refuses it, a read matches the literal rather than widening).
func (o *Organization) UnitByRef(ref string) *Unit {
	if u := o.Unit(ref); u != nil {
		return u
	}
	folded := NormalizeKey(ref)
	if folded == "" {
		return nil
	}
	if u := o.unitByKey(func(k string) bool { return NormalizeKey(k) == folded }); u != nil {
		return u
	}
	var named *Unit
	for u := range o.AllUnits() {
		if NormalizeKey(u.Name) != folded {
			continue
		}
		if named != nil {
			return nil
		}
		named = u
	}
	return named
}

// unitByKey is the first unit whose live key matches, trying every unit that
// has an id before any unit keyed on its name — see [Organization.Unit].
func (o *Organization) unitByKey(match func(key string) bool) *Unit {
	for u := range o.AllUnits() {
		if strings.TrimSpace(u.ID) != "" && match(u.Key()) {
			return u
		}
	}
	for u := range o.AllUnits() {
		if strings.TrimSpace(u.ID) == "" && match(u.Key()) {
			return u
		}
	}
	return nil
}

// ---- seat identity -------------------------------------------------- //
//
// An agent seat's runtime identity is DERIVED from the org, never looked up
// in a process: a UUIDv5 over (org name, handle), so every node computes the
// same id for the same seat with no database and no running instance.
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
	if r := o.Role(handle); r != nil && r.IsAgent() {
		return r
	}
	return nil
}

// SeatByHandle returns the seat with this handle, of EITHER kind, or nil.
//
// The counterpart to [Organization.AgentSeatByHandle], which answers "who can
// I publish an inbox event to" and so never returns a human seat (no inbox).
// This answers "is this handle somebody in the company", which a MENTION asks:
// a person named on a work item is notified through their seat's contact
// transports, and skipping humans would drop every mention of a colleague.
//
// [Organization.Role] IS this lookup; this only names the caller's question,
// not a second walk that could answer differently.
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

// AgentHandleByID names the agent seat whose derived id is id by its handle,
// for a reader naming a seat a peer reported by id (ADR-0013).
//
// THE ID ITSELF WHERE THIS ORG HOLDS NO SUCH SEAT (retired since the peer
// reported it, or a peer on another revision): the honest name, rather than
// none. "" for uuid.Nil (a shared server's instance). A nil org names every
// seat by its id.
func (o *Organization) AgentHandleByID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	if o != nil {
		if role := o.AgentSeatByID(id); role != nil {
			return role.Handle()
		}
	}
	return id.String()
}

// ---- normalisation --------------------------------------------------- //

// Normalize applies the derivations the chart implies but nobody writes. The
// config layer calls it once before [Organization.Validate]; every accessor
// assumes it ran. Idempotent: live config re-applies whole revisions.
//
// The order is load-bearing: (1) a root seat naming a unit KEY moves into it,
// before it misses the unit's MCP credentials and lead; (2) lead and Slack
// channel CASCADE to children; (3) a unit's MCP credentials go to its DIRECT
// AGENT members; (4) the lead AUTO-MANAGES unmanaged direct members; (5) unit
// KEY manages entries expand; (6, 7) seats record [Role.UnitKeys], [Role.Limits].
// Steps 4 and 5 read manages through ONE resolver ([managesIndex]) so step 4
// sees entries as step 5 leaves them: as written, the lead double-manages
// members and can manage itself, a two-seat cycle.
func (o *Organization) Normalize() {
	o.attachRootSeats()
	for _, u := range o.Units {
		propagateDownward(u, "", "")
	}
	o.inheritMCPEnv()
	index := o.managesIndex()
	o.autoManageByLead(index)
	o.expandManages(index)
	o.placeSeats()
	o.resolveMCPLimits()
	for r := range o.AllRoles() {
		r.Contact.Normalize()
	}
}

// placeSeats records, on every seat, the keys of the units it sits in: its own
// unit's first, then each enclosing unit's ([Role.UnitKeys]).
//
// Recomputed whole on every pass, so a second pass lands on the same answer.
// A unit with no key adds nothing, since nothing can name it.
func (o *Organization) placeSeats() {
	for _, r := range o.Roles {
		r.UnitKeys = nil
	}
	var place func(u *Unit, outer []string)
	place = func(u *Unit, outer []string) {
		chain := outer
		if key := u.Key(); key != "" {
			chain = append([]string{key}, outer...)
		}
		for _, r := range u.Roles {
			r.UnitKeys = slices.Clone(chain)
		}
		for _, c := range u.Children {
			place(c, chain)
		}
	}
	for _, u := range o.Units {
		place(u, nil)
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
// One level, deliberately: a unit declares what its own team shares; cascading
// would hand a division's credentials to every seat beneath it, against the
// per-seat identity these give.
//
// Agents only: a human seat runs no tools and is REFUSED an mcp_env, so
// layering one under it put a field its author never wrote and validation
// rejected the company (a human lead in a unit sharing a tracker token could
// not be declared).
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
	// seats is every seat handle in the company.
	seats map[string]struct{}
	// unitSeats is each unit KEY's seat handles, descendants included, in
	// [Unit.AllRoles] order. The FIRST unit answering to a key owns it, the
	// same answer [Organization.Unit] gives: an applied revision can still
	// hold two units on one key, since an apply does not enforce the
	// admission rules, and an expansion that read the last one while every
	// lookup read the first would manage one team while reporting another.
	unitSeats map[string][]string
}

func (o *Organization) managesIndex() managesIndex {
	index := managesIndex{
		seats:     make(map[string]struct{}),
		unitSeats: make(map[string][]string),
	}
	for r := range o.AllRoles() {
		index.seats[r.Handle()] = struct{}{}
	}
	for u := range o.AllUnits() {
		key := u.Key()
		if key == "" {
			continue
		}
		if _, claimed := index.unitSeats[key]; claimed {
			continue
		}
		handles := make([]string, 0, len(u.Roles))
		for r := range u.AllRoles() {
			handles = append(handles, r.Handle())
		}
		index.unitSeats[key] = handles
	}
	return index
}

// resolve returns the HANDLES one manages entry of manager stands for.
// manager is the managing seat's own handle.
//
// A token that is BOTH a seat handle and a unit key stays a seat reference: an
// operator who named a person means that person, and expanding it would hand
// them a whole team.
//
// A unit key stands for every seat in its subtree except manager itself.
//
// An entry matching neither stands for itself, verbatim: live config bootstraps
// an org in pieces, so naming a seat not yet arrived is ordinary, and dropping
// it would rewrite the operator's chart. [Organization.DanglingRefs] reports it.
func (x managesIndex) resolve(manager, entry string) []string {
	if _, isSeat := x.seats[entry]; isSeat {
		return []string{entry}
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
// Guards: a member another member manages keeps that manager, a listed member
// is not listed twice, and a member that manages the LEAD is never claimed (a
// two-seat cycle). All read manages RESOLVED ([managesIndex.resolve]).
//
// THE SHIELD IS THE UNIT'S OWN DIRECT MEMBERS' manages, never the company's: a
// root CEO managing a division by name lists every seat in it, so an org-wide
// shield would strip every team lead of its roster. A seat reached both ways
// has two managers; [Organization.Manager] answers the first in
// [Organization.AllRoles] order, a root seat first.
func (o *Organization) autoManageByLead(index managesIndex) {
	// EVERY UNIT'S LEAD THROUGH ONE INDEX: seats are attached and leads
	// propagated, and this pass writes only the leads' manages, so the chart
	// is as the Index needs it ([Index] says what [Organization.EffectiveLead]
	// per unit would cost). Built at the first unit naming a lead.
	leads := sync.OnceValue(o.Index)

	// THE LEAD'S MANAGED SET IS COMPUTED ONCE PER LEAD, not per unit: this loop
	// only APPENDS to a lead's Manages and records each append here, so the set
	// is exact at every later iteration. A root seat is the effective lead of
	// every unit beneath it, so recomputing was O(units x seats)
	// (TestALeadsManagedSetIsBuiltOncePerLeadRatherThanPerUnit).
	//
	// KEYED ON THE ROLE POINTER, not the handle: it identifies the seat whose
	// slice is appended to, and a handle lookup would merge two seats that
	// share one in an unadmitted chart.
	byLeadCache := map[*Role]map[string]struct{}{}

	for u := range o.AllUnits() {
		if u.Lead == "" || len(u.Roles) == 0 {
			continue
		}
		lead := leads().EffectiveLead(u)
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
)

// DanglingRef is a name that resolved to nothing.
type DanglingRef struct {
	Kind RefKind
	// From is what carries the reference, by the DISPLAY name a person
	// reads: the unit (for a lead), the seat (for a unit reference or a
	// manages entry).
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
	default:
		return fmt.Sprintf("%s reference %q on %q resolves to nothing", d.Kind, d.To, d.From)
	}
}

// DanglingRefs reports the soft references [Organization.Normalize] could not
// resolve, in author order (root seats' unit references, unit leads, manages
// entries). It assumes Normalize has run.
//
// NOT validation errors: live config bootstraps an org in pieces, so rejecting
// a partially wired one would make per-entity bootstrap impossible. Still a
// WARNING, since an entry persisting across revisions is a misspelling nothing
// else reports (config.Company.DanglingRefs logs org_dangling_reference).
//
// A lead is reported on the unit declaring it ([Unit.DeclaredLead]), not its
// inheritors; a manages entry naming a unit with no seats is not a misspelling.
// Resolved as the engine does (seats by handle, units by key).
func (o *Organization) DanglingRefs() []DanglingRef {
	seats := make(map[string]struct{})
	for r := range o.AllRoles() {
		seats[r.Handle()] = struct{}{}
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
// RUNNABLE rules are what a running company depends on (a seat with no handle
// owns no inbox, two seats on one handle share one): nothing may run a company
// that breaks one. ADMISSION rules ([Organization.ValidateAdmission]) are
// authoring hygiene a later build may add or relax, so a submitted document is
// refused for one but an APPLIED revision only warns: in a rolling upgrade a
// newer peer may activate a revision admitted under rules this build lacks, and
// refusing it would split the fleet's epoch. The config layer picks the class.
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
// two units answering to one key, a unit reference on a seat declared inside
// a unit of a different key, and a schedule whose cron has five fields that
// the grammar refuses. See the class note above [Organization.Validate]. It
// assumes [Organization.Normalize] has run, so a root seat moved into its unit
// is counted once, where it now sits.
//
// NO NAME IS HELD UNIQUE, a seat's or a unit's, by rule rather than leniency:
// nothing references one by name (`lead:`, `manages:` and `unit:` name a handle
// or key), so readers of a name (a colleague lookup, a roster) answer a shared
// one with an honest list naming each seat's handle.
func (o *Organization) ValidateAdmission() error {
	return errors.Join(o.validateUnitKeys(), o.validateUnitRefs(), o.validateCronGrammar())
}

// validateCronGrammar refuses every schedule, a seat's or a unit's, whose cron
// the grammar refuses — see [Schedule.cronGrammar]; a seat's schedules are read
// here, not in [Role.Validate], per the class note above [Organization.Validate].
//
// The owner is named by its IDENTITY (a seat's handle, a unit's key): a name
// may be shared and the message must say which one to fix.
func (o *Organization) validateCronGrammar() error {
	var errs []error
	for r := range o.AllRoles() {
		for i, s := range r.Schedules {
			if err := s.cronGrammar(fmt.Sprintf("role %q", r.Handle())); err != nil {
				errs = append(errs, &SeatError{Seat: r, Field: []any{"schedules", i, "cron"}, Err: err})
			}
		}
	}
	for u := range o.AllUnits() {
		for i, s := range u.Schedules {
			if err := s.cronGrammar(fmt.Sprintf("unit %q", strings.TrimSpace(u.Key()))); err != nil {
				errs = append(errs, &UnitError{Unit: u, Field: []any{"schedules", i, "cron"}, Err: err})
			}
		}
	}
	return errors.Join(errs...)
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
				ErrDuplicateHandle, g.key, len(g.members), describeSeats(g.members)),
		})
	}
	return errors.Join(errs...)
}

// validateUnitKeys enforces chart-wide uniqueness of a unit's KEY — the
// address [Unit.Key] names, its id or, on a unit that declares none, its
// name: ONE message per key, naming every unit that answers to it.
//
// A collision is silent: [Organization.Unit] resolves a key to the FIRST unit
// answering, so one unit receives the other's work. Folded by [NormalizeKey],
// as [Organization.UnitByRef] resolves, or units admitted here would resolve to
// whichever was walked first. A NAME IS NOT A KEY: units named alike on
// distinct ids are two addresses ([Organization.ValidateAdmission]). THE FIRST
// SPELLING MET OWNS THE GROUP, as the operator searches for what they wrote.
// An ADMISSION rule (class note above [Organization.Validate]).
func (o *Organization) validateUnitKeys() error {
	groups := groupBy(o.placedUnits(), func(u placedUnit) string {
		return NormalizeKey(u.unit.Key())
	})
	var errs []error
	for _, g := range groups {
		spelling := strings.TrimSpace(g.members[0].unit.Key())
		carriers := make([]*Unit, len(g.members))
		for i, m := range g.members {
			carriers[i] = m.unit
		}
		errs = append(errs, &DuplicateError{
			Kind: DuplicateUnitKey, Key: spelling, Units: carriers,
			Err: fmt.Errorf(
				"%w %q: %d units answer to it (%s). A unit's key is its id "+
					"when it declares one and its name otherwise; a manages "+
					"entry or a seat's unit reference resolves to the first "+
					"unit answering to it, and the org chart gives one address "+
					"to one unit, so one team's work, routing and pages are "+
					"filed under another. Give each of these units its own id",
				ErrDuplicateUnit, spelling, len(g.members), describeUnits(g.members)),
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

// describeSeats renders seats sharing a handle for one grouped message: each
// seat's name and where it sits, which is what tells them apart.
func describeSeats(seats []placedSeat) string {
	parts := make([]string, len(seats))
	for i, s := range seats {
		parts[i] = fmt.Sprintf("seat %q %s", s.role.Name, s.place)
	}
	return strings.Join(parts, "; ")
}

// describeUnits renders units sharing a key for one grouped message: each
// unit's name, where it sits and the key AS IT WROTE IT, because two units
// answering to one address may spell it two ways (`Product Team` and
// `product-team`), and the spelling is what an operator changes.
func describeUnits(units []placedUnit) string {
	parts := make([]string, len(units))
	for i, u := range units {
		parts[i] = fmt.Sprintf("unit %q %s (key %q)", u.unit.Name, u.place,
			strings.TrimSpace(u.unit.Key()))
	}
	return strings.Join(parts, "; ")
}

// validateContactIdentities enforces chart-wide uniqueness of every external
// account a seat is reachable at: ONE message per identity, naming every seat.
//
// Consumers resolve a duplicate DIFFERENT WAYS (the registry registers it for
// neither seat, every chart walk answers the first). Checked on the DECLARED
// values, as [HumanContact.Identities] explains; two references resolving to
// one value are the registry's ([notify.SharedIdentity]). COMPARED AS THE
// VALUES SIT: [HumanContact.Normalize] already folded the case-insensitive
// platforms' values, so folding again would refuse `slack_user_id` values
// differing only in case. Per FIELD, not transport. A RUNNABLE rule (class
// note above [Organization.Validate]).
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
					describeSeats(g.members)),
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
	// THE LEAD ONLY OF A UNIT WITH A SCHEDULE IT WOULD RUN, and through one
	// index of the chart built at the first such unit, for the reason
	// autoManageByLead gives: asked of every unit, the chart's own method
	// walked every seat for each inherited lead on every validation, whether
	// or not the unit had a schedule to judge.
	leads := sync.OnceValue(o.Index)
	runsAsLead := func(s Schedule) bool { return s.IsEnabled() && s.TargetsLead() }
	for u := range o.AllUnits() {
		if !slices.ContainsFunc(u.Schedules, runsAsLead) {
			continue
		}
		lead := leads().EffectiveLead(u)
		if lead == nil || !lead.IsHuman() {
			continue
		}
		for i, s := range u.Schedules {
			if !runsAsLead(s) {
				continue
			}
			errs = append(errs, &UnitError{Unit: u, Field: []any{"schedules", i}, Err: fmt.Errorf(
				"unit %q: schedule %q: %w: it targets the unit lead, but the effective lead %q is a human seat — define the schedule on an agent seat instead",
				u.Name, s.Name, ErrUnrunnableSchedule, lead.Name)})
		}
	}
	return errors.Join(errs...)
}
