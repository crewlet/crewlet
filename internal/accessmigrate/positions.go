package accessmigrate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Where the engine before per-seat access looked for a seat's credential at
// each vendor, FROZEN as it read them: the `mcp_env` servers and keys each
// pass scanned, in its own order, and the seat blocks' credential fields.

// envKey is one `mcp_env` key a pass read, and the credential field it held.
type envKey struct {
	key   string
	field string
}

// envPositions are the `mcp_env` servers each kind's pass scanned, and the keys
// in each.
var envPositions = map[string]struct {
	kind integration.Kind
	keys []envKey
}{
	"datadog": {integration.KindDatadog, []envKey{
		{"DD_APP_KEY", "APP_KEY"}, {"DATADOG_APP_KEY", "APP_KEY"}, {"app_key", "APP_KEY"},
		// THE KEY DATADOG'S OWN MCP SERVER READS, which a company added by
		// hand beside the pass's key to reach that server (design 12.4).
		{"DD_APPLICATION_KEY", "APP_KEY"},
	}},
	"gitlab": {integration.KindGitLab, []envKey{
		{"GITLAB_TOKEN", "TOKEN"}, {"GITLAB_PERSONAL_ACCESS_TOKEN", "TOKEN"},
		{"Private-Token", "TOKEN"}, {"Authorization", "TOKEN"},
	}},
	"atlassian":  {integration.KindAtlassian, atlassianKeys},
	"jira":       {integration.KindAtlassian, atlassianKeys},
	"confluence": {integration.KindAtlassian, atlassianKeys},
	// GITHUB'S TOOL WAS AUTHORIZED BY THE TOKEN THE ENGINE ISSUES AT USE, which
	// is never sealed: a position to drop once the engine binds it, and never
	// a value to move.
	"github": {integration.KindGitHub, []envKey{{"Authorization", "APP_TOKEN"}}},
}

// atlassianKeys are the token and address keys the Atlassian pass read.
var atlassianKeys = []envKey{
	{"JIRA_API_TOKEN", "TOKEN"}, {"CONFLUENCE_API_TOKEN", "TOKEN"},
	{"ATLASSIAN_API_TOKEN", "TOKEN"}, {"JIRA_PERSONAL_TOKEN", "TOKEN"},
	{"CONFLUENCE_PERSONAL_TOKEN", "TOKEN"}, {"JIRA_TOKEN", "TOKEN"},
	{"CONFLUENCE_TOKEN", "TOKEN"}, {"Authorization", "TOKEN"},
	{"JIRA_USERNAME", "EMAIL"}, {"JIRA_EMAIL", "EMAIL"}, {"ATLASSIAN_EMAIL", "EMAIL"},
	{"CONFLUENCE_USERNAME", "EMAIL"}, {"CONFLUENCE_EMAIL", "EMAIL"},
}

// managedServers are the `mcp_env` servers [envPositions] reads, in a stable
// order.
var managedServers = []string{"datadog", "gitlab", "atlassian", "jira", "confluence", "github"}

// blockPositions are the seat block fields that held a seat's credential.
var blockPositions = map[seataccess.Lane][]envKey{
	seataccess.LaneSlack:      {{"bot_token", "BOT_TOKEN"}, {"signing_secret", "SIGNING_SECRET"}},
	seataccess.LaneMattermost: {{"bot_token", "BOT_TOKEN"}},
	seataccess.LaneGitHub: {{"private_key", "APP_KEY"},
		{"webhook_secret", "APP_WEBHOOK_SECRET"}},
}

// primaryField is the field whose presence put a seat in a kind's plan.
var primaryField = map[integration.Kind]string{
	integration.KindDatadog: "APP_KEY", integration.KindGitLab: "TOKEN",
	integration.KindAtlassian: "TOKEN", integration.KindMattermost: "BOT_TOKEN",
	integration.KindSlack: "BOT_TOKEN", integration.KindGitHub: "APP_KEY",
}

