package authapi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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
	if !s.admit(w, r, source, types.FailPassword) {
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
	// THE ATTEMPT AS THE AUDIT TRAIL COUNTS IT: the value typed goes in as
	// the subject — keyed in memory and never kept — and the person only
	// once THIS ENGINE resolved one, so a failure names somebody real
	// rather than whatever the caller claimed to be.
	attempt := authevents.Failure{
		Client: source, Method: types.FailPassword, Subject: in.Login,
		Person: held.ID,
	}
	// THE DECOY RUNS ON THE MISS, and it is not optional. Without it the
	// no-such-login arm returns in microseconds and the wrong-password arm
	// pays an argon2 verify — a difference a stopwatch reads as a roster.
	// internal/iam/credential owns the cost; this is the branch that
	// spends it.
	if held.ID == "" {
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, attempt, "no such "+method)
		return
	}
	if !stageAdmits(held.Stage) {
		// A SUSPENDED PERSON PAYS THE VERIFY TOO. Skipping it would make
		// suspension measurable: an attacker with a known-good password
		// would learn from the timing alone which accounts had been
		// turned off, which is the roster again in a different shape.
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, attempt, "stage "+string(held.Stage))
		return
	}

	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		// A PERSON WITH NO PASSWORD — enrolled through a provider, or
		// invited and not yet redeemed. The decoy again, for the same
		// reason the stage arm pays it.
		s.throttle.Decoy(in.Password)
		s.refuseSignIn(w, r, arrived, attempt, "no password credential")
		return
	}
	ok, _ := s.hasher.Verify(verifier.Verifier, in.Password)
	if !ok {
		s.refuseSignIn(w, r, arrived, attempt, "password mismatch")
		return
	}

	// THE FIRST FACTOR CHECKED OUT, so what follows may be specific: the
	// caller has proved who they are, and telling them a second factor is
	// needed discloses nothing to anybody else.
	var factor factorUse
	if holdsSecondFactor(held) {
		if in.Code == "" {
			s.throttle.Pad(r.Context(), arrived)
			httpjson.Fail(w, http.StatusUnauthorized,
				httpjson.CodeSecondFactorRequired)
			return
		}
		attempt.Method = types.FailSecondFactor
		var proved bool
		if factor, proved = s.proveSecondFactor(w, r, arrived, attempt, held,
			in.Code); !proved {
			return
		}
	}

	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, held, signIn{
		method: types.SignInPassword, factor: factor.factor,
	})
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
		blinder, err := s.blinder.Blinder(ctx)
		if err != nil {
			log.WarnContext(ctx, "api_sign_in_blind_failed", "error", err)
			return iamdomain.Sighting{}, "address"
		}
		blind, err := blinder.Email(typed)
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

// holdsSecondFactor reports whether this person holds any second factor.
//
// HELD RATHER THAN CONFIGURED, and the difference is what makes
// `second_factor: optional` mean anything: the deployment says whether one may
// be required, and the PERSON's own credentials say whether one is. A
// deployment that requires it refuses a person who holds none at enrolment
// rather than here, which is where somebody can still do something about it.
func holdsSecondFactor(held iamdomain.Sighting) bool {
	for _, method := range iamdomain.SecondFactorMethods {
		if _, ok := firstCredential(held.Credentials, method); ok {
			return true
		}
	}
	return false
}

// factorUse is a second factor that checked out, and what spending it takes.
type factorUse struct {
	factor types.SecondFactor

	// credential is the id of the credential that matched.
	credential string

	// step is the TOTP step the code was for; verifier is the recovery
	// code's own digest.
	step     int64
	verifier string

	// remaining is how many recovery codes are left once this one is
	// spent, as the spend itself established.
	remaining int
}

// checkSecondFactor verifies a presented code against EVERY second factor the
// person holds, and reports which one it proved.
//
// EVERY ONE, and that is a repair: it used to check only the first factor held
// — the authenticator app, whenever there was one — so a recovery code, which
// exists for exactly the day the app is lost, was checked against the app's
// six digits and refused for everybody who had both. Both are evaluated
// whatever the first answered, so the time taken does not say which one a
// person holds; the app wins where both would match.
func (s *Service) checkSecondFactor(held iamdomain.Sighting, code string) (factorUse, bool) {
	var app, recovery factorUse
	appOK, recoveryOK := false, false
	if c, ok := firstCredential(held.Credentials, iamdomain.MethodTOTP); ok {
		// THE LAST ACCEPTED STEP is what makes a code single-use, and it
		// is read from the credential rather than kept in memory: the
		// node that accepts the next code is rarely the node that
		// accepted the last.
		step, matched := credential.VerifyTOTP(c.Verifier, code, s.now(), lastStep(c))
		app = factorUse{factor: types.FactorTOTP, credential: c.ID, step: step}
		appOK = matched
	}
	if c, ok := firstCredential(held.Credentials, iamdomain.MethodRecovery); ok {
		verifiers := recoveryVerifiers(c)
		if i := credential.SpendRecoveryCode(verifiers, code); i >= 0 {
			recovery = factorUse{factor: types.FactorRecovery, credential: c.ID,
				verifier: verifiers[i], remaining: len(verifiers) - 1}
			recoveryOK = true
		}
	}
	switch {
	case appOK:
		return app, true
	case recoveryOK:
		return recovery, true
	}
	return factorUse{}, false
}

