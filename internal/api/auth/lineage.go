package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHO A CREDENTIAL'S NAME STANDS FOR NOW: the fourth question the guard
// answers, for a surface that holds the NAME of a credential rather than the
// credential itself.
//
// # Why a surface would hold only the name
//
// A view ticket is minted on the dashboard's origin for the signed-in viewer
// and opened on the artifact origin, which holds no credential and never sees
// a cookie: that is the whole point of the origin. So the ticket carries the
// name of the credential it was minted through (`session:<lineage>`,
// `pat:<id>`, a Tier A token's `token:<id>`, the development principal's
// login), and every document the frame loads asks again who that credential
// stands for, from the identity rows, so a sign-out, a revocation, a
// suspension or a seat move ends the frame at its next document.
//
// # The same composition as a request's, and no second policy
//
// Each class is decided by the table its bearer is decided by
// ([session.StandingByName], [credential.CheckHeld]) over the rows alone, and
// composed by the function its bearer is composed by (the session arm's
// principal, [Guard.machinePrincipal], [Guard.principalFor]), so a ticket's
// principal is the one a request through the same credential would get on
// this node. What only a bearer can prove (its signature, a session's idle
// deadline, a token's secret) was proved by the request that minted the
// ticket, and the ticket never outlives the credential's absolute deadline.

// ErrNoLineage reports a principal acting through no credential a name can
// stand for: a seat's own turn, the engine itself.
var ErrNoLineage = errors.New("auth: the principal acts through no credential a " +
	"name can stand for")

// LineageOf is the name of the credential p acts through, the one
// [Lineages.Principal] resolves back: `session:<lineage>` for a browser
// session and `pat:<id>` for a machine token ([iam.Principal.Via]), and the
// login of a Tier A token or of the development principal, each of which is
// its own credential. ONE PACKAGE KNOWS BOTH DIRECTIONS, so a name minted here
// is always one this package can resolve.
func LineageOf(p iam.Principal) (string, error) {
	if p.Via != "" {
		if !iam.ValidCredentialName(p.Via) {
			return "", fmt.Errorf("%w: it acts through %q, which names no credential",
				ErrNoLineage, p.Via)
		}
		return p.Via, nil
	}
	switch {
	case strings.HasPrefix(p.Login, iam.TokenLoginPrefix):
		return p.Login, nil
	case p.Kind == iam.KindMachine && strings.HasPrefix(p.Login, DevPrincipalLoginPrefix):
		return p.Login, nil
	}
	return "", fmt.Errorf("%w: %s %q", ErrNoLineage, p.Kind, iam.RecordOwner(p))
}

// SessionStandings is the one read a session's name needs beyond the session
// arm's directory: whose session a lineage is, and whether the rows still hold
// it live (not ended, not past its absolute deadline, not opened before the
// company's last invalidation, at its holder's current revocation epoch),
// proved against the identity log's end so an absence is never a guess.
//
// CONSUMER-DEFINED and one method wide. The engine answers it
// (engine.Engine.SessionStanding); an ERROR is the unknown arm, always.
type SessionStandings interface {
	SessionStanding(ctx context.Context, lineage string, now time.Time) (owner string, live bool, err error)
}

// Lineages re-composes, from the identity rows alone, the principal a
// credential's name stands for.
type Lineages struct {
	guard    *Guard
	standing SessionStandings
}

// NewLineages builds the resolver over a guard wired exactly as the API's is:
// its Tier A table and admin posture, its seat bindings, its session and
// machine-token arms, its development principal. Pass the API's own guard, so
// the two can never be wired apart.
func NewLineages(g *Guard, standing SessionStandings) (*Lineages, error) {
	switch {
	case g == nil:
		return nil, errors.New("auth: a lineage resolver needs the guard whose arms it " +
			"composes principals through")
	case standing == nil:
		return nil, errors.New("auth: a lineage resolver needs to read whose a session " +
			"is; without it no session's name could be resolved")
	}
	return &Lineages{guard: g, standing: standing}, nil
}

// Principal answers who the credential named lineage stands for now: resolved,
// with the principal composed as a request through it would be on this node;
// [iam.Anonymous] for a credential that is over, a class this build does not
// name, or a seat the chart will not let it act as; [iam.Unknown] for a node
// that cannot tell, which a caller answers 503.
func (l *Lineages) Principal(ctx context.Context, lineage string) (iam.Principal, iam.Resolution) {
	return l.PrincipalFor(ctx, lineage, session.NeedRead)
}

// PrincipalFor is [Lineages.Principal] for a request that needs need of the
// session: an assistant grant's request is judged at the need its own method
// carries ([needOf]), exactly as the person's cookie sending it would be, so a
// write through a grant is never admitted on rows that would answer the
// person's own write 503.
func (l *Lineages) PrincipalFor(ctx context.Context, lineage string, need session.Need) (iam.Principal, iam.Resolution) {
	switch {
	case strings.HasPrefix(lineage, iam.SessionPrefix):
		return l.session(ctx, lineage, need)
	case strings.HasPrefix(lineage, iam.MachineTokenPrefix):
		return l.machineToken(ctx, lineage)
	case strings.HasPrefix(lineage, iam.TokenLoginPrefix):
		entry, held := l.guard.tokens.byLogin(lineage)
		if !held {
			return iam.Principal{}, iam.Anonymous
		}
		return l.tierA(ctx, entry, "")
	case l.guard.dev != nil && lineage == l.guard.dev.login:
		return l.guard.dev.principal(), iam.Resolved
	}
	return iam.Principal{}, iam.Anonymous
}

