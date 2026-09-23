package authapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE SECOND FACTOR, enrolled and recovered.
//
// # Both routes are guarded AND require a step-up
//
// Adding a second factor and replacing the codes that bypass it are the two
// gestures that decide whether a stolen session can be turned into a permanent
// hold on somebody's account. A session alone is not enough for either: what
// is being changed is the thing that would stop the person holding that
// session, so the person has to be at the keyboard.
//
// # A machine token is refused both, whoever it acts as
//
// A personal access token acts AS its owner — a person, carrying their seat
// — and is stepped up by construction for the grants it was minted with,
// because it has nothing else to present. That makes the kind check below
// pass and the step-up clock read fresh, so without a refusal of its own a
// token would reach the two gestures a step-up exists to protect: whoever
// holds a pipeline's environment could enrol their own second factor on the
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
	// CARRIED BY THE CLIENT rather than parked on the node, for the
	// reason an OIDC flight is: the two legs may land on different
	// ingress nodes, and a seed held in a map on the first is an
	// enrolment that fails whenever the second goes elsewhere. It is not
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

// EnrolTOTP mints a seed, or stores one a code has proved.
func (s *Service) EnrolTOTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.steppedUp(w, r)
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
		if err := json.Unmarshal(body, &in); err != nil {
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
			return
		}
	}

	if in.Secret == "" || in.Code == "" {
		// THE FIRST LEG: a seed, and nothing written. A person who
		// never completes the second leg has enrolled nothing, which is
		// the correct outcome rather than a half-enrolled factor.
		secret, err := credential.NewTOTPSecret()
		if err != nil {
			log.ErrorContext(r.Context(), "api_totp_secret_failed", "error", err)
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
			return
		}
		httpjson.Write(w, http.StatusOK, totpEnrolResponse{
			Secret: secret,
			URI:    credential.TOTPURI(s.issuerLabel(), principal.Login, secret),
		})
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

	person := principal.ID.String()
	// THE ID IS MINTED ONCE, outside the apply: the decide may run again
	// against a fresh snapshot, and an id minted inside it would be a
	// different credential on each run — and a different one again in the
	// event that says which was enrolled.
	id := uuid.New().String()
	const reason = "enrolled a second factor"
	stored, err := s.writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			return append(without(held, iamdomain.MethodTOTP),
				totpCredential(id, in.Secret, step))
		},
		OpID:   "totp:" + person + ":" + id,
		Reason: reason,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_totp_enrol_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	if !landed(stored) {
		// NOT "enrolled": nothing can say the factor is on the log. The
		// second leg carries its secret, so presenting it again with a
		// fresh code enrols it — the same factor whichever attempt lands.
		unresolved(w, r, "api_totp_enrol_unresolved", stored)
		return
	}
	s.audit.Emit(r.Context(), types.IAMCredentialMinted{
		Credential: id, Kind: types.CredentialTOTP, Owner: person,
		By: iam.ActorFor(principal).Name, Reason: reason,
	})
	log.InfoContext(r.Context(), "api_totp_enrolled", "person", person)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "enrolled"})
}

// RegenerateRecovery issues a fresh set of single-use codes, retiring the old.
//
// THE OLD SET IS REPLACED RATHER THAN EXTENDED, which is what makes this the
// move after a set is lost or printed somewhere it should not have been: a set
// that merely grew would leave whatever leaked still working.
func (s *Service) RegenerateRecovery(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.steppedUp(w, r)
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
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			return append(without(held, iamdomain.MethodRecovery),
				recoveryCredential(id, verifiers))
		},
		OpID:   "recovery:" + person + ":" + id,
		Reason: reason,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_recovery_store_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
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
		By: iam.ActorFor(principal).Name, Reason: reason,
	})
	log.InfoContext(r.Context(), "api_recovery_regenerated", "person", person)
	// ANSWERED ONCE AND NEVER AGAIN. What is stored is the hashes, so a
	// person who loses this answer regenerates rather than recovers — and
	// that is the property that makes a leaked estate not a set of
	// bypasses.
	httpjson.Write(w, http.StatusOK, totpRecoveryResponse{Codes: codes})
}

// steppedUp resolves the caller and refuses one whose proof of identity is
// stale.
//
// THE SAME THRESHOLD THE CLIENT IS WARNED AT, read from the one setting: a
// gate that refused at a different number from the one a screen says would
// either nag early or surprise late, and both read as a bug in the engine.
func (s *Service) steppedUp(w http.ResponseWriter, r *http.Request) (iam.Principal, bool) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return iam.Principal{}, false
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return iam.Principal{}, false
	}
	if principal.Kind != iam.KindPerson {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
			map[string]string{
				"detail": "a machine credential holds no second factor",
			})
		return iam.Principal{}, false
	}
	if machineToken(w, r) {
		return iam.Principal{}, false
	}
	if s.stepUpDue(principal) {
		httpjson.Fail(w, http.StatusForbidden, httpjson.CodeStepUpRequired)
		return iam.Principal{}, false
	}
	return principal, true
}

// machineToken refuses a request that presented a machine token, on a gesture
// that manages the proof its owner signs in with, and says whether it did.
//
// ASKED OF THE CREDENTIAL AND NEVER OF THE PRINCIPAL: a token acts as its
// owner, so the principal is a person with a fresh step-up clock and nothing
// on it says a person is absent. What the request PRESENTED is the one fact
// that does, and it is the guard's to state ([auth.PresentedToken]).
func machineToken(w http.ResponseWriter, r *http.Request) bool {
	if _, fromToken := auth.PresentedToken(r.Context()); !fromToken {
		return false
	}
	httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
		map[string]string{
			"detail": "a machine token proves nobody is present, so it cannot " +
				"confirm its owner's identity or change how they prove it; " +
				"sign in as the person",
		})
	return true
}

// issuerLabel is what an authenticator app shows beside the account.
//
// THE DEPLOYMENT'S OWN HOST, because a person may hold accounts on several
// Crewlet deployments and a constant would render them as identical rows in
// one app — which is how somebody types the staging code into production.
func (s *Service) issuerLabel() string {
	if label := providerLabel(s.boot.API.ExternalBase()); label != "" {
		return label
	}
	return "Crewlet"
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

// totpCredential builds the stored second factor.
func totpCredential(id, secret string, step int64) iamdomain.Credential {
	raw, _ := json.Marshal(step)
	return iamdomain.Credential{
		V: iamdomain.DocumentVersion, ID: id,
		Method: iamdomain.MethodTOTP, Verifier: secret,
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
