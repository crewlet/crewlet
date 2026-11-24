package api

import (
	"slices"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/tools"
)

// The org projection: the company's identity and its seat and unit tree, as
// GET /org, the socket snapshot's `org` key and the `org` push carry it.
//
// # An explicit type, never a marshal of the config's own structs
//
// These surfaces answer EVERYBODY (the org chart is the overview, ADR-0033);
// /config is admin's. Marshalling config.Role and Unit was deny-by-omission:
// contact identities, `${VAR}` names, placement labels and sandbox setup
// commands (where a registry credential is written) reached everybody, though
// no secret tag covers them. A field now reaches everybody only by being
// written into these types; TestEveryOrgFieldIsClassified fails on a config
// field nobody has classified.

// OrgProjection is the reader's view of a company: the overview everybody
// sees, and an admin with the resolved fields beside it.
//
// Seats and units come from the organisation this node runs
// ([org.Organization]), the tree a turn reads. Fields carry what the founder
// WROTE (a declared handle, a unit's own lead) so declared reads apart from
// inherited; Derived has the effective hierarchy. Timezone alone is resolved:
// an unwritten one IS UTC, and an empty one a client defaults to the browser's
// zone. Token budgets are ceilings in force, beside the /budgets meters.
//
// Every field is omitted when empty, so a node with no active company answers
// `{}`, which the dashboard reads as "nothing loaded".
type OrgProjection struct {
	Name     string   `json:"name,omitempty"`
	Mission  string   `json:"mission,omitempty"`
	Vision   string   `json:"vision,omitempty"`
	Policies []string `json:"policies,omitempty"`

	// Timezone is the company's ONE clock (ADR-0018) as the engine resolves
	// it: the IANA name the settings write, or `UTC` where they write none.
	//
	// AN OVERVIEW FIELD, because every date a reader is shown is cut on
	// it — "today", "this week", a due band, an overdue mark — and a screen
	// that cut them on the browser's own zone would put a task under a
	// different day from the one the engine's own answer beside it names.
	// It says where a company keeps its hours, which its every timestamp
	// already does; it names no account, no credential and nothing to dial.
	Timezone string `json:"timezone,omitempty"`

	// TokenBudget is the company's own ceiling per calendar window, as the
	// settings write it: the windows it caps and nothing for the ones it
	// leaves open. An overview field for the reason the package doc
	// gives; how much of each window is spent is the /budgets answer.
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty"`

	// Services is which tracker, knowledge base and chat the company runs,
	// and whether the engine hosts its code, as the engine resolves them:
	// see [OrgServices].
	Services *OrgServices `json:"services,omitempty"`

	Roles []OrgSeat `json:"roles,omitempty"`
	Units []OrgUnit `json:"units,omitempty"`

	// Derived is the hierarchy the engine derives from the document above:
	// each seat's handle, its effective unit, its primary manager and
	// reports, and each unit's effective type, lead and channel.
	//
	// THE ENGINE SAYS IT RATHER THAN A CLIENT WORKING IT OUT: every rule here
	// is one a second implementation gets wrong (see [config.Derive]). The
	// fields above stay as WRITTEN, so declared and inherited leads read apart.
	//
	// WITHOUT PATHS, the one difference from the admin's configuration
	// answers: a reader has no document to point into, and membership is in
	// each unit's seats. Otherwise these are the fields above, resolved.
	Derived *config.Derived `json:"derived,omitempty"`
}

