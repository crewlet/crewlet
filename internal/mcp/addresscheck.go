package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// CHECKING AN ADDRESS BEFORE IT IS ADDED: what a remote server at it is, and
// how it is signed in to.
//
// A check is a [Test] of the address with nothing configured: no header, no
// prefix, no override. Where the server answers its handshake and lists its
// tools to a request that carries no credential, it needs no sign-in. Where it
// answers 401 with a Bearer challenge, it wants an OAuth sign-in, and the
// check reads how one is made: the server's protected-resource metadata
// (RFC 9728), at the address its challenge names or at the well-known address
// on its own origin, and the metadata of the first authorization server that
// names (RFC 8414, or OpenID Connect discovery where only that is published).
// A server that refuses without a Bearer challenge wants a credential in a
// header.
//
// # Whose address each request goes to
//
// The address a person typed is the operator's, and is dialled as the server
// would be once added, a private address included (ADR-0072): typing it is
// what adding it would make the engine dial. Every other address the check
// reaches is one the SERVER named, in its challenge or its metadata, and is
// fetched through the public-address guard ([httpx.Guarded]) unless it is on
// the typed address's own origin, which the operator already chose. A
// metadata address on another origin is https or it is not fetched, no fetch
// follows a redirect, every body is read to [maxMetadataBody] and no further,
// and every fetch is bounded by [metadataFetchBudget]. Nothing the check sends
// carries a credential: it has none.
//
// An address the guard refuses ends the discovery with [FailBlockedAddress]:
// the server's sign-in points somewhere the engine does not follow, which its
// vendor fixes.

// AddressCheck is what checking an address found.
type AddressCheck struct {
	// Test is the address's walk, with no credential sent.
	Test TestReport
	// Auth is the sign-in the server asks for: [catalogue.AuthNone] for one
	// that listed its tools to no credential, [catalogue.AuthOAuth] for one
	// that challenged for a Bearer token and publishes protected-resource
	// metadata, [catalogue.AuthHeaders] for one that refused another way or
	// publishes none; "" where the check did not reach an answer to tell by.
	Auth catalogue.AuthMethod
	// OAuth is what its sign-in metadata says, nil unless it challenged for
	// a Bearer token.
	OAuth *OAuthDiscovery
}

// OAuthDiscovery is what a server's sign-in metadata says. WORDS AND
// ADDRESSES A THIRD PARTY PUBLISHED: whoever shows them says whose they are,
// and every address in it is written with its credentials withheld
// ([redact.URL]).
type OAuthDiscovery struct {
	// ResourceMetadata is the address the protected-resource metadata was
	// read from, "" where none was.
	ResourceMetadata string
	// Resource is the resource the metadata names.
	Resource string
	// AuthorizationServers are the issuers it names, at most
	// [maxAuthorizationServers].
	AuthorizationServers []string
	// Issuer is the authorization server whose metadata was read, "" where
	// none was.
	Issuer string
	// Scopes are the scopes the metadata lists, at most [maxScopes].
	Scopes []string
	// Client is how the engine would register at the authorization server:
	// a client metadata document where it says it reads one, dynamic
	// registration where it offers an endpoint for it, and a client an
	// administrator registers beforehand otherwise. "" where its metadata
	// was not read.
	Client catalogue.OAuthClient
	// PKCE is whether the authorization server offers S256, the one PKCE
	// method an MCP client may use.
	PKCE bool
	// Failure is why the discovery stopped short, nil where it read both
	// documents.
	Failure *Failure

	// Endpoints are the addresses a sign-in is made at, as the documents
	// published them: nil unless both documents were read. FOR THE ENGINE'S
	// OWN SIGN-IN (internal/mcp/oauth), never shown: an address here is
	// written as published, its query included, and every request the
	// engine sends to one goes through [ServerNamedClient].
	Endpoints *OAuthEndpoints
}

