package authapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// Prefix is the surface's own path, named here because two other rules read
// it: the guard's exemption list and the drain gate.
//
// NAMED RATHER THAN SPELLED AT EACH SITE for [auth.SocketPath]'s reason: a
// prefix each rule spelled for itself could stop matching the one the guard
// exempts, without either looking wrong on its own — and what that produces
// here is a sign-in route behind a credential, which is a deployment nobody
// can enter.
const Prefix = "/auth/"

// Routes registers the surface.
//
// # The auth column, stated here because it is the whole security shape
//
// FOUR ARE UNGUARDED, because requiring a credential to obtain one is a
// deployment nobody can enter: the posture read, the sign-in, the bootstrap
// and the OIDC pair. Every one is admitted per SOURCE by the throttle and
// every one is origin-checked like any other state change.
//
// THE REST NEED A SESSION, and they are guarded by the same middleware every
// other route is — reading what it resolved rather than validating a second
// time, because two readings of one request eventually disagree and the one
// that would be wrong here decides whether somebody is signed in.
// [auth.Mux] RATHER THAN *http.ServeMux, and it is the package that owns the
// exemption list that defines it — see there for why a mux this surface can
// name is what makes the gate below possible at all.
func (s *Service) Routes(mux auth.Mux) {
	// Unguarded.
	mux.HandleFunc("GET "+auth.PathAuthConfig, s.Config)
	mux.HandleFunc("POST "+auth.PathAuthLogin, s.Login)
	mux.HandleFunc("POST "+auth.PathAuthBootstrap, s.Bootstrap)
	// THE GET RENDERS AND THE POST SPENDS, and they are different
	// operations rather than one route branching on a method: a link is
	// followed by mail clients prefetching, scanners and preview cards,
	// every one of them a GET — and an invitation spent by one is an
	// account created for somebody who never saw it.
	mux.HandleFunc("GET "+auth.AuthInvitePrefix+"{id}", s.ViewInvite)
	mux.HandleFunc("POST "+auth.AuthInvitePrefix+"{id}", s.RedeemInvite)
	if s.provider != nil {
		// ABSENT RATHER THAN ERRORING on a deployment with no provider,
		// which is the honest shape: a 404 says this company does not
		// sign in that way, where a 503 would say it does and is broken.
		mux.HandleFunc("GET "+auth.PathAuthOIDCStart, s.OIDCStart)
		mux.HandleFunc("GET "+auth.PathAuthOIDCCallback, s.OIDCCallback)
	}

	// Guarded.
	mux.HandleFunc("GET /auth/session", s.Session)
	mux.HandleFunc("POST /auth/token", s.Token)
	mux.HandleFunc("POST /auth/step-up", s.StepUp)
	mux.HandleFunc("POST /auth/totp", s.EnrolTOTP)
	mux.HandleFunc("POST /auth/totp/recovery", s.RegenerateRecovery)
	mux.HandleFunc("POST /auth/logout", s.Logout)
	mux.HandleFunc("POST /auth/logout/all", s.LogoutEverywhere)
	// THE THIRD LOGOUT: one NAMED session, which is what a person uses to
	// end the one they left open somewhere else without ending the one
	// they are using to do it.
	mux.HandleFunc("POST /auth/logout/{lineage}", s.LogoutOne)
}

// tokenLifetime is how long a session exchanged from a Tier A token lasts.
//
// ONE HOUR, and shorter than a person's session on purpose. What this
// exchanges is a credential in a config file for one in a cookie, so the
// window is sized to the gesture it exists for — a script or an operator CLI
// doing a burst of work — rather than to a working day. A long one would make
// a stolen cookie as good as the token it came from, without the token's own
// revocation path.
const tokenLifetime = time.Hour

