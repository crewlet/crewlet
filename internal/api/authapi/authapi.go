// Package authapi is how a person becomes a principal.
//
// # What is here, and what is deliberately not
//
// Twelve routes, and every one of them is a step in the same sequence:
// somebody proves who they are and leaves holding a session cookie. What
// decides whether that session may DO anything is internal/authz, reached
// through the grant on each route it guards — this package establishes
// identity and never authority.
//
// The machinery it drives belongs to other packages and none of it is
// reimplemented here: internal/iam/credential hashes and verifies, throttles
// and pads; internal/iam/oidc runs the provider round trip;
// internal/iam/session mints and validates the bearer; internal/iamdomain
// writes the records and reads the estate. What this package owns is the HTTP
// shape of the sequence and the refusals.
//
// # The rule that shapes every refusal on this surface
//
// A SIGN-IN SURFACE MUST NOT BE A ROSTER. Every failed sign-in answers one
// code, at one wall-clock deadline measured from arrival, whatever actually
// went wrong — no such login, wrong password, wrong second factor, a person
// suspended, a person removed. A caller that could tell those apart has a list
// of who works here and a way to test it.
//
// Both halves are needed and neither works alone: a distinguishing code makes
// the timing pad pointless, and a distinguishing delay makes the single code
// pointless. internal/iam/credential owns the timing half — admission per
// source BEFORE the subject resolves, a fixed-cost decoy on the miss, both
// arms padded to one deadline — and this package owns the shape half.
//
// The exceptions are named rather than assumed, and each discloses nothing a
// stranger did not already have: a throttle refusal is keyed on the SOURCE, so
// it tells somebody they are rate-limited, which they knew; a second-factor
// prompt is reached only by somebody who already passed the first; an
// invitation's refusal is read by somebody holding the link; and a STALE
// founder code (`bootstrap_code_stale`) is answered only to somebody presenting
// a code whose digest is on the identity log or in the serving node's own file
// — which is to say, holding a real one.
//
// # A second factor is spent by the sign-in it completes
//
// Either factor the person holds is accepted — an app code, or one of their
// recovery codes when the phone is not to hand — and the one used is SPENT in
// a write to their own credentials, decided in the snapshot that checked it:
// an app code records the step it was accepted at, a recovery code is
// removed. Without the first, the drift tolerance is a ninety-second window in
// which one shoulder-surfed code works repeatedly; without the second, a
// one-time code is a second password. Two sign-ins racing one code are
// arbitrated like any other write to one person, and the loser is refused as a
// wrong code would be. A spend this node cannot RECORD is a 503 rather than a
// session: signing somebody in on a code that stays usable is the replay the
// spend exists to close.
//
// # Three outcomes stay three, on every write
//
// A spend whose outcome nobody can establish is the same 503 as one that could
// not be recorded, and so is every other write this surface makes: a session
// start, a close, a revocation, a factor stored, an enrolment. [landed] is the
// one test — applied or pending — and [unresolved] the one answer, carrying
// the operation id. Nothing is built on an unknown write and nothing is
// announced about it, because a row saying a session started, ended or a
// factor was enrolled is the one row in the trail that must not be false.
//
// # What it says about itself
//
// Every refusal reaches [Audit.Failed] and never [Audit.Emit]: a failed
// attempt's rate is the caller's to choose, so it is a counter and one row per
// client per minute from the engine's own loop (internal/iam/authevents). A
// success is announced as the event it is — the session it opened, the step-up
// it completed, the recovery code it spent — at the site that produced it.
//
// # A login cannot require a login
//
// Some routes here are unguarded, because requiring a credential to obtain one
// is a deployment nobody can enter: the posture read, the login itself, the
// first operator's bootstrap, the OIDC pair and the invitation pair. Each that
// touches the store is admitted per SOURCE by the throttle, and each that
// changes state is origin-checked like every other write (internal/api/auth's
// CSRF gate) — the guard is what they are exempt from, not the cross-site
// rule, and that gate exempting them too is what left login CSRF open.
package authapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("api.auth")

