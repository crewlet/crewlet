package authapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// A SIGN-IN OR A SIGN-OUT EMPTIES THE BROWSER'S CACHE, AND A STEP-UP KEEPS IT.
//
// An answer read by object id is kept by the browser that asked for it under
// a cache key that holds no cookie (internal/api/httpjson's Immutable): the
// cookie's bytes move at every re-issue, which would miss every kept answer
// five minutes later. So what a browser keeps is scoped to who it is signed in
// as from this side: every route here whose answer may change who that is —
// a sign-in, a Tier A token's exchange, a sign-out — sends
// `Clear-Site-Data: "cache"` beside the cookie it sets or clears, and the next
// person to sign in on the browser is never answered from what the last one
// was sent. A step-up replaces the session the same person presented, so it
// carries theirs on and empties nothing: a confirmation would otherwise cost
// the person every answer their browser kept.
//
// Mutations: set the sign-in's cookie through session.Continue, the
// exchange's likewise, or clear the sign-out's cookies without the header,
// and that row is red; set the step-up's replacement through session.Begin
// and its row is red.
func TestASignInOrOutEmptiesTheBrowsersCacheAndAStepUpKeepsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(t *testing.T) *httptest.ResponseRecorder
		// empties is the Clear-Site-Data the response carries, SPELLED
		// OUT: `"cache"` and nothing else, or none at all.
		empties string
	}{
		{"a password sign-in", func(t *testing.T) *httptest.ResponseRecorder {
			return newSignInRig(t).signIn(t, "jane.doe", password, appCode(t, clock))
		}, `"cache"`},
		{"a Tier A token's exchange", func(t *testing.T) *httptest.ResponseRecorder {
			r := newExchangeRig(t)
			req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
			req.Header.Set("Authorization", "Bearer "+opsValue)
			rec := httptest.NewRecorder()
			r.handler.ServeHTTP(rec, req)
			return rec
		}, `"cache"`},
		{"a sign-out", func(t *testing.T) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			surface(t).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
			return rec
		}, `"cache"`},
		{"a step-up", func(t *testing.T) *httptest.ResponseRecorder {
			return newStepUpRig(t, session.RowValid).stepUp(t)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := tc.answer(t)
			if rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s), want 200: this row holds nothing", rec.Code, rec.Body)
			}
			// THE COOKIE MOVES BESIDE IT, set or cleared, so the row is
			// about a response that changes what the browser holds.
			if len(rec.Result().Cookies()) == 0 {
				t.Fatalf("set no cookie: this row holds the header beside nothing")
			}
			if got := strings.Join(rec.Header().Values("Clear-Site-Data"), ", "); got != tc.empties {
				t.Errorf("carried Clear-Site-Data %q, want %q", got, tc.empties)
			}
		})
	}
}
