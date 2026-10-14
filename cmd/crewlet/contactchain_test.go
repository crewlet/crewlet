package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
)

// A SEAT'S CONTACT ID RESOLVES THROUGH THE NODE'S OWN CHAIN on the surface
// `crewlet run` serves. A contact id may be a `${VAR}`, and the one here is set
// in no process — only in the environment the engine was handed — so a read
// surface wired to the process environment instead finds nobody by it: the
// exact-id tier of `colleague` answers an admin with no candidate at all,
// while every delivery to that person is routed by the id the node resolved.
// [serveAPI]'s sources are where an engine meets the API, so it is held here,
// where a test boots an engine and serves it.
// Mutation: drop `Env` from the read surface's sources and the id names nobody.
func TestAContactIDResolvesThroughTheNodesOwnChain(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_CLI_TEST_FOUNDER_SLACK_ID"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	company, err := config.ParseCompany([]byte(peopleCompanyYAML + `    contact:
      slack_user_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := testEngineWith(t, engine.Options{Company: company,
		Environment: config.MapSource{variable: "U0FOUNDER"}})

	boot := bootstrapFor(t, freePort(t))
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/query/colleague?q=U0FOUNDER", nil)
	req.Header.Set("Authorization", "Bearer "+cliFixtureToken)
	rec := httptest.NewRecorder()
	surface.app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /query/colleague = %d: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Match *struct {
			Handle string `json:"handle"`
		} `json:"match"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if answer.Match == nil || answer.Match.Handle != "founder" {
		t.Errorf("the founder's Slack id resolves to %s, want the founder the "+
			"handed environment names", rec.Body)
	}
}
