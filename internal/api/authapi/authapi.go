// Package authapi is how a person becomes a principal.
//
// # What is here, and what is deliberately not
//
// Every route here is a step in the same sequence: somebody proves who they
// are and leaves holding a session cookie. What
// decides whether that session may DO anything is internal/authz, reached
// through the grant on each route it guards — this package establishes
// identity and never authority.
//
// The machinery it drives belongs to other packages and none of it is
// reimplemented here: internal/iam/credential hashes and verifies, throttles
// and pads; internal/iam/session mints and validates the bearer;
// internal/iamdomain writes the records and reads the estate. What this package owns is the HTTP
// shape of the sequence and the refusals.
//
// # The rule that shapes every refusal on this surface
//
// A SIGN-IN SURFACE MUST NOT BE A ROSTER. Every failed sign-in answers one
// code, at one wall-clock deadline measured from admission, whatever actually
// went wrong — no such login, wrong password, wrong second factor, a person
// suspended, a person removed. A caller that could tell those apart has a list
// of who works here and a way to test it.
//
// Both halves are needed and neither works alone: a distinguishing code makes
// the timing pad pointless, and a distinguishing delay makes the single code
// pointless. internal/iam/credential owns the timing half — a delay curve
// decided BEFORE the subject resolves, on the subject as it was TYPED from the
// source it came from, a fixed-cost decoy on the miss, both arms padded to one
// deadline — and this package owns the shape half.
//
// The exceptions are named rather than assumed, and each discloses nothing a
// stranger did not already have: a throttle refusal names only the caller's
// own recent failures from where they are, on what they typed — a name nobody
// holds climbs the curve exactly as a real one does — so it tells somebody
// they are rate-limited, which they knew; a second-factor
// prompt is reached only by somebody who already passed the first; an
// invitation's refusal is read by somebody holding the link.
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
// # A required second factor is enrolled before anything else
//
// `api.auth.local.totp: required` says nobody acts on a password alone, and a
// person who holds no second factor has nothing else to present. So a sign-in
// this surface verified that proved a password and no second factor opens a
// session marked ENROLMENT-ONLY on its own start record, and answers
// `status: second_factor_enrolment_required`; internal/api/auth refuses that
// session every route but the four that let its person in, and enrolling an
// authenticator through it REPLACES it with a whole session, as a step-up
// does. The mark is the session's because what restricts it is what its
// sign-in proved — see [Service.enrolmentOnly].
//
// # Three outcomes stay three, on every write
//
// A spend whose outcome nobody can establish is the same 503 as one that could
// not be recorded, and so is every other write this surface makes: a session
// start, a close, a revocation, a factor stored, an enrolment. [landed] is the
// one test — applied or pending — and [unresolved] the one answer, carrying
// the operation id. Nothing is built on an unknown write and nothing is
// announced about it, because a row saying a session started, ended or a
// factor was enrolled is the one row in the trail that must not be false. A
// write that landed as a copy this call cannot prove is its own
// ([statelog.Result.Collapsed]) is not built on either, where what it would
// build is formed inside its decide ([built]).
//
// # Every operation id is one the ledger can vouch for
//
// The publisher vouches for a retry by the instant an operation id carries,
// and a session subject's ledger rows go after an hour
// ([iamdomain.SessionOpsRetention]). So every write here is published under an
// id in the engine's grammar: a fresh one per request ([statelog.NewOpID]),
// or a step of the lineage this request minted, for everything answered inside
// the request and never re-asked — and one DERIVED from the invitation, at its
// instant, for the redemption a retry must reproduce. None is derived from a
// session's lineage, whose instant is when that session began.
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
// is a deployment nobody can enter: the posture read, the login itself and
// the invitation pair. Each that
// touches the store is admitted by the throttle first, and each that
// changes state is origin-checked like every other write (internal/api/auth's
// CSRF gate) — the guard is what they are exempt from, not the cross-site
// rule, and that gate exempting them too is what left login CSRF open.
package authapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
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

	// Enrol creates a person. Reached by redeeming an invitation, and by
	// nothing else here: the first person is invited like everybody
	// after them, by an administrator or a Tier A token through /iam.
	Enrol(ctx context.Context, in iamdomain.Enrolment) (statelog.Result, error)

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
// bearer from it, says no event about it, and answers 503 with the identity
// Retry-After — an unknown outcome is the one a retry is FOR, so it carries
// one — and the OPERATION ID, which is how the write is found in `iam_history`
// and, where the gesture derives its id from what the caller presented (an
// invitation), the id the retry lands under by construction. Every other write
// here is a fresh operation per request, because each is answered inside it
// and never re-asked: a sign-in, a sign-out or a factor asked again is a new
// gesture, whose own decide reads what the first one left.
//
// # Unless this node's ledger cannot vouch for it
//
// An UNVOUCHED unknown ([statelog.Result.Unvouched]) was not published at all:
// this node's operation ledger may have lost the row the operation needs, so
// it answers the same operation the same way every time until the write
// reaches it. Where the operation is derived from what the caller presented —
// an invitation's redemption is — the same request here re-derives it and
// meets the same silence, so the answer carries NO Retry-After, says
// `unvouched`, and sends the caller to another node.
//
// # And a COLLAPSED answer is not built on either
//
// A write the framework answered with a copy of the operation it cannot prove
// is this call's own ([statelog.Result.Collapsed]) landed — but whatever this
// call's decide computed may describe a decision nothing published, or none
// at all: a second factor's spend whose verdict was another round's, a factor
// or a set of recovery codes this call formed and nothing stored. Where the
// answer is formed inside the decide, the route asks [built] rather than
// [landed], and a collapsed write is answered here as the unknown it is to
// this call — nothing shown, nothing opened, nothing announced — while a new
// request is a new operation that forms its own.
func unresolved(w http.ResponseWriter, r *http.Request, event string,
	result statelog.Result) {

	log.WarnContext(r.Context(), event, "op_id", result.OpID,
		"outcome", string(result.Outcome), "collapsed", result.Collapsed,
		"unvouched", result.Unvouched)
	detail := httpjson.Detail{
		"detail": "this node cannot establish whether that change landed; " +
			"nothing was built on it — try again",
		"op_id": result.OpID,
	}
	retry := auth.RetryIdentity(nil)
	switch {
	case result.Unvouched:
		retry = 0
		detail["detail"] = "this node's operation ledger cannot vouch for that " +
			"change, so this node answers it the same way every time; nothing " +
			"was built on it — try again on another node"
		detail["unvouched"] = true
	case result.Collapsed:
		detail["detail"] = collapsedDetail
	}
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable, retry, detail)
}

