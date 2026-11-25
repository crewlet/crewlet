package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// AN INTEGRATION'S CONSENT GRANT (ADR-0178): the integration family of the
// company's grants (oauthgrant.go), one per kind, for a Google product that
// refuses service accounts (googleauth.Consent). It signs in at Google's fixed
// endpoints as the company's own OAuth client, asks for the scopes its seats'
// levels need and keeps the grant only if Google granted exactly those. Signing in again never
// revokes the grant it replaces, and giving one up revokes at Google only
// when no other live grant of the company has a client in the same project.

// consentKeyPrefix marks an integration grant's key ([grantKind.key]): no MCP
// server name holds a '/'.
const consentKeyPrefix = "integration/"

// The refusals of an integration's sign-in.
var (
	// ErrIntegrationSignInNeeded is an integration whose grant nobody holds,
	// or whose vendor refused to renew it.
	ErrIntegrationSignInNeeded = errors.New("engine: the integration needs somebody to sign in with its vendor")
	// ErrNoConsentProduct is an integration kind no consent product is.
	ErrNoConsentProduct = errors.New("engine: this integration is not signed in to with a person's consent")
	// ErrNoConsentCallback is an engine with no public address: Google sends
	// the person's browser back to it, and offers these scopes no device
	// sign-in that would do without one.
	ErrNoConsentCallback = errors.New("engine: this engine has no public address for Google to send " +
		"the person back to, and Google offers no device sign-in for this product; set api.external_url " +
		"to an address the person's browser reaches, then sign in")
	// ErrIntegrationNotSignedIn is a sign-out of an integration nobody
	// signed in to.
	ErrIntegrationNotSignedIn = errors.New("engine: nobody is signed in to the integration")
)

// consentGrants is the integration family's [grantKind].
type consentGrants struct{}

func (consentGrants) family() coord.GrantFamily { return coord.IntegrationGrantFamily }
func (consentGrants) source() secrets.Source    { return secrets.SourceIntegrationSignIn }
func (consentGrants) key(kind string) string    { return consentKeyPrefix + kind }
func (consentGrants) page() string              { return "the integration's page in Settings, Integrations" }

func (consentGrants) title(kind string) string {
	if p, ok := googleauth.ConsentOf(integration.Kind(kind)); ok {
		return p.Name
	}
	return kind
}

// revokesReplaced is false: Google's revocation ends every grant the person
// made to the client's project, so revoking the old token would end the one
// just sealed. The old token is held nowhere and lapses at Google.
func (consentGrants) revokesReplaced() bool { return false }

// keepFresh leaves a refused token unrevoked where revoking it would end a
// live grant too: this kind's own grant, signed in before, or another kind's
// with a client in the same Cloud project (as a sign-out does).
func (consentGrants) keepFresh(ctx context.Context, e *Engine, kind, clientID string) string {
	ending, err := e.sharingProject(ctx, integration.Kind(kind), clientID)
	if err == nil {
		var held bool
		if _, held, err = e.backends.Fleet.OAuthGrant(ctx, coord.IntegrationGrantFamily, kind); held {
			ending = append([]string{kind}, ending...)
		}
	}
	switch {
	case err != nil:
		return "nothing was kept, and nothing was revoked at Google, since the other sign-ins could not " +
			"be read; remove Crewlet at myaccount.google.com to end the access there"
	case len(ending) > 0:
		return "nothing was kept, and nothing was revoked at Google, since that would also end " +
			GrantSignOut{Shared: ending}.titles() + "'s sign-in; remove Crewlet at myaccount.google.com " +
			"to end the access there"
	}
	return ""
}

// accept keeps the grant only where Google granted exactly the scopes asked.
func (consentGrants) accept(tokens oauth.Tokens, asked []string) ([]string, error) {
	if err := googleauth.CheckConsentScope(asked, tokens.Scopes); err != nil {
		return nil, err
	}
	return slices.Clone(tokens.Scopes), nil
}

// refused adds the Testing-mode reading to an invalid_grant that came about a
// week after the sign-in.
func (consentGrants) refused(g coord.OAuthGrant, said string, now time.Time) string {
	if strings.HasPrefix(said, "invalid_grant") && googleauth.LooksLikeTestingMode(g.At, now) {
		return said + ". " + googleauth.TestingModeHint
	}
	return said
}

