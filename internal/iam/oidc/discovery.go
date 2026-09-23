package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/jwks"
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
	EndSessionEndpoint    string `json:"end_session_endpoint"`

	// ScopesSupported and IDTokenSigningAlgValuesSupported are read to
	// REPORT rather than to decide. A provider that does not advertise
	// `offline_access` is one whose deactivation probe will never work,
	// and saying so at validation is better than an operator discovering
	// it when somebody's off-boarding takes a week.
	ScopesSupported    []string `json:"scopes_supported"`
	SigningAlgorithms  []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported    []string `json:"claims_supported"`
	ResponseTypes      []string `json:"response_types_supported"`
	CodeChallengeTypes []string `json:"code_challenge_methods_supported"`
}

// SupportsOfflineAccess reports whether the deactivation probe can work at
// all against this provider.
func (m Metadata) SupportsOfflineAccess() bool {
	for _, scope := range m.ScopesSupported {
		if scope == "offline_access" {
			return true
		}
	}
	return false
}

// SupportsS256 reports whether the provider advertises the PKCE
// transformation this engine uses.
//
// ADVISORY, and deliberately not a refusal: the field is optional, plenty of
// providers implement S256 without advertising it, and a login refused because
// a document omitted a list is an outage caused by metadata.
func (m Metadata) SupportsS256() bool {
	for _, method := range m.CodeChallengeTypes {
		if method == "S256" {
			return true
		}
	}
	return len(m.CodeChallengeTypes) == 0
}

// Provider is a discovered identity provider, with its key set cached beside
// its metadata.
//
// SAFE FOR CONCURRENT USE.
type Provider struct {
	config Config
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	metadata  Metadata
	fetchedAt time.Time
	keys      *jwks.Set
}

// NewProvider builds one. A nil client takes one from [httpx].
func NewProvider(config Config, client *http.Client, now func() time.Time) *Provider {
	if client == nil {
		client = httpx.Client(ExchangeTimeout)
	}
	if now == nil {
		now = time.Now
	}
	return &Provider{config: config, client: client, now: now}
}

// Config is the provider's configuration.
func (p *Provider) Config() Config { return p.config }

// Exchange redeems an authorization code over THIS PROVIDER'S OWN CLIENT, the
// one its discovery and its key set already use: the three requests go to one
// party, and a caller reaching [Config.Exchange] with a client of its own — or
// with none, which takes a fresh default — gives that party a second timeout
// policy and a second trust store. The callback did exactly that, so a
// provider built with a client that trusted its issuer discovered and fetched
// keys fine and then failed every code exchange.
func (p *Provider) Exchange(ctx context.Context, tokenEndpoint, code, verifier string) (
	Tokens, error) {

	return p.config.Exchange(ctx, p.client, tokenEndpoint, code, verifier)
}

// Metadata returns the discovery document, fetching it when the cache is cold
// or stale.
func (p *Provider) Metadata(ctx context.Context) (Metadata, error) {
	p.mu.Lock()
	cached, age := p.metadata, p.now().Sub(p.fetchedAt)
	p.mu.Unlock()
	if cached.Issuer != "" && age < MetadataTTL {
		return cached, nil
	}

	// THE FETCH IS OFF THE LOCK, for internal/jwks' reason one layer up:
	// this is a request to somebody else's host, and holding a mutex
	// across it serialises every sign-in behind one slow provider. Two
	// nodes racing here cost one extra request and write the same
	// document, which is a cost worth paying to keep the lock short —
	// unlike the key set, where a rotation can make the race a herd.
	fetched, err := p.fetch(ctx)
	if err != nil {
		if cached.Issuer != "" {
			// STALE METADATA BEATS REFUSING EVERY LOGIN. The endpoints
			// in it change on the order of never; the provider's
			// metadata host being briefly down is not a reason nobody
			// can sign in.
			return cached, nil
		}
		return Metadata{}, err
	}
	p.mu.Lock()
	p.metadata, p.fetchedAt = fetched, p.now()
	if p.keys == nil || fetched.JWKSURI != cached.JWKSURI {
		// THE KEY SET IS FETCHED WITH THIS PROVIDER'S OWN CLIENT, not
		// one of its own: the two requests go to the same party over
		// the same transport, and a key source with a client of its
		// own would be a second timeout policy and a second trust
		// store for one provider.
		p.keys = jwks.New(jwks.Options{
			URL: fetched.JWKSURI, Now: p.now, Client: p.client,
		})
	}
	p.mu.Unlock()
	return fetched, nil
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