// desiredKeys are the block keys a lane's [seataccess.Desired] is written in,
// which the migration replaces and nothing else.
var desiredKeys = map[seataccess.Lane][]string{
	seataccess.LaneDatadog:    {"enabled", "role"},
	seataccess.LaneGitLab:     {"enabled", "access_level", "member_role_id", "projects"},
	seataccess.LaneJira:       {"enabled", "project_role", "projects"},
	seataccess.LaneConfluence: {"enabled", "space_role", "spaces"},
	seataccess.LaneMattermost: {"enabled", "roles", "channels"},
	seataccess.LaneGitHub:     {"enabled", "permissions", "repos"},
	seataccess.LaneSlack:      {"enabled"},
}

// position is one place a seat named its credential.
type position struct {
	handle string
	kind   integration.Kind
	field  string
	// from is the variable named there.
	from string

	// server and key are an `mcp_env` position; lane and key a seat block's.
	server string
	lane   seataccess.Lane
	key    string

	// inUnit is an `mcp_env` position the seat inherits from its unit's
	// shared block rather than names itself ([config.Unit.MCPEnv]).
	inUnit bool
}

// seatPositions are every place one seat named a credential for a kind
// connected reports, each a whole `${VAR}`: a literal is a credential somebody
// manages by hand, which no pass ever wrote and this does not move.
//
// THE `mcp_env` THE PASSES READ WAS THE SEAT'S OWN LAID OVER ITS UNIT'S, key
// by key ([org.MCPEnv.WithDefaults]): unit is the seat's own unit, nil for a
// seat at the root, and a key the seat inherits from it is a position too,
// marked as the unit's.
func seatPositions(seat, unit map[string]any, handle string,
	connected func(integration.Kind) bool) []position {

	var out []position
	env, shared := object(seat["mcp_env"]), object(unit["mcp_env"])
	for _, server := range managedServers {
		spec := envPositions[server]
		if !connected(spec.kind) {
			continue
		}
		own, inherited := object(env[server]), object(shared[server])
		for _, k := range spec.keys {
			value, inUnit := own[k.key], false
			if !has(own, k.key) {
				value, inUnit = inherited[k.key], true
			}
			name, whole := envref.Whole(stripBearer(text(value)))
			if !whole {
				continue
			}
			out = append(out, position{handle: handle, kind: spec.kind, field: k.field,
				from: name, server: server, key: k.key, inUnit: inUnit})
		}
	}
	for _, lane := range []seataccess.Lane{seataccess.LaneSlack, seataccess.LaneMattermost,
		seataccess.LaneGitHub} {
		if !connected(lane.Kind()) {
			continue
		}
		block := object(object(seat["integrations"])[string(lane)])
		for _, spec := range blockPositions[lane] {
			if name, ok := envref.Whole(text(block[spec.key])); ok {
				out = append(out, position{handle: handle, kind: lane.Kind(),
					field: spec.field, from: name, lane: lane, key: spec.key})
			}
		}
	}
	return out
}

// stripBearer drops the scheme an Authorization header carries, as the passes
// did before reading the reference under it.
func stripBearer(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "Bearer "))
}

// HandedIn is the variable a seat names for its credential at kind where the
// credential was handed in from outside Crewlet: the first position the old
// passes read, as the seat holds it now. Empty where the seat names none. It
// is what the Agents tab shows as an external lane's credential.
func HandedIn(role *config.Role, kind integration.Kind) string {
	raw, err := json.Marshal(role)
	if err != nil {
		return ""
	}
	var seat map[string]any
	if json.Unmarshal(raw, &seat) != nil {
		return ""
	}
	handle := role.Seat().Handle()
	for _, p := range seatPositions(seat, nil, handle, func(k integration.Kind) bool {
		return k == kind
	}) {
		if p.field == primaryField[kind] || primaryField[kind] == "" {
			return p.from
		}
	}
	return ""
}