// OAuthEndpoints are where a server's sign-in is made, read from its
// protected-resource metadata (RFC 9728) and its authorization server's
// metadata (RFC 8414): addresses a third party published, each dialled by the
// engine through [ServerNamedClient] alone.
type OAuthEndpoints struct {
	// Server is the server's own address, the one the documents were read
	// for: every other address here is judged against its origin.
	Server string
	// Resource is the resource the protected-resource metadata names, which
	// a token is asked for (RFC 8707).
	Resource string
	// Issuer is the authorization server, as its metadata names itself.
	Issuer string
	// Authorization is where a person's browser is sent to consent, Token
	// where a code and a refresh token are exchanged, Registration where a
	// client registers (RFC 7591, "" where none is offered) and Revocation
	// where a token is revoked (RFC 7009, "" where none is offered).
	Authorization string
	Token         string
	Registration  string
	Revocation    string
	// ClientIDMetadataDocument is whether the authorization server reads a
	// client's metadata document from its client id.
	ClientIDMetadataDocument bool
	// PKCEMethods are the code challenge methods it offers.
	PKCEMethods []string
	// TokenAuthMethods are how a client may authenticate at its token
	// endpoint, nil where it says nothing (which RFC 8414 reads as
	// client_secret_basic).
	TokenAuthMethods []string
	// IssuerInResponse is whether it names itself in each authorization
	// response (RFC 9207), which a sign-in then requires.
	IssuerInResponse bool
	// Scopes are the scopes the resource lists, else the authorization
	// server's, at most [maxScopes]; ResourceScopes is whether they are the
	// resource's own.
	Scopes         []string
	ResourceScopes bool
	// OfflineAccess is whether the authorization server lists the
	// offline_access scope (OpenID Connect's ask for a refresh token).
	OfflineAccess bool
}

// The bounds of an address check.
const (
	// addressCheckStage bounds each stage of an address check's walk.
	// REASONED: a remote MCP server answers its handshake in well under a
	// second, and a serverless one starting cold in a few; a person is
	// waiting on the add flow, so twenty seconds is past any server that
	// will answer at all.
	addressCheckStage = 20 * time.Second
	// metadataFetchBudget bounds one fetch of a metadata document, its body
	// included. REASONED: a static JSON document from a vendor's edge; ten
	// seconds is several round trips past a slow one.
	metadataFetchBudget = 10 * time.Second
	// maxMetadataBody bounds a metadata document. REASONED: the largest
	// published authorization server metadata (an identity provider's, with
	// every claim and algorithm listed) is around 10 KiB; 64 KiB keeps any
	// real one whole and refuses a server's choice of what this process
	// holds.
	maxMetadataBody = 64 << 10
	// maxAuthorizationServers and maxScopes bound the lists an answer
	// carries of a document's. REASONED: a resource names one authorization
	// server in practice and a vendor a few dozen scopes; past these a
	// document is not one a person reads whole.
	maxAuthorizationServers = 5
	maxScopes               = 50
	// maxMetadataWord bounds each string an answer carries of a document's.
	maxMetadataWord = 512
)

// CheckAddress checks the remote server at raw before it is added: see the
// file's comment. The whole check is bounded by ctx.
func CheckAddress(ctx context.Context, raw string, log *slog.Logger) AddressCheck {
	spec := Spec{Name: "address-check", Transport: TransportHTTP, URL: raw,
		StartupTimeout: addressCheckStage, RequestTimeout: addressCheckStage}
	out := AddressCheck{Test: Test(ctx, spec, log)}
	if out.Test.Serving() {
		out.Auth = catalogue.AuthNone
		return out
	}
	switch out.Test.Failure.Class {
	case FailNeedsSignIn:
		out.OAuth = discoverOAuth(ctx, raw, out.Test.challenge)
		out.Auth = catalogue.AuthHeaders
		if out.OAuth.ResourceMetadata != "" {
			out.Auth = catalogue.AuthOAuth
		}
	case FailCredentialRejected, FailMissingPermission:
		out.Auth = catalogue.AuthHeaders
	}
	return out
}

