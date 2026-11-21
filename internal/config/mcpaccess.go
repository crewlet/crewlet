package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// WHICH AGENTS AN MCP SERVER REACHES, decided by one grant on its entry:
// `access` grants a server to agent seats and nothing else does. Absent, every
// agent seat; written, exactly the seats it names, each unit standing for every
// agent seat beneath it, minus `except`. A shared server's tools reach only
// those seats; a per-agent server starts a child for them and nobody else.
//
// A seat's `mcp_env` block is VALUES ONLY and grants nothing, so one for a
// server that does not reach the seat is refused ([Company.validateMCPAccess]).
//
// A SERVER BOUND TO A VENDOR'S PER-SEAT CREDENTIAL reaches the seats on at that
// integration (`roles[].integrations.<lane>.enabled`) and takes no `access`:
// two switches for one grant would disagree. See adr/0076-an-mcp-server-reaches-an-agent-through-one-grant-on-its-entry.md.

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
// NOT THE SERVER IS ON ([MCPServer.Grants] adds that): a turned-off server
// keeps its grant, so stored values, tests of a seat's child and the answer
// listing who a server reaches read the grant itself.
//
// A human seat is reached by nothing: it runs no tools. The seat is the one the
// engine runs ([org.Organization] as built), which carries where it sits
// ([org.Role.UnitKeys]) and its lanes ([org.Role.AccessLanes]).
//
// A seat both named and left out is reached: `seats` is the more particular.
// Admission refuses that pair, so only a stored revision holds it.
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
// that hands a seat a server's values or tools to a seat the server reaches.
//
//   - `access` on a server bound to a vendor's per-seat credential is refused.
//   - Each `access` name is a unit's key or an AGENT seat's handle, once, and no
//     seat is both named and left out. Removing a seat or unit a grant still
//     names is refused, so a later one taking its handle is never granted by it.
//   - A seat's or unit's `mcp_env` block and an executor box's listing are
//     refused where the server does not reach the seat: each reads as a grant.
//
// It reads the grant ([MCPServer.Reach]) whether or not the server is on; a block
// the engine reads by its own name is exempt. ADMISSION rule: breaking it warns.
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
		w := grantedToNobodyAdvice.raise(at(idx(field("mcp_servers"), i), "access"), fmt.Sprintf(
			"%q is granted to no agent seat, so no agent is offered its tools and %s. Name the "+
				"units or seats that get it, or remove access to grant it to every agent",
			server.Name, running))
		w.Item = server.Name
		out = append(out, w)
	}
	return out
}

// grantedToNobodyAdvice is [Company.accessAdvisories]' rule; the warning's
// Item is the server.
var grantedToNobodyAdvice = says("MCP servers no agent may use",
	"No agent may use this MCP server, so what it offers goes unused. Choose who gets it, or "+
		"open it to every agent.")
