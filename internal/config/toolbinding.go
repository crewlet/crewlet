package config

import (
	"maps"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// THE SEAT'S OWN CREDENTIAL, BOUND TO ITS TOOL BY THE ENGINE (D15).
//
// A seat that is on at an integration with a tool server gets its own sealed
// credential added to its environment for that server, under the key the
// server reads: Datadog's remote server reads `DD_APPLICATION_KEY`, the GitLab
// servers `GITLAB_PERSONAL_ACCESS_TOKEN` or GitLab's `Private-Token` header, and
// so on. Nothing in the document names the credential, so no writer can point
// a seat's tool at a variable anything else reads (D4), and the seat's tool
// works the moment its credential is sealed rather than when somebody
// remembers to declare it.
//
// BOUND WHERE THE ORG MODEL IS BUILT ([Company.organization]), which is the
// one place every reader of a seat's tool environment takes it from: the
// engine starting a seat's server, a coding run's box, [MCPServer.Grants] and
// the org projection. A binding written anywhere later would leave one of them
// reading the seat without it.
//
// THE TABLE IS HERE, in the leaf, because the org model is built here and the
// vendor packages that own each integration sit above this one; they read it
// back through internal/seataccess rather than restating it.

// SeatToolKey is one credential field and the key a tool server reads it
// under.
type SeatToolKey struct {
	// Field is the credential's field, which with the vendor and the seat's
	// handle names the sealed value ([envref.SealedName]).
	Field string

	// Env is the variable a stdio server reads; Header the header an http
	// server reads, after Prefix.
	Env    string
	Header string
	Prefix string
}

// SeatToolBinding binds one integration's per-seat credential to its tool
// server.
type SeatToolBinding struct {
	// Lane is the seat block that turns the binding on
	// (`roles[].integrations.<lane>.enabled`).
	Lane string

	// Vendor is the integration the credential is sealed under.
	Vendor string

	// Server is the `mcp_servers` entry, by name, the credential is bound
	// to. It must be per-seat (`shared: false`): a shared server acts as no
	// seat.
	Server string

	Keys []SeatToolKey
}

// SeatToolBindings is every binding this build makes. Jira, Confluence and
// Mattermost are absent: the engine's own tools and transport read their
// credentials directly, with no server to start.
func SeatToolBindings() []SeatToolBinding {
	return []SeatToolBinding{
		{Lane: "slack", Vendor: "slack", Server: "slack", Keys: []SeatToolKey{
			{Field: "BOT_TOKEN", Env: "SLACK_BOT_TOKEN", Header: "Authorization", Prefix: "Bearer "},
		}},
		{Lane: "github", Vendor: "github", Server: "github", Keys: []SeatToolKey{
			// The installation token the engine issues at use, never
			// sealed (internal/github's Issuer).
			{Field: "APP_TOKEN", Env: "GITHUB_PERSONAL_ACCESS_TOKEN", Header: "Authorization", Prefix: "Bearer "},
		}},
		{Lane: "gitlab", Vendor: "gitlab", Server: "gitlab", Keys: []SeatToolKey{
			{Field: "TOKEN", Env: "GITLAB_PERSONAL_ACCESS_TOKEN", Header: "Private-Token"},
		}},
		{Lane: "datadog", Vendor: "datadog", Server: "datadog", Keys: []SeatToolKey{
			// The organization's API key is the server's own header,
			// the same for every seat; this is the seat's application key.
			{Field: "APP_KEY", Env: "DD_APPLICATION_KEY", Header: "DD_APPLICATION_KEY"},
		}},
	}
}

// SeatToolBindingFor is lane's binding, or false where the lane has none.
func SeatToolBindingFor(lane string) (SeatToolBinding, bool) {
	for _, b := range SeatToolBindings() {
		if b.Lane == lane {
			return b, true
		}
	}
	return SeatToolBinding{}, false
}

// SeatAccessEnabled reports whether the seat is on at lane: its block is
// there and says `enabled: true`.
func (r *Role) SeatAccessEnabled(lane string) bool {
	on, _ := r.seatEnabled(lane)
	return SeatAccessOn(on)
}

// PerSeatServer is the company's per-seat server named name, or nil where it
// declares none or declares it shared.
func (c *Company) PerSeatServer(name string) *MCPServer {
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if server.Name == name && !server.IsShared() {
			return server
		}
	}
	return nil
}

// bindSeatTools adds each agent seat's own credential to its environment for
// every per-seat server an integration it is on binds to. It runs after
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
		seat := seats[role.Handle]
		if seat == nil {
			continue
		}
		for _, b := range SeatToolBindings() {
			server := c.PerSeatServer(b.Server)
			if server == nil || !role.SeatAccessEnabled(b.Lane) {
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
			for _, key := range b.Keys {
				ref := "${" + envref.SealedName(b.Vendor, key.Field, seat.Handle()) + "}"
				name, value := key.Env, ref
				if server.Kind() == TransportHTTP {
					name, value = key.Header, key.Prefix+ref
				}
				// ONE KEY, whatever case a unit wrote it in: an http
				// header is one header in any case ([MCPServer.ValueKey]).
				for held := range block {
					if server.ValueKey(held) == server.ValueKey(name) {
						delete(block, held)
					}
				}
				block[name] = value
			}
			env[server.Name] = block
			seat.MCPEnv = env
		}
	}
}
