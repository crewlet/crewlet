package authapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE SECOND FACTOR, enrolled and recovered.
//
// # Both routes are guarded AND ask for a SENSITIVE step-up
//
// Adding a second factor and replacing the codes that bypass it are the two
// gestures that decide whether a stolen session can be turned into a permanent
// hold on somebody's account. A session alone is not enough for either: what
// is being changed is the thing that would stop the person holding that
// session, so the person has to be at the keyboard — within the quarter-hour
// window, not the ordinary hour, because this is the second-factor reset's
// gesture by another door and `/iam` asks the reset and a factor's revocation
// the same (see [Service.mayChangeProof]).
//
// # A machine token is refused both, whoever it acts as
//
// A personal access token acts AS its owner — a person, carrying their seat
// — so the kind check below passes for it. It is stepped up by construction
// for the ordinary window only, which the sensitive one these gestures ask
// would refuse anyway; the refusal of its own is what SAYS why: whoever holds
// a pipeline's environment must never enrol their own second factor on the
// owner's account, or regenerate the recovery codes and read them. A token
// PROVES NOBODY IS PRESENT, so it manages no proof — see [machineToken].
//
// # The enrolment is TWO requests, and the secret is only in the first answer
//
// A secret handed out and stored in one step is one nobody proved they can
// use: a person scans the QR code, their app is misconfigured, and they have
// enrolled a factor that will lock them out. So the first request answers a
// seed and stores nothing, and the second presents a code derived from it —
// which is the only evidence that the app on the other side works.

// totpEnrolRequest is what either leg of an enrolment presents.
type totpEnrolRequest struct {
	// Secret is echoed back on the second leg, from the first leg's
	// answer.
	//
	// CARRIED BY THE CLIENT rather than parked on the node: the two
	// legs may land on different ingress nodes, and a seed held in a map
	// on the first is an enrolment that fails whenever the second goes
	// elsewhere. It is not
	// a credential until a code proves it, so a client holding it for
	// one round trip is holding a value that grants nothing.
	Secret string `json:"secret"`

	// Code is a code derived from that seed, which is what proves the
	// authenticator app on the other side actually works.
	Code string `json:"code"`
}

// totpEnrolResponse is the seed and how to render it.
type totpEnrolResponse struct {
	Secret string `json:"secret"`

	// URI is the `otpauth://` form an authenticator app reads from a QR
	// code, built here so a client renders one rather than composing a
	// scheme it would have to keep in step with this engine.
	URI string `json:"uri"`
}

// totpRecoveryResponse is the one and only time the codes are readable.
type totpRecoveryResponse struct {
	// Codes are ten single-use codes, IN THE CLEAR and answered exactly
	// once: what is stored is their hashes, so nothing — not this engine,
	// not an operator, not a restore — can produce them again.
	Codes []string `json:"codes"`
}

// offerTOTPSeed is enrolment's FIRST LEG: a seed, and nothing written. A
// person who never completes the second leg has enrolled nothing, which is
// the correct outcome rather than a half-enrolled factor.
func (s *Service) offerTOTPSeed(w http.ResponseWriter, r *http.Request, login string) {
	secret, err := credential.NewTOTPSecret()
	if err != nil {
		log.ErrorContext(r.Context(), "api_totp_secret_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	httpjson.Write(w, http.StatusOK, totpEnrolResponse{
		Secret: secret,
		URI:    credential.TOTPURI(s.issuerLabel(), login, secret),
	})
}

