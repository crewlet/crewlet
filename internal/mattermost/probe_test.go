package mattermost_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// leaping is a clock an hour further on at every read, so the client's retry
// window is spent at the first failure and a case about an outage does not
// wait it out.
func leaping() func() time.Time {
	var reads atomic.Int64
	return func() time.Time {
		return time.Unix(0, 0).Add(time.Duration(reads.Add(1)) * time.Hour)
	}
}

func me(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/me" || r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

const sysadmin = `{"id":"u1","username":"ops","roles":"system_user system_admin"}`

func mattermostVendor() setuptest.Vendor {
	return setuptest.Vendor{
		App:          "Mattermost",
		Requirements: mattermost.Requirements(nil, nil),
		Probe: func(srv *httptest.Server) setup.Probe {
			return mattermost.Probe(srv.Client(), leaping())
		},
		Values: func(srv *httptest.Server) map[string]string {
			return map[string]string{"url": srv.URL, "team": "acme"}
		},
		Accept: me(sysadmin),
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid or expired session, please login again."}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

func TestTheMattermostProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, mattermostVendor())
}

func mattermostCheck(t *testing.T, h http.HandlerFunc) setup.Verdict {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	got, err := mattermost.Probe(srv.Client(), leaping())(context.Background(), setup.Check{
		Field: "admin_token",
		Value: func(field string) string {
			if field == "url" {
				return srv.URL
			}
			return "a-typed-token"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A TOKEN THAT AUTHENTICATES AS SOMEBODY WHO CANNOT CREATE BOTS is refused
// now, rather than on the first bot creation minutes later.
func TestAMattermostTokenWithoutTheAdminRoleIsRefused(t *testing.T) {
	t.Parallel()
	got := mattermostCheck(t, me(`{"id":"u2","username":"jane","roles":"system_user"}`))
	if got.Outcome != setup.VerifyRefused {
		t.Errorf("a non-administrator's token answered %+v", got)
	}
	works := mattermostCheck(t, me(sysadmin))
	if works.Detail != "Works. Mattermost accepted it for @ops, a system administrator." {
		t.Errorf("an administrator's token answered %+v", works)
	}
}
