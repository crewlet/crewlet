package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/iam"
)

const toolBoard = `{"v":1,"title":"Delivery",
"params":[{"key":"scope","label":"Whose","default":"mine","options":[{"value":"mine","label":"Mine"},{"value":"all","label":"All"}]}],
"datasets":[{"id":"work","title":"Open work","source":"engine","engine":{"name":"tracker.items","params":{"scope":{"param":"scope"}}}},
{"id":"deals","title":"Pipeline","source":"store","store":{"collection":"deals"},"columns":[{"key":"amount","type":"number"}]}],
"widgets":[{"id":"mine","type":"table","title":"My work","dataset":"work","columns":["key","title"],"w":12},
{"id":"total","type":"stat","title":"Pipeline","dataset":"deals","value":"amount","agg":"sum","w":4}]}`

// A BOARD IS OUTLINED FOR AN AGENT WITHOUT A FIGURE: its filters, datasets
// with their columns and widgets, fenced as text the board's author wrote;
// what any figure shows is each viewer's own.
func TestABoardIsOutlinedForAnAgent(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	turn := asked(t)
	h := r.f.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{"dashboard.json", toolBoard})
	r.f.setKind(h.ID, artifacts.KindDashboard)

	got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": h.ID.String(), "view": "outline"})
	if got.Failed {
		t.Fatalf("the outline was refused: %s", got.Output)
	}
	outline, _ := decode(t, got)["outline"].(map[string]any)
	text, _ := outline["text"].(string)
	for _, want := range []string{"Dashboard: Delivery", `Filter scope (Whose): mine | all, default "mine"`,
		"Dataset work (engine, tracker.items): Open work [key:text", "Dataset deals (store, collection deals)",
		"Widget total (stat, w 4, dataset deals): Pipeline"} {
		if !strings.Contains(text, want) {
			t.Errorf("the outline lacks %q:\n%s", want, text)
		}
	}
	seen, _ := fencesIn(t, turn.Boundary, got.Output)
	assertFences(t, seen, []wantFence{{"source=artifact_file trust=colleague authors=human:founder", "Pipeline"}})
}

// THE CATALOGUE IS THE ENGINE'S, whole or one source, and an unknown name is
// refused naming every source there is.
func TestTheDashboardSourceCatalogueIsListed(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn := turnFor(t, "agent-cto")
	got := r.call(t, turn, builtin.DashboardSourcesTool, map[string]any{})
	if got.Failed {
		t.Fatalf("the catalogue was refused: %s", got.Output)
	}
	sources, _ := decode(t, got)["sources"].([]any)
	if len(sources) != len(dashspec.Catalogue()) {
		t.Errorf("the tool listed %d sources, the catalogue holds %d", len(sources), len(dashspec.Catalogue()))
	}
	one := decode(t, r.call(t, turn, builtin.DashboardSourcesTool, map[string]any{"name": "spend.daily"}))
	if s, _ := one["sources"].([]any); len(s) != 1 || s[0].(map[string]any)["name"] != "spend.daily" {
		t.Errorf("one source answered %v", one)
	}
	if bad := r.call(t, turn, builtin.DashboardSourcesTool, map[string]any{"name": "crm.deals"}); !bad.Failed ||
		!strings.Contains(bad.Output, "tracker.items") {
		t.Errorf("an unknown source answered %s", bad.Output)
	}
}
