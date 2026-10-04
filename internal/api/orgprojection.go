package api

import (
	"slices"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/tools"
)

// The org projection: the company's identity and its seat and unit tree, as
// GET /org, the socket snapshot's `org` key and the `org` push carry it.
//
// # An explicit type, never a marshal of the config's own structs
//
// These three surfaces answer every principal holding `state:read` — the
// READER's grant, which a colleague who may change nothing about the
// deployment holds — while the only surface that shows the whole document,
// /config, asks `config:read`. Every route and socket is authenticated; there
// is no anonymous reader any more (`api.auth.allow_anonymous_read` is a named
// refusal in internal/config), but the audience here is still far wider than
// the configuration's. The projection used to marshal config.Role and
// config.Unit verbatim and rely on nothing else, which made it
// deny-by-omission: every field somebody added to a role reached this audience
// the day it landed. That had already happened, back when the audience was
// anybody at all. Contact identities (a person's Slack member id, GitHub
// login, email), `${VAR}` names in a Mattermost username, placement labels and
// sandbox setup commands (routinely the place a registry credential is
// written) all reached it, and the secret tags redaction relies on covered
// none of them because none of them is a credential in the narrow sense.
//
// So the reader's shape is spelled out here field by field, and a field
// reaches a `state:read` holder only by being written into one of these types
// on purpose. Everything else stays behind the `config:read` answers, which is
// where the dashboard reads it. Configured work that has a read surface of its
// own is not repeated here either: schedules are described by /schedules. A
// token budget IS carried, as the ceilings in force, beside the /budgets
// meters that say how much of each window is spent: a ceiling is a statement
// of what the company may spend, not an account or a credential, and a seat's
// page that could show the meter and not the rule behind it would be answering
// "how close" without "to what".
//
// TestEveryOrgFieldIsClassified fails the day config.Company, config.Role or
// config.Unit gains a field nobody has classified, so a new field needs a
// decision rather than defaulting to either side.
//
// # The tree is the running organisation's
//
// The seats and units are read from the organisation this node runs
// ([org.Organization]) — the document's `roles:` and `units:` as the engine
// built them, every root seat a `unit:` reference moved already in its unit —
// so the tree a reader is shown is the one a turn reads. The company's name,
// mission, vision, policies, clock and own token budget come from the
// document.
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
// credentials the seat may inherit from its unit. Both are resolved from the
// seat AS THE ENGINE RUNS IT — the running organisation, where a unit's
// credentials are already layered under its agent members — never from the
// authored role.
//
// NEITHER REACHES A `state:read` READER. A provider key names a model entry
// and a server name names a tool server, and both are read off a seat's
// RUNTIME settings, which `GET /config` serves only to a `config:read` holder;
// a label derived from them follows the same rule, or the projection would be
// a way round that guard. So the projection is
// [stream.Shaped]: a `config:read` audience is handed the resolved fields, and
// every other audience the same tree without them — on the push, the snapshot
// and GET /org alike. What they never carry, for any audience, is anything a
// reader could act with: the entries they name, and every credential under
// `mcp_env`, stay guarded.

// OrgProjection is the reader's view of a company: what `state:read` sees.
//
// Every field is omitted when empty, so a node with no active company answers
// `{}`, which is the shape the dashboard already reads as "nothing loaded".
type OrgProjection struct {
	Name     string   `json:"name,omitempty"`
	Mission  string   `json:"mission,omitempty"`
	Vision   string   `json:"vision,omitempty"`
	Policies []string `json:"policies,omitempty"`

	// Timezone is the company's ONE clock (ADR-0018) as the engine resolves
	// it: the IANA name the settings write, or `UTC` where they write none.
	//
	// A `state:read` FIELD, because every date a reader is shown is cut on
	// it — "today", "this week", a due band, an overdue mark — and a screen
	// that cut them on the browser's own zone would put a task under a
	// different day from the one the engine's own answer beside it names.
	// It says where a company keeps its hours, which its every timestamp
	// already does; it names no account, no credential and nothing to dial.
	Timezone string `json:"timezone,omitempty"`

	// TokenBudget is the company's own ceiling per calendar window, as the
	// settings write it: the windows it caps and nothing for the ones it
	// leaves open. A `state:read` field for the reason the package doc
	// gives; how much of each window is spent is the /budgets answer.
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty"`

	Roles []OrgSeat `json:"roles,omitempty"`
	Units []OrgUnit `json:"units,omitempty"`

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
	// WITHOUT PATHS, which is the one difference from the `config:read`
	// answers: a reader is given no document to point into, and membership
	// is in each unit's seats. Nothing else here is a fact the fields above
	// do not already carry, so it needs no classification of its own beyond
	// this: it is those values, resolved.
	Derived *config.Derived `json:"derived,omitempty"`
}

