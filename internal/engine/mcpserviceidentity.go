package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
)

// SIGNING IN TO A REMOTE MCP SERVER AS A SERVICE IDENTITY (ADR-0075): OAuth's
// client credentials grant, for a server whose entry's `oauth.grant` is
// client_credentials ([config.MCPOAuth.ServiceIdentity]).
//
// # Issued at use, held in memory, kept nowhere
//
// The engine signs in as the company's own client, with the id the
// configuration names and the secret its `${VAR}` resolves to, whenever the
// server needs an access token: at its start, as a token nears its end
// ([oauthRenewAhead]), and once on a 401. It is sent by the token source a
// person's sign-in is sent by ([oauthSource]), so there is one path a token
// takes to a server however it was issued. Each node is issued its own token
// and holds it in its memory alone: never sealed, recorded, answered, audited
// or logged. Nothing else is kept: no person signs in, no refresh token is
// kept, and no record says a token exists, so a removal or a sign-out has
// nothing of a service identity's to revoke.
//
// # No fleet lease
//
// A renewal of a person's sign-in holds the server's renewal lease because it
// spends the one refresh token the fleet shares. A service identity spends
// nothing shared: each node's request is whole in itself, and the token one
// node is issued takes nothing from another's. The node's source is the one
// holder that asks for its own token (its lock keeps two requests on the node
// from asking at once), and a lease across nodes would add a store round trip,
// and a failure whenever the store blinks, to every token for nothing it
// protects.
//
// # A refusal is held, not asked again by every request
//
// A vendor that refuses the credentials (a secret revoked or rotated at the
// vendor, a client not allowed the grant, a scope it will not issue) refuses
// them at every request until somebody changes them, and a vendor that is down
// is down for every request. So the source holds a failure, whatever it was,
// for the restart schedule's wait ([mcp.RestartWait]), doubling with each one
// in a row: a request within the wait is answered it without the vendor being
// asked, and the first after it asks again. A change to what the engine signs
// in with is a new source ([serviceIdentity.key]), which asks at once: an
// operator who stores a new secret is asking a new question (the shape of
// github.Issuer's held refusal).
//
// # Where the token endpoint is
//
// Read from the server's own metadata, as a person's sign-in reads it
// (internal/mcp's address check): the protected-resource metadata names the
// authorization server, whose metadata names the token endpoint, the grants it
// issues and how it takes a secret ([oauth.CheckClientCredentials]). Read at
// the source's first token, kept while tokens are issued, and read again after
// any failure, so a vendor that moves its endpoint is followed. Every address
// in it is dialled through [mcp.ServerNamedClient]: on the server's own origin
// as the server is dialled (the operator chose it, ADR-0072), anywhere else
// over https through the public-address guard, following no redirect.

// serviceIdentity is one server's service identity on this node, as one source
// signs in with it: the server's address, the client and the scopes, fixed for
// the source's life. Its other fields are read and written under the source's
// lock alone ([oauthSource.renewLocked]).
type serviceIdentity struct {
	engine *Engine
	server string
	// address is the server's address, resolved: what its metadata is read
	// for, and the resource every token is asked for.
	address string
	// client is the company's own client, its secret resolved.
	client oauth.Client
	scopes []string

	// endpoints are where tokens are asked for, nil until read and after a
	// failure.
	endpoints *mcp.OAuthEndpoints
	// held is the failure the source answers until its wait ends, nil while
	// tokens are issued.
	held *serviceFailure
}

// serviceFailure is the last failure to issue a service identity's token, and
// when to ask again.
type serviceFailure struct {
	err error
	// n counts the failures in a row, which doubles the wait.
	n     int
	retry time.Time
}

