package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// A DESIGN IS READ ONE BOARD AT A TIME: the outline names every artboard's file,
// title and frame in the canvas's order, fenced as text the design's author
// wrote, so an agent opens only the boards it changes.
func TestADesignIsOutlinedForAnAgent(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{"founder": founderPrincipal(iam.Apps...)}, nil)
	turn := asked(t)
	h := r.f.seed("founder", iam.ActorHuman, founder,
		[]artifacts.Share{shareWith("agent-cto", artifacts.RoleEditor)},
		[2]string{"canvas.json", `{"v":1,"title":"Launch","boards":{"hero.html":{"x":0,"y":0,"w":1440,` +
			`"h":900,"title":"Hero"},"mobile.html":{"x":1540,"y":0,"w":390,"h":844,"title":"Mobile"}},` +
			`"order":["hero.html","mobile.html"]}`},
		[2]string{"hero.html", `<!doctype html><h1>Ship</h1>`},
		[2]string{"mobile.html", `<!doctype html><h1>Go</h1>`})
	r.f.setKind(h.ID, artifacts.KindDesign)

	got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{"artifact": h.ID.String(), "view": "outline"})
	if got.Failed {
		t.Fatalf("the outline was refused: %s", got.Output)
	}
	out := decode(t, got)
	outline, _ := out["outline"].(map[string]any)
	text, _ := outline["text"].(string)
	for _, want := range []string{"Design: Launch", "Cover: hero.html",
		"1. hero.html: Hero at (0, 0), 1440 by 900", "2. mobile.html: Mobile at (1540, 0), 390 by 844",
		"Notes: 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("the outline lacks %q:\n%s", want, text)
		}
	}
	if outline["boards"] != float64(2) || out["file"] != nil {
		t.Errorf("the outline answered %v boards and a file %v", outline["boards"], out["file"])
	}
	seen, _ := fencesIn(t, turn.Boundary, got.Output)
	assertFences(t, seen, []wantFence{{"source=artifact_file trust=colleague authors=human:founder", "Mobile"}})
}