// OrgServices is which tracker, knowledge base, chat and git host the company
// runs, as [config.Company.TrackerBackendFor],
// [config.Company.KnowledgeBackendFor], [config.Company.ChatBackendFor] and
// [config.Company.CodeBackendFor] resolve it: the named backend, else the connected vendor, else the engine's own. The
// git host is `none` unless the document names `gitlet` (a GitHub or GitLab
// repository is an integration, never a host drawn in Gitlet's place).
//
// AN OVERVIEW FIELD, the one narrow exception to the guarded `tracker:`,
// `knowledge:`, `chat:` and `code:` blocks: which surfaces exist depends on it.
// NOTHING ELSE of any block reaches this audience (see the classification
// test). CHAT NAMES A POSTURE, NOT A PRODUCT: `vendor` may be Mattermost, Slack
// or both, told to a Chat holder on the `viewer` answer (`chat_vendors`).
type OrgServices struct {
	// Tracker is `worklet`, `jira` or `none`.
	Tracker string `json:"tracker"`
	// Knowledge is `doclet`, `confluence` or `none`.
	Knowledge string `json:"knowledge"`
	// Chat is `chatlet`, `vendor` or `none`.
	Chat string `json:"chat"`
	// Code is `gitlet` or `none`.
	Code string `json:"code"`

	// Restart is which of the four THIS NODE serves only once it restarts,
	// each by its key here (`tracker`, `knowledge`, `chat`, `code`): the
	// engine's own services the company turned on while this node's runtime
	// was already running without them ([engine.Engine.AwaitingRestart]).
	// Absent where the node serves everything the company runs.
	//
	// THE ONE NODE FACT ON A COMPANY'S PROJECTION, here because it decides
	// which dashboard surfaces exist FOR THIS READER, who reads through the
	// node that built it: `chatlet` on a node that cannot serve Chatlet yet
	// would draw a workspace whose every screen is refused, with nothing saying
	// a restart brings it. Named by key, not as a second set of backends, so
	// a reader still learns what the company runs.
	Restart []string `json:"restart,omitempty"`
}

// servicesOf is the company's resolved backends, and which of them this node
// serves only once restarted, as awaiting reads them off the same company; a
// nil awaiting names none.
func servicesOf(c *config.Company, awaiting func(*config.Company) []string) *OrgServices {
	services := &OrgServices{
		Tracker:   string(c.TrackerBackendFor()),
		Knowledge: string(c.KnowledgeBackendFor()),
		Chat:      string(c.ChatBackendFor()),
		Code:      string(c.CodeBackendFor()),
	}
	if awaiting != nil {
		services.Restart = slices.Clone(awaiting(c))
	}
	return services
}

// OrgSeat is the public half of one seat: who it is and what it is for.
//
// Plain strings rather than the config's enum types, so the wire contract does
// not move when an internal type is renamed or reshaped.
//
// LLM and ToolSources are RESOLVED (a second implementation gets the rule
// wrong): LLM per [phase.Resolve], ToolSources the servers GRANTED
// ([config.MCPServer.Grants]), both from the seat AS THE ENGINE RUNS IT. NEITHER
// REACHES THE OVERVIEW: provider keys and server names come from RUNTIME
// settings `GET /config` serves only to admin, so the projection is
// [stream.Shaped] on push, snapshot and GET /org, and nobody gets the entries
// they name or any `mcp_env` credential.
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

	// Avatar is RESOLVED (see [OrgSeat]) and reaches every audience: the
	// character and colour an agent seat is drawn as, defaults filled in so no
	// screen keeps a copy of them. Presentation only. Absent on a human seat.
	Avatar *OrgAvatar `json:"avatar,omitempty"`

	// TokenBudget is this seat's own ceilings, as the document writes
	// them; a window it does not name is capped only by the company's.
	TokenBudget *OrgTokenBudget `json:"token_budget,omitempty"`

	// LLM is RESOLVED, and only an admin audience is handed it (see
	// [OrgSeat]): each phase's provider chain keyed by the phase's wire name,
	// the first entry the model it runs on, later ones fallbacks in the order
	// tried. Present for every phase on an agent seat of a company with a
	// provider; absent on a human seat, with no provider, and for non-admins.
	LLM map[string][]string `json:"llm,omitempty"`

	// ToolSources is RESOLVED, and only an admin audience is handed it (see
	// [OrgSeat]): where this seat's tools come from, in the registry's origin
	// grammar: `builtin`, then `mcp:<server>` per granted server in company
	// order. What the seat is GRANTED, not what runs: a server that failed to
	// start is still listed (the node status record's MCP report says it
	// failed). Absent on a human seat and for non-admins.
	ToolSources []string `json:"tool_sources,omitempty"`
}

