package api

import (
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// The org projection: the company's identity and its seat and unit tree, as
// GET /org, the socket snapshot's `org` key and the `org` push carry it.
//
// # An explicit public type, never a marshal of the config's own structs
//
// These three surfaces are READABLE WITHOUT A KEY under the default posture
// (`api.auth.anonymous: public` — GET /org is the public reach's one route),
// and the only surface that shows the whole document, /config, is admin in
// full. The projection used to marshal
// config.Role and config.Unit verbatim and rely on nothing else, which made it
// deny-by-omission: every field somebody added to a role was public the day it
// landed. That had already happened. Contact identities (a person's Slack
// member id, GitHub login, email), `${VAR}` names in a Mattermost username,
// placement labels and sandbox setup commands (routinely the place a registry
// credential is written) all reached an anonymous reader, and the secret tags
// redaction relies on covered none of them because none of them is a
// credential in the narrow sense.
//
// So the public shape is spelled out here field by field, and a field reaches
// a reader only by being written into one of these types on purpose.
// Everything else stays behind the admin `config` query, which is where the
// dashboard reads it. Configured work that has a read surface of its own is
// not repeated here either: schedules are described by /schedules.
// TestEveryOrgFieldIsClassified fails the day config.Company, config.Role or
// config.Unit gains a field nobody has classified, so a new field needs a
// decision rather than defaulting to either side.
//
// # Every field names the reach it is served at
//
// The projection is one shape served to three reaches (ADR-0031), and every
// field of every type below carries a `reach` tag saying the lowest one that
// reads it; [orgProjection] zeroes, at every depth, each field the caller's
// reach does not cover, which omits it on the wire. The withholding rule
// decides each tag:
//
//   - PUBLIC is the chart — who the company is (its name, mission, vision and
//     clock) and its shape: every seat's name, handle and kind, who manages
//     whom, the units, their types and leads, and the hierarchy derived from
//     them. The sign-in page names the company from it, and a company's chart
//     is the face it shows.
//   - MEMBER adds what the company published for the people in it: each
//     seat's goal, backstory, responsibilities, behavioural guidelines and
//     availability, each unit's purpose, goals, knowledge references and
//     channel, and the company's policies. A colleague reads them; a stranger
//     has no use for them.
//   - ADMIN adds how the company is RUN: every token ceiling, each seat's
//     resolved model chains and its tool sources — which provider entries and
//     which servers the engine is wired to.
//
// A field whose tag is missing or names no reach is withheld from EVERY
// reader, an admin included — never published by default — and the
// classification test fails on one before it ships. The schedules, the
// placement, the sandbox and the workers are not carried at any reach: an
// admin reads them through the config query, and a member reads the routines
// through the schedules surface.
//
// # Authored values, not effective ones
//
// Each field carries what the founder WROTE: a seat's declared handle (empty
// when the engine derives it from the name), a unit's own lead (empty when it
// inherits one). The effective hierarchy the engine derives is a different
// fact with a different shape, and mixing the two into one field would leave a
// reader unable to tell a declared lead from an inherited one.
//
// The company's clock is the one exception, and it is not a mixture: an
// unwritten `timezone` IS UTC, with no inheritance and nothing a reader could
// want to tell apart, so [OrgProjection.Timezone] carries the clock the engine
// resolved rather than leaving a client to default an empty string — which a
// browser does to ITS OWN zone.
//
// # Resolved values: what a seat runs on and what it can reach
//
// Two seat fields are RESOLVED rather than authored, and both for the reason
// the derived hierarchy is: the rule is one a second implementation gets
// wrong. [OrgSeat.LLM] is every phase's provider chain as a turn resolves it
// ([phase.Resolve], the function the turn's own chain goes through) — a seat
// can write `llm` in three shapes, override any phase with a flat
// `llm_<phase>` field, or write nothing and land on the company's `default`
// provider or its first — so the authored fields alone would leave a reader
// to re-run four levels of precedence. [OrgSeat.ToolSources] is the servers
// the seat is GRANTED ([config.MCPServer.Grants], the rule the engine starts
// a seat's children by), which depends on the company's server list and on
// credentials the seat may inherit from its unit.
//
// Neither carries anything a reader could act with. A provider KEY is the
// label a document gives a model entry (`fast`, `review`), never the model's
// account, endpoint or key, and a server NAME is the label its tools are
// already prefixed with in every prompt; the entries they name, and every
// credential under `mcp_env`, stay guarded. Both are served to an admin alone
// all the same: which model entries and servers a seat is wired to is how the
// company is run.

// OrgProjection is the classified view of a company: what a reach is served of
// its chart.
//
// Every field is omitted when empty, so a node with no active company answers
// `{}`, which is the shape the dashboard already reads as "nothing loaded" —
// and a field withheld from the caller's reach is omitted the same way.
type OrgProjection struct {
	Name     string   `json:"name,omitempty" reach:"public"`
	Mission  string   `json:"mission,omitempty" reach:"public"`
	Vision   string   `json:"vision,omitempty" reach:"public"`
	Policies []string `json:"policies,omitempty" reach:"member"`

	// Timezone is the company's ONE clock (ADR-0018) as the engine resolves
	// it: the IANA name the document writes, or `UTC` where it writes none.
	//
	// PUBLIC, because every date an anonymous reader is shown is cut on it
	// — "today", "this week", a due band, an overdue mark — and a screen
	// that cut them on the browser's own zone would put a task under a
	// different day from the one the engine's own answer beside it names.
	// It says where a company keeps its hours, which its every timestamp
	// already does; it names no account, no credential and nothing to dial.
	Timezone string `json:"timezone,omitempty" reach:"public"`

	// TokenBudget is the company's own ceiling per calendar window, as the
	// document writes it: the windows it caps and nothing for the ones it
	// leaves open. ADMIN: what the company may spend is how it is run, beside
	// the /budgets meters that say how much of each window is spent — and a
	// member is told which windows are refusing or near, and when each turns
	// over, with no figure (the `budget` push's member variant).
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty" reach:"admin"`

	Roles []OrgSeat `json:"roles,omitempty" reach:"public"`
	Units []OrgUnit `json:"units,omitempty" reach:"public"`

	// Derived is the hierarchy the engine derives from the document above:
	// each seat's handle, its effective unit, its primary manager and
	// reports, and each unit's effective type, lead and channel.
	//
	// THE ENGINE SAYS IT RATHER THAN A CLIENT WORKING IT OUT. Every rule
	// here is one a second implementation gets wrong, and the dashboard's
	// TypeScript had already diverged from the engine on three of them and
	// on the handle of a name with a dotted capital I; see [config.Derive].
	// The fields above stay as WRITTEN, so a reader can still tell a
	// declared lead from an inherited one.
	//
	// WITHOUT PATHS, which is the one difference from the guarded answers:
	// a reader of the chart is given no document to point into, and
	// membership is in each unit's seats. Nothing here is a fact the fields
	// above do not already carry, resolved — and each field is served at the
	// reach of the authored one it resolves (a unit's effective channel is
	// its channel's).
	Derived *OrgDerived `json:"derived,omitempty" reach:"public"`
}

// OrgSeat is one seat on the chart: who it is and what it is for.
//
// Plain strings rather than the config's enum types, so the wire contract does
// not move when an internal type is renamed or reshaped.
type OrgSeat struct {
	Name                 string   `json:"name" reach:"public"`
	Kind                 string   `json:"kind,omitempty" reach:"public"`
	Handle               string   `json:"handle,omitempty" reach:"public"`
	Goal                 string   `json:"goal,omitempty" reach:"member"`
	Backstory            string   `json:"backstory,omitempty" reach:"member"`
	Responsibilities     []string `json:"responsibilities,omitempty" reach:"member"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty" reach:"member"`
	Manages              []string `json:"manages,omitempty" reach:"public"`
	Availability         string   `json:"availability,omitempty" reach:"member"`

	// TokenBudget is this seat's own ceilings, as written; a window it does
	// not name is capped only by the company's.
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty" reach:"admin"`

	// LLM is RESOLVED (see the package doc): each phase's provider chain,
	// keyed by the phase's wire name, the first key the model it runs on and
	// every later one a fallback in the order they are tried. Every phase is
	// present on an agent seat of a company with a provider, and the field
	// is absent on a human seat, which runs no model, and on a company that
	// configures none.
	LLM map[string][]string `json:"llm,omitempty" reach:"admin"`

	// ToolSources is RESOLVED (see the package doc): where this seat's tools
	// come from, in the registry's own origin grammar — `builtin` first, then
	// `mcp:<server>` for each server it is granted, in the order the company
	// declares them. What the seat is GRANTED, not what is running: a server
	// that failed to start is still listed, and the node heartbeat's MCP
	// report is what says it failed. Absent on a human seat.
	ToolSources []string `json:"tool_sources,omitempty" reach:"admin"`
}

// OrgTokenBudget is a token budget on the wire: the most one scope may spend
// in each calendar window on the company clock, a window with no ceiling
// absent. Its own type rather than config.TokenBudget, for the reason OrgSeat
// spells its enums as strings.
type OrgTokenBudget struct {
	Day   *int `json:"day,omitempty" reach:"admin"`
	Week  *int `json:"week,omitempty" reach:"admin"`
	Month *int `json:"month,omitempty" reach:"admin"`
}

// OrgDerived is the hierarchy the engine derives ([config.Derive]) as the
// chart carries it: without the document paths. Its own types rather than the
// config's, so every field names the reach it is served at as the authored
// ones do.
type OrgDerived struct {
	// Seats is every seat in the engine's own order.
	Seats []OrgDerivedSeat `json:"seats" reach:"public"`
	// Units is every unit, depth first, parents before children.
	Units []OrgDerivedUnit `json:"units" reach:"public"`
}

// OrgDerivedSeat is one seat as the engine runs it. See [config.DerivedSeat]
// for each field.
type OrgDerivedSeat struct {
	Handle          string   `json:"handle" reach:"public"`
	Name            string   `json:"name" reach:"public"`
	Kind            string   `json:"kind" reach:"public"`
	PlacedByRef     bool     `json:"placed_by_ref" reach:"public"`
	Manager         string   `json:"manager" reach:"public"`
	Managers        []string `json:"managers" reach:"public"`
	Reports         []string `json:"reports" reach:"public"`
	AutoReports     []string `json:"auto_reports" reach:"public"`
	OnboardingChain []string `json:"onboarding_chain" reach:"public"`
}

// OrgDerivedUnit is one unit as the engine runs it. See [config.DerivedUnit]
// for each field.
//
// The effective CHANNEL is a member's, as the authored one is, and omitted when
// empty so a reach it is withheld from is not told a unit has none.
type OrgDerivedUnit struct {
	Name             string   `json:"name" reach:"public"`
	Type             string   `json:"type" reach:"public"`
	Lead             string   `json:"lead" reach:"public"`
	LeadInherited    bool     `json:"lead_inherited" reach:"public"`
	Channel          string   `json:"channel,omitempty" reach:"member"`
	ChannelInherited bool     `json:"channel_inherited,omitempty" reach:"member"`
	Seats            []string `json:"seats" reach:"public"`
}

// orgDerived is the derived hierarchy without its paths.
func orgDerived(d config.Derived) *OrgDerived {
	out := &OrgDerived{}
	if d.Seats != nil {
		out.Seats = make([]OrgDerivedSeat, 0, len(d.Seats))
	}
	for _, s := range d.Seats {
		out.Seats = append(out.Seats, OrgDerivedSeat{
			Handle: s.Handle, Name: s.Name, Kind: s.Kind, PlacedByRef: s.PlacedByRef,
			Manager: s.Manager, Managers: s.Managers, Reports: s.Reports,
			AutoReports: s.AutoReports, OnboardingChain: s.OnboardingChain,
		})
	}
	if d.Units != nil {
		out.Units = make([]OrgDerivedUnit, 0, len(d.Units))
	}
	for _, u := range d.Units {
		out.Units = append(out.Units, OrgDerivedUnit{
			Name: u.Name, Type: u.Type, Lead: u.Lead, LeadInherited: u.LeadInherited,
			Channel: u.Channel, ChannelInherited: u.ChannelInherited, Seats: u.Seats,
		})
	}
	return out
}

// orgTokenBudget copies an authored budget, nil when it caps nothing so the
// field is omitted rather than written as `{}`.
func orgTokenBudget(b config.TokenBudget) *OrgTokenBudget {
	if b.Day == nil && b.Week == nil && b.Month == nil {
		return nil
	}
	clone := func(v *int) *int {
		if v == nil {
			return nil
		}
		n := *v
		return &n
	}
	return &OrgTokenBudget{Day: clone(b.Day), Week: clone(b.Week), Month: clone(b.Month)}
}

// OrgUnit is one unit on the chart, nesting to any depth.
type OrgUnit struct {
	Name      string    `json:"name" reach:"public"`
	Type      string    `json:"type,omitempty" reach:"public"`
	Purpose   string    `json:"purpose,omitempty" reach:"member"`
	Lead      string    `json:"lead,omitempty" reach:"public"`
	Goals     []string  `json:"goals,omitempty" reach:"member"`
	Channel   string    `json:"channel,omitempty" reach:"member"`
	Knowledge []string  `json:"knowledge,omitempty" reach:"member"`
	Roles     []OrgSeat `json:"roles,omitempty" reach:"public"`
	Children  []OrgUnit `json:"children,omitempty" reach:"public"`
}

// orgProjection builds the current company's chart as reach is served it.
//
// Read through the source on every call rather than captured, because an apply
// replaces the company and a projection built at boot would keep showing a
// seat a revision deleted.
//
// Every slice is COPIED, which the marshal round trip this replaced did
// implicitly. The projection is handed to the socket hub and to REST callers,
// while the applied company is shared by every reader of the epoch, so an
// aliased slice would make the projection only as immutable as every one of
// those readers is careful — and the withholding below writes into the value
// it built, which must never be the company's.
func orgProjection(company func() *config.Company, reach auth.Reach) OrgProjection {
	if company == nil {
		return OrgProjection{}
	}
	c := company()
	if c == nil {
		return OrgProjection{}
	}
	b := orgBuilder{
		asRun:     c.SeatsAsRun(),
		providers: c.Providers.ProviderOrder(),
		servers:   c.MCPServers,
	}
	out := OrgProjection{
		Name:        c.Name,
		Mission:     c.Mission,
		Vision:      c.Vision,
		Policies:    slices.Clone(c.Policies),
		Timezone:    c.Location().String(),
		TokenBudget: orgTokenBudget(c.TokenBudget),
		Roles:       b.seats(c.Roles),
		Units:       b.units(c.Units),
		Derived:     orgDerived(config.Derive(c)),
	}
	withhold(reflect.ValueOf(&out).Elem(), reach)
	return out
}

// withhold zeroes every field of the struct v whose `reach` tag reach does not
// cover, and descends into every field it keeps — through pointers and slices
// to the structs they hold — so a seat nested three units deep is withheld by
// the same rule as the company.
//
// A FIELD WITH NO VALID TAG IS ZEROED FOR EVERY REACH, an admin's included: an
// unclassified field is withheld rather than published by default, and
// TestEveryOrgFieldIsClassified fails on it before it ships. A map's values
// are not descended into; no projection type holds a struct in one.
func withhold(v reflect.Value, reach auth.Reach) {
	typ := v.Type()
	for i := range typ.NumField() {
		if !typ.Field(i).IsExported() {
			continue
		}
		field := v.Field(i)
		need := auth.Reach(typ.Field(i).Tag.Get("reach"))
		if !need.Valid() || !reach.Covers(need) {
			field.SetZero()
			continue
		}
		withholdWithin(field, reach)
	}
}

// withholdWithin applies [withhold] to every struct a kept value holds.
func withholdWithin(v reflect.Value, reach auth.Reach) {
	switch v.Kind() {
	case reflect.Struct:
		withhold(v, reach)
	case reflect.Pointer:
		if !v.IsNil() {
			withholdWithin(v.Elem(), reach)
		}
	case reflect.Slice:
		for i := range v.Len() {
			withholdWithin(v.Index(i), reach)
		}
	}
}

// orgBuilder carries what a seat's resolved fields are resolved against.
type orgBuilder struct {
	// asRun is each authored seat's running form ([config.Company.SeatsAsRun]).
	asRun map[*config.Role]*org.Role
	// providers is providers.llm's keys in config order.
	providers []string
	servers   []config.MCPServer
}

func (b orgBuilder) seats(roles []config.Role) []OrgSeat {
	if len(roles) == 0 {
		return nil
	}
	out := make([]OrgSeat, 0, len(roles))
	for i := range roles {
		r := &roles[i]
		seat := OrgSeat{
			Name:                 r.Name,
			Kind:                 string(r.Kind),
			Handle:               r.Handle,
			Goal:                 r.Goal,
			Backstory:            r.Backstory,
			Responsibilities:     slices.Clone(r.Responsibilities),
			BehavioralGuidelines: slices.Clone(r.BehavioralGuidelines),
			Manages:              slices.Clone(r.Manages),
			Availability:         r.Availability,
			TokenBudget:          orgTokenBudget(r.TokenBudget),
		}
		if run := b.asRun[r]; run != nil && run.IsAgent() {
			seat.LLM = b.llm(run)
			seat.ToolSources = b.toolSources(run)
		}
		out = append(out, seat)
	}
	return out
}

// llm is every phase's resolved chain, nil for a company with no provider.
func (b orgBuilder) llm(seat *org.Role) map[string][]string {
	if len(b.providers) == 0 {
		return nil
	}
	out := make(map[string][]string, len(phase.All))
	for _, ph := range phase.All {
		out[ph.String()] = phase.Resolve(seat, ph, b.providers)
	}
	return out
}

// toolSources is the builtins, then each granted server in declaration order.
func (b orgBuilder) toolSources(seat *org.Role) []string {
	out := []string{tools.OriginBuiltin}
	for i := range b.servers {
		if b.servers[i].Grants(seat) {
			out = append(out, tools.Origin(b.servers[i].Name))
		}
	}
	return out
}

func (b orgBuilder) units(units []config.Unit) []OrgUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]OrgUnit, 0, len(units))
	for i := range units {
		u := &units[i]
		out = append(out, OrgUnit{
			Name:      u.Name,
			Type:      string(u.Type),
			Purpose:   u.Purpose,
			Lead:      u.Lead,
			Goals:     slices.Clone(u.Goals),
			Channel:   u.Channel,
			Knowledge: slices.Clone(u.Knowledge),
			Roles:     b.seats(u.Roles),
			Children:  b.units(u.Children),
		})
	}
	return out
}
