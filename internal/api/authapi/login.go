package authapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// maxLoginBody bounds what a sign-in will read.
//
// FOUR KILOBYTES, which is a login, a password and a six-digit code with room
// to spare — and it is deliberately small because this route is UNGUARDED:
// whoever can reach the port can post to it, so the body limit is one of the
// few things bounding what an anonymous caller can make this node allocate.
const maxLoginBody = 4 << 10

// loginRequest is what a sign-in presents.
type loginRequest struct {
	// Login is the person's own login, or their address. Both are
	// accepted because a person knows one or the other and neither is a
	// disclosure: a wrong one of either answers exactly as a wrong
	// password does.
	Login    string `json:"login"`
	Password string `json:"password"`

	// Code is the second factor, when one is being presented. Absent on
	// the first leg, which is what produces the second-factor prompt.
	Code string `json:"code"`
}

// loginResponse is what a completed sign-in answers.
//
// IT CARRIES NO BEARER. The cookie is the credential, set on the response, and
// a body that also carried it would put the one value most worth stealing
// somewhere a script can read and a log can keep.
type loginResponse struct {
	// Person is the caller's own id, which a client needs to address its
	// own record. Not a name and not an address: the client reads those
	// from the session route once it is signed in.
	Person string `json:"person"`

	// Login is what they are known as, echoed so a client renders the
	// same spelling the estate holds rather than the one that was typed.
	Login string `json:"login"`

	// Seat is the chart seat this person holds, or empty. It is what makes
	// somebody act as THEMSELVES rather than as the credential.
	Seat string `json:"seat,omitempty"`

	// ExpiresAt is the absolute deadline no re-issue moves, so a client
	// can say when the session ends rather than discovering it.
	ExpiresAt time.Time `json:"expires_at"`

	// Position is where the session's start record landed.
	//
	// IT IS IN THE ANSWER BECAUSE THE WRITE DID NOT WAIT. The record is
	// durable and this node has not necessarily applied it, so a client
	// that immediately asks a question this node answers from its own
	// rows has a position to require — and, far more often, simply never
	// needs it, because the bearer carries the same number and every node
	// validates against its own applier.
	Position string `json:"position"`
}

// Login signs somebody in with what they know.
//
// # UNGUARDED, and what stands in for the guard
//
// A login cannot require a login. What bounds it instead is the per-SOURCE
// throttle, run before anything is looked up, and the pad that makes every
// refusal leave at one deadline. It is origin-checked like every other state
// change — being exempt from the credential guard is not being exempt from the
// cross-site rule.
//
// # One refusal, and why the shape is the whole of it
//
// Every way this can fail answers [httpjson.CodeSignInRefused] with a 401, at
// the same instant: a login nobody holds, a wrong password, a person
// suspended, a person removed, a second factor that does not check out. The
// arms are distinguishable only in this node's log. See the package doc for
// why both halves are needed.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	arrived := s.now()
	source := s.sourceOf(r)
	if !s.admit(w, r, source) {
		return
	}
	if s.backend() != config.AuthBackendLocal {
		// A DEPLOYMENT THAT SIGNS IN THROUGH A PROVIDER SERVES NO
		// PASSWORD ROUTE, and it says so rather than refusing as though
		// the credentials were wrong: this is a fact about the
		// deployment that every caller may know, and answering
		// `sign_in_refused` would send somebody to reset a password
		// this company does not have.
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownQuery,
			map[string]string{
				"detail": "this deployment does not sign in with passwords",
				"hint":   "see GET /auth/config for how it does",
			})
		return
	}

	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in loginRequest
	if err := json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}

	held, method := s.resolve(r.Context(), in.Login)
	// THE DECOY RUNS ON THE MISS, and it is not optional. Without it the
	// no-such-login arm returns in microseconds and the wrong-password arm
	// pays an argon2 verify — a difference a stopwatch reads as a roster.
	// internal/iam/credential owns the cost; this is the branch that
	// spends it.
	if held.ID == "" {
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, source, "no such "+method)
		return
	}
	if !stageAdmits(held.Stage) {
		// A SUSPENDED PERSON PAYS THE VERIFY TOO. Skipping it would make
		// suspension measurable: an attacker with a known-good password
		// would learn from the timing alone which accounts had been
		// turned off, which is the roster again in a different shape.
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, source, "stage "+string(held.Stage))
		return
	}

	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		// A PERSON WITH NO PASSWORD — enrolled through a provider, or
		// invited and not yet redeemed. The decoy again, for the same
		// reason the stage arm pays it.
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, source, "no password credential")
		return
	}
	ok, _ := s.hasher.Verify(verifier.Verifier, in.Password)
	if !ok {
		s.refuseSignIn(w, r, arrived, source, "password mismatch")
		return
	}

	// THE FIRST FACTOR CHECKED OUT, so what follows may be specific: the
	// caller has proved who they are, and telling them a second factor is
	// needed discloses nothing to anybody else.
	if factor, need := s.secondFactor(held); need {
		if in.Code == "" {
			s.throttle.Pad(r.Context(), arrived)
			httpjson.Fail(w, http.StatusUnauthorized,
				httpjson.CodeSecondFactorRequired)
			return
		}
		if !s.checkSecondFactor(factor, in.Code) {
			// STILL THE GENERIC REFUSAL, because a wrong CODE and a
			// wrong password must not be distinguishable to somebody
			// who has stolen one of the two.
			s.refuseSignIn(w, r, arrived, source, "second factor mismatch")
			return
		}
	}

	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, held)
}

