package confluence_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// confluenceVendor is a Confluence at a loopback address, which is whichever
// deployment the company declared: a Cloud site at a host no rule names is
// exactly what an Isolated Cloud company's is.
func confluenceVendor(declared atlassianhost.Deployment) setuptest.Vendor {
	return setuptest.Vendor{
		App: "Confluence",
		// BOTH DEPLOYMENTS' FORMS, since each drops the other's fields.
		Requirements: append(confluence.Requirements(nil, true, nil), confluence.Requirements(nil, false, nil)...),
		Probe:        func(srv *httptest.Server) setup.Probe { return confluence.Probe(srv.Client()) },
		Values: func(srv *httptest.Server) map[string]string {
			return map[string]string{"url": srv.URL, "email": "ops@acme.example"}
		},
		Declared: declared,
		Accept: func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains([]string{"/rest/api/user/current"}, r.URL.Path) || r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountId":"5b10","displayName":"Jane Doe"}`))
		},
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Current user not permitted to use Confluence"}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
}

func TestTheConfluenceProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	for _, declared := range []atlassianhost.Deployment{atlassianhost.Cloud, atlassianhost.DataCenter} {
		t.Run(string(declared), func(t *testing.T) {
			t.Parallel()
			setuptest.Run(t, confluenceVendor(declared))
		})
	}
}

// A WORKING TOKEN NAMES ITS ACCOUNT by the name a person recognises, and a
// refusal repeats Confluence's own words out of its error object.
func TestAConfluenceCheckSaysWhoAndWhy(t *testing.T) {
	t.Parallel()
	v := confluenceVendor(atlassianhost.DataCenter)
	for _, c := range []struct {
		vendor http.HandlerFunc
		want   setup.VerifyOutcome
		says   string
	}{
		{v.Accept, setup.VerifyWorks, "Works. Confluence accepted it for Jane Doe."},
		{v.Refuse, setup.VerifyRefused, "Confluence refused it: "},
	} {
		srv := httptest.NewServer(c.vendor)
		got, err := v.Probe(srv)(context.Background(), setup.Check{
			Field: "token", Declared: v.Declared,
			Value: func(field string) string {
				if field == "token" {
					return "a-pat"
				}
				return v.Values(srv)[field]
			},
		})
		srv.Close()
		if err != nil || got.Outcome != c.want || !strings.HasPrefix(got.Detail, c.says) ||
			strings.Contains(got.Detail, "{") {
			t.Errorf("answered %+v, %v, want %s %q", got, err, c.want, c.says)
		}
	}
}

// WHAT THE CHECK CANNOT BE MADE FROM is the request's fault, said before any
// site is asked: no address at all, or a Cloud token with no account email,
// which Cloud refuses as a bearer whatever the token.
func TestAConfluenceCheckThatCannotBeMadeIsUnusable(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		values   map[string]string
		declared atlassianhost.Deployment
	}{
		"no site":             {map[string]string{"token": "t"}, atlassianhost.Unknown},
		"a Cloud token alone": {map[string]string{"token": "t", "url": "https://acme.atlassian.net"}, atlassianhost.Unknown},
	} {
		_, err := confluence.Probe(nil)(context.Background(), setup.Check{
			Field: "token", Declared: c.declared,
			Value: func(field string) string { return c.values[field] },
		})
		if !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("%s: answered %v", name, err)
		}
	}
}
