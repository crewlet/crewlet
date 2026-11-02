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
//
// TWO GRANTS ([MCPOAuthGrant]): a person's sign-in (authorization_code, the
// default), or a SERVICE IDENTITY (client_credentials), a client the company
// registered at the vendor that the engine signs in as with its id and
// secret, with no person and nothing kept but those two: each node is issued
// its own access token as the server is used, and holds it in memory alone.
type MCPOAuth struct {
	// Grant is how the engine is granted access: a person signs the
	// company in once (authorization_code, the default, written or not), or
	// the engine signs in as a service identity, the company's own client,
	// whenever it needs a token (client_credentials).
	Grant MCPOAuthGrant `yaml:"grant,omitempty" json:"grant,omitempty" js:"enum=authorization_code|client_credentials" desc:"How the engine is granted access: authorization_code (default: a person signs the company in once) or client_credentials (a service identity: the company's own client, with client_id and client_secret, and no person)."`

	// Client is how the engine registers at the server's authorization
	// server for a person's sign-in: by publishing a client metadata
	// document (cimd), by registering when a sign-in starts (dcr), or as a
	// client an administrator registered at the vendor beforehand (own).
	// The address check (`POST /mcp-probe`) says which the server supports.
	// REQUIRED for authorization_code, and refused for client_credentials,
	// which always signs in as the company's own client.
	Client catalogue.OAuthClient `yaml:"client,omitempty" json:"client,omitempty" js:"enum=cimd|dcr|own" desc:"How the engine registers at the authorization server for a person's sign-in (grant authorization_code): cimd (its published client metadata document), dcr (registers when a sign-in starts) or own (a client registered at the vendor beforehand)."`

	// ClientID is the company's own client's id: required for client own
	// and for client_credentials, and refused otherwise, where the id is the
	// engine's document address (cimd) or what the registration issues
	// (dcr).
	ClientID string `yaml:"client_id,omitempty" json:"client_id,omitempty" desc:"The client id of the company's own client (client: own, or grant: client_credentials)."`

	// ClientSecret is the company's own client's secret: required for
	// client_credentials, and for client own present where the client was
	// registered as confidential. A ${VAR} in the secret store, as every
	// credential is; stored from the server's page it is write-only.
	ClientSecret string `secret:"true" yaml:"client_secret,omitempty" json:"client_secret,omitempty" desc:"The secret of the company's own client: required for grant client_credentials, and for client own where the client has one; ${VAR} supported."`

	// Scopes are the scopes the sign-in asks for. Absent, a person's
	// sign-in asks for the ones the server's own metadata lists, or none
	// where it lists none, and a service identity asks for none, which its
	// authorization server answers with the client's own default.
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty" desc:"Scopes the sign-in asks for; absent, a person's sign-in asks for the ones the server lists and a service identity for its client's default."`
}

// MCPOAuthGrant is the OAuth grant the engine is given a server's access by.
type MCPOAuthGrant string

const (
	// GrantAuthorizationCode is a person's sign-in, made once from the
	// server's page and renewed by the engine with the refresh token it
	// issued (OAuth 2.1's authorization code grant with PKCE). The default.
	GrantAuthorizationCode MCPOAuthGrant = "authorization_code"
	// GrantClientCredentials is a service identity: the engine signs in as
	// the company's own client, with its id and secret, whenever it needs an
	// access token (RFC 6749, 4.4). No person signs in, and nothing is kept
	// but the configuration's id and `${VAR}`.
	GrantClientCredentials MCPOAuthGrant = "client_credentials"
)

// MCPOAuthGrants is every grant, for a reader's copy.
var MCPOAuthGrants = []MCPOAuthGrant{GrantAuthorizationCode, GrantClientCredentials}

// Valid reports whether g is a grant this build signs in by.
func (g MCPOAuthGrant) Valid() bool { return slices.Contains(MCPOAuthGrants, g) }

// GrantType is the grant the block signs in by: [GrantAuthorizationCode]
// where none is written.
func (o *MCPOAuth) GrantType() MCPOAuthGrant {
	if o.Grant == "" {
		return GrantAuthorizationCode
	}
	return o.Grant
}

// ServiceIdentity reports whether the block signs in as a service identity
// ([GrantClientCredentials]): no person's sign-in, and a token issued at use.
func (o *MCPOAuth) ServiceIdentity() bool {
	return o != nil && o.GrantType() == GrantClientCredentials
}

// PersonSignIn reports whether the server is signed in to by a person's grant
// ([GrantAuthorizationCode]): what a sign-in, the grant the engine keeps for
// it and the alarms about it are for. False for a service identity, which
// nobody signs in to, and for a server with no oauth block.
func (m *MCPServer) PersonSignIn() bool {
	return m.OAuth != nil && !m.OAuth.ServiceIdentity()
}

// MaxMCPOAuthScopes is the most scopes a sign-in asks for: a vendor's whole
// list is a few dozen, and the address check keeps a document's first fifty.
const MaxMCPOAuthScopes = 50

// validateOAuth holds the server's `oauth` block to what a sign-in can make:
// a remote server for the whole company, signed in to by a grant that names
// what it needs (a person's registration, or the company's own client as a
// service identity), asking for scopes an authorization request can carry.
// Each problem is reported at the field that has it.
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
		switch http.CanonicalHeaderKey(key) {
		case "Authorization":
			p.add(entry(at(server, "headers"), key), ErrConflict,
				"the OAuth sign-in sends the Authorization header with each request; "+
					"remove this header, or remove oauth to send it instead")
		case http.CanonicalHeaderKey(MCPValueClientSecret):
			// THE KEY THE CLIENT'S SECRET IS STORED UNDER from the
			// server's page ([MCPValueClientSecret]): a header so named
			// could not be told from it.
			p.add(entry(at(server, "headers"), key), ErrConflict,
				"%s names the OAuth client's secret on a server signed in to with OAuth; "+
					"give the header another name", MCPValueClientSecret)
		}
	}
	switch {
	case o.Grant != "" && !o.Grant.Valid():
		p.add(at(path, "grant"), ErrUnknownValue, "%q (want %s)", o.Grant, words.Or(MCPOAuthGrants))
	case o.ServiceIdentity():
		o.validateServiceIdentity(p, path)
	default:
		o.validateSignIn(p, path)
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

// validateSignIn holds a person's sign-in (authorization_code) to a
// registration that names what it needs, and only that: the company's own
// client needs its id and may carry a secret, and a published document or a
// registration names neither.
func (o *MCPOAuth) validateSignIn(p *problems, path Path) {
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
}

// validateServiceIdentity holds a service identity (client_credentials) to
// what signing in as one takes: the company's own client's id and secret, and
// no registration, since nobody signs in to register one.
func (o *MCPOAuth) validateServiceIdentity(p *problems, path Path) {
	if o.Client != "" {
		p.add(at(path, "client"), ErrConflict, "client says how a person's sign-in registers "+
			"the engine; client_credentials signs in as the company's own client, the one "+
			"client_id names, so remove client")
	}
	if strings.TrimSpace(o.ClientID) == "" {
		p.add(at(path, "client_id"), ErrMissing, "client_credentials signs in as a client the "+
			"company registered at the vendor: give the id it was registered under")
	}
	if strings.TrimSpace(o.ClientSecret) == "" {
		p.add(at(path, "client_secret"), ErrMissing, "client_credentials signs in with the "+
			"client's secret: give it as a ${VAR} the secret store holds")
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
