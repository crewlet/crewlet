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
// prompt is reached only by somebody who already passed the first; and an
// invitation's refusal is read by somebody holding the link.
//
// # A login cannot require a login
//
// Four routes here are unguarded, because requiring a credential to obtain one
// is a deployment nobody can enter: the posture read, the login itself, the
// OIDC pair and the invitation GET. Each is admitted per SOURCE by the
// throttle and each is origin-checked like every other state change — the
// guard is what they are exempt from, not the cross-site rule.
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
	"github.com/crewlet/crewlet/internal/iam"
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

	// SessionOwner is who holds one session, or empty for a lineage this
	// node does not hold. It is what makes ending a NAMED session a check
	// against this node's own rows rather than a claim the caller made.
	SessionOwner(ctx context.Context, lineage string) (string, error)
}

// Writer is what this surface writes, defined here for Directory's reason.
type Writer interface {
	// OpenSession records a session beginning and returns where the record
	// landed. The position travels into the bearer, which is what lets a
	// node below it serve on the signature alone.
	OpenSession(ctx context.Context, in iamdomain.SessionStart) (statelog.Position, error)

	// CloseSession ends one, keeping its row until the sweep collects it.
	CloseSession(ctx context.Context, lineage, reason, opID string) (statelog.Position, error)

	// Revoke bumps a person's revocation epoch, which ends every session
	// they hold.
	Revoke(ctx context.Context, personID, opID, reason string) (statelog.Position, error)

	// Enrol creates a person. Reached by the bootstrap route and by
	// redeeming an invitation, and by nothing else here.
	Enrol(ctx context.Context, in iamdomain.Enrolment) (statelog.Position, error)

	// MintBootstrap publishes the hash of the one-time code this node
	// wrote, and SpendBootstrap records it being used.
	//
	// TWO RECORDS ON ONE SUBJECT, which is what makes two nodes minting
	// or redeeming contend — the enrolment beside a redemption arbitrates
	// on a fresh person id nobody else would name, so two of those would
	// both succeed and a company would have two founders.
	MintBootstrap(ctx context.Context, in iamdomain.BootstrapMint) (statelog.Position, error)
	SpendBootstrap(ctx context.Context, in iamdomain.BootstrapSpend) (statelog.Position, error)

	// SetCredentials replaces a person's credential set, forming the new
	// whole from their own row INSIDE the snapshot — which is what keeps
	// a two-request enrolment from pairing an old decision with a new
	// expectation.
	SetCredentials(ctx context.Context, in iamdomain.CredentialSet) (statelog.Position, error)

	// SpendInvitation records a link being used, naming the person it
	// created. It arbitrates on the ADDRESS BLIND, which is what makes
	// two nodes redeeming one link contend.
	SpendInvitation(ctx context.Context, in iamdomain.InvitationSpend) (statelog.Position, error)
}

// Opener opens one sealed value under the key of whatever it belongs to.
//
// CONSUMER-DEFINED AND ONE METHOD, like every other seam here: this surface
// opens an invitation's address and nothing else, so it takes the ability to
// open one value rather than a sealer that could reach every person's.
type Opener interface {
	Open(ctx context.Context, owner string, field iamdomain.Field, sealed string) (string, error)
}

// Blinder derives the keyed blind an address is matched on.
//
// THE BLIND AND NEVER THE ADDRESS, because a sign-in runs before anybody is
// authenticated and the lookup must not carry personal data. It must agree
// with what the chart derives its own index from, or one address would reach a
// seat and a different person.
type Blinder interface {
	Blind(email string) (string, error)
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

	// Blinder derives the address blind. REQUIRED.
	Blinder Blinder

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

	// Guard is what resolves a caller's own address through this
	// deployment's trusted proxies. REQUIRED.
	//
	// THE GUARD'S AND NOT A SECOND PARSE, because "is this peer the proxy"
	// has to have one answer: keyed on a proxy's address the throttle
	// buckets the whole internet together, and keyed on a header anybody
	// can send it buckets nothing at all. Two readers of
	// `api.trusted_proxies` would drift the day one of them was edited.
	Guard *auth.Guard

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
	blinder   Blinder
	sessions  session.Directory
	opener    Opener
	cipher    secrets.Cipher
	provider  *oidc.Provider
	guard     *auth.Guard
	now       func() time.Time
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
		{"Guard", opts.Guard == nil},
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
		cipher:   opts.Cipher,
		provider: opts.Provider,
		guard:    opts.Guard, now: opts.Now,
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
// IT PADS BEFORE IT ANSWERS. The pad is measured from when the request
// ARRIVED rather than from here, so a slow arm and a fast one leave at the
// same instant — which is the only shape in which a stopwatch learns nothing.
func (s *Service) refuseSignIn(w http.ResponseWriter, r *http.Request,
	arrived time.Time, source, why string) {

	s.throttle.Fail(r.Context(), source)
	log.WarnContext(r.Context(), "api_sign_in_refused",
		// THE ARM, for the log only. Never the login, never the
		// presented value, and never anything that would let a log
		// reader reconstruct who was being guessed at from a feed an
		// operator's screen renders.
		"reason", why, "route", r.URL.Path, "source", source)
	s.throttle.Pad(r.Context(), arrived)
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSignInRefused)
}

// admit runs the per-source throttle, answering false once it has written the
// refusal.
//
// BEFORE ANYTHING IS LOOKED UP, which is what keying on the source rather than
// on the subject buys: a caller cannot learn that a login exists by watching
// which requests get rate-limited.
func (s *Service) admit(w http.ResponseWriter, r *http.Request, source string) bool {
	err := s.throttle.Admit(r.Context(), source)
	if err == nil {
		return true
	}
	if errors.Is(err, credential.ErrThrottled) {
		// THE ONE SPECIFIC REFUSAL HERE, and it is safe because it is
		// keyed on the source: a stranger learns they are rate-limited,
		// which they already knew.
		httpjson.Fail(w, http.StatusTooManyRequests, httpjson.CodeThrottled)
		return false
	}
	log.WarnContext(r.Context(), "api_sign_in_admission_failed",
		"error", err, "source", source)
	httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
	return false
}

// stageAdmits reports whether a person's enrolment stage lets them act.
//
// AN ALLOWLIST OF ONE. A stage this build does not know answers false, where a
// denylist would have admitted it — and the value comes off a row a newer peer
// may have written.
func stageAdmits(stage iam.Stage) bool { return stage == iam.StageActive }