// errFactorSpent reports a second factor that checked out and had been spent
// by the time the spend was recorded: a code used twice, the second time
// concurrently.
var errFactorSpent = errors.New("authapi: this second factor was already spent")

// spendSecondFactor records a second factor as used, so it can never prove
// anybody again.
//
// # It is what makes a code single-use, and it was missing
//
// A TOTP code carries its step and a recovery code is one of ten, and both
// are only single-use if the use is WRITTEN: the step onto the credential,
// the recovery verifier off it. Neither was — so an observed six-digit code
// worked for the whole drift window, and a recovery code worked for ever.
//
// # Decided inside the write's own snapshot
//
// The set it writes is formed from the credential as the DECIDE reads it, and
// if that already records this step, or no longer holds this recovery code,
// the code was spent by somebody else between this node's check and this
// write — the second of two concurrent uses — and the answer is
// [errFactorSpent] rather than a sign-in. A decide may run again against a
// fresh snapshot, so the verdict is the LAST run's.
//
// A FRESH OPERATION ID PER USE, never one derived from the step: two uses of
// one code under one id would collapse into one record in the operation
// ledger, and the second would read the first's success as its own.
//
// THE WRITE'S OWN ANSWER comes back beside the use, because only a spend that
// LANDED makes the code single-use: an unknown one may not be on the log, and
// a session opened on it would be a session on a code that still works.
func (s *Service) spendSecondFactor(ctx context.Context, person string,
	use factorUse) (factorUse, statelog.Result, error) {

	spent := false
	remaining := use.remaining
	result, err := s.writer.SetCredentials(ctx, iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			spent = true
			out := make([]iamdomain.Credential, 0, len(held))
			for _, c := range held {
				if c.ID != use.credential || !c.RevokedAt.IsZero() {
					out = append(out, c)
					continue
				}
				switch use.factor {
				case types.FactorTOTP:
					if lastStep(c) < use.step {
						spent = false
						c = withExtra(c, "last_step", use.step)
					}
				case types.FactorRecovery:
					verifiers := recoveryVerifiers(c)
					if i := slices.Index(verifiers, use.verifier); i >= 0 {
						spent = false
						left := slices.Delete(slices.Clone(verifiers), i, i+1)
						remaining = len(left)
						c = withExtra(c, "verifiers", left)
					}
				}
				out = append(out, c)
			}
			return out
		},
		OpID:   "second-factor:" + person + ":" + uuid.NewString(),
		Reason: "spent a " + string(use.factor) + " second factor",
	})
	if err != nil {
		return factorUse{}, result, err
	}
	if !landed(result) {
		return factorUse{}, result, nil
	}
	if spent {
		return factorUse{}, result, errFactorSpent
	}
	use.remaining = remaining
	return use, result, nil
}

// withExtra is a credential with one carried field replaced, on a copy of its
// map so the row the decide read is never written through.
func withExtra(c iamdomain.Credential, key string, value any) iamdomain.Credential {
	raw, err := json.Marshal(value)
	if err != nil {
		return c
	}
	extra := make(map[string]json.RawMessage, len(c.Extra)+1)
	maps.Copy(extra, c.Extra)
	extra[key] = raw
	c.Extra = extra
	return c
}

