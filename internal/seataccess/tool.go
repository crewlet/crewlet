package seataccess

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// ToolBinding is a recognised server's binding for one integration (D15). The
// recognition is the MCP catalogue's ([catalogue.Bindings]), which the org
// model binds a seat's credential by, so what a lane reports and what a
// seat's server is started with cannot disagree.
type ToolBinding = catalogue.Binding

// ToolFor is the conventional server this build recognises for lane's
// integration, or nil where it recognises none. A company binds any other
// server by declaring its `seat_credentials`.
func ToolFor(lane Lane) *ToolBinding {
	b, ok := config.SeatToolBindingFor(string(lane))
	if !ok {
		return nil
	}
	return &b
}

// ToolServerOf is the mcp_servers entry lane's per-seat tool server is started
// from: the company's own where it declares one reading the lane's credential,
// an enabled one first, else the name the catalogue's server for the lane is
// conventionally declared under; "" where there is neither.
func ToolServerOf(c *config.Company, lane Lane) string {
	if c != nil {
		servers := c.SeatServersFor(string(lane.Kind()))
		if i := slices.IndexFunc(servers, (*config.MCPServer).IsEnabled); i >= 0 {
			return servers[i].Name
		}
		if len(servers) > 0 {
			return servers[0].Name
		}
	}
	if b := ToolFor(lane); b != nil {
		return b.Server
	}
	return ""
}

// ToolStateOf is whether the seat with handle can use its credential through
// a tool on lane right now, as the access route reports it: off while the seat
// or its account is off; live or waiting where a per-seat server reads the
// lane's credential, or the seat's box sets every one of the lane's
// [LaneSpec.BoxEnv] variables to exactly its sealed name (waiting until the
// credential is sealed);
// server_off where every such server is turned off; and otherwise builtin for
// Jira and Confluence, whose credential the engine's own tools read, and
// no_server for the rest, a box CLI whose variable is unset or points
// elsewhere included.
//
// A SERVER TURNED OFF READS NO CREDENTIAL ([config.MCPServer.IsEnabled]): it
// starts for nobody, so a lane whose only server is off is not live however
// well its credential is sealed. Jira and Confluence stay builtin then, since
// the engine's own tools read their credential with or without a server.
func ToolStateOf(c *config.Company, handle string, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	var servers []*config.MCPServer
	if c != nil {
		servers = c.SeatServersFor(string(lane.Kind()))
	}
	if !slices.ContainsFunc(servers, (*config.MCPServer).IsEnabled) {
		switch {
		case lane == LaneJira || lane == LaneConfluence:
			return ToolBuiltin
		case boxReads(c, handle, lane):
			// THE SEAT'S OWN BOX IS ITS TOOL: Expo's EAS CLI reads the
			// seat's token as EXPO_TOKEN.
		case len(servers) > 0:
			return ToolServerOff
		default:
			return ToolNoServer
		}
	}
	if have.Credential == nil && lane != LaneGitHub {
		// GITHUB'S TOKEN IS ISSUED AT USE from the app's sealed key, so an
		// installed app is a live tool; every other credential is the
		// sealed value itself.
		return ToolWaiting
	}
	return ToolLive
}

// boxReads reports whether the seat with handle sets every one of lane's
// [LaneSpec.BoxEnv] variables in its own box to exactly `${<its sealed
// credential>}`.
func boxReads(c *config.Company, handle string, lane Lane) bool {
	return boxSets(roleOf(c, handle), BoxLines(lane.Kind(), lane, handle))
}

// boxSets reports whether role's own box settings set every one of lines to
// exactly its value, as a whole reference. False for no lines.
func boxSets(role *config.Role, lines []BoxLine) bool {
	if role == nil || len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		value, set := role.Sandbox.EnvValue(line.Name)
		name, whole := envref.Whole(value)
		if !set || !whole || "${"+name+"}" != line.Value {
			return false
		}
	}
	return true
}

