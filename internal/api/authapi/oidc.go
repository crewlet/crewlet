package authapi

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// flightCookieName is where the sealed login-in-progress rides.
//
// ITS OWN COOKIE rather than a field of the session one, because the two exist
// at opposite times: the flight is set before anybody is signed in and cleared
// the moment they are, and a session cookie carrying a login in progress would
// be a cookie whose meaning depends on which half of a round trip it is in.
const flightCookieName = "crewlet_oidc_flight"

// OIDCStart sends the browser to the provider.
//
// # It keeps its state in the BROWSER, sealed, and on no node
//
// A fleet serves logins from whichever ingress node a request lands on, so a
// flight held in a map on the node that started it is a login that fails
// whenever the callback reaches a different one. The sealed cookie is what
// makes a login begun on one node finishable on another, and what it seals —
// the PKCE verifier — is the value that must not be readable, which is why it
// is encrypted rather than signed.
func (s *Service) OIDCStart(w http.ResponseWriter, r *http.Request) {
	metadata, err := s.provider.Metadata(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_discovery_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, retryIdentity)
		return
	}
	redirect, sealed, err := s.provider.Config().Start(
		s.cipher, metadata.AuthorizationEndpoint, s.returnTo(r), s.now())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_start_failed", "error", err)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	http.SetCookie(w, s.flightCookie(sealed, s.now().Add(oidc.FlightTTL)))
	// A REDIRECT AND NOT A JSON BODY, because the caller is a BROWSER
	// following a link: a body would need a script to act on it, and the
	// page that starts a login is the one page a person may reach with
	// nothing loaded.
	http.Redirect(w, r, redirect, http.StatusFound)
}

// OIDCCallback finishes the round trip.
//
// # Linking is EXPLICIT, and an email match is never a link
//
// At most providers a person can set their own address, so an address the
// provider asserts is a claim the attacker controls — and the person it would
// link them to is whoever is most worth becoming. So a subject this estate
// does not already hold a credential for is REFUSED rather than provisioned or
// matched: somebody with an account links it deliberately, and there is no
// `auto_provision` setting because that is the same decision written as a
// field, and a field is how it ends up on by accident.
func (s *Service) OIDCCallback(w http.ResponseWriter, r *http.Request) {
	arrived := s.now()
	source := s.sourceOf(r)
	if !s.admit(w, r, source, types.FailOIDC) {
		return
	}
	// A ROUND TRIP THAT ENDS IN NOBODY is one failed attempt, whichever
	// check refused it. The subject is the provider's own, once an ID
	// token verified and there is one; before that nothing names anybody.
	attempt := authevents.Failure{Client: source, Method: types.FailOIDC}
	cookie, err := r.Cookie(flightCookieName)
	if err != nil || cookie.Value == "" {
		s.refuseSignIn(w, r, arrived, attempt, "no flight cookie")
		return
	}
	// THE FLIGHT IS CLEARED WHATEVER HAPPENS NEXT. A login that failed
	// must not leave a redeemable flight in the browser, and one that
	// succeeded has no more use for it.
	http.SetCookie(w, s.flightCookie("", time.Time{}))

	flight, err := oidc.Open(s.cipher, cookie.Value, s.now())
	if err != nil {
		s.refuseSignIn(w, r, arrived, attempt, "flight: "+err.Error())
		return
	}
	// THE STATE COMPARISON, and it is what stops a callback link somebody
	// was SENT from completing a login in their browser: the attacker
	// cannot know the value sealed in the cookie beside it.
	if r.URL.Query().Get("state") != flight.State {
		s.refuseSignIn(w, r, arrived, attempt, "state mismatch")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.refuseSignIn(w, r, arrived, attempt, "no authorization code")
		return
	}

	metadata, err := s.provider.Metadata(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_discovery_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, retryIdentity)
		return
	}
	tokens, err := s.provider.Exchange(r.Context(),
		metadata.TokenEndpoint, code, flight.Verifier)
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_exchange_failed", "error", err)
		s.refuseSignIn(w, r, arrived, attempt, "code exchange failed")
		return
	}
	keys, err := s.provider.Keys(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_keys_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, retryIdentity)
		return
	}
	claims, err := s.provider.Config().Verify(r.Context(), keys,
		tokens.IDToken, flight.Nonce, s.now())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_id_token_refused", "error", err)
		s.refuseSignIn(w, r, arrived, attempt, "id token: "+err.Error())
		return
	}

	held, err := s.personForSubject(r, claims)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	}
	attempt.Subject, attempt.Person = claims.Issuer+"|"+claims.Subject, held.ID
	if held.ID == "" || !stageAdmits(held.Stage) {
		// NO LINK, NO SIGN-IN. See this function's doc: an address the
		// provider asserts is not a link, and this is where that rule
		// is enforced rather than merely stated.
		s.refuseSignIn(w, r, arrived, attempt, "no linked credential for this subject")
		return
	}
	s.throttle.Flush(r.Context(), source)

	// THE GROUP MAPPING RIDES INSIDE THE SESSION, never onto the person's
	// own row: what a provider's groups confer is true for as long as that
	// assertion is, and writing it to the estate would outlive the
	// provider saying it.
	held.Grants = mergeGrants(held.Grants,
		s.boot.API.Auth.OIDC.GrantsFor(claims.Groups))
	// BACK TO WHERE THE LOGIN BEGAN, by redirect. The callback is a
	// browser following the provider's redirect, and it used to answer the
	// JSON body the password route does — which a browser renders as text
	// and goes nowhere from, with a dead `if flight.Return != ""` after it
	// that was meant to be this. The return path was checked at the start
	// to be a path on this deployment and was sealed into the flight since,
	// so it is not the caller's to change now.
	s.completeSignIn(w, r, held, signIn{
		method: types.SignInOIDC, acr: claims.ACR, redirect: flight.Return,
		refresh: tokens.Refresh,
	})
}

