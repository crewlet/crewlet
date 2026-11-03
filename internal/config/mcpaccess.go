package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// WHICH AGENTS AN MCP SERVER REACHES, decided by one grant on its entry.
//
// A server reaches an agent seat through `access` on its `mcp_servers` entry
// and through nothing else: absent, it reaches every agent seat; written, it
// reaches exactly the seats it names, each unit it names standing for every
// agent seat beneath that unit at any depth, minus the seats `except` leaves
// out. That holds for a shared server, whose one instance's tools reach only
// the seats it is granted to, and for a per-agent server, which starts a child
// for each of those seats and for nobody else.
//
// A seat's `mcp_env` block is VALUES ONLY. It holds what the seat's child of a
// server it is granted is started with, and grants nothing, so a block for a
// server that does not reach the seat is refused rather than left to look like
// a grant ([Company.validateMCPAccess]).
//
// A SERVER BOUND TO A VENDOR'S PER-SEAT CREDENTIAL is the one exception: it
// reaches the seats that are on at that vendor's integration
// (`roles[].integrations.<lane>.enabled`), because that lane is where the
// engine provisions, seals and binds the seat's own credential, and two
// switches for one grant would disagree. Such a server takes no `access`.
//
// See adr/0076-an-mcp-server-reaches-an-agent-through-one-grant-on-its-entry.md.

// MCPAccess is the agent seats an MCP server reaches ([MCPServer.Access]).
//
// NAMES, AS THE CHART WRITES THEM: a seat by its handle and a unit by its key,
// matched exactly, as `lead:` and `manages:` are, since each is an identity
// written into the document on every write and never changed in place.
type MCPAccess struct {
	// Units are unit keys. Each reaches every agent seat in the unit and in
	// every unit beneath it, so a seat that joins a team is granted what the
	// team is.
	Units []string `yaml:"units,omitempty" json:"units,omitempty" desc:"Unit keys whose agent seats get this server, the seats of every unit beneath each included."`

	// Seats are agent seat handles, each reached by name.
	Seats []string `yaml:"seats,omitempty" json:"seats,omitempty" desc:"Agent seat handles that get this server."`

	// Except are agent seat handles a unit in Units would reach and that are
	// left out.
	Except []string `yaml:"except,omitempty" json:"except,omitempty" desc:"Agent seat handles left out although a unit in units holds them."`
}

// MCPReachReason is why an MCP server reaches an agent seat.
type MCPReachReason string

const (
	// MCPReachEveryAgent is a server with no `access`, which reaches every
	// agent seat.
	MCPReachEveryAgent MCPReachReason = "every_agent"
	// MCPReachUnit is a seat beneath a unit `access.units` names.
	MCPReachUnit MCPReachReason = "unit"
	// MCPReachSeat is a seat `access.seats` names.
	MCPReachSeat MCPReachReason = "seat"
	// MCPReachIntegration is a seat on at the integration whose per-seat
	// credential the server reads.
	MCPReachIntegration MCPReachReason = "integration"
)

// MCPReachReasons is the closed set, in the order a reader is told them.
func MCPReachReasons() []MCPReachReason {
	return []MCPReachReason{MCPReachEveryAgent, MCPReachUnit, MCPReachSeat, MCPReachIntegration}
}

// Valid reports whether r is one of [MCPReachReasons].
func (r MCPReachReason) Valid() bool { return slices.Contains(MCPReachReasons(), r) }

// MCPReach is why a server reaches one seat.
type MCPReach struct {
	Reason MCPReachReason
	// Via names what grants it: for [MCPReachUnit], the key of the unit in
	// `access.units` nearest the seat, and for [MCPReachIntegration], the lane
	// the seat is on at (the first, sorted). Empty otherwise.
	Via string
}

// Reach reports whether the server's grant reaches seat, and why, WHETHER OR
// NOT THE SERVER IS ON: [MCPServer.Grants] adds that. A turned-off server keeps
// its grant for the day it is turned back on, so a value stored for a seat, a
// test of a seat's child and the answer that lists who a server reaches read
// the grant itself.
//
// A human seat is reached by nothing: it runs no tools. The seat is the one
// the engine runs ([org.Organization] as the configuration built it), which
// carries where it sits ([org.Role.UnitKeys]) and the lanes it is on at
// ([org.Role.AccessLanes]).
//
// A seat both named and left out is reached: `seats` is the more particular
// of the two. Admission refuses that pair, so only a stored revision holds it.
func (m *MCPServer) Reach(seat *org.Role) (MCPReach, bool) {
	if seat == nil || !seat.IsAgent() {
		return MCPReach{}, false
	}
	if bound := m.SeatBindings(); len(bound) > 0 {
		for _, lane := range seat.AccessLanes {
			if slices.ContainsFunc(bound, func(k BoundSeatKey) bool { return k.Kind == SeatCredentialKind(lane) }) {
				return MCPReach{Reason: MCPReachIntegration, Via: lane}, true
			}
		}
		return MCPReach{}, false
	}
	a := m.Access
	if a == nil {
		return MCPReach{Reason: MCPReachEveryAgent}, true
	}
	handle := seat.Handle()
	if slices.Contains(a.Seats, handle) {
		return MCPReach{Reason: MCPReachSeat}, true
	}
	if slices.Contains(a.Except, handle) {
		return MCPReach{}, false
	}
	for _, key := range seat.UnitKeys {
		if slices.Contains(a.Units, key) {
			return MCPReach{Reason: MCPReachUnit, Via: key}, true
		}
	}
	return MCPReach{}, false
}

