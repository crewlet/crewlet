package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/textcut"
)

// SIGNING THE COMPANY IN TO A REMOTE MCP SERVER WITH OAUTH, and keeping it
// signed in (ADR-0073).
//
// A server whose entry carries `oauth` ([config.MCPOAuth]) is reached with an
// access token the engine is issued under ONE GRANT FOR THE COMPANY, which a
// person makes once, from the server's page, and the engine renews on every
// node that runs the server for as long as the vendor honours it.
//
// # Where each part of a grant lives
//
//   - THE REFRESH TOKEN, and the client a registration issued, are SEALED in
//     the secret store under the engine's own name for the server,
//     `mcp/<server>/refresh` ([fleetsecrets.Estate]): a row no operator
//     surface lists, reveals, sets or deletes, and no `${VAR}` reaches, so
//     nothing can read it back by construction rather than by a check.
//   - THE PUBLIC HALF (who signed in and when, the client and the issuer,
//     the scopes, when it runs out, and why its last renewal failed) is a
//     coordination record ([coord.MCPGrant]) every node's status answer and
//     alarm reads.
//   - THE ACCESS TOKEN is issued AT USE and held in each node's memory
//     alone ([oauthSource]): never sealed, never in a record, an answer, an
//     audit or a log line. Each node renews its own as it runs out.
//
// # One renewer at a time, fleet-wide
//
// A renewal spends the refresh token: OAuth 2.1 rotates it for a client with
// no secret, and a vendor that sees one used twice may revoke the whole grant.
// So a renewal on any node holds the server's renewal lease
// ([oauthRenewalClass]) while it reads the sealed grant, asks the vendor and
// writes the rotated token back, and the write is at the version it read
// ([fleetsecrets.Estate.Swap]). The lease keeps two nodes from spending one
// refresh token; the version keeps a renewal whose lease lapsed under it from
// writing over the one that took over.
//
// # A sign-in is a state the engine signed and spends once
//
// `POST /mcp-servers/{name}/oauth` starts one ([Engine.StartMCPSignIn]): the
// engine reads the server's sign-in metadata (internal/mcp's address check),
// registers where the client is a registration, mints a PKCE verifier, and
// keeps everything the callback needs, sealed, on a coordination record that
// lapses with it ([coord.MCPSignIn]). The person's browser carries only the
// authorization address: the state in it is the sign-in's id SIGNED under the
// fleet keyring ([runtoken], domain [mcpOAuthDomain]), so the callback, which
// is unauthenticated (`GET /webhooks/mcp-oauth`), trusts nothing but that
// signature, and spends the sign-in by moving it on at the version it read
// ([Engine.CompleteMCPSignIn]). The verifier never leaves the engine.
//
// # Every node starts the server from the new grant
//
// A sign-in that completes, and a sign-out, re-activate the revision as a
// sealed credential does: every node applies again, reads the grant record
// afresh, and starts or stops the server ([Engine.bearerFor]).

// mcpOAuthDomain separates a sign-in's state from every other token the fleet
// keyring signs.
const mcpOAuthDomain = "mcp-oauth"

// The bounds of a sign-in and a renewal.
const (
	// MCPSignInTTL is how long a sign-in's state is accepted: a person
	// reading a vendor's consent page, choosing an account and approving,
	// with room for a second factor. Fifteen minutes, as a Slack install
	// link's.
	MCPSignInTTL = 15 * time.Minute

	// oauthRenewAhead is how long before an access token runs out the
	// engine renews it. REASONED: a token is checked when a request
	// arrives, not while a call runs, so this covers the clock between the
	// engine and the vendor, and a request in flight across the boundary.
	oauthRenewAhead = 2 * time.Minute

	// oauthFreshWindow is how long a token just issued is sent again rather
	// than renewed when a server refuses it. REASONED: a token refused
	// within half a minute of being issued is refused for a reason a
	// renewal does not change (a scope, a revoked grant), and renewing it
	// on every refusal would spend the refresh token once per request; this
	// bounds it to two renewals a minute per node and server.
	oauthFreshWindow = 30 * time.Second

	// oauthRenewalLeaseTTL bounds the renewal lease. REASONED: a renewal is
	// one request to the token endpoint ([oauth]'s 15-second budget) and two
	// store writes; thirty seconds outlasts both, and a renewer that dies
	// holding it costs the next renewal at most that long.
	oauthRenewalLeaseTTL = 30 * time.Second

	// oauthRenewalWait is how long a renewal waits for another node's to
	// end before it gives up and the request fails, to be retried.
	oauthRenewalWait = 20 * time.Second

	// oauthRenewalPoll is how often a waiting renewal asks for the lease.
	oauthRenewalPoll = 200 * time.Millisecond

	// oauthStoreBudget bounds the store writes a callback or a sign-out
	// makes after the request that carried it may have ended.
	oauthStoreBudget = 15 * time.Second
)

// oauthRenewalClass is the lease class a renewal holds, one resource per
// server.
const oauthRenewalClass coord.Class = "mcp_oauth_renewal"

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
	// ErrNoMCPSignIn is a sign-in id no record holds, or one that lapsed.
	ErrNoMCPSignIn = errors.New("engine: this sign-in is not known any more; start it again")
	// ErrMCPNotSignedIn is a sign-out of a server nobody signed in to.
	ErrMCPNotSignedIn = errors.New("engine: nobody is signed in to the MCP server")
	// ErrMCPServiceIdentity is a sign-in to a server the engine signs in to
	// as a service identity (oauth.grant: client_credentials), which nobody
	// signs in to: its tokens are issued as it is used.
	ErrMCPServiceIdentity = errors.New("engine: the MCP server is signed in to as a service " +
		"identity with client credentials, and its tokens are issued as it is used; nobody signs in to it")
)

// MCPSignInRefusal is why a sign-in could not be started or finished, in words
// a person acts on. It never carries a code, a token or the verifier.
type MCPSignInRefusal struct {
	// Status is the HTTP status the route or the landing answers with.
	Status int
	// Reason is what went wrong, and Remedy what to do about it.
	Reason, Remedy string
}

func (r *MCPSignInRefusal) Error() string { return "engine: " + r.Reason + ". " + r.Remedy }

