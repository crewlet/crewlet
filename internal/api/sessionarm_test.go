package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// cookieArm is the guard's REAL session arm over rows a case states — one
// person per bearer, found by the person the bearer names — and the signer
// its cookies are minted with, so a case presents a session cookie exactly as
// a signed-in browser does.
type cookieArm struct {
	arm    *auth.Sessions
	signer *session.Signer
	rows   personRows
	name   string
}

// personRows answers each bearer's identity by the person it names: a person
// it holds no row for is a session a node that covers the start has ended.
type personRows map[string]session.Identity

func (r personRows) Resolve(_ context.Context, _, person string) (session.Identity, error) {
	if identity, ok := r[person]; ok {
		return identity, nil
	}
	return session.Identity{Applied: cookieApplied}, nil
}

// AwaitApplied arrives at once: these rows cover every start a case mints at.
func (personRows) AwaitApplied(context.Context, uint64) error { return nil }

// cookieApplied is how far every row here is applied, past every bearer's
// start, so an absent row reads as a session that ENDED rather than one not
// yet arrived.
const cookieApplied = 1 << 20

// newCookieArm builds the arm for a bootstrap, over no rows yet.
func newCookieArm(t *testing.T, b *config.Bootstrap) *cookieArm {
	t.Helper()
	signer, err := session.New(session.Options{
		Material: runtoken.Material{ActiveID: "k1",
			Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "the-posture-signing-key"}}},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	rows := personRows{}
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: signer, Directory: rows, Applier: rows, Chart: postureNoSeats{},
		External: b.API.ExternalBase(), Audit: silentAudit{},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("auth.NewSessions: %v", err)
	}
	return &cookieArm{arm: arm, signer: signer, rows: rows,
		name: session.CookieName(b.API.ExternalBase())}
}

// person enrols one person holding grants, whose session proved who they are
// at provedAt, and answers the cookie their browser holds.
func (c *cookieArm) person(t *testing.T, grants []iam.Grant, provedAt time.Time) *http.Cookie {
	t.Helper()
	cookie := c.mint(t)
	c.rows[cookie.person] = session.Identity{
		Applied: cookieApplied, Generation: 1,
		Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: provedAt},
		Person: session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive,
			Login: "jane.doe", Grants: grants},
	}
	return cookie.cookie
}

// ended is a cookie whose session this node has applied the end of.
func (c *cookieArm) ended(t *testing.T) *http.Cookie {
	t.Helper()
	return c.mint(t).cookie
}

type minted struct {
	cookie *http.Cookie
	person string
}

func (c *cookieArm) mint(t *testing.T) minted {
	t.Helper()
	lineage := uuid.Must(uuid.NewV7())
	millis := clock.UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	person := uuid.Must(uuid.NewV7()).String()
	bearer, err := c.signer.Mint(session.Mint{
		Lineage: lineage, Person: person, Epoch: 1, Generation: 1,
		StartPosition: 1, AbsoluteExpiresAt: clock.Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return minted{cookie: &http.Cookie{Name: c.name, Value: bearer}, person: person}
}
