package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// EVERY ANSWER UNDER /auth IS no-store, THE GUARD'S OWN INCLUDED.
//
// /auth answers a second-factor seed, recovery codes shown once, who somebody
// is and a Set-Cookie: no cache may keep them, nor a refusal there. A wrapper
// around the routes left the guard's own answers cacheable (the 401, the 403 to
// an enrolment-only session, the 503), so the guard marks every answer under
// /auth before it resolves anything.
//
// The CONTROL is a refusal outside /auth, which carries no such header.
// Mutation: drop the guard's marking and the /auth rows fail; mark every path
// and the control does.
func TestEveryAnswerUnderAuthIsNoStoreTheGuardsOwnIncluded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		path    string
		prepare func(*signedIn) func(*http.Request)
		status  int
		marked  bool
	}{
		{"no credential on a guarded /auth route", "/auth/totp/recovery",
			func(*signedIn) func(*http.Request) { return nil },
			http.StatusUnauthorized, true},
		{"an enrolment-only session on a route it may not reach", "/auth/totp/recovery",
			func(s *signedIn) func(*http.Request) {
				s.dir.identity.Session.EnrolmentOnly = true
				return s.withCookie
			}, http.StatusForbidden, true},
		{"a node that cannot read the estate", "/auth/session",
			func(s *signedIn) func(*http.Request) {
				s.dir.err = errors.New("the replicated estate is not open")
				return s.withCookie
			}, http.StatusServiceUnavailable, true},
		{"a refusal outside /auth (the control)", "/iam/people",
			func(*signedIn) func(*http.Request) { return nil },
			http.StatusUnauthorized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSignedIn(t)
			prepare := tc.prepare(s)
			h := s.guard().Middleware(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			method := http.MethodPost
			if tc.path == "/auth/session" || tc.path == "/iam/people" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tc.path, nil)
			if prepare != nil {
				prepare(req)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("answered %d, want the guard's %d; this case is about "+
					"what the guard writes", rec.Code, tc.status)
			}
			got := rec.Header().Get("Cache-Control")
			if tc.marked && got != "no-store" {
				t.Errorf("answered %d with Cache-Control %q, want no-store",
					rec.Code, got)
			}
			if !tc.marked && got != "" {
				t.Errorf("a refusal outside /auth carries Cache-Control %q", got)
			}
		})
	}
}