// refusedState is the one answer for a state that is forged, edited, lapsed,
// signed under a key this fleet no longer holds, never minted, or already
// spent: the only move for any of them is a new sign-in, and telling a caller
// which tells an attacker the same.
var refusedState = &MCPSignInRefusal{
	Status: http.StatusBadRequest,
	Reason: "this sign-in link is not one this engine issued, has expired, or was already used",
	Remedy: "Start the sign-in again from the server's page in Settings, MCP & tools",
}

// MCPSignInStarted is a sign-in begun: the address the person's browser is
// sent to, and the record the page polls.
type MCPSignInStarted struct {
	SignIn       coord.MCPSignIn
	AuthorizeURL string
}

// MCPSignInLanding is what the vendor sent the person's browser back with.
type MCPSignInLanding struct {
	State, Code, Issuer string
	// Error and Description are the vendor's refusal (RFC 6749, 4.1.2.1).
	Error, Description string
}

// signInSealed is what a sign-in's callback finishes it with, sealed on its
// record ([coord.MCPSignIn.Sealed]).
type signInSealed struct {
	Verifier  string             `json:"verifier"`
	Client    grantClient        `json:"client"`
	Endpoints mcp.OAuthEndpoints `json:"endpoints"`
	Scopes    []string           `json:"scopes"`
	Redirect  string             `json:"redirect"`
}

// grantClient is the client a grant was made as. The company's own client's
// secret is never kept here: it is the configuration's `${VAR}`, resolved at
// each use, so a rotated secret is the one sent.
type grantClient struct {
	Kind   catalogue.OAuthClient `json:"kind"`
	ID     string                `json:"id"`
	Secret string                `json:"secret,omitempty"`
}

// sealedGrant is a server's grant as its estate row holds it.
type sealedGrant struct {
	Grant     uuid.UUID          `json:"grant"`
	Refresh   string             `json:"refresh"`
	Client    grantClient        `json:"client"`
	Endpoints mcp.OAuthEndpoints `json:"endpoints"`
}

// grantRow is a server's estate row: the engine's own name for its grant.
func grantRow(server string) string { return "mcp/" + server + "/refresh" }

// signInAAD binds a sign-in's sealed half to the sign-in, so an envelope moved
// to another record fails to open.
func signInAAD(id uuid.UUID) string { return secrets.AADForVar("mcp-sign-in/" + id.String()) }

// CallbackURL is where a vendor sends a person's browser back after a sign-in,
// "" where the engine has no public address. A vendor that allowlists
// redirects is given this.
func (e *Engine) CallbackURL() string {
	if e.webhookBase == "" {
		return ""
	}
	return e.webhookBase + "/webhooks/mcp-oauth"
}

// ClientMetadataURL is the address of the engine's client metadata document,
// the client id it signs in as by a published document (cimd), "" where the
// engine has no public address.
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

// oauthSigner is the fleet keyring's signer for sign-in states.
func (e *Engine) oauthSigner() (*runtoken.Signer, error) {
	e.oauth.signerOnce.Do(func() {
		e.oauth.signer, e.oauth.signerErr = runtoken.New(runtoken.Options{
			Domain: mcpOAuthDomain, Material: tokenMaterial(e.boot), Now: e.now})
	})
	return e.oauth.signer, e.oauth.signerErr
}

// oauthState is this node's part in its servers' OAuth: the signer, and the
// access token source of each server under its current grant.
type oauthState struct {
	signerOnce sync.Once
	signer     *runtoken.Signer
	signerErr  error

	mu sync.Mutex
	// sources are each server's token source, under the grant it was made
	// for: a new grant is a new source.
	sources map[string]*oauthSource
	// last are each server's grant as this node last read it, for an apply
	// whose read fails: a store that blinks is not a sign-out.
	last map[string]*coord.MCPGrant
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

// fleetReady is the stores a sign-in reads and writes, or why there are none.
func (e *Engine) fleetReady() (*fleetsecrets.Estate, error) {
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil, errors.New("engine: this engine holds no fleet store and keyring to keep " +
			"a sign-in in; it was not built by engine.New")
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).Estate(), nil
}

// StartMCPSignIn begins a sign-in to server as by: see the file's comment.
func (e *Engine) StartMCPSignIn(ctx context.Context, name string, by iam.Actor) (MCPSignInStarted, error) {
	server, err := e.oauthServer(name)
	if err != nil {
		return MCPSignInStarted{}, err
	}
	if server.OAuth.ServiceIdentity() {
		return MCPSignInStarted{}, ErrMCPServiceIdentity
	}
	if _, ready := e.fleetReady(); ready != nil {
		return MCPSignInStarted{}, ready
	}
	signer, err := e.oauthSigner()
	if err != nil {
		return MCPSignInStarted{}, fmt.Errorf("engine: sign a sign-in's state: %w", err)
	}
	redirect := e.CallbackURL()
	if redirect == "" {
		return MCPSignInStarted{}, ErrNoCallbackAddress
	}
	env := e.resolver()
	address := env.Value(server.URL)
	if missing := env.Missing(server.URL); len(missing) > 0 {
		return MCPSignInStarted{}, &MCPSignInRefusal{Status: http.StatusConflict,
			Reason: "the server's url reads " + strings.Join(missing, ", ") + ", which is not set",
			Remedy: "Set it in Settings, Secrets, then sign in"}
	}
	check := mcp.CheckAddress(ctx, address, log)
	ep, refusal := signInEndpoints(check)
	if refusal != nil {
		return MCPSignInStarted{}, refusal
	}
	cfg := server.OAuth
	if unfit := oauth.CheckSignIn(ep, cfg.Client); unfit != nil {
		return MCPSignInStarted{}, &MCPSignInRefusal{Status: http.StatusConflict,
			Reason: unfit.Error(), Remedy: "Change the server's oauth block, or ask its vendor"}
	}
	client, err := e.signInClient(ctx, server, ep, redirect)
	if err != nil {
		return MCPSignInStarted{}, err
	}
	key, err := oauth.NewPKCE()
	if err != nil {
		return MCPSignInStarted{}, err
	}
	scopes := oauth.Scopes(cfg.Scopes, ep)
	now := e.now()
	record := coord.MCPSignIn{
		ID: uuid.New(), Server: name, Status: coord.MCPSignInPending,
		StartedAt: now, ExpiresAt: now.Add(MCPSignInTTL),
		By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
	}
	sealed, err := json.Marshal(signInSealed{Verifier: key.Verifier, Client: client,
		Endpoints: *ep, Scopes: scopes, Redirect: redirect})
	if err != nil {
		return MCPSignInStarted{}, fmt.Errorf("engine: encode a sign-in: %w", err)
	}
	if record.Sealed, err = e.cipher.Encrypt(string(sealed), signInAAD(record.ID)); err != nil {
		return MCPSignInStarted{}, fmt.Errorf("engine: seal a sign-in: %w", err)
	}
	stored, err := e.replaceSignIn(ctx, record)
	if err != nil {
		return MCPSignInStarted{}, err
	}
	state := signer.Mint(name+stateSep+stored.ID.String(), MCPSignInTTL)
	authorize, err := oauth.AuthorizeURL(ep, oauth.Client{Kind: client.Kind, ID: client.ID},
		redirect, state, key.Challenge, scopes)
	if err != nil {
		return MCPSignInStarted{}, &MCPSignInRefusal{Status: http.StatusConflict,
			Reason: err.Error(), Remedy: "Ask the server's vendor; nothing was signed in"}
	}
	log.InfoContext(ctx, "mcp_sign_in_started", "server", name, "sign_in", stored.ID.String(),
		"client", string(client.Kind), "issuer", redact.URL(ep.Issuer), "by", by.Name)
	return MCPSignInStarted{SignIn: stored, AuthorizeURL: authorize}, nil
}