// built reports whether a write landed AS THIS CALL'S OWN, so that what its
// decide computed describes the record that landed: [landed], and not
// [statelog.Result.Collapsed] — see [unresolved].
func built(result statelog.Result) bool {
	return landed(result) && !result.Collapsed
}

// writeFailed answers a write the identity estate REFUSED or could not make:
// 503 carrying the operation it was published under — so the trail can find
// it — and the Retry-After the refusal's own rule gives ([auth.RetryIdentity]),
// none for one waiting cannot clear.
//
// THE OPERATION ID ON EVERY ONE, because the refusals here used to carry
// nothing but the code, and a refused write is the one an operator reading
// the identity trail most needs to find.
func writeFailed(w http.ResponseWriter, r *http.Request, event, opID string,
	err error) {

	var refused *statelog.Unavailable
	if errors.As(err, &refused) && refused.OpID != "" {
		opID = refused.OpID
	}
	log.WarnContext(r.Context(), event, "op_id", opID, "error", err)
	detail := "this node could not make that change, and nothing was built on it"
	if errors.Is(err, iamdomain.ErrCollapsed) {
		// THE ONE REFUSAL HERE THAT IS NOT "COULD NOT": the operation
		// landed, as a copy whose answer this call cannot give — a session
		// start's counters, a mint's grants — which [unresolved] words the
		// same way for a copy found by a write that returns no error.
		detail = collapsedDetail
	}
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable, auth.RetryIdentity(err),
		httpjson.Detail{"detail": detail, "op_id": opID})
}

