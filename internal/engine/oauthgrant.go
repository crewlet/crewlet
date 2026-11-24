package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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

// THE COMPANY'S OAUTH GRANTS, ONE IMPLEMENTATION FOR EVERY FAMILY (ADR-0075,
// ADR-0178): an MCP server's (mcpoauth.go) and an integration's consent grant
// (consentgrant.go) are each a named grant of a [coord.GrantFamily]. Its
// refresh token is sealed at `<owner>/<name>/refresh` ([fleetsecrets.Estate]);
// its public half is a [coord.OAuthGrant]; its access token lives only in each
// node's memory ([oauthSource]). A renewal holds the family's lease and writes a
// rotated token at the version it read. A sign-in's state is its key and id
// signed under the fleet keyring ([oauthDomain]), all the unauthenticated
// callback trusts; the PKCE verifier stays sealed on its [coord.OAuthSignIn],
// spent once. What differs by family is a [grantKind].

// oauthDomain separates a sign-in's state from every other token the fleet
// keyring signs.
const oauthDomain = "mcp-oauth"

// The bounds of a sign-in and a renewal.
const (
	// SignInTTL is how long a sign-in's state is accepted: a consent page,
	// an account chosen and a second factor. Fifteen minutes, as a Slack
	// install link's.
	SignInTTL = 15 * time.Minute

	// oauthRenewAhead is how long before an access token runs out the engine
	// renews it: a token is checked when a request arrives, so this covers
	// clock skew with the vendor and a request in flight across the expiry.
	oauthRenewAhead = 2 * time.Minute

	// oauthFreshWindow is how long a just-issued token is sent again rather
	// than renewed when refused: so young, it was refused for a reason a
	// renewal does not change (a scope, a revoked grant), and renewing on
	// every refusal would spend the refresh token per request. It bounds
	// renewals to two a minute per node and grant.
	oauthFreshWindow = 30 * time.Second

	// oauthRenewalLeaseTTL bounds the renewal lease: it outlasts one token
	// request ([oauth]'s 15-second budget) and two store writes, and a renewer
	// that dies holding it delays the next renewal at most that long.
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

// grantKind is what one family of grants adds to the shared machinery.
type grantKind interface {
	// family is where its rows, records and leases are filed, and source
	// what its sealed rows are stamped with.
	family() coord.GrantFamily
	source() secrets.Source
	// key is a grant's name across every family: a node's token sources
	// and a sign-in's state carry it. It parses back through [kindOfKey].
	key(name string) string
	// page is where a person signs in again, for a remedy's sentence, and
	// title what a person calls the grant's subject.
	page() string
	title(name string) string
	// usable is why the grant may not be finished, renewed or sent under
	// the running configuration, nil where it may.
	usable(e *Engine, name string, g *coord.OAuthGrant) *signInNeeded
	// client is the client a grant was made as, the company's own client's
	// secret resolved from the running configuration.
	client(e *Engine, name string, c grantClient) (oauth.Client, error)
	// accept checks an exchange's tokens against the scopes asked for and
	// answers the scopes to record.
	accept(tokens oauth.Tokens, asked []string) ([]string, error)
	// revokesReplaced is whether a sign-in that replaces a grant revokes the
	// one it replaces at the vendor, and keepFresh why a token an exchange
	// issued and the engine refused is left unrevoked ("" to revoke it).
	revokesReplaced() bool
	keepFresh(ctx context.Context, e *Engine, name, clientID string) string
	// refused is the words recorded for a renewal refused for good.
	refused(g coord.OAuthGrant, said string, now time.Time) string
}

// kindOfKey is the kind and name a key ([grantKind.key]) names.
func kindOfKey(key string) (grantKind, string) {
	if name, ok := strings.CutPrefix(key, consentKeyPrefix); ok {
		return consentGrants{}, name
	}
	return mcpGrants{}, key
}

// SignInRefusal is why a sign-in could not be started or finished, in words a
// person acts on. It never carries a code, a token or the verifier.
type SignInRefusal struct {
	// Status is the HTTP status the route or the landing answers with.
	Status int
	// Reason is what went wrong, and Remedy what to do about it.
	Reason, Remedy string
}

func (r *SignInRefusal) Error() string { return "engine: " + r.Reason + ". " + r.Remedy }

// refusedState is the one answer for a state forged, edited, lapsed, signed
// under a retired key, never minted or already spent: each needs a new
// sign-in, and telling a caller which tells an attacker too.
var refusedState = &SignInRefusal{
	Status: http.StatusBadRequest,
	Reason: "this sign-in link is not one this engine issued, has expired, or was already used",
	Remedy: "Start the sign-in again from its page in Settings",
}

// SignInLanding is what the vendor sent the person's browser back with.
type SignInLanding struct {
	State, Code, Issuer string
	// Error and Description are the vendor's refusal (RFC 6749, 4.1.2.1).
	Error, Description string
}

// SignInLanded is a finished sign-in: what it signed in to.
type SignInLanded struct {
	// Name is the grant's name: an MCP server's, or an integration kind;
	// Title is what a person calls it.
	Name, Title string
	// Integration is whether it is an integration's consent grant.
	Integration bool
}

// signInSealed is what a sign-in's callback finishes it with, sealed on its
// record ([coord.OAuthSignIn.Sealed]).
type signInSealed struct {
	Verifier  string             `json:"verifier"`
	Client    grantClient        `json:"client"`
	Endpoints mcp.OAuthEndpoints `json:"endpoints"`
	Scopes    []string           `json:"scopes"`
	Redirect  string             `json:"redirect"`
}

// grantClient is the client a grant was made as. The company's own client's
// secret is never kept here but resolved from its `${VAR}` at each use, so a
// rotated secret is the one sent.
type grantClient struct {
	Kind   catalogue.OAuthClient `json:"kind"`
	ID     string                `json:"id"`
	Secret string                `json:"secret,omitempty"`
}

// sealedGrant is a grant as its estate row holds it.
type sealedGrant struct {
	Grant     uuid.UUID          `json:"grant"`
	Refresh   string             `json:"refresh"`
	Client    grantClient        `json:"client"`
	Endpoints mcp.OAuthEndpoints `json:"endpoints"`
}

// grantRow is a grant's estate row: the engine's own name for it.
func grantRow(f coord.GrantFamily, name string) string { return f.Owner + "/" + name + "/refresh" }

// signInAAD binds a sign-in's sealed half to the sign-in and its family, so an
// envelope moved to another record fails to open.
func signInAAD(f coord.GrantFamily, id uuid.UUID) string {
	return secrets.AADForVar(strings.ReplaceAll(f.SignInClass, "_", "-") + "/" + id.String())
}

// oauthSigner is the fleet keyring's signer for sign-in states.
func (e *Engine) oauthSigner() (*runtoken.Signer, error) {
	e.oauth.signerOnce.Do(func() {
		e.oauth.signer, e.oauth.signerErr = runtoken.New(runtoken.Options{
			Domain: oauthDomain, Material: tokenMaterial(e.boot), Now: e.now})
	})
	return e.oauth.signer, e.oauth.signerErr
}

// oauthState is this node's part in the company's grants: the signer, and the
// access token source of each grant, by key.
type oauthState struct {
	signerOnce sync.Once
	signer     *runtoken.Signer
	signerErr  error

	mu sync.Mutex
	// sources are each grant's token source; a new grant is a new source.
	sources map[string]*oauthSource
	// last are each grant as this node last read it, for a read that
	// fails: a store that blinks is not a sign-out.
	last map[string]*coord.OAuthGrant

	// consentAt replaces Google's consent endpoints with a test's fake
	// vendor; nil everywhere else.
	consentAt *mcp.OAuthEndpoints
}

// fleetReady is the stores a sign-in reads and writes, or why there are none.
func (e *Engine) fleetReady() (*fleetsecrets.Estate, error) {
	if e.backends == nil || e.backends.Fleet == nil || e.cipher == nil {
		return nil, errors.New("engine: this engine holds no fleet store and keyring to keep " +
			"a sign-in in; it was not built by engine.New")
	}
	return fleetsecrets.New(e.backends.Fleet, e.cipher).Estate(), nil
}

// signInStart is what a family's start resolved: where to sign in and as what.
type signInStart struct {
	kind     grantKind
	name     string
	ep       *mcp.OAuthEndpoints
	client   grantClient
	scopes   []string
	redirect string
	// extra are the vendor's own authorization parameters.
	extra url.Values
}

// SignInStarted is a sign-in begun: the address the person's browser is sent
// to, and the record the page polls.
type SignInStarted struct {
	SignIn       coord.OAuthSignIn
	AuthorizeURL string
}

// startSignIn begins the sign-in st resolved, as by: the proof key, the
// sealed record, the signed state and the authorization address.
func (e *Engine) startSignIn(ctx context.Context, st signInStart, by iam.Actor) (SignInStarted, error) {
	if refusal := grantUnkeepable(st); refusal != nil {
		return SignInStarted{}, refusal
	}
	signer, err := e.oauthSigner()
	if err != nil {
		return SignInStarted{}, fmt.Errorf("engine: sign a sign-in's state: %w", err)
	}
	key, err := oauth.NewPKCE()
	if err != nil {
		return SignInStarted{}, err
	}
	fam := st.kind.family()
	now := e.now()
	record := coord.OAuthSignIn{
		ID: uuid.New(), Name: st.name, Status: coord.OAuthSignInPending,
		StartedAt: now, ExpiresAt: now.Add(SignInTTL),
		By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
	}
	sealed, err := json.Marshal(signInSealed{Verifier: key.Verifier, Client: st.client,
		Endpoints: *st.ep, Scopes: st.scopes, Redirect: st.redirect})
	if err != nil {
		return SignInStarted{}, fmt.Errorf("engine: encode a sign-in: %w", err)
	}
	if record.Sealed, err = e.cipher.Encrypt(string(sealed), signInAAD(fam, record.ID)); err != nil {
		return SignInStarted{}, fmt.Errorf("engine: seal a sign-in: %w", err)
	}
	if len(record.Sealed) > coord.MaxOAuthSealedBytes {
		// WHAT THE CALLBACK NEEDS rides the sign-in's record, which the
		// fleet holds to a bound: an authorization server whose addresses
		// and lists outgrow it is refused here, before anybody consents.
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict,
			Reason: fmt.Sprintf("%s's authorization server (%s) describes itself in more than the "+
				"%d bytes a sign-in keeps for its callback", st.name, redact.URL(st.ep.Issuer),
				coord.MaxOAuthSealedBytes),
			Remedy: "Ask the vendor; nothing was signed in"}
	}
	// THE LINK IS MADE BEFORE THE SIGN-IN IS STORED (under the id the store
	// keeps), so a refused state or authorize URL leaves the earlier sign-in,
	// not a pending one nobody could finish in its place.
	state, err := signer.Mint(st.kind.key(st.name)+stateSep+record.ID.String(), SignInTTL)
	if err != nil {
		return SignInStarted{}, fmt.Errorf("engine: the state of %s's sign-in: %w", st.name, err)
	}
	authorize, err := oauth.AuthorizeURL(st.ep, oauth.Client{Kind: st.client.Kind, ID: st.client.ID},
		st.redirect, state, key.Challenge, st.scopes, st.extra)
	if err != nil {
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict,
			Reason: err.Error(), Remedy: "Ask the vendor; nothing was signed in"}
	}
	stored, err := e.replaceSignIn(ctx, fam, record)
	if err != nil {
		return SignInStarted{}, err
	}
	log.InfoContext(ctx, fam.Owner+"_sign_in_started", "grant", st.name, "sign_in", stored.ID.String(),
		"client", string(st.client.Kind), "issuer", redact.URL(st.ep.Issuer), "by", by.Name)
	return SignInStarted{SignIn: stored, AuthorizeURL: authorize}, nil
}