// stateSep joins a state's subject fields: no server name or uuid holds it.
const stateSep = "~"

// signInEndpoints are where a sign-in at the checked server is made, or why it
// cannot be.
func signInEndpoints(check mcp.AddressCheck) (*mcp.OAuthEndpoints, *MCPSignInRefusal) {
	switch {
	case check.OAuth != nil && check.OAuth.Failure != nil:
		return nil, &MCPSignInRefusal{Status: http.StatusBadGateway,
			Reason: "the server's sign-in metadata could not be read: " + check.OAuth.Failure.Said,
			Remedy: "Test the server from its page to see where it stops"}
	case check.OAuth != nil && check.OAuth.Endpoints != nil:
		return check.OAuth.Endpoints, nil
	case check.Auth == catalogue.AuthNone:
		return nil, &MCPSignInRefusal{Status: http.StatusConflict,
			Reason: "the server answers without a sign-in",
			Remedy: "Remove its oauth block, or check its address"}
	case check.Test.Failure != nil && check.Auth == "":
		return nil, &MCPSignInRefusal{Status: http.StatusBadGateway,
			Reason: "the server could not be reached: " + check.Test.Failure.Said,
			Remedy: "Test the server from its page to see where it stops"}
	default:
		return nil, &MCPSignInRefusal{Status: http.StatusConflict,
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
			return grantClient{}, &MCPSignInRefusal{Status: http.StatusConflict,
				Reason: "a client metadata document is read by the vendor over https, and this " +
					"engine's public address is not https",
				Remedy: "Set " + cmpOr(e.webhookBaseKey, "api.external_url") + " to an https address, or " +
					"set oauth.client to dcr or own"}
		}
		return grantClient{Kind: catalogue.ClientCIMD, ID: document}, nil
	case catalogue.ClientDCR:
		registered, err := oauth.Register(ctx, ep, oauth.ClientMetadata("", e.externalBase, redirect))
		if err != nil {
			return grantClient{}, &MCPSignInRefusal{Status: http.StatusBadGateway,
				Reason: "the authorization server did not register the engine: " + err.Error(),
				Remedy: "Set oauth.client to own with a client registered at the vendor, or ask its vendor"}
		}
		return grantClient{Kind: catalogue.ClientDCR, ID: registered.ID, Secret: registered.Secret}, nil
	default:
		if missing := e.resolver().Missing(cfg.ClientSecret); len(missing) > 0 {
			return grantClient{}, &MCPSignInRefusal{Status: http.StatusConflict,
				Reason: "the client secret reads " + strings.Join(missing, ", ") + ", which is not set",
				Remedy: "Set it in Settings, Secrets, then sign in"}
		}
		return grantClient{Kind: catalogue.ClientOwn, ID: cfg.ClientID}, nil
	}
}

// cmpOr is the first non-empty of a and b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// replaceSignIn files record as the server's one sign-in, replacing whatever
// sign-in it had: a second start is a person who wants to start again.
func (e *Engine) replaceSignIn(ctx context.Context, record coord.MCPSignIn) (coord.MCPSignIn, error) {
	fleet := e.backends.Fleet
	for range maxRetryRounds {
		held, found, err := fleet.MCPSignIn(ctx, record.Server)
		if err != nil {
			return coord.MCPSignIn{}, fmt.Errorf("engine: read the server's sign-in: %w", err)
		}
		if found {
			if _, gone := fleet.DeleteMCPSignIn(ctx, record.Server, held.Version); gone != nil {
				return coord.MCPSignIn{}, fmt.Errorf("engine: replace the server's sign-in: %w", gone)
			}
		}
		stored, created, err := fleet.CreateMCPSignIn(ctx, record)
		if err != nil {
			return coord.MCPSignIn{}, fmt.Errorf("engine: record the sign-in: %w", err)
		}
		if created {
			return stored, nil
		}
	}
	return coord.MCPSignIn{}, errors.New("engine: the server's sign-in kept changing; start it again")
}

// maxRetryRounds bounds a compare-and-set a person's gesture retries: it
// contends with at most the handful of other people acting on one server at
// once, so eight lost rounds is a store that is not settling.
const maxRetryRounds = 8

// MCPSignIn reads a sign-in by id, for the page that polls it.
func (e *Engine) MCPSignIn(ctx context.Context, id uuid.UUID) (coord.MCPSignIn, error) {
	if _, err := e.fleetReady(); err != nil {
		return coord.MCPSignIn{}, err
	}
	all, err := e.backends.Fleet.ListMCPSignIns(ctx)
	if err != nil {
		return coord.MCPSignIn{}, fmt.Errorf("engine: read the sign-ins: %w", err)
	}
	for _, s := range all {
		if s.ID == id && !s.Lapsed(e.now()) {
			return s, nil
		}
	}
	return coord.MCPSignIn{}, ErrNoMCPSignIn
}

