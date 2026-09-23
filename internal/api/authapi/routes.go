package authapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
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
// doing a burst of work, or an operator using the dashboard on the day the
// identity provider is down — rather than to a working day. A stolen cookie
// is as good as the token for this long and no longer: the entry leaving the
// configuration ends it at once, and so does `crewlet iam invalidate-all`.
const tokenLifetime = time.Hour

// Token exchanges a Tier A bearer for a session cookie.
//
// # What it is for
//
// A browser cannot present a Tier A token on a WebSocket, and a script that
// holds one should not have to re-send it on every request once it has been
// accepted once. This turns the deployment's own machine credential into a
// session, which every surface then treats identically.
//
// # The session IS the token, re-read from the configuration on every request
//
// Nothing about the token is enrolled and nothing is copied into the session:
// its subject is the token's LOGIN (`token:<id>`), and the guard answers that
// subject from the entry this node holds for it NOW — the same composition the
// bearer gets, so the grants are the entry's cut to this node's ceiling on
// each request, a token the identity directory binds to a seat acts as that
// seat, and removing the entry from the configuration ends the session on the
// next request. It used to be minted for the token's derived principal id,
// which no directory row holds: once the start record applied every request
// answered 401 and cleared the cookie, and before that it served a grantless
// nobody — and a bound token was refused the exchange outright.
//
// IT IS STEPPED UP BY CONSTRUCTION, as the bearer is: presenting the token was
// the proof, and there is nothing else a config-file credential could present.
// A break-glass session that could reach no sensitive surface would be no use
// on the day it exists for — the day the identity provider is down.
//
// # Only a presented token is exchanged
//
// A session — a person's, or one this route already minted — has no value to
// exchange, and exchanging one for another would reset nothing anybody needs
// reset. A personal access token is presented on every request by design.
func (s *Service) Token(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		// THE TOKEN MATCHED AND ITS SEAT COULD NOT BE SAID, which is 503
		// here for the reason it is everywhere: a session minted now
		// would act as the bare credential on this node and as its seat
		// on the next.
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	entry, presented := auth.PresentedTierA(r.Context())
	if !presented {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{
				"detail": "this route exchanges a Tier A token, presented as " +
					"the bearer, for a session",
				"hint": "a session already in hand is used as it is; see " +
					"GET /auth/session",
			})
		return
	}
	subject := iam.TokenLogin(entry.ID)

	lineage, err := uuid.NewV7()
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_lineage_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	expires := s.now().Add(tokenLifetime)
	opened, err := s.writer.OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: subject,
		AbsoluteExpiresAt: expires,
		OpID:              "session:" + lineage.String(),
		NoWait:            true,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	if !landed(opened.Result) {
		unresolved(w, r, "api_token_exchange_unresolved", opened.Result)
		return
	}
	at := opened.Result.Position
	bearer, err := s.signer.Mint(session.Mint{
		Lineage: lineage, Person: subject,
		Epoch: opened.Epoch, Generation: opened.Generation,
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
		"token", entry.ID, "seat", principal.Seat, "expires_at", expires)
	s.audit.Emit(r.Context(), types.IAMSessionStarted{
		Person: principal.ID.String(), Login: principal.Login,
		Method: types.SignInToken, Lineage: lineage.String(),
		Remote: s.sourceOf(r), ExpiresAt: expires,
	})
	httpjson.Write(w, http.StatusOK, loginResponse{
		Person: principal.ID.String(), Login: principal.Login,
		Seat: principal.Seat, ExpiresAt: expires, Position: at.String(),
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
//
// # It REPLACES the session it was made from, and is not a new sign-in
//
// The proof is recorded by opening a fresh session — a new lineage, so the
// identifier a privilege change is made under is not the one that circulated
// before it — and the session it replaces is ENDED, first. It used to be left
// running: every step-up left a second live session behind, a copy of the old
// cookie went on working for the rest of its week, and a person's session
// listing grew by one per confirmation. And the replacement CONFIRMS the
// sign-in rather than repeating it, so it inherits that session's absolute
// deadline and the grants its identity provider's groups conferred: a step-up
// that restarted the absolute clock would let a session be kept alive for
// ever by confirming it, and one that dropped the carried grants would cost a
// person the authority they stepped up to use — or, copied onto a fresh
// deadline, let group-derived authority outlive the provider's assertion.
func (s *Service) StepUp(w http.ResponseWriter, r *http.Request) {
	arrived := s.now()
	source := s.sourceOf(r)
	if !s.admit(w, r, source, types.FailPassword) {
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
	// NOR CAN A TOKEN ACTING AS A PERSON, which the kind above cannot see.
	if machineToken(w, r) {
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
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	// THE CALLER IS KNOWN here, so the subject is who they signed in as and
	// the person is whoever this node resolved that login to.
	attempt := authevents.Failure{
		Client: source, Method: types.FailPassword, Subject: principal.Login,
		Person: held.ID,
	}
	if held.ID == "" || !stageAdmits(held.Stage) {
		s.refuseSignIn(w, r, arrived, attempt, "step-up subject not active")
		return
	}
	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, attempt, "step-up: no password credential")
		return
	}
	if ok, _ := s.hasher.Verify(verifier.Verifier, in.Password); !ok {
		s.refuseSignIn(w, r, arrived, attempt, "step-up: password mismatch")
		return
	}
	var factor factorUse
	if holdsSecondFactor(held) {
		attempt.Method = types.FailSecondFactor
		var proved bool
		if factor, proved = s.proveSecondFactor(w, r, arrived, attempt, held,
			in.Code); !proved {
			return
		}
	}

	replaced, ok := s.replacedSession(w, r, held)
	if !ok {
		return
	}
	// THE PROOF IS RECORDED BY RE-OPENING THE SESSION, which stamps a
	// fresh reauth instant on the row every node reads. A field written
	// locally would be proof on ONE node, and the surface that asks for it
	// is reached through whichever node a request lands on.
	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, held, signIn{
		method: types.SignInPassword, factor: factor.factor,
		stepUp:      true,
		replaces:    replaced.Bearer.Lineage.String(),
		absolute:    replaced.Bearer.AbsoluteExpiresAt,
		groupGrants: replaced.Session.GroupGrants,
	})
}

// replacedSession is the session a step-up confirms, validated here, or false
// once it has written the refusal.
//
// # Read under the signature AND the rows, and never the bare cookie
//
// What a step-up inherits — the absolute deadline and the carried grants —
// and what it ends are the session the caller PRESENTED, so it is read the
// way the guard read it: the signature decides the lineage, and this node's
// rows decide that it is live. A lineage off an unverified cookie would let a
// caller name somebody else's session to end, and a deadline off one would
// let them choose their own.
//
// THE PERSON MUST BE THE ONE WHO PROVED: a principal resolved from a cookie
// is its bearer's person, so a mismatch is a session that changed hands
// between the guard and here, and the answer is the one the guard would now
// give. A node that cannot say is the unknown arm, as it is everywhere.
func (s *Service) replacedSession(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting) (session.Validation, bool) {

	v := s.signer.Validate(r.Context(), s.directoryFor(), session.Presented(r))
	switch {
	case v.Row == session.RowValid && v.Bearer.Person == held.ID:
		return v, true
	case v.Row == session.RowBehind || v.Row == session.RowStalled:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	}
	return session.Validation{}, false
}