// collapsedDetail is what a write that landed as a copy this call cannot prove
// is its own ([statelog.Result.Collapsed]) is answered with: it did land, so
// "cannot establish whether it landed" would send a client to find out what it
// could have been told, and what is true is that nothing this call formed
// describes it.
const collapsedDetail = "that change landed as a copy this request cannot " +
	"prove is its own, so nothing was built on it — try again, which starts " +
	"it afresh"

// removed reports a write refused because the person it names was removed
// from the directory ([statelog.ReasonDeleted]) — between the read this
// surface decided on and the record. Nothing will ever write them again, so no
// route here answers it as a 503 somebody retries.
func removed(err error) bool {
	var refused *statelog.Unavailable
	return errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted
}

// Sealer is the sealing this surface does, and nothing else of the estate's.
//
// CONSUMER-DEFINED AND THREE METHODS, like every other seam here: it opens an
// invitation's address, and seals and opens a second factor's seed — the one
// credential this estate keeps as a secret rather than a verifier. Nothing
// here opens a person's name or address, so a sign-in route can reach only
// the values it was handed.
//
// NO CONTEXT, because nothing is fetched: [iamdomain.Sealer], which a running
// node hands in, seals under the fleet keyring this process already holds.
type Sealer interface {
	// OpenInvitation opens the address an invitation was issued to, bound
	// to that invitation ([iamdomain.Sealer.OpenInvitation]).
	OpenInvitation(invitationID, sealed string) (string, error)

	// SealCredential and OpenCredential seal and open a credential secret
	// under the keyring, bound to its person AND to the credential it
	// belongs to — see [iamdomain.Sealer.SealCredential] for why both
	// halves.
	SealCredential(person, credential string, field iamdomain.Field,
		plaintext string) (string, error)
	OpenCredential(person, credential string, field iamdomain.Field,
		sealed string) (string, error)
}

// Blinds is where the keyed blind an address is matched on comes from.
//
// THE BLIND AND NEVER THE ADDRESS, because a sign-in runs before anybody is
// authenticated and the lookup must not carry personal data. It must agree
// with what the chart derives its own index from, or one address would reach a
// seat and a different person.
//
// RESOLVED PER REQUEST rather than held from construction, for
// [iamdomain.Blinds]' reason: the company's key is minted by the first node
// that needs it, after this surface was built.
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
//
// EmitOnce is the first door for a fact worth one row per window rather than
// one per occurrence — a person's second factor at its ceiling, which every
// further wrong code would otherwise announce again — decided by the node's
// one dedupe rather than a second copy of it here.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
	EmitOnce(ctx context.Context, class authevents.OnceClass, key string,
		window time.Duration, payload events.Payload) bool
	Failed(ctx context.Context, f authevents.Failure)
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
	// unused on `none`, but taken unconditionally: a nil here would
	// make the local backend's refusal a panic on the first sign-in
	// rather than a refusal at boot.
	Hasher *credential.Hasher

	// Throttle is the sign-in delay curve with the two timing defences
	// around it. REQUIRED — without it every refusal on this surface is an
	// oracle with a stopwatch, and a password is guessed at line rate.
	Throttle *credential.Throttle

	// Blinder is where the address blinds come from. REQUIRED.
	Blinder Blinds

	// Sealer seals and opens the values this surface handles. REQUIRED.
	//
	// AN INVITATION'S ADDRESS is one: a form has to show which address a
	// link was sent to, or the person guesses which of theirs it was. It
	// is sealed under the INVITATION's key rather than a person's, because
	// there is no person yet — minting one for an invitation that may
	// never be redeemed would leave a key behind for every address anybody
	// ever typed.
	//
	// A SECOND FACTOR'S SEED is the other, sealed when it is enrolled and
	// opened only to check a code: it is a secret the replicated estate
	// would otherwise hold in the clear on every node, in every snapshot
	// and every backup.
	Sealer Sealer

	// Sessions is what one bearer is validated against. REQUIRED.
	//
	// A SECOND SEAM OVER THE SAME READER, and narrower than [Directory]
	// on purpose: it carries one method and no lookup by login, so a
	// validation path cannot reach the half a sign-in uses, and a
	// sign-in path cannot resolve an arbitrary bearer.
	//
	// THE ESTATE'S ROWS AND NOTHING ELSE: [New] reads it through
	// [auth.SessionSubjects] over this surface's own Tier A, so a session
	// exchanged from a Tier A token is judged here exactly as the guard
	// judges it — see [Service.directoryFor].
	Sessions session.Directory

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
	sealer    Sealer
	clients   *auth.Clients
	audit     Audit
	now       func() time.Time

	// rehashes are the password rewrites this surface runs after a
	// sign-in has answered — see [Service.rehashPassword] — and what
	// [Service.Stop] ends.
	rehashes rehashes
}