// OrgSeat is the public half of one seat: who it is and what it is for.
//
// Plain strings rather than the config's enum types, so the wire contract does
// not move when an internal type is renamed or reshaped.
type OrgSeat struct {
	Name                 string   `json:"name"`
	Kind                 string   `json:"kind,omitempty"`
	Handle               string   `json:"handle,omitempty"`
	Goal                 string   `json:"goal,omitempty"`
	Backstory            string   `json:"backstory,omitempty"`
	Responsibilities     []string `json:"responsibilities,omitempty"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty"`
	Manages              []string `json:"manages,omitempty"`
	Availability         string   `json:"availability,omitempty"`

	// TokenBudget is this seat's own ceilings, as the document writes
	// them; a window it does not name is capped only by the company's.
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty"`

	// LLM is RESOLVED, and only a `config:read` audience is handed it (see
	// the package doc): each phase's provider chain, keyed by the phase's
	// wire name, the first key the model it runs on and every later one a
	// fallback in the order they are tried. Every phase is present on an
	// agent seat of a company with a provider, and the field is absent on a
	// human seat, which runs no model, on a company that configures none,
	// and for every reader without `config:read`.
	LLM map[string][]string `json:"llm,omitempty"`

	// ToolSources is RESOLVED, and only a `config:read` audience is handed it
	// (see the package doc): where this seat's tools come from, in the
	// registry's own origin grammar — `builtin` first, then `mcp:<server>`
	// for each server it is granted, in the order the company declares them.
	// What the seat is GRANTED, not what is running: a server that failed to
	// start is still listed, and the node heartbeat's MCP report is what says
	// it failed. Absent on a human seat and for every reader without
	// `config:read`.
	ToolSources []string `json:"tool_sources,omitempty"`
}

// OrgTokenBudget is a token budget on the wire: the most one scope may spend
// in each calendar window on the company clock, a window with no ceiling
// absent. Its own type rather than config.TokenBudget, for the reason OrgSeat
// spells its enums as strings.
type OrgTokenBudget struct {
	Day   *int `json:"day,omitempty"`
	Week  *int `json:"week,omitempty"`
	Month *int `json:"month,omitempty"`
}

// orgTokenBudget copies the company's authored budget, nil when it caps nothing
// so the field is omitted rather than written as `{}`.
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

// seatTokenBudget is a seat's ceilings as the composed org holds them, nil when
// it caps nothing — the company's shape, so a reader draws both alike.
func seatTokenBudget(c org.TokenCeilings) *OrgTokenBudget {
	window := func(p period.Period) *int {
		n, capped := c[p]
		if !capped {
			return nil
		}
		return &n
	}
	out := OrgTokenBudget{Day: window(period.Day), Week: window(period.Week), Month: window(period.Month)}
	if out.Day == nil && out.Week == nil && out.Month == nil {
		return nil
	}
	return &out
}

