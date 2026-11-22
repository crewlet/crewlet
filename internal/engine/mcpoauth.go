package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/secrets"
)

// SIGNING THE COMPANY IN TO A REMOTE MCP SERVER WITH OAUTH, and keeping it
// signed in (ADR-0075 holds the argument): the MCP family of the company's
// grants (oauthgrant.go), one per server. What it adds is the server's
// discovered endpoints and three ways of registering, a grant bound to the
// server's address, and a replaced grant revoked at its vendor.

// The refusals of a sign-in, each naming what the person can change.
var (
	// ErrMCPNoOAuth is a server whose entry carries no oauth block.
	ErrMCPNoOAuth = errors.New("engine: the MCP server is not signed in to with OAuth; " +
		"give its entry an oauth block first")
	// ErrNoCallbackAddress is an engine with no public address for the
	// vendor to send the person back to.
	ErrNoCallbackAddress = errors.New("engine: this engine has no public address for the " +
		"vendor to send the person back to; set api.external_url, or api.public.external_url " +
		"where the public listener is on")
	// ErrMCPNotSignedIn is a sign-out of a server nobody signed in to.
	ErrMCPNotSignedIn = errors.New("engine: nobody is signed in to the MCP server")
	// ErrMCPServiceIdentity is a sign-in to a server the engine reaches as a
	// service identity (oauth.grant: client_credentials), whose tokens are
	// issued at use.
	ErrMCPServiceIdentity = errors.New("engine: the MCP server is signed in to as a service " +
		"identity with client credentials, and its tokens are issued as it is used; nobody signs in to it")
)

// mcpGrants is the MCP family's [grantKind].
type mcpGrants struct{}

func (mcpGrants) family() coord.GrantFamily { return coord.MCPGrantFamily }
func (mcpGrants) source() secrets.Source    { return secrets.SourceMCPSignIn }
func (mcpGrants) key(name string) string    { return name }
func (mcpGrants) page() string              { return "the server's page" }
func (mcpGrants) title(name string) string  { return name }
func (mcpGrants) revokesReplaced() bool     { return true }

func (mcpGrants) keepFresh(context.Context, *Engine, string, string) string { return "" }

func (mcpGrants) refused(_ coord.OAuthGrant, said string, _ time.Time) string { return said }

func (mcpGrants) accept(tokens oauth.Tokens, asked []string) ([]string, error) {
	return grantedScopes(tokens.Scopes, asked), nil
}

// usable holds a server back that lost its oauth block or moved to a service
// identity, and, for a grant, one whose address moved off it ([grantMoved]).
func (mcpGrants) usable(e *Engine, server string, g *coord.OAuthGrant) *signInNeeded {
	entry, err := e.oauthServer(server)
	switch {
	case err != nil:
		return &signInNeeded{server: server, unconfigured: "the server is no longer signed in to with OAuth"}
	case entry.OAuth.ServiceIdentity():
		return &signInNeeded{server: server,
			unconfigured: "the server now signs in as a service identity, which nobody signs in to",
			what:         "Nothing to do: the engine issues its tokens as it is used"}
	case g != nil:
		return e.grantMoved(server, *g)
	}
	return nil
}

func (mcpGrants) client(e *Engine, server string, c grantClient) (oauth.Client, error) {
	entry, err := e.oauthServer(server)
	if err != nil {
		entry = nil
	}
	return e.oauthClient(entry, c)
}

// CallbackURL is where a vendor sends a person's browser back after a sign-in,
// "" where the engine has no public address. A vendor that allowlists
// redirects is given this.
func (e *Engine) CallbackURL() string {
	if e.webhookBase == "" {
		return ""
	}
	return e.webhookBase + "/webhooks/mcp-oauth"
}

// ClientMetadataURL is the engine's client metadata document, its client id
// under cimd, "" where the engine has no public address.
func (e *Engine) ClientMetadataURL() string {
	if e.webhookBase == "" {
		return ""
	}
	return e.webhookBase + "/webhooks/mcp-oauth/client-metadata.json"
}