// Stop ends the work this surface runs after its answers: no password rewrite
// starts from now on, and those in flight are waited for until ctx ends and
// then cancelled — returning only once every one has.
//
// CALLED ONCE THE LISTENER HAS STOPPED, so no request can start another, and
// before the engine the writes go to is torn down. Safe to call more than
// once.
func (s *Service) Stop(ctx context.Context) { s.rehashes.stop(ctx) }

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
		{"Sealer", opts.Sealer == nil},
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
		blinder: opts.Blinder,
		// THE GUARD'S READING OF A BEARER'S SUBJECT, built from the same
		// Tier A — see [Service.directoryFor] for what the bare estate
		// cost a token's own sign-out.
		sessions: auth.SessionSubjects(opts.Bootstrap, opts.Sessions),
		sealer:   opts.Sealer,
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
// whoever can reach this listener, so it goes to the throttle's curve, the
// audit trail's counter and its per-client, per-minute tally — the engine's
// own loop decides when a row is written, and the row carries counts rather
// than what was typed.
//
// IT PADS BEFORE IT ANSWERS. The pad is measured from when the request was
// ADMITTED rather than from here, so a slow arm and a fast one leave at the
// same instant — which is the only shape in which a stopwatch learns nothing.
func (s *Service) refuseSignIn(w http.ResponseWriter, r *http.Request,
	in admission, attempt authevents.Failure, why string) {

	in.ticket.Fail()
	s.audit.Failed(r.Context(), attempt)
	log.WarnContext(r.Context(), "api_sign_in_refused",
		// THE ARM, for the log only. Never the login, never the
		// presented value, and never anything that would let a log
		// reader reconstruct who was being guessed at from a feed an
		// operator's screen renders.
		"reason", why, "method", string(attempt.Method), "route", r.URL.Path,
		"source", attempt.Client)
	s.throttle.Pad(r.Context(), in.at)
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
}

// admission is one attempt the throttle let through: its ticket, the instant a
// refusal is padded from, and where it came from. An attempt on no curve —
// [Service.uncounted] — holds a nil ticket, whose methods do nothing.
type admission struct {
	ticket *credential.Ticket
	at     time.Time
	source string
}

