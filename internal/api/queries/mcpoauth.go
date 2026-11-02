package queries

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// AN MCP SERVER'S OAUTH SIGN-IN, as the configuration's reader reads it.
//
// The public half of a server's grant ([coord.MCPGrant]) beside what the
// configuration asks for: how the engine registers, the client id it signs in
// as, the authorization server, the scopes granted, who signed in and when,
// when the grant runs out and why its last renewal failed. NEVER A TOKEN, and
// never the client's secret: the refresh token is the engine's own sealed row,
// which nothing answers, and the access token is held in each node's memory
// alone (internal/engine's mcpoauth.go).
//
// THE ADMINISTRATOR'S READING ALONE. A member's reading carries the server's
// state (`needs_sign_in` among them) and none of this: who signed the company
// in at a vendor is an account, which B5's allow-list never adds.

// MCPGrantReader reads the MCP servers' OAuth grants and the two addresses a
// vendor is given for a sign-in, as the engine keeps and serves them.
type MCPGrantReader interface {
	MCPGrants(ctx context.Context) ([]coord.MCPGrant, error)
	// MCPSignInAddresses are the callback a vendor sends a person back to
	// and the engine's client metadata document, each "" where the engine
	// has no public address.
	MCPSignInAddresses() (callback, clientMetadata string)
}

// MCPOAuthStatus is where a server's sign-in is.
type MCPOAuthStatus string

const (
	// MCPOAuthConnected is a grant the engine renews as the server is used.
	MCPOAuthConnected MCPOAuthStatus = "connected"
	// MCPOAuthNeedsSignIn is a server nobody has signed in to.
	MCPOAuthNeedsSignIn MCPOAuthStatus = "needs_sign_in"
	// MCPOAuthExpired is a grant past the end its vendor stated.
	MCPOAuthExpired MCPOAuthStatus = "expired"
	// MCPOAuthRefreshFailed is a grant its vendor refused to renew.
	MCPOAuthRefreshFailed MCPOAuthStatus = "refresh_failed"
)

// MCPOAuthStatuses is every status, for the gate holding a client's copy.
var MCPOAuthStatuses = []MCPOAuthStatus{MCPOAuthConnected, MCPOAuthNeedsSignIn, MCPOAuthExpired,
	MCPOAuthRefreshFailed}

// Valid reports whether s is a status this build sends.
func (s MCPOAuthStatus) Valid() bool { return slices.Contains(MCPOAuthStatuses, s) }

// MCPOAuthView is a server's sign-in, the administrator's reading.
type MCPOAuthView struct {
	Status MCPOAuthStatus `json:"status"`
	// Client is how the engine registers (`cimd`, `dcr`, `own`), as the
	// configuration says.
	Client string `json:"client"`
	// ClientID is the client id the grant was made as, else the one the
	// configuration names for the company's own client; "" for a
	// registration nobody has made yet. PUBLIC in every kind.
	ClientID string `json:"client_id"`
	// Issuer is the authorization server the grant is from, and Resource
	// the server address it was issued for (RFC 8707), the one its tokens
	// are sent to; each "" with none.
	Issuer   string `json:"issuer"`
	Resource string `json:"resource"`
	// Scopes are what the grant was granted, else what the configuration
	// asks for. ALWAYS A LIST.
	Scopes []string `json:"scopes"`
	// ConnectedBy and ConnectedAt are who signed in and when, null with no
	// grant.
	ConnectedBy *string    `json:"connected_by"`
	ConnectedAt *time.Time `json:"connected_at"`
	// ExpiresAt is when the grant runs out where its vendor said, null
	// otherwise.
	ExpiresAt *time.Time `json:"expires_at"`
	// Failure is the vendor's words for the renewal it refused for good,
	// bounded and redacted, and when; null while the grant renews.
	Failure *MCPOAuthFailure `json:"failure"`
}

// MCPOAuthFailure is a renewal a vendor refused for good.
type MCPOAuthFailure struct {
	Said string    `json:"said"`
	At   time.Time `json:"at"`
}

// oauthView is server's sign-in as grant (nil for none) says, at now; nil for a
// server the configuration does not sign in to with OAuth.
func oauthView(server *config.MCPServer, grant *coord.MCPGrant, now time.Time) *MCPOAuthView {
	if server == nil || server.OAuth == nil {
		return nil
	}
	cfg := server.OAuth
	out := &MCPOAuthView{Status: MCPOAuthNeedsSignIn, Client: string(cfg.Client), ClientID: cfg.ClientID,
		Scopes: append([]string{}, cfg.Scopes...)}
	if grant == nil {
		return out
	}
	by, at := grant.By, grant.At
	out.Status, out.ClientID, out.Issuer, out.Resource = MCPOAuthConnected, grant.ClientID, grant.Issuer,
		grant.Resource
	out.Scopes = append([]string{}, grant.Scopes...)
	out.ConnectedBy, out.ConnectedAt, out.ExpiresAt = &by, &at, grant.Expiry
	switch {
	case grant.Failed():
		out.Status = MCPOAuthRefreshFailed
		out.Failure = &MCPOAuthFailure{Said: grant.Failure, At: *grant.FailedAt}
	case grant.Expiry != nil && !now.Before(*grant.Expiry):
		out.Status = MCPOAuthExpired
	}
	return out
}

// mcpGrants is every server's grant by name, nil where the engine keeps none
// on this surface. A read that fails is UNKNOWN, never "nobody signed in".
func (s Sources) mcpGrants(ctx context.Context) (map[string]*coord.MCPGrant, error) {
	if s.MCPGrants == nil {
		return nil, nil
	}
	grants, err := s.MCPGrants.MCPGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*coord.MCPGrant, len(grants))
	for i := range grants {
		out[grants[i].Server] = &grants[i]
	}
	return out, nil
}