// CompleteMCPSignIn finishes the sign-in the landing's state names: it
// verifies and spends the state, exchanges the code, seals the grant and
// re-activates the revision so every node starts the server under it. It
// answers the server's name. Every refusal is an [*MCPSignInRefusal]; none
// quotes the code, a token or the verifier.
func (e *Engine) CompleteMCPSignIn(ctx context.Context, landing MCPSignInLanding) (string, error) {
	signer, err := e.oauthSigner()
	if err != nil {
		return "", refusedState
	}
	server, id, ok := openState(signer, landing.State)
	if !ok {
		return "", refusedState
	}
	estate, err := e.fleetReady()
	if err != nil {
		return server, err
	}
	// DETACHED AND BOUNDED: the browser may go away while the grant is being
	// sealed, and a grant the vendor issued and the engine dropped is one
	// nobody can use or revoke.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget+2*time.Minute)
	defer cancel()
	session, err := e.spendSignIn(ctx, server, id)
	if err != nil {
		return server, err
	}
	fail := func(reason, remedy string) (string, error) {
		e.endSignIn(ctx, session, coord.MCPSignInFailed, reason)
		log.WarnContext(ctx, "mcp_sign_in_failed", "server", server, "sign_in", id.String(),
			"reason", reason)
		return server, &MCPSignInRefusal{Status: http.StatusBadGateway, Reason: reason, Remedy: remedy}
	}
	if landing.Error != "" {
		said := vendorSaid(landing.Error, landing.Description)
		if landing.Error == "access_denied" {
			return fail("the sign-in was declined at the vendor ("+said+")",
				"Start it again from the server's page if that was not meant")
		}
		return fail("the vendor refused the sign-in ("+said+")",
			"Start it again from the server's page, or ask the vendor")
	}
	plain, err := e.cipher.Decrypt(session.Sealed, signInAAD(session.ID))
	if err != nil {
		return fail("the sign-in could not be opened on this node", "Start it again from the server's page")
	}
	var sealed signInSealed
	if json.Unmarshal([]byte(plain), &sealed) != nil {
		return fail("the sign-in could not be read on this node", "Start it again from the server's page")
	}
	ep := &sealed.Endpoints
	// THE ISSUER THE CODE CAME FROM (RFC 9207): a code another
	// authorization server issued is never sent to this one's endpoint.
	switch {
	case landing.Issuer != "" && strings.TrimSuffix(landing.Issuer, "/") != strings.TrimSuffix(ep.Issuer, "/"):
		return fail("the sign-in came back from an authorization server other than the one it was "+
			"started at", "Start it again from the server's page")
	case landing.Issuer == "" && ep.IssuerInResponse:
		return fail("the authorization server did not name itself in its answer, which it says it "+
			"always does", "Start it again from the server's page")
	case strings.TrimSpace(landing.Code) == "":
		return fail("the vendor sent no approval code", "Start it again from the server's page")
	}
	entry, err := e.oauthServer(server)
	if err != nil {
		return fail("the server is no longer signed in to with OAuth", "Add its oauth block, then sign in")
	}
	if entry.OAuth.ServiceIdentity() {
		// CHANGED UNDER THE SIGN-IN: a code exchanged for a server that now
		// signs in as a service identity would be a grant nothing uses.
		return fail("the server now signs in as a service identity, which nobody signs in to",
			"Nothing to do: the engine issues its tokens as it is used")
	}
	client, err := e.oauthClient(entry, sealed.Client)
	if err != nil {
		return fail(err.Error(), "Set it in Settings, Secrets, then sign in again")
	}
	tokens, err := oauth.Exchange(ctx, ep, client, landing.Code, sealed.Verifier, sealed.Redirect, e.now())
	if err != nil {
		return fail("the authorization server did not complete the sign-in: "+oauthWords(err),
			"Start it again from the server's page, or ask the vendor")
	}
	grant := sealedGrant{Grant: uuid.New(), Refresh: tokens.Refresh, Client: sealed.Client, Endpoints: *ep}
	by := iam.Actor{Name: session.By, Kind: session.ByKind, OperatorID: session.OperatorID}
	if err := e.sealGrant(ctx, estate, entry, grant, by); err != nil {
		// NOT KEPT, SO REVOKED: a grant the vendor issued and nothing holds
		// is one nobody could end otherwise.
		_, _ = oauth.Revoke(ctx, ep, client, tokens.Refresh, "refresh_token")
		return fail("the grant could not be kept: "+err.Error(), "Start it again from the server's page")
	}
	record := coord.MCPGrant{
		Server: server, Grant: grant.Grant, Client: string(sealed.Client.Kind), ClientID: sealed.Client.ID,
		Issuer: ep.Issuer, Resource: ep.Resource, Scopes: grantedScopes(tokens.Scopes, sealed.Scopes),
		By: session.By, ByKind: session.ByKind, OperatorID: session.OperatorID, At: e.now(),
		Expiry: instantOrNil(tokens.RefreshExpiry),
	}
	if err := e.putGrant(ctx, record); err != nil {
		return fail("the grant is sealed and could not be recorded: "+err.Error(),
			"Start it again from the server's page")
	}
	e.endSignIn(ctx, session, coord.MCPSignInApproved, "")
	log.InfoContext(ctx, "mcp_signed_in", "server", server, "sign_in", id.String(),
		"issuer", redact.URL(ep.Issuer), "by", session.By, "scopes", strings.Join(record.Scopes, " "))
	e.reloadForGrant(ctx, server, "reload: signed in to MCP server "+server, by)
	return server, nil
}

// openState verifies a state's signature and expiry and reads its fields.
func openState(signer *runtoken.Signer, state string) (string, uuid.UUID, bool) {
	server, raw, ok := strings.Cut(signer.Validate(strings.TrimSpace(state)), stateSep)
	if !ok || server == "" {
		return "", uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", uuid.Nil, false
	}
	return server, id, true
}

