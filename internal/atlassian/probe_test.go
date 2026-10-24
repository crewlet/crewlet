package atlassian_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

func atlassianVendor() setuptest.Vendor {
	return setuptest.Vendor{
		App:          "Atlassian",
		Requirements: atlassian.Requirements(nil, nil),
		Probe: func(srv *httptest.Server) setup.Probe {
			return atlassian.Probe(atlassian.ClientOptions{BaseURL: srv.URL})
		},
		Values: func(*httptest.Server) map[string]string {
			return map[string]string{"org_id": "org-1", "deployment": "cloud"}
		},
		Accept: func(w http.ResponseWriter, r *http.Request) {
			// ONE PAGE OF ONE, read and kept by nobody: the answer is the
			// status.
			if r.Method != http.MethodGet ||
				r.URL.Path != "/admin/account-management/v1/orgs/org-1/service-accounts" ||
				r.URL.Query().Get("limit") != "1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"accountId":"a1"}],"links":{"next":"x"}}`))
		},
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
}

func TestTheAtlassianProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, atlassianVendor())
}

// A SCOPED KEY IS REFUSED WITH ITS REMEDY. It authenticates at every other
// organization route and is refused at the one that creates an account, so
// a 403 here is the answer a person most needs to read.
func TestAScopedAtlassianKeyIsRefusedWithItsRemedy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	got, err := atlassian.Probe(atlassian.ClientOptions{BaseURL: srv.URL})(context.Background(),
		setup.Check{Field: "api_key", Value: func(field string) string {
			if field == "org_id" {
				return "org-1"
			}
			return "a-scoped-key"
		}})
	if err != nil || got.Outcome != setup.VerifyRefused || !strings.Contains(got.Detail, "without scopes") {
		t.Errorf("a 403 answered %+v, %v", got, err)
	}
}

// THE KEY IS CHECKED AGAINST AN ORGANIZATION, so one with no organization is
// a check this cannot make.
func TestAnAtlassianKeyWithNoOrganizationIsUnusable(t *testing.T) {
	t.Parallel()
	_, err := atlassian.Probe(atlassian.ClientOptions{BaseURL: "http://127.0.0.1:1"})(
		context.Background(), setup.Check{Field: "api_key",
			Value: func(field string) string {
				if field == "api_key" {
					return "k"
				}
				return ""
			}})
	if !errors.Is(err, setup.ErrUnusableInput) {
		t.Errorf("no organization answered %v", err)
	}
}
