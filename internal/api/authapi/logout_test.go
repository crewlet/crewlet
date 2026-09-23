package authapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// closingWriter records every session close it is asked for.
type closingWriter struct {
	stubWriter
	mu     sync.Mutex
	closed [][2]string
}

func (w *closingWriter) CloseSession(_ context.Context, lineage, person, _,
	_ string) (statelog.Result, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = append(w.closed, [2]string{lineage, person})
	return applied(statelog.Position{}), nil
}

// A SIGN-OUT ENDS WHICHEVER COOKIE THE GUARD ACCEPTED.
//
// The guard reads a bearer under either name, because a deployment that
// corrected `api.external_url` from http to https has every signed-in browser
// still holding the bare one. The sign-out read and cleared only the name this
// deployment issues — so a person holding the other clicked "sign out", was
// told they were signed out, and was not: their session was never closed and
// their cookie was never deleted, and the guard went on accepting it.
//
// AND THE CLOSE NAMES THE SESSION'S PERSON, off the same verified bearer as
// the lineage, which is the bucket the record has to be filed under.
func TestASignOutEndsTheBearerUnderEitherName(t *testing.T) {
	t.Parallel()
	b := bootstrapFor(t) // https, so the name this deployment issues is __Host-
	signer, err := session.New(session.Options{
		Material: runtoken.Material{
			ActiveID: "k1",
			Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "a-fixture-signing-key"}},
		},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	lineage, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint a lineage: %v", err)
	}
	millis := clock.UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	const person = "018f3a9c-0000-7000-8000-0000000000a1"
	bearer, err := signer.Mint(session.Mint{
		Lineage: lineage, Person: person, StartPosition: 1,
		AbsoluteExpiresAt: clock.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	writer := &closingWriter{}
	mux := http.NewServeMux()
	buildWith(t, b, nil, func(o *authapi.Options) { o.Writer = writer }).Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: bearer})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body.String())
	}

	if len(writer.closed) != 1 || writer.closed[0] != [2]string{lineage.String(), person} {
		t.Errorf("closed %v, want exactly [%s %s]: the bare-named cookie the "+
			"guard accepts was never ended", writer.closed, lineage, person)
	}
	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	for _, name := range []string{session.HostCookieName, session.CookieBaseName} {
		if !cleared[name] {
			t.Errorf("the sign-out did not clear %q, so a browser holding it "+
				"is still signed in", name)
		}
	}
}