// grantUnkeepable refuses a sign-in whose grant the fleet could not record,
// before anybody is sent to consent: a client id, issuer or resource past
// [coord.MaxGrantAddressBytes], or a scope it would ask for past
// [coord.MaxGrantScopeBytes]. Each comes from the configuration, the
// registration's answer or the authorization server's metadata, all known
// now; refused after consent, the grant would be sealed and unrecorded.
func grantUnkeepable(st signInStart) *SignInRefusal {
	refuse := func(field string, size, bound int) *SignInRefusal {
		return &SignInRefusal{Status: http.StatusConflict,
			Reason: fmt.Sprintf("%s's authorization server (%s) gives a %s of %d bytes, past the %d the "+
				"grant a sign-in makes can keep", st.name, redact.URL(st.ep.Issuer), field, size, bound),
			Remedy: "Ask the vendor; nothing was signed in"}
	}
	for _, f := range []struct {
		field, value string
	}{{"client id", st.client.ID}, {"issuer", st.ep.Issuer}, {"resource", st.ep.Resource}} {
		if len(f.value) > coord.MaxGrantAddressBytes {
			return refuse(f.field, len(f.value), coord.MaxGrantAddressBytes)
		}
	}
	for _, scope := range st.scopes {
		if len(scope) > coord.MaxGrantScopeBytes {
			return refuse("scope to ask for", len(scope), coord.MaxGrantScopeBytes)
		}
	}
	return nil
}

