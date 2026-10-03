package authapi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
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

	// Status is what the session just opened may do: everything its
	// grants allow, or — on a deployment that requires a second factor
	// the person does not hold — nothing but enrol one.
	//
	// IN THE ANSWER, and not only in the refusal every other route would
	// give, because a client has to render the enrolment rather than
	// discover it: a sign-in that succeeded and then answered 403 on the
	// first screen reads as a broken deployment.
	Status sessionStatus `json:"status"`
}

// sessionStatus is what a session may do, as a sign-in and `GET /auth/session`
// report it.
type sessionStatus string

const (
	// statusSignedIn is an ordinary session.
	statusSignedIn sessionStatus = "signed_in"

	// statusEnrolmentRequired is a session that may only enrol a second
	// factor — the code the guard refuses every other route with, so a
	// client matches one string wherever it meets it.
	statusEnrolmentRequired = sessionStatus(httpjson.CodeSecondFactorEnrolmentRequired)
)

// Login signs somebody in with what they know.
//
// # UNGUARDED, and what stands in for the guard
//
// A login cannot require a login. What bounds it instead is the throttle's
// curve — on the login as it was typed, from its source, run before anything
// is looked up — and the argon2id derivation every arm spends.
// It is origin-checked like every other state change — being exempt from the
// credential guard is not being exempt from the cross-site rule.
//
// # One refusal, and why the shape is the whole of it
//
// Every way this can fail answers [httpjson.CodeSignInRefused] with a 401,
// each after one derivation at this build's cost: a login nobody holds, a
// wrong password, a person suspended, a person removed, a second factor that
// does not check out. The arms are distinguishable only in this node's log.
// See the package doc for why both halves are needed.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	source := s.sourceOf(r)
	if s.backend() != config.AuthBackendLocal {
		// A DEPLOYMENT WHOSE PEOPLE DO NOT SIGN IN SERVES NO PASSWORD
		// ROUTE, and it says so rather than refusing as though the
		// credentials were wrong: this is a fact about the deployment
		// that every caller may know, and answering `sign_in_refused`
		// would send somebody to reset a password this company does not
		// have.
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
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	// THE CURVE, ON WHAT WAS TYPED — before it is looked up, so a login
	// nobody holds climbs it exactly as a real one does.
	adm, ok := s.admit(w, r, credential.Attempt{
		Source: source, Subject: in.Login}, types.FailPassword)
	if !ok {
		return
	}
	defer adm.ticket.Release()

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
	// internal/iam/credential owns the cost; this is the branch that spends
	// it.
	if held.ID == "" {
		if s.decoy(w, r, adm, in.Password) {
			s.refuseSignIn(w, r, adm, attempt, "no such "+method)
		}
		return
	}
	if !stageAdmits(held.Stage) {
		// A SUSPENDED PERSON PAYS THE VERIFY TOO. Skipping it would make
		// suspension measurable: an attacker with a known-good password
		// would learn from the timing alone which accounts had been
		// turned off, which is the roster again in a different shape.
		if s.decoy(w, r, adm, in.Password) {
			s.refuseSignIn(w, r, adm, attempt, "stage "+string(held.Stage))
		}
		return
	}

	verifier, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		// A PERSON WITH NO PASSWORD — one who never set one, or whose
		// password was revoked. The decoy again, for the same reason the
		// stage arm pays it.
		if s.decoy(w, r, adm, in.Password) {
			s.refuseSignIn(w, r, adm, attempt, "no password credential")
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
		s.refuseSignIn(w, r, adm, attempt, "password mismatch")
		return
	}

	// THE FIRST FACTOR CHECKED OUT, so what follows may be specific: the
	// caller has proved who they are, and telling them a second factor is
	// needed discloses nothing to anybody else.
	var factor factorUse
	if holdsSecondFactor(held) {
		if in.Code == "" {
			// NEITHER A SUCCESS NOR A FAILURE, and the ticket is released
			// as one: the password proved itself and the sign-in is not
			// complete. Counted as a success it would clear the pair, and
			// somebody holding the password would clear their curve
			// between every guess at the code.
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
	// THE SIGN-IN HAS SUCCEEDED, so this is the one instant a verifier
	// written under an older cost can be rewritten: the plaintext is in hand.
	// DEFERRED, so it starts once this has answered — see
	// [Service.rehashPassword].
	if stale {
		defer s.rehashPassword(r, held.ID, verifier, in.Password)
	}

	adm.ticket.Succeed()
	s.completeSignIn(w, r, held, signIn{
		method: types.SignInPassword, factor: factor.factor,
	})
}

// rehashPassword rewrites a password verifier written under a weaker cost at
// this hasher's current one, for a sign-in that has just proved the password —
// in the BACKGROUND, owned by this surface, once the answer is on its way.
//
// # The only instant a cost raise can be carried out
//
// The plaintext is not stored, so a stronger digest can be computed only when
// somebody presents their password — which is why the parameters ride in the
// stored verifier and [credential.Hasher.Verify] reports one that is stale. It
// reported it to nobody: every sign-in discarded the flag, so raising the cost
// changed new passwords and left every existing verifier at the old one for
// the life of the deployment.
//
// STALE IS WEAKER and never merely different, which is what makes acting on
// the flag safe in a rolling upgrade: a node still on an older build meets the
// verifiers a newer one wrote at a raised cost, and rewriting those at this
// build's cost would be a downgrade, undone by the next sign-in on an upgraded
// node and redone by the next here. Verify leaves them unreported.
//
// # Never on the request, and never at a sign-in's expense
//
// The person is waiting for a session, not for a stronger digest, so nothing
// they wait on includes it: callers DEFER this, so it starts once the handler
// has answered, whatever it answered. It was inline once, given the time left
// before a refusal deadline the sign-in used to be padded to — and a 64 MiB
// derivation after a 64 MiB verification had all but used that up, so the
// write that followed ran on an expired context, failed, and the verifier was
// never rewritten on any real hardware while every such sign-in paid for a
// second derivation.
//
// SO IT HAS BUDGETS OF ITS OWN, and neither is the request's: the derivation takes a
// slot of the cap only if one is free at once ([credential.Hasher.Rehash]) —
// a rewrite nobody waits on must not queue ahead of the sign-ins behind it —
// and the write has [rehashBudget]. One rewrite per person runs at a time,
// because a person signing in twice while the first is in flight would pay a
// second derivation whose write then finds the verifier already replaced and
// publishes nothing. Anything short of a confirmed write is LOGGED and changes
// nothing: the old verifier still verifies at its own cost, and the next
// sign-in asks again.
//
// # Once per verifier, and never over somebody else's change
//
// Each rewrite is an operation of its own, minted when it writes
// ([errVerifierMoved] says why it is not derived from the verifier), and the
// swap is decided inside the write's own snapshot: only the credential that
// was verified, still holding the verifier that was verified, is rewritten — a
// password changed in between keeps its change, and a second rewrite of a
// verifier the first already replaced finds it gone and publishes nothing —
// and it keeps its id, because a re-hash is the same credential at a
// different cost.
func (s *Service) rehashPassword(r *http.Request, person string,
	stale iamdomain.Credential, password string) {

	// WITHOUT CANCEL, because the request is answered and its context about
	// to end — a rewrite that inherited it would do nothing at all — and
	// cancellable by the surface's stop. The WRITE is bounded, by
	// [rehashBudget], once the derivation before it has run: see
	// [Service.rewriteVerifier].
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	if !s.rehashes.start(person, cancel) {
		cancel()
		log.DebugContext(ctx, "api_password_rehash_skipped", "person", person,
			"reason", "a rewrite for this person is in flight, or this "+
				"surface is stopping")
		return
	}
	go func() {
		defer s.rehashes.finish(person)
		s.rewriteVerifier(ctx, person, stale, password)
	}()
}

// errVerifierMoved refuses a rewrite whose credential no longer holds the
// verifier this sign-in verified, in the write's own snapshot — so nothing is
// published.
//
// # One operation per rewrite, and a moved verifier writes nothing
//
// A rewrite's operation is minted NOW ([statelog.NewOpID]), in the grammar the
// ledger vouches for a retry by. It was derived from the person and the stale
// verifier, so that every node rewriting one verifier made one operation — and
// carried no instant, which read as minted at the epoch and was answered
// `unknown` without being published once the ledger had swept anything. So two
// sign-ins that both present a stale verifier are two operations now, and the
// second must not publish a set that changes nothing: its decide meets the
// verifier the first one wrote and refuses, which publishes nothing at all.
var errVerifierMoved = errors.New("authapi: the verifier to rewrite is no " +
	"longer the credential's")

// rehashBudget bounds a background rewrite's write.
//
// THE PUBLISHER'S OWN RESOLVE BUDGET ([statelog.DefaultResolveBudget]): the
// write waits that long for this node's applier and then answers `pending`,
// which is durable, so bounding the whole write at it caps only what the
// publisher does not bound itself — an append the broker never acknowledges —
// without cutting short a write that would have landed. A second number here
// would be a second opinion about how long an identity write takes.
const rehashBudget = statelog.DefaultResolveBudget

// rewriteVerifier is one background rewrite: derive at the current cost, and
// swap the verifier in the write's own snapshot.
func (s *Service) rewriteVerifier(ctx context.Context, person string,
	stale iamdomain.Credential, password string) {

	fresh, err := s.hasher.Rehash(password)
	if err != nil {
		log.InfoContext(ctx, "api_password_rehash_skipped",
			"person", person, "error", err)
		return
	}
	// THE BUDGET STARTS AT THE WRITE, never before the derivation: it is the
	// publisher's own resolve budget, sized for what the write waits on, and
	// an argon2id derivation at the shipped cost ahead of it is a quarter of
	// a second on an idle host and seconds on a loaded one — spent out of the
	// write's budget, the write began on a context already expired and no
	// verifier was rewritten on exactly the host that most needed the time.
	ctx, done := context.WithTimeout(ctx, rehashBudget)
	defer done()
	result, err := s.writer.SetCredentials(ctx, iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			out := slices.Clone(held)
			swapped := false
			for i, c := range out {
				if c.ID == stale.ID && c.Method == iamdomain.MethodPassword &&
					c.Verifier == stale.Verifier {
					out[i].Verifier = fresh
					swapped = true
				}
			}
			if !swapped {
				return nil, errVerifierMoved
			}
			return out, nil
		},
		OpID:   statelog.NewOpID(s.now(), "rehash"),
		Reason: "re-hashed the password at the current cost",
	})
	if errors.Is(err, errVerifierMoved) {
		log.DebugContext(ctx, "api_password_rehash_skipped", "person", person,
			"reason", "the verifier this sign-in presented is no longer the "+
				"one held: another rewrite, or a password change, landed first")
		return
	}
	if err != nil || !landed(result) {
		log.WarnContext(ctx, "api_password_rehash_unrecorded",
			"person", person, "error", errText(err), "op_id", result.OpID,
			"outcome", string(result.Outcome))
		return
	}
	log.InfoContext(ctx, "api_password_rehashed", "person", person,
		"position", result.Position.String())
}