// spendSignIn moves the pending sign-in id on to completing, at the version it
// read: the one way a state is spent, on any node.
func (e *Engine) spendSignIn(ctx context.Context, server string, id uuid.UUID) (coord.MCPSignIn, error) {
	held, found, err := e.backends.Fleet.MCPSignIn(ctx, server)
	switch {
	case err != nil:
		return coord.MCPSignIn{}, &MCPSignInRefusal{Status: http.StatusServiceUnavailable,
			Reason: "this engine could not confirm the sign-in was unused",
			Remedy: "Go back and approve it again in a moment"}
	case !found || held.ID != id || held.Status != coord.MCPSignInPending || e.now().After(held.ExpiresAt):
		return coord.MCPSignIn{}, refusedState
	}
	held.Status = coord.MCPSignInCompleting
	spent, ok, err := e.backends.Fleet.UpdateMCPSignIn(ctx, held)
	if err != nil || !ok {
		return coord.MCPSignIn{}, refusedState
	}
	return spent, nil
}

// endSignIn records how a sign-in ended, for the page that polls it. The
// sealed half goes with it: nothing needs the verifier after this.
func (e *Engine) endSignIn(ctx context.Context, s coord.MCPSignIn, status coord.MCPSignInStatus, detail string) {
	s.Status, s.Detail, s.Sealed = status, textcut.Bytes(detail, coord.MaxMCPGrantWords), ""
	if _, ok, err := e.backends.Fleet.UpdateMCPSignIn(ctx, s); err != nil || !ok {
		log.WarnContext(ctx, "mcp_sign_in_end_not_recorded", "server", s.Server,
			"sign_in", s.ID.String(), "status", string(status), "error", err,
			"detail", "the page polling it reads the sign-in as it last stood until it lapses")
	}
}

// oauthClient is the client a grant was made as, the company's own client's
// secret resolved from entry, the server's configuration, now.
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

// sealGrant seals grant as the server's, over whatever grant it had, whose
// refresh token is revoked at its vendor.
func (e *Engine) sealGrant(ctx context.Context, estate *fleetsecrets.Estate, entry *config.MCPServer,
	grant sealedGrant, by iam.Actor) error {

	server := entry.Name
	value, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("encode the grant: %w", err)
	}
	author := secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
	name := grantRow(server)
	for range maxRetryRounds {
		held, found, err := estate.Held(ctx, name)
		if err != nil {
			return err
		}
		var wrote bool
		if found {
			e.revokeHeld(ctx, entry, held.Value)
			wrote, err = estate.Swap(ctx, name, string(value), held.Version, author, secrets.SourceMCPSignIn, e.now())
		} else {
			wrote, err = estate.Create(ctx, name, string(value), author, secrets.SourceMCPSignIn, e.now())
		}
		if err != nil {
			return err
		}
		if wrote {
			return nil
		}
	}
	return errors.New("the server's grant kept changing")
}

// putGrant records g as the server's grant, over whatever it had.
func (e *Engine) putGrant(ctx context.Context, g coord.MCPGrant) error {
	fleet := e.backends.Fleet
	for range maxRetryRounds {
		held, found, err := fleet.MCPGrant(ctx, g.Server)
		if err != nil {
			return err
		}
		var wrote bool
		if found {
			g.Version = held.Version
			_, wrote, err = fleet.UpdateMCPGrant(ctx, g)
		} else {
			_, wrote, err = fleet.CreateMCPGrant(ctx, g)
		}
		if err != nil {
			return err
		}
		if wrote {
			return nil
		}
	}
	return errors.New("the server's grant kept changing")
}

// grantedScopes is what a grant was granted: what the token answer said, else
// what was asked for (RFC 6749 reads silence as the request granted).
func grantedScopes(granted, asked []string) []string {
	if len(granted) > 0 {
		return granted
	}
	return append([]string{}, asked...)
}

// instantOrNil is t, or nil for the zero instant.
func instantOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// vendorSaid is a vendor's error code and description from the landing's
// query, as a page and a record may hold them: one line, bounded, redacted.
func vendorSaid(code, description string) string {
	said := strings.Join(strings.Fields(code), " ")
	if d := strings.Join(strings.Fields(description), " "); d != "" {
		said += ": " + d
	}
	return redact.Secrets(textcut.Bytes(said, 512))
}

// oauthWords is an OAuth error as a record or a page holds it: the vendor's
// own words for a refusal, else the error, bounded.
func oauthWords(err error) string {
	var refused *oauth.Error
	if errors.As(err, &refused) {
		return vendorSaid(cmpOr(refused.Code, fmt.Sprintf("HTTP %d", refused.Status)), refused.Description)
	}
	return redact.Secrets(textcut.Bytes(err.Error(), 512))
}

// reloadForGrant re-activates the revision so every node reads the server's
// grant afresh and starts or stops it, after a sign-in or a sign-out.
func (e *Engine) reloadForGrant(ctx context.Context, server, summary string, by iam.Actor) {
	e.forgetSource(server)
	writer := e.configWriterOrNil()
	if writer == nil {
		log.WarnContext(ctx, "mcp_grant_not_republished", "server", server,
			"detail", "this node cannot re-activate the revision, so each node takes the new "+
				"grant at its next apply; POST /config/reload")
		return
	}
	if err := writer.Reload(ctx, summary, by); err != nil {
		log.ErrorContext(ctx, "mcp_grant_not_republished", "server", server, "error", err,
			"detail", "the grant is kept, and each node takes it at its next apply; POST /config/reload")
	}
}

// MCPSignOut is what signing out of a server did.
type MCPSignOut struct {
	// Revocation is what the vendor was asked: [RevokedAtVendor] where it
	// revoked the grant, [RevokeNotOffered] where it publishes no
	// revocation endpoint (the grant is forgotten, and lapses at the
	// vendor), [RevokeFailed] where it could not be asked or refused (the
	// grant is forgotten all the same).
	Revocation MCPRevocation
	// Said is the vendor's words where the revocation failed.
	Said string
}

// MCPRevocation is what a vendor was asked when a grant was given up.
type MCPRevocation string

const (
	// RevokedAtVendor is a grant its vendor revoked.
	RevokedAtVendor MCPRevocation = "revoked"
	// RevokeNotOffered is a vendor that publishes no revocation endpoint:
	// the grant is forgotten here and lapses there.
	RevokeNotOffered MCPRevocation = "not_offered"
	// RevokeFailed is a vendor that could not be asked, or refused.
	RevokeFailed MCPRevocation = "failed"
)

// MCPRevocations is every outcome, for the gates holding a client's copy.
var MCPRevocations = []MCPRevocation{RevokedAtVendor, RevokeNotOffered, RevokeFailed}

