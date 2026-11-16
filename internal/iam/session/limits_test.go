package session_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// limitsRig is a signed-in session whose signer reads limits a case changes
// after the sign-in, as an administrator changes the company's policy.
type limitsRig struct {
	*signedIn
	mu     sync.Mutex
	limits session.Limits
}

func newLimitsRig(t *testing.T, at session.Limits) *limitsRig {
	t.Helper()
	r := &limitsRig{limits: at}
	c := &clock{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	signer, err := session.New(session.Options{Material: keyring(), Now: c.now, Limits: r.current})
	if err != nil {
		t.Fatalf("build a signer: %v", err)
	}
	lineage := uuid.Must(uuid.NewV7())
	cookie, err := signer.Mint(session.Mint{
		Lineage: lineage, Person: personID, Epoch: 3, Generation: 1, StartPosition: startPos,
		SignedInAt: c.at, AbsoluteExpiresAt: c.at.Add(at.Lifetime),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	r.signedIn = &signedIn{
		t: t, signer: signer, clock: c, lineage: lineage, cookie: cookie, person: personID,
		dir: &directory{identity: session.Identity{
			Applied: startPos, Generation: 1,
			Session: session.LineageRow{Found: true, Epoch: 3},
			Person:  session.PersonRow{Found: true, Epoch: 3, Stage: iam.StageActive, Login: "sarah.chen"},
		}},
	}
	return r
}

func (r *limitsRig) current() session.Limits {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.limits
}

func (r *limitsRig) set(l session.Limits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limits = l
}

// request validates the rig's cookie and keeps a re-issued one, as a browser
// keeps the cookie a response sets.
func (r *limitsRig) request() session.Validation {
	v := r.validate()
	if v.Reissue != "" {
		r.cookie = v.Reissue
	}
	return v
}

// A LOWERED LIFETIME ENDS A LIVE SESSION AT ITS NEXT REQUEST; A RAISED ONE
// STRETCHES NO SESSION SIGNED IN BEFORE IT (ADR-0152).
// Mutation: check the signed deadline alone and the lowered row is served.
func TestALoweredLifetimeEndsALiveSessionAndARaisedOneStretchesNone(t *testing.T) {
	t.Parallel()
	week, day := 7*24*time.Hour, 24*time.Hour
	lowered := newLimitsRig(t, session.Limits{Lifetime: week, Idle: week})
	lowered.clock.advance(2 * day)
	if got := lowered.request(); got.Row != session.RowValid {
		t.Fatalf("the control: a two-day-old session under a week's lifetime landed on %q", got.Row)
	}
	lowered.set(session.Limits{Lifetime: day, Idle: week})
	if got := lowered.request(); got.Row != session.RowEnded {
		t.Errorf("a two-day-old session under a lifetime lowered to a day landed on %q, want ended", got.Row)
	}

	raised := newLimitsRig(t, session.Limits{Lifetime: day, Idle: week})
	raised.set(session.Limits{Lifetime: week, Idle: week})
	raised.clock.advance(day + time.Minute)
	got := raised.request()
	if got.Row != session.RowEnded {
		t.Errorf("a session signed in under a day's lifetime lived past it once the "+
			"lifetime was raised: landed on %q", got.Row)
	}
}

// A LOWERED IDLE LIMIT ENDS A LIVE SESSION AT ITS NEXT REQUEST; A RAISED ONE
// NEVER STRETCHES ITS WINDOW, however often it is re-issued.
// Mutation: re-issue at the limit in force and the raised row is served.
func TestALoweredIdleLimitEndsALiveSessionAndARaisedOneStretchesNone(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	lowered := newLimitsRig(t, session.Limits{Lifetime: week, Idle: 12 * time.Hour})
	lowered.clock.advance(2 * time.Hour)
	lowered.set(session.Limits{Lifetime: week, Idle: time.Hour})
	if got := lowered.request(); got.Row != session.RowEnded {
		t.Errorf("a session unused for two hours under an idle limit lowered to one "+
			"landed on %q, want ended", got.Row)
	}

	raised := newLimitsRig(t, session.Limits{Lifetime: week, Idle: time.Hour})
	raised.set(session.Limits{Lifetime: week, Idle: 12 * time.Hour})
	raised.clock.advance(30 * time.Minute)
	if got := raised.request(); got.Row != session.RowValid || got.Reissue == "" {
		t.Fatalf("the control: a session used after half an hour landed on %q "+
			"(reissued %t)", got.Row, got.Reissue != "")
	}
	raised.clock.advance(time.Hour + time.Minute)
	if got := raised.request(); got.Row != session.RowEnded {
		t.Errorf("a session issued under an hour's idle limit outlived an hour unused "+
			"once the limit was raised: landed on %q", got.Row)
	}
}

// A SHORT IDLE LIMIT IS RE-ISSUED WITHIN A TENTH OF ITSELF, and the default
// keeps the five-minute throttle.
// Mutation: throttle every re-issue at five minutes and the short row fails.
func TestAShortIdleLimitIsReissuedWithinATenthOfItself(t *testing.T) {
	t.Parallel()
	short := newLimitsRig(t, session.Limits{Lifetime: time.Hour, Idle: 15 * time.Minute})
	short.clock.advance(2 * time.Minute)
	if got := short.request(); got.Reissue == "" {
		t.Error("a cookie two minutes stale under a fifteen-minute idle limit was not re-issued")
	}
	long := newLimitsRig(t, session.Limits{Lifetime: 7 * 24 * time.Hour, Idle: 12 * time.Hour})
	long.clock.advance(2 * time.Minute)
	if got := long.request(); got.Reissue != "" {
		t.Error("a cookie two minutes stale under the default idle limit was re-issued")
	}
}

// A BEARER NEEDS ITS SIGN-IN INSTANT: a lowered lifetime is measured from it,
// so a mint without one is refused rather than read as now.
func TestAMintWithoutItsSignInInstantIsRefused(t *testing.T) {
	t.Parallel()
	signer, err := session.New(session.Options{Material: keyring()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Mint(session.Mint{
		Lineage: uuid.Must(uuid.NewV7()), Person: personID,
		AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Error("a bearer was minted with no sign-in instant")
	}
}