// proveSecondFactor checks a presented code and spends it, answering false
// once it has written the refusal.
//
// ONE PATH FOR THE SIGN-IN AND THE STEP-UP, which are the two places a second
// factor is presented, so the check, the spend and the refusal are decided
// once. A recovery code spent here is ALSO its own event, because a person
// down to their last one is one lost phone away from needing an administrator.
func (s *Service) proveSecondFactor(w http.ResponseWriter, r *http.Request,
	arrived time.Time, attempt authevents.Failure, held iamdomain.Sighting,
	code string) (factorUse, bool) {

	use, ok := s.checkSecondFactor(held, code)
	if !ok {
		// STILL THE GENERIC REFUSAL, because a wrong CODE and a wrong
		// password must not be distinguishable to somebody who has
		// stolen one of the two.
		s.refuseSignIn(w, r, arrived, attempt, "second factor mismatch")
		return factorUse{}, false
	}
	use, spend, err := s.spendSecondFactor(r.Context(), held.ID, use)
	switch {
	case errors.Is(err, errFactorSpent):
		s.refuseSignIn(w, r, arrived, attempt, "second factor already spent")
		return factorUse{}, false
	case err != nil:
		// NOT A REFUSAL: the code was right, and this node could not
		// record that it was used. Signing somebody in on a code that
		// stays usable is the replay the spend exists to close, so the
		// honest answer is that this node cannot finish the sign-in now.
		log.ErrorContext(r.Context(), "api_second_factor_unspent",
			"person", held.ID, "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return factorUse{}, false
	case !landed(spend):
		// AND NOT A SPEND EITHER: nothing can establish whether it is on
		// the log, which for a code is the same as not having recorded
		// it. A retry presents the code again and is decided afresh — a
		// spend that did land refuses it as spent.
		unresolved(w, r, "api_second_factor_unresolved", spend)
		return factorUse{}, false
	}
	if use.factor == types.FactorRecovery {
		s.audit.Emit(r.Context(), types.IAMRecoveryCodeUsed{
			Person: held.ID, Login: held.Login, Remaining: use.remaining,
			Remote: attempt.Client,
		})
	}
	return use, true
}

// signIn is how a completed sign-in was proved, for the event it announces.
type signIn struct {
	method types.SignInMethod
	factor types.SecondFactor

	// acr is what an identity provider asserted about the authentication
	// it performed, and empty on every other method.
	acr string

	// stepUp marks a signed-in person confirming who they are again, which
	// announces itself as a step-up rather than as a fresh sign-in; replaces
	// is the session the confirmation was made from, which is ENDED before
	// its replacement opens, and absolute is that session's own deadline,
	// which the replacement keeps — see [Service.StepUp].
	stepUp   bool
	replaces string
	absolute time.Time

	// groupGrants are what the identity provider's groups conferred, and
	// empty on every other method. They ride into the session record and
	// never onto the person — see [iamdomain.Session.GroupGrants].
	groupGrants []iam.Grant

	// redirect is where a BROWSER that arrived by navigation goes next,
	// rather than a JSON body it has no script to read. Empty answers
	// JSON, which is what every fetch-driven route wants.
	redirect string

	// refresh is the refresh token a PROVIDER sign-in obtained, and empty
	// for every other way in: the deactivation probe asks the provider
	// with it, so it goes into custody beside the session it belongs to —
	// see [Service.keep].
	refresh string

	// provedAt is when a PROVIDER sign-in's person authenticated at the
	// provider ([oidc.Flight.ProvedAt]) — possibly long ago, and the zero
	// time when the provider did not say. Every other way in was proved
	// HERE, at this instant, and ignores it.
	provedAt time.Time
}

// proofOf is the instant a sign-in proved who somebody is: the provider's own
// for a provider sign-in, and this one for everything this surface verified
// itself.
func (s *Service) proofOf(how signIn) time.Time {
	if how.method == types.SignInOIDC {
		return how.provedAt
	}
	return s.now()
}

// completeSignIn opens the session and sets the cookie.
func (s *Service) completeSignIn(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting, how signIn) {

	lineage, err := uuid.NewV7()
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_lineage_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	expires := s.now().Add(s.boot.API.Auth.Session.Absolute())
	if !how.absolute.IsZero() {
		expires = how.absolute
	}
	if how.replaces != "" {
		// THE SESSION BEING REPLACED ENDS FIRST, and a failure to end it
		// fails the gesture. In the other order a close that did not land
		// would leave the new session open beside the old one — the
		// second live session this exists to prevent — or need a
		// compensating close of a session whose open record this node
		// may not have applied. This way round a failure changes nothing
		// and the retry is clean; the one residue, an open that fails
		// after the close landed, is a person asked to sign in again.
		closed, closeErr := s.writer.CloseSession(r.Context(), how.replaces,
			held.ID, "replaced by a step-up", "step-up:"+how.replaces)
		if closeErr != nil {
			log.WarnContext(r.Context(), "api_step_up_close_failed",
				"error", closeErr, "lineage", how.replaces)
			httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
			return
		}
		if !landed(closed) {
			// THE SAME GESTURE FAILING, for the same reason: opening the
			// replacement beside a close nothing can confirm is the
			// second live session this order exists to prevent.
			unresolved(w, r, "api_step_up_close_unresolved", closed)
			return
		}
	}
	opened, err := s.writer.OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: held.ID,
		AbsoluteExpiresAt: expires,
		// EVERY PATH HERE IS A PROOF — a password and its second factor,
		// an identity provider's token, an invitation, the bootstrap
		// code, a step-up — so the session is fresh from the instant it
		// was proved, and a step-up surface asks again once this node's
		// window has passed. It is the one field that says so: without it
		// every session was stale from its first request. A PROVIDER'S
		// proof is dated by the provider, because it answers from its own
		// session and a token received now may carry an authentication
		// from last week — see [Service.proofOf].
		ProvedAt:    s.proofOf(how),
		GroupGrants: how.groupGrants,
		OpID:        "session:" + lineage.String(),
		// THE ONE WRITE IN THIS ESTATE THAT DOES NOT WAIT, because
		// nothing in this answer reads the row: the bearer carries the
		// position and every node validates against its own applier.
		NoWait: true,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_session_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}
	if !landed(opened.Result) {
		// NO BEARER FROM AN UNRESOLVED START. It would carry position
		// zero, which every node that has applied anything reads as a
		// session that ended — a cookie that signs its holder out on
		// their first request, beside an event saying they signed in.
		unresolved(w, r, "api_sign_in_session_unresolved", opened.Result)
		return
	}
	at := opened.Result.Position
	if how.refresh != "" && !s.keep(r, lineage.String(), held.ID, how.refresh, at) {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
		return
	}

	// THE EPOCH AND THE GENERATION THE SESSION WAS OPENED AT, as the
	// domain read them in the snapshot it formed the record in. A bearer
	// carrying zero for either is one the first revocation or the first
	// fleet-wide invalidation ends — and every sign-in after it too.
	bearer, err := s.signer.Mint(session.Mint{
		Lineage: lineage, Person: held.ID,
		Epoch: opened.Epoch, Generation: opened.Generation,
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
	if how.stepUp {
		s.audit.Emit(r.Context(), types.IAMStepUpCompleted{
			Person: held.ID, Login: held.Login, Lineage: lineage.String(),
			Replaces: how.replaces, SecondFactor: how.factor,
			Remote: s.sourceOf(r),
		})
	} else {
		s.audit.Emit(r.Context(), types.IAMSessionStarted{
			Person: held.ID, Login: held.Login, Method: how.method,
			Lineage: lineage.String(), Remote: s.sourceOf(r),
			SecondFactor: how.factor, ACR: how.acr, ExpiresAt: expires,
		})
	}
	if how.redirect != "" {
		// A BROWSER THAT ARRIVED BY NAVIGATION leaves the same way. The
		// cookie is on this response, so the page it lands on is signed
		// in; a JSON body here was what the provider's callback answered,
		// which a browser renders as text and goes nowhere.
		http.Redirect(w, r, how.redirect, http.StatusFound)
		return
	}
	httpjson.Write(w, http.StatusOK, loginResponse{
		Person: held.ID, Login: held.Login, Seat: held.Seat,
		ExpiresAt: expires, Position: at.String(),
	})
}

// keep takes custody of a provider sign-in's refresh token, reporting whether
// the sign-in may go on.
//
// A SIGN-IN WHOSE TOKEN COULD NOT BE KEPT IS REFUSED, and the session it just
// opened is closed. Admitted, it would be a session the deactivation probe
// never sees — somebody disabled at the provider keeping it until its absolute
// deadline, which is the one thing the probe exists to prevent; refused, it
// costs the person one retry. The close is best effort and is the node's own
// record under the lineage's op id: no cookie was issued, so nothing can
// present the session either way, and the close only keeps the sessions
// screen from listing it as live.
func (s *Service) keep(r *http.Request, lineage, person, refresh string,
	at statelog.Position) bool {

	err := s.custody.Hold(r.Context(), iamdomain.RefreshGrant{
		Lineage: lineage, Person: person,
		Issuer: s.provider.Config().Issuer, Token: refresh,
		Start: uint64(at.Packed()),
	}, s.now())
	if err == nil {
		return true
	}
	log.ErrorContext(r.Context(), "api_sign_in_refresh_unkept",
		"person", person, "lineage", lineage, "error", err,
		"detail", "the provider's refresh token could not be kept, so the "+
			"deactivation probe could never ask about this session; the "+
			"sign-in is refused rather than admitted unprobed")
	// WITHOUT CANCEL: the request is about to be answered and its context
	// ended, and a cleanup that inherits a dead context does nothing.
	closed, err := s.writer.CloseSession(context.WithoutCancel(r.Context()),
		lineage, person, reasonRefreshUnkept, "close:"+lineage)
	if err != nil || !landed(closed) {
		log.WarnContext(r.Context(), "api_sign_in_session_left_open",
			"lineage", lineage, "error", errText(err), "op_id", closed.OpID,
			"outcome", string(closed.Outcome))
	}
	return false
}

// reasonRefreshUnkept is what a session closed by [Service.keep] records.
const reasonRefreshUnkept = "refresh_unkept"

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