// SignOutMCPServer gives up the server's grant: revoked at its vendor where it
// offers that (RFC 7009), forgotten here, and the revision re-activated so
// every node stops sending its tokens. [ErrMCPNotSignedIn] where nobody signed
// in.
func (e *Engine) SignOutMCPServer(ctx context.Context, name string, by iam.Actor) (MCPSignOut, error) {
	entry, err := e.oauthServer(name)
	if err != nil {
		return MCPSignOut{}, err
	}
	out, held, err := e.RevokeMCPGrant(ctx, entry)
	if err != nil {
		return MCPSignOut{}, err
	}
	if !held {
		return MCPSignOut{}, ErrMCPNotSignedIn
	}
	log.InfoContext(ctx, "mcp_signed_out", "server", name, "by", by.Name,
		"revocation", string(out.Revocation))
	e.reloadForGrant(ctx, name, "reload: signed out of MCP server "+name, by)
	return out, nil
}

// RevokeMCPGrant revokes the grant of entry's server at its vendor where it
// offers that, and deletes it here (the sealed row, the public record and any
// sign-in in progress), reporting whether there was one. It re-activates
// nothing: a removal of the server is itself a revision, and a sign-out
// re-activates after it ([Engine.SignOutMCPServer]).
//
// ENTRY IS THE SERVER AS IT WAS CONFIGURED, which a removal passes from the
// document before it: the company's own client authenticates its revocation
// with the secret that entry names, which the configuration no longer does.
func (e *Engine) RevokeMCPGrant(ctx context.Context, entry *config.MCPServer) (MCPSignOut, bool, error) {
	server := entry.Name
	estate, err := e.fleetReady()
	if err != nil {
		return MCPSignOut{}, false, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget+time.Minute)
	defer cancel()
	fleet := e.backends.Fleet
	out := MCPSignOut{Revocation: RevokeNotOffered}
	held, sealedFound, err := estate.Held(ctx, grantRow(server))
	if err != nil {
		return MCPSignOut{}, false, fmt.Errorf("engine: read the server's grant: %w", err)
	}
	if sealedFound {
		out = e.revokeHeld(ctx, entry, held.Value)
		if _, gone := estate.Delete(ctx, grantRow(server)); gone != nil {
			return MCPSignOut{}, false, fmt.Errorf("engine: delete the server's grant: %w", gone)
		}
	}
	record, recordFound, err := fleet.MCPGrant(ctx, server)
	if err != nil {
		return MCPSignOut{}, false, fmt.Errorf("engine: read the server's grant record: %w", err)
	}
	if recordFound {
		if _, err := fleet.DeleteMCPGrant(ctx, server, record.Version); err != nil {
			return MCPSignOut{}, false, fmt.Errorf("engine: delete the server's grant record: %w", err)
		}
	}
	if session, found, err := fleet.MCPSignIn(ctx, server); err == nil && found {
		_, _ = fleet.DeleteMCPSignIn(ctx, server, session.Version)
	}
	e.forgetSource(server)
	return out, sealedFound || recordFound, nil
}

// revokeHeld revokes the refresh token in a sealed grant at its vendor, and
// says what came of it.
func (e *Engine) revokeHeld(ctx context.Context, entry *config.MCPServer, value string) MCPSignOut {
	server := entry.Name
	var grant sealedGrant
	if err := json.Unmarshal([]byte(value), &grant); err != nil || grant.Refresh == "" {
		return MCPSignOut{Revocation: RevokeFailed, Said: "the kept grant could not be read"}
	}
	client := oauth.Client{Kind: grant.Client.Kind, ID: grant.Client.ID, Secret: grant.Client.Secret}
	if c, err := e.oauthClient(entry, grant.Client); err == nil {
		client = c
	}
	revoked, err := oauth.Revoke(ctx, &grant.Endpoints, client, grant.Refresh, "refresh_token")
	switch {
	case err != nil:
		log.WarnContext(ctx, "mcp_grant_not_revoked", "server", server, "error", oauthWords(err),
			"detail", "the grant is forgotten here; it lapses at the vendor, or is revoked there by hand")
		return MCPSignOut{Revocation: RevokeFailed, Said: oauthWords(err)}
	case !revoked:
		return MCPSignOut{Revocation: RevokeNotOffered}
	}
	return MCPSignOut{Revocation: RevokedAtVendor}
}

// HeldMCPGrant reports whether the server has a grant, for a removal's dry
// run: the record every node reads, or the sealed row where only that is left.
func (e *Engine) HeldMCPGrant(ctx context.Context, server string) (bool, error) {
	estate, err := e.fleetReady()
	if err != nil {
		return false, err
	}
	if _, recorded, readErr := e.backends.Fleet.MCPGrant(ctx, server); readErr != nil || recorded {
		return recorded, readErr
	}
	_, found, err := estate.Held(ctx, grantRow(server))
	return found, err
}

// signInNeeded is a server held back because nobody holds a grant for it, or
// its vendor refused to renew the one it had: the outcome a pass reports in
// place of a start.
type signInNeeded struct {
	server string
	// failure is the vendor's words for the renewal it refused, "" where
	// nobody signed in.
	failure string
	// moved is a grant issued for another address than the one the server
	// is configured at now, which its tokens are never sent to.
	moved string
}

func (s *signInNeeded) Error() string {
	if s.moved != "" {
		return fmt.Sprintf("%s is not started: its sign-in was made for %s, which is not the server's "+
			"address now; sign in again from its page", s.server, s.moved)
	}
	if s.failure != "" {
		return fmt.Sprintf("%s is not started: its vendor refused to renew the sign-in (%s); sign in "+
			"again from its page", s.server, s.failure)
	}
	return s.server + " is not started: nobody has signed in to it; sign in from its page"
}

func (s *signInNeeded) Unwrap() error { return mcp.ErrSignInNeeded }

