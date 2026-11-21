package authapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
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
// Routes register on [auth.Mux], which owns the exemption list, not
// *http.ServeMux. NOTHING HERE MAY BE STORED: the guard marks every answer
// under /auth `no-store`, its own refusals included, which a wrapper could not
// reach ([auth.Guard.Middleware]). Every route is origin-checked.
func (s *Service) Routes(mux auth.Mux) {
	// UNGUARDED, because requiring a credential to obtain one is a deployment
	// nobody can enter: the posture read, the sign-in, the invitation's and a
	// reset link's pairs (holding the link is the credential), and the sign-out
	// of THIS session, which clears the cookie whatever this node can read and
	// verifies every bearer itself ([Service.Logout]). The sign-in meets the
	// throttle's curve, keyed on the login as TYPED; the rest name nobody and
	// meet none ([Service.uncounted]).
	mux.HandleFunc("GET "+auth.PathAuthConfig, s.Config)
	mux.HandleFunc("POST "+auth.PathAuthLogin, s.Login)
	// THE GET RENDERS AND THE POST SPENDS, and they are different
	// operations rather than one route branching on a method: a link is
	// followed by mail clients prefetching, scanners and preview cards,
	// every one of them a GET — and an invitation spent by one is an
	// account created for somebody who never saw it.
	mux.HandleFunc("GET "+auth.AuthInvitePrefix+"{id}", s.ViewInvite)
	mux.HandleFunc("POST "+auth.AuthInvitePrefix+"{id}", s.RedeemInvite)
	// A RESET LINK IS THE SAME PAIR, for the same reason: the GET says whose
	// password it sets and spends nothing, the POST is the person.
	mux.HandleFunc("GET "+auth.AuthResetPrefix+"{id}", s.ViewReset)
	mux.HandleFunc("POST "+auth.AuthResetPrefix+"{id}", s.SpendReset)
	mux.HandleFunc("POST "+auth.PathAuthLogout, s.Logout)

	// GUARDED: the shared middleware's resolution is read, never validated a
	// second time, since two readings of one request eventually disagree.
	mux.HandleFunc("GET "+auth.PathAuthSession, s.Session)
	mux.HandleFunc("POST /auth/token", s.Token)
	mux.HandleFunc("POST "+auth.PathAuthStepUp, s.StepUp)
	mux.HandleFunc("POST "+auth.PathAuthTOTP, s.EnrolTOTP)
	mux.HandleFunc("POST /auth/totp/recovery", s.RegenerateRecovery)
	mux.HandleFunc("POST /auth/password", s.ChangePassword)
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
// doing a burst of work, or an operator using the dashboard on the day nobody
// can sign in as a person — rather than to a working day. A stolen cookie
// is as good as the token for this long and no longer: the entry leaving the
// configuration ends it at once, and so does `crewlet iam invalidate-all`.
const tokenLifetime = time.Hour

// Token exchanges a Tier A bearer for a session cookie.
//
// A browser cannot present a Tier A token on a WebSocket; this turns the
// machine credential into a session every surface treats identically. The
// session IS the token: its subject is the token's LOGIN (`token:<id>`), which
// the guard answers from the entry this node holds NOW, so removing the entry
// ends it. It answers to the entry's ID, not its VALUE: cutting off a leaked
// value at once takes a new id. It is STEPPED UP BY CONSTRUCTION, as the
// bearer is, or break-glass would reach no step-up surface. Only a presented
// token is exchanged. As every fresh sign-in does ([Service.openSignIn]), it
// closes the session the old cookie named; the header wins the request.
func (s *Service) Token(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		// THE TOKEN MATCHED AND ITS SEAT COULD NOT BE SAID, which is 503
		// here for the reason it is everywhere: a session minted now
		// would act as the bare credential on this node and as its seat
		// on the next.
		auth.UnavailableIdentity(w, iam.Reason(r.Context()))
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	entry, presented := auth.PresentedTierA(r.Context())
	if !presented {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{
				"detail": "Send an API token in the Authorization header to " +
					"sign in with it.",
				"hint": "Already signed in? Use your session as it is (GET " +
					"/auth/session).",
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
	signedIn := s.now()
	expires := signedIn.Add(tokenLifetime)
	opID := sessionOpID(lineage)
	s.endEveryHeld(r)
	opened, err := s.behalf(principal).OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: subject,
		AbsoluteExpiresAt: expires,
		UserAgent:         r.UserAgent(), Remote: s.sourceOf(r),
		OpID:   opID,
		NoWait: true,
	})
	if err != nil {
		// A COLLAPSED START ([iamdomain.ErrCollapsed]) arrives here too:
		// the counters a bearer carries are the decide's, and nothing can
		// prove they are the landed record's — so no cookie, and the next
		// exchange is a new session.
		gestureFailed(w, r, "api_token_exchange_failed", opID, err)
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
		SignedInAt:        signedIn,
		AbsoluteExpiresAt: expires,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_token_exchange_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	session.Issue(w, s.boot.API.ExternalBase(), bearer, expires)
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
		// A TOKEN'S SESSION IS NEVER RESTRICTED: presenting the token
		// was the whole proof, and it holds no second factor to enrol.
		Status: statusSignedIn,
	})
}