// discoverOAuth reads the sign-in metadata of the server at raw, whose 401
// carried challenge.
func discoverOAuth(ctx context.Context, raw, challenge string) *OAuthDiscovery {
	out := &OAuthDiscovery{AuthorizationServers: []string{}, Scopes: []string{}}
	typed, err := url.Parse(raw)
	if err != nil {
		out.Failure = discoveryFailure("", err)
		return out
	}
	f := fetcher{server: raw}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	read, err := f.first(ctx, resourceMetadataAddresses(typed, challenge), &prm)
	if err != nil {
		out.Failure = discoveryFailure("the server's protected-resource metadata", err)
		return out
	}
	out.ResourceMetadata = redact.URL(read)
	out.Resource = word(prm.Resource)
	out.Scopes = words(prm.ScopesSupported)
	resourceScopes := len(out.Scopes) > 0
	// THE RESOURCE IS THE SERVER'S OWN (RFC 9728, 3.3): metadata naming
	// another resource would have a token asked for, and audience-bound to,
	// somebody else's server, and nothing in it is used.
	if !ResourceServes(prm.Resource, raw) {
		out.Failure = &Failure{Class: FailNotMCP, At: time.Now().UTC(), Said: fmt.Sprintf(
			"the server's protected-resource metadata names the resource %s, which is not the "+
				"server at %s", redact.URL(word(prm.Resource)), redact.URL(raw))}
		return out
	}
	for _, issuer := range prm.AuthorizationServers[:min(len(prm.AuthorizationServers), maxAuthorizationServers)] {
		out.AuthorizationServers = append(out.AuthorizationServers, redact.URL(word(issuer)))
	}
	if len(prm.AuthorizationServers) == 0 {
		out.Failure = &Failure{Class: FailNotMCP, At: time.Now().UTC(),
			Said: "the server's protected-resource metadata names no authorization server"}
		return out
	}
	issuer := prm.AuthorizationServers[0]
	var asm struct {
		Issuer                        string   `json:"issuer"`
		AuthorizationEndpoint         string   `json:"authorization_endpoint"`
		TokenEndpoint                 string   `json:"token_endpoint"`
		RegistrationEndpoint          string   `json:"registration_endpoint"`
		RevocationEndpoint            string   `json:"revocation_endpoint"`
		ClientIDMetadataDocument      bool     `json:"client_id_metadata_document_supported"`
		CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
		TokenEndpointAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
		IssuerParameterSupported      bool     `json:"authorization_response_iss_parameter_supported"`
		ScopesSupported               []string `json:"scopes_supported"`
	}
	addresses, err := authorizationServerAddresses(issuer)
	if err == nil {
		_, err = f.first(ctx, addresses, &asm)
	}
	if err != nil {
		out.Failure = discoveryFailure("the authorization server's metadata", err)
		return out
	}
	// THE ISSUER IT NAMES IS THE ONE ASKED FOR (RFC 8414, 3.3): metadata
	// naming another is somebody else's, and nothing in it is used.
	if strings.TrimSuffix(asm.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		out.Failure = &Failure{Class: FailNotMCP, At: time.Now().UTC(), Said: fmt.Sprintf(
			"the authorization server's metadata names the issuer %s, not %s, which asked for it",
			redact.URL(word(asm.Issuer)), redact.URL(word(issuer)))}
		return out
	}
	out.Issuer = redact.URL(word(issuer))
	if len(out.Scopes) == 0 {
		out.Scopes = words(asm.ScopesSupported)
	}
	switch {
	case asm.ClientIDMetadataDocument:
		out.Client = catalogue.ClientCIMD
	case asm.RegistrationEndpoint != "":
		out.Client = catalogue.ClientDCR
	default:
		out.Client = catalogue.ClientOwn
	}
	for _, m := range asm.CodeChallengeMethodsSupported {
		if m == "S256" {
			out.PKCE = true
		}
	}
	out.Endpoints = &OAuthEndpoints{
		Server: raw, Resource: prm.Resource, Issuer: asm.Issuer,
		Authorization: asm.AuthorizationEndpoint, Token: asm.TokenEndpoint,
		Registration: asm.RegistrationEndpoint, Revocation: asm.RevocationEndpoint,
		ClientIDMetadataDocument: asm.ClientIDMetadataDocument,
		PKCEMethods:              words(asm.CodeChallengeMethodsSupported),
		TokenAuthMethods:         words(asm.TokenEndpointAuthMethods),
		IssuerInResponse:         asm.IssuerParameterSupported,
		Scopes:                   slices.Clone(out.Scopes),
		ResourceScopes:           resourceScopes,
		OfflineAccess:            slices.Contains(asm.ScopesSupported, "offline_access"),
	}
	return out
}