// consentConfigured is kind's consent product and the client the running
// configuration names for it, or why there is none.
func (e *Engine) consentConfigured(kind string) (googleauth.Consent, googleauth.ConsentClient, *signInNeeded) {
	needed := func(why string) *signInNeeded {
		return &signInNeeded{server: kind, integration: true, unconfigured: why}
	}
	p, ok := googleauth.ConsentOf(integration.Kind(kind))
	if !ok {
		return googleauth.Consent{}, googleauth.ConsentClient{}, needed("it is not signed in to with a person's consent")
	}
	c := e.Company()
	if c == nil || c.Config == nil {
		return p, googleauth.ConsentClient{}, needed("no configuration is running")
	}
	client, on := p.Client(c.Config)
	switch {
	case !on:
		return p, client, needed("the integration is turned off")
	case client.ID == "" || client.Secret == "":
		return p, client, needed("no Google sign-in client is set; add it on the Google page's Settings tab")
	}
	return p, client, nil
}

// usable holds a grant back while its integration is off or names no client,
// and one made as another client than the configuration names now: Google
// renews a refresh token only for the client it was issued to.
func (consentGrants) usable(e *Engine, kind string, g *coord.OAuthGrant) *signInNeeded {
	_, client, needed := e.consentConfigured(kind)
	if needed != nil {
		return needed
	}
	if g != nil && g.ClientID != client.ID {
		return &signInNeeded{server: kind, integration: true,
			unconfigured: "the integration's OAuth client changed since the sign-in; sign in again"}
	}
	return nil
}

func (consentGrants) client(e *Engine, kind string, c grantClient) (oauth.Client, error) {
	_, configured, needed := e.consentConfigured(kind)
	if needed != nil {
		return oauth.Client{}, needed
	}
	return e.consentClient(c.ID, configured.Secret)
}

// consentClient is the company's own client with its secret resolved now.
func (e *Engine) consentClient(id, secret string) (oauth.Client, error) {
	env := e.resolver()
	if missing := env.Missing(secret); len(missing) > 0 {
		return oauth.Client{}, fmt.Errorf("the OAuth client secret reads %s, which is not set",
			strings.Join(missing, ", "))
	}
	return oauth.Client{Kind: catalogue.ClientOwn, ID: id, Secret: env.Value(secret)}, nil
}

// consentEndpoints are Google's consent endpoints, or a test's fake vendor.
func (e *Engine) consentEndpoints() *mcp.OAuthEndpoints {
	if e.oauth.consentAt != nil {
		ep := *e.oauth.consentAt
		return &ep
	}
	return &mcp.OAuthEndpoints{
		Server: googleauth.ConsentIssuer, Issuer: googleauth.ConsentIssuer,
		Authorization: googleauth.ConsentAuthorizeURL, Token: googleauth.ConsentTokenURL,
		Revocation: googleauth.ConsentRevokeURL, PKCEMethods: []string{oauth.MethodS256},
		TokenAuthMethods: []string{"client_secret_basic", "client_secret_post"},
	}
}

// StartIntegrationSignIn begins kind's consent sign-in as by: the company's
// own client, Google's endpoints, and exactly the scopes the product's seats
// need now.
func (e *Engine) StartIntegrationSignIn(ctx context.Context, kind integration.Kind, by iam.Actor) (SignInStarted, error) {
	p, client, needed := e.consentConfigured(string(kind))
	switch {
	case needed != nil && p.Kind == "":
		return SignInStarted{}, ErrNoConsentProduct
	case needed != nil:
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict, Reason: needed.Error(),
			Remedy: needed.remedy()}
	}
	if _, err := e.fleetReady(); err != nil {
		return SignInStarted{}, err
	}
	redirect := e.CallbackURL()
	if redirect == "" {
		return SignInStarted{}, ErrNoConsentCallback
	}
	if _, err := e.consentClient(client.ID, client.Secret); err != nil {
		return SignInStarted{}, &SignInRefusal{Status: http.StatusConflict, Reason: err.Error(),
			Remedy: "Set it in Settings, Secrets, then sign in"}
	}
	scopes, err := googleauth.ConsentScopes(p, e.Company().Config)
	if err != nil {
		return SignInStarted{}, err
	}
	return e.startSignIn(ctx, signInStart{kind: consentGrants{}, name: string(kind), ep: e.consentEndpoints(),
		client: grantClient{Kind: catalogue.ClientOwn, ID: client.ID}, scopes: scopes,
		redirect: redirect, extra: googleauth.ConsentParams()}, by)
}