// machineToken refuses a STEP-UP through a request that presented a machine
// token, and says whether it did.
//
// THE STEP-UP'S OWN, because confirming who you are is no verb in the
// authority table: a token acts as its owner, so the kind check passes and
// nobody is at a keyboard. Every other gesture needing a person present is the
// table's to refuse ([authz.Presence]). It answers `token_refused`, as
// [authz.Decide] does, not `step_up_required`, which a client would replay and
// be refused again. ASKED OF THE CREDENTIAL, NEVER THE PRINCIPAL'S KIND OR
// CLOCK: only what the request came THROUGH ([auth.PresentedToken]) says so.
func machineToken(w http.ResponseWriter, r *http.Request) bool {
	if _, fromToken := auth.PresentedToken(r.Context()); !fromToken {
		return false
	}
	authz.EnvelopeRefusal(w, r, authz.Policy{},
		authz.Decision{Reason: authz.ReasonTokenRefused})
	return true
}

// notAPerson refuses a gesture only a person can make to a principal that is
// none — a Tier A token's session, a service account — with the request's own
// sentence.
//
// `403 forbidden`, AS `POST /auth/password` ANSWERS THE SAME FACT, and never
// `step_up_required`: no proof the caller could give lifts it, so a refusal
// that asked for one sent a client to a step-up that refuses the same caller
// again. Nor is it the authority table's refusal — a verb's row decides who
// may take a gesture, and this is the rule of the gesture itself, which no
// permission changes.
func notAPerson(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden,
		map[string]string{"detail": detail})
}

