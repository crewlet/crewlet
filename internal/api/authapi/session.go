package authapi

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/google/uuid"
)

// providerLabel is the host a provider is reached at, for a button's label.
func providerLabel(issuer string) string {
	parsed, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// directoryFor adapts this surface's own seam to the one
// [session.Signer.Validate] takes.
//
// TWO SEAMS OVER ONE READER, and that is deliberate rather than duplication:
// this package's [Directory] is what a SIGN-IN may reach and carries no
// per-bearer resolve, while session's is what a VALIDATION needs and carries
// nothing else. Widening either to be the other would give a route access to
// the half it has no business with.
func (s *Service) directoryFor() session.Directory { return s.sessions }

// configResponse is what a client reads before anybody has signed in.
//
// # What is in it, and what is deliberately not
//
// Enough to render the right form and nothing that says who works here. A
// backend name, whether a provider button is offered and what it is called,
// whether the first-operator route is still open, and the password floor so a
// form can refuse twelve characters before a round trip.
//
// THERE IS NO USER LIST, no count of people, and no hint of whether any
// particular login exists. This route is unguarded, so everything on it is
// public — and the one question an attacker most wants answered here is who
// they could be.
type configResponse struct {
	// Backend is how this deployment signs people in: local, oidc or
	// none.
	Backend config.AuthBackend `json:"backend"`

	// Provider is the identity provider's host, for a button to be
	// labelled with. The HOST and never the whole issuer URL: a browser
	// is redirected to that host the moment somebody clicks, so it is not
	// a secret — while the path, which may name a tenant or a realm, is
	// deployment topology nothing on a sign-in page needs.
	Provider string `json:"provider,omitempty"`

	// Bootstrap reports whether the first-operator route is still
	// available. It closes for good the moment anybody is enrolled.
	//
	// IT IS SAFE TO SAY, and saying it is what a client needs to render
	// the difference between a sign-in form and a set-up form. What it
	// discloses is whether this company has started, which whoever can
	// reach an unstarted one is about to find out anyway.
	Bootstrap bool `json:"bootstrap"`

	// MinPasswordLength is the floor a form enforces before it posts.
	MinPasswordLength int `json:"min_password_length,omitempty"`

	// SecondFactor reports whether this deployment requires one.
	SecondFactor string `json:"second_factor,omitempty"`
}

// Config answers what a client needs before anybody has signed in.
//
// UNGUARDED, necessarily: it is what a sign-in page reads to know what kind of
// sign-in page to be. See [configResponse] for what that costs and what it is
// therefore not allowed to carry.
func (s *Service) Config(w http.ResponseWriter, r *http.Request) {
	out := configResponse{Backend: s.backend()}
	if s.backend() == config.AuthBackendLocal {
		out.MinPasswordLength = iam.MinPasswordChars
		out.SecondFactor = string(s.boot.API.Auth.Local.TOTP)
	}
	if s.provider != nil {
		out.Provider = providerLabel(s.boot.API.Auth.OIDC.Issuer)
	}
	// THE ESTATE DECIDES, not the setting alone. `api.auth.bootstrap` says
	// whether the route MAY run and the estate says whether it still can:
	// a company with people in it closes it for good whatever the file
	// says, because the route creates an operator carrying the whole
	// ceiling.
	if s.boot.API.Auth.Bootstrap != config.BootstrapAccessClosed {
		held, err := s.directory.AnyPerson(r.Context())
		if err != nil {
			// THE UNKNOWN ARM IS REPORTED AS CLOSED, which is the
			// fail-safe direction: a client told the route is open
			// and refused is confusing, and a client told it is
			// closed when the estate could not be read merely waits.
			log.WarnContext(r.Context(), "api_auth_config_estate_unreadable",
				"error", err)
		} else {
			out.Bootstrap = !held
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

// sessionResponse is who the caller is.
type sessionResponse struct {
	Person    string        `json:"person"`
	Login     string        `json:"login"`
	Seat      string        `json:"seat,omitempty"`
	Kind      iam.Kind      `json:"kind"`
	Stage     iam.Stage     `json:"stage"`
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`
	ExpiresAt time.Time     `json:"expires_at"`
	ReauthAt  time.Time     `json:"reauth_at,omitzero"`

	// StepUpDue reports whether the next sensitive action will ask this
	// caller to confirm who they are, so a client can say so before they
	// start rather than after.
	StepUpDue bool `json:"step_up_due"`
}

// Session answers who the caller is.
//
// GUARDED, unlike everything else in this file — the guard resolves the cookie
// and this reads what it resolved, rather than validating a second time. Two
// readings of one request eventually disagree, and the one that would be
// wrong here is the one that decides whether somebody is signed in.
func (s *Service) Session(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		// THE THIRD ANSWER. This node could not tell, which is 503 and
		// never 401: a browser reads 401 as "sign in again" and
		// discards the cookie, so answering it during an identity
		// outage signs the whole company out and stampedes the provider
		// with re-authentications.
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	httpjson.Write(w, http.StatusOK, sessionResponse{
		Person: principal.ID.String(), Login: principal.Login,
		Seat:      principal.Seat,
		Kind:      principal.Kind,
		Stage:     principal.Stage,
		Grants:    principal.Grants,
		Colleague: principal.Colleague,
		ReauthAt:  principal.ReauthAt,
		StepUpDue: s.stepUpDue(principal),
	})
}

// stepUpDue reports whether this caller's proof of identity is old enough that
// the next sensitive action will ask them to confirm it.
//
// DERIVED FROM THE SAME SETTING THE GATE USES, never a second number: a client
// that warned at a different threshold from the one that refuses would either
// nag early or surprise late, and both read as a bug in the engine.
func (s *Service) stepUpDue(p iam.Principal) bool {
	if p.ReauthAt.IsZero() {
		// NOTHING PROVED YET is due by definition, which is the honest
		// reading of a session opened by a route that does not prove
		// identity at all — see the Tier A token exchange.
		return true
	}
	return s.now().Sub(p.ReauthAt) >= s.boot.API.Auth.Session.StepUp()
}

// retryIdentity is the Retry-After an identity answer this node could not give
// carries, in seconds.
//
// TWO, and it is the same number internal/api's own caller shape uses: an
// identity read is a keyed lookup of a replicated row, so what a caller is
// waiting out is an applier catching up or a store blip rather than anything
// that takes a person's attention.
const retryIdentity = 2

// Logout ends THIS session.
//
// THE COOKIE IS CLEARED WHATEVER THE WRITE DID. A logout that answered 503
// because the log could not be reached would leave a person looking at a
// signed-in page having asked to leave, on a shared machine — so the cookie
// goes first, the record is published, and a failure to publish is reported in
// the log rather than to the person walking away from the screen.
//
// That is safe because the cookie is the only thing the browser holds: without
// it nothing is presented, and the record catching up later merely makes the
// row agree with what already happened.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, session.Clear(s.boot.API.ExternalBase()))

	lineage := s.lineageOf(r)
	if lineage == "" {
		// NOTHING TO END, and it is not an error: a client that clears
		// its own cookie and posts here is asking for exactly what
		// already happened.
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
		return
	}
	if _, err := s.writer.CloseSession(r.Context(), lineage,
		"signed out", "logout:"+lineage); err != nil {
		log.WarnContext(r.Context(), "api_sign_out_record_failed",
			"error", err, "lineage", lineage)
	}
	log.InfoContext(r.Context(), "api_sign_out", "lineage", lineage)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// LogoutEverywhere ends every session this person holds, by bumping their own
// revocation epoch.
//
// THE EPOCH AND NOT A SWEEP OF ROWS, because it is the only move that is
// immediate on every node: a bearer carries the epoch it was opened at, so one
// write ends every session in flight without any node having to find them.
func (s *Service) LogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	http.SetCookie(w, session.Clear(s.boot.API.ExternalBase()))

	person := principal.ID.String()
	if _, err := s.writer.Revoke(r.Context(), person,
		"logout-all:"+person+":"+s.now().UTC().Format(time.RFC3339Nano),
		"signed out everywhere"); err != nil {
		// REPORTED, unlike the single logout above, and the difference
		// is what the caller asked for: clearing this browser's cookie
		// does not end the OTHER sessions, so a failure here means the
		// thing they asked for did not happen and they have to ask
		// again.
		log.WarnContext(r.Context(), "api_sign_out_all_failed",
			"error", err, "person", person)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	log.InfoContext(r.Context(), "api_sign_out_all", "person", person)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out everywhere"})
}

// lineageOf is the session this request is carrying, or empty.
//
// # It is the SIGNATURE that decides, never the field
//
// A lineage read straight out of a cookie without checking the mac is a
// lineage the CALLER CHOSE, and closing a session on one is a denial of
// service against anybody whose session id leaks: a proxy log, a referrer, a
// screenshot. So this goes through [session.Signer.Validate], which parses
// under the keyring and yields a bearer only when the signature verifies.
//
// WHAT IT DELIBERATELY IGNORES is the verdict. A logout has to work for a
// cookie that is expired, revoked, or from a session this node has never
// applied — which is precisely when somebody most wants to sign out — and
// every one of those rows carries a bearer whose signature checked out. The
// only row that does not is the malformed one, whose bearer is the zero value
// and whose lineage is therefore empty.
func (s *Service) lineageOf(r *http.Request) string {
	cookie, err := r.Cookie(session.CookieName(s.boot.API.ExternalBase()))
	if err != nil || cookie.Value == "" {
		return ""
	}
	verdict := s.signer.Validate(r.Context(), s.directoryFor(), cookie.Value)
	if verdict.Bearer.Lineage == uuid.Nil {
		return ""
	}
	return verdict.Bearer.Lineage.String()
}

// LogoutOne ends ONE named session, which is how somebody signs out of a
// laptop they left in an office without signing out of the browser they are
// doing it from.
//
// # It is somebody's OWN session, and that is checked against the estate
//
// A lineage is not a secret — it is in a cookie, a proxy log, a screenshot —
// so a route that closed whatever lineage it was handed would let anybody end
// anybody's session. The person who OWNS it is what decides, read from this
// node's own rows rather than claimed by the caller.
//
// The one exception is the same one every surface here has: a principal
// carrying `fleet:operate` may end a session they do not own, because ending
// somebody's session is what an operator does when a laptop is stolen and its
// holder cannot be reached.
func (s *Service) LogoutOne(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	lineage := r.PathValue("lineage")
	if lineage == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}

	owner, err := s.directory.SessionOwner(r.Context(), lineage)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	}
	if owner == "" {
		// ABSENT IS NOT AN ERROR HERE. A session that has already ended
		// is exactly what the caller asked for, and a 404 would send
		// somebody looking for a session they successfully closed.
		//
		// IT IS ALSO THE SAME ANSWER A SESSION THEY DO NOT OWN GETS
		// BELOW, which is what keeps this from being an oracle for
		// which lineages exist.
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
		return
	}
	if !mayEnd(principal, owner) {
		httpjson.Fail(w, http.StatusForbidden, httpjson.CodeUnauthorized)
		return
	}
	if _, err := s.writer.CloseSession(r.Context(), lineage,
		"signed out from another session", "logout-one:"+lineage); err != nil {
		log.WarnContext(r.Context(), "api_sign_out_one_failed",
			"error", err, "lineage", lineage)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	// AND THE COOKIE GOES IF IT WAS THIS ONE, so a person who ends the
	// session they are using is not left looking at a signed-in page.
	if lineage == s.lineageOf(r) {
		http.SetCookie(w, session.Clear(s.boot.API.ExternalBase()))
	}
	log.InfoContext(r.Context(), "api_sign_out_one",
		"lineage", lineage, "by", principal.Login)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
}

// mayEnd reports whether this caller may end a session that person holds.
//
// # Both values come from this node, and neither from the caller
//
// The owner is read out of this node's own rows and the caller is what the
// guard resolved. A lineage is not a secret — cookie, proxy log, screenshot —
// so a rule that trusted anything the request carried about whose session it
// is would let anybody end anybody's.
//
// THE OPERATOR EXCEPTION is `fleet:operate`, and it is what somebody does when
// a laptop is stolen and its holder cannot be reached. It is the deployment's
// grant rather than a colleague level, because ending a person's sessions is
// an act on the DEPLOYMENT's security rather than on the company's work.
func mayEnd(p iam.Principal, owner string) bool {
	if owner != "" && owner == p.ID.String() {
		return true
	}
	return p.Can(iam.GrantFleetOperate)
}