// EnrolTOTP mints a seed, or stores one a code has proved.
func (s *Service) EnrolTOTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.mayChangeProof(w, r)
	if !ok {
		return
	}
	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in totpEnrolRequest
	if len(body) > 0 {
		if err = json.Unmarshal(body, &in); err != nil {
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
			return
		}
	}
	person := principal.ID.String()
	// WHICH SESSION IS ENROLLING, decided before anything is offered or
	// written: an enrolment-only one is held to a rule a whole one is not
	// (see [errFactorHeld]), and is the session the enrolment replaces.
	replaced, restricted, ok := s.enrolmentSession(w, r, person)
	if !ok {
		return
	}

	if in.Secret == "" || in.Code == "" {
		s.offerTOTPSeed(w, r, principal.Login)
		return
	}

	step, valid := credential.VerifyTOTP(in.Secret, in.Code, s.now(), 0)
	if !valid {
		// SPECIFIC, because the caller is already authenticated AND
		// stepped up: what they need to know is that their app's clock
		// or their typing was wrong, and the generic sign-in refusal
		// would send them to check a password that was right.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{
				"detail": "that code does not match the secret",
				"hint": "check the authenticator app has the right account, " +
					"and that this device's clock is correct",
			})
		return
	}

	// THE ID IS MINTED ONCE, outside the apply: the decide may run again
	// against a fresh snapshot, and an id minted inside it would be a
	// different credential on each run — and a different one again in the
	// event that says which was enrolled. It is also what the seed is
	// sealed AS, so it has to exist before the seal.
	id := uuid.New().String()
	// THE SEED IS SEALED BEFORE IT ENTERS ANY RECORD, under the fleet
	// keyring and bound to this person and this credential: the record is
	// replicated to every node, snapshotted, backed up and donated to
	// joining peers, and a seed in the clear there is a second factor
	// everybody with a copy holds. Nothing past this line carries it in the
	// clear.
	sealed, err := s.sealer.SealCredential(person, id, iamdomain.FieldTOTP,
		in.Secret)
	if err != nil {
		// A FAULT OF THIS NODE'S, never of the caller's typing — the
		// keyring's cipher refused to seal — and nothing was stored.
		// A 500, because no wait clears it.
		log.ErrorContext(r.Context(), "api_totp_seal_failed",
			"person", person, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	const reason = "enrolled a second factor"
	stored, err := s.writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			// IN THE SNAPSHOT THE FACTOR LANDS ON, never from a read
			// made first: a factor enrolled between that read and this
			// write is exactly the one this refusal is about.
			if restricted && holdsFactor(held) {
				return nil, errFactorHeld
			}
			return append(without(held, iamdomain.MethodTOTP),
				totpCredential(id, sealed, step)), nil
		},
		OpID:   "totp:" + person + ":" + id,
		Reason: reason,
	})
	if errors.Is(err, errFactorHeld) {
		log.WarnContext(r.Context(), "api_totp_enrol_refused_factor_held",
			"person", person, "lineage", replaced.Bearer.Lineage.String())
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeSecondFactorRequired,
			map[string]string{
				"detail": "this account holds a second factor now, and this " +
					"session was opened on a password alone, so it cannot " +
					"enrol another over it",
				"hint": "sign in again with the code from your authenticator " +
					"app, or one of your recovery codes",
			})
		return
	}
	if err != nil {
		log.ErrorContext(r.Context(), "api_totp_enrol_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentity(err))
		return
	}
	if !landed(stored) {
		// NOT "enrolled": nothing can say the factor is on the log. The
		// second leg carries its secret, so presenting it again with a
		// fresh code enrols it — the same factor whichever attempt lands.
		// Through a session that may only enrol, a retry that finds the
		// first attempt landed after all is refused as [errFactorHeld]:
		// the factor it holds is the seed in hand, so signing in with it
		// is the way on.
		unresolved(w, r, "api_totp_enrol_unresolved", stored)
		return
	}
	s.audit.Emit(r.Context(), types.IAMCredentialMinted{
		Credential: id, Kind: types.CredentialTOTP, Owner: person,
		By:         iam.ActorFor(principal).Name,
		OperatorID: iam.ActorFor(principal).OperatorID, Reason: reason,
	})
	log.InfoContext(r.Context(), "api_totp_enrolled", "person", person)
	if restricted {
		s.completeEnrolment(w, r, principal, replaced)
		return
	}
	httpjson.Write(w, http.StatusOK, totpEnrolled{Status: "enrolled"})
}