// keptScopes is what a grant's record keeps of the scopes its token answer
// granted: each the engine asked for is at most [coord.MaxGrantScopeBytes]
// ([grantUnkeepable]), so one past it is a scope the vendor added unasked,
// which nothing the engine does relies on. It is left out of the record and
// logged, rather than failing a consent the person already gave; a scope is
// never cut, since a cut scope is another scope.
func keptScopes(ctx context.Context, name string, granted []string) []string {
	kept := make([]string, 0, len(granted))
	for _, scope := range granted {
		if len(scope) > coord.MaxGrantScopeBytes {
			log.WarnContext(ctx, "oauth_scope_not_recorded", "grant", name, "bytes", len(scope),
				"bound", coord.MaxGrantScopeBytes, "detail", "the vendor granted a scope the engine "+
					"did not ask for, past what a grant's record keeps; the grant is recorded without it")
			continue
		}
		kept = append(kept, scope)
	}
	return kept
}

// stateSep joins a state's subject fields: no grant key or uuid holds it.
const stateSep = "~"

// cmpOr is the first non-empty of a and b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// replaceSignIn files record as the grant's one sign-in, replacing whatever
// sign-in it had: a second start is a person who wants to start again.
func (e *Engine) replaceSignIn(ctx context.Context, fam coord.GrantFamily, record coord.OAuthSignIn) (coord.OAuthSignIn, error) {
	fleet := e.backends.Fleet
	for range maxRetryRounds {
		held, found, err := fleet.OAuthSignIn(ctx, fam, record.Name)
		if err != nil {
			return coord.OAuthSignIn{}, fmt.Errorf("engine: read the sign-in: %w", err)
		}
		if found {
			if _, gone := fleet.DeleteOAuthSignIn(ctx, fam, record.Name, held.Version); gone != nil {
				return coord.OAuthSignIn{}, fmt.Errorf("engine: replace the sign-in: %w", gone)
			}
		}
		stored, created, err := fleet.CreateOAuthSignIn(ctx, fam, record)
		if err != nil {
			return coord.OAuthSignIn{}, fmt.Errorf("engine: record the sign-in: %w", err)
		}
		if created {
			return stored, nil
		}
	}
	return coord.OAuthSignIn{}, errors.New("engine: the sign-in kept changing; start it again")
}

// maxRetryRounds bounds a person's gesture's compare-and-set retries: it
// contends only with the few people acting on one grant at once, so eight
// lost rounds is a store not settling.
const maxRetryRounds = 8

// signInByID reads a family's sign-in by id, for the page that polls it.
func (e *Engine) signInByID(ctx context.Context, fam coord.GrantFamily, id uuid.UUID) (coord.OAuthSignIn, error) {
	if _, err := e.fleetReady(); err != nil {
		return coord.OAuthSignIn{}, err
	}
	all, err := e.backends.Fleet.ListOAuthSignIns(ctx, fam)
	if err != nil {
		return coord.OAuthSignIn{}, fmt.Errorf("engine: read the sign-ins: %w", err)
	}
	for _, s := range all {
		if s.ID == id && !s.Lapsed(e.now()) {
			return s, nil
		}
	}
	return coord.OAuthSignIn{}, ErrNoSignIn
}

// ErrNoSignIn is a sign-in id no record holds, or one that lapsed.
var ErrNoSignIn = errors.New("engine: this sign-in is not known any more; start it again")

