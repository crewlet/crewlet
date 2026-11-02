package setupapi

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A DRAFT IS JUDGED AS THE VENDOR JUDGES IT: a lower-case key is within the
// connection's reach, and a set of Confluence permissions without View, which
// every other one needs, is refused naming it.
func TestADraftIsJudgedAsTheVendorJudgesIt(t *testing.T) {
	t.Parallel()
	jira := seataccess.Options{Lane: seataccess.LaneJira, Live: true, Scope: []string{"ENG"},
		Levels: []seataccess.Level{{ID: "10101", Name: "Developers"}}}
	if refusal := notOffered(integration.KindAtlassian, seataccess.LaneJira, jira, "",
		seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"eng"}}); refusal != nil {
		t.Fatalf("a lower-case key was refused: %v", refusal)
	}
	if refusal := notOffered(integration.KindAtlassian, seataccess.LaneJira, jira, "",
		seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"web"}}); refusal == nil {
		t.Fatal("a key outside reach was accepted")
	}
	conf := seataccess.Options{Lane: seataccess.LaneConfluence, Live: true, ScopeOpen: true,
		Multiple: true, RequiredLevel: "read:space", Levels: []seataccess.Level{
			{ID: "", Name: "Product access only"}, {ID: "read:space", Name: "View"},
			{ID: "create:page", Name: "Add pages"},
		}}
	refusal := notOffered(integration.KindAtlassian, seataccess.LaneConfluence, conf, "",
		seataccess.Desired{Enabled: true, Level: "create:page"})
	if refusal == nil || refusal["required"] != "read:space" ||
		!strings.Contains(refusal["detail"].(string), "without View") {
		t.Fatalf("a set without View = %v, want it refused naming View", refusal)
	}
	for _, level := range []string{"create:page read:space", ""} {
		if refusal := notOffered(integration.KindAtlassian, seataccess.LaneConfluence, conf, "",
			seataccess.Desired{Enabled: true, Level: level}); refusal != nil {
			t.Fatalf("%q was refused: %v", level, refusal)
		}
	}
}