// Directory is what this surface reads about people, defined HERE and kept to
// what signing somebody in needs.
//
// THREE METHODS AND NO LISTING. A consumer-defined seam is the tree's
// convention, and on this surface it is also a boundary: the routes below run
// before anybody is authenticated, so the widest thing they can reach is the
// widest thing an unauthenticated caller can reach. A method that enumerated
// people would make this package one bug away from serving a roster.
type Directory interface {
	// PersonByLogin resolves a login, answering the ZERO value and a nil
	// error for one nobody holds — never a sentinel. See
	// [iamdomain.Reader.PersonByLogin] for why the absence is not an
	// error.
	PersonByLogin(ctx context.Context, login string) (iamdomain.Sighting, error)

	// PersonByEmailBlind resolves a keyed address blind, on the same three
	// answers.
	PersonByEmailBlind(ctx context.Context, blind string) (iamdomain.Sighting, error)

	// PersonBySubjectBlind resolves an identity provider's blinded subject
	// to whoever holds a LIVE link to it, on the same three answers — and
	// an error for a subject two people hold, which resolves neither.
	PersonBySubjectBlind(ctx context.Context, blind string, now time.Time) (
		iamdomain.Sighting, error)

	// AnyPerson reports whether anybody is enrolled at all, which is the
	// bootstrap decision. One bit, never a listing.
	AnyPerson(ctx context.Context) (bool, error)

	// InvitationByID resolves one invitation, on the same three answers:
	// the zero value for one nobody issued, and an error for a node that
	// could not tell.
	//
	// BY ITS OWN ID and never by address, because the id is what the link
	// carries and an address lookup here would be a way to ask whether
	// somebody has been invited.
	InvitationByID(ctx context.Context, id string) (iamdomain.InvitationRow, error)

	// SessionStanding is who holds one session, or empty for a lineage this
	// node does not hold, and whether this node's rows still hold it LIVE.
	// It is what makes ending a NAMED session a check against this node's
	// own rows rather than a claim the caller made — and what keeps a
	// session a record already ended from being ended, and announced,
	// again.
	SessionStanding(ctx context.Context, lineage string, now time.Time) (
		owner string, live bool, err error)

	// BootstrapCode resolves one code by its digest WHATEVER BECAME OF IT
	// — the zero value for one the log holds no row for — which is what
	// lets a founder holding a code that no longer works be told why, and
	// what lets any node redeem a code another node wrote.
	BootstrapCode(ctx context.Context, id string) (iamdomain.BootstrapCode, error)
}

// Writer is what this surface writes, defined here for Directory's reason.
//
// # Every write answers all three outcomes
//
// Each method answers the framework's own [statelog.Result], and never a bare
// position, because the position alone cannot tell `applied` and `pending`
// (both durable) from `unknown` (nothing established): a zero position beside
// a nil error read as success, so a second factor whose spend may not have
// landed opened a session, and every event this surface announces was said of
// writes that may not exist. What this surface does on each is [unresolved]'s
// to say.
type Writer interface {
	// OpenSession records a session beginning and returns the write's
	// answer and the two counters it was opened at. The position and both
	// counters travel into the bearer: the position is what lets a node
	// below it serve on the signature alone, and the epoch and generation
	// are what a later revocation or invalidation is compared against.
	OpenSession(ctx context.Context, in iamdomain.SessionStart) (iamdomain.SessionOpened, error)

	// CloseSession ends one, keeping its row until the sweep collects it.
	// The person is the session's own, and the record is filed under
	// their bucket.
	CloseSession(ctx context.Context, lineage, person, reason,
		opID string) (statelog.Result, error)

	// Revoke bumps a person's revocation epoch, which ends every session
	// they hold.
	Revoke(ctx context.Context, personID, opID, reason string) (statelog.Result, error)

	// Enrol creates a person. Reached by the bootstrap route and by
	// redeeming an invitation, and by nothing else here.
	Enrol(ctx context.Context, in iamdomain.Enrolment) (statelog.Result, error)

	// MintBootstrap publishes the hash of the one-time code this node
	// wrote at boot. Every node's lands: a fleet offers one code per node.
	//
	// There is no spend here. A founding is [Writer.Enrol] naming the code,
	// which TAKES it on the company's one bootstrap subject before it
	// claims anything — that take, not a spend published after the
	// person, is what makes exactly one founder land.
	MintBootstrap(ctx context.Context, in iamdomain.BootstrapMint) (statelog.Result, error)

	// ReissueBootstrap withdraws every outstanding code, releases an
	// unfinished founding, and mints the given one — decided in the mint's
	// own snapshot, so exactly one code is live where it lands.
	ReissueBootstrap(ctx context.Context, in iamdomain.BootstrapMint) (
		statelog.Result, error)

	// SetCredentials replaces a person's credential set, forming the new
	// whole from their own row INSIDE the snapshot — which is what keeps
	// a two-request enrolment from pairing an old decision with a new
	// expectation.
	SetCredentials(ctx context.Context, in iamdomain.CredentialSet) (statelog.Result, error)

	// SpendInvitation records a link being used, naming the person it
	// created. It arbitrates on the ADDRESS BLIND, which is what makes
	// two nodes redeeming one link contend.
	SpendInvitation(ctx context.Context, in iamdomain.InvitationSpend) (statelog.Result, error)
}