// apply rewrites one position in its seat, or its unit, and reports whether
// it changed it.
//
// AN `mcp_env` KEY IS DROPPED where the engine now binds the seat's own
// credential to that server itself (D15), and pointed at the engine's name for
// it where it does not, so the tool keeps reading the seat's own credential
// either way. A SEAT BLOCK FIELD is pointed at the engine's name. Neither ever
// keeps naming a variable a company block shares.
//
// A KEY THE SEAT INHERITS FROM ITS UNIT is dropped from the unit's block where
// the engine binds the server, since it binds each member's own credential;
// where it does not, the seat names its own credential over it, which wins key
// by key, and the unit's block is left to its other members.
func (p position) apply(seat, unit map[string]any) bool {
	to := seataccess.SinkName(p.kind, p.field, p.handle)
	pointer := "${" + to + "}"
	if p.inUnit {
		shared := object(unit["mcp_env"])
		block := object(shared[p.server])
		if bound(p.kind, p.server) {
			if !has(block, p.key) {
				return false
			}
			dropEnvKey(unit, p.server, p.key)
			return true
		}
		if strings.HasPrefix(strings.TrimSpace(text(block[p.key])), "Bearer ") {
			pointer = "Bearer " + pointer
		}
		env := object(seat["mcp_env"])
		if env == nil {
			env = map[string]any{}
			seat["mcp_env"] = env
		}
		own := object(env[p.server])
		if own == nil {
			own = map[string]any{}
			env[p.server] = own
		}
		if text(own[p.key]) == pointer {
			return false
		}
		own[p.key] = pointer
		return true
	}
	if p.server != "" {
		env := object(seat["mcp_env"])
		block := object(env[p.server])
		if !has(block, p.key) {
			return false
		}
		if bound(p.kind, p.server) {
			dropEnvKey(seat, p.server, p.key)
			return true
		}
		if strings.HasPrefix(strings.TrimSpace(text(block[p.key])), "Bearer ") {
			pointer = "Bearer " + pointer
		}
		if text(block[p.key]) == pointer {
			return false
		}
		block[p.key] = pointer
		return true
	}
	block := object(object(seat["integrations"])[string(p.lane)])
	if text(block[p.key]) == pointer {
		return false
	}
	block[p.key] = pointer
	return true
}

// dropEnvKey removes one key of one server's `mcp_env` block from holder (a
// seat or a unit), and the block and the env once they are empty.
func dropEnvKey(holder map[string]any, server, key string) {
	env := object(holder["mcp_env"])
	block := object(env[server])
	delete(block, key)
	if len(block) == 0 {
		delete(env, server)
	}
	if len(env) == 0 {
		delete(holder, "mcp_env")
	}
}

// bound reports whether the engine binds kind's credential to server itself
// ([seataccess.ToolFor]), which makes every credential key the seat wrote for
// that server redundant: the engine injects the seat's own value under the key
// the server reads.
func bound(kind integration.Kind, server string) bool {
	for _, lane := range seataccess.LanesOf(kind) {
		if b := seataccess.ToolFor(lane); b != nil && b.Server == server {
			return true
		}
	}
	return false
}

// decodeSeat reads a stored seat into this build's type.
func decodeSeat(seat map[string]any) (*config.Role, error) {
	raw, err := json.Marshal(seat)
	if err != nil {
		return nil, fmt.Errorf("accessmigrate: encode a seat: %w", err)
	}
	var role config.Role
	if err := json.Unmarshal(raw, &role); err != nil {
		return nil, fmt.Errorf("accessmigrate: decode a seat: %w", err)
	}
	return &role, nil
}

// writeSeatIntegrations writes each lane's block of role, as [seataccess.SetDesired]
// left it, into the stored seat: the keys a [seataccess.Desired] is written in
// are replaced, and every other key of the stored block stays as stored.
func writeSeatIntegrations(seat map[string]any, role *config.Role,
	lanes map[seataccess.Lane]seataccess.Desired) error {

	raw, err := json.Marshal(role.Integrations)
	if err != nil {
		return fmt.Errorf("accessmigrate: encode a seat's integrations: %w", err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return fmt.Errorf("accessmigrate: decode a seat's integrations: %w", err)
	}
	stored := object(seat["integrations"])
	if stored == nil {
		stored = map[string]any{}
		seat["integrations"] = stored
	}
	for lane := range lanes {
		block := object(stored[string(lane)])
		if block == nil {
			block = map[string]any{}
			stored[string(lane)] = block
		}
		fresh := object(encoded[string(lane)])
		for _, key := range desiredKeys[lane] {
			delete(block, key)
			if v, ok := fresh[key]; ok {
				block[key] = v
			}
		}
	}
	return nil
}