// ClientMetadata is the engine's client metadata document, served at
// [Engine.ClientMetadataURL]: a public client whose one redirect is
// [Engine.CallbackURL].
func (e *Engine) ClientMetadata() (oauth.Metadata, bool) {
	if e.webhookBase == "" {
		return oauth.Metadata{}, false
	}
	return oauth.ClientMetadata(e.ClientMetadataURL(), e.externalBase, e.CallbackURL()), true
}

// oauthServer is the configured server name and its oauth block, or why a
// sign-in gesture cannot be made on it.
func (e *Engine) oauthServer(name string) (*config.MCPServer, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil, ErrNoMCPServer
	}
	for i := range c.Config.MCPServers {
		server := &c.Config.MCPServers[i]
		if server.Name != name {
			continue
		}
		if server.OAuth == nil {
			return nil, ErrMCPNoOAuth
		}
		return server, nil
	}
	return nil, ErrNoMCPServer
}

// StartMCPSignIn begins a sign-in to server as by (ADR-0075).
func (e *Engine) StartMCPSignIn(ctx context.Context, name string, by iam.Actor) (SignInStarted, error) {
	server, err := e.oauthServer(name)
	if err != nil {
		return SignInStarted{}, err
	}
	if server.OAuth.ServiceIdentity() {
		return SignInStarted{}, ErrMCPServiceIdentity
	}
	if _, ready := e.fleetReady(); ready != nil {
		return SignInStarted{}, ready
	}
	redirect := e.CallbackURL()
	if redirect == "" {
		return SignInStarted{}, ErrNoCallbackAddress
	}
	env := e.resolver()
	address := env.Value(server.URL)
	if missing := env.Missing(server.URL); len(missing) > 0 {
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict,
			Reason: "the server's url reads " + strings.Join(missing, ", ") + ", which is not set",
			Remedy: "Set it in Settings, Secrets, then sign in"}
	}
	check := mcp.CheckAddress(ctx, address, log)
	ep, refusal := signInEndpoints(check)
	if refusal != nil {
		return SignInStarted{}, refusal
	}
	cfg := server.OAuth
	if unfit := oauth.CheckSignIn(ep, cfg.Client); unfit != nil {
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict,
			Reason: unfit.Error(), Remedy: "Change the server's oauth block, or ask its vendor"}
	}
	client, err := e.signInClient(ctx, server, ep, redirect)
	if err != nil {
		return SignInStarted{}, err
	}
	return e.startSignIn(ctx, signInStart{kind: mcpGrants{}, name: name, ep: ep, client: client,
		scopes: oauth.Scopes(cfg.Scopes, ep), redirect: redirect}, by)
}

// signInEndpoints are where a sign-in at the checked server is made, or why it
// cannot be.
func signInEndpoints(check mcp.AddressCheck) (*mcp.OAuthEndpoints, *SignInRefusal) {
	switch {
	case check.OAuth != nil && check.OAuth.Failure != nil:
		return nil, &SignInRefusal{Status: http.StatusBadGateway,
			Reason: "the server's sign-in metadata could not be read: " + check.OAuth.Failure.Said,
			Remedy: "Test the server from its page to see where it stops"}
	case check.OAuth != nil && check.OAuth.Endpoints != nil:
		return check.OAuth.Endpoints, nil
	case check.Auth == catalogue.AuthNone:
		return nil, &SignInRefusal{Status: http.StatusConflict,
			Reason: "the server answers without a sign-in",
			Remedy: "Remove its oauth block, or check its address"}
	case check.Test.Failure != nil && check.Auth == "":
		return nil, &SignInRefusal{Status: http.StatusBadGateway,
			Reason: "the server could not be reached: " + check.Test.Failure.Said,
			Remedy: "Test the server from its page to see where it stops"}
	default:
		return nil, &SignInRefusal{Status: http.StatusConflict,
			Reason: "the server does not ask for an OAuth sign-in: it publishes no protected-resource metadata",
			Remedy: "Send its credential in a header instead of oauth"}
	}
}