// landed reports whether a write's record is durable: applied here, or
// pending here and applied everywhere in time. The one outcome it refuses is
// `unknown`, of which nothing can be said — so nothing is built on it and
// nothing is announced about it.
func landed(result statelog.Result) bool {
	return result.Outcome == statelog.OutcomeApplied ||
		result.Outcome == statelog.OutcomePending
}

// ErrUnresolved reports a write this surface made whose outcome could not be
// established, to a caller that is not a request — the boot path that mints
// the first bootstrap code, and `POST /iam/bootstrap-code`, which re-issues
// one. It is statelog's own ErrUnavailable underneath, so a caller that asks
// `errors.Is(err, statelog.ErrUnavailable)` answers it as the 503 it is.
var ErrUnresolved = fmt.Errorf("authapi: the write's outcome is unknown: %w",
	statelog.ErrUnavailable)

// errText is an error's message, or empty — so a log line carries the field
// only when there is one.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// unresolved answers a write whose outcome this node could not establish.
//
// # 503, the op id, and nothing built on it
//
// The record may or may not be on the log, so this surface does exactly what
// a single write does with `unknown`: it opens no session on it, mints no
// bearer from it, says no event about it, and answers 503 with the Retry-After
// every 503 here carries — and the OPERATION ID, which is how the write is
// found in `iam_history` and, where the gesture derives its id from what the
// caller presented (a logout of one lineage, an invitation, the bootstrap
// code), the id the retry lands under by construction.
func unresolved(w http.ResponseWriter, r *http.Request, event string,
	result statelog.Result) {

	log.WarnContext(r.Context(), event, "op_id", result.OpID,
		"outcome", string(result.Outcome))
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds,
		httpjson.Detail{
			"detail": "this node cannot establish whether that change landed; " +
				"nothing was built on it — try again",
			"op_id": result.OpID,
		})
}

// Opener opens one sealed value under the key of whatever it belongs to.
//
// CONSUMER-DEFINED AND ONE METHOD, like every other seam here: this surface
// opens an invitation's address and nothing else, so it takes the ability to
// open one value rather than a sealer that could reach every person's.
type Opener interface {
	Open(ctx context.Context, owner string, field iamdomain.Field, sealed string) (string, error)
}

// Blinds is where the keyed blind an address or a provider subject is matched
// on comes from.
//
// THE BLIND AND NEVER THE ADDRESS, because a sign-in runs before anybody is
// authenticated and the lookup must not carry personal data. It must agree
// with what the chart derives its own index from, or one address would reach a
// seat and a different person.
//
// RESOLVED PER REQUEST rather than held from construction, for
// [iamdomain.Blinds]' reason: the company's key is minted by the first node
// that needs it, after this surface was built. The blinder that comes back
// keeps the two classes apart in its own signature — [iamdomain.Blinder.Email]
// and [iamdomain.Blinder.Subject] — so no caller can blind an address under
// the subject class and read the miss as nobody holding it.
type Blinds interface {
	Blinder(ctx context.Context) (*iamdomain.Blinder, error)
}

// Audit is where this surface's authentication facts go.
//
// TWO METHODS AND TWO DOORS, and the split is the admission rule: a fact a
// verified credential authored — somebody signed in, signed out, enrolled a
// factor — is published as it happens, and a FAILED attempt is only ever
// COUNTED, because whoever failed decides how many of those there are. See
// internal/iam/authevents, whose Trail is what a running node hands in.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
	Failed(ctx context.Context, f authevents.Failure)
}

// Custody keeps an OIDC session's refresh token for the deactivation probe.
//
// CONSUMER-DEFINED and one method wide: internal/iamdomain's Refreshes
// satisfies it, and the only thing this surface ever does with a refresh
// token is hand it over — nothing on the request path reads one back.
type Custody interface {
	Hold(ctx context.Context, grant iamdomain.RefreshGrant, now time.Time) error
}