// BoxLines is every line the seat with handle's sandbox env needs for its box
// CLI to read its credentials on lane, in the catalog's order: each variable
// set to its sealed name as a whole reference, by [ByYou]. Nil for a lane
// whose tool is a server, and for an unregistered kind.
func BoxLines(kind integration.Kind, lane Lane, handle string) []BoxLine {
	vendor, ok := Lookup(kind)
	if !ok {
		return nil
	}
	return boxLinesOf(vendor.Catalog(), lane, handle)
}

// boxLinesOf is [BoxLines] from cat.
func boxLinesOf(cat Catalog, lane Lane, handle string) []BoxLine {
	spec, ok := cat.Lane(lane)
	if !ok || len(spec.BoxEnv) == 0 || strings.TrimSpace(handle) == "" {
		return nil
	}
	out := make([]BoxLine, 0, len(spec.BoxEnv))
	for _, b := range spec.BoxEnv {
		out = append(out, BoxLine{Name: b.Env, Value: "${" + SinkName(cat.Kind, b.Field, handle) + "}", By: ByYou})
	}
	return out
}

// BoxOf is the seat with handle's box as lane needs it: the lines a person
// adds ([BoxLines]), then the settings the engine supplies while the seat's
// lane is on, sorted. Nil where there is neither.
func BoxOf(c *config.Company, kind integration.Kind, lane Lane, handle string) *Box {
	vendor, ok := Lookup(kind)
	if !ok {
		return nil
	}
	return boxOf(c, vendor, lane, handle)
}

// boxOf is [BoxOf] for vendor.
func boxOf(c *config.Company, vendor Vendor, lane Lane, handle string) *Box {
	lines := boxLinesOf(vendor.Catalog(), lane, handle)
	env := vendorDefaults(c, roleOf(c, handle), vendor)
	for _, name := range slices.Sorted(maps.Keys(env)) {
		lines = append(lines, BoxLine{Name: name, Value: env[name], By: ByCrewlet})
	}
	if len(lines) == 0 {
		return nil
	}
	return &Box{Env: lines}
}

// BoxDefault is one non-secret setting a vendor puts in a seat's box
// ([BoxDefaulter]): Env set to Value, for the integration Kind.
type BoxDefault struct {
	Kind  integration.Kind
	Env   string
	Value string
}

// BoxDefaults is every setting the registered vendors put in the box of the
// seat with handle, each for a box-CLI lane the seat has on, sorted by
// variable. Nil where there are none.
func BoxDefaults(c *config.Company, handle string) []BoxDefault {
	return boxDefaults(c, handle, Registered())
}

// boxDefaults is [BoxDefaults] over vendors.
func boxDefaults(c *config.Company, handle string, vendors []Vendor) []BoxDefault {
	role := roleOf(c, handle)
	var out []BoxDefault
	for _, v := range vendors {
		for env, value := range vendorDefaults(c, role, v) {
			out = append(out, BoxDefault{Kind: v.Kind(), Env: env, Value: value})
		}
	}
	slices.SortFunc(out, func(a, b BoxDefault) int {
		return cmp.Or(strings.Compare(a.Env, b.Env), strings.Compare(string(a.Kind), string(b.Kind)))
	})
	return out
}

// vendorDefaults is what v supplies to role's box: nil unless v is a
// [BoxDefaulter] and role has one of its box-CLI lanes on.
func vendorDefaults(c *config.Company, role *config.Role, v Vendor) map[string]string {
	defaulter, ok := v.(BoxDefaulter)
	if role == nil || !ok || !boxLaneOn(v.Catalog(), role) {
		return nil
	}
	return defaulter.BoxDefaults(c, role).Env
}

// boxLaneOn reports whether role has on a lane of cat whose tool is a box CLI.
func boxLaneOn(cat Catalog, role *config.Role) bool {
	for _, spec := range cat.Lanes {
		if len(spec.BoxEnv) == 0 {
			continue
		}
		if d, _ := DesiredOf(role, spec.Lane); d.Enabled {
			return true
		}
	}
	return false
}

// roleOf is the agent seat with handle in c, or nil.
func roleOf(c *config.Company, handle string) *config.Role {
	if c == nil || strings.TrimSpace(handle) == "" {
		return nil
	}
	for role := range c.EachRole() {
		if strings.TrimSpace(role.IdentityKey()) == handle {
			return role
		}
	}
	return nil
}