// CompleteSignIn finishes the sign-in the landing's state names, of whichever
// family: it spends the state, exchanges the code, seals the grant and
// re-activates the revision. Every refusal is a [*SignInRefusal]; none quotes
// the code, a token or the verifier.
func (e *Engine) CompleteSignIn(ctx context.Context, landing SignInLanding) (SignInLanded, error) {
	signer, err := e.oauthSigner()
	if err != nil {
		return SignInLanded{}, refusedState
	}
	kind, name, id, ok := openState(signer, landing.State)
	if !ok {
		return SignInLanded{}, refusedState
	}
	_, integrationGrant := kind.(consentGrants)
	landed := SignInLanded{Name: name, Title: kind.title(name), Integration: integrationGrant}
	estate, err := e.fleetReady()
	if err != nil {
		return landed, err
	}
	fam := kind.family()
	// DETACHED AND BOUNDED: the browser may leave mid-seal, and a grant the
	// vendor issued and the engine dropped is one nobody can use or revoke.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget+2*time.Minute)
	defer cancel()
	session, err := e.spendSignIn(ctx, fam, name, id)
	if err != nil {
		return landed, err
	}
	again := "Start it again from " + kind.page()
	fail := func(reason, remedy string) (SignInLanded, error) {
		e.endSignIn(ctx, fam, session, coord.OAuthSignInFailed, reason)
		log.WarnContext(ctx, fam.Owner+"_sign_in_failed", "grant", name, "sign_in", id.String(), "reason", reason)
		return landed, &SignInRefusal{Status: http.StatusBadGateway, Reason: reason, Remedy: remedy}
	}
	if landing.Error != "" {
		said := vendorSaid(landing.Error, landing.Description)
		if landing.Error == "access_denied" {
			return fail("the sign-in was declined at the vendor ("+said+")", again+" if that was not meant")
		}
		return fail("the vendor refused the sign-in ("+said+")", again+", or ask the vendor")
	}
	plain, err := e.cipher.Decrypt(session.Sealed, signInAAD(fam, session.ID))
	if err != nil {
		return fail("the sign-in could not be opened on this node", again)
	}
	var sealed signInSealed
	if json.Unmarshal([]byte(plain), &sealed) != nil {
		return fail("the sign-in could not be read on this node", again)
	}
	ep := &sealed.Endpoints
	// THE ISSUER (RFC 9207): a code another authorization server issued is
	// never sent to this one's endpoint.
	switch {
	case landing.Issuer != "" && strings.TrimSuffix(landing.Issuer, "/") != strings.TrimSuffix(ep.Issuer, "/"):
		return fail("the sign-in came back from an authorization server other than the one it was started at", again)
	case landing.Issuer == "" && ep.IssuerInResponse:
		return fail("the authorization server did not name itself in its answer, which it says it always does", again)
	case strings.TrimSpace(landing.Code) == "":
		return fail("the vendor sent no approval code", again)
	}
	// CHANGED UNDER THE SIGN-IN: exchanged now, it is a grant nothing uses.
	if unusable := kind.usable(e, name, nil); unusable != nil {
		return fail(unusable.reason(), unusable.remedy())
	}
	client, err := kind.client(e, name, sealed.Client)
	if err != nil {
		return fail(err.Error(), "Set it in Settings, Secrets, then sign in again")
	}
	tokens, err := oauth.Exchange(ctx, ep, client, landing.Code, sealed.Verifier, sealed.Redirect, e.now())
	if err != nil {
		return fail("the authorization server did not complete the sign-in: "+oauthWords(err), again+", or ask the vendor")
	}
	scopes, err := kind.accept(tokens, sealed.Scopes)
	if err != nil {
		// NOT KEPT: a grant other than the one asked for is one nothing
		// should hold.
		return fail(err.Error()+e.dropFresh(ctx, kind, name, ep, client, tokens.Refresh), again)
	}
	grant := sealedGrant{Grant: uuid.New(), Refresh: tokens.Refresh, Client: sealed.Client, Endpoints: *ep}
	by := iam.Actor{Name: session.By, Kind: session.ByKind, OperatorID: session.OperatorID}
	if err := e.sealGrant(ctx, estate, kind, name, grant, by); err != nil {
		// NOT KEPT: nobody could end it otherwise.
		return fail("the grant could not be kept: "+err.Error()+e.dropFresh(ctx, kind, name, ep, client, tokens.Refresh), again)
	}
	record := coord.OAuthGrant{
		Name: name, Grant: grant.Grant, Client: string(sealed.Client.Kind), ClientID: sealed.Client.ID,
		Issuer: ep.Issuer, Scopes: keptScopes(ctx, name, scopes),
		By: session.By, ByKind: session.ByKind, OperatorID: session.OperatorID, At: e.now(),
		Expiry: instantOrNil(tokens.RefreshExpiry),
	}
	if fam.Resourced {
		record.Resource = ep.Resource
	}
	if err := e.putGrant(ctx, fam, record); err != nil {
		return fail("the grant is sealed and could not be recorded: "+err.Error(), again)
	}
	e.endSignIn(ctx, fam, session, coord.OAuthSignInApproved, "")
	log.InfoContext(ctx, fam.Owner+"_signed_in", "grant", name, "sign_in", id.String(),
		"issuer", redact.URL(ep.Issuer), "by", session.By, "scopes", strings.Join(record.Scopes, " "))
	e.reloadForGrant(ctx, kind.key(name), "reload: signed in to "+kind.key(name), by)
	return landed, nil
}

// dropFresh disposes of a refresh token an exchange issued and the engine did
// not keep: revoked at the vendor where the kind allows it
// ([grantKind.keepFresh]), else held nowhere, answering what a person reads
// about it ("" once revoked).
func (e *Engine) dropFresh(ctx context.Context, kind grantKind, name string, ep *mcp.OAuthEndpoints,
	client oauth.Client, refresh string) string {

	if kept := kind.keepFresh(ctx, e, name, client.ID); kept != "" {
		return "; " + kept
	}
	_, _ = oauth.Revoke(ctx, ep, client, refresh, "refresh_token")
	return ""
}

// openState verifies a state's signature and expiry and reads its fields.
func openState(signer *runtoken.Signer, state string) (grantKind, string, uuid.UUID, bool) {
	key, raw, ok := strings.Cut(signer.Validate(strings.TrimSpace(state)), stateSep)
	if !ok || key == "" {
		return nil, "", uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, "", uuid.Nil, false
	}
	kind, name := kindOfKey(key)
	if name == "" {
		return nil, "", uuid.Nil, false
	}
	return kind, name, id, true
}

