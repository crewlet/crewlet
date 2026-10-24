package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// instanceAnswering is a GitLab whose token describes itself as self, or
// does not answer the description at all where self is empty: an instance
// older than 15.5.
func instanceAnswering(self string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"Alice"}`))
		case r.URL.Path == "/api/v4/personal_access_tokens/self" && self != "":
			_, _ = w.Write([]byte(self))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
		}
	}
}

func gitlabVendor() setuptest.Vendor {
	return setuptest.Vendor{
		App:          "GitLab",
		Requirements: gitlab.Requirements(nil, nil),
		Probe:        func(srv *httptest.Server) setup.Probe { return gitlab.Probe(srv.Client()) },
		Values: func(srv *httptest.Server) map[string]string {
			return map[string]string{"url": srv.URL}
		},
		Accept: instanceAnswering(`{"scopes":["api"],"expires_at":"2027-01-31","active":true}`),
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}
}

func TestTheGitLabProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, gitlabVendor())
}

func gitlabCheck(t *testing.T, field string, h http.HandlerFunc) setup.Verdict {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	got, err := gitlab.Probe(srv.Client())(context.Background(), setup.Check{
		Field: field,
		Value: func(name string) string {
			if name == "url" {
				return srv.URL
			}
			return "glpat-typed"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// THE SCOPE IS WHAT THE FIELD NEEDS, because a token without it
// authenticates: the access token creates accounts and needs `api`, the
// routing token reads and is satisfied by `read_api`.
func TestAGitLabTokenIsHeldToTheScopeItsFieldNeeds(t *testing.T) {
	t.Parallel()
	readOnly := instanceAnswering(`{"scopes":["read_api"]}`)
	if got := gitlabCheck(t, "admin_token", readOnly); got.Outcome != setup.VerifyRefused {
		t.Errorf("a read_api token as the access token answered %+v", got)
	}
	if got := gitlabCheck(t, "token", readOnly); got.Outcome != setup.VerifyWorks ||
		got.Detail != "Works. GitLab accepted it for the account alice." {
		t.Errorf("a read_api token as the routing token answered %+v", got)
	}
	expiring := gitlabCheck(t, "admin_token", instanceAnswering(`{"scopes":["api"],"expires_at":"2027-01-31"}`))
	if expiring.Detail != "Works. GitLab accepted it for the account alice, and it expires on 2027-01-31." {
		t.Errorf("answered %+v", expiring)
	}
}

// AN INSTANCE THAT CANNOT DESCRIBE THE TOKEN still proved it works: the
// check stands on the identity rather than failing a working token.
func TestAGitLabTokenWithNoDescriptionStandsOnItsIdentity(t *testing.T) {
	t.Parallel()
	if got := gitlabCheck(t, "admin_token", instanceAnswering("")); got.Outcome != setup.VerifyWorks {
		t.Errorf("answered %+v", got)
	}
}
