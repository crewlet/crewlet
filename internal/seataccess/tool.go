package seataccess

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

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
// lane's credential, or where the lane's tool is a CLI in the seat's box whose
// credentials the engine sets there ([LaneSpec.BoxEnv], [BoxDefaults]),
// waiting until the credential is sealed; server_off where every such server
// is turned off; and otherwise builtin for Jira and Confluence, whose
// credential the engine's own tools read, and no_server for the rest. SSH is
// builtin whatever the servers: its tools are the engine's own `ssh_read` and
// `ssh_operate`, which issue the seat's certificate at use.
//
// A SERVER TURNED OFF READS NO CREDENTIAL ([config.MCPServer.IsEnabled]): it
// starts for nobody, so a lane whose only server is off is not live however
// well its credential is sealed. Jira and Confluence stay builtin then, since
// the engine's own tools read their credential with or without a server.
func ToolStateOf(c *config.Company, handle string, lane Lane, want Desired, have Observed) ToolState {
	if !want.Enabled || !have.Exists || !have.Enabled {
		return ToolOff
	}
	if lane == LaneSSH {
		return ToolBuiltin
	}
	var servers []*config.MCPServer
	if c != nil {
		servers = c.SeatServersFor(string(lane.Kind()))
	}
	if !slices.ContainsFunc(servers, (*config.MCPServer).IsEnabled) {
		switch {
		case lane == LaneJira || lane == LaneConfluence:
			return ToolBuiltin
		case len(boxCredentials(c, lane, handle)) > 0:
			// THE SEAT'S OWN BOX IS ITS TOOL: Expo's EAS CLI reads the
			// seat's token as EXPO_TOKEN, which the engine sets there.
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

// ToolOf is [ToolStateOf] with the grant the lane's last sync left settling
// ([LaneRecord.Settling]): a tool the seat could use reads waiting, for the
// settling's reason, while now is before its end, and as ever after it. A
// settling grant never makes the sync itself waiting, which is a person's step.
func ToolOf(c *config.Company, handle string, lane Lane, want Desired, have Observed,
	settling *Settling, now time.Time) (ToolState, string) {
	state := ToolStateOf(c, handle, lane, want, have)
	switch {
	case !settling.active(now):
		return state, ""
	case state == ToolLive, state == ToolBuiltin, state == ToolWaiting:
		return ToolWaiting, settling.Reason
	}
	return state, ""
}

// boxCredentials is each variable lane's tool reads in the box of the seat
// with handle, set to the seat's own sealed credential ([LaneSpec.BoxEnv]):
// for a seat on at the lane, where the engine seals the kind's credentials
// ([sealsSeats]). Nil otherwise, for a lane whose tool is a server, and for an
// unregistered kind.
func boxCredentials(c *config.Company, lane Lane, handle string) map[string]string {
	vendor, ok := Lookup(lane.Kind())
	if !ok {
		return nil
	}
	return laneCredentials(c, vendor.Catalog(), roleOf(c, handle), lane)
}

// laneCredentials is [boxCredentials] from cat, for role.
func laneCredentials(c *config.Company, cat Catalog, role *config.Role, lane Lane) map[string]string {
	spec, ok := cat.Lane(lane)
	if !ok || len(spec.BoxEnv) == 0 || role == nil || !sealsSeats(c, cat.Kind) {
		return nil
	}
	if d, _ := DesiredOf(role, lane); !d.Enabled {
		return nil
	}
	handle := strings.TrimSpace(role.IdentityKey())
	out := make(map[string]string, len(spec.BoxEnv))
	for _, b := range spec.BoxEnv {
		out[b.Env] = "${" + SinkName(cat.Kind, b.Field, handle) + "}"
	}
	return out
}

// sealsSeats reports whether the engine seals kind's agent credentials, so
// the name a box variable is set to holds the seat's own: where it provisions
// the kind ([config.Company.SeatCredentialMode]). Where they are handed in from
// outside Crewlet, the seat's own settings name the variable that holds one.
func sealsSeats(c *config.Company, kind integration.Kind) bool {
	// MANAGED OR NOT turns GitHub's mode alone, whose box credential is
	// issued at use ([BoxDefaulter]) and never sealed.
	return c.SeatCredentialMode(string(kind), false) == config.SeatCredentialEngine
}

// Credential reports whether f holds one of the seat's credentials, a whole
// reference, rather than literal text, which is a setting ([BoxSet]).
func (f BoxFile) Credential() bool {
	_, whole := envref.Whole(f.Ref)
	return whole
}

// BoxOf is the seat with handle's box as lane's vendor fills it while the seat
// has one of its lanes on ([BoxDefaults]): the credentials lane's tool reads
// there, then every other variable, the variables naming its files' paths
// ([BoxFile.PathEnv]) among them, each by name, and the files it writes, by
// path. Nil where there is none of them.
func BoxOf(c *config.Company, kind integration.Kind, lane Lane, handle string) *Box {
	vendor, ok := Lookup(kind)
	if !ok {
		return nil
	}
	return boxOf(c, vendor, lane, roleOf(c, handle))
}

// boxOf is [BoxOf] for vendor and role.
func boxOf(c *config.Company, vendor Vendor, lane Lane, role *config.Role) *Box {
	set := vendorDefaults(c, role, vendor)
	tool := laneCredentials(c, vendor.Catalog(), role, lane)
	var lines []BoxLine
	for _, name := range slices.Sorted(maps.Keys(tool)) {
		lines = append(lines, BoxLine{Name: name, Value: tool[name], Tool: true})
	}
	supplied := maps.Clone(set.Env)
	for name, value := range set.Credentials {
		if _, read := tool[name]; !read {
			if supplied == nil {
				supplied = map[string]string{}
			}
			supplied[name] = value
		}
	}
	var files []BoxFileLine
	for _, f := range set.Files {
		files = append(files, BoxFileLine{Path: f.Path, Value: f.Ref, Rewritten: f.Rewritten})
		if _, setting := set.Env[f.PathEnv]; f.PathEnv != "" && !setting {
			// THE PATH AS EVERY BOX READS IT, from its home: the launch joins
			// it onto the box's own, and a setting of the same name wins.
			if supplied == nil {
				supplied = map[string]string{}
			}
			supplied[f.PathEnv] = "~/" + f.Path
		}
	}
	for _, name := range slices.Sorted(maps.Keys(supplied)) {
		lines = append(lines, BoxLine{Name: name, Value: supplied[name]})
	}
	slices.SortFunc(files, func(a, b BoxFileLine) int { return strings.Compare(a.Path, b.Path) })
	if len(lines) == 0 && len(files) == 0 {
		return nil
	}
	if lines == nil {
		// ON THE WIRE AS A LIST, never null, for a box the engine fills
		// with files alone.
		lines = []BoxLine{}
	}
	return &Box{Env: lines, Files: files}
}

// BoxDefault is one thing a vendor puts in a seat's box ([BoxSet]) for the
// integration Kind: Env set to Value, a credential where Credential is set,
// or, where File is set, a file.
type BoxDefault struct {
	Kind       integration.Kind
	Env        string
	Value      string
	Credential bool

	// File is one of the vendor's [BoxSet.Files], with Env and Value empty.
	File *BoxFile
}

// BoxDefaults is everything the registered vendors put in the box of the
// seat with handle, each for a vendor with a lane the seat has on: the
// credentials by variable, then the settings by variable, then the files by
// path. Nil where there are none.
func BoxDefaults(c *config.Company, handle string) []BoxDefault {
	return boxDefaults(c, handle, Registered())
}

// boxDefaults is [BoxDefaults] over vendors.
func boxDefaults(c *config.Company, handle string, vendors []Vendor) []BoxDefault {
	role := roleOf(c, handle)
	var out []BoxDefault
	for _, v := range vendors {
		set := vendorDefaults(c, role, v)
		for env, value := range set.Credentials {
			out = append(out, BoxDefault{Kind: v.Kind(), Env: env, Value: value, Credential: true})
		}
		for env, value := range set.Env {
			out = append(out, BoxDefault{Kind: v.Kind(), Env: env, Value: value})
		}
		for _, f := range set.Files {
			out = append(out, BoxDefault{Kind: v.Kind(), File: &f})
		}
	}
	slices.SortFunc(out, func(a, b BoxDefault) int {
		ar, ak := a.order()
		br, bk := b.order()
		return cmp.Or(cmp.Compare(ar, br), strings.Compare(ak, bk), strings.Compare(string(a.Kind), string(b.Kind)))
	})
	return out
}

// order is where d sorts among a seat's defaults: the credentials first, by
// variable, then the settings, then the files, by path.
func (d BoxDefault) order() (int, string) {
	switch {
	case d.File != nil:
		return 2, d.File.Path
	case d.Credential:
		return 0, d.Env
	}
	return 1, d.Env
}

// vendorDefaults is what v supplies to role's box: nothing unless role has
// one of v's lanes on, and then each on lane's credentials ([LaneSpec.BoxEnv])
// beside what v supplies as a [BoxDefaulter].
func vendorDefaults(c *config.Company, role *config.Role, v Vendor) BoxSet {
	cat := v.Catalog()
	if role == nil || !laneOn(cat, role) {
		return BoxSet{}
	}
	var set BoxSet
	if defaulter, ok := v.(BoxDefaulter); ok {
		set = defaulter.BoxDefaults(c, role)
	}
	for _, spec := range cat.Lanes {
		for name, value := range laneCredentials(c, cat, role, spec.Lane) {
			if set.Credentials == nil {
				set.Credentials = map[string]string{}
			}
			set.Credentials[name] = value
		}
	}
	return set
}

// laneOn reports whether role has one of cat's lanes on. Any lane, since what
// a vendor's box CLI needs follows the seat at the vendor, whether the lane's
// tool is that CLI, a server beside it, or both.
func laneOn(cat Catalog, role *config.Role) bool {
	for _, spec := range cat.Lanes {
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