// spendSignIn moves the pending sign-in id on to completing at the version it
// read: the one way a state is spent, on any node.
func (e *Engine) spendSignIn(ctx context.Context, fam coord.GrantFamily, name string, id uuid.UUID) (coord.OAuthSignIn, error) {
	held, found, err := e.backends.Fleet.OAuthSignIn(ctx, fam, name)
	switch {
	case err != nil:
		return coord.OAuthSignIn{}, &SignInRefusal{Status: http.StatusServiceUnavailable,
			Reason: "this engine could not confirm the sign-in was unused",
			Remedy: "Go back and approve it again in a moment"}
	case !found || held.ID != id || held.Status != coord.OAuthSignInPending || e.now().After(held.ExpiresAt):
		return coord.OAuthSignIn{}, refusedState
	}
	held.Status = coord.OAuthSignInCompleting
	spent, ok, err := e.backends.Fleet.UpdateOAuthSignIn(ctx, fam, held)
	if err != nil || !ok {
		return coord.OAuthSignIn{}, refusedState
	}
	return spent, nil
}

// endSignIn records how a sign-in ended, for the page that polls it. The
// sealed half goes with it: nothing needs the verifier after this.
func (e *Engine) endSignIn(ctx context.Context, fam coord.GrantFamily, s coord.OAuthSignIn,
	status coord.OAuthSignInStatus, detail string) {

	s.Status, s.Detail, s.Sealed = status, textcut.Bytes(detail, coord.MaxGrantWords), ""
	if _, ok, err := e.backends.Fleet.UpdateOAuthSignIn(ctx, fam, s); err != nil || !ok {
		log.WarnContext(ctx, fam.Owner+"_sign_in_end_not_recorded", "grant", s.Name,
			"sign_in", s.ID.String(), "status", string(status), "error", err,
			"detail", "the page polling it reads the sign-in as it last stood until it lapses")
	}
}

// sealGrant seals grant as name's, writing over any grant it replaces at the
// version read, and revoking the replaced one's refresh token at its vendor
// only where the kind says so ([grantKind.revokesReplaced]).
func (e *Engine) sealGrant(ctx context.Context, estate *fleetsecrets.Estate, kind grantKind, name string,
	grant sealedGrant, by iam.Actor) error {

	value, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("encode the grant: %w", err)
	}
	author := secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
	row := grantRow(kind.family(), name)
	for range maxRetryRounds {
		held, found, err := estate.Held(ctx, row)
		if err != nil {
			return err
		}
		var wrote bool
		if found {
			if kind.revokesReplaced() {
				e.revokeHeld(ctx, kind, name, held.Value, func(c grantClient) (oauth.Client, error) {
					return kind.client(e, name, c)
				})
			}
			wrote, err = estate.Swap(ctx, row, string(value), held.Version, author, kind.source(), e.now())
		} else {
			wrote, err = estate.Create(ctx, row, string(value), author, kind.source(), e.now())
		}
		if err != nil {
			return err
		}
		if wrote {
			return nil
		}
	}
	return errors.New("the grant kept changing")
}

// putGrant records g as its name's grant, over whatever it had.
func (e *Engine) putGrant(ctx context.Context, fam coord.GrantFamily, g coord.OAuthGrant) error {
	fleet := e.backends.Fleet
	for range maxRetryRounds {
		held, found, err := fleet.OAuthGrant(ctx, fam, g.Name)
		if err != nil {
			return err
		}
		var wrote bool
		if found {
			g.Version = held.Version
			_, wrote, err = fleet.UpdateOAuthGrant(ctx, fam, g)
		} else {
			_, wrote, err = fleet.CreateOAuthGrant(ctx, fam, g)
		}
		if err != nil {
			return err
		}
		if wrote {
			return nil
		}
	}
	return errors.New("the grant kept changing")
}

// grantedScopes is what the token answer granted, else what was asked for
// (RFC 6749 reads silence as the request granted).
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

// vendorSaid is a vendor's error code and description as a page or record may
// hold them: one line, redacted, bounded.
func vendorSaid(code, description string) string {
	said := strings.Join(strings.Fields(code), " ")
	if d := strings.Join(strings.Fields(description), " "); d != "" {
		said += ": " + d
	}
	return redactedOpening(said)
}

// oauthWords is an OAuth error as a record or a page holds it: the vendor's
// own words for a refusal, else the error, redacted and bounded.
func oauthWords(err error) string {
	var refused *oauth.Error
	if errors.As(err, &refused) {
		return vendorSaid(cmpOr(refused.Code, fmt.Sprintf("HTTP %d", refused.Status)), refused.Description)
	}
	return redactedOpening(err.Error())
}

// oauthWordsBytes bounds what a record or a page keeps of an OAuth error: the
// bound the oauth package keeps a vendor's own words to.
const oauthWordsBytes = 512

// redactedOpening is the first [oauthWordsBytes] of said's whole redaction:
// redacted first and cut after, reading one byte past the bound
// ([redact.Prefix]).
func redactedOpening(said string) string {
	opening, _ := redact.Prefix(said, oauthWordsBytes+1)
	return textcut.Bytes(opening, oauthWordsBytes)
}

// reloadForGrant re-activates the revision so every node reads the grant
// afresh, after a sign-in or a sign-out.
func (e *Engine) reloadForGrant(ctx context.Context, key, summary string, by iam.Actor) {
	e.forgetSource(key)
	writer := e.configWriterOrNil()
	if writer == nil {
		log.WarnContext(ctx, "oauth_grant_not_republished", "grant", key,
			"detail", "this node cannot re-activate the revision, so each node takes the new "+
				"grant at its next apply; POST /config/reload")
		return
	}
	if err := writer.Reload(ctx, summary, by); err != nil {
		log.ErrorContext(ctx, "oauth_grant_not_republished", "grant", key, "error", err,
			"detail", "the grant is kept, and each node takes it at its next apply; POST /config/reload")
	}
}

