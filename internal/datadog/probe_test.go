package datadog_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

func datadogVendor(t *testing.T) setuptest.Vendor {
	return setuptest.Vendor{
		App:          "Datadog",
		Requirements: datadog.Requirements(nil, nil),
		Probe: func(srv *httptest.Server) setup.Probe {
			return datadog.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
		},
		Values: func(*httptest.Server) map[string]string {
			return map[string]string{"site": "datadoghq.eu", "api_key": "api", "app_key": "app"}
		},
		Accept: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/org" || r.Header.Get("DD-API-KEY") == "" ||
				r.Header.Get("DD-APPLICATION-KEY") == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"orgs":[{"name":"FairSplit","public_id":"abc"}]}`))
		},
		Refuse: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
		},
		Outage: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
}

// THE SUITE every vendor's probe passes.
func TestTheDatadogProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, datadogVendor(t))
}

// A WORKING PAIR NAMES ITS ORGANIZATION and its region, the two things an
// operator checks to see they connected the account they meant to.
func TestADatadogCheckNamesTheOrganization(t *testing.T) {
	t.Parallel()
	v := datadogVendor(t)
	srv := httptest.NewServer(v.Accept)
	t.Cleanup(srv.Close)
	got, err := v.Probe(srv)(context.Background(), setup.Check{
		Field: "api_key",
		Value: func(field string) string { return v.Values(srv)[field] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Detail != "Works. Datadog accepted it for the FairSplit organization in datadoghq.eu." {
		t.Errorf("detail = %q", got.Detail)
	}
}

// THE KEYS ARE CHECKED TOGETHER, so one without the other is a check this
// cannot make, said as such, and never a refusal sent to Datadog.
func TestADatadogKeyWithoutItsPartnerIsUnusable(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	t.Cleanup(srv.Close)
	probe := datadog.Probe(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
	for _, c := range []struct {
		field  string
		values map[string]string
		says   string
	}{
		{"api_key", map[string]string{"api_key": "a", "site": "datadoghq.com"}, "no application key"},
		{"app_key", map[string]string{"app_key": "b", "site": "datadoghq.com"}, "no API key"},
		{"api_key", map[string]string{"api_key": "a", "app_key": "b", "site": "datadog.example"}, "region"},
	} {
		_, err := probe(context.Background(), setup.Check{
			Field: c.field, Value: func(f string) string { return c.values[f] },
		})
		if !errors.Is(err, setup.ErrUnusableInput) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s with %v: %v", c.field, c.values, err)
		}
	}
	if called.Load() {
		t.Error("Datadog was asked about a pair that was not whole")
	}
}