// errFactorHeld refuses an enrolment-only session's enrolment over a second
// factor its person has come to hold since the session opened.
//
// # A password must never override a factor
//
// An enrolment-only session was proved by a password alone, and its proof is
// fresh for the sensitive window, so it satisfies what an enrolment asks of
// a whole session. Without this, whoever holds such a session — somebody who
// knows the password of a person who held no factor, signed in before that
// person enrolled their own — could enrol THEIR authenticator over the
// owner's, lock the owner out and be handed a whole session for it. Every
// other path already refuses a password alone once a factor is held: the
// sign-in and the step-up both demand the code. A whole session may still
// replace its own factor, which is what enrolling one again is for.
var errFactorHeld = errors.New("authapi: an enrolment-only session cannot " +
	"enrol over a second factor its person already holds")

// holdsFactor reports whether a credential set holds any live second factor —
// a revoked one is not held, which is [firstCredential]'s rule.
func holdsFactor(held []iamdomain.Credential) bool {
	for _, method := range iamdomain.SecondFactorMethods {
		if _, ok := firstCredential(held, method); ok {
			return true
		}
	}
	return false
}

// totpEnrolled is what a completed enrolment answers.
type totpEnrolled struct {
	// Status is `enrolled`.
	Status string `json:"status"`

	// Session is the session the enrolment OPENED, present only when the
	// request carried an enrolment-only one — which the enrolment ended,
	// its cookie on this response replacing it. See
	// [Service.completeEnrolment].
	Session *loginResponse `json:"session,omitempty"`
}

// enrolmentSession is the session this request presented when it is one that
// may only enrol a second factor — or false when it is not — and whether the
// caller may go on, false once it has written the refusal.
//
// # Read off the bearer, and held to the rows
//
// WHETHER it is one is the bearer's own signed mark with the row's
// ([session.Validation.EnrolmentOnly]), readable on a node that has not
// applied the session. But what the enrolment ENDS and what its replacement
// KEEPS — the lineage, the deadline, the carried grants — are the presented
// session's, read under the signature and the rows as a step-up reads the
// session it replaces: neither may come off a cookie nobody verified, nor off
// a session this node cannot say is live. So a restricted session this node
// is behind on is `503` and one that is over is `401`, before anything is
// offered or written.
//
// A HEADER CREDENTIAL CARRIES NO SESSION, so it is never one — and every
// credential in a header that could reach an enrolment at all was refused by
// [Service.mayChangeProof] already.
func (s *Service) enrolmentSession(w http.ResponseWriter, r *http.Request,
	person string) (session.Validation, bool, bool) {

	if r.Header.Get("Authorization") != "" {
		return session.Validation{}, false, true
	}
	v := s.presentedSession(r)
	switch {
	case !v.EnrolmentOnly():
		return v, false, true
	case v.Row == session.RowValid && v.Bearer.Person == person:
		return v, true, true
	case v.Row == session.RowBehind || v.Row == session.RowStalled:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(nil))
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	}
	return session.Validation{}, false, false
}

// completeEnrolment replaces an enrolment-only session with a whole one, once
// the second factor it was restricted for is enrolled.
//
// # Why the session is replaced rather than lifted
//
// What restricted it is what its SIGN-IN proved — a password alone — and that
// is a fact about the session, recorded once when it opened. The enrolment
// satisfies the rule the restriction stood for: the person now holds a second
// factor. So the gesture opens the session that earns, and ENDS the
// restricted one first, exactly as a step-up does — the same deadline, one
// live session and not two. A restriction lifted in
// place would be a session that proved one thing and is trusted for another.
//
// # And it keeps the restricted session's PROOF
//
// The code the enrolment checked proves possession of a seed this same
// session was handed moments earlier, which says nothing about who is holding
// it — so the replacement is proved when the password was, and never now.
// Dated now, it restarted both step-up windows: whoever held the restricted
// cookie was handed a fresh sensitive window — recovery codes, reveals — that
// the password sign-in never earned.
func (s *Service) completeEnrolment(w http.ResponseWriter, r *http.Request,
	principal iam.Principal, replaced session.Validation) {

	proved := replaced.Session.ProvedAt
	answer, ok := s.openSignIn(w, r, iamdomain.Sighting{
		ID: principal.ID.String(), Kind: principal.Kind, Stage: principal.Stage,
		Login: principal.Login, Seat: principal.Seat,
	}, signIn{
		method: types.SignInPassword, factor: types.FactorTOTP,
		stepUp: true, replaces: replaced.Bearer.Lineage.String(),
		absolute: replaced.Bearer.AbsoluteExpiresAt,
		provedAt: &proved,
		because:  "replaced by a second factor's enrolment",
	})
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, totpEnrolled{Status: "enrolled", Session: &answer})
}