// personForSubject resolves the provider's subject to somebody this estate
// already holds a credential for.
func (s *Service) personForSubject(r *http.Request, claims oidc.Claims) (
	iamdomain.Sighting, error) {

	// THE SUBJECT IS BLINDED for the address's reason and the only one: it
	// identifies a person at a third party, which is what makes it
	// personal data rather than an opaque handle. Under its OWN class, so
	// a subject can never collide with an address — they are different
	// namespaces and a shared derivation would let one resolve as the
	// other.
	blind, err := s.blinder.Subject(claims.Issuer, claims.Subject)
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_blind_failed", "error", err)
		return iamdomain.Sighting{}, err
	}
	held, err := s.directory.PersonByEmailBlind(r.Context(), blind)
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_lookup_failed", "error", err)
		return iamdomain.Sighting{}, err
	}
	return held, nil
}

// mergeGrants is the union of what a person carries and what their provider
// groups confer.
//
// A UNION AND NOT A REPLACEMENT, because the two answer different questions: a
// person's own grants are what this company gave them and the mapped ones are
// what their directory membership says today. Replacing would make a group
// removal silently revoke something an administrator granted here.
//
// The CEILING is applied downstream, per node per request, so nothing this
// composes can exceed what the deployment allows.
func mergeGrants(held, mapped []iam.Grant) []iam.Grant {
	out := make([]iam.Grant, 0, len(held)+len(mapped))
	out = append(out, held...)
	for _, g := range mapped {
		if !containsGrant(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func containsGrant(set []iam.Grant, want iam.Grant) bool {
	for _, g := range set {
		if g == want {
			return true
		}
	}
	return false
}

// returnTo is where the browser goes once the login completes.
//
// # An open redirect is the ordinary way a sign-in flow leaks a credential
//
// The value is a query parameter, which is to say the caller's, so it is
// refused unless it is a PATH on this deployment: no scheme, no host, and a
// leading single slash. `//evil.example.com` is a protocol-relative URL that
// browsers follow off-site, which is exactly the shape a naive "starts with /"
// check admits.
func (s *Service) returnTo(r *http.Request) string {
	want := strings.TrimSpace(r.URL.Query().Get("return_to"))
	if want == "" {
		return "/dashboard"
	}
	if !strings.HasPrefix(want, "/") || strings.HasPrefix(want, "//") {
		return "/dashboard"
	}
	parsed, err := url.Parse(want)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "/dashboard"
	}
	return want
}

// flightCookie builds the sealed login-in-progress cookie.
//
// SAME-SITE LAX AND NOT STRICT, and it is the one cookie here that has to be:
// the callback is a cross-site navigation from the provider, and Strict would
// withhold the cookie on exactly the request that needs it. Lax sends it on a
// top-level GET, which is what a redirect back from a provider is.
func (s *Service) flightCookie(value string, expires time.Time) *http.Cookie {
	cookie := &http.Cookie{
		Name: flightCookieName, Value: value, Path: "/auth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure:  strings.HasPrefix(s.boot.API.ExternalBase(), "https://"),
		Expires: expires,
	}
	if value == "" {
		cookie.MaxAge = -1
	}
	return cookie
}
