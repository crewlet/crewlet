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
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SECOND FACTOR, enrolled and recovered.
//
// Both routes are guarded AND need a step-up ([Service.mayChangeProof]): they
// decide whether a stolen session becomes a permanent hold on an account, so
// the person must be present within the step-up window.
//
// A machine token is refused both, though it acts as its owner and is stepped
// up: [authz.ActionCredentialProof] needs a person present, so a pipeline's
// environment can never enrol a factor or read regenerated recovery codes.
//
// Enrolment is TWO requests: the first answers a seed and stores nothing, the
// second presents a code derived from it, the only proof the app works.

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
	body, err := httpjson.ReadBody(w, r, MaxLoginBodyBytes)
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
				"detail": "That code doesn't match.",
				"hint": "Check that your authenticator app shows the right " +
					"account and your device's clock is correct.",
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
	// replicated to every data node, snapshotted, backed up and donated to
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
	const reason = "set up two-step verification"
	// A FRESH OPERATION PER ENROLMENT, in the grammar the ledger vouches for
	// a retry by: this request is the whole of it, and an attempt asked
	// again is a new enrolment of the same seed. It was `totp:<person>:<id>`,
	// which carries no instant and read as minted at the epoch.
	opID := statelog.NewOpID(s.now(), "totp-enrol")
	stored, err := s.behalf(principal).SetCredentials(r.Context(), iamdomain.CredentialSet{
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
		OpID:   opID,
		Reason: reason,
	})
	if errors.Is(err, errFactorHeld) {
		log.WarnContext(r.Context(), "api_totp_enrol_refused_factor_held",
			"person", person, "lineage", replaced.Bearer.Lineage.String())
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeSecondFactorRequired,
			map[string]string{
				"detail": "Two-step verification is already set up on this " +
					"account.",
				"hint": "Sign in again with a code from your authenticator " +
					"app, or a recovery code.",
			})
		return
	}
	if err != nil {
		if removed(err) {
			s.proofOfRemoved(w, r)
			return
		}
		writeFailed(w, r, "api_totp_enrol_failed", opID, err)
		return
	}
	if !built(stored) {
		// NOT "enrolled": nothing can say the factor is on the log — or it
		// landed as a copy this call cannot prove is its own, whose set is
		// not this call's to report. The second leg carries its secret, so
		// presenting it again with a fresh code enrols it — the same
		// factor whichever attempt lands. Through a session that may only
		// enrol, a retry that finds the first attempt landed after all is
		// refused as [errFactorHeld]: the factor it holds is the seed in
		// hand, so signing in with it is the way on.
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
// An enrolment-only session is proved by a password alone and fresh for the
// step-up window, so it passes what an enrolment asks. Without this refusal,
// someone who knew the password of a person with no factor could enrol THEIR
// authenticator over the owner's once the owner enrolled one, lock the owner
// out and be handed a whole session. Sign-in and step-up already demand the
// code once a factor is held. A whole session may still replace its own.
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
// WHETHER it is one is the bearer's signed mark ([session.Validation.
// EnrolmentOnly]), readable on a node that has not applied the session. What
// the enrolment ENDS and its replacement KEEPS (lineage, deadline, grants) is
// read under the signature and the rows, as a step-up does: never off an
// unverified cookie or a session this node cannot say is live. So a restricted
// session this node is behind on is `503` and one that is over is `401`. A
// HEADER CREDENTIAL CARRIES NO SESSION, so it is never one.
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
		auth.UnavailableIdentity(w, nil)
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	}
	return session.Validation{}, false, false
}