// bearerFor is the token source a start of server sends with, or why it may
// not start: nobody signed in, or the grant's renewal was refused for good.
//
// READ AFRESH at each pass, from the leader, so the pass a sign-in's
// re-activation runs reads the grant it wrote; a read that fails takes the
// grant this node last read, since a store that blinks is not a sign-out.
func (e *Engine) bearerFor(ctx context.Context, server string) (mcp.BearerSource, *signInNeeded) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, &signInNeeded{server: server}
	}
	g, found, err := e.backends.Fleet.MCPGrant(ctx, server)
	e.oauth.mu.Lock()
	defer e.oauth.mu.Unlock()
	switch {
	case err != nil:
		log.WarnContext(ctx, "mcp_grant_unread", "server", server, "error", err,
			"detail", "the grant this node last read is used")
		last := e.oauth.last[server]
		if last == nil {
			return nil, &signInNeeded{server: server}
		}
		g, found = *last, true
	case found:
		if e.oauth.last == nil {
			e.oauth.last = map[string]*coord.MCPGrant{}
		}
		e.oauth.last[server] = &g
	default:
		delete(e.oauth.last, server)
	}
	if !found {
		delete(e.oauth.sources, server)
		return nil, &signInNeeded{server: server}
	}
	if g.Failed() {
		delete(e.oauth.sources, server)
		return nil, &signInNeeded{server: server, failure: g.Failure}
	}
	if moved := e.grantMoved(server, g); moved != nil {
		delete(e.oauth.sources, server)
		return nil, moved
	}
	if s := e.oauth.sources[server]; s != nil && s.grant == g.Grant {
		return s, nil
	}
	if e.oauth.sources == nil {
		e.oauth.sources = map[string]*oauthSource{}
	}
	s := &oauthSource{engine: e, server: server, grant: g.Grant}
	e.oauth.sources[server] = s
	return s, nil
}

// grantMoved is why g may not be used for server, nil where it may: the
// address server is configured at now (resolved) is not the resource g was
// issued for ([mcp.ResourceServes]).
//
// THE GRANT GOES WITH ITS RESOURCE, NOT ITS NAME. A server's entry is
// rewritten by any configuration write, and the transport sends a source's
// token to whatever address the spec names: a url moved to another origin
// would otherwise carry the company's token for one vendor to whoever answers
// there. A server whose address moved is held back for a sign-in at the new
// one.
func (e *Engine) grantMoved(server string, g coord.MCPGrant) *signInNeeded {
	entry, err := e.oauthServer(server)
	if err != nil {
		return &signInNeeded{server: server}
	}
	if mcp.ResourceServes(g.Resource, e.resolver().Value(entry.URL)) {
		return nil
	}
	return &signInNeeded{server: server, moved: redact.URL(g.Resource)}
}

// forgetSource drops this node's token source for server, and the token it
// holds.
func (e *Engine) forgetSource(server string) {
	e.oauth.mu.Lock()
	defer e.oauth.mu.Unlock()
	delete(e.oauth.sources, server)
	delete(e.oauth.last, server)
}

// oauthSource is one server's access token on this node, under one grant: the
// [mcp.BearerSource] every start of the server under that grant sends with.
// The token is held here and nowhere else.
//
// ONE SOURCE FOR BOTH GRANTS, so a server is sent its token, renews it ahead
// of its end and renews it once on a 401 by the same path whichever way it is
// issued: by renewing the company's grant ([oauthSource.renewGrant]), or by
// signing in as the company's service identity ([serviceIdentity.issue]).
type oauthSource struct {
	engine *Engine
	server string
	// grant is the company's grant the source renews, uuid.Nil for a
	// service identity's source.
	grant uuid.UUID
	// service is the service identity the source signs in as, nil for a
	// grant's source (mcpserviceidentity.go).
	service *serviceIdentity

	mu     sync.Mutex
	token  string
	expiry time.Time
	minted time.Time
}

// Token is the access token to send now, renewed first where it is about to
// run out ([oauthRenewAhead]).
func (s *oauthSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.engine.now()
	if s.token != "" && (s.expiry.IsZero() || now.Before(s.expiry.Add(-oauthRenewAhead))) {
		return s.token, nil
	}
	return s.renewLocked(ctx)
}

// Rejected renews the token a server refused, once: a token renewed since the
// refused one was sent is answered as it is, and so is one issued within
// [oauthFreshWindow], which a renewal would not change.
func (s *oauthSource) Rejected(ctx context.Context, refused string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && (refused != s.token || s.engine.now().Sub(s.minted) < oauthFreshWindow) {
		return s.token, nil
	}
	return s.renewLocked(ctx)
}

// renewLocked is issued a new access token, as a service identity or by
// renewing the grant, and holds it. Called with s.mu held, which is what
// keeps two requests on this node from asking at once.
func (s *oauthSource) renewLocked(ctx context.Context) (string, error) {
	now := s.engine.now()
	issue := s.renewGrant
	if s.service != nil {
		issue = s.service.issue
	}
	tokens, err := issue(ctx)
	if err != nil {
		return "", err
	}
	s.token, s.expiry, s.minted = tokens.Access, tokens.Expiry, now
	return s.token, nil
}