// ResourceServes reports whether resource, as a server's protected-resource
// metadata names it, is the server at raw: the same origin, and a path that
// is the server's own or one above it. RFC 9728 asks for the identifier the
// metadata was read for; a vendor that names its origin for a server under a
// path is naming the same resource, and one naming another origin or another
// path is not.
func ResourceServes(resource, raw string) bool {
	r, err := url.Parse(resource)
	if err != nil || r.Host == "" || r.RawQuery != "" || r.Fragment != "" {
		return false
	}
	server, err := url.Parse(raw)
	if err != nil || !sameOrigin(r, originOfURL(server)) {
		return false
	}
	scope := strings.TrimSuffix(r.EscapedPath(), "/")
	path := strings.TrimSuffix(server.EscapedPath(), "/")
	return scope == "" || path == scope || strings.HasPrefix(path, scope+"/")
}

// ServerNamedClient is the client a request to address, which a server at
// server or its metadata named, is sent with, and the address parsed: on the
// server's own origin, the client its address is dialled with (the
// operator's, ADR-0072); on any other, https alone and through the
// public-address guard ([httpx.Guarded]). Neither follows a redirect, and
// each request is bounded by budget.
//
// THE ONE RULE for every address a sign-in reaches, the metadata this check
// reads and the token, registration and revocation endpoints a sign-in
// sends to, so no request a remote party named is dialled another way.
func ServerNamedClient(server, address string, budget time.Duration) (*http.Client, *url.URL, error) {
	typed, err := url.Parse(server)
	if err != nil || typed.Host == "" {
		return nil, nil, fmt.Errorf("the server %s is not an address", redact.URL(word(server)))
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return nil, nil, &badDocument{address: redact.URL(word(address)), class: FailNotMCP,
			why: "is not an address"}
	}
	client := httpx.Client(budget)
	if !sameOrigin(u, originOfURL(typed)) {
		// THE SERVER NAMED IT: an address on another origin is reached
		// through the guard, over TLS or not at all (ADR-0072).
		if !strings.EqualFold(u.Scheme, "https") {
			return nil, nil, &badDocument{address: redact.URL(u.String()), class: FailNotMCP,
				why: "is not an https address, and an address a server's sign-in names on another " +
					"origin is reached over https alone"}
		}
		client = httpx.Guarded(budget)
	}
	// NO REDIRECT IS FOLLOWED: a document or an endpoint is at the address
	// that names it, and a redirect would be one more address the server
	// chose for the engine to reach.
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &noRedirects, u, nil
}

// discoveryFailure is a failed discovery as the answer carries it: classed by
// what the error is, a guard's refusal [FailBlockedAddress] among them, and a
// document that is not one as [FailNotMCP].
func discoveryFailure(what string, err error) *Failure {
	class := ClassOf(err)
	var bad *badDocument
	if class == "" && errors.As(err, &bad) {
		class = bad.class
	}
	said := err.Error()
	if what != "" {
		said = "reading " + what + ": " + said
	}
	return &Failure{Class: class, Said: said, At: time.Now().UTC()}
}

// badDocument is a metadata address that answered something other than the
// document asked for, classed by its status where it gave one.
type badDocument struct {
	address string
	class   FailureClass
	why     string
}

func (b *badDocument) Error() string { return b.address + " " + b.why }

// resourceMetadataAddresses is where a server's protected-resource metadata
// is looked for, in order: the address its challenge names, else the
// well-known address on its own origin for its path, then for its root
// (RFC 9728, 3.1, and the MCP authorization specification's fallback).
func resourceMetadataAddresses(typed *url.URL, challenge string) []string {
	if named := challengeParam(challenge, "resource_metadata"); named != "" {
		return []string{named}
	}
	root := url.URL{Scheme: typed.Scheme, Host: typed.Host, Path: "/.well-known/oauth-protected-resource"}
	out := []string{}
	if p := strings.TrimSuffix(typed.EscapedPath(), "/"); p != "" {
		out = append(out, root.String()+p)
	}
	return append(out, root.String())
}

