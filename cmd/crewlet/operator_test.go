package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE OPERATOR'S SURFACES ARE HANDED THE ESTATE ROUTER'S SEAMS, never this
// node's own copy: the board and every other REST read, a project's file rows,
// the purge, and every seam of the operator's own MCP. Read straight off the
// node's copy, a data node whose copy was out of service — wrong rather than
// behind, or its file shut — sent its seats' calls to a peer's copy and went
// on answering its operator from the one it had stopped serving. The router's
// facade is the only type that routes, so a seam of any other type here is a
// surface reading one node's copy whatever its state.
func TestTheOperatorsSurfacesAreHandedTheRouter(t *testing.T) {
	t.Parallel()
	e := testEngine(t)

	if _, ok := nativeWork(e).(estate.Work); !ok {
		t.Errorf("the REST reads are handed %T, want the router's estate.Work", nativeWork(e))
	}
	if _, ok := nativeFiles(e).(estate.Work); !ok {
		t.Errorf("the file rows are handed %T, want the router's estate.Work", nativeFiles(e))
	}
	if _, ok := nativePages(e).(estate.Pages); !ok {
		t.Errorf("the knowledge reads are handed %T, want the router's estate.Pages", nativePages(e))
	}
	if _, ok := nativeWorkSearch(e).(estate.Work); !ok {
		t.Errorf("the ranked search is handed %T, want the router's estate.Work", nativeWorkSearch(e))
	}
	if _, ok := nativePurger(e).(purgeAdapter); !ok {
		t.Errorf("the purge is handed %T, want the router's writer", nativePurger(e))
	}

	opts := api.EngineOperatorOptions(e)
	work := opts.Work
	if work.Reader == nil || opts.Pages.Reader == nil {
		t.Fatalf("the premise: a company on the native tracker and knowledge base hands "+
			"the operator's MCP neither (work %v, pages %v)", work.Reader, opts.Pages.Reader)
	}
	operator := builtin.Actor{Handle: "founder", Kind: tracker.AuthorOperator, OperatorID: "founder"}
	for _, seam := range []struct {
		name string
		got  any
	}{
		{"Reader", work.Reader},
		{"Files", work.Files},
		{"Inbox", work.Inbox},
		{"Writer", work.Writer(operator)},
		{"Dependencies", work.Dependencies(operator)},
		{"Merges", work.Merges(operator)},
		{"Moves", work.Moves(operator)},
		{"FileWriter", work.FileWriter(operator)},
		{"ViewWriter", work.ViewWriter(operator)},
		{"CatalogueWriter", work.CatalogueWriter(operator)},
		{"PersonWriter", work.PersonWriter(operator)},
		{"TrashWriter", work.TrashWriter(operator)},
		{"Placer", work.Placer(operator)},
		{"ProjectWriter", work.ProjectWriter(operator)},
		{"Pages.Reader", opts.Pages.Reader},
		{"Pages.Writer", opts.Pages.Writer},
	} {
		switch seam.got.(type) {
		case estate.Work, estate.WorkWriter, estate.Pages:
		default:
			t.Errorf("the operator's MCP seam %s is %T, want the router's", seam.name, seam.got)
		}
	}
}

// A PERSON IS BOUND THROUGH THE NODE'S OWN CHAIN ON EVERY SURFACE `crewlet run`
// SERVES. A token's binding to a person may be a `${VAR}`, and the one here is
// set in no process — only in the environment the engine was handed — so a
// surface wired to read the process environment instead leaves the founder
// unbound: the operator surface writes as the bare token and reads their rows
// under one name, and the dashboard answers the token as nobody. Both are wired
// where an engine meets the API — [api.EngineOperatorOptions] and [serveAPI]'s
// sources — so they are held here, where a test boots an engine and serves it.
// One engine, because both read the same chart.
func TestEverySurfaceBindsAPersonThroughTheNodesOwnChain(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_CLI_TEST_BOUND_FOUNDER"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	company, err := config.ParseCompany([]byte(companyYAML + `  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := testEngineWith(t, engine.Options{Company: company,
		Environment: config.MapSource{variable: "ops-founder"}})

	// THE OPERATOR'S SURFACE: a write through the founder's token is the
	// founder's own, and a read about the founder is by both of their names.
	t.Run("the_operator_surface", func(t *testing.T) {
		t.Parallel()
		work := api.EngineOperatorOptions(e).Work
		actor, err := work.Actor(auth.WithOperator(t.Context(), "ops-founder"), nil)
		if err != nil {
			t.Fatalf("actor: %v", err)
		}
		if actor.Seat != "founder" {
			t.Errorf("the founder's token writes as seat %q, want the founder the "+
				"handed environment binds it to", actor.Seat)
		}
		if got := work.Party("founder"); got.OperatorID != "ops-founder" {
			t.Errorf("the founder's party is %+v, want the credential the handed "+
				"environment binds to them", got)
		}
	})

	// THE DASHBOARD'S READS, through the API `crewlet run` serves: the viewer
	// is the person the token is bound to.
	t.Run("the_dashboard", func(t *testing.T) {
		t.Parallel()
		boot := bootstrapFor(t, 0)
		boot.API.Port = freePort(t)
		boot.API.Auth.Tokens = []config.APIToken{{ID: "ops-founder", Token: "founder-test-token"}}
		surface, err := serveNode(t, boot, e)
		if err != nil {
			t.Fatalf("serveAPI: %v", err)
		}
		t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/viewer", nil)
		req.Header.Set("Authorization", "Bearer founder-test-token")
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /viewer = %d: %s", rec.Code, rec.Body)
		}
		var viewer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &viewer); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if viewer["handle"] != "founder" {
			t.Errorf("the founder's token views the dashboard as %v, want the founder "+
				"the handed environment binds it to", viewer["handle"])
		}
	})
}
