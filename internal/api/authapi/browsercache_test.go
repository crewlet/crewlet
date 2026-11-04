package authapi_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// ONLY A PERSON'S OWN SIGN-OUT EMPTIES THE BROWSER'S CACHE.
//
// An answer read by object id is kept under a key naming who it is for
// (internal/api/httpjson's Immutable), so a sign-in, a Tier A token's exchange
// and a step-up set their cookie and leave the cache alone: Chrome holds a
// response that empties it until it has scanned this origin's whole cache. A
// sign-out, a sign-out everywhere and a named sign-out of the session this
// browser holds send `Clear-Site-Data: "cache"` beside the cookies they clear,
// so what a tab that named no reader kept is not served to whoever uses the
// browser next. A named sign-out of another session leaves this browser alone.
//
// Mutations: sign a row out through clearSession and that row is red; send
// the header from session.Issue and the first three rows are red.
func TestOnlyASignOutEmptiesTheBrowsersCache(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(t *testing.T) *httptest.ResponseRecorder
		// empties is the Clear-Site-Data the response carries, SPELLED
		// OUT: `"cache"` and nothing else, or none at all.
		empties string
		// cookies is whether the response sets or clears a cookie.
		cookies bool
	}{
		{"a password sign-in", func(t *testing.T) *httptest.ResponseRecorder {
			return newSignInRig(t).signIn(t, "jane.doe", password, appCode(t, clock))
		}, "", true},
		{"a Tier A token's exchange", func(t *testing.T) *httptest.ResponseRecorder {
			r := newExchangeRig(t)
			req := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
			req.Header.Set("Authorization", "Bearer "+opsValue)
			rec := httptest.NewRecorder()
			r.handler.ServeHTTP(rec, req)
			return rec
		}, "", true},
		{"a step-up", func(t *testing.T) *httptest.ResponseRecorder {
			return newStepUpRig(t, session.RowValid).stepUp(t)
		}, "", true},
		{"a sign-out", func(t *testing.T) *httptest.ResponseRecorder {
			mux := http.NewServeMux()
			surface(t).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
			return rec
		}, `"cache"`, true},
		{"a sign-out everywhere", func(t *testing.T) *httptest.ResponseRecorder {
			return newSignInRig(t).asPerson(http.MethodPost, "/auth/logout/all", "")
		}, `"cache"`, true},
		{"a named sign-out of this browser's session", func(t *testing.T) *httptest.ResponseRecorder {
			r := newExchangeRig(t)
			held, _ := r.exchange(boundValue)
			lineages := r.estate.lineages()
			if held == nil || len(lineages) != 1 {
				t.Fatalf("the bound token opened %v: this row holds nothing", lineages)
			}
			return namedSignOut(r, held, lineages[0])
		}, `"cache"`, true},
		{"a named sign-out of another session", func(t *testing.T) *httptest.ResponseRecorder {
			r := newExchangeRig(t)
			held, _ := r.exchange(boundValue)
			opened := r.estate.lineages()
			if other, _ := r.exchange(boundValue); held == nil || other == nil {
				t.Fatal("the bound token did not exchange: this row holds nothing")
			}
			for _, lineage := range r.estate.lineages() {
				if !slices.Contains(opened, lineage) {
					return namedSignOut(r, held, lineage)
				}
			}
			t.Fatal("the second exchange opened no session of its own")
			return nil
		}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := tc.answer(t)
			if rec.Code != http.StatusOK {
				t.Fatalf("answered %d (%s), want 200: this row holds nothing", rec.Code, rec.Body)
			}
			if got := len(rec.Result().Cookies()) != 0; got != tc.cookies {
				t.Fatalf("set or cleared a cookie: %v, want %v", got, tc.cookies)
			}
			if got := strings.Join(rec.Header().Values("Clear-Site-Data"), ", "); got != tc.empties {
				t.Errorf("carried Clear-Site-Data %q, want %q", got, tc.empties)
			}
		})
	}
}

// namedSignOut posts a named sign-out of lineage from the browser holding
// cookie.
func namedSignOut(r *exchangeRig, cookie *http.Cookie, lineage string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/logout/"+lineage, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}