// Options is what the surface is built from.
type Options struct {
	// Bootstrap is Tier A. REQUIRED: the cookie's name and Secure flag,
	// the session deadlines, the backend and the grant ceiling all come
	// from it, and a surface that guessed any of them would mint cookies
	// the next node rejects.
	Bootstrap *config.Bootstrap

	// Directory and Writer are the identity estate's two halves.
	// REQUIRED.
	Directory Directory
	Writer    Writer

	// Signer mints and validates bearers. REQUIRED — a deployment whose
	// keyring cannot sign for the fleet is refused at construction by
	// internal/iam/session rather than here.
	Signer *session.Signer

	// Hasher verifies passwords. REQUIRED on the `local` backend and
	// unused on the others, but taken unconditionally: a nil here would
	// make the local backend's refusal a panic on the first sign-in
	// rather than a refusal at boot.
	Hasher *credential.Hasher

	// Throttle is the fleet's failed-attempt window with the two timing
	// defences around it. REQUIRED — without it every refusal on this
	// surface is an oracle with a stopwatch.
	Throttle *credential.Throttle

	// Blinder is where the address and subject blinds come from. REQUIRED.
	Blinder Blinds

	// Cipher seals the OIDC login-in-progress. REQUIRED when a provider
	// is configured and unused otherwise, but taken unconditionally: a
	// nil here would make the start route panic on the first login
	// rather than refuse at boot.
	//
	// THE FLEET'S KEYRING, which is what lets a login begun on one
	// ingress node be finished on another — a flight sealed under a
	// per-node key is a login that fails whenever the callback lands
	// somewhere else.
	Cipher secrets.Cipher

	// Opener opens a sealed value under its own key. REQUIRED.
	//
	// AN INVITATION'S ADDRESS is what this surface opens and the only
	// thing: a form has to show which address a link was sent to, or the
	// person guesses which of theirs it was. It is sealed under the
	// INVITATION's key rather than a person's, because there is no person
	// yet — minting one for an invitation that may never be redeemed
	// would leave a key behind for every address anybody ever typed.
	Opener Opener

	// Sessions is what one bearer is validated against. REQUIRED.
	//
	// A SECOND SEAM OVER THE SAME READER, and narrower than [Directory]
	// on purpose: it carries one method and no lookup by login, so a
	// validation path cannot reach the half a sign-in uses, and a
	// sign-in path cannot resolve an arbitrary bearer.
	Sessions session.Directory

	// Provider is the OIDC provider, or nil on a deployment that has
	// none. Nil is an ORDINARY state: the two OIDC routes are then absent
	// rather than answering an error, which is the honest shape for a
	// company signing in with passwords.
	Provider *oidc.Provider

	// Custody keeps the refresh token a provider sign-in obtains, for the
	// deactivation probe. REQUIRED WHERE A PROVIDER IS: the probe is the
	// only thing that notices somebody disabled at the provider, and a
	// sign-in whose token was dropped is a session no central
	// deactivation ends before its absolute deadline.
	Custody Custody

	// Clients resolves a caller's own address through this deployment's
	// trusted proxies. REQUIRED.
	//
	// ONE PARSER, which is what matters rather than one instance: keyed on
	// a proxy's address the throttle buckets the whole internet together
	// and locks the company out the moment one attacker arrives, and keyed
	// on a header anybody can send it buckets nothing at all. Both this
	// and the guard hold one, built from the same Tier A by the same pure
	// function, so they cannot disagree — what would drift is two readings
	// of `api.trusted_proxies`, and there is one.
	Clients *auth.Clients

	// Audit records what this surface saw. REQUIRED: a sign-in surface
	// that recorded nothing would leave no row saying who signed in, and
	// no count of who failed to.
	Audit Audit

	// Now is the clock.
	Now func() time.Time
}

// Service serves the sign-in surface.
type Service struct {
	boot      *config.Bootstrap
	directory Directory
	writer    Writer
	signer    *session.Signer
	hasher    *credential.Hasher
	throttle  *credential.Throttle
	blinder   Blinds
	sessions  session.Directory
	opener    Opener
	cipher    secrets.Cipher
	clients   *auth.Clients
	provider  *oidc.Provider
	audit     Audit
	custody   Custody
	now       func() time.Time

	// codeMu serialises every change to this node's founder-code FILE —
	// the boot offer, a re-issue and the removal after a redemption — so
	// two re-issues arriving at once cannot each withdraw, each write and
	// each mint, leaving two live codes and a file holding only one.
	codeMu sync.Mutex
}

