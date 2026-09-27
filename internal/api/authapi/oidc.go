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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// flightCookieBase is the name the sealed login-in-progress rides under,
// before the prefix — see [Service.flightCookieName].
//
// ITS OWN COOKIE rather than a field of the session one, because the two exist
// at opposite times: the flight is set before anybody is signed in and cleared
// the moment they are, and a session cookie carrying a login in progress would
// be a cookie whose meaning depends on which half of a round trip it is in.
const flightCookieBase = "crewlet_oidc_flight"

// flightCookieName is the name the flight is set and read under on this
// deployment: `__Host-crewlet_oidc_flight` wherever the session cookie takes
// the prefix, the bare name on plain http — [session.NameFor], one rule for
// every cookie a browser is handed.
//
// THE PREFIX IS WHY IT IS ONE NAME, READ AS ISSUED. A browser sets a
// `__Host-` cookie only from this exact host, Secure, at `Path=/`, so a
// sibling host cannot write one; the bare name any sibling can write, with a
// Domain covering this one. Read under both names on https, a flight another
// host planted — one its author began, for the provider account the browser
// happens to hold — would be the one this callback finished. A flight lives
// ten minutes, so a deployment moving from http to https costs a login begun
// in those minutes its restart and nothing more.
func (s *Service) flightCookieName() string {
	return session.NameFor(s.boot.API.ExternalBase(), flightCookieBase)
}

// OIDCStart sends the browser to the provider: an ordinary sign-in, or — with
// `?step_up=<window>` — a signed-in person confirming who they are, inside the
// window a `step_up_required` refusal named.
//
// # It keeps its state in the BROWSER, sealed, and on no node
//
// A fleet serves logins from whichever ingress node a request lands on, so a
// flight held in a map on the node that started it is a login that fails
// whenever the callback reaches a different one. The sealed cookie is what
// makes a login begun on one node finishable on another, and what it seals —
// the PKCE verifier — is the value that must not be readable, which is why it
// is encrypted rather than signed.
//
// # A link any page can send a browser to, so it redeems nothing
//
// A GET is what a cross-site page can cause, and the round trip it starts
// signs in whoever the provider says is at the browser — which is why a
// sign-in and a confirmation are safe here: at worst they sign a person in as
// themselves. A REDEMPTION is not, because its callback pins the account the
// provider comes back with to the person an invitation creates, so it starts
// from the invitation's own page by a POST the origin check covers
// ([Service.StartProviderRedemption]). `?invite=` is REFUSED here rather than
// ignored: ignored, an outdated link would start an ordinary sign-in and fail a
// whole provider round trip later as an account nobody is linked to.
func (s *Service) OIDCStart(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Has("invite") {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "an invitation is redeemed through the " +
				"identity provider from its own page, by POST " +
				auth.AuthInvitePrefix + "{id}" + providerRedemption +
				" — never by a link, which any site could send a browser to"})
		return
	}
	want := oidc.Flight{Return: returnPath(query.Get("return_to"))}
	if query.Has("step_up") {
		var ok bool
		if want, ok = s.stepUpFlight(w, r, want,
			iam.Recency(query.Get("step_up"))); !ok {
			return
		}
	}
	s.launch(w, r, want, http.StatusFound)
}