// authorizationServerAddresses is where an issuer's metadata is looked for,
// in order: the OAuth well-known address (RFC 8414, 3.1), then the OpenID
// Connect one with the issuer's path inserted and appended, as the MCP
// authorization specification asks a client to try them.
func authorizationServerAddresses(issuer string) ([]string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("the authorization server %s is not an address", redact.URL(word(issuer)))
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	base := url.URL{Scheme: u.Scheme, Host: u.Host}
	out := []string{base.String() + "/.well-known/oauth-authorization-server" + path,
		base.String() + "/.well-known/openid-configuration" + path}
	if path != "" {
		out = append(out, base.String()+path+"/.well-known/openid-configuration")
	}
	return out, nil
}

// challengeParam is the value of one auth-param of a Bearer challenge
// (RFC 9110, 11.6.1), "" where it has none.
func challengeParam(challenge, name string) string {
	scheme, params, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	for params != "" {
		var key, value string
		params = strings.TrimLeft(params, " ,")
		key, params, _ = strings.Cut(params, "=")
		key = strings.TrimSpace(key)
		params = strings.TrimLeft(params, " ")
		if strings.HasPrefix(params, `"`) {
			end := strings.IndexByte(params[1:], '"')
			if end < 0 {
				return ""
			}
			value, params = params[1:end+1], params[end+2:]
		} else {
			value, params, _ = strings.Cut(params, ",")
		}
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// fetcher reads metadata documents for one check.
type fetcher struct {
	// server is the typed address: a document on its origin is fetched as
	// the operator's address is, and any other through the guard
	// ([ServerNamedClient]).
	server string
}

// errNoDocument is a list of addresses none of which answered a document.
var errNoDocument = errors.New("no address answered the document")

// first reads the first of addresses that answers a document into v, and
// says which. A document that answers but cannot be read stops the search:
// the server published one, and it is wrong.
func (f fetcher) first(ctx context.Context, addresses []string, v any) (string, error) {
	last := errNoDocument
	for _, address := range addresses {
		err := f.fetch(ctx, address, v)
		if err == nil {
			return address, nil
		}
		last = err
		var bad *badDocument
		if !errors.As(err, &bad) || bad.class != FailNoEndpoint {
			return "", err
		}
	}
	return "", last
}

// fetch reads one metadata document into v.
func (f fetcher) fetch(ctx context.Context, address string, v any) error {
	noRedirects, u, err := ServerNamedClient(f.server, address, metadataFetchBudget)
	if err != nil {
		return err
	}
	shown := redact.URL(u.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return &badDocument{address: shown, class: FailNotMCP, why: "is not an address a request can be sent to"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := noRedirects.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", shown, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &badDocument{address: shown, class: FailNoEndpoint, why: "answered 404, publishing no document there"}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &badDocument{address: shown, class: FailNotMCP,
			why: fmt.Sprintf("answered %d, a redirect the engine does not follow for a sign-in's metadata", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		class := classOfStatus(resp.StatusCode, false, false, false)
		if class == "" {
			class = FailNotMCP
		}
		return &badDocument{address: shown, class: class, why: fmt.Sprintf("answered %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", shown, err)
	}
	if len(body) > maxMetadataBody {
		return &badDocument{address: shown, class: FailNotMCP,
			why: fmt.Sprintf("answered a document over %d KiB, larger than any sign-in's metadata", maxMetadataBody>>10)}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &badDocument{address: shown, class: FailNotMCP, why: "answered something that is not a JSON document"}
	}
	return nil
}

// unwrapURLError is a client's error without the url.Error around it, whose
// sentence repeats the address unredacted; the caller names the address
// itself.
func unwrapURLError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// word is s cut to [maxMetadataWord], on a character boundary.
func word(s string) string { return textcut.Bytes(s, maxMetadataWord) }

// words is the first [maxScopes] of in, each cut by [word], and an empty list
// for none: the most of any list a metadata document gives that an answer or
// a sign-in carries.
func words(in []string) []string {
	n := maxScopes
	out := make([]string, 0, min(len(in), n))
	for _, s := range in[:min(len(in), n)] {
		out = append(out, word(s))
	}
	return out
}