// IntegrationSignIn reads an integration's sign-in by id, for the page that
// polls it.
func (e *Engine) IntegrationSignIn(ctx context.Context, id uuid.UUID) (coord.OAuthSignIn, error) {
	return e.signInByID(ctx, coord.IntegrationGrantFamily, id)
}

// IntegrationGrant reads kind's grant, its public half.
func (e *Engine) IntegrationGrant(ctx context.Context, kind integration.Kind) (coord.OAuthGrant, bool, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return coord.OAuthGrant{}, false, errors.New("engine: this engine holds no fleet store")
	}
	return e.backends.Fleet.OAuthGrant(ctx, coord.IntegrationGrantFamily, string(kind))
}

// SignInClientSubject is the subject of a consent product's finding that no
// Google sign-in client is set.
const SignInClientSubject = "integrations.google_cloud.sign_in_client"

// IntegrationGrantFinding is what kind's pass reports of its grant: nil while
// it renews, `credential_missing` while no sign-in client is set or nobody has
// signed in, and `credential_rejected` once its vendor refused it for good, in
// the vendor's redacted words. It raises where the store could not be read.
func (e *Engine) IntegrationGrantFinding(ctx context.Context, kind integration.Kind) (*integration.Finding, error) {
	if _, client, _ := e.consentConfigured(string(kind)); client.ID == "" {
		return &integration.Finding{Kind: integration.FindingCredentialMissing, Subject: SignInClientSubject,
			Detail: "No Google sign-in client is set, so nobody can sign in yet.",
			Remedy: "Add the sign-in client on the Google page's Settings tab, then sign in."}, nil
	}
	g, found, err := e.IntegrationGrant(ctx, kind)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return &integration.Finding{Kind: integration.FindingCredentialMissing,
			Detail: "Waiting for a person to sign in with Google and allow access."}, nil
	case g.Failed():
		return &integration.Finding{Kind: integration.FindingCredentialRejected,
			Detail: "Google no longer accepts the sign-in: " + g.Failure}, nil
	}
	return nil, nil
}

// IntegrationCredential is an integration's consent grant as its product sends
// with it: the token, renewed ahead of expiry, and one renewal on a 401.
type IntegrationCredential struct{ source *oauthSource }

// IntegrationCredential is kind's grant to send with, or why there is none
// (an error wrapping [ErrIntegrationSignInNeeded]).
func (e *Engine) IntegrationCredential(ctx context.Context, kind integration.Kind) (IntegrationCredential, error) {
	s, needed := e.grantSource(ctx, consentGrants{}, string(kind))
	if needed != nil {
		return IntegrationCredential{}, needed
	}
	return IntegrationCredential{source: s}, nil
}

// Do sends one request with the grant's token: send is called with the token,
// and once more with a renewed one where the vendor answered 401 (ADR-0075).
func (c IntegrationCredential) Do(ctx context.Context,
	send func(ctx context.Context, token string) (*http.Response, error)) (*http.Response, error) {

	token, err := c.source.Token(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := send(ctx, token)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_ = resp.Body.Close()
	renewed, err := c.source.Rejected(ctx, token)
	if err != nil {
		return nil, err
	}
	return send(ctx, renewed)
}

// RecordIntegrationAccounts records the vendor accounts kind's grant reached
// when its product last looked, at the version read.
func (e *Engine) RecordIntegrationAccounts(ctx context.Context, kind integration.Kind, accounts []string) error {
	accounts = slices.Sorted(slices.Values(accounts))
	for range maxRetryRounds {
		g, found, err := e.IntegrationGrant(ctx, kind)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrIntegrationNotSignedIn
		}
		if slices.Equal(g.Accounts, accounts) {
			return nil
		}
		g.Accounts = accounts
		_, wrote, err := e.backends.Fleet.UpdateOAuthGrant(ctx, coord.IntegrationGrantFamily, g)
		if err != nil || wrote {
			return err
		}
	}
	return errors.New("engine: the integration's grant kept changing")
}

// SignOutIntegration gives up kind's grant ([Engine.RevokeIntegrationGrant])
// and re-activates the revision so every node stops sending it;
// [ErrIntegrationNotSignedIn] where there is none.
func (e *Engine) SignOutIntegration(ctx context.Context, kind integration.Kind, by iam.Actor) (GrantSignOut, error) {
	c := e.Company()
	if c == nil {
		return GrantSignOut{}, errors.New("engine: no configuration is running")
	}
	out, held, err := e.RevokeIntegrationGrant(ctx, c.Config, kind)
	if err != nil {
		return GrantSignOut{}, err
	}
	if !held {
		return GrantSignOut{}, ErrIntegrationNotSignedIn
	}
	log.InfoContext(ctx, "integration_signed_out", "grant", string(kind), "by", by.Name,
		"revocation", string(out.Revocation))
	e.reloadForGrant(ctx, consentKeyPrefix+string(kind), "reload: signed out of integration "+string(kind), by)
	return out, nil
}