// launch seals want into a flight and sends the browser to the provider,
// answering status: 302 where a link was followed, and 303 where a form was
// posted, which is what tells a browser to follow it with a GET.
//
// THE PROVIDER IS ASKED ONLY ONCE THE REQUEST IS ONE THIS SURFACE WOULD
// SERVE, so a refusal a caller can act on — who they are, which invitation,
// which login — is never hidden behind an outage at somebody else's provider.
func (s *Service) launch(w http.ResponseWriter, r *http.Request, want oidc.Flight,
	status int) {

	metadata, err := s.provider.Metadata(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_discovery_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
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
	// A REDIRECT AND NOT A JSON BODY, because the caller is a BROWSER: a
	// body would need a script to act on it, and a script's fetch cannot
	// follow a redirect to the provider's cross-origin page at all.
	http.Redirect(w, r, redirect, status)
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
	source := s.sourceOf(r)
	// ON NO CURVE: before an ID token verifies nothing here names anybody,
	// and afterwards the provider has already proved who it is — see
	// [Service.uncounted]. A round trip that ends in nobody still reaches
	// the failure tally.
	adm := s.uncounted(r)
	// A ROUND TRIP THAT ENDS IN NOBODY is one failed attempt, whichever
	// check refused it. The subject is the provider's own, once an ID
	// token verified and there is one; before that nothing names anybody.
	attempt := authevents.Failure{Client: source, Method: types.FailOIDC}
	cookie, err := r.Cookie(s.flightCookieName())
	if err != nil || cookie.Value == "" {
		s.refuseSignIn(w, r, adm, attempt, "no flight cookie")
		return
	}
	// THE FLIGHT IS CLEARED WHATEVER HAPPENS NEXT. A login that failed
	// must not leave a redeemable flight in the browser, and one that
	// succeeded has no more use for it.
	http.SetCookie(w, s.flightCookie("", time.Time{}))

	flight, err := oidc.Open(s.cipher, cookie.Value, s.now())
	if err != nil {
		s.refuseSignIn(w, r, adm, attempt, "flight: "+err.Error())
		return
	}
	// THE STATE COMPARISON, and it is what stops a callback link somebody
	// was SENT from completing a login in their browser: the attacker
	// cannot know the value sealed in the cookie beside it.
	if r.URL.Query().Get("state") != flight.State {
		s.refuseSignIn(w, r, adm, attempt, "state mismatch")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.refuseSignIn(w, r, adm, attempt, "no authorization code")
		return
	}

	metadata, err := s.provider.Metadata(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_discovery_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	// A TURN AT THE PROVIDER'S TOKEN ENDPOINT FIRST ([oidc.ExchangeSlots]),
	// waited for on this request. A browser that leaves while it waits
	// has asked the provider nothing and spent nothing: it is answered 503,
	// counted as no attempt, and the reload the answer invites presents
	// the same flight to be exchanged then.
	admission, err := s.provider.Admit(r.Context())
	if err != nil {
		abandoned(w, r, source, err)
		return
	}
	defer admission.Release()
	// ONE EXCHANGE PER FLIGHT, decided once the provider WILL be asked and
	// before it is: whoever started a flight holds its cookie and its
	// state, and could otherwise make this node exchange a code at the
	// provider once per request for the flight's whole ten minutes. Spent
	// before the turn came, a flight whose browser left during the wait
	// was refused as a replay on the reload, having reached nobody.
	if !s.redeemed.Redeem(flight, s.now()) {
		s.refuseSignIn(w, r, adm, attempt, "flight already redeemed on this node")
		return
	}
	tokens, err := admission.Exchange(r.Context(),
		metadata.TokenEndpoint, code, flight.Verifier)
	admission.Release()
	if err != nil && r.Context().Err() != nil {
		// THE BROWSER WENT AWAY DURING THE EXCHANGE ITSELF: nothing was
		// refused, so nothing is counted as a failed attempt — and the
		// flight stays spent, since the provider may have been asked.
		abandoned(w, r, source, err)
		return
	}
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_exchange_failed", "error", err)
		s.refuseSignIn(w, r, adm, attempt, "code exchange failed")
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
		s.refuseSignIn(w, r, adm, attempt, "id token: "+err.Error())
		return
	}

	attempt.Subject = claims.Issuer + "|" + claims.Subject
	// WHEN THE PERSON PROVED WHO THEY ARE, by the provider's own account,
	// and a confirmation the provider did not give refused here.
	provedAt, err := flight.ProvedAt(claims, s.now())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_step_up_unconfirmed", "error", err)
		s.refuseSignIn(w, r, adm, attempt, "step-up: "+err.Error())
		return
	}
	if flight.Invite != "" {
		s.redeemThroughProvider(w, r, adm, attempt, flight, claims,
			tokens.Refresh, provedAt)
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
		s.refuseSignIn(w, r, adm, attempt, "no linked credential for this subject")
		return
	}

	// BACK TO WHERE THE LOGIN BEGAN, by redirect. The callback is a
	// browser following the provider's redirect, and it used to answer the
	// JSON body the password route does — which a browser renders as text
	// and goes nowhere from, with a dead `if flight.Return != ""` after it
	// that was meant to be this. The return path was checked at the start
	// to be a path on this deployment and was sealed into the flight since,
	// so it is not the caller's to change now.
	how := signIn{
		method: types.SignInOIDC, acr: claims.ACR, redirect: flight.Return,
		refresh: tokens.Refresh, provedAt: &provedAt,
		// THE GROUP MAPPING RIDES INSIDE THE SESSION, never onto the
		// person's own row: what a provider's groups confer is true for
		// as long as that assertion is, and writing it to the estate
		// would outlive the provider saying it. It used to be merged into
		// this sighting and then dropped, since nothing downstream reads
		// a sighting's grants — so no mapping ever conferred anything.
		groupGrants: s.boot.API.Auth.OIDC.GrantsFor(claims.Groups),
	}
	if flight.MaxAge > 0 {
		// A CONFIRMATION REPLACES the session it was made from, as the
		// password step-up does — see [Service.StepUp] — and keeps its
		// absolute deadline. The groups are the ones the provider just
		// asserted, which are newer than the session's.
		replaced, ok := s.replacedSession(w, r, held)
		if !ok {
			return
		}
		how.stepUp = true
		how.replaces = replaced.Bearer.Lineage.String()
		how.absolute = replaced.Bearer.AbsoluteExpiresAt
	}
	s.completeSignIn(w, r, held, how)
}