// session resolves a browser session's name.
func (l *Lineages) session(ctx context.Context, name string, need session.Need) (iam.Principal, iam.Resolution) {
	s := l.guard.sessions
	raw := strings.TrimPrefix(name, iam.SessionPrefix)
	lineage, err := uuid.Parse(raw)
	if s == nil || err != nil || !iam.ValidSessionName(name) {
		// A NODE WITH NO SESSION ARM resolves no cookie either, so no
		// session's name stands for anybody here.
		return iam.Principal{}, iam.Anonymous
	}
	now := s.now()
	owner, live, err := l.standing.SessionStanding(ctx, raw, now)
	switch {
	case err != nil:
		log.Log(ctx, UnreadLevel(ctx), "api_lineage_unavailable", "lineage", name,
			"error", err)
		return iam.Principal{}, iam.Unknown
	case !live:
		return iam.Principal{}, iam.Anonymous
	}
	if strings.HasPrefix(owner, iam.TokenLoginPrefix) {
		// A SESSION EXCHANGED FROM A TIER A TOKEN stands for the token,
		// composed from the entry this node still holds under its id, as
		// the guard composes that session's cookie.
		entry, held := l.guard.tokens.byLogin(owner)
		if !held {
			return iam.Principal{}, iam.Anonymous
		}
		return l.tierA(ctx, entry, iam.SessionName(raw))
	}
	identity, err := s.directory.Resolve(ctx, raw, owner)
	v := session.StandingByName(lineage, owner, identity, err)
	switch v.Answer(need) {
	case session.AnswerServe:
	case session.AnswerUnavailable:
		log.Log(ctx, UnreadLevel(ctx), "api_lineage_unavailable", "lineage", name,
			"row", string(v.Row), "detail", v.Detail, "error", errText(v.Err))
		return iam.Principal{}, iam.Unknown
	default:
		return iam.Principal{}, iam.Anonymous
	}
	if v.Session.EnrolmentOnly {
		// A SESSION THAT MAY ONLY ENROL A SECOND FACTOR reaches nothing a
		// ticket opens.
		return iam.Principal{}, iam.Anonymous
	}
	binding := session.ResolveSeat(ctx, s.chart, v.Person)
	switch binding.Answer() {
	case session.AnswerServe:
	case session.AnswerUnavailable:
		log.Log(ctx, UnreadLevel(ctx), "api_lineage_seat_unavailable", "lineage", name,
			"seat", v.Person.Seat, "detail", binding.Detail, "error", errText(binding.Err))
		return iam.Principal{}, iam.Unknown
	default:
		return iam.Principal{}, iam.Anonymous
	}
	return s.principal(v, binding, l.guard.admin, l.guard.proof), iam.Resolved
}

// machineToken resolves a machine token's name.
func (l *Lineages) machineToken(ctx context.Context, name string) (iam.Principal, iam.Resolution) {
	t := l.guard.machine
	id := strings.TrimPrefix(name, iam.MachineTokenPrefix)
	if t == nil || !iam.ValidMachineTokenName(name) {
		return iam.Principal{}, iam.Anonymous
	}
	now := t.now()
	// BY ITS ID ALONE: a ticket holds the token's name, never its secret,
	// which the request that minted the ticket proved — see [TokenDirectory].
	row, err := t.directory.MachineToken(ctx, credential.Token{ID: id})
	if err != nil {
		log.Log(ctx, UnreadLevel(ctx), "api_lineage_unavailable", "lineage", name,
			"error", err)
		return iam.Principal{}, iam.Unknown
	}
	check := credential.CheckHeld(row, now, statelog.StallGrace)
	switch check.Answer {
	case credential.TokenValid:
	case credential.TokenUnknown:
		log.Log(ctx, UnreadLevel(ctx), "api_lineage_unavailable", "lineage", name,
			"detail", check.Detail)
		return iam.Principal{}, iam.Unknown
	default:
		return iam.Principal{}, iam.Anonymous
	}
	p, refusal, unresolved := l.guard.machinePrincipal(ctx, id, row, now)
	switch {
	case unresolved != nil:
		return iam.Principal{}, iam.Unknown
	case refusal != nil:
		return iam.Principal{}, iam.Anonymous
	}
	return p, iam.Resolved
}

// tierA resolves a Tier A token's entry, through a session exchanged from it
// when via names one.
func (l *Lineages) tierA(ctx context.Context, entry config.APIToken, via string) (
	iam.Principal, iam.Resolution) {

	p, how, refusal := l.guard.principalFor(ctx, entry, l.guard.now())
	switch {
	case how != iam.Resolved:
		return iam.Principal{}, how
	case refusal != nil:
		return iam.Principal{}, iam.Anonymous
	}
	p.Via = via
	return p, iam.Resolved
}