// RegenerateRecovery issues a fresh set of single-use codes, retiring the old.
//
// THE OLD SET IS REPLACED RATHER THAN EXTENDED, which is what makes this the
// move after a set is lost or printed somewhere it should not have been: a set
// that merely grew would leave whatever leaked still working.
func (s *Service) RegenerateRecovery(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.mayChangeProof(w, r)
	if !ok {
		return
	}
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		log.ErrorContext(r.Context(), "api_recovery_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	person := principal.ID.String()
	id := uuid.New().String()
	const reason = "regenerated the recovery codes"
	stored, err := s.writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			return append(without(held, iamdomain.MethodRecovery),
				recoveryCredential(id, verifiers)), nil
		},
		OpID:   "recovery:" + person + ":" + id,
		Reason: reason,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_recovery_store_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentity(err))
		return
	}
	if !landed(stored) {
		// THE CODES ARE NOT SHOWN: a set nothing can say is stored is a
		// set that may not work, handed out as the one way back in. A
		// retry mints a fresh set, which replaces whichever landed.
		unresolved(w, r, "api_recovery_store_unresolved", stored)
		return
	}
	s.audit.Emit(r.Context(), types.IAMCredentialMinted{
		Credential: id, Kind: types.CredentialRecovery, Owner: person,
		By:         iam.ActorFor(principal).Name,
		OperatorID: iam.ActorFor(principal).OperatorID, Reason: reason,
	})
	log.InfoContext(r.Context(), "api_recovery_regenerated", "person", person)
	// ANSWERED ONCE AND NEVER AGAIN. What is stored is the hashes, so a
	// person who loses this answer regenerates rather than recovers — and
	// that is the property that makes a leaked estate not a set of
	// bypasses.
	httpjson.Write(w, http.StatusOK, totpRecoveryResponse{Codes: codes})
}

// mayChangeProof resolves the caller and refuses one who may not change how
// they prove who they are now — a machine, a machine token, or a person whose
// proof of identity is older than the gesture's window.
//
// # The window is the authority table's, and it is the SENSITIVE one
//
// Enrolling a second factor, replacing it and regenerating the recovery codes
// are [authz.ActionCredentialProof] — the verb `/iam` asks of revoking a
// factor, so the two surfaces that change how somebody proves who they are
// cannot answer differently about it. It was the ordinary hour here, read off
// the session's own deadline, while the same person revoking their factor
// through `/iam` was asked the quarter-hour: a cookie proved forty minutes ago
// could swap the factor for a stranger's and read back fresh recovery codes,
// and was refused only the gesture that took one away.
//
// DECIDED AT THIS SURFACE'S OWN INSTANT and rendered in the router's own
// envelope, so the refusal names its `reason` and its `window` — which is
// what a client needs to ask the person to confirm who they are and replay
// the request, and what this route used to leave out.
func (s *Service) mayChangeProof(w http.ResponseWriter, r *http.Request) (iam.Principal, bool) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(iam.Reason(r.Context())))
		return iam.Principal{}, false
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return iam.Principal{}, false
	}
	window, _ := authz.RecencyOf(authz.ActionCredentialProof)
	if principal.Kind != iam.KindPerson {
		refuseStepUp(w, window, "a machine credential holds no second factor")
		return iam.Principal{}, false
	}
	if machineToken(w, r, window) {
		return iam.Principal{}, false
	}
	d := authz.Decide(r.Context(), principal, authz.ActionCredentialProof,
		authz.Object{Kind: authz.KindPerson, Owner: principal.ID.String()},
		authz.NoChart{}, s.now())
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, authz.Policy{}, d)
		return iam.Principal{}, false
	}
	return principal, true
}

