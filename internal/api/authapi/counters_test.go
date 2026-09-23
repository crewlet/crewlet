package authapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SIGN-IN'S BEARER CARRIES THE COUNTERS ITS SESSION WAS OPENED AT.
//
// The revocation epoch and the fleet's session generation are what end a
// bearer, so a bearer carrying less than the values the writer opened its
// session at is over the moment it is presented. It used to carry zero for
// both whatever the writer held: after somebody's first "sign out everywhere"
// they could never sign in again, and after the first `invalidate-all` nobody
// could. internal/iamdomain proves the writer READS the counters; this proves
// the surface MINTS what the writer read.
//
// Mutation: drop either field from the mint and the bearer reads zero.
func TestASignInsBearerCarriesTheCountersItsSessionOpenedAt(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	r.estate.counters = iamdomain.SessionOpened{Epoch: 3, Generation: 2}
	rec := r.signIn(t, "jane.doe", password, appCode(t, clock))
	if rec.Code != http.StatusOK {
		t.Fatalf("a correct password and code answered %d: %s", rec.Code,
			rec.Body.String())
	}
	bearer := bearerIn(t, rec.Result().Cookies())
	if bearer.Epoch != 3 || bearer.Generation != 2 {
		t.Errorf("the bearer carries epoch %d and generation %d, want the 3 "+
			"and 2 its session was opened at", bearer.Epoch, bearer.Generation)
	}
}

// bearerIn is the session bearer a response set, parsed under the fixture's
// own key. The directory answers nothing: what is read back is what the
// SIGNATURE covers, which is the whole of what the surface minted.
func bearerIn(t *testing.T, cookies []*http.Cookie) session.Bearer {
	t.Helper()
	for _, c := range cookies {
		for _, name := range session.CookieNames {
			if c.Name == name && c.Value != "" {
				return fixtureSigner(t).Validate(t.Context(), unreadable{},
					c.Value).Bearer
			}
		}
	}
	t.Fatalf("the response set no session cookie: %v", cookies)
	return session.Bearer{}
}

// unreadable is a directory that cannot be read, so a validation reports the
// bearer it parsed and decides nothing else.
type unreadable struct{}

func (unreadable) Resolve(context.Context, string, string) (session.Identity, error) {
	return session.Identity{}, errors.New("not consulted")
}