// OrgAvatar is how an agent seat is drawn: a character id and a colour name,
// as plain strings for the reason OrgSeat spells its enums as strings.
type OrgAvatar struct {
	Character string `json:"character"`
	Color     string `json:"color"`
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
	// `manages` entry is the authored value, which may name a unit, and with
	// only display names a client could not resolve it (a name differs from the
	// key once a unit declares an id). Handing it the id-or-name rule instead
	// would be a second derivation of what the engine keeps in one place.
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
// Read through the source on every call, not captured: an apply replaces the
// company, and a projection built at boot would show a deleted seat.
//
// awaiting is which services this node serves only once restarted, read off
// the SAME company the rest is built from ([OrgServices.Restart]).
//
// Every slice is COPIED: the projection goes to the socket hub and REST
// callers while the applied company is shared by every reader of the epoch, so
// an aliased slice is only as immutable as the most careless reader.
func orgProjection(company func() (*config.Company, *org.Organization),
	awaiting func(*config.Company) []string) OrgProjection {

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
		models:  c.Providers.PhaseModels(),
		servers: c.MCPServers,
		avatars: avatars(c),
	}
	return OrgProjection{
		Name:        c.Name,
		Mission:     c.Mission,
		Vision:      c.Vision,
		Policies:    slices.Clone(c.Policies),
		Timezone:    c.Location().String(),
		TokenBudget: orgTokenBudget(c.TokenBudget),
		Services:    servicesOf(c, awaiting),
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
	// models is providers.llm's keys in config order and the LLM fleets.
	models  phase.Models
	servers []config.MCPServer

	// avatars is each authored seat's avatar by handle. The DOCUMENT's, not
	// the running organisation's: how a seat is drawn is presentation the
	// runtime model has no use for, so it is read where it is written, and
	// found by the handle both shapes carry — the one key that names a seat
	// in either.
	avatars map[string]*config.RoleAvatar
}

// avatars is every seat's authored avatar in c, keyed by the handle the seat
// runs under ([config.Role.IdentityKey], the one the running organisation
// carries) — nil entries included, which [config.RoleAvatar.Resolved] answers
// with the defaults.
func avatars(c *config.Company) map[string]*config.RoleAvatar {
	out := map[string]*config.RoleAvatar{}
	for role := range c.EachRole() {
		out[role.IdentityKey()] = role.Avatar
	}
	return out
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
			look := b.avatars[r.Handle()].Resolved(r.Handle())
			seat.Avatar = &OrgAvatar{Character: string(look.Character), Color: string(look.Color)}
		}
		out = append(out, seat)
	}
	return out
}

// llm is every phase's resolved chain, nil for a company with no provider.
func (b orgBuilder) llm(seat *org.Role) map[string][]string {
	if len(b.models.Order) == 0 {
		return nil
	}
	out := make(map[string][]string, len(phase.All))
	for _, ph := range phase.All {
		out[ph.String()] = phase.Resolve(seat, ph, b.models).Keys
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

// For is the projection as audience may read it: whole for an admin, and
// without any seat's resolved model chain or tool sources for everybody else —
// see the package doc. Every other field is the reader's. The variant is named
// after which of the two it is, `admin` or `overview`.
//
// A COPY, never the projection edited in place: one projection is handed to
// every client of a broadcast, each in its own shape.
func (p OrgProjection) For(audience stream.Audience) (string, any) {
	if audience.Admin() {
		return "admin", p
	}
	narrow := p
	narrow.Roles = withoutResolved(p.Roles)
	narrow.Units = unitsWithoutResolved(p.Units)
	return "overview", narrow
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
