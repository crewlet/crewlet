// Package iamapi serves the company's identity directory over HTTP.
//
// # Always guarded, reads included
//
// For the reason /secrets guards its listing: a map of who can reach a
// company and how is worth as much to an attacker as the grants themselves.
// There is no anonymous posture here and no read that skips the guard —
// internal/api/auth's exemption list is the whole of what is unguarded, and
// nothing under /iam is on it.
//
// # Three parties ask, and they are not the same party
//
// The person a row is ABOUT, whoever manages people, and whoever audits.
// internal/authz holds that as two classes rather than a condition here, so a
// REST route and a socket frame cannot answer it differently — and the object
// a route names is a person ID, never a login, because a login is a value
// somebody changes and a self check against a mutable name opens the wrong
// row the day they swap.
//
// # A write waits for this node's own apply
//
// The administrator's next read is almost always against the node they just
// wrote through, so `200` means the rows they are about to read are the rows
// this write produced. A node that has not applied it yet answers `202` with
// the position, which is the honest version of the same promise: the record
// is durable and every node will apply it, and reading at that position is
// what makes it visible. `503` carries the op id, because the only safe retry
// is the SAME one.
//
// # Nothing here opens a sealed value it was not asked to
//
// A name and an address are ciphertext in every row, sealed under the fleet
// keyring. So the listing opens them for the page it returns and nothing more,
// and a value this node's keyring cannot open is reported as SEALED rather
// than as a failure — a state a caller renders, not an outage they retry. A
// removed person has nothing left to open: the removal erased their sealed
// values from every row.
package iamapi

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("api.iam")

// Directory is the read side this surface needs.
//
// CONSUMER-DEFINED and kept to what the routes read: internal/iamdomain's
// reader answers more than this — a sign-in's keyed lookups among them — and
// those are exactly what must not be reachable from a route. Naming each
// method here is what keeps the enumeration-safe half of that package out of
// an HTTP handler.
type Directory interface {
	People(ctx context.Context, q iamdomain.PeopleQuery) (iamdomain.PeoplePage, error)
	Person(ctx context.Context, id string) (iamdomain.PersonRow, error)
	Credentials(ctx context.Context, personID string) ([]iamdomain.CredentialRow, error)
	Sessions(ctx context.Context, personID string) ([]iamdomain.SessionRecord, error)
	History(ctx context.Context, q iamdomain.HistoryQuery) (iamdomain.HistoryPage, error)
	PositionAt(ctx context.Context, at time.Time) (uint64, error)

	// PersonByLogin names who holds a login on this node, which an edit
	// asks BEFORE its first record: a rename refused by somebody else
	// holding the name, met only at its own record, was met after an
	// earlier step landed.
	PersonByLogin(ctx context.Context, login string) (iamdomain.Sighting, error)

	// Claims is the claim duty's own reading, which the report shows on
	// demand: a duplicate or an orphan the duty warns about.
	Claims(ctx context.Context, now time.Time) (iamdomain.ClaimReport, error)
}