// key is what the identity's tokens are issued for: the server's address, the
// client's id and secret, and the scopes. A source is kept across applies
// while its key holds and replaced when it moves, which restarts the server
// with the new source, as a rotated header does.
//
// A DIGEST, so nothing compares or holds the secret but the identity that
// signs in with it.
func (s *serviceIdentity) key() [sha256.Size]byte {
	h := sha256.New()
	for _, part := range append([]string{s.address, s.client.ID, s.client.Secret}, s.scopes...) {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// newServiceIdentity is server's service identity as env resolves it now.
func (e *Engine) newServiceIdentity(server *config.MCPServer, env *config.Resolver) *serviceIdentity {
	cfg := server.OAuth
	return &serviceIdentity{
		engine: e, server: server.Name, address: env.Value(server.URL),
		client: oauth.Client{Kind: catalogue.ClientOwn, ID: cfg.ClientID, Secret: env.Value(cfg.ClientSecret)},
		scopes: slices.Clone(cfg.Scopes),
	}
}

// serviceSource is the token source server's service identity signs in with on
// this node: the one the node already holds where the identity's key has not
// moved, a new one otherwise. The key is a digest of what it signs in with, so
// the source for one server is found again only while all of it holds.
func (e *Engine) serviceSource(server *config.MCPServer, env *config.Resolver) *oauthSource {
	id := e.newServiceIdentity(server, env)
	e.oauth.mu.Lock()
	defer e.oauth.mu.Unlock()
	if held := e.oauth.sources[server.Name]; held != nil && held.service != nil &&
		held.service.key() == id.key() {
		return held
	}
	if e.oauth.sources == nil {
		e.oauth.sources = map[string]*oauthSource{}
	}
	s := &oauthSource{engine: e, server: server.Name, service: id}
	e.oauth.sources[server.Name] = s
	return s
}

// tokenSourceFor is the token source a start of server, signed in to with
// OAuth, sends with: its service identity's, or the company's grant's
// ([Engine.bearerFor]), which is held back while nobody holds one.
func (e *Engine) tokenSourceFor(ctx context.Context, server *config.MCPServer,
	env *config.Resolver) (mcp.BearerSource, *signInNeeded) {

	if server.OAuth.ServiceIdentity() {
		return e.serviceSource(server, env), nil
	}
	return e.bearerFor(ctx, server.Name)
}

// issue signs in as the service identity for a new access token, unless a
// failure is held: see the file's comment. Called with the source's lock held.
func (s *serviceIdentity) issue(ctx context.Context) (oauth.Tokens, error) {
	e := s.engine
	asked := e.now()
	if s.held != nil && asked.Before(s.held.retry) {
		return oauth.Tokens{}, s.held.err
	}
	tokens, err := s.ask(ctx, asked)
	if err == nil {
		if s.held != nil {
			log.InfoContext(ctx, "mcp_service_identity_signed_in", "server", s.server,
				"detail", "the authorization server issues the service identity's tokens again")
		}
		s.held = nil
		return tokens, nil
	}
	// THE CALLER'S OWN END IS NO ANSWER from the vendor: a request cut short
	// by its context holds nothing for the next one.
	if ctx.Err() != nil {
		return oauth.Tokens{}, err
	}
	n := 1
	if s.held != nil {
		n = s.held.n + 1
	}
	wait := mcp.RestartWait(n, e.mcpRetryBase)
	s.held = &serviceFailure{err: err, n: n, retry: e.now().Add(wait)}
	log.WarnContext(ctx, "mcp_service_identity_refused", "server", s.server, "class",
		string(mcp.ClassOf(err)), "error", err.Error(), "retry_seconds", int(wait.Seconds()),
		"detail", "every request to the server is refused this until then, when the authorization "+
			"server is asked again; a change to the client's id, its secret or the scopes asks at once")
	return oauth.Tokens{}, err
}

// ask reads where tokens are issued where it has not, and asks there.
func (s *serviceIdentity) ask(ctx context.Context, now time.Time) (oauth.Tokens, error) {
	if s.endpoints == nil {
		ep, err := serviceEndpoints(mcp.CheckAddress(ctx, s.address, log))
		if err != nil {
			return oauth.Tokens{}, err
		}
		if refused := oauth.CheckClientCredentials(ep); refused != nil {
			return oauth.Tokens{}, mcp.Classed(mcp.FailCredentialRejected, fmt.Errorf(
				"%s does not sign in a service identity: %w; change the server's oauth block, or ask "+
					"its vendor", s.server, refused))
		}
		s.endpoints = ep
	}
	tokens, err := oauth.ClientCredentials(ctx, s.endpoints, s.client, s.scopes, now)
	if err != nil {
		// READ AGAIN AT THE NEXT ASK: a vendor that moved its token endpoint
		// is followed, and one that is down costs one more read.
		s.endpoints = nil
		return oauth.Tokens{}, serviceRefusal(s.server, err)
	}
	return tokens, nil
}

// serviceEndpoints are where a service identity's tokens are issued for the
// checked server, or why there are none, classed as the server's failure.
func serviceEndpoints(check mcp.AddressCheck) (*mcp.OAuthEndpoints, error) {
	switch {
	case check.OAuth != nil && check.OAuth.Failure != nil:
		return nil, mcp.Classed(check.OAuth.Failure.Class, errors.New("the server's sign-in "+
			"metadata could not be read: "+check.OAuth.Failure.Said))
	case check.OAuth != nil && check.OAuth.Endpoints != nil:
		return check.OAuth.Endpoints, nil
	case check.Auth == catalogue.AuthNone:
		return nil, mcp.Classed(mcp.FailCredentialRejected, errors.New("the server answers without "+
			"a sign-in, so it is sent no token; remove its oauth block"))
	case check.Test.Failure != nil && check.Auth == "":
		return nil, mcp.Classed(check.Test.Failure.Class, errors.New("the server could not be "+
			"reached to read its sign-in metadata: "+check.Test.Failure.Said))
	default:
		return nil, mcp.Classed(mcp.FailCredentialRejected, errors.New("the server does not take an "+
			"OAuth sign-in: it publishes no protected-resource metadata; send its credential in a "+
			"header instead of oauth"))
	}
}

// serviceRefusal is a failed ask for a service identity's token as the
// server's failure: the vendor's own words for a refusal, classed by what it
// means for the credentials, and anything else classed by its cause. It never
// carries the secret or a token (internal/mcp/oauth withholds both).
func serviceRefusal(server string, err error) error {
	var refused *oauth.Error
	if !errors.As(err, &refused) {
		class := mcp.ClassOf(err)
		if class == "" {
			class = mcp.FailUnreachable
		}
		return mcp.Classed(class, fmt.Errorf("%s's service identity could not be signed in: %w",
			server, err))
	}
	said := oauthWords(err)
	switch {
	case refused.Status == 429:
		return mcp.Classed(mcp.FailRateLimited, fmt.Errorf("the authorization server asked the "+
			"engine to slow down (%s)", said))
	case refused.Code == "invalid_scope", refused.Code == "access_denied", refused.Code == "invalid_target":
		return mcp.Classed(mcp.FailMissingPermission, fmt.Errorf("the authorization server will not "+
			"issue the service identity a token for the scopes or the server asked for (%s); grant the "+
			"client them at the vendor, or change oauth.scopes", said))
	case refused.Definitive():
		return mcp.Classed(mcp.FailCredentialRejected, fmt.Errorf("the authorization server refused "+
			"the service identity's credentials (%s); check oauth.client_id and the secret "+
			"oauth.client_secret reads", said))
	}
	return mcp.Classed(mcp.FailUnreachable, fmt.Errorf("the authorization server did not issue the "+
		"service identity a token (%s)", said))
}