// resolve finds the person a sign-in names, by login or by address.
//
// IT REPORTS WHICH SPELLING IT TRIED, for the log line alone: an operator
// diagnosing a failed sign-in needs to know whether the value was read as a
// login or as an address, and the caller is told neither.
func (s *Service) resolve(ctx context.Context, typed string) (iamdomain.Sighting, string) {
	if typed == "" {
		return iamdomain.Sighting{}, "login"
	}
	// AN ADDRESS IS TRIED AS AN ADDRESS, by its BLIND. A sign-in runs
	// before anybody is authenticated, so the lookup must not carry the
	// address itself — and the blind has to be derived the same way the
	// chart derives its own index, or one address would reach a seat and a
	// different person.
	if looksLikeAddress(typed) {
		blind, err := s.blinder.Email(typed)
		if err != nil {
			log.WarnContext(ctx, "api_sign_in_blind_failed", "error", err)
			return iamdomain.Sighting{}, "address"
		}
		held, err := s.directory.PersonByEmailBlind(ctx, blind)
		if err != nil {
			log.WarnContext(ctx, "api_sign_in_lookup_failed", "error", err)
			return iamdomain.Sighting{}, "address"
		}
		return held, "address"
	}
	held, err := s.directory.PersonByLogin(ctx, typed)
	if err != nil {
		// AN ERROR IS NOT "NO SUCH PERSON" and is not reported as one to
		// the caller either — they get the same refusal as everybody
		// else, which is correct here for once: telling somebody the
		// store is down is telling them this login might be real.
		log.WarnContext(ctx, "api_sign_in_lookup_failed", "error", err)
		return iamdomain.Sighting{}, "login"
	}
	return held, "login"
}

// looksLikeAddress reports whether a typed credential is an address.
//
// ONE '@' AND SOMETHING EITHER SIDE, which is deliberately the whole rule. A
// login cannot contain '@' — internal/iam's grammar joins a person's login
// with dots and a machine's with a colon — so the presence of one is
// unambiguous, and anything stricter would refuse an address the estate holds
// rather than merely failing to find it.
func looksLikeAddress(typed string) bool {
	at := -1
	for i := range len(typed) {
		if typed[i] == '@' {
			if at >= 0 {
				return false
			}
			at = i
		}
	}
	return at > 0 && at < len(typed)-1
}

// firstCredential finds a live credential of one method.
//
// LIVE: a revoked or expired one is not a credential, and a caller that looked
// only at the method would verify against a password somebody already retired.
func firstCredential(held []iamdomain.Credential, method iamdomain.CredentialMethod) (
	iamdomain.Credential, bool) {

	for _, c := range held {
		if c.Method != method || !c.RevokedAt.IsZero() {
			continue
		}
		return c, true
	}
	return iamdomain.Credential{}, false
}