// Writer is one party's authority to change the identity estate, as this
// surface uses it.
//
// CONSUMER-DEFINED, and what it deliberately leaves out is [Writer.As]: that
// is how a PARTY is chosen, and a route that could choose one would be a
// route that could act as somebody else. The engine hands this surface an
// [Authority] instead, one writer per caller.
//
// EVERY WRITE ANSWERS ITS WHOLE [statelog.Result]. It used to answer a bare
// position, which cannot tell `applied` and `pending` from `unknown` — so an
// unknown outcome read as 200, and the removal, the revocation and the reset
// announced beside it had not necessarily happened.
type Writer interface {
	Enrol(ctx context.Context, in iamdomain.Enrolment) (statelog.Result, error)
	UpdatePerson(ctx context.Context, in iamdomain.PersonUpdate) (statelog.Result, error)
	SetStage(ctx context.Context, personID string, stage iam.Stage,
		opID, reason string) (statelog.Result, error)
	SetCredentials(ctx context.Context, in iamdomain.CredentialSet) (statelog.Result, error)
	MintToken(ctx context.Context, in iamdomain.TokenMint) (iamdomain.TokenMinted, error)
	Claim(ctx context.Context, kind iamdomain.ObjectKind, token, personID,
		opID string) (statelog.Result, error)
	Release(ctx context.Context, kind iamdomain.ObjectKind, token, holder,
		opID, reason string) (statelog.Result, error)

	// Rename and Rebind MOVE a person's login or seat, claiming the new
	// one before releasing the old — so a refusal changes nothing. A
	// release followed by a claim, which is what this surface used to
	// publish, left somebody whose new login was refused with none at all.
	Rename(ctx context.Context, personID, from, to, opID, reason string) (
		statelog.Result, error)
	Rebind(ctx context.Context, personID, from, to, opID, reason string) (
		statelog.Result, error)
	Invite(ctx context.Context, in iamdomain.InviteMint) (iamdomain.InviteIssued, error)

	// MayConfer is the record's own conferral rule, asked BEFORE the first
	// record of an edit that moves a claim ahead of its grants — so a grant
	// the caller may not confer is refused with nothing moved.
	MayConfer(before, after []iam.Grant) error

	Revoke(ctx context.Context, personID, opID, reason string) (statelog.Result, error)
	InvalidateAll(ctx context.Context, opID, reason string) (statelog.Result, error)
	Remove(ctx context.Context, personID, opID, reason string) (statelog.Result, error)
}

// Authority hands this surface one party's [Writer].
//
// A FUNCTION rather than a writer, because the author of an identity record
// is a property of the WRITER and never of the call — an authentication trail
// whose author field is chosen by the caller is not a trail — so a surface
// serving many parties takes one writer each.
//
// THE GRANTS TRAVEL WITH THE PARTY, and they are not this package's opinion:
// internal/iamdomain refuses a record the party may not author and refuses
// conferring a grant the party does not hold, so a handler that somehow
// skipped its own check still cannot make somebody an administrator.
//
// THE PARTY IS THE PRINCIPAL, WHOLE — the engine's writer derives from it the
// name records carry and the credential beside it ([iam.ActorFor]), the
// grants, and the principal's own id, which is what a gesture decided on WHO
// is making it asks: a person's token is theirs alone to mint. That id used
// to travel as a field of the mint the handler filled in, which made the
// domain's rule exactly as strong as this one route.
type Authority func(principal iam.Principal) Writer

// Opener opens one of a person's sealed values — [iamdomain.Sealer], over this
// node's keyring.
//
// NO CONTEXT, because nothing is fetched: the keyring is this process's own,
// so opening a value is arithmetic that either succeeds or names a value this
// ring cannot open. See [Service.open].
type Opener interface {
	Open(personID string, field iamdomain.Field, sealed string) (string, error)
}

// Audit is where this surface's identity facts go: a credential minted or
// revoked, a second factor reset, a session ended by somebody other than its
// holder.
//
// ONE METHOD, because everything this surface records was authored by a
// caller the guard resolved: there is no failed attempt here to count, only a
// write to announce. internal/iam/authevents' Trail is what a running node
// hands in.
type Audit interface {
	Emit(ctx context.Context, payload events.Payload)
}

