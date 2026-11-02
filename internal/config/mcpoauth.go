package config

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/words"
)

// MCPOAuth is how the company signs in to a remote MCP server with OAuth: one
// grant for the company, which a person makes once from the server's page
// and the engine renews on every node that runs the server (internal/mcp/oauth,
// internal/engine's mcpoauth.go).
//
// THE GRANT IS NOT HERE. What a sign-in is granted (the refresh token, and the
// client a registration issued) is the engine's own, sealed in the secret
// store under a name no `${VAR}` reaches and never answered back; this block
// says only how the sign-in is made.
//
// ONE GRANT PER SERVER, for the company: a per-agent server (`shared: false`)
// carries each seat's own credential in its `mcp_env`, and a sign-in is
// nobody's in particular, so the two are refused together.
type MCPOAuth struct {
	// Client is how the engine registers at the server's authorization
	// server: by publishing a client metadata document (cimd), by
	// registering when a sign-in starts (dcr), or as a client an
	// administrator registered at the vendor beforehand (own). The address
	// check (`POST /mcp-probe`) says which the server supports.
	Client catalogue.OAuthClient `yaml:"client" json:"client" js:"required;enum=cimd|dcr|own" desc:"How the engine registers at the authorization server: cimd (its published client metadata document), dcr (registers when a sign-in starts) or own (a client registered at the vendor beforehand)."`

	// ClientID is the company's own client's id: required for client own,
	// and refused otherwise, where the id is the engine's document address
	// (cimd) or what the registration issues (dcr).
	ClientID string `yaml:"client_id,omitempty" json:"client_id,omitempty" desc:"The client id of the company's own client (client: own)."`

	// ClientSecret is the company's own client's secret, for a client
	// registered as confidential; absent for a public one. A ${VAR} in the
	// secret store, as every credential is.
	ClientSecret string `secret:"true" yaml:"client_secret,omitempty" json:"client_secret,omitempty" desc:"The secret of the company's own client, where it has one (client: own); ${VAR} supported."`

	// Scopes are the scopes the sign-in asks for. Absent, it asks for the
	// ones the server's own metadata lists, or none where it lists none.
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty" desc:"Scopes the sign-in asks for; absent asks for the ones the server lists."`
}

// MaxMCPOAuthScopes is the most scopes a sign-in asks for: a vendor's whole
// list is a few dozen, and the address check keeps a document's first fifty.
const MaxMCPOAuthScopes = 50

// validateOAuth holds the server's `oauth` block to what a sign-in can make:
// a remote server for the whole company, signed in to by one registration
// that names what it needs, asking for scopes an authorization request can
// carry.
func (m *MCPServer) validateOAuth(p *problems, server Path) {
	o := m.OAuth
	if o == nil {
		return
	}
	path := at(server, "oauth")
	if m.Kind() != TransportHTTP {
		p.add(path, ErrConflict, "oauth signs the company in to a remote server; a stdio "+
			"server is launched with its credentials in env")
	}
	if !m.IsShared() {
		p.add(path, ErrConflict, "an OAuth sign-in is one grant for the company, and a "+
			"per-agent server (shared: false) acts as each seat's own identity: give each "+
			"seat its credential in mcp_env, or make the server shared")
	}
	// THE SIGN-IN SENDS AUTHORIZATION, at every request, so a configured
	// one would be overwritten at best and sent beside the token at worst.
	for _, key := range slices.Sorted(maps.Keys(m.Headers)) {
		if http.CanonicalHeaderKey(key) == "Authorization" {
			p.add(entry(at(server, "headers"), key), ErrConflict,
				"the OAuth sign-in sends the Authorization header with each request; "+
					"remove this header, or remove oauth to send it instead")
		}
	}
	switch {
	case o.Client == "":
		p.add(at(path, "client"), ErrMissing, "say how the engine registers at the "+
			"authorization server: %s", words.Or(catalogue.OAuthClients))
	case !o.Client.Valid():
		p.add(at(path, "client"), ErrUnknownValue, "%q (want %s)", o.Client,
			words.Or(catalogue.OAuthClients))
	}
	if o.Client == catalogue.ClientOwn && strings.TrimSpace(o.ClientID) == "" {
		p.add(at(path, "client_id"), ErrMissing, "the company's own client (client: own) "+
			"needs the id it was registered under at the vendor")
	}
	if o.Client != catalogue.ClientOwn && o.Client.Valid() {
		if o.ClientID != "" {
			p.add(at(path, "client_id"), ErrConflict, "client_id is the company's own "+
				"client's (client: own); with client %s the id is %s", o.Client, issuedID(o.Client))
		}
		if o.ClientSecret != "" {
			p.add(at(path, "client_secret"), ErrConflict, "client_secret is the company's own "+
				"client's (client: own); with client %s the engine signs in as a public client", o.Client)
		}
	}
	if len(o.Scopes) > MaxMCPOAuthScopes {
		p.add(at(path, "scopes"), ErrOutOfRange, "lists %d scopes, and a sign-in asks for at "+
			"most %d", len(o.Scopes), MaxMCPOAuthScopes)
	}
	seen := map[string]bool{}
	for i, scope := range o.Scopes {
		switch {
		case !scopeToken(scope):
			p.add(idx(at(path, "scopes"), i), ErrUnknownValue, "%q is not a scope: a scope "+
				"is printable characters with no space, quote or backslash (RFC 6749, 3.3)", scope)
		case seen[scope]:
			p.add(idx(at(path, "scopes"), i), ErrConflict, "%q is listed twice", scope)
		}
		seen[scope] = true
	}
}

// issuedID says where a client id comes from for a registration that is not
// the company's own.
func issuedID(c catalogue.OAuthClient) string {
	if c == catalogue.ClientCIMD {
		return "the address of the engine's client metadata document"
	}
	return "the one the registration issues"
}

// scopeToken reports whether s is one scope as RFC 6749 spells it: one or
// more of the printable ASCII characters but space, `"` and `\`.
func scopeToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}
