package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A LINK FOR A PERSON IS BUILT ON THE DASHBOARD'S ADDRESS, never the vendors'.
// Behind a public listener (Tier A `api.public`) the vendors' address is a
// socket that serves the webhooks and answers the dashboard 404, so a skill's
// ${crewlet_base_url} and a page change's link composed on it would land every
// person on nothing — while a webhook registered on the dashboard's address
// would land every delivery on the 404 the other way round.
//
// THROUGH [New], because the address a link is composed on is the one the
// engine took from its Tier A at construction ([Engine.publicBase]), and a
// case handing an engine literal its base would pass with that wiring gone.
func TestPeopleAreLinkedToTheDashboardAndVendorsToThePublicBase(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.API.ExternalURL = "https://crewlet.example.com/"
	b.API.Public = config.APIPublic{Port: 8443, ExternalURL: "https://hooks.example.com"}
	e := baseEngine(t, b)
	c := &Company{Config: &config.Company{}}

	if got := e.publicBase(c); got != "https://crewlet.example.com" {
		t.Errorf("a page change links on %q, want the dashboard's address", got)
	}
	if got := skillVariables(e.resolver(), c, e.publicBase(c), kitVersion())[config.ReservedBaseURLVariable]; got != "https://crewlet.example.com" {
		t.Errorf("${%s} = %q, want the dashboard's address", config.ReservedBaseURLVariable, got)
	}
	if got := e.webhookBase; got != "https://hooks.example.com" {
		t.Errorf("a webhook is registered on %q, want the vendors' address", got)
	}
	// AND A PASS WITH NO ADDRESS NAMES THE KEY THAT ADDRESS IS READ FROM,
	// taken from the same block: the public listener's, never the person's.
	if got := e.webhookBaseKey; got != "api.public.external_url" {
		t.Errorf("a pass with no address is told to set %q, want the public "+
			"listener's key", got)
	}
}

// NO DASHBOARD ADDRESS IS NO LINK, never the vendors' address borrowed: that is
// the wrong one exactly where the two differ, and a skill rendering
// ${crewlet_base_url} literally is the registry's signal that it is undefined.
func TestNoDashboardBaseComposesNoLink(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.API.Public = config.APIPublic{Port: 8443, ExternalURL: "https://hooks.example.com"}
	e := baseEngine(t, b)
	c := &Company{Config: &config.Company{}}
	if got := e.publicBase(c); got != "" {
		t.Errorf("a page change links on %q, want no link", got)
	}
	if _, set := skillVariables(e.resolver(), c, e.publicBase(c), kitVersion())[config.ReservedBaseURLVariable]; set {
		t.Errorf("${%s} is set, want it undefined: nothing names the dashboard",
			config.ReservedBaseURLVariable)
	}
}

// baseEngine is an engine with no company over b, its store and stream in the
// case's own directory.
//
// A NODE THAT SERVES NO API — a data, seats and workers node — because that is
// where a public listener stands alone: on an ingress node it needs `api.port`
// beside it, and a served API requires `api.external_url`, so the two
// addresses could not be told apart there with one of them unset. The links
// composed here are a worker's and a seat's as much as the API's: a skill is
// rendered into a seat's prompt and a page change's link into its wake.
func baseEngine(t *testing.T, b config.Bootstrap) *Engine {
	t.Helper()
	b.Node.Roles = []string{"data", "seats", "workers"}
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}