// Options is what this surface is built from.
type Options struct {
	// Directory and Authority are required: a surface with a reader and
	// no writer would serve a directory nobody can change, and one with a
	// writer and no reader could not answer what it just wrote.
	Directory Directory
	Authority Authority

	// Opener opens a person's sealed name and address. REQUIRED: every
	// node holds the keyring, so a surface that opened nothing would
	// render every person in the company as sealed — a posture no running
	// node is in, and the look of a keyring nobody has.
	Opener Opener

	// ExternalBase is `api.external_url`, which is what an invitation's
	// link is built from.
	//
	// OPTIONAL, AND ITS ABSENCE REFUSES ONE ROUTE rather than the
	// surface. The whole directory — reading it, granting, suspending,
	// revoking — needs no external address at all; only the invitation
	// does, because a link built from a bind address is one nobody
	// outside the host can follow and the engine reads no scheme or host
	// off a request. Refusing to build over it would take the directory
	// down for a setting that affects a single gesture.
	ExternalBase string

	// Bindings classifies one person's seat binding for the
	// dangling-binding arm of the report. See [Bindings].
	//
	// NIL-ABLE, AND THE ABSENCE IS THE THIRD VALUE — the same shape
	// internal/api/chartapi's `Held` takes, one estate the other way
	// round: a node that cannot ask skips the arm rather than guessing.
	Bindings Bindings

	// Ceiling is this node's own `api.auth.max_grants`, which the report
	// compares a person's declared grants against. Empty means this node
	// grants nothing, which is a real posture rather than "no ceiling" —
	// config refuses an absent one on a node that serves.
	Ceiling []iam.Grant

	// Audit records what this surface changed. REQUIRED: the directory is
	// where credentials are minted and sessions ended by somebody else,
	// and a surface that announced none of it would leave the live feed
	// silent about the writes an investigation looks for first.
	Audit Audit

	// Now is the clock, injectable so a case can pin an expiry.
	Now func() time.Time
}

// Service is the surface.
type Service struct {
	directory Directory
	authority Authority
	opener    Opener
	external  string
	bindings  Bindings
	ceiling   []iam.Grant
	audit     Audit
	now       func() time.Time
}

// New builds it, or says which half is missing.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Directory == nil:
		return nil, errors.New("iamapi: this surface needs the identity " +
			"directory's read side")
	case opts.Authority == nil:
		return nil, errors.New("iamapi: this surface needs an authority — a " +
			"writer per caller, because an identity record's author is a " +
			"property of the writer and never of the call")
	case opts.Opener == nil:
		return nil, errors.New("iamapi: this surface needs the keyring's " +
			"opener — without it every person in the directory would render " +
			"as sealed")
	case opts.Audit == nil:
		return nil, errors.New("iamapi: this surface needs an audit trail — it " +
			"mints and revokes credentials and ends other people's sessions, " +
			"and a directory that announced none of it would leave the live " +
			"feed silent about exactly those writes")
	}
	s := &Service{
		directory: opts.Directory, authority: opts.Authority,
		opener:   opts.Opener,
		external: opts.ExternalBase, bindings: opts.Bindings,
		ceiling: slices.Clone(opts.Ceiling), audit: opts.Audit,
		now: opts.Now,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// writerFor is the caller's own authority to change this estate.
//
// IT TAKES THE PRINCIPAL FROM THE CONTEXT and never from a body, which is the
// whole of what makes the trail one: [iam.ActorFor] resolves a person to their
// seat handle or their login and states the KIND as a column, so a write by
// somebody at a dashboard and a write by the same person's token are two rows
// naming one party rather than two parties three rows apart.
func (s *Service) writerFor(ctx context.Context) (Writer, bool) {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return nil, false
	}
	return s.authority(principal), true
}

// open opens one sealed value into something a screen can render, and whether
// it did.
//
// A VALUE THAT DOES NOT OPEN IS NOT AN OUTAGE: it is ciphertext this node's
// keyring holds no key for — dropped from the ring before the values were
// moved off it, or a restore under a different keyring — and the person is
// still somebody this node knows. So it renders as sealed, is logged, and
// never takes the listing down over one row.
func (s *Service) open(ctx context.Context, personID string,
	field iamdomain.Field, sealed []byte) (string, bool) {

	if len(sealed) == 0 {
		return "", true
	}
	opened, err := s.opener.Open(personID, field, string(sealed))
	if err != nil {
		log.WarnContext(ctx, "api_iam_unseal_failed",
			"person", personID, "field", string(field), "error", err)
		return "", false
	}
	return opened, true
}
