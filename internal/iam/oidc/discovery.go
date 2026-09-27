package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/jwks"
	"github.com/crewlet/crewlet/internal/logging"
)

// DISCOVERY: the three endpoints a round trip needs, read from the provider
// rather than configured.
//
// An operator configures ONE value — the issuer — and everything else is read
// from the document it serves. Configuring the endpoints instead would be
// three more fields to get right, three more things that go stale when a
// provider moves one, and three chances to point the token exchange at a host
// that is not the one whose keys verify the token.
//
// THE DOCUMENT IS FETCHED FROM THE ISSUER AND ITS `issuer` MUST MATCH. A
// provider that serves metadata naming a different issuer is either
// misconfigured or is somebody's redirect, and the value in it is what every
// subsequent comparison is made against — so accepting a mismatch would let
// the metadata decide what the tokens are checked against.

// MetadataTTL is how long a discovery document is trusted.
//
// TWENTY-FOUR HOURS. The endpoints in it change on the order of never, and the
// one value that DOES rotate — the signing keys — is behind its own cache with
// its own much shorter window. A long TTL here is what keeps a sign-in from
// depending on the provider's metadata host being up.
const MetadataTTL = 24 * time.Hour

// Metadata is the subset of an OpenID provider's discovery document this
// engine reads.
type Metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`

	// EndSessionEndpoint is where a person signing out here is sent to end
	// their session AT THE PROVIDER too (`POST /auth/logout/oidc`), and
	// empty at a provider that publishes none.
	EndSessionEndpoint string `json:"end_session_endpoint"`

	// The four lists below are read to REPORT rather than to decide — see
	// [Metadata.Concerns].
	ScopesSupported    []string `json:"scopes_supported"`
	SigningAlgorithms  []string `json:"id_token_signing_alg_values_supported"`
	ResponseTypes      []string `json:"response_types_supported"`
	CodeChallengeTypes []string `json:"code_challenge_methods_supported"`
}

// Concern is one thing a provider's discovery document says this engine's
// sign-in cannot rely on there: the document field that says it, and a
// sentence for the operator.
type Concern struct {
	Field  string
	Detail string
}

// Concerns reads a discovery document's advertised capabilities against what
// this engine asks of a provider, one [Concern] per mismatch.
//
// # Advisory, and never a refusal
//
// Every one of these lists is optional or unevenly kept — plenty of providers
// implement S256 without advertising it — so an ABSENT list says nothing, and
// a list that disagrees is reported rather than acted on: a login refused
// because a document omitted a value is an outage caused by metadata. What a
// concern is FOR is the operator who would otherwise meet it a round trip
// later as something with no name: every sign-in refused as an unverifiable
// token, a provider ignoring the PKCE challenge it was sent, a deactivation
// probe that never has a refresh token to ask with. [Provider.Metadata]
// reports each one when it fetches a document, which is once a day.
func (m Metadata) Concerns(c Config) []Concern {
	var out []Concern
	if len(m.ResponseTypes) > 0 && !slices.Contains(m.ResponseTypes, "code") {
		out = append(out, Concern{Field: "response_types_supported", Detail: fmt.Sprintf(
			"the provider advertises %v and not `code`, the authorization code "+
				"flow this engine signs in with, so it may refuse every sign-in "+
				"request", m.ResponseTypes)})
	}
	if len(m.SigningAlgorithms) > 0 && !slices.ContainsFunc(m.SigningAlgorithms,
		func(alg string) bool { return slices.Contains(Algorithms, alg) }) {

		out = append(out, Concern{Field: "id_token_signing_alg_values_supported",
			Detail: fmt.Sprintf("the provider signs ID tokens with %v and this "+
				"engine verifies %v, so every sign-in will be refused as an "+
				"unverifiable token; configure the application at the provider "+
				"to sign with RS256", m.SigningAlgorithms, Algorithms)})
	}
	if len(m.CodeChallengeTypes) > 0 && !slices.Contains(m.CodeChallengeTypes, "S256") {
		out = append(out, Concern{Field: "code_challenge_methods_supported",
			Detail: fmt.Sprintf("the provider advertises %v and not S256, the "+
				"only PKCE method this engine sends, so it may refuse the "+
				"challenge or ignore it — and an ignored challenge is a code "+
				"anybody who intercepts it can redeem", m.CodeChallengeTypes)})
	}
	if len(m.ScopesSupported) > 0 && slices.Contains(c.requested(), "offline_access") &&
		!slices.Contains(m.ScopesSupported, "offline_access") {

		out = append(out, Concern{Field: "scopes_supported", Detail: "the provider " +
			"does not advertise `offline_access`, so a sign-in may get no refresh " +
			"token and the deactivation probe nothing to ask with: somebody " +
			"disabled at the provider keeps their session here until its " +
			"absolute deadline"})
	}
	return out
}

// Provider is a discovered identity provider, with its key set cached beside
// its metadata.
//
// SAFE FOR CONCURRENT USE.
type Provider struct {
	config Config
	client *http.Client
	now    func() time.Time
	logger *slog.Logger

	// slots admits a request to the token endpoint: a buffered channel of
	// [ExchangeSlots], taken for the length of one exchange or refresh.
	slots chan struct{}

	mu        sync.Mutex
	metadata  Metadata
	fetchedAt time.Time
	keys      *jwks.Set

	// discovering is the discovery fetch in progress, which every caller
	// that finds the document cold or stale waits on — see
	// [Provider.Metadata].
	discovering *discovery
}

// discovery is one fetch of the document, answered to everybody waiting on it.
type discovery struct {
	done     chan struct{}
	metadata Metadata
	err      error
}

// ExchangeSlots is how many requests this node has in flight to one identity
// provider's token endpoint at once — code exchanges and deactivation probes
// together.
//
// EIGHT, the design's figure, and what it is sized against is the morning's
// sign-in wave: at a hundred milliseconds an exchange, eight slots clear a
// 3,000-person company in about 37 seconds, each person waiting only for the
// exchanges ahead of theirs. What it buys is that this engine never shows a
// provider more than eight requests from one node, however many callbacks
// land at once — including a burst an unauthenticated caller can make by
// starting flights and calling back, each of which costs an exchange. No
// legitimate callback is REFUSED for it: a caller waits for a slot on its own
// request's context, for as long as that lives, and a browser that gives up
// has asked for nothing more.
const ExchangeSlots = 8

// NewProvider builds one. A nil client takes the identity provider's own from
// [httpx.IdentityProviderClient], whose connection cap is derived from
// [ExchangeSlots].
//
// ONE PROVIDER PER ISSUER PER PROCESS, which is what makes its slots the
// per-issuer bound: the engine builds one from Tier A's one `api.auth.oidc`
// block, and the sign-in surface and the probe share it.
func NewProvider(config Config, client *http.Client, now func() time.Time) *Provider {
	if client == nil {
		client = httpx.IdentityProviderClient(ExchangeTimeout)
	}
	if now == nil {
		now = time.Now
	}
	return &Provider{config: config, client: client, now: now,
		logger: log, slots: make(chan struct{}, ExchangeSlots)}
}

// log is where a provider reports what its discovery document says it cannot
// be relied on for — see [Metadata.Concerns].
var log = logging.Get("iam.oidc")

// WithLogger replaces where the provider reports, and returns it for chaining.
// CALLED ONCE, before the provider is used; a nil one keeps the default.
func (p *Provider) WithLogger(logger *slog.Logger) *Provider {
	if logger != nil {
		p.logger = logger
	}
	return p
}

// Config is the provider's configuration.
func (p *Provider) Config() Config { return p.config }

// Exchange redeems an authorization code, inside one of this provider's
// [ExchangeSlots] and over THIS PROVIDER'S OWN CLIENT, the one its discovery
// and its key set already use: the three requests go to one party, and a
// client of the caller's own gave that party a second timeout policy and a
// second trust store — the callback did exactly that, so a provider built
// with a client that trusted its issuer discovered and fetched keys fine and
// then failed every code exchange. The configuration's exchange is unexported
// so there is no way to the token endpoint that skips the slots.
//
// A caller waits for a slot on its own context; one that ends first is
// answered its context's error, having asked the provider nothing.
func (p *Provider) Exchange(ctx context.Context, tokenEndpoint, code, verifier string) (
	Tokens, error) {

	release, err := p.slot(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer release()
	return p.config.exchange(ctx, p.client, tokenEndpoint, code, verifier)
}

// Refresh exchanges a refresh token — the deactivation probe's one question —
// inside a slot and over the provider's own client, for [Provider.Exchange]'s
// reasons.
func (p *Provider) Refresh(ctx context.Context, tokenEndpoint, refresh string) (
	Tokens, error) {

	release, err := p.slot(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer release()
	return p.config.refresh(ctx, p.client, tokenEndpoint, refresh)
}

// slot waits for one of the provider's [ExchangeSlots], answering how to give
// it back, or the caller's own context ending first.
func (p *Provider) slot(ctx context.Context) (func(), error) {
	select {
	case p.slots <- struct{}{}:
		return func() { <-p.slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("oidc: wait for a turn at the provider's token "+
			"endpoint: %w", ctx.Err())
	}
}

// Metadata returns the discovery document, fetching it when the cache is cold
// or stale.
//
// # ONE FETCH HOWEVER MANY ASK, and it belongs to none of them
//
// The document is read from somebody else's host, so the lock is held to read
// the cache and to join or start a fetch, and never across the request —
// internal/jwks' rule one layer up. What that opens, every caller finding the
// cache cold at once, is closed by a SINGLE FLIGHT: the cache is cold at boot
// and stale every [MetadataTTL], and both land under load — a node restarted
// in the morning's sign-in wave, a day's expiry in the middle of one — where
// every start and callback in flight fetched the document for itself, a herd
// at the provider's metadata host from one node. The fetch runs on a context
// no caller can cancel, bounded by [ExchangeTimeout], and each caller waits on
// its own: run on the first caller's, one browser leaving mid sign-in would
// fail everybody waiting beside it.
func (p *Provider) Metadata(ctx context.Context) (Metadata, error) {
	p.mu.Lock()
	if cached, age := p.metadata, p.now().Sub(p.fetchedAt); cached.Issuer != "" &&
		age < MetadataTTL {

		p.mu.Unlock()
		return cached, nil
	}
	inflight := p.discovering
	if inflight == nil {
		inflight = &discovery{done: make(chan struct{})}
		p.discovering = inflight
		go p.discover(context.WithoutCancel(ctx), inflight)
	}
	p.mu.Unlock()

	select {
	case <-inflight.done:
		return inflight.metadata, inflight.err
	case <-ctx.Done():
		return Metadata{}, ctx.Err()
	}
}

// discover performs one flight's fetch and answers everybody waiting on it.
func (p *Provider) discover(ctx context.Context, inflight *discovery) {
	ctx, cancel := context.WithTimeout(ctx, ExchangeTimeout)
	defer cancel()
	fetched, err := p.fetch(ctx)

	p.mu.Lock()
	defer func() {
		p.discovering = nil
		p.mu.Unlock()
		close(inflight.done)
	}()
	if err != nil {
		inflight.err = err
		if p.metadata.Issuer != "" {
			// STALE METADATA BEATS REFUSING EVERY LOGIN. The endpoints
			// in it change on the order of never; the provider's
			// metadata host being briefly down is not a reason nobody
			// can sign in.
			inflight.metadata, inflight.err = p.metadata, nil
		}
		return
	}
	previous := p.metadata.JWKSURI
	p.metadata, p.fetchedAt = fetched, p.now()
	if p.keys == nil || fetched.JWKSURI != previous {
		// THE KEY SET IS FETCHED WITH THIS PROVIDER'S OWN CLIENT, not
		// one of its own: the two requests go to the same party over
		// the same transport, and a key source with a client of its
		// own would be a second timeout policy and a second trust
		// store for one provider.
		p.keys = jwks.New(jwks.Options{
			URL: fetched.JWKSURI, Now: p.now, Client: p.client,
		})
	}
	inflight.metadata = fetched
	// SAID ONCE PER FETCH, which is once a day: a concern is about the
	// provider, and every sign-in between two fetches would repeat it.
	for _, concern := range fetched.Concerns(p.config) {
		p.logger.WarnContext(ctx, "oidc_provider_metadata_concern",
			"issuer", fetched.Issuer, "field", concern.Field,
			"detail", concern.Detail)
	}
}

// Keys is the provider's cached key set, discovering first if it has to.
func (p *Provider) Keys(ctx context.Context) (Keys, error) {
	if _, err := p.Metadata(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keys == nil {
		return nil, fmt.Errorf("%w: the provider published no jwks_uri",
			ErrNotConfigured)
	}
	return p.keys, nil
}

// MetadataPath is where a provider serves its discovery document.
const MetadataPath = "/.well-known/openid-configuration"

// maxMetadataBytes bounds what a misbehaving provider can make this buffer.
const maxMetadataBytes = 1 << 20

func (p *Provider) fetch(ctx context.Context) (Metadata, error) {
	endpoint := strings.TrimSuffix(p.config.Issuer, "/") + MetadataPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: discovery request: %w", ErrNotConfigured, err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: reach %s: %w", ErrNotConfigured, endpoint, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("%w: %s answered %d", ErrNotConfigured,
			endpoint, res.StatusCode)
	}
	var doc Metadata
	if err := json.NewDecoder(io.LimitReader(res.Body, maxMetadataBytes)).Decode(&doc); err != nil {
		return Metadata{}, fmt.Errorf("%w: decode %s: %w", ErrNotConfigured, endpoint, err)
	}
	// THE DOCUMENT'S OWN ISSUER MUST BE THE ONE THAT SERVED IT. Every
	// token comparison afterwards is against the configured issuer, so a
	// document naming another one is either a misconfiguration or
	// somebody's redirect — and accepting it would let the metadata
	// decide what the tokens are checked against.
	if doc.Issuer != p.config.Issuer {
		return Metadata{}, fmt.Errorf("%w: %s serves metadata for issuer %q, "+
			"and `oidc.issuer` is %q", ErrNotConfigured, endpoint, doc.Issuer,
			p.config.Issuer)
	}
	for field, value := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"jwks_uri":               doc.JWKSURI,
	} {
		if value == "" {
			return Metadata{}, fmt.Errorf("%w: %s published no %s",
				ErrNotConfigured, endpoint, field)
		}
	}
	return doc, nil
}
