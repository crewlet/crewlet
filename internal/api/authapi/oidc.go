package authapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
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
// `?invite=<id>` starts a REDEMPTION rather than a sign-in: the invitation and
// the login the person chose (`?login=`, or the one the invitation's page
// proposed) are checked here and sealed into the flight, and the callback
// enrols the person the invitation creates and links the subject the provider
// comes back with to them — see invite_oidc.go.
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
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	want := oidc.Flight{Return: s.returnTo(r)}
	if invite := r.URL.Query().Get("invite"); invite != "" {
		var ok bool
		if want, ok = s.redemptionFlight(w, r, want, invite); !ok {
			return
		}
	}
	redirect, sealed, err := s.provider.Config().Start(
		s.cipher, metadata.AuthorizationEndpoint, want, s.now())
	if err != nil {
		// A FAULT AND NOT AN OUTAGE, so a 500 rather than a 503 asking to
		// be retried: what fails here is this node's own configuration
		// (no keyring, a provider block that does not validate, a
		// discovery document naming an authorization endpoint that is not
		// a url) or its own randomness and cipher — none of which clears
		// by waiting two seconds. What does clear by waiting, the
		// provider's discovery being unreachable, answered above.
		log.ErrorContext(r.Context(), "api_oidc_start_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
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
// does not already hold a LINK for is REFUSED rather than provisioned or
// matched, and a link is made in exactly two ways: an invitation redeemed
// through this same round trip (the flight carries it, and the callback
// enrols and links in one sequence — [Service.redeemThroughProvider]), or an
// administrator pinning it. There is no `auto_provision` setting, because
// that is the same decision written as a field, and a field is how it ends up
// on by accident.
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
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
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
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	claims, err := s.provider.Config().Verify(r.Context(), keys,
		tokens.IDToken, flight.Nonce, s.now())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_id_token_refused", "error", err)
		s.refuseSignIn(w, r, arrived, attempt, "id token: "+err.Error())
		return
	}

	attempt.Subject = claims.Issuer + "|" + claims.Subject
	if flight.Invite != "" {
		s.redeemThroughProvider(w, r, arrived, attempt, flight, claims,
			tokens.Refresh)
		return
	}
	held, err := s.personForSubject(r, claims)
	if errors.Is(err, iamdomain.ErrSubjectAmbiguous) {
		// A CONFLICT AND NOT AN OUTAGE: a restore left this subject
		// linked to two people, and no retry clears it — an operator
		// unlinking one of them does. It used to answer 503, which a
		// browser retries at the provider for ever.
		// A FAILED ATTEMPT on the trail naming the subject, and NOT a
		// throttle failure: the caller proved the subject to the provider,
		// so this is nobody guessing.
		attempt.Subject = claims.Issuer + "|" + claims.Subject
		s.refuseSubjectConflict(w, r, attempt, "this identity provider "+
			"account is linked to more than one person in this company; an "+
			"administrator has to remove one of the links", err)
		return
	}
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	attempt.Person = held.ID
	if held.ID == "" || !stageAdmits(held.Stage) {
		// NO LINK, NO SIGN-IN. See this function's doc: an address the
		// provider asserts is not a link, and this is where that rule
		// is enforced rather than merely stated.
		s.refuseSignIn(w, r, arrived, attempt, "no linked credential for this subject")
		return
	}
	s.throttle.Flush(r.Context(), source)

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
		// THE GROUP MAPPING RIDES INSIDE THE SESSION, never onto the
		// person's own row: what a provider's groups confer is true for
		// as long as that assertion is, and writing it to the estate
		// would outlive the provider saying it. It used to be merged into
		// this sighting and then dropped, since nothing downstream reads
		// a sighting's grants — so no mapping ever conferred anything.
		groupGrants: s.boot.API.Auth.OIDC.GrantsFor(claims.Groups),
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
	blinder, err := s.blinder.Blinder(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_blind_failed", "error", err)
		return iamdomain.Sighting{}, err
	}
	blind, err := blinder.Subject(claims.Issuer, claims.Subject)
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_blind_failed", "error", err)
		return iamdomain.Sighting{}, err
	}
	// THE LINK, NEVER THE PERSON ROW'S ADDRESS BLIND: the two are
	// different namespaces by construction, so a subject looked up as an
	// address matches nobody and every provider sign-in is refused.
	held, err := s.directory.PersonBySubjectBlind(r.Context(), blind, s.now())
	if errors.Is(err, iamdomain.ErrSubjectAmbiguous) {
		// THE CALLER ANSWERS IT, as a conflict: see OIDCCallback.
		return iamdomain.Sighting{}, err
	}
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_lookup_failed", "error", err)
		return iamdomain.Sighting{}, err
	}
	return held, nil
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