// New builds the surface, or refuses a missing dependency by name.
//
// IT REFUSES RATHER THAN SERVING A NARROWER SURFACE, for the reason
// internal/api's own constructor gives: every one of these is something the
// engine beside this holds, so a nil is a wiring mistake — and a sign-in route
// that quietly answered 503 because a hasher was missing reads as an outage
// rather than as the misconfiguration it is.
func New(opts Options) (*Service, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Bootstrap", opts.Bootstrap == nil},
		{"Directory", opts.Directory == nil},
		{"Writer", opts.Writer == nil},
		{"Signer", opts.Signer == nil},
		{"Hasher", opts.Hasher == nil},
		{"Throttle", opts.Throttle == nil},
		{"Blinder", opts.Blinder == nil},
		{"Sessions", opts.Sessions == nil},
		{"Opener", opts.Opener == nil},
		// THE CIPHER ONLY WHERE A PROVIDER IS. A deployment signing in
		// with passwords seals no flight, so requiring it would refuse
		// a wiring that is complete.
		{"Cipher", opts.Provider != nil && opts.Cipher == nil},
		// AND CUSTODY, for the same reason and the same condition.
		{"Custody", opts.Provider != nil && opts.Custody == nil},
		{"Clients", opts.Clients == nil},
		{"Audit", opts.Audit == nil},
	} {
		if field.absent {
			missing = append(missing, "Options."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("authapi: %s required: this surface is built "+
			"beside an engine that holds every one of them, so a nil is a "+
			"wiring mistake rather than a posture", joinNames(missing))
	}
	s := &Service{
		boot: opts.Bootstrap, directory: opts.Directory, writer: opts.Writer,
		signer: opts.Signer, hasher: opts.Hasher, throttle: opts.Throttle,
		blinder: opts.Blinder, sessions: opts.Sessions,
		opener:   opts.Opener,
		cipher:   opts.Cipher,
		provider: opts.Provider,
		custody:  opts.Custody,
		clients:  opts.Clients, audit: opts.Audit, now: opts.Now,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// joinNames renders a missing-dependency list.
func joinNames(names []string) string {
	out := names[0]
	for _, name := range names[1:] {
		out += ", " + name
	}
	return out
}

// refuseSignIn is EVERY failed sign-in, whatever went wrong.
//
// ONE FUNCTION so there is one place the code and the status are chosen, and
// no route can be the one that answered differently. The reason is LOGGED and
// never sent: an operator investigating needs to know which arm it was, and
// the caller must not.
//
// THE ATTEMPT IS COUNTED, NEVER PUBLISHED. A failed sign-in is authored by
// whoever can reach this listener, so it goes to the audit trail's counter and
// its per-client, per-minute tally — the engine's own loop decides when a row
// is written, and the row carries counts rather than what was typed.
//
// IT PADS BEFORE IT ANSWERS. The pad is measured from when the request
// ARRIVED rather than from here, so a slow arm and a fast one leave at the
// same instant — which is the only shape in which a stopwatch learns nothing.
func (s *Service) refuseSignIn(w http.ResponseWriter, r *http.Request,
	arrived time.Time, attempt authevents.Failure, why string) {

	s.throttle.Fail(r.Context(), attempt.Client)
	s.audit.Failed(r.Context(), attempt)
	log.WarnContext(r.Context(), "api_sign_in_refused",
		// THE ARM, for the log only. Never the login, never the
		// presented value, and never anything that would let a log
		// reader reconstruct who was being guessed at from a feed an
		// operator's screen renders.
		"reason", why, "method", string(attempt.Method), "route", r.URL.Path,
		"source", attempt.Client)
	s.throttle.Pad(r.Context(), arrived)
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
}

// admit runs the per-source throttle, answering false once it has written the
// refusal.
//
// BEFORE ANYTHING IS LOOKED UP, which is what keying on the source rather than
// on the subject buys: a caller cannot learn that a login exists by watching
// which requests get rate-limited.
//
// A THROTTLED REQUEST IS A FAILED ATTEMPT TOO, counted apart as one the
// ceiling turned away: a client that keeps going after it was stopped is the
// part of a guessing run an operator most wants to see, and the method names
// which door it was pushing on.
func (s *Service) admit(w http.ResponseWriter, r *http.Request, source string,
	method types.FailureMethod) bool {

	err := s.throttle.Admit(r.Context(), source)
	if err == nil {
		return true
	}
	if errors.Is(err, credential.ErrThrottled) {
		s.audit.Failed(r.Context(), authevents.Failure{
			Client: source, Method: method, Throttled: true,
		})
		// THE ONE SPECIFIC REFUSAL HERE, and it is safe because it is
		// keyed on the source: a stranger learns they are rate-limited,
		// which they already knew.
		httpjson.Fail(w, http.StatusTooManyRequests, httpjson.CodeThrottled)
		return false
	}
	log.WarnContext(r.Context(), "api_sign_in_admission_failed",
		"error", err, "source", source)
	httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentitySeconds)
	return false
}

// stageAdmits reports whether a person's enrolment stage lets them act.
//
// AN ALLOWLIST OF ONE. A stage this build does not know answers false, where a
// denylist would have admitted it — and the value comes off a row a newer peer
// may have written.
func stageAdmits(stage iam.Stage) bool { return stage == iam.StageActive }