// stepUpRequest is what confirming identity presents.
type stepUpRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// StepUp is a person confirming who they are, on a session that is already
// valid.
//
// Sessions live for days, so surfaces that change what a company IS ask for
// proof taken minutes ago; this is where it is given. It is the only route
// BOTH guarded and throttled: unbounded retries against a known person are a
// password oracle with the enumeration done.
//
// It REPLACES its session: a fresh one (new lineage, so a privilege change is
// not made under a circulated identifier) is opened and the old ENDED first. The
// replacement inherits the absolute deadline: restarting the clock would let
// confirming keep a session alive for ever.
func (s *Service) StepUp(w http.ResponseWriter, r *http.Request) {
	source := s.sourceOf(r)
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	if principal.Kind != iam.KindPerson {
		// A MACHINE HAS NOTHING TO CONFIRM WITH, which is the point of
		// the step-up rather than a gap in it: a credential in a config
		// file cannot prove a person is at the keyboard.
		notAPerson(w, "Only a person can confirm it's them. Sign in as yourself.")
		return
	}
	// NOR CAN A TOKEN ACTING AS A PERSON, which the kind above cannot see.
	if machineToken(w, r) {
		return
	}

	body, err := httpjson.ReadBody(w, r, MaxLoginBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in stepUpRequest
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	// THE CURVE ON WHO THEY SIGNED IN AS, which is the subject here: the
	// same pair a password sign-in by that login climbs, so a stolen cookie
	// is not a way round the curve on the password it guards.
	adm, ok := s.admit(w, r, credential.Attempt{
		Source: source, Subject: principal.Login}, types.FailPassword)
	if !ok {
		return
	}
	defer adm.ticket.Release()

	held, err := s.directory.PersonByLogin(r.Context(), principal.Login)
	if err != nil {
		log.WarnContext(r.Context(), "api_step_up_lookup_failed", "error", err)
		auth.UnavailableIdentity(w, err)
		return
	}
	attempt := authevents.Failure{Source: source, Method: types.FailPassword}
	if held.ID == "" || !stageAdmits(held.Stage) {
		s.refuseSignIn(w, r, adm, attempt, "step-up subject not active")
		return
	}
	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		if s.decoy(w, r, adm, in.Password) {
			s.refuseSignIn(w, r, adm, attempt, "step-up: no password credential")
		}
		return
	}
	proved, stale, err := s.hasher.Verify(r.Context(), verifier.Verifier,
		in.Password)
	if err != nil {
		abandoned(w, r, adm.source, err)
		return
	}
	if !proved {
		s.refuseSignIn(w, r, adm, attempt, "step-up: password mismatch")
		return
	}
	var factor factorUse
	if holdsSecondFactor(held) {
		if in.Code == "" {
			// ASKED FOR, as the sign-in asks, and released as neither a
			// success nor a failure for the sign-in's reason: the password
			// proved itself. Checked as a code it was refused as a wrong
			// one, and a person whose password was right read that it was
			// not.
			httpjson.Fail(w, http.StatusUnauthorized,
				httpjson.CodeSecondFactorRequired)
			return
		}
		attempt.Method = types.FailSecondFactor
		var factored bool
		if factor, factored = s.proveSecondFactor(w, r, adm, attempt, held,
			in.Code); !factored {
			return
		}
	}
	// A STEP-UP PRESENTS THE PASSWORD TOO, and a person who only ever
	// confirms on a long-lived session would otherwise keep the old cost
	// for as long as that session lasts. Deferred, like the sign-in's.
	if stale {
		defer s.rehashPassword(r, held.ID, verifier, in.Password)
	}

	replaced, ok := s.replacedSession(w, r, held)
	if !ok {
		return
	}
	// THE PROOF IS RECORDED BY RE-OPENING THE SESSION, which stamps a
	// fresh reauth instant on the row every node reads. A field written
	// locally would be proof on ONE node, and the surface that asks for it
	// is reached through whichever node a request lands on.
	adm.ticket.Succeed()
	s.completeSignIn(w, r, held, signIn{
		method: types.SignInPassword, factor: factor.factor,
		stepUp:   true,
		replaces: replaced.Bearer.Lineage.String(),
		signedIn: replaced.Bearer.SignedInAt,
		absolute: replaced.Bearer.AbsoluteExpiresAt,
	})
}

// replacedSession is the session a step-up confirms, validated here, or false
// once it has written the refusal.
//
// Read under the signature AND the rows, never the bare cookie: the signature
// decides the lineage, this node's rows that it is live, as the guard read it.
// An unverified cookie would let a caller name somebody else's session to end,
// or choose their deadline. THE PERSON MUST BE THE ONE WHO PROVED: a mismatch
// is a session that changed hands, answered as the guard now would. A node
// that cannot say is the unknown arm, as everywhere.
func (s *Service) replacedSession(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting) (session.Validation, bool) {

	v := s.signer.Validate(r.Context(), s.directoryFor(),
		session.Presented(r, s.boot.API.ExternalBase()))
	switch {
	case v.Row == session.RowValid && v.Bearer.Person == held.ID:
		return v, true
	case v.Row == session.RowBehind || v.Row == session.RowStalled:
		auth.UnavailableIdentity(w, nil)
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	}
	return session.Validation{}, false
}