// Revocation is what a vendor was asked when a grant was given up.
type Revocation string

const (
	// RevokedAtVendor is a grant its vendor revoked.
	RevokedAtVendor Revocation = "revoked"
	// RevokeNotOffered is a vendor that publishes no revocation endpoint:
	// the grant is forgotten here and lapses there.
	RevokeNotOffered Revocation = "not_offered"
	// RevokeFailed is a vendor that could not be asked, or refused.
	RevokeFailed Revocation = "failed"
	// RevokeShared is a grant forgotten here and left at its vendor, since
	// revoking it would end another grant too ([GrantSignOut.Shared]).
	RevokeShared Revocation = "shared"
)

// GrantSignOut is what giving up a grant did.
type GrantSignOut struct {
	// Revocation is what the vendor answered; the grant is forgotten here
	// whatever it was.
	Revocation Revocation
	// Said is the vendor's words where the revocation failed.
	Said string
	// Shared names the other grants a revocation would have ended, where
	// Revocation is [RevokeShared].
	Shared []string
}

// giveUp deletes name's sealed row, its public record and any sign-in in
// progress, first revoking the refresh token at the vendor unless keep
// answers what to report instead, with the client client resolves (the configuration before a
// removal, whose secret the new one may no longer name). It reports whether
// there was a grant, and re-activates nothing.
func (e *Engine) giveUp(ctx context.Context, kind grantKind, name string,
	client func(grantClient) (oauth.Client, error), keep func(sealedGrant) *GrantSignOut) (GrantSignOut, bool, error) {

	estate, err := e.fleetReady()
	if err != nil {
		return GrantSignOut{}, false, err
	}
	fam := kind.family()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget+time.Minute)
	defer cancel()
	fleet := e.backends.Fleet
	out := GrantSignOut{Revocation: RevokeNotOffered}
	row := grantRow(fam, name)
	held, sealedFound, err := estate.Held(ctx, row)
	if err != nil {
		return GrantSignOut{}, false, fmt.Errorf("engine: read the grant: %w", err)
	}
	if sealedFound {
		var grant sealedGrant
		_ = json.Unmarshal([]byte(held.Value), &grant)
		if kept := keep(grant); kept != nil {
			out = *kept
		} else {
			out = e.revokeHeld(ctx, kind, name, held.Value, client)
		}
		if _, gone := estate.Delete(ctx, row); gone != nil {
			return GrantSignOut{}, false, fmt.Errorf("engine: delete the grant: %w", gone)
		}
	}
	record, recordFound, err := fleet.OAuthGrant(ctx, fam, name)
	if err != nil {
		return GrantSignOut{}, false, fmt.Errorf("engine: read the grant record: %w", err)
	}
	if recordFound {
		if _, err := fleet.DeleteOAuthGrant(ctx, fam, name, record.Version); err != nil {
			return GrantSignOut{}, false, fmt.Errorf("engine: delete the grant record: %w", err)
		}
	}
	if session, found, err := fleet.OAuthSignIn(ctx, fam, name); err == nil && found {
		_, _ = fleet.DeleteOAuthSignIn(ctx, fam, name, session.Version)
	}
	e.forgetSource(kind.key(name))
	return out, sealedFound || recordFound, nil
}

// revokeHeld revokes the refresh token in a sealed grant at its vendor, and
// says what came of it.
func (e *Engine) revokeHeld(ctx context.Context, kind grantKind, name, value string,
	resolve func(grantClient) (oauth.Client, error)) GrantSignOut {

	var grant sealedGrant
	if err := json.Unmarshal([]byte(value), &grant); err != nil || grant.Refresh == "" {
		return GrantSignOut{Revocation: RevokeFailed, Said: "the kept grant could not be read"}
	}
	client := oauth.Client{Kind: grant.Client.Kind, ID: grant.Client.ID, Secret: grant.Client.Secret}
	if c, err := resolve(grant.Client); err == nil {
		client = c
	}
	revoked, err := oauth.Revoke(ctx, &grant.Endpoints, client, grant.Refresh, "refresh_token")
	switch {
	case err != nil:
		log.WarnContext(ctx, kind.family().Owner+"_grant_not_revoked", "grant", name, "error", oauthWords(err),
			"detail", "the grant is forgotten here; it lapses at the vendor, or is revoked there by hand")
		return GrantSignOut{Revocation: RevokeFailed, Said: oauthWords(err)}
	case !revoked:
		return GrantSignOut{Revocation: RevokeNotOffered}
	}
	return GrantSignOut{Revocation: RevokedAtVendor}
}

// heldGrant reports whether name has a grant: the record every node reads, or
// the sealed row where only that is left.
func (e *Engine) heldGrant(ctx context.Context, fam coord.GrantFamily, name string) (bool, error) {
	estate, err := e.fleetReady()
	if err != nil {
		return false, err
	}
	if _, recorded, readErr := e.backends.Fleet.OAuthGrant(ctx, fam, name); readErr != nil || recorded {
		return recorded, readErr
	}
	_, found, err := estate.Held(ctx, grantRow(fam, name))
	return found, err
}

// signInNeeded is a grant that cannot be sent, in place of a token: nobody
// signed in, its vendor refused to renew it, or the configuration moved.
type signInNeeded struct {
	server string
	// integration is whether the grant is an integration's consent grant.
	integration bool
	// failure is the vendor's words for the renewal it refused, "" where none was.
	failure string
	// moved is the address the grant was issued for, where the server is no
	// longer configured.
	moved string
	// unconfigured is why the configuration no longer takes the grant, ""
	// where it does, and what the remedy where the usual one does not fit.
	unconfigured, what string
}