// admit runs the throttle's curve for one attempt, answering its [admission]
// or false once it has written the refusal.
//
// BEFORE ANYTHING IS LOOKED UP, on the subject as the caller TYPED it and the
// source it came from: keyed on what it resolved to, the curve would be one
// only real people could climb, and the roster again. A route whose
// credential names nobody takes [Service.uncounted] instead. A wait of up to
// [credential.InlineDelay] is served inside this call; a longer one is
// `429 throttled` naming the time left.
//
// THE CALLER RESOLVES THE TICKET — [credential.Ticket.Fail] through
// [Service.refuseSignIn] or its siblings, [credential.Ticket.Succeed] where
// the credential proved itself — and DEFERS [credential.Ticket.Release], so an
// attempt that reached no verdict counts as nothing.
//
// THE PAD RUNS FROM ADMISSION, not from arrival: the curve's own wait is the
// same for a name that exists and one that does not, and a deadline it had
// already spent would leave the verification after it unpadded.
//
// A THROTTLED REQUEST IS A FAILED ATTEMPT TOO, counted apart as one the curve
// turned away: a client that keeps going after it was stopped is the part of
// a guessing run an operator most wants to see, and the method names which
// door it was pushing on.
func (s *Service) admit(w http.ResponseWriter, r *http.Request,
	attempt credential.Attempt, method types.FailureMethod) (admission, bool) {

	ticket, err := s.throttle.Admit(r.Context(), attempt)
	if err == nil {
		return admission{ticket: ticket, at: s.throttle.Now(),
			source: attempt.Source}, true
	}
	if errors.Is(err, credential.ErrThrottled) {
		s.audit.Failed(r.Context(), authevents.Failure{
			Client: attempt.Source, Method: method, Throttled: true,
		})
		httpjson.Throttled(w, credential.RetryAfter(err))
		return admission{}, false
	}
	abandoned(w, r, attempt.Source, err)
	return admission{}, false
}

// abandoned answers an attempt whose wait ended with its request — for its
// place on the curve, or for its source's turn at the verify cap: the caller
// went away, or this node is stopping. Nothing was attempted and nothing was
// decided, so the answer is for a node that will take the attempt later, and a
// caller that defers [credential.Ticket.Release] counts it as nothing.
func abandoned(w http.ResponseWriter, r *http.Request, source string, err error) {
	log.DebugContext(r.Context(), "api_sign_in_wait_abandoned",
		"error", err, "source", source)
	httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentity(err))
}

// decoy spends the turn a verification would have, for a subject with no
// verifier to check, answering false once it has answered a request that went
// away before its turn came — see [credential.Hasher.Decoy].
func (s *Service) decoy(w http.ResponseWriter, r *http.Request, in admission,
	presented string) bool {

	if err := s.hasher.Decoy(r.Context(), in.source, presented); err != nil {
		abandoned(w, r, in.source, err)
		return false
	}
	return true
}

// hash is a new password's verifier, derived in the turn of the source that
// chose it, or false once it has answered.
func (s *Service) hash(w http.ResponseWriter, r *http.Request, in admission,
	password, route string) (string, bool) {

	verifier, err := s.hasher.Hash(r.Context(), in.source, password)
	switch {
	case err == nil:
		return verifier, true
	case r.Context().Err() != nil:
		abandoned(w, r, in.source, err)
	default:
		log.ErrorContext(r.Context(), "api_"+route+"_hash_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
	return "", false
}

// uncounted is the admission of an attempt whose credential names nobody — an
// invitation link: the instant its refusal is padded from and where it came
// from, and no ticket.
//
// # No curve, and not for want of a key
//
// The only key such an attempt has is its SOURCE, and a curve on the source
// alone is one anybody sharing the address holds shut for everybody else at
// it — an office, a VPN's egress, the whole internet behind a proxy this
// deployment was not told to trust. These routes had one, so one stranger
// could keep every invitation at that address answering 429. What bounds a
// walk is the credential itself — a link's secret is 256 bits of crypto/rand
// — and what shows one is the audit trail's failure tally, which every refusal
// still reaches.
func (s *Service) uncounted(r *http.Request) admission {
	return admission{at: s.throttle.Now(), source: s.sourceOf(r)}
}

// stageAdmits reports whether a person's enrolment stage lets them act.
//
// AN ALLOWLIST OF ONE. A stage this build does not know answers false, where a
// denylist would have admitted it — and the value comes off a row a newer peer
// may have written.
func stageAdmits(stage iam.Stage) bool { return stage == iam.StageActive }