// completeEnrolment replaces an enrolment-only session with a whole one, once
// the second factor it was restricted for is enrolled.
//
// The restriction records what its SIGN-IN proved (a password alone), so the
// gesture ENDS the restricted session first, as a step-up does (same deadline,
// one live session), and opens the one the factor earns; lifted in place, a
// session would prove one thing and be trusted for another.
//
// It keeps the restricted session's PROOF: the checked code proves possession
// of a seed handed over moments earlier, not who holds it, so the replacement
// is proved when the password was, never now. Dated now, it would restart the
// step-up window and hand a restricted cookie recovery codes and reveals.
func (s *Service) completeEnrolment(w http.ResponseWriter, r *http.Request,
	principal iam.Principal, replaced session.Validation) {

	proved := replaced.Session.ProvedAt
	answer, ok := s.openSignIn(w, r, iamdomain.Sighting{
		ID: principal.ID.String(), Kind: principal.Kind, Stage: principal.Stage,
		Login: principal.Login, Seat: principal.Seat,
	}, signIn{
		method: types.SignInPassword, factor: types.FactorTOTP,
		stepUp: true, replaces: replaced.Bearer.Lineage.String(),
		signedIn: replaced.Bearer.SignedInAt,
		absolute: replaced.Bearer.AbsoluteExpiresAt,
		provedAt: &proved,
		because:  "replaced after setting up two-step verification",
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
//
// ONLY BESIDE AN AUTHENTICATOR ([errNoAuthenticator]): recovery codes stand in
// for one, and held alone they are a second factor every sign-in then asks for
// with ten single-use answers and no way to make more.
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
	// A FRESH OPERATION PER SET, for the enrolment's reason: a retry mints
	// a fresh set, which is a new operation replacing whichever landed.
	opID := statelog.NewOpID(s.now(), "recovery-codes")
	stored, err := s.behalf(principal).SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			// IN THE SNAPSHOT THE CODES LAND ON, for the enrolment's
			// reason: an authenticator revoked a moment ago is exactly the
			// one this refusal is about.
			if _, ok := firstCredential(held, iamdomain.MethodTOTP); !ok {
				return nil, errNoAuthenticator
			}
			return append(without(held, iamdomain.MethodRecovery),
				recoveryCredential(id, verifiers)), nil
		},
		OpID:   opID,
		Reason: reason,
	})
	if errors.Is(err, errNoAuthenticator) {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalid,
			map[string]string{"detail": "Set up an authenticator app first. " +
				"You'll get recovery codes with it."})
		return
	}
	if err != nil {
		if removed(err) {
			s.proofOfRemoved(w, r)
			return
		}
		writeFailed(w, r, "api_recovery_store_failed", opID, err)
		return
	}
	if !built(stored) {
		// THE CODES ARE NOT SHOWN: a set nothing can say is stored is a
		// set that may not work, handed out as the one way back in — and a
		// set that landed as a copy this call cannot prove is its own is
		// not the set in hand. A retry mints a fresh set, which replaces
		// whichever landed.
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

// errNoAuthenticator refuses recovery codes to a person who holds no
// authenticator app, in the snapshot the codes would land on.
//
// A RECOVERY CREDENTIAL IS A SECOND FACTOR ON ITS OWN ([holdsFactor]), so a set
// issued to somebody with a password alone turned every later sign-in into a
// demand for a code that only those ten single-use codes could answer — a
// person who opened "new recovery codes" to look locked themselves into
// spending them, and an administrator's reset was the way back. Codes are
// issued with the first authenticator (the dashboard's enrolment
// asks for them at once) and replaced beside one.
var errNoAuthenticator = errors.New("authapi: recovery codes need an " +
	"authenticator they stand in for")

// proofOfRemoved answers a change to somebody's proof whose person was removed
// between the session this was asked through and its record ([removed]): the
// session is over with them, which nothing will reopen.
func (s *Service) proofOfRemoved(w http.ResponseWriter, r *http.Request) {
	log.InfoContext(r.Context(), "api_proof_person_removed")
	s.clearSession(w)
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSessionRevoked)
}

// mayChangeProof resolves the caller and refuses one who may not change how
// they prove who they are now — a machine (`403 forbidden`, as
// [notAPerson] says why), a machine token, or a person whose proof of identity
// is older than the gesture's window.
//
// Enrolling or replacing a factor and regenerating recovery codes are
// [authz.ActionCredentialProof], the verb `/iam` asks of revoking one. Its row
// needs a PERSON PRESENT, so the table refuses a machine token
// (`token_refused`) and no route must remember to. DECIDED AT THIS SURFACE'S
// OWN INSTANT and rendered in the router's envelope, so the refusal names its
// `reason` and `window`, which a client needs to ask the person to re-prove
// and replay.
func (s *Service) mayChangeProof(w http.ResponseWriter, r *http.Request) (iam.Principal, bool) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		auth.UnavailableIdentity(w, iam.Reason(r.Context()))
		return iam.Principal{}, false
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return iam.Principal{}, false
	}
	if principal.Kind != iam.KindPerson {
		notAPerson(w, "Only people can set up two-step verification.")
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