// signInClient is the client a sign-in at ep is made as: the engine's
// published document, a registration made now, or the company's own client.
func (e *Engine) signInClient(ctx context.Context, server *config.MCPServer, ep *mcp.OAuthEndpoints,
	redirect string) (grantClient, error) {

	cfg := server.OAuth
	switch cfg.Client {
	case catalogue.ClientCIMD:
		document := e.ClientMetadataURL()
		if !strings.HasPrefix(document, "https://") {
			return grantClient{}, &SignInRefusal{Status: http.StatusConflict,
				Reason: "a client metadata document is read by the vendor over https, and this " +
					"engine's public address is not https",
				Remedy: "Set " + cmpOr(e.webhookBaseKey, "api.external_url") + " to an https address, or " +
					"set oauth.client to dcr or own"}
		}
		return grantClient{Kind: catalogue.ClientCIMD, ID: document}, nil
	case catalogue.ClientDCR:
		registered, err := oauth.Register(ctx, ep, oauth.ClientMetadata("", e.externalBase, redirect))
		if err != nil {
			return grantClient{}, &SignInRefusal{Status: http.StatusBadGateway,
				Reason: "the authorization server did not register the engine: " + err.Error(),
				Remedy: "Set oauth.client to own with a client registered at the vendor, or ask its vendor"}
		}
		return grantClient{Kind: catalogue.ClientDCR, ID: registered.ID, Secret: registered.Secret}, nil
	default:
		if missing := e.resolver().Missing(cfg.ClientSecret); len(missing) > 0 {
			return grantClient{}, &SignInRefusal{Status: http.StatusConflict,
				Reason: "the client secret reads " + strings.Join(missing, ", ") + ", which is not set",
				Remedy: "Set it in Settings, Secrets, then sign in"}
		}
		return grantClient{Kind: catalogue.ClientOwn, ID: cfg.ClientID}, nil
	}
}

// oauthClient is the client a grant was made as, the company's own client's
// secret resolved from entry now.
func (e *Engine) oauthClient(entry *config.MCPServer, c grantClient) (oauth.Client, error) {
	out := oauth.Client{Kind: c.Kind, ID: c.ID, Secret: c.Secret}
	if c.Kind != catalogue.ClientOwn {
		return out, nil
	}
	if entry == nil || entry.OAuth == nil {
		return oauth.Client{}, ErrMCPNoOAuth
	}
	env := e.resolver()
	if missing := env.Missing(entry.OAuth.ClientSecret); len(missing) > 0 {
		return oauth.Client{}, fmt.Errorf("the client secret reads %s, which is not set",
			strings.Join(missing, ", "))
	}
	out.Secret = env.Value(entry.OAuth.ClientSecret)
	return out, nil
}

// grantMoved is why g may not be used for server, nil where it may: the
// server's resolved address now is not the resource g was issued for
// ([mcp.ResourceServes]).
//
// THE GRANT GOES WITH ITS RESOURCE, NOT ITS NAME: any configuration write can
// move a server's url, and the transport sends the token wherever the spec
// points, so one vendor's token would reach whoever answers at the new origin.
func (e *Engine) grantMoved(server string, g coord.OAuthGrant) *signInNeeded {
	entry, err := e.oauthServer(server)
	if err != nil {
		return &signInNeeded{server: server}
	}
	if mcp.ResourceServes(g.Resource, e.resolver().Value(entry.URL)) {
		return nil
	}
	return &signInNeeded{server: server, moved: redact.URL(g.Resource)}
}

// MCPSignIn reads an MCP sign-in by id, for the page that polls it.
func (e *Engine) MCPSignIn(ctx context.Context, id uuid.UUID) (coord.OAuthSignIn, error) {
	return e.signInByID(ctx, coord.MCPGrantFamily, id)
}