// renewGrant renews the company's grant under the fleet's renewal lease: see
// the file's comment. Called with s.mu held.
func (s *oauthSource) renewGrant(ctx context.Context) (oauth.Tokens, error) {
	e := s.engine
	estate, err := e.fleetReady()
	if err != nil {
		return oauth.Tokens{}, err
	}
	fleet := e.backends.Fleet
	record, found, err := fleet.MCPGrant(ctx, s.server)
	switch {
	case err != nil:
		return oauth.Tokens{}, fmt.Errorf("engine: read %s's grant: %w", s.server, err)
	case !found || record.Grant != s.grant:
		return oauth.Tokens{}, &signInNeeded{server: s.server}
	case record.Failed():
		// REFUSED FOR GOOD ALREADY, on this node or another: the vendor is
		// not asked again until somebody signs in.
		return oauth.Tokens{}, &signInNeeded{server: s.server, failure: record.Failure}
	}
	if moved := e.grantMoved(s.server, record); moved != nil {
		return oauth.Tokens{}, moved
	}
	release, err := e.holdRenewal(ctx, s.server)
	if err != nil {
		return oauth.Tokens{}, err
	}
	defer release()
	held, found, err := estate.Held(ctx, grantRow(s.server))
	if err != nil {
		return oauth.Tokens{}, fmt.Errorf("engine: read %s's grant: %w", s.server, err)
	}
	var grant sealedGrant
	if !found || json.Unmarshal([]byte(held.Value), &grant) != nil || grant.Grant != s.grant {
		return oauth.Tokens{}, &signInNeeded{server: s.server}
	}
	entry, err := e.oauthServer(s.server)
	if err != nil {
		return oauth.Tokens{}, &signInNeeded{server: s.server}
	}
	client, err := e.oauthClient(entry, grant.Client)
	if err != nil {
		return oauth.Tokens{}, fmt.Errorf("engine: renew %s's sign-in: %w", s.server, err)
	}
	now := e.now()
	tokens, err := oauth.Refresh(ctx, &grant.Endpoints, client, grant.Refresh, now)
	var refused *oauth.Error
	if errors.As(err, &refused) && refused.Definitive() {
		e.recordRenewalRefused(ctx, record, oauthWords(err))
		return oauth.Tokens{}, &signInNeeded{server: s.server, failure: oauthWords(err)}
	}
	if err != nil {
		log.WarnContext(ctx, "mcp_oauth_renewal_failed", "server", s.server, "error", oauthWords(err),
			"detail", "the next request asks again")
		return oauth.Tokens{}, fmt.Errorf("engine: renew %s's sign-in: %s", s.server, oauthWords(err))
	}
	if tokens.Refresh != "" && tokens.Refresh != grant.Refresh {
		grant.Refresh = tokens.Refresh
		value, err := json.Marshal(grant)
		if err == nil {
			var wrote bool
			wrote, err = estate.Swap(ctx, grantRow(s.server), string(value), held.Version,
				secrets.Author{Name: e.selfNode(), Kind: string(iam.ActorSystem)}, secrets.SourceMCPSignIn, now)
			if err == nil && !wrote {
				err = errors.New("the kept grant moved while it was renewed")
			}
		}
		if err != nil {
			// THE TOKEN IS STILL GOOD for this node's requests; what is lost
			// is the next renewal, which then asks for a sign-in.
			log.ErrorContext(ctx, "mcp_oauth_rotation_not_kept", "server", s.server, "error", err,
				"detail", "the vendor rotated the refresh token and the new one could not be kept; "+
					"the next renewal asks for a sign-in")
		}
	}
	if expiry := instantOrNil(tokens.RefreshExpiry); expiry != nil {
		e.recordGrantExpiry(ctx, record, *expiry)
	}
	return tokens, nil
}

// holdRenewal takes the server's renewal lease, waiting up to
// [oauthRenewalWait] for another node's renewal to end, and answers its
// release.
func (e *Engine) holdRenewal(ctx context.Context, server string) (func(), error) {
	resource := oauthRenewalClass.Resource(server)
	owner := e.selfNode() + "/" + uuid.NewString()
	deadline := e.now().Add(oauthRenewalWait)
	for {
		lease, _, err := e.backends.Coord.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: oauthRenewalLeaseTTL, Ungated: true})
		if err != nil {
			return nil, fmt.Errorf("engine: take %s's renewal lease: %w", server, err)
		}
		if lease != nil {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget)
				defer cancel()
				if _, err := e.backends.Coord.Release(releaseCtx, resource, owner, lease.Epoch); err != nil {
					log.WarnContext(ctx, "mcp_oauth_renewal_lease_kept", "server", server, "error", err,
						"detail", "it lapses on its own")
				}
			}, nil
		}
		if !e.now().Before(deadline) {
			return nil, fmt.Errorf("engine: another node is renewing %s's sign-in; the next request asks again",
				server)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(oauthRenewalPoll):
		}
	}
}

// recordRenewalRefused marks the server's grant as refused for good, in its
// vendor's words, so no node asks the vendor again until somebody signs in.
func (e *Engine) recordRenewalRefused(ctx context.Context, record coord.MCPGrant, said string) {
	now := e.now()
	record.Failure, record.FailedAt = textcut.Bytes("the vendor refused to renew the sign-in: "+said,
		coord.MaxMCPGrantWords), &now
	if _, ok, err := e.backends.Fleet.UpdateMCPGrant(ctx, record); err != nil || !ok {
		log.WarnContext(ctx, "mcp_oauth_refusal_not_recorded", "server", record.Server, "error", err,
			"detail", "the next renewal on any node asks the vendor again")
	}
	log.ErrorContext(ctx, "mcp_oauth_renewal_refused", "server", record.Server, "said", said,
		"detail", "the server needs somebody to sign in to it again")
}

// recordGrantExpiry records when the server's grant now runs out, where its
// vendor said and it moved.
func (e *Engine) recordGrantExpiry(ctx context.Context, record coord.MCPGrant, expiry time.Time) {
	if record.Expiry != nil && record.Expiry.Equal(expiry) {
		return
	}
	record.Expiry = &expiry
	if _, _, err := e.backends.Fleet.UpdateMCPGrant(ctx, record); err != nil {
		log.WarnContext(ctx, "mcp_oauth_expiry_not_recorded", "server", record.Server, "error", err)
	}
}

// testBearer is the token source a test of the saved server sends with: its
// grant's, or nil where nobody holds one, so the test walks to where the
// server asks for a sign-in and says so.
func (e *Engine) testBearer(ctx context.Context, server string) mcp.BearerSource {
	bearer, missing := e.bearerFor(ctx, server)
	if missing != nil {
		return nil
	}
	return bearer
}

// MCPGrants is every MCP server's grant as the fleet records it, for the
// servers' answers: the public half, never a token.
func (e *Engine) MCPGrants(ctx context.Context) ([]coord.MCPGrant, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, nil
	}
	return e.backends.Fleet.ListMCPGrants(ctx)
}

// MCPSignInAddresses are the callback a vendor sends a person back to and the
// engine's client metadata document, each "" where the engine has no public
// address.
func (e *Engine) MCPSignInAddresses() (string, string) {
	return e.CallbackURL(), e.ClientMetadataURL()
}

// RevokeMCPServerGrant is [Engine.RevokeMCPGrant] in the plain words a removal
// of the server answers (configapi.Grants): what the vendor was asked, its
// words where it refused, and whether there was a grant.
func (e *Engine) RevokeMCPServerGrant(ctx context.Context, entry *config.MCPServer) (string, string, bool, error) {
	out, held, err := e.RevokeMCPGrant(ctx, entry)
	if err != nil {
		return "", "", false, err
	}
	return string(out.Revocation), out.Said, held, nil
}