// OrgUnit is the public half of one unit, nesting to any depth.
type OrgUnit struct {
	// ID is the unit's KEY — what a `manages:` entry and a seat's `unit:`
	// resolve — and it carries [config.Unit.IdentityKey] rather than the
	// authored field, so a unit that declares no id answers with the name
	// that is its key instead of with nothing.
	//
	// IT IS PUBLIC BECAUSE NOTHING ELSE HERE RESOLVES A REFERENCE. A seat's
	// `manages` entry on this projection is the authored value, which may
	// name a unit — and with only display names beside it a client had
	// nothing in the same response to resolve that key against. It either
	// matched on the name, which is a different value the moment a unit
	// declares an id, or rendered the raw string and called the reference
	// broken. Giving the client the id-or-name rule instead would be a
	// second derivation of the one thing this engine keeps in one place.
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name"`
	Type      string    `json:"type,omitempty"`
	Purpose   string    `json:"purpose,omitempty"`
	Lead      string    `json:"lead,omitempty"`
	Goals     []string  `json:"goals,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Knowledge []string  `json:"knowledge,omitempty"`
	Roles     []OrgSeat `json:"roles,omitempty"`
	Children  []OrgUnit `json:"children,omitempty"`
}

// orgProjection builds the reader's view of the current company.
//
// Read through the source on every call rather than captured, because an apply
// replaces the company and a projection built at boot would keep showing a
// seat a revision deleted.
//
// Every slice is COPIED, which the marshal round trip this replaced did
// implicitly. The projection is handed to the socket hub and to REST callers,
// while the applied company is shared by every reader of the epoch, so an
// aliased slice would make the projection only as immutable as every one of
// those readers is careful.
func orgProjection(company func() (*config.Company, *org.Organization)) OrgProjection {
	if company == nil {
		return OrgProjection{}
	}
	c, roster := company()
	if c == nil {
		return OrgProjection{}
	}
	// THE ROSTER for the tree, so a reader is shown the seats and units as
	// the engine runs them; the company's NAME, mission, vision, policies,
	// clock and own budget from the document.
	derived := config.DeriveFrom(roster)
	b := orgBuilder{
		providers: c.Providers.ProviderOrder(),
		servers:   c.MCPServers,
	}
	return OrgProjection{
		Name:        c.Name,
		Mission:     c.Mission,
		Vision:      c.Vision,
		Policies:    slices.Clone(c.Policies),
		Timezone:    c.Location().String(),
		TokenBudget: orgTokenBudget(c.TokenBudget),
		Roles:       b.seats(rootSeats(roster)),
		Units:       b.units(topUnits(roster)),
		Derived:     &derived,
	}
}

// rootSeats is the seats that belong to no unit, which is what the `roles:`
// half of this projection has always been.
func rootSeats(o *org.Organization) []*org.Role {
	if o == nil {
		return nil
	}
	return o.Roles
}

// topUnits is the units at the top of the tree; each carries its own children.
func topUnits(o *org.Organization) []*org.Unit {
	if o == nil {
		return nil
	}
	return o.Units
}

// orgBuilder carries what a seat's resolved fields are resolved against: the
// company's settings, while the seat they are resolved for is the running
// organisation's.
type orgBuilder struct {
	// providers is providers.llm's keys in config order.
	providers []string
	servers   []config.MCPServer
}

// seats is the public half of a list of seats, with the resolved fields of
// every agent seat.
//
// THE DECLARED HANDLE rather than the derived one, because these fields are
// the AS-WRITTEN half of the projection: `derived` beside them is where a
// reader finds what the engine resolved, and a left-hand side that quietly
// answered the resolved value would leave a client unable to tell a seat that
// declared a handle from one whose handle came from its name.
func (b orgBuilder) seats(roles []*org.Role) []OrgSeat {
	if len(roles) == 0 {
		return nil
	}
	out := make([]OrgSeat, 0, len(roles))
	for _, r := range roles {
		seat := OrgSeat{
			Name:                 r.Name,
			Kind:                 string(r.Kind),
			Handle:               r.DeclaredHandle,
			Goal:                 r.Goal,
			Backstory:            r.Backstory,
			Responsibilities:     slices.Clone(r.Responsibilities),
			BehavioralGuidelines: slices.Clone(r.BehavioralGuidelines),
			Manages:              slices.Clone(r.Manages),
			Availability:         r.Availability,
			TokenBudget:          seatTokenBudget(r.TokenBudget),
		}
		if r.IsAgent() {
			seat.LLM = b.llm(r)
			seat.ToolSources = b.toolSources(r)
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

// units is the public half of a list of units, nesting to any depth.
//
// THE DECLARED LEAD AND CHANNEL, on [orgBuilder.seats]' own reasoning: the
// cascade's answers are in `derived`, which reports whether each was
// inherited, and a left-hand side carrying the inherited value would make the
// two halves say the same thing and lose the distinction.
func (b orgBuilder) units(units []*org.Unit) []OrgUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]OrgUnit, 0, len(units))
	for _, u := range units {
		out = append(out, OrgUnit{
			ID:        u.Key(),
			Name:      u.Name,
			Type:      string(u.Type),
			Purpose:   u.Purpose,
			Lead:      u.DeclaredLead,
			Goals:     slices.Clone(u.Goals),
			Channel:   u.DeclaredChannel,
			Knowledge: slices.Clone(u.KnowledgeRefs),
			Roles:     b.seats(u.Roles),
			Children:  b.units(u.Children),
		})
	}
	return out
}

// For is the projection as audience may read it: whole for a `config:read`
// holder, and without any seat's resolved model chain or tool sources for
// everybody else — see the package doc. Every other field is the reader's.
//
// A COPY, never the projection edited in place: one projection is handed to
// every client of a broadcast, each in its own shape.
func (p OrgProjection) For(audience stream.Audience) (string, any) {
	if audience.Holds(iam.GrantConfigRead) {
		return "config:read", p
	}
	narrow := p
	narrow.Roles = withoutResolved(p.Roles)
	narrow.Units = unitsWithoutResolved(p.Units)
	return "state:read", narrow
}

// withoutResolved is seats with their resolved fields removed.
func withoutResolved(seats []OrgSeat) []OrgSeat {
	if seats == nil {
		return nil
	}
	out := slices.Clone(seats)
	for i := range out {
		out[i].LLM, out[i].ToolSources = nil, nil
	}
	return out
}

// unitsWithoutResolved is units whose every seat, at any depth, has its
// resolved fields removed.
func unitsWithoutResolved(units []OrgUnit) []OrgUnit {
	if units == nil {
		return nil
	}
	out := slices.Clone(units)
	for i := range out {
		out[i].Roles = withoutResolved(out[i].Roles)
		out[i].Children = unitsWithoutResolved(out[i].Children)
	}
	return out
}
