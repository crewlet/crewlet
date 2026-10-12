package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
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

	native := api.EngineReads(e)()
	if _, ok := native.Work.(estate.Work); !ok {
		t.Errorf("the REST reads are handed %T, want the router's estate.Work", native.Work)
	}
	if _, ok := native.Files.(estate.Work); !ok {
		t.Errorf("the file rows are handed %T, want the router's estate.Work", native.Files)
	}
	if _, ok := native.Pages.(estate.Pages); !ok {
		t.Errorf("the knowledge reads are handed %T, want the router's estate.Pages", native.Pages)
	}
	if _, ok := native.Backlinks.(estate.Pages); !ok {
		t.Errorf("the backlinks are handed %T, want the router's estate.Pages", native.Backlinks)
	}
	if _, ok := native.WorkSearch.(estate.Work); !ok {
		t.Errorf("the ranked search is handed %T, want the router's estate.Work", native.WorkSearch)
	}
	// THE ROUTES' WRITES go through the router by construction — the purge's
	// and the file bytes' adapters hold the router's writer facade
	// (estate.WorkWriter, a struct over the router) and can hold nothing else
	// — so what is held here is that a node running the native tracker serves
	// them, and its eviction gate, at all.
	routes := api.EngineNative(e)
	if _, ok := routes.Purger(); !ok {
		t.Error("the purge is not served on a node running the native tracker")
	}
	if _, ok := routes.FileReads(); !ok {
		t.Error("a file's download is not served on a node running the native tracker")
	}
	if _, ok := routes.FileWrites(); !ok {
		t.Error("a file's upload is not served on a node running the native tracker")
	}
	if _, ok := routes.Nodes(); !ok {
		t.Error("the eviction gate is not served on a node running the state log")
	}

	opts := api.EngineOperatorOptions(e)
	work := opts.Work
	if work.Reader == nil || opts.Pages.Reader == nil {
		t.Fatalf("the premise: a company on the native tracker and knowledge base hands "+
			"the operator's MCP neither (work %v, pages %v)", work.Reader, opts.Pages.Reader)
	}
	founder := builtin.Actor{Handle: "founder", Kind: tracker.AuthorOperator, OperatorID: "founder"}
	for _, seam := range []struct {
		name string
		got  any
	}{
		{"Reader", work.Reader},
		{"Files", work.Files},
		{"Inbox", work.Inbox},
		{"Search", work.Search},
		{"Writer", work.Writer(founder)},
		{"Dependencies", work.Dependencies(founder)},
		{"Merges", work.Merges(founder)},
		{"Moves", work.Moves(founder)},
		{"FileWriter", work.FileWriter(founder)},
		{"ViewWriter", work.ViewWriter(founder)},
		{"CatalogueWriter", work.CatalogueWriter(founder)},
		{"PersonWriter", work.PersonWriter(founder)},
		{"TrashWriter", work.TrashWriter(founder)},
		{"Placer", work.Placer(founder)},
		{"ProjectWriter", work.ProjectWriter(founder)},
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

// A NODE IN A MAINTENANCE MODE SERVES A FILE'S BYTES AND WRITES NONE. The mode
// hands no writer to any surface, so the upload, the removal and the purge are
// absent there; a download is a READ, and a maintenance-mode node serves its
// reads — the file listing beside it among them. Gated on the writer, the
// download answered `no_route` for a file the same node had just listed.
//
// A data node, which is what one engine alone can be; a node without `data`
// in the mode is held to the same by internal/e2e's
// TestAStatelessNodeInAMaintenanceModeServesAFileAndTakesNone, beside the data
// member it needs to join.
func TestAMaintenanceNodeServesAFilesBytesAndWritesNone(t *testing.T) {
	t.Parallel()
	e := testEngineWith(t, engine.Options{Mode: statelog.ModeMaintenance})
	if api.EngineReads(e)().Files == nil {
		t.Fatal("the premise: a maintenance-mode node lists no project's files")
	}
	routes := api.EngineNative(e)
	if _, ok := routes.FileReads(); !ok {
		t.Error("a node in a maintenance mode does not serve a file's bytes, " +
			"which it lists")
	}
	if _, ok := routes.FileWrites(); ok {
		t.Error("a node in a maintenance mode serves a file's upload, a write")
	}
	if _, ok := routes.Purger(); ok {
		t.Error("a node in a maintenance mode serves the purge, a write")
	}
}

// A NODE IN A MAINTENANCE MODE OFFERS ITS OPERATOR EVERY READ A NODE IN SERVICE
// OFFERS, AND OF THE REST ONLY THE GESTURES THAT APPEND TO NO STATE LOG. Both
// transports serve this one catalogue, the dashboard's act transport and an
// operator's own assistant over MCP, so this is what a person can do through
// either during a capacity window:
//
//   - every READ, as the REST routes and the dashboard serve this node's reads
//     in the mode. Gated on the writer as well, the catalogue offered no work-
//     item or page tool at all, so an assistant could not read the board the
//     dashboard beside it was drawing.
//   - no WRITE TO A LOG. The work-item and page writers are handed only behind
//     the engine's one writer gate, which hands none out in a mode that
//     publishes nothing, whatever the node holds.
//   - what moves no log, which the mode has no reason to withhold: a parked
//     run's answer (a message on the seat's own mailbox, read once a node
//     holds the seat again), a seat's pause and resume (one coordination
//     record), a note to a running turn (an ephemeral scatter), and an answer
//     from the company's knowledge (a charge to the coordination store's token
//     counters). The retention guide said the endpoint offered the search
//     alone, and sent an operator looking mid-window for a way to pause a seat
//     that was there all along.
//
// THE READS ARE A NODE IN SERVICE'S, not a list kept here, so a read tool the
// catalogue gains is held to this without anybody adding it. The writes are
// the EXACT set, both ways: one appearing here is a new gesture somebody must
// judge against the window, and one going missing is a gesture the guide
// promises and the node no longer serves.
func TestAMaintenanceNodeOffersItsOperatorEveryReadAndNoLogWrite(t *testing.T) {
	t.Parallel()
	inService := operatorCatalogue(t, testEngine(t))
	e := testEngineWith(t, engine.Options{Mode: statelog.ModeMaintenance})
	opts := api.EngineOperatorOptions(e)
	if opts.Work.Reader == nil || opts.Pages.Reader == nil {
		t.Errorf("a node in a maintenance mode does not hand its operator the reads it "+
			"serves (tracker %v, knowledge base %v)",
			opts.Work.Reader != nil, opts.Pages.Reader != nil)
	}
	if opts.Work.Writer != nil || opts.Work.FileWriter != nil || opts.Pages.Writer != nil {
		t.Error("a node in a maintenance mode hands its operator a writer")
	}
	surface := operatorCatalogue(t, e)

	want := operatorReads(inService)
	for _, read := range []string{builtin.ListWorkItemsTool, builtin.GetPageTool,
		tracker.ReadProjectFileTool} {
		if !slices.Contains(want, read) {
			t.Fatalf("the premise: a node in service offers its operator %v, without %s",
				want, read)
		}
	}
	if got := operatorReads(surface); !slices.Equal(got, want) {
		t.Errorf("a node in a maintenance mode offers its operator the reads %v, "+
			"want a node in service's %v", got, want)
	}
	acts := slices.Sorted(slices.Values(surface.Acts()))
	noLog := slices.Sorted(slices.Values([]string{
		builtin.AnswerKnowledgeTool, builtin.AnswerRunTool, builtin.PauseSeatTool,
		builtin.ResumeSeatTool, builtin.SteerTurnTool,
	}))
	if !slices.Equal(acts, noLog) {
		t.Errorf("a node in a maintenance mode offers its operator the writes %v, "+
			"want exactly those that append to no log %v", acts, noLog)
	}
}

// AN OPERATOR'S WRITE IS JUDGED AGAINST THE CONTAINERS THE CURRENT COMPANY
// RESERVES. The skills container and the org root are Tier B, and the
// operator's catalogue is not rebuilt for a revision that moves one of them —
// so a list evaluated when it was built went on refusing the container the
// company moved away from and wrote into the one it moved to, where pages are
// excluded from every search; on a node that booted with no company it
// reserved nothing at all. Pressed through the act transport `crewlet run`
// serves, so the catalogue judged is the one a person's dashboard reaches.
func TestAnOperatorWriteIsJudgedAgainstTheCurrentCompanysReservedContainers(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token")
	n.reconcile()
	founder := companyYAML + `  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ops
`
	if err := n.settle(n.put(founder)); err != nil {
		t.Fatalf("the company: %v", err)
	}
	refused := func(container string) (bool, string) {
		t.Helper()
		status, body := n.act(t, builtin.WritePageTool, map[string]any{
			"title": "a page in " + container, "body": "x", "container": container,
		})
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &answer)
		return status == http.StatusForbidden && answer.Error == "forbidden" &&
			strings.Contains(body, "reserved container"), body
	}
	if ok, body := refused(config.DefaultSkillsContainer); !ok {
		t.Fatalf("the premise: a page into the default skills container %s answered %s",
			config.DefaultSkillsContainer, body)
	}

	// A REVISION MOVING THE SKILLS CONTAINER, which changes nothing the
	// catalogue is built from.
	if err := n.settle(n.put(founder + "knowledge:\n  skills_container: SKILLS\n")); err != nil {
		t.Fatalf("the revision moving the skills container: %v", err)
	}
	if ok, body := refused("SKILLS"); !ok {
		t.Errorf("a page into the skills container the company moved to answered %s, "+
			"want 403 forbidden naming a reserved container", body)
	}
}

// operatorCatalogue is the catalogue the operator surface over e serves now —
// the one `crewlet run` builds ([api.NewEngineOperators]).
func operatorCatalogue(t *testing.T, e *engine.Engine) *operator.Server {
	t.Helper()
	operators, err := api.NewEngineOperators(t.Context(), e)
	if err != nil {
		t.Fatalf("the operator surface: %v", err)
	}
	t.Cleanup(func() { operators.Close(context.Background()) })
	server, ok := operators.Operator()
	if !ok {
		t.Fatal("the operator surface serves no catalogue beside an engine")
	}
	return server
}

// operatorReads is every tool an operator surface offers that its act
// transport does not — its proven reads, by the catalogue's own hint — sorted.
func operatorReads(surface interface {
	Tools() []string
	Acts() []string
},
) []string {
	acts := surface.Acts()
	var out []string
	for _, name := range surface.Tools() {
		if !slices.Contains(acts, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
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