// grantLanes is every integration lane whose seats a server bound to a vendor's
// per-seat credential reaches, sorted; empty for any other server.
func (m *MCPServer) grantLanes() []string {
	bound := m.SeatBindings()
	var out []string
	for _, lane := range SeatAccessLanes() {
		if slices.ContainsFunc(bound, func(k BoundSeatKey) bool { return k.Kind == SeatCredentialKind(lane) }) {
			out = append(out, lane)
		}
	}
	slices.Sort(out)
	return out
}

// markAccessLanes records on each agent seat of o the integration lanes its
// authored seat is on at ([org.Role.AccessLanes]): what [MCPServer.Reach]
// grants a server bound to a vendor's per-seat credential by.
func (c *Company) markAccessLanes(o *org.Organization) {
	for role := range c.EachRole() {
		seat := o.Role(role.IdentityKey())
		if seat == nil || !seat.IsAgent() {
			continue
		}
		var lanes []string
		for _, lane := range SeatAccessLanes() {
			if role.SeatAccessEnabled(lane) {
				lanes = append(lanes, lane)
			}
		}
		slices.Sort(lanes)
		seat.AccessLanes = lanes
	}
}

// validateMCPAccess holds every grant to what the chart holds, and every field
// that hands a seat a server's values or its tools to a seat the server
// reaches.
//
//   - `access` on a server bound to a vendor's per-seat credential is refused:
//     the integration's lane grants it, and two switches for one grant would
//     disagree.
//   - Each name in `access` must be one: a unit's key, or an AGENT seat's
//     handle (a person's seat runs no tools), each once, and no seat both
//     named and left out. A seat or a unit that is removed while a grant still
//     names it is refused the same way, naming the entry, as removing a
//     server some block still names is: the grant is changed first, by
//     whoever may change the server, so a later seat or unit that takes the
//     same handle or key is never granted by an entry written for another.
//   - A seat's `mcp_env` block for a server, a unit's block for one that
//     reaches none of the unit's own agent seats, and an executor box's
//     listing of one, are refused where the server does not reach the seat:
//     each would otherwise read as a grant the engine does not make.
//
// THE GRANT ITSELF, WHETHER OR NOT THE SERVER IS ON ([MCPServer.Reach]): a
// turned-off server's blocks are held to the rule a running one's are. A block
// the engine reads by its own name ([MCPServerReference.EngineRead]) is the
// engine's as well as the server's, so it is not held to the server's grant.
//
// An ADMISSION rule, like the rest of the file rules: a stored revision that
// breaks it runs, granting by the rule above, and warns.
func (c *Company) validateMCPAccess(o *org.Organization) error {
	var p problems
	servers := map[string]*MCPServer{}
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if name := strings.TrimSpace(server.Name); name != "" {
			servers[name] = server
		}
		if server.Access != nil {
			server.validateAccess(&p, at(idx(field("mcp_servers"), i), "access"), o)
		}
	}
	for role, path := range c.EachRole() {
		seat := o.Role(role.IdentityKey())
		if seat == nil || !seat.IsAgent() {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(role.MCPEnv)) {
			server, declared := servers[name]
			if !declared || engineReadsMCPEnv(name) {
				continue
			}
			if _, reached := server.Reach(seat); !reached {
				p.add(entry(at(path, "mcp_env"), name), ErrConflict,
					"%s, so nothing reads this block: an mcp_env block holds the values a "+
						"server the seat already gets is started with, and grants nothing. %s",
					server.unreached(fmt.Sprintf("seat %q", seat.Handle())), server.grantRemedy("the seat"))
			}
		}
		if role.Sandbox == nil {
			continue
		}
		for j, name := range role.Sandbox.MCP.Servers {
			server, declared := servers[name]
			if !declared {
				continue
			}
			if _, reached := server.Reach(seat); !reached {
				p.add(idx(at(path, "sandbox.mcp.servers"), j), ErrConflict,
					"%s, so its executor box is not given it: a box holds only a server the "+
						"seat is granted. %s", server.unreached(fmt.Sprintf("seat %q", seat.Handle())),
					server.grantRemedy("the seat"))
			}
		}
	}
	for unit, path := range c.EachUnit() {
		built := o.Unit(unit.IdentityKey())
		if built == nil {
			continue
		}
		var agents []*org.Role
		for _, r := range built.Roles {
			if r.IsAgent() {
				agents = append(agents, r)
			}
		}
		if len(agents) == 0 {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(unit.MCPEnv)) {
			server, declared := servers[name]
			if !declared || engineReadsMCPEnv(name) {
				continue
			}
			if slices.ContainsFunc(agents, func(r *org.Role) bool { _, ok := server.Reach(r); return ok }) {
				continue
			}
			p.add(entry(at(path, "mcp_env"), name), ErrConflict,
				"%s, so nothing reads this block: a unit's mcp_env block holds values for its "+
					"own agent seats a server already reaches, and grants nothing. %s",
				server.unreached(fmt.Sprintf("any agent seat of unit %q", built.Key())),
				server.grantRemedy("the unit"))
		}
	}
	return p.err()
}

