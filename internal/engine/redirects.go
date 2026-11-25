package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/notice"
)

// THE CLIENTS A PERSON REGISTERED, whose redirect URIs only a person changes:
// a Google consent product's client and an MCP server's `oauth.client: own`.
// A `dcr` client registers the callback anew at each sign-in, a `cimd` one is
// published at the address now, and a service identity has no redirect, so
// none of those is left behind when [Engine.CallbackURL] moves (internal/notice).

// RegisteredClients is every OAuth client a person registered that the running
// configuration signs in as and a grant was made through, with the redirect
// its last grant came back through: one per Google client, naming every
// product sharing it, and one per MCP server.
func (e *Engine) RegisteredClients(ctx context.Context) ([]notice.RegisteredClient, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, nil
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no fleet store")
	}
	google, err := e.backends.Fleet.ListOAuthGrants(ctx, coord.IntegrationGrantFamily)
	if err != nil {
		return nil, fmt.Errorf("engine: read the integrations' grants: %w", err)
	}
	servers, err := e.backends.Fleet.ListOAuthGrants(ctx, coord.MCPGrantFamily)
	if err != nil {
		return nil, fmt.Errorf("engine: read the MCP servers' grants: %w", err)
	}
	return append(googleClients(c.Config, byName(google)), mcpClients(c.Config, byName(servers))...), nil
}

// byName is grants by their name.
func byName(grants []coord.OAuthGrant) map[string]coord.OAuthGrant {
	out := make(map[string]coord.OAuthGrant, len(grants))
	for _, g := range grants {
		out[g.Name] = g
	}
	return out
}

// googleClients is each Google client cfg's consent products sign in as, once
// however many share it, decided by the newest grant made as it, whose product
// it names first: where a person signs in again.
func googleClients(cfg *config.Company, grants map[string]coord.OAuthGrant) []notice.RegisteredClient {
	var out []notice.RegisteredClient
	at := map[string]int{}
	for _, p := range googleauth.Consents() {
		client, on := p.Client(cfg)
		if !on || client.ID == "" {
			continue
		}
		i, seen := at[client.ID]
		if !seen {
			i = len(out)
			at[client.ID] = i
			out = append(out, notice.RegisteredClient{Vendor: notice.VendorGoogle, ID: client.ID,
				Label: googleauth.ShortClientID(client.ID)})
		}
		rc := &out[i]
		use := notice.ClientUse{Name: string(p.Kind), Title: p.Name}
		// A GRANT MADE AS ANOTHER CLIENT says nothing of this one's list.
		if g, held := grants[string(p.Kind)]; held && g.ClientID == client.ID && g.At.After(rc.At) {
			rc.Redirect, rc.At = g.Redirect, g.At
			rc.Uses = append([]notice.ClientUse{use}, rc.Uses...)
			continue
		}
		rc.Uses = append(rc.Uses, use)
	}
	// A CLIENT NO GRANT WAS MADE AS proves nothing about its list.
	return slices.DeleteFunc(out, func(c notice.RegisteredClient) bool { return c.At.IsZero() })
}

// mcpClients is each MCP server cfg runs that a person signs in to as the
// company's own client, as its grant made as that client left it.
func mcpClients(cfg *config.Company, grants map[string]coord.OAuthGrant) []notice.RegisteredClient {
	var out []notice.RegisteredClient
	for i := range cfg.MCPServers {
		s := &cfg.MCPServers[i]
		if !s.IsEnabled() || !s.PersonSignIn() || s.OAuth.Client != catalogue.ClientOwn {
			continue
		}
		g, held := grants[s.Name]
		if !held || g.Client != string(catalogue.ClientOwn) || g.ClientID != s.OAuth.ClientID {
			continue
		}
		out = append(out, notice.RegisteredClient{Vendor: notice.VendorMCP, ID: g.ClientID, Label: s.Name,
			Uses: []notice.ClientUse{{Name: s.Name, Title: s.Name}}, Redirect: g.Redirect, At: g.At})
	}
	return out
}