// A SIGN-OUT ENDS A SESSION ONLY WHILE THE ROWS STILL HOLD IT.
//
// The sign-out read the cookie under its signature alone, so every cookie ever
// signed under a live key — past its deadline, revoked, a session a record had
// already ended — published a close and announced another `iam_session_ended`
// on every post: a row per request, authored by whoever held a cookie the
// engine no longer accepts, and a second ending for a session whose ending the
// trail already held. What a sign-out claims is that the session was live
// until now, and only this node's rows can say so.
//
// EVERY CASE CLEARS THE COOKIE AND ANSWERS 200, whatever the rows said: a
// person walking away from a shared machine is signed out of it either way.
// A node too far behind to read the rows still records the close the person
// asked for, and announces nothing it cannot say was live.
func TestASignOutEndsOnlyASessionStillLive(t *testing.T) {
	t.Parallel()
	live := func(r *signInRig) session.Identity {
		return session.Identity{
			Applied: ^uint64(0) >> 1, Generation: 2,
			Session: session.SessionRow{Found: true, Epoch: 3,
				ProvedAt: clock.Add(-2 * time.Hour)},
			Person: session.PersonRow{Found: true, Epoch: 3,
				Stage: iam.StageActive, Login: r.estate.person.Login},
		}
	}
	for _, tc := range []struct {
		name string
		// rows turns a live session's rows into the case's.
		rows func(*session.Identity)
		// mintedAgo is how long before the request the cookie was
		// minted, which is what puts it past its idle deadline.
		mintedAgo time.Duration
		closes    bool
		announces bool
	}{{
		name: "live", rows: func(*session.Identity) {},
		closes: true, announces: true,
	}, {
		name: "opened on a node ahead of this one",
		rows: func(id *session.Identity) {
			id.Applied, id.Session = 0, session.SessionRow{}
		},
		closes: true, announces: true,
	}, {
		name: "ended by a record",
		rows: func(id *session.Identity) { id.Session.Ended = true },
	}, {
		name: "revoked everywhere",
		rows: func(id *session.Identity) { id.Person.Epoch = 4 },
	}, {
		name: "past its idle deadline", rows: func(*session.Identity) {},
		mintedAgo: session.Idle + time.Minute,
	}, {
		name:   "on a node that cannot read its rows",
		rows:   func(id *session.Identity) { id.Lag = statelog.StallGrace + time.Second },
		closes: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			identity := &session.Identity{}
			r := newSignInRigWith(t, func(o *authapi.Options) {
				o.Sessions = rowsFunc(func() session.Identity { return *identity })
			})
			*identity = live(r)
			tc.rows(identity)

			minted := clock.Add(-tc.mintedAgo)
			signer, err := session.New(session.Options{
				Material: runtoken.Material{
					ActiveID: "k1",
					Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "a-fixture-signing-key"}},
				},
				Now: func() time.Time { return minted },
			})
			if err != nil {
				t.Fatalf("session.New: %v", err)
			}
			lineage := uuid.Must(uuid.NewV7())
			millis := minted.Add(-time.Hour).UnixMilli()
			for i := range 6 {
				lineage[i] = byte(millis >> (8 * (5 - i)))
			}
			cookie, err := signer.Mint(session.Mint{
				Lineage: lineage, Person: r.estate.person.ID, StartPosition: 1,
				Epoch: 3, Generation: 2, AbsoluteExpiresAt: clock.Add(72 * time.Hour),
			})
			if err != nil {
				t.Fatalf("mint: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
			req.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: cookie})
			rec := httptest.NewRecorder()
			mux := http.NewServeMux()
			r.svc.Routes(mux)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d (body %s): a sign-out answers 200 whatever "+
					"the rows said", rec.Code, rec.Body)
			}
			cleared := false
			for _, c := range rec.Result().Cookies() {
				if c.Name == session.CookieBaseName && c.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Errorf("the cookie was not cleared, so the browser is still " +
					"holding it")
			}

			r.estate.mu.Lock()
			closes := slices.Clone(r.estate.closes)
			r.estate.mu.Unlock()
			if got := len(closes) == 1; got != tc.closes || len(closes) > 1 {
				t.Errorf("closed %+v, want a close %v", closes, tc.closes)
			}
			emitted, _ := r.audit.snapshot()
			var ended []types.IAMSessionEnded
			for _, e := range emitted {
				if p, ok := e.(types.IAMSessionEnded); ok {
					ended = append(ended, p)
				}
			}
			switch {
			case tc.announces && (len(ended) != 1 || ended[0].Reason != types.EndLogout ||
				ended[0].Lineage != lineage.String()):
				t.Errorf("announced %+v, want one logout ending of %s", ended, lineage)
			case !tc.announces && len(ended) != 0:
				t.Errorf("announced %+v for a session the rows did not hold "+
					"live: its ending was not this sign-out's to say", ended)
			}
		})
	}
}

// rowsFunc is a session directory answering whatever its function reads now.
type rowsFunc func() session.Identity

func (f rowsFunc) Resolve(context.Context, string, string) (session.Identity, error) {
	return f(), nil
}

// ENDING A NAMED SESSION THAT IS ALREADY OVER WRITES NOTHING.
//
// The owner check read only who holds the lineage, so a session a record had
// already ended — or one past its deadline — was closed again and announced
// again, naming the person who asked as the one who ended it: the trail then
// held two endings for one session, the second one false about its cause.
func TestEndingANamedSessionAlreadyOverWritesNothing(t *testing.T) {
	t.Parallel()
	for _, over := range []bool{false, true} {
		r := newSignInRig(t)
		r.estate.owner, r.estate.over = r.estate.person.ID, over
		rec := r.asPerson(http.MethodPost,
			"/auth/logout/0192f00d-0000-7000-8000-0000000000bb", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("over=%v: status %d (%s)", over, rec.Code, rec.Body)
		}
		r.estate.mu.Lock()
		closes := len(r.estate.closes)
		r.estate.mu.Unlock()
		emitted, _ := r.audit.snapshot()
		switch {
		case over && (closes != 0 || len(emitted) != 0):
			t.Errorf("a session already over was closed %d times and "+
				"announced %+v", closes, emitted)
		case !over && (closes != 1 || len(emitted) != 1):
			t.Errorf("THE CONTROL: a live session was closed %d times and "+
				"announced %+v, want once each", closes, emitted)
		}
	}
}