// validateAccess holds one server's `access`, written at path, to names the
// chart o holds; see [Company.validateMCPAccess].
func (m *MCPServer) validateAccess(p *problems, path Path, o *org.Organization) {
	if lanes := m.grantLanes(); len(lanes) > 0 {
		p.add(path, ErrConflict,
			"this server reads each agent's own %s credential, so the agents it reaches are "+
				"the ones on at %s (roles[].integrations.<lane>.enabled), the one place that "+
				"grant is made: remove access, and turn agents on or off at the integration",
			words.And(kindsOf(m.SeatBindings())), words.Or(lanes))
		return
	}
	a := m.Access
	seat := func(list string, j int, handle string) {
		here := idx(at(path, list), j)
		r := o.Role(handle)
		switch {
		case strings.TrimSpace(handle) == "":
			p.add(here, ErrMissing, "an empty entry names no seat: give an agent seat's handle")
		case r == nil:
			p.add(here, ErrUnknownValue, "%q is no seat's handle in this company: give an "+
				"agent seat's handle, or remove the entry", handle)
		case !r.IsAgent():
			p.add(here, ErrConflict, "%q is a person's seat, and a person runs no tools: "+
				"a server is granted to agent seats", handle)
		}
	}
	once := func(list string, names []string) {
		seen := map[string]bool{}
		for j, name := range names {
			if seen[name] {
				p.add(idx(at(path, list), j), ErrConflict, "%q is listed twice", name)
			}
			seen[name] = true
		}
	}
	for j, key := range a.Units {
		here := idx(at(path, "units"), j)
		switch {
		case strings.TrimSpace(key) == "":
			p.add(here, ErrMissing, "an empty entry names no unit: give a unit's key")
		case o.Unit(key) == nil:
			p.add(here, ErrUnknownValue, "%q is no unit's key in this company: give a unit's "+
				"id, or remove the entry", key)
		}
	}
	for j, handle := range a.Seats {
		seat("seats", j, handle)
	}
	for j, handle := range a.Except {
		seat("except", j, handle)
		if slices.Contains(a.Seats, handle) {
			p.add(idx(at(path, "except"), j), ErrConflict, "%q is granted in access.seats "+
				"and left out here: keep it in one of the two", handle)
		}
	}
	once("units", a.Units)
	once("seats", a.Seats)
	once("except", a.Except)
}

// unreached says that the server does not reach who.
func (m *MCPServer) unreached(who string) string {
	if lanes := m.grantLanes(); len(lanes) > 0 {
		return fmt.Sprintf("%q reaches only the agent seats on at %s, and not %s",
			m.Name, words.Or(lanes), who)
	}
	return fmt.Sprintf("%q is not granted to %s", m.Name, who)
}

// grantRemedy is what to do about a field naming a server that does not reach
// whom (the seat, the unit).
func (m *MCPServer) grantRemedy(whom string) string {
	if lanes := m.grantLanes(); len(lanes) > 0 {
		return fmt.Sprintf("Turn %s on at %s, or remove the entry", whom, words.Or(lanes))
	}
	return fmt.Sprintf("Grant it to %s in the server's access, or remove the entry", whom)
}

// kindsOf is the integrations a server's bound keys belong to, each once, sorted.
func kindsOf(bound []BoundSeatKey) []string {
	var out []string
	for _, k := range bound {
		if !slices.Contains(out, k.Kind) {
			out = append(out, k.Kind)
		}
	}
	slices.Sort(out)
	return out
}

// accessAdvisories warns of a server whose `access` reaches no agent seat: no
// agent is offered its tools, a per-agent server starts nothing, and a shared
// one still starts its one instance.
func (c *Company) accessAdvisories() []Warning {
	o, _ := c.organization()
	var out []Warning
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if server.Access == nil || len(server.grantLanes()) > 0 {
			continue
		}
		reached := false
		for seat := range o.AllRoles() {
			if _, ok := server.Reach(seat); ok {
				reached = true
				break
			}
		}
		if reached {
			continue
		}
		running := "it starts nothing"
		if server.IsShared() {
			running = "its one instance still starts (turn it off with enabled: false to start nothing)"
		}
		out = append(out, advisory(at(idx(field("mcp_servers"), i), "access"), fmt.Sprintf(
			"%q is granted to no agent seat, so no agent is offered its tools and %s. Name the "+
				"units or seats that get it, or remove access to grant it to every agent",
			server.Name, running)))
	}
	return out
}