// Token exchanges a Tier A bearer for a session cookie.
//
// # What it is for
//
// A browser cannot present a Tier A token on a WebSocket, and a script that
// holds one should not have to re-send it on every request once it has been
// accepted once. This turns the deployment's own machine credential into an
// ordinary session, which every surface then treats identically.
//
// # Keyed on the SOURCE alone
//
// The session it opens is not a person — it is the token, acting as itself —
// so nothing here reads the identity estate and nothing is enrolled. The
// principal the guard already resolved is what it copies, which is also what
// keeps the exchange from being a way to acquire authority: a token that
// carries two grants gets a session carrying the same two, intersected with
// this node's ceiling exactly as the token itself was.
//
// ITS REAUTH CLOCK IS ZERO, so every step-up surface asks it to confirm who it
// is — and it cannot, because there is nothing for a machine to confirm with.
// That is the correct answer rather than an oversight: a config-file
// credential must not be able to reach a surface that exists to require a
// person.
func (s *Service) Token(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	if principal.Kind != iam.KindMachine {
		// A PERSON ALREADY HOLDS A SESSION. Exchanging one for another
		// would reset nothing they need reset and would cost them their
		// step-up clock, which is the one thing this route cannot
		// carry over.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{
				"detail": "this route exchanges a machine credential for a session",
				"hint":   "you already hold one; see GET /auth/session",
			})
		return
	}

	lineage, err := uuid.NewV7()
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_lineage_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	expires := s.now().Add(tokenLifetime)
	at, err := s.writer.OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: principal.ID.String(),
		AbsoluteExpiresAt: expires,
		OpID:              "session:" + lineage.String(),
		NoWait:            true,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_failed", "error", err)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	bearer, err := s.signer.Mint(session.Mint{
		Lineage: lineage, Person: principal.ID.String(),
		StartPosition:     uint64(at.Packed()),
		AbsoluteExpiresAt: expires,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	http.SetCookie(w, session.Cookie(s.boot.API.ExternalBase(), bearer, expires))
	log.InfoContext(r.Context(), "api_token_exchanged",
		"login", principal.Login, "expires_at", expires)
	httpjson.Write(w, http.StatusOK, loginResponse{
		Person: principal.ID.String(), Login: principal.Login,
		ExpiresAt: expires, Position: at.String(),
	})
}

// stepUpRequest is what confirming identity presents.
type stepUpRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// StepUp is a person confirming who they are, on a session that is already
// valid.
//
// # Why it exists beside the sign-in
//
// A session lives for days and a laptop is left unlocked. The surfaces that
// change what a company IS — its credentials, its chart, its configuration —
// ask for proof taken minutes ago rather than proof taken on Monday, and this
// is where that proof is given. It is the only route here that is BOTH guarded
// and throttled: the caller is known, and an unbounded retry against a known
// person is a password oracle with the enumeration already done.
func (s *Service) StepUp(w http.ResponseWriter, r *http.Request) {
	arrived := s.now()
	source := s.sourceOf(r)
	if !s.admit(w, r, source) {
		return
	}
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	if principal.Kind != iam.KindPerson {
		// A MACHINE HAS NOTHING TO CONFIRM WITH, which is the point of
		// the step-up rather than a gap in it: a credential in a config
		// file cannot prove a person is at the keyboard.
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
			map[string]string{
				"detail": "a machine credential cannot confirm a person's identity",
			})
		return
	}

	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in stepUpRequest
	if err := json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}

	held, err := s.directory.PersonByLogin(r.Context(), principal.Login)
	if err != nil {
		log.WarnContext(r.Context(), "api_step_up_lookup_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	}
	if held.ID == "" || !stageAdmits(held.Stage) {
		s.refuseSignIn(w, r, arrived, source, "step-up subject not active")
		return
	}
	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, source, "step-up: no password credential")
		return
	}
	if ok, _ := s.hasher.Verify(verifier.Verifier, in.Password); !ok {
		s.refuseSignIn(w, r, arrived, source, "step-up: password mismatch")
		return
	}
	if factor, need := s.secondFactor(held); need && !s.checkSecondFactor(factor, in.Code) {
		s.refuseSignIn(w, r, arrived, source, "step-up: second factor mismatch")
		return
	}

	// THE PROOF IS RECORDED BY RE-OPENING THE SESSION, which stamps a
	// fresh reauth instant on the row every node reads. A field written
	// locally would be proof on ONE node, and the surface that asks for it
	// is reached through whichever node a request lands on.
	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, held)
}
