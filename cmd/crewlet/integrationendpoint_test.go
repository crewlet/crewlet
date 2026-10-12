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
// Every writer of a surface's endpoint — the reconcile loop and the dashboard's
// setup write alike — stamps the vendors' address, `config.API.WebhookBase`:
// the public listener's `api.public.external_url` once `api.public` is set. The
// integrations answer compares that recorded endpoint with an address
// [serveAPI] hands it, and it was handed the PERSON's `api.external_url`: on
// every deployment with a public listener each healthy registration read as
// moved, for ever, and its card as Action needed — while a hook registered on
// the admin socket, where every webhook route answers 404, read as current.
//
// THROUGH serveAPI, because the comparison itself is internal/api/queries' and
// has its own cases; what only a running node can get wrong is which address
// it is handed. Both directions, so the case fails on a wiring that answers
// true or false for everything.
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

	boot := bootstrapFor(t, freePort(t))
	const vendors = "https://hooks.example.com"
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: freePort(t), ExternalURL: vendors + "/"}
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
