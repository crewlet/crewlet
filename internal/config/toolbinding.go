package config

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// THE SEAT'S OWN CREDENTIAL, BOUND TO ITS TOOL BY THE ENGINE (D15).
//
// A seat that is on at an integration gets its own sealed credential added to
// its environment for every per-seat server that reads that integration's
// credential, under the key that server reads. Nothing in the document names
// the credential, so no writer can point a seat's tool at a variable anything
// else reads (D4), and the tool works the moment the credential is sealed.
//
// # Which key a server reads is the server's, so the server says it
//
// No fixed key per integration fits every server a company may run: `glab mcp
// serve` reads GITLAB_TOKEN where another GitLab server reads
// GITLAB_PERSONAL_ACCESS_TOKEN, and Slack's community server reads
// SLACK_MCP_XOXB_TOKEN. So an `mcp_servers` entry declares what it reads
// ([MCPServer.SeatCredentials]), and the MCP catalogue supplies the
// declaration for the servers this build recognises by their command or their
// address ([catalogue.SeatKeysFor]): ONE recognition, which also names the
// entry and the vendor mark a server is drawn with. A per-seat server named after an integration with
// neither is reported ([Company.UnboundSeatServers]): it would start, and act
// as nobody.
//
// BOUND WHERE THE ORG MODEL IS BUILT ([Company.organization]), the one place
// every reader of a seat's tool environment takes it from: the engine starting
// a seat's server, an executor's box, [MCPServer.Grants] and the org
// projection.

// SeatCredential is one seat credential a per-seat server reads, as an
// `mcp_servers` entry declares it.
type SeatCredential struct {
	// Kind is the integration whose seat credential it is.
	Kind string `yaml:"kind" json:"kind" js:"required;enum=atlassian|datadog|github|gitlab|mattermost|slack" desc:"Integration whose per-seat credential this server reads."`

	// Field is which of the integration's seat credentials: TOKEN or EMAIL
	// for Atlassian; empty is the integration's credential itself.
	Field string `yaml:"field,omitempty" json:"field,omitempty" desc:"Which seat credential (Atlassian: TOKEN or EMAIL). Empty means the integration's credential."`

	// Key is the variable (stdio) or header (http) the server reads it under.
	Key string `yaml:"key" json:"key" js:"required" desc:"Environment variable (stdio) or header (http) the server reads it under."`

	// As says which, and must agree with the transport; empty takes the
	// transport's.
	As string `yaml:"as,omitempty" json:"as,omitempty" js:"enum=env|header" desc:"env (stdio) or header (http); defaults to the transport's."`

	// Prefix precedes the value, as `Bearer ` does in an Authorization
	// header.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty" desc:"Text before the value, e.g. \"Bearer \"."`
}

// seatCredentialFields is each integration's per-seat fields a seat's tool
// server may read, the credential itself first ([seatFields]). GitHub's is the
// installation token the engine issues at use, never sealed.
func seatCredentialFields(kind string) []string {
	var out []string
	for _, f := range seatFields(kind) {
		if f.tool {
			out = append(out, f.name)
		}
	}
	return out
}

// SeatCredentialKinds is every integration a server can read a seat
// credential of.
func SeatCredentialKinds() []string {
	return []string{"atlassian", "datadog", "github", "gitlab", "mattermost", "slack"}
}

// SeatCredentialKind is the integration a seat block's lane belongs to:
// Atlassian for Jira and Confluence, the lane itself otherwise.
func SeatCredentialKind(lane string) string {
	if lane == "jira" || lane == "confluence" {
		return "atlassian"
	}
	return lane
}

// SeatToolBindingFor is the conventional recognised server for lane's
// integration, from the catalogue's recognition table, or false where this
// build recognises none. A company binds any other server by declaring its
// `seat_credentials`.
func SeatToolBindingFor(lane string) (catalogue.Binding, bool) {
	return catalogue.BindingFor(SeatCredentialKind(lane))
}

// Launch is the server as recognition reads it: its transport and its address
// or its command, never a credential.
func (m *MCPServer) Launch() catalogue.Launch {
	return catalogue.Launch{Transport: catalogue.Transport(m.Kind()), URL: m.URL, Command: m.Command,
		Args: slices.Clone(m.Args)}
}

// BoundSeatKey is one seat credential a server is started with: the
// integration and field it is, and the key and prefix the server reads it
// under on its own transport.
type BoundSeatKey struct {
	Kind, Field, Key, Prefix string
}

// SeatBindings is every seat credential this per-seat server reads: what it
// declares, or what this build recognises it as reading. Empty for a shared
// server, which acts as no seat.
func (m *MCPServer) SeatBindings() []BoundSeatKey {
	if m.IsShared() {
		return nil
	}
	var out []BoundSeatKey
	if len(m.SeatCredentials) > 0 {
		for _, d := range m.SeatCredentials {
			field := d.Field
			if field == "" {
				if fields := seatCredentialFields(d.Kind); len(fields) > 0 {
					field = fields[0]
				}
			}
			out = append(out, BoundSeatKey{Kind: d.Kind, Field: field, Key: d.Key, Prefix: d.Prefix})
		}
		return out
	}
	kind, keys, ok := catalogue.SeatKeysFor(m.Launch())
	if !ok {
		return nil
	}
	for _, k := range keys {
		key, prefix := k.Env, ""
		if m.Kind() == TransportHTTP {
			key, prefix = k.Header, k.Prefix
		}
		if key != "" {
			out = append(out, BoundSeatKey{Kind: kind, Field: k.Field, Key: key, Prefix: prefix})
		}
	}
	return out
}

