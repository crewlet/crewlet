package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// THE INSTANCE ADDRESS IS AN ENTERPRISE SERVER'S, so the probe reaches the
// fake through the same /api/v3 derivation every other client uses.
func githubVendor() setuptest.Vendor {
	return setuptest.Vendor{
		App:          "GitHub",
		Requirements: github.Requirements(nil, nil),
		Probe:        func(srv *httptest.Server) setup.Probe { return github.Probe(srv.Client()) },
		Values: func(srv *httptest.Server) map[string]string {
			return map[string]string{"url": srv.URL}
		},
		Accept: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v3/user" || r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"Octocat"}`))
		},
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com"}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

func TestTheGitHubProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, githubVendor())
}

// A REFUSAL REPEATS GITHUB'S OWN WORDS, read out of its error object rather
// than shown as one.
func TestAGitHubRefusalSaysWhatGitHubSaid(t *testing.T) {
	t.Parallel()
	v := githubVendor()
	for _, c := range []struct {
		vendor http.HandlerFunc
		want   string
	}{
		{v.Refuse, "GitHub refused it: Bad credentials."},
		{v.Accept, "Works. GitHub accepted it for the account octocat."},
	} {
		srv := httptest.NewServer(c.vendor)
		got, err := v.Probe(srv)(context.Background(), setup.Check{
			Field: "token",
			Value: func(field string) string {
				if field == "token" {
					return "ghp_typed"
				}
				return v.Values(srv)[field]
			},
		})
		srv.Close()
		if err != nil || got.Detail != c.want {
			t.Errorf("answered %+v, %v, want %q", got, err, c.want)
		}
	}
}