// machineToken refuses a request that presented a machine token, on a gesture
// that manages the proof its owner signs in with, and says whether it did.
// window is the proof the gesture asks for, named on the refusal as every
// `step_up_required` here names it.
//
// ASKED OF THE CREDENTIAL, NEVER OF THE PRINCIPAL'S KIND OR ITS CLOCK: a token
// acts as its owner, so the principal is a person with a fresh step-up clock,
// and both checks [Service.steppedUp] makes around this one pass it. The one
// thing on it that says nobody is present is what it came THROUGH — its
// [iam.Principal.Via], which the guard stamps and [auth.PresentedToken] reads.
func machineToken(w http.ResponseWriter, r *http.Request, window iam.Recency) bool {
	if _, fromToken := auth.PresentedToken(r.Context()); !fromToken {
		return false
	}
	refuseStepUp(w, window, "a machine token proves nobody is present, so it "+
		"cannot confirm its owner's identity or change how they prove it; sign "+
		"in as the person")
	return true
}

// refuseStepUp answers `403 step_up_required` in the ONE envelope every such
// refusal on this API carries — the authority table's own ([authz.StepUpDetail]):
// the reason, the window the gesture asks a proof inside, the (empty) grants —
// with a sentence saying why this caller cannot give that proof here.
//
// ONE SHAPE, because a client branches on it: the dashboard's confirmation
// asks the person for the proof the window names and replays the request,
// and a refusal that named no window left it guessing which of the two to
// ask for.
func refuseStepUp(w http.ResponseWriter, window iam.Recency, detail string) {
	fields := authz.StepUpDetail(authz.Decision{
		Reason: authz.ReasonStepUp, Recency: window,
	})
	fields["detail"] = detail
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeStepUpRequired, fields)
}

// issuerLabel is what an authenticator app shows beside the account.
//
// THE DEPLOYMENT'S OWN HOST, because a person may hold accounts on several
// Crewlet deployments and a constant would render them as identical rows in
// one app — which is how somebody types the staging code into production.
func (s *Service) issuerLabel() string {
	parsed, err := url.Parse(s.boot.API.ExternalBase())
	if err != nil || parsed.Host == "" {
		return "Crewlet"
	}
	return parsed.Host
}

// without is a credential set with one method removed.
func without(held []iamdomain.Credential, method iamdomain.CredentialMethod) []iamdomain.Credential {
	out := make([]iamdomain.Credential, 0, len(held))
	for _, c := range held {
		if c.Method != method {
			out = append(out, c)
		}
	}
	return out
}

// totpCredential builds the stored second factor around a seed ALREADY SEALED
// ([iamdomain.Sealer.SealCredential] under this id): the one argument that
// could carry the seed in the clear is named for what it must hold instead.
func totpCredential(id, sealedSeed string, step int64) iamdomain.Credential {
	raw, _ := json.Marshal(step)
	return iamdomain.Credential{
		V: iamdomain.DocumentVersion, ID: id,
		Method: iamdomain.MethodTOTP, Verifier: sealedSeed,
		// THE LAST ACCEPTED STEP RIDES IN Extra, so a replay inside the
		// same thirty-second window is refused: the enrolment's own code
		// counts as spent, which is what stops somebody watching the
		// enrolment from reusing it.
		Extra: map[string]json.RawMessage{"last_step": raw},
	}
}

// recoveryCredential builds the stored code set.
func recoveryCredential(id string, verifiers []string) iamdomain.Credential {
	raw, _ := json.Marshal(verifiers)
	return iamdomain.Credential{
		V: iamdomain.DocumentVersion, ID: id,
		Method: iamdomain.MethodRecovery,
		Extra:  map[string]json.RawMessage{"verifiers": raw},
	}
}