func (s *signInNeeded) Error() string {
	if s.integration {
		switch {
		case s.unconfigured != "":
			return s.server + "'s sign-in is not used: " + s.unconfigured
		case s.failure != "":
			return fmt.Sprintf("%s's sign-in can no longer be renewed (%s); sign in again from its page",
				s.server, s.failure)
		}
		return s.server + " has no sign-in: nobody has signed in to it; sign in from its page"
	}
	switch {
	case s.unconfigured != "":
		return s.server + " is not started: " + s.unconfigured
	case s.moved != "":
		return fmt.Sprintf("%s is not started: its sign-in was made for %s, which is not the server's "+
			"address now; sign in again from its page", s.server, s.moved)
	case s.failure != "":
		return fmt.Sprintf("%s is not started: its vendor refused to renew the sign-in (%s); sign in "+
			"again from its page", s.server, s.failure)
	}
	return s.server + " is not started: nobody has signed in to it; sign in from its page"
}

func (s *signInNeeded) Unwrap() error {
	if s.integration {
		return ErrIntegrationSignInNeeded
	}
	return mcp.ErrSignInNeeded
}

// reason and remedy are s as a failed sign-in's two sentences.
func (s *signInNeeded) reason() string { return cmpOr(s.unconfigured, s.Error()) }

func (s *signInNeeded) remedy() string {
	if s.what != "" {
		return s.what
	}
	if s.integration {
		return "Turn the integration on with its OAuth client, then sign in"
	}
	return "Add the server's oauth block, then sign in"
}

// grantSource is the token source name's grant is sent with, or why it may not
// be ([signInNeeded]).
//
// READ AFRESH at each call, so the pass a sign-in's re-activation runs reads
// the grant it wrote; a failed read takes the grant this node last read, since
// a store that blinks is not a sign-out.
func (e *Engine) grantSource(ctx context.Context, kind grantKind, name string) (*oauthSource, *signInNeeded) {
	_, integrationGrant := kind.(consentGrants)
	needed := &signInNeeded{server: name, integration: integrationGrant}
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, needed
	}
	key := kind.key(name)
	g, found, err := e.backends.Fleet.OAuthGrant(ctx, kind.family(), name)
	e.oauth.mu.Lock()
	defer e.oauth.mu.Unlock()
	switch {
	case err != nil:
		log.WarnContext(ctx, "oauth_grant_unread", "grant", key, "error", err,
			"detail", "the grant this node last read is used")
		last := e.oauth.last[key]
		if last == nil {
			return nil, needed
		}
		g, found = *last, true
	case found:
		if e.oauth.last == nil {
			e.oauth.last = map[string]*coord.OAuthGrant{}
		}
		e.oauth.last[key] = &g
	default:
		delete(e.oauth.last, key)
	}
	if !found {
		delete(e.oauth.sources, key)
		return nil, needed
	}
	if g.Failed() {
		delete(e.oauth.sources, key)
		needed.failure = g.Failure
		return nil, needed
	}
	if unusable := kind.usable(e, name, &g); unusable != nil {
		delete(e.oauth.sources, key)
		return nil, unusable
	}
	if s := e.oauth.sources[key]; s != nil && s.grant == g.Grant {
		return s, nil
	}
	if e.oauth.sources == nil {
		e.oauth.sources = map[string]*oauthSource{}
	}
	s := &oauthSource{engine: e, server: name, kind: kind, grant: g.Grant}
	e.oauth.sources[key] = s
	return s, nil
}

// forgetSource drops this node's token source for the grant key names, and
// the token it holds.
func (e *Engine) forgetSource(key string) {
	e.oauth.mu.Lock()
	defer e.oauth.mu.Unlock()
	delete(e.oauth.sources, key)
	delete(e.oauth.last, key)
}