// secondFactor reports which second factor this person holds, if any.
//
// HELD RATHER THAN CONFIGURED, and the difference is what makes
// `second_factor: optional` mean anything: the deployment says whether one may
// be required, and the PERSON's own credentials say whether one is. A
// deployment that requires it refuses a person who holds none at enrolment
// rather than here, which is where somebody can still do something about it.
func (s *Service) secondFactor(held iamdomain.Sighting) (iamdomain.Credential, bool) {
	for _, method := range iamdomain.SecondFactorMethods {
		if c, ok := firstCredential(held.Credentials, method); ok {
			return c, true
		}
	}
	return iamdomain.Credential{}, false
}

// checkSecondFactor verifies a presented code against the credential that
// holds it.
func (s *Service) checkSecondFactor(factor iamdomain.Credential, code string) bool {
	switch factor.Method {
	case iamdomain.MethodTOTP:
		// THE LAST ACCEPTED STEP is what makes a code single-use, and it
		// is read from the credential rather than kept in memory: the
		// node that accepts the next code is rarely the node that
		// accepted the last.
		_, ok := credential.VerifyTOTP(factor.Verifier, code, s.now(), lastStep(factor))
		return ok
	case iamdomain.MethodRecovery:
		return credential.SpendRecoveryCode(recoveryVerifiers(factor), code) >= 0
	}
	return false
}

// completeSignIn opens the session and sets the cookie.
func (s *Service) completeSignIn(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting) {

	lineage, err := uuid.NewV7()
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_lineage_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	expires := s.now().Add(s.boot.API.Auth.Session.Absolute())
	at, err := s.writer.OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: held.ID,
		AbsoluteExpiresAt: expires,
		OpID:              "session:" + lineage.String(),
		// THE ONE WRITE IN THIS ESTATE THAT DOES NOT WAIT, because
		// nothing in this answer reads the row: the bearer carries the
		// position and every node validates against its own applier.
		NoWait: true,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_session_failed", "error", err)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}

	bearer, err := s.signer.Mint(session.Mint{
		Lineage: lineage, Person: held.ID,
		StartPosition:     uint64(at.Packed()),
		AbsoluteExpiresAt: expires,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	http.SetCookie(w, session.Cookie(s.boot.API.ExternalBase(), bearer, expires))
	log.InfoContext(r.Context(), "api_sign_in",
		"person", held.ID, "login", held.Login, "seat", held.Seat,
		"position", at.String())
	httpjson.Write(w, http.StatusOK, loginResponse{
		Person: held.ID, Login: held.Login, Seat: held.Seat,
		ExpiresAt: expires, Position: at.String(),
	})
}

// backend is how this deployment signs people in.
func (s *Service) backend() config.AuthBackend {
	return s.boot.API.Auth.Resolved()
}

// sourceOf is what the throttle keys on.
//
// THE CLIENT, RESOLVED THROUGH THE TRUSTED PROXIES, and never the login: keyed
// on the subject a throttle is an oracle — "this account exists and I can lock
// it" — and keyed on a client the only thing it discloses is a rate limit the
// caller already met.
//
// THROUGH THE GUARD, because resolving it is the one place `api.trusted_proxies`
// is read: keyed on a proxy's own address this throttle would bucket the whole
// internet together and lock the company out the moment one attacker arrives,
// and keyed on a header anybody may send it would let that attacker pick their
// own bucket. See internal/api/auth/client.go.
func (s *Service) sourceOf(r *http.Request) string { return s.clients.Of(r) }

// lastStep reads the last accepted TOTP step off a credential's carried
// fields, or zero.
//
// ZERO MEANS "NOTHING ACCEPTED YET", which is the honest reading of an absent
// field and the right one: a code cannot have been replayed if none has been
// spent.
func lastStep(c iamdomain.Credential) int64 {
	raw, ok := c.Extra["last_step"]
	if !ok {
		return 0
	}
	var step int64
	if err := json.Unmarshal(raw, &step); err != nil {
		return 0
	}
	return step
}

// recoveryVerifiers reads the hashed recovery codes off a credential.
func recoveryVerifiers(c iamdomain.Credential) []string {
	raw, ok := c.Extra["verifiers"]
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