// stepUpFlight is what a provider STEP-UP start seals: the window the provider
// is asked to have authenticated the person inside, which is the window the
// refusal that sent them here named. Answers false once it has written the
// refusal.
//
// # Why this is the provider's own round trip and not the password route
//
// Somebody who signs in only through their provider holds no password here,
// so `POST /auth/step-up` has nothing to verify — and once the surfaces that
// change what a company is ask for a recent proof, such a person could
// otherwise never make one of those changes at all. The confirmation is the
// provider's: asked with `prompt=login` and `max_age`, and accepted only on an
// `auth_time` inside the window ([oidc.Flight.ProvedAt]).
//
// # The window is the one the refusal named, and it is `max_age`
//
// A `403 step_up_required` says which window its gesture needs — `step_up` or
// `step_up_sensitive`, its `window` — and the confirmation asks the provider
// for exactly that. It used to ask for the ordinary window whatever the
// gesture needed, counting on `prompt=login` to make the answer fresh; but
// `prompt=login` is a request a provider may ignore (OpenID Connect Core
// 3.1.2.1 says SHOULD), and one that honoured `max_age` alone answered from
// its own session half an hour old — inside the hour, so accepted, and outside
// the fifteen minutes, so the sensitive gesture refused it again, round after
// round, until the provider's session was old enough to prompt. With the
// window as `max_age`, such a provider must authenticate the person to answer
// at all.
//
// # Who may ask
//
// A SIGNED-IN PERSON, and never a machine: a token proves nobody is present,
// and the step-up exists to prove somebody is. This route is unguarded, so
// the guard hands a request it could not resolve through rather than
// answering it — and a node that cannot establish who is signed in says so
// with a 503, never the 401 that tells a browser to sign in again. A request
// presenting a BEARER is refused before any of that, alike whatever the
// bearer: the provider confirms a browser's own session and a bearer has none,
// and an answer here that told a matching bearer from a refused one would be
// one more place to test a value, on a route the guard does not judge it on.
func (s *Service) stepUpFlight(w http.ResponseWriter, r *http.Request,
	want oidc.Flight, window iam.Recency) (oidc.Flight, bool) {

	maxAge, ok := s.windowOf(window)
	if !ok {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "step_up names the window to confirm " +
				"inside: " + string(iam.RecencyStepUp) + " or " +
				string(iam.RecencySensitive) + ", the window a " +
				"step_up_required refusal carries"})
		return oidc.Flight{}, false
	}
	// EVERY PRESENTED BEARER IS REFUSED ALIKE, matched, refused or
	// uncheckable, and before its resolution is read. The provider confirms
	// a BROWSER's own session, and a bearer has none — and an answer that
	// differed for a good bearer and a bad one, a step-up refusal for one
	// and a 401 for the other, was a way to test bearer values on a route
	// the guard does not judge them on.
	if auth.PresentedBearer(r.Context()) {
		refuseStepUp(w, window, "a bearer credential proves nobody is "+
			"present, so the identity provider cannot confirm it; confirm "+
			"from the browser the person signed in with")
		return oidc.Flight{}, false
	}
	principal, how := iam.From(r.Context())
	switch {
	case how == iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return oidc.Flight{}, false
	case how != iam.Resolved:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return oidc.Flight{}, false
	case principal.Kind != iam.KindPerson:
		refuseStepUp(w, window, "a machine credential cannot confirm a person's identity")
		return oidc.Flight{}, false
	}
	if machineToken(w, r, window) {
		return oidc.Flight{}, false
	}
	want.MaxAge = maxAge
	return want, true
}