// rehashes is the background rewrites a surface owns: which person each is
// for, how to cancel it, and a way to wait for them all.
//
// ITS LIFETIME IS THE SURFACE'S, and [Service.Stop] ends it — none starts
// after, those in flight are waited for as long as the stop allows and then
// cancelled — because a goroutine nobody can stop would go on writing to a
// broker the engine is tearing down.
type rehashes struct {
	mu       sync.Mutex
	stopped  bool
	inFlight map[string]context.CancelFunc
	running  sync.WaitGroup
}

// start claims the one rewrite a person may have in flight, reporting false
// when theirs is already running or the surface is stopping.
func (h *rehashes) start(person string, cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return false
	}
	if _, running := h.inFlight[person]; running {
		return false
	}
	if h.inFlight == nil {
		h.inFlight = map[string]context.CancelFunc{}
	}
	h.inFlight[person] = cancel
	h.running.Add(1)
	return true
}

// finish releases a person's rewrite once it has returned.
func (h *rehashes) finish(person string) {
	h.mu.Lock()
	cancel := h.inFlight[person]
	delete(h.inFlight, person)
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.running.Done()
}

// stop refuses every rewrite from now on, waits for those in flight until ctx
// ends, then cancels whatever is left and waits for it to return.
//
// GRACEFUL AND THEN CUT, which is [http.Server.Shutdown]'s shape and for its
// reason: a rewrite a moment from landing is worth the moment, and one waiting
// on a broker that has stopped answering is not worth the shutdown.
func (h *rehashes) stop(ctx context.Context) {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		h.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	h.mu.Lock()
	for _, cancel := range h.inFlight {
		cancel()
	}
	h.mu.Unlock()
	<-done
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
// HELD RATHER THAN CONFIGURED: what a sign-in asks for is what the PERSON
// holds, so somebody holding a factor is always asked for it, whatever the
// deployment says. A deployment that REQUIRES one does not refuse a person who
// holds none here — they could never sign in to enrol it — but opens them a
// session that may do nothing else ([Service.enrolmentOnly]).
//
// ONE READING WITH THE ENROLMENT'S, which asks the same of the set a decide
// read rather than of a sighting ([holdsFactor]).
func holdsSecondFactor(held iamdomain.Sighting) bool {
	return holdsFactor(held.Credentials)
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
//
// # The app's seed is opened here and nowhere else
//
// It is stored SEALED under the fleet keyring and bound to its person and its
// credential ([iamdomain.Sealer.SealCredential]), and opened only to check a
// code. A seed that does not open is never a match — a value that is not this
// credential's seed (moved, forged, or enrolled in the clear) or one sealed
// under a key this node's ring no longer holds — and it is logged, because it
// is a row somebody has to repair: nothing a retry of the code could change,
// so there is no third answer to give. Opening fetches nothing, which is why
// there is no outage to tell from a wrong code.
func (s *Service) checkSecondFactor(ctx context.Context, held iamdomain.Sighting,
	code string) (factorUse, bool) {

	var app, recovery factorUse
	appOK, recoveryOK := false, false
	if c, ok := firstCredential(held.Credentials, iamdomain.MethodTOTP); ok {
		seed, err := s.sealer.OpenCredential(held.ID, c.ID,
			iamdomain.FieldTOTP, c.Verifier)
		switch {
		case err != nil:
			log.ErrorContext(ctx, "api_totp_seed_unopenable",
				"person", held.ID, "credential", c.ID, "error", err,
				"hint", "the seed on this credential is not one sealed for it "+
					"under a key this node's keyring holds; put the key back "+
					"on the ring, or reset the person's second factor so they "+
					"enrol again")
		default:
			// THE LAST ACCEPTED STEP is what makes a code single-use,
			// and it is read from the credential rather than kept in
			// memory: the node that accepts the next code is rarely
			// the node that accepted the last.
			step, matched := credential.VerifyTOTP(seed, code, s.now(), lastStep(c))
			app = factorUse{factor: types.FactorTOTP, credential: c.ID, step: step}
			appOK = matched
		}
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
// [errFactorSpent] rather than a sign-in, REFUSED in that snapshot so that
// nothing is published: it used to publish the unchanged set, a record of a
// spend that spent nothing. A decide may run again against a fresh snapshot,
// so the verdict is the LAST run's.
//
// A FRESH OPERATION ID PER USE, never one derived from the step: two uses of
// one code under one id would collapse into one record in the operation
// ledger, and the second would read the first's success as its own. Minted in
// the grammar ([statelog.NewOpID]), whose instant the ledger vouches for a
// retry by — `second-factor:<person>:<uuid>` carried none.
//
// THE WRITE'S OWN ANSWER comes back beside the use, because only a spend that
// LANDED AS THIS CALL'S OWN makes the code single-use for this sign-in: an
// unknown one may not be on the log, one that collapsed onto a copy has a
// verdict this call cannot prove is its own, and a session opened on either
// could be a session on a code that still works or on one somebody else
// spent. The caller reads that off the result ([built]).
func (s *Service) spendSecondFactor(ctx context.Context, person string,
	use factorUse) (factorUse, statelog.Result, error) {

	remaining := use.remaining
	opID := statelog.NewOpID(s.now(), "second-factor")
	result, err := s.writer.SetCredentials(ctx, iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			spent := true
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
			if spent {
				// SPENT BY SOMEBODY ELSE between this node's check and
				// this snapshot: refused here, so nothing is published
				// — a record of a set that changed nothing, under an
				// operation that was then answered as though the code
				// were this sign-in's.
				return nil, errFactorSpent
			}
			return out, nil
		},
		OpID:   opID,
		Reason: "spent a " + string(use.factor) + " second factor",
	})
	if result.OpID == "" {
		// A REFUSAL CARRIES NO RESULT, and the answer still names the
		// operation, which is how the trail finds it.
		result.OpID = opID
	}
	if err != nil {
		return factorUse{}, result, err
	}
	// WHETHER A SESSION MAY BE OPENED ON THIS SPEND is the result's to say,
	// and the caller reads it ([built]): an unknown one, or one that landed
	// as a copy this call cannot prove is its own, has a verdict that may be
	// another round's — and a session opened on it could be one opened on a
	// code somebody else spent.
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
//
// # On the person's own curve, before the code is looked at
//
// The attempt's pair — the login as typed, from its address — is one curve,
// and a caller holding the password divides it by every address and spelling
// they have: a /48 of IPv6 is sixty-five thousand fresh pairs, and six digits
// fall to that in about an hour. So a code is ALSO decided on a curve keyed
// on the PERSON the login resolved to ([credential.Throttle.AdmitSecondFactor]),
// before it is checked: every address's guesses at
// one person climb it together, a wait past five seconds is `429` with the
// time left, a wrong code or a code already spent is a failure on it, and the
// code that completes the sign-in lifts it. Keyed on the resolved person here
// and nowhere else, because this is reached only past the password: it tells
// nobody anything about who exists that the password did not. And the wrong
// code that takes the curve to its ceiling is announced
// ([types.IAMSecondFactorThrottled]) — the curve reports that transition once
// per climb, so nothing here keeps a memory of having said it — because it
// means somebody holding this person's password is guessing at their code.
func (s *Service) proveSecondFactor(w http.ResponseWriter, r *http.Request,
	adm admission, attempt authevents.Failure, held iamdomain.Sighting,
	code string) (factorUse, bool) {

	curve, err := s.throttle.AdmitSecondFactor(r.Context(), held.ID)
	switch {
	case errors.Is(err, credential.ErrThrottled):
		throttled := attempt
		throttled.Throttled = true
		s.audit.Failed(r.Context(), throttled)
		httpjson.Throttled(w, credential.RetryAfter(err))
		return factorUse{}, false
	case err != nil:
		abandoned(w, r, adm.source, err)
		return factorUse{}, false
	}
	defer curve.Release()

	use, ok := s.checkSecondFactor(r.Context(), held, code)
	if !ok {
		// STILL THE GENERIC REFUSAL, because a wrong CODE and a wrong
		// password must not be distinguishable to somebody who has
		// stolen one of the two.
		s.refuseSecondFactor(w, r, adm, attempt, held, curve, "second factor mismatch")
		return factorUse{}, false
	}
	use, spend, err := s.spendSecondFactor(r.Context(), held.ID, use)
	switch {
	case errors.Is(err, errFactorSpent):
		s.refuseSecondFactor(w, r, adm, attempt, held, curve,
			"second factor already spent")
		return factorUse{}, false
	case removed(err):
		// THE PERSON WAS REMOVED between the read this was decided on and
		// the spend: nobody is left to sign in as, which is a failed
		// sign-in like every other — THE ONE refusal, counted on the
		// attempt's curve and in the trail's count — and not the person's
		// own curve, since no code was wrong. Rendered by hand it was the
		// one arm a failure count never saw.
		s.refuseSignIn(w, r, adm, attempt, "person removed")
		return factorUse{}, false
	case err != nil:
		// NOT A REFUSAL: the code was right, and this node could not
		// record that it was used. Signing somebody in on a code that
		// stays usable is the replay the spend exists to close, so the
		// honest answer is that this node cannot finish the sign-in now.
		writeFailed(w, r, "api_second_factor_unspent", spend.OpID, err)
		return factorUse{}, false
	case !built(spend):
		// AND NOT A SPEND EITHER: nothing can establish whether it is on
		// the log — or it landed as a copy whose verdict this call cannot
		// prove is its own — which for a code is the same as not having
		// recorded it. A retry presents the code again and is decided
		// afresh — a spend that did land refuses it as spent.
		unresolved(w, r, "api_second_factor_unresolved", spend)
		return factorUse{}, false
	}
	curve.Succeed()
	if use.factor == types.FactorRecovery {
		s.audit.Emit(r.Context(), types.IAMRecoveryCodeUsed{
			Person: held.ID, Login: held.Login, Remaining: use.remaining,
			Remote: attempt.Client,
		})
	}
	return use, true
}

// refuseSecondFactor is a second factor that did not prove itself: a failure
// on the person's curve — announced when it is the one that takes that curve
// to its ceiling ([credential.Ticket.Fail]) — and then the same generic
// refusal every failed sign-in answers.
func (s *Service) refuseSecondFactor(w http.ResponseWriter, r *http.Request,
	adm admission, attempt authevents.Failure, held iamdomain.Sighting,
	curve *credential.Ticket, why string) {

	if curve.Fail() {
		s.audit.Emit(r.Context(), types.IAMSecondFactorThrottled{
			Person: held.ID, Login: held.Login, Remote: attempt.Client,
		})
	}
	s.refuseSignIn(w, r, adm, attempt, why)
}

// signIn is how a completed sign-in was proved, for the event it announces.
type signIn struct {
	method types.SignInMethod
	factor types.SecondFactor

	// stepUp marks a signed-in person confirming who they are again, which
	// announces itself as a step-up rather than as a fresh sign-in; replaces
	// is the session the confirmation was made from, which is ENDED before
	// its replacement opens, and absolute is that session's own deadline,
	// which the replacement keeps — see [Service.StepUp].
	stepUp   bool
	replaces string
	absolute time.Time

	// provedAt is when this sign-in's person proved who they are, where
	// that was NOT here and now — and nil for every way in that was: a
	// password, a second factor, an invitation.
	//
	// ONE WAY IN SETS IT: an ENROLMENT's replacement session inherits the
	// proof of the enrolment-only session it replaces, because the code
	// the enrolment checked proves possession of a seed that same session
	// was handed moments earlier — which says nothing about who is
	// holding it. Dated now, it restarted the step-up window and handed
	// whoever held the restricted cookie a fresh one its password never
	// earned. A POINTER, so "proved nothing datable" (a zero time)
	// and "proved here" (nil) stay two answers.
	provedAt *time.Time

	// because is what the replaced session's close records as its reason,
	// and empty for a step-up's own ("replaced by a step-up").
	because string
}

// enrolmentOnly reports whether a sign-in opens a session that may do nothing
// but enrol a second factor.
//
// # A sign-in that proved a password and nothing else, where one is required
//
// `api.auth.local.totp: required` says nobody signs in on a password alone. So
// a sign-in THIS SURFACE verified — the password route, a password step-up, an
// invitation's redemption — that proved no second factor opens
// a restricted session. Proving none means holding none: the password route
// and the step-up demand a code from anybody who holds a factor, and a new
// person holds none yet.
//
// A Tier A exchange never reaches this: presenting the token was its whole
// proof, and it holds no second factor to enrol.
func (s *Service) enrolmentOnly(how signIn) bool {
	switch how.method {
	case types.SignInPassword, types.SignInInvite:
		return how.factor == "" && s.secondFactorRequired()
	}
	return false
}

// secondFactorRequired reports whether this deployment requires a second factor
// of somebody who signs in with a password: the `api.auth.local` block's own
// `totp`, and nothing where there is no such block.
//
// THE BLOCK IS WHAT STATES what a password sign-in needs, and
// [iam.SecondFactor.Requires] reads a value this build does not know as
// required, which is the safe direction for "must you prove more".
func (s *Service) secondFactorRequired() bool {
	local := s.boot.API.Auth.Local
	return local != nil && local.TOTP.Requires()
}

// proofOf is the instant a sign-in proved who somebody is: the replaced
// session's for an enrolment's replacement, and this one for everything this
// surface verified itself — see [signIn.provedAt].
func (s *Service) proofOf(how signIn) time.Time {
	if how.provedAt != nil {
		return *how.provedAt
	}
	return s.now()
}

// sessionOpID is the operation a session's START is published under: a step
// of the lineage it opens.
//
// THE LINEAGE IS A UUID7 THIS REQUEST MINTED, so the id carries the instant
// the session began, in the grammar the ledger vouches for a retry by
// ([statelog.OpMintedAt]). It was `session:<lineage>`, which carries none and
// reads as minted at the epoch — and a session subject's ledger rows go after
// an hour ([iamdomain.SessionOpsRetention]), so within an hour of the first
// sweep every sign-in was answered `unknown` without being published.
func sessionOpID(lineage uuid.UUID) string {
	return statelog.StepOpID(lineage.String(), "session")
}

// personRemoved answers a sign-in whose person was removed from the directory
// between the read it was decided on and its record ([removed]) — which
// nothing will ever write again, so it is not a 503 anybody retries.
//
// THE REFUSAL THE WAY IN ALREADY HAS: a session being replaced is one that
// has ended, and a fresh sign-in is the generic refusal every failed one
// answers, because a removed person cannot sign in and the caller holds
// nothing that says otherwise.
func (s *Service) personRemoved(w http.ResponseWriter, r *http.Request, how signIn) {
	log.InfoContext(r.Context(), "api_sign_in_person_removed",
		"replaces", how.replaces)
	if how.replaces != "" {
		s.clearSession(w)
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSessionRevoked)
		return
	}
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
}

// completeSignIn opens the session, sets the cookie and answers.
func (s *Service) completeSignIn(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting, how signIn) {

	answer, ok := s.openSignIn(w, r, held, how)
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// openSignIn opens the session, sets its cookie and announces it, answering
// what a JSON caller is told — or false once it has written a refusal.
//
// SEPARATE FROM THE ANSWER because one caller answers in a shape of its own: a
// second factor's enrolment that replaces an enrolment-only session reports
// the enrolment, with the session it opened beside it.
func (s *Service) openSignIn(w http.ResponseWriter, r *http.Request,
	held iamdomain.Sighting, how signIn) (loginResponse, bool) {

	lineage, err := uuid.NewV7()
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_lineage_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return loginResponse{}, false
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
		because := how.because
		if because == "" {
			because = "replaced by a step-up"
		}
		//
		// A STEP OF THE GESTURE THE NEW LINEAGE NAMES, so it carries the
		// instant this request minted that lineage at. It was derived from
		// the REPLACED lineage, whose instant is when that session began —
		// days ago for a week-long session, while a session subject's
		// ledger rows go after an hour ([iamdomain.SessionOpsRetention]):
		// such an id is one the ledger cannot vouch for, and its close was
		// answered `unknown` without being published, so no step-up of a
		// session older than an hour could land. Nothing needs the retry of
		// a step-up to be the same close: closing a closed session changes
		// nothing, and a step-up asked again is a new gesture.
		closeOp := statelog.StepOpID(lineage.String(), "close-replaced")
		closed, closeErr := s.writer.CloseSession(r.Context(), how.replaces,
			held.ID, because, closeOp)
		if closeErr != nil {
			if removed(closeErr) {
				s.personRemoved(w, r, how)
				return loginResponse{}, false
			}
			writeFailed(w, r, "api_step_up_close_failed", closeOp, closeErr)
			return loginResponse{}, false
		}
		if !landed(closed) {
			// THE SAME GESTURE FAILING, for the same reason: opening the
			// replacement beside a close nothing can confirm is the
			// second live session this order exists to prevent.
			unresolved(w, r, "api_step_up_close_unresolved", closed)
			return loginResponse{}, false
		}
	}
	restricted := s.enrolmentOnly(how)
	opened, err := s.writer.OpenSession(r.Context(), iamdomain.SessionStart{
		Lineage: lineage.String(), Person: held.ID,
		AbsoluteExpiresAt: expires,
		// EVERY PATH HERE IS A PROOF — a password and its second factor,
		// an invitation, a step-up — so the session
		// is fresh from the instant it was proved, and a step-up surface
		// asks again once this node's window has passed. It is the one
		// field that says so: without it every session was stale from
		// its first request. An enrolment's replacement keeps the proof
		// of the session it replaces — see [Service.proofOf].
		ProvedAt: s.proofOf(how),
		// A PASSWORD ALONE WHERE A SECOND FACTOR IS REQUIRED opens a
		// session that may only enrol one — see [Service.enrolmentOnly].
		EnrolmentOnly: restricted,
		OpID:          sessionOpID(lineage),
		// THE ONE WRITE IN THIS ESTATE THAT DOES NOT WAIT, because
		// nothing in this answer reads the row: the bearer carries the
		// position and every node validates against its own applier.
		NoWait: true,
	})
	if err != nil {
		if removed(err) {
			s.personRemoved(w, r, how)
			return loginResponse{}, false
		}
		// A COLLAPSED START ([iamdomain.ErrCollapsed]) arrives here too:
		// the epoch and the generation a bearer carries are the decide's,
		// and nothing can prove they are the landed record's — so no
		// bearer, and signing in again opens a session of its own.
		writeFailed(w, r, "api_sign_in_session_failed", sessionOpID(lineage), err)
		return loginResponse{}, false
	}
	if !landed(opened.Result) {
		// NO BEARER FROM AN UNRESOLVED START. It would carry position
		// zero, which every node that has applied anything reads as a
		// session that ended — a cookie that signs its holder out on
		// their first request, beside an event saying they signed in.
		unresolved(w, r, "api_sign_in_session_unresolved", opened.Result)
		return loginResponse{}, false
	}
	at := opened.Result.Position

	// THE EPOCH AND THE GENERATION THE SESSION WAS OPENED AT, as the
	// domain read them in the snapshot it formed the record in. A bearer
	// carrying zero for either is one the first revocation or the first
	// fleet-wide invalidation ends — and every sign-in after it too.
	bearer, err := s.signer.Mint(session.Mint{
		Lineage: lineage, Person: held.ID,
		Epoch: opened.Epoch, Generation: opened.Generation,
		StartPosition:     uint64(at.Packed()),
		AbsoluteExpiresAt: expires,
		// AND THE BEARER CARRIES THE RESTRICTION TOO, signed: the record
		// above did not wait, so no node has the row yet — this one
		// included — and a node serves reads on the bearer alone until it
		// does. See [session.Bearer.EnrolmentOnly].
		EnrolmentOnly: restricted,
	})
	if err != nil {
		log.ErrorContext(r.Context(), "api_sign_in_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return loginResponse{}, false
	}
	http.SetCookie(w, session.Cookie(s.boot.API.ExternalBase(), bearer, expires))
	log.InfoContext(r.Context(), "api_sign_in",
		"person", held.ID, "login", held.Login, "seat", held.Seat,
		"position", at.String(), "enrolment_only", restricted)
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
			SecondFactor: how.factor, ExpiresAt: expires,
		})
	}
	status := statusSignedIn
	if restricted {
		status = statusEnrolmentRequired
	}
	return loginResponse{
		Person: held.ID, Login: held.Login, Seat: held.Seat,
		ExpiresAt: expires, Position: at.String(), Status: status,
	}, true
}

