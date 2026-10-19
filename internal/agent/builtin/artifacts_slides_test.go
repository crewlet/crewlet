package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// A DECK IS READ ONE SLIDE AT A TIME: the outline names every slide's file and
// title in order, fenced as text the deck's author wrote, so an agent reads
// only the slides it changes; a page has no outline.
func TestADeckIsOutlinedForAnAgent(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	turn := asked(t)
	h := r.f.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{"deck.json", `{"title":"Plan","order":["cover","next"],"sections":{"s":{"description":"Why","start":"cover"}}}`},
		[2]string{"slides/cover.html", `<section id="cover" style="background:#fff"><h1>Plan</h1><aside>Hi.</aside></section>`},
		[2]string{"slides/next.html", `<section id="next" hidden style="background:#fff"><h2>Next steps</h2></section>`})
	r.f.setKind(h.ID, artifacts.KindSlides)

	got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": h.ID.String(), "view": "outline"})
	if got.Failed {
		t.Fatalf("the outline was refused: %s", got.Output)
	}
	out := decode(t, got)
	outline, _ := out["outline"].(map[string]any)
	text, _ := outline["text"].(string)
	for _, want := range []string{"Deck: Plan", "Section from cover: Why",
		"1. cover (slides/cover.html): Plan [notes]", "2. next (slides/next.html): Next steps [hidden]"} {
		if !strings.Contains(text, want) {
			t.Errorf("the outline lacks %q:\n%s", want, text)
		}
	}
	if outline["slides"] != float64(2) || out["file"] != nil {
		t.Errorf("the outline answered %v slides and a file %v", outline["slides"], out["file"])
	}
	seen, _ := fencesIn(t, turn.Boundary, got.Output)
	assertFences(t, seen, []wantFence{{"source=artifact_file trust=colleague authors=human:founder", "Next steps"}})

	page := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	turn = turnFor(t, "agent-cto")
	if got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": page.ID.String(),
		"view": "outline"}); !got.Failed || !strings.Contains(got.Output, "no outline") {
		t.Errorf("a page's outline answered %s", got.Output)
	}
}