// RevokeIntegrationGrant deletes kind's sealed row, record and any sign-in in
// progress, reporting whether there was a grant. It revokes at Google only
// where no other live grant of the company has a client in the same Cloud
// project, since Google's revocation would end that one too.
//
// BEFORE IS THE CONFIGURATION BEFORE a disconnect: the client secret it names
// authenticates the revocation, which the new configuration may not name.
func (e *Engine) RevokeIntegrationGrant(ctx context.Context, before *config.Company, kind integration.Kind) (GrantSignOut, bool, error) {
	secret := ""
	if p, ok := googleauth.ConsentOf(kind); ok && before != nil {
		client, _ := p.Client(before)
		secret = client.Secret
	}
	return e.giveUp(ctx, consentGrants{}, string(kind),
		func(c grantClient) (oauth.Client, error) { return e.consentClient(c.ID, secret) },
		func(g sealedGrant) *GrantSignOut {
			others, err := e.sharingProject(ctx, kind, g.Client.ID)
			switch {
			case err != nil:
				// NOT KNOWN TO BE ALONE, SO KEPT: a revocation cannot be
				// taken back, and a grant left at Google lapses unused.
				return &GrantSignOut{Revocation: RevokeFailed,
					Said: "the other sign-ins could not be read, so nothing was revoked at Google"}
			case len(others) > 0:
				return &GrantSignOut{Revocation: RevokeShared, Shared: others}
			}
			return nil
		})
}

// sharingProject is every other live integration grant whose client is in the
// same Cloud project as clientID's ([googleauth.ProjectNumber]), by kind.
func (e *Engine) sharingProject(ctx context.Context, kind integration.Kind, clientID string) ([]string, error) {
	grants, err := e.backends.Fleet.ListOAuthGrants(ctx, coord.IntegrationGrantFamily)
	if err != nil {
		return nil, err
	}
	project, known := googleauth.ProjectNumber(clientID)
	var out []string
	for _, g := range grants {
		if g.Name == string(kind) || g.Failed() {
			continue
		}
		other, otherKnown := googleauth.ProjectNumber(g.ClientID)
		if (known && otherKnown && other == project) || g.ClientID == clientID {
			out = append(out, g.Name)
		}
	}
	return out, nil
}

// GiveUpIntegrationGrant is [Engine.RevokeIntegrationGrant] in the plain words
// a disconnect answers (configapi.Grants): the revocation, the sentence a
// person reads about it, and whether there was a grant. A kind no consent
// product is has none.
func (e *Engine) GiveUpIntegrationGrant(ctx context.Context, before *config.Company, kind string) (string, string, bool, error) {
	if _, ok := googleauth.ConsentOf(integration.Kind(kind)); !ok {
		return "", "", false, nil
	}
	out, held, err := e.RevokeIntegrationGrant(ctx, before, integration.Kind(kind))
	if err != nil || !held {
		return "", "", held, err
	}
	return string(out.Revocation), out.Sentence(), true, nil
}

// Sentence is what a person reads about a consent grant given up: what Google
// was asked, and where the engine left Google alone, why and how to finish it.
func (o GrantSignOut) Sentence() string {
	switch o.Revocation {
	case RevokedAtVendor:
		return "Crewlet deleted its copy and Google revoked the access."
	case RevokeShared:
		return "Crewlet deleted its copy. To remove the access at Google too, remove Crewlet at " +
			"myaccount.google.com, which also ends " + o.titles() + "'s sign-in."
	case RevokeFailed:
		return "Crewlet deleted its copy; Google could not be asked to revoke it (" + o.Said +
			"). Remove Crewlet at myaccount.google.com to end the access there."
	}
	return "Crewlet deleted its copy."
}

// titles are the products of the grants a revocation would have ended, as a
// sentence names them.
func (o GrantSignOut) titles() string {
	titles := make([]words.Said, len(o.Shared))
	for i, kind := range o.Shared {
		titles[i] = words.Said(consentGrants{}.title(kind))
	}
	return words.And(titles)
}