// backend is how this deployment signs people in.
func (s *Service) backend() config.AuthBackend {
	return s.boot.API.Auth.Resolved()
}

// passwordFloor is the shortest password this deployment accepts: its own
// `api.auth.local.min_password_length`, or the engine's twelve.
//
// ONE READING FOR EVERY SITE THAT SETS OR DESCRIBES A PASSWORD — the two
// enrolments that choose one and the two answers that tell a form what to
// refuse before it posts — so a form can never be told one number while the
// route enforces another. That was the shape before this: every site said
// twelve, and none read the setting.
func (s *Service) passwordFloor() int { return s.boot.API.Auth.Local.Passwords() }

// sourceOf is the source the throttle keys on, beside what was typed.
//
// THE CLIENT, RESOLVED THROUGH THE TRUSTED PROXIES — the half of the key that
// says where an attempt came from, so a run at one account from one address is
// slowed without the person it is aimed at, signing in from somewhere else,
// sharing its curve.
//
// THROUGH THE GUARD, because resolving it is the one place `api.trusted_proxies`
// is read: keyed on a proxy's own address, every caller's attempts at one login
// would share one curve, so a stranger guessing at somebody's login would slow
// that person's own sign-in; keyed on a header anybody may send, the stranger
// would pick their own curve. See internal/api/auth/client.go.
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