// windowOf is how long a proof counts for inside window, by this node's own
// settings, or false for a value that names no window a gesture asks for.
//
// THIS NODE'S, as the guard's are ([auth] composes both deadlines from its
// own): the callback judges `auth_time` against the value sealed here, so the
// window a confirmation is held to is the one it was asked for, whichever node
// finishes it.
func (s *Service) windowOf(window iam.Recency) (time.Duration, bool) {
	switch window {
	case iam.RecencyStepUp:
		return s.boot.API.Auth.Session.StepUp(), true
	case iam.RecencySensitive:
		return s.boot.API.Auth.Session.StepUpSensitive(), true
	}
	return 0, false
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

// returnPath is where the browser goes once the login completes, from the
// value the caller asked for: the value itself when it is a path on this
// deployment, and the dashboard otherwise.
//
// # An open redirect is the ordinary way a sign-in flow leaks a credential
//
// The value is the caller's — a query parameter on the sign-in start, a form
// field on a redemption's — so it is kept only if it is a PATH ON THIS
// DEPLOYMENT, and the question that decides that is how a BROWSER reads the
// `Location` it becomes, never how Go's parser reads it. The two disagree, and
// every disagreement is an address off-site:
//
//   - `//evil.example.com` is a protocol-relative URL, which a browser
//     follows to another host — the shape a naive "starts with /" admits.
//   - `/\evil.example.com` is the SAME URL to a browser, which reads a
//     backslash as a slash in an http(s) address (the WHATWG URL standard's
//     special-scheme rule), while Go's parser reads it as a path — so the
//     check this replaced admitted it, and `http.Redirect` emitted it
//     verbatim.
//   - `/<TAB>/evil.example.com` is that URL again, because a browser strips
//     every tab and newline from an address before it parses one.
//
// So a backslash and every control character are refused anywhere in the
// value, and the path is judged a second time DECODED — `/%2F/evil` and
// `/%5Cevil` are paths a browser keeps, and refused anyway, because they are
// never a screen this deployment serves and a page that decoded one before
// navigating would be the open redirect again.
//
// AND IT IS BOUNDED at [maxReturnPath], a longer one taking the default as any
// other path this deployment will not honour does: the value rides in the
// flight cookie, and one too long for it is a sign-in that fails a whole
// provider round trip later with nothing to say why.
//
// IT IS ASKED TWICE: where the value arrives, so nothing else is sealed into a
// flight, and where it LEAVES, as the redirect a sign-in answers — because a
// flight is opened by whichever node the callback reaches, and during a
// rolling upgrade that may be a build that sealed a value by a looser rule.
func returnPath(raw string) string {
	want := strings.TrimSpace(raw)
	if !localPath(want) {
		return auth.PathDashboard
	}
	return want
}

// localPath reports whether a value is one [returnPath] keeps — see there.
func localPath(want string) bool {
	if want == "" || len(want) > maxReturnPath || !offSiteFree(want) {
		return false
	}
	parsed, err := url.Parse(want)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" ||
		parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	// THE DECODED PATH, which `%2F` and `%5C` spell a second way.
	return offSiteFree(parsed.Path)
}

// offSiteFree reports whether a path starts with exactly one slash and holds
// nothing a browser would read as the start of another address: no second
// leading slash, no backslash, and no control character — the bytes the WHATWG
// URL parser strips before it parses, tab and newline among them.
func offSiteFree(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	for i := 0; i < len(path); i++ {
		if b := path[i]; b == '\\' || b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// maxReturnPath bounds a return path, in bytes as the caller sent it.
//
// THE FLIGHT COOKIE IS WHAT IT HAS TO FIT. The path is sealed into the flight
// beside the state, the nonce and the verifier (43 characters each), an
// invitation's id and its link's secret (43 characters too) and a login of up
// to [iam.MaxLogin], and RFC 6265 §6.1 asks a browser to keep a cookie of 4096
// bytes — name, value and attributes — and promises nothing past it. Past it a
// browser DROPS the cookie without a word: the start answers, the person goes
// round the provider, and the callback refuses them for carrying no flight. It
// was unbounded, so a return path of a few kilobytes — a dashboard link
// carrying a long search in its fragment — was exactly that failure.
//
// 400 is what still fits at its worst: every byte one the flight's JSON writes
// as six (`<`, `&`, a byte that is not UTF-8) and the whole envelope base64,
// sealed beside the longest invitation id, its secret and a login under a
// keyring id of sixty-four characters, leaves the cookie under the floor with
// room to spare — the case a test seals. And it is several times the longest
// address the dashboard routes to, fragment and query included.
const maxReturnPath = 400

// flightCookie builds the sealed login-in-progress cookie.
//
// SAME-SITE LAX AND NOT STRICT, and it is the one cookie here that has to be:
// the callback is a cross-site navigation from the provider, and Strict would
// withhold the cookie on exactly the request that needs it. Lax sends it on a
// top-level GET, which is what a redirect back from a provider is.
//
// `Path=/`, SECURE WHEN PREFIXED AND NO DOMAIN, which are the three things a
// browser requires of a `__Host-` cookie before it will set one — see
// [Service.flightCookieName]. It was scoped to `/auth/`, which the prefix does
// not allow; the cost of `/` is a sealed value of a few hundred bytes riding
// every request for the ten minutes a login takes.
func (s *Service) flightCookie(value string, expires time.Time) *http.Cookie {
	cookie := &http.Cookie{
		Name: s.flightCookieName(), Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure:  session.HostPrefixed(s.boot.API.ExternalBase()),
		Expires: expires,
	}
	if value == "" {
		cookie.MaxAge = -1
	}
	return cookie
}
