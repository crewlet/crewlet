package main

import (
	"context"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
)

// A REGISTRATION IS JUDGED AGAINST THE ADDRESS A VENDOR REACHES, never the one a
// person does.
//
// Every writer of a surface's endpoint (the reconcile loop, the dashboard's setup
// write) stamps `config.API.WebhookBase`: the public listener's
// `api.public.external_url` once `api.public` is set. The integrations answer
// compares the recorded endpoint with an address [serveAPI] hands it; handed the
// PERSON's `api.external_url`, every healthy registration read as moved, and a
// hook on the admin socket (webhook routes answer 404) read as current.
//
// THROUGH serveAPI, because the comparison is internal/api/queries' and has its
// own cases; only the wiring can hand it the wrong address. Both directions.
func TestARegistrationIsJudgedAgainstTheVendorsAddress(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// A SURFACE TO HAVE A ROW. Slack's request URL is the one a person
	// types at the vendor, so nothing re-points it and this comparison is
	// the only thing that can say it moved.
	company.Integrations.Slack = &config.Slack{}
	e := testEngineWith(t, engine.Options{Company: company})

	boot := bootstrapFor(t, reservePort(t))
	const vendors = "https://hooks.example.com"
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: reservePort(t), ExternalURL: vendors + "/"}
	if boot.API.ExternalBase() == vendors {
		t.Fatal("the fixture's two addresses are one, so the case cannot tell them apart")
	}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	statuses, err := e.IntegrationStore()
	if err != nil {
		t.Fatalf("integration store: %v", err)
	}
	admin := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	for _, tc := range []struct {
		name     string
		endpoint string
		current  bool
	}{
		{"registered where vendors reach this deployment", vendors, true},
		{"registered on the person's address, which answers webhooks 404", boot.API.ExternalBase(), false},
	} {
		if err := statuses.SaveIntegration(t.Context(), integration.State{
			Kind: integration.KindSlack, Endpoint: tc.endpoint,
		}); err != nil {
			t.Fatalf("%s: record the endpoint: %v", tc.name, err)
		}
		row := integrationRow(t, getJSON(t, admin+"/integrations"), "slack")
		if got := row["endpoint"]; got != tc.endpoint {
			t.Fatalf("%s: endpoint = %v, want the recorded %q", tc.name, got, tc.endpoint)
		}
		if got := row["endpoint_current"]; got != tc.current {
			t.Errorf("%s: endpoint_current = %v, want %v", tc.name, got, tc.current)
		}
	}
}

// integrationRow is the integrations answer's row for one surface.
func integrationRow(t *testing.T, body map[string]any, kind string) map[string]any {
	t.Helper()
	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		if entry, _ := row.(map[string]any); entry["key"] == kind {
			return entry
		}
	}
	t.Fatalf("no %s row in %v", kind, body)
	return nil
}