// SignOutMCPServer gives up the server's grant: revoked at its vendor where it
// offers that (RFC 7009), forgotten here, and the revision re-activated so
// every node stops sending its tokens; [ErrMCPNotSignedIn] where there is none.
func (e *Engine) SignOutMCPServer(ctx context.Context, name string, by iam.Actor) (GrantSignOut, error) {
	entry, err := e.oauthServer(name)
	if err != nil {
		return GrantSignOut{}, err
	}
	out, held, err := e.RevokeMCPGrant(ctx, entry)
	if err != nil {
		return GrantSignOut{}, err
	}
	if !held {
		return GrantSignOut{}, ErrMCPNotSignedIn
	}
	log.InfoContext(ctx, "mcp_signed_out", "server", name, "by", by.Name,
		"revocation", string(out.Revocation))
	e.reloadForGrant(ctx, name, "reload: signed out of MCP server "+name, by)
	return out, nil
}

// RevokeMCPGrant revokes the grant of entry's server at its vendor where it
// offers that, and deletes the sealed row, the public record and any sign-in
// in progress, reporting whether there was a grant. It re-activates nothing:
// a removal is itself a revision, and [Engine.SignOutMCPServer] re-activates.
//
// ENTRY IS THE SERVER AS CONFIGURED BEFORE a removal: the company's own client
// authenticates the revocation with the secret that entry names, which the
// new configuration no longer does.
func (e *Engine) RevokeMCPGrant(ctx context.Context, entry *config.MCPServer) (GrantSignOut, bool, error) {
	return e.giveUp(ctx, mcpGrants{}, entry.Name,
		func(c grantClient) (oauth.Client, error) { return e.oauthClient(entry, c) },
		func(sealedGrant) *GrantSignOut { return nil })
}

// HeldMCPGrant reports whether the server has a grant, for a removal's dry
// run: the record every node reads, or the sealed row where only that is left.
func (e *Engine) HeldMCPGrant(ctx context.Context, server string) (bool, error) {
	return e.heldGrant(ctx, coord.MCPGrantFamily, server)
}

// bearerFor is the token source a start of server sends with, or why it may
// not start ([signInNeeded]), read afresh at each pass ([Engine.grantSource]).
func (e *Engine) bearerFor(ctx context.Context, server string) (mcp.BearerSource, *signInNeeded) {
	s, needed := e.grantSource(ctx, mcpGrants{}, server)
	if needed != nil {
		return nil, needed
	}
	return s, nil
}

// testBearer is the token source a test of the saved server sends with, nil
// where it has no grant, so the test reaches the server's sign-in demand and
// reports it.
func (e *Engine) testBearer(ctx context.Context, server string) mcp.BearerSource {
	bearer, missing := e.bearerFor(ctx, server)
	if missing != nil {
		return nil
	}
	return bearer
}

// MCPGrants is every MCP server's grant as the fleet records it, for the
// servers' answers: the public half, never a token.
func (e *Engine) MCPGrants(ctx context.Context) ([]coord.OAuthGrant, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, nil
	}
	return e.backends.Fleet.ListOAuthGrants(ctx, coord.MCPGrantFamily)
}

// MCPSignInAddresses are the callback a vendor sends a person back to and the
// engine's client metadata document, each "" where the engine has no public
// address.
func (e *Engine) MCPSignInAddresses() (string, string) {
	return e.CallbackURL(), e.ClientMetadataURL()
}

// RevokeMCPServerGrant is [Engine.RevokeMCPGrant] in the plain words a
// server's removal answers (configapi.Grants): the revocation, the vendor's
// words, and whether there was a grant.
func (e *Engine) RevokeMCPServerGrant(ctx context.Context, entry *config.MCPServer) (string, string, bool, error) {
	out, held, err := e.RevokeMCPGrant(ctx, entry)
	if err != nil {
		return "", "", false, err
	}
	return string(out.Revocation), out.Said, held, nil
}