// validateSeatCredentials checks a server's declared seat credentials.
func (m *MCPServer) validateSeatCredentials(p *problems, base Path) {
	if len(m.SeatCredentials) == 0 {
		return
	}
	if m.IsShared() {
		p.add(base, ErrConflict,
			"a shared server acts as no seat, so it reads no seat's credential: "+
				"set shared: false, or remove seat_credentials")
		return
	}
	want := "env"
	if m.Kind() == TransportHTTP {
		want = "header"
	}
	for i, d := range m.SeatCredentials {
		here := idx(base, i)
		fields := seatCredentialFields(d.Kind)
		switch {
		case fields == nil:
			p.add(at(here, "kind"), ErrUnknownValue, "%q is not an integration with a per-seat "+
				"credential; give one of %s", d.Kind, words.Or(SeatCredentialKinds()))
		case d.Field != "" && !slices.Contains(fields, d.Field):
			p.add(at(here, "field"), ErrUnknownValue, "%q is not one of %s's seat credentials; "+
				"give one of %s, or leave it out", d.Field, d.Kind, words.Or(fields))
		}
		if strings.TrimSpace(d.Key) == "" {
			p.add(at(here, "key"), ErrMissing, "required: the variable or header the server reads it under")
		}
		if d.As != "" && d.As != want {
			p.add(at(here, "as"), ErrConflict, "this server's transport reads a %s, not a %s", want, d.As)
		}
	}
}

// SeatAccessEnabled reports whether the seat is on at lane: its block is
// there and says `enabled: true`.
func (r *Role) SeatAccessEnabled(lane string) bool {
	on, _ := r.seatEnabled(lane)
	return SeatAccessOn(on)
}

// seatOnAt reports whether the seat is on at any lane of an integration.
func (r *Role) seatOnAt(kind string) bool {
	for _, lane := range SeatAccessLanes() {
		if SeatCredentialKind(lane) == kind && r.SeatAccessEnabled(lane) {
			return true
		}
	}
	return false
}

// SeatServersFor is every per-seat server that reads kind's seat credential.
func (c *Company) SeatServersFor(kind string) []*MCPServer {
	var out []*MCPServer
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if slices.ContainsFunc(server.SeatBindings(), func(k BoundSeatKey) bool { return k.Kind == kind }) {
			out = append(out, server)
		}
	}
	return out
}

// UnboundSeatServers is every per-seat server named after kind or one of its
// lanes that reads no seat credential: it neither declares seat_credentials
// nor is a server this build recognises. It would start and act as nobody.
func (c *Company) UnboundSeatServers(kind string) []string {
	var out []string
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if server.IsShared() || SeatCredentialKind(server.Name) != kind {
			continue
		}
		if len(server.SeatBindings()) == 0 {
			out = append(out, server.Name)
		}
	}
	return out
}

// boundRef is the value a seat's server is handed for one key it reads, or
// false to leave whatever the seat wrote for that key (D4b).
//
//   - ENGINE-HELD (the engine provisions the integration): the name the
//     engine seals the seat's credential under.
//   - GITHUB in either mode: the installation token the engine issues at use
//     from the seat's own app key, which answers that name for that seat.
//   - EXTERNAL (handed in from outside Crewlet): the seat's own position for
//     it where the integration has one on the seat's block (a Slack or
//     Mattermost bot token), and otherwise the value the seat already names
//     for the server, which the engine does not touch.
func (c *Company) boundRef(in *RoleIntegrations, handle string, k BoundSeatKey) (string, bool) {
	// NOT MANAGED: whether only an operator writes the document changes the
	// mode for GitHub alone, which is bound the same way in either.
	if k.Kind == "github" || c.SeatCredentialMode(k.Kind, false) == SeatCredentialEngine {
		return "${" + envref.SealedName(k.Kind, k.Field, handle) + "}", true
	}
	var external string
	switch k.Kind {
	case "slack":
		if in.Slack != nil {
			external = in.Slack.BotToken
		}
	case "mattermost":
		if in.Mattermost != nil {
			external = in.Mattermost.BotToken
		}
	}
	external = strings.TrimSpace(external)
	if _, whole := envref.Whole(external); whole {
		return external, true
	}
	return "", false
}

// bindSeatTools adds each agent seat's own credential to its environment for
// every per-seat server that reads an integration it is on at. It runs after
// normalization, so the engine's binding wins over a unit's value for the same
// key, as a seat's own value always does.
func (c *Company) bindSeatTools(o *org.Organization) {
	seats := map[string]*org.Role{}
	for seat := range o.AllRoles() {
		if seat.IsAgent() {
			seats[seat.Handle()] = seat
		}
	}
	for role := range c.EachRole() {
		// BY THE DERIVED HANDLE, which is the org model's key: a seat that
		// declares none takes its handle from its name.
		seat := seats[role.IdentityKey()]
		if seat == nil {
			continue
		}
		for i := range c.MCPServers {
			server := &c.MCPServers[i]
			// key to value: what the server reads, and what it is handed.
			bound := map[string]string{}
			for _, k := range server.SeatBindings() {
				if !role.seatOnAt(k.Kind) {
					continue
				}
				if ref, ok := c.boundRef(&role.Integrations, seat.Handle(), k); ok {
					bound[k.Key] = k.Prefix + ref
				}
			}
			if len(bound) == 0 {
				continue
			}
			env := seat.MCPEnv.Clone()
			if env == nil {
				env = org.MCPEnv{}
			}
			block := maps.Clone(env[server.Name])
			if block == nil {
				block = map[string]string{}
			}
			for key, value := range bound {
				// ONE KEY, whatever case a unit wrote it in: an http
				// header is one header in any case ([MCPServer.ValueKey]).
				for held := range block {
					if server.ValueKey(held) == server.ValueKey(key) {
						delete(block, held)
					}
				}
				block[key] = value
			}
			env[server.Name] = block
			seat.MCPEnv = env
		}
	}
}