// oauthSource is one grant's access token on this node: the
// [mcp.BearerSource] its requests send with, and the token's only holder.
//
// ONE SOURCE FOR EVERY GRANT, so sending, renewing ahead of expiry and the
// one renewal on a 401 share a path whether the token comes from a company's
// grant of any family ([oauthSource.renewGrant]) or an MCP server's service
// identity ([serviceIdentity.issue]).
type oauthSource struct {
	engine *Engine
	// server is the grant's name (the MCP server's, the integration kind).
	server string
	// kind is the family of the grant the source renews, and grant its
	// sign-in; both unset for a service identity's source.
	kind  grantKind
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

// Rejected renews the token a vendor refused, once: one already renewed since,
// or issued within [oauthFreshWindow], is answered as it is.
func (s *oauthSource) Rejected(ctx context.Context, refused string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && (refused != s.token || s.engine.now().Sub(s.minted) < oauthFreshWindow) {
		return s.token, nil
	}
	return s.renewLocked(ctx)
}

// renewLocked is issued a new access token and holds it. Called with s.mu
// held, which keeps two requests on this node from asking at once.
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

// renewGrant renews the company's grant under the family's renewal lease
// (ADR-0075). Called with s.mu held.
func (s *oauthSource) renewGrant(ctx context.Context) (oauth.Tokens, error) {
	e, kind := s.engine, s.kind
	fam := kind.family()
	_, integrationGrant := kind.(consentGrants)
	needed := func() *signInNeeded { return &signInNeeded{server: s.server, integration: integrationGrant} }
	estate, err := e.fleetReady()
	if err != nil {
		return oauth.Tokens{}, err
	}
	fleet := e.backends.Fleet
	record, found, err := fleet.OAuthGrant(ctx, fam, s.server)
	switch {
	case err != nil:
		return oauth.Tokens{}, fmt.Errorf("engine: read %s's grant: %w", s.server, err)
	case !found || record.Grant != s.grant:
		return oauth.Tokens{}, needed()
	case record.Failed():
		// REFUSED FOR GOOD ALREADY, on any node: the vendor is not asked again
		// until somebody signs in.
		n := needed()
		n.failure = record.Failure
		return oauth.Tokens{}, n
	}
	if unusable := kind.usable(e, s.server, &record); unusable != nil {
		return oauth.Tokens{}, unusable
	}
	release, err := e.holdRenewal(ctx, fam, s.server)
	if err != nil {
		return oauth.Tokens{}, err
	}
	defer release()
	row := grantRow(fam, s.server)
	held, found, err := estate.Held(ctx, row)
	if err != nil {
		return oauth.Tokens{}, fmt.Errorf("engine: read %s's grant: %w", s.server, err)
	}
	var grant sealedGrant
	if !found || json.Unmarshal([]byte(held.Value), &grant) != nil || grant.Grant != s.grant {
		return oauth.Tokens{}, needed()
	}
	client, err := kind.client(e, s.server, grant.Client)
	if err != nil {
		return oauth.Tokens{}, fmt.Errorf("engine: renew %s's sign-in: %w", s.server, err)
	}
	now := e.now()
	tokens, err := oauth.Refresh(ctx, &grant.Endpoints, client, grant.Refresh, now)
	var refused *oauth.Error
	if errors.As(err, &refused) && refused.Definitive() {
		said := kind.refused(record, oauthWords(err), now)
		e.recordRenewalRefused(ctx, fam, record, said)
		n := needed()
		n.failure = said
		return oauth.Tokens{}, n
	}
	if err != nil {
		log.WarnContext(ctx, fam.Owner+"_oauth_renewal_failed", "grant", s.server, "error", oauthWords(err),
			"detail", "the next request asks again")
		return oauth.Tokens{}, fmt.Errorf("engine: renew %s's sign-in: %s", s.server, oauthWords(err))
	}
	if tokens.Refresh != "" && tokens.Refresh != grant.Refresh {
		grant.Refresh = tokens.Refresh
		value, err := json.Marshal(grant)
		if err == nil {
			var wrote bool
			wrote, err = estate.Swap(ctx, row, string(value), held.Version,
				secrets.Author{Name: e.selfNode(), Kind: string(iam.ActorSystem)}, kind.source(), now)
			if err == nil && !wrote {
				err = errors.New("the kept grant moved while it was renewed")
			}
		}
		if err != nil {
			// THE TOKEN IS STILL GOOD here; only the next renewal is lost.
			log.ErrorContext(ctx, fam.Owner+"_oauth_rotation_not_kept", "grant", s.server, "error", err,
				"detail", "the vendor rotated the refresh token and the new one could not be kept; "+
					"the next renewal asks for a sign-in")
		}
	}
	if expiry := instantOrNil(tokens.RefreshExpiry); expiry != nil {
		e.recordGrantExpiry(ctx, fam, record, *expiry)
	}
	return tokens, nil
}

// holdRenewal takes name's renewal lease in fam, waiting up to
// [oauthRenewalWait] for another node's renewal, and answers its release.
func (e *Engine) holdRenewal(ctx context.Context, fam coord.GrantFamily, name string) (func(), error) {
	resource := fam.Renewal.Resource(name)
	owner := e.selfNode() + "/" + uuid.NewString()
	deadline := e.now().Add(oauthRenewalWait)
	for {
		lease, _, err := e.backends.Coord.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: oauthRenewalLeaseTTL, Ungated: true})
		if err != nil {
			return nil, fmt.Errorf("engine: take %s's renewal lease: %w", name, err)
		}
		if lease != nil {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthStoreBudget)
				defer cancel()
				if _, err := e.backends.Coord.Release(releaseCtx, resource, owner, lease.Epoch); err != nil {
					log.WarnContext(ctx, fam.Owner+"_oauth_renewal_lease_kept", "grant", name, "error", err,
						"detail", "it lapses on its own")
				}
			}, nil
		}
		if !e.now().Before(deadline) {
			return nil, fmt.Errorf("engine: another node is renewing %s's sign-in; the next request asks again", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(oauthRenewalPoll):
		}
	}
}

// recordRenewalRefused marks the grant as refused for good, in its vendor's
// words, so no node asks the vendor again until somebody signs in.
func (e *Engine) recordRenewalRefused(ctx context.Context, fam coord.GrantFamily, record coord.OAuthGrant, said string) {
	now := e.now()
	record.Failure, record.FailedAt = textcut.Bytes("the vendor refused to renew the sign-in: "+said,
		coord.MaxGrantWords), &now
	if _, ok, err := e.backends.Fleet.UpdateOAuthGrant(ctx, fam, record); err != nil || !ok {
		log.WarnContext(ctx, fam.Owner+"_oauth_refusal_not_recorded", "grant", record.Name, "error", err,
			"detail", "the next renewal on any node asks the vendor again")
	}
	log.ErrorContext(ctx, fam.Owner+"_oauth_renewal_refused", "grant", record.Name, "said", said,
		"detail", "the grant needs somebody to sign in again")
}

// recordGrantExpiry records when the grant now runs out, where the vendor
// moved it.
func (e *Engine) recordGrantExpiry(ctx context.Context, fam coord.GrantFamily, record coord.OAuthGrant, expiry time.Time) {
	if record.Expiry != nil && record.Expiry.Equal(expiry) {
		return
	}
	record.Expiry = &expiry
	if _, _, err := e.backends.Fleet.UpdateOAuthGrant(ctx, fam, record); err != nil {
		log.WarnContext(ctx, fam.Owner+"_oauth_expiry_not_recorded", "grant", record.Name, "error", err)
	}
}
