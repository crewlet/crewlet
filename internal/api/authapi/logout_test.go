package authapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
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
	_ string) (statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = append(w.closed, [2]string{lineage, person})
	return statelog.Position{}, nil
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
