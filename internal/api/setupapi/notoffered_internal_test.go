package setupapi

import (
	"encoding/json"
	"maps"
	"slices"
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
		Scoped: true, ScopeLive: true, Levels: []seataccess.Level{{ID: "10101", Name: "Developers"}}}
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
	if refusal == nil || refusal.Required != "read:space" || refusal.Field != seataccess.FieldLevel ||
		!strings.Contains(refusal.Detail, "without View") {
		t.Fatalf("a set without View = %v, want it refused naming View", refusal)
	}
	for _, level := range []string{"create:page read:space", ""} {
		if refusal := notOffered(integration.KindAtlassian, seataccess.LaneConfluence, conf, "",
			seataccess.Desired{Enabled: true, Level: level}); refusal != nil {
			t.Fatalf("%q was refused: %v", level, refusal)
		}
	}
}

// A SCOPE IS JUDGED ON WHAT WAS READ, and an empty one is refused where the lane
// requires one whatever was read. Mutation: judge an unread list, and every
// entry is "outside reach" while the vendor is down.
func TestAScopeIsJudgedOnWhatWasRead(t *testing.T) {
	t.Parallel()
	unread := seataccess.Options{Lane: seataccess.LaneJira, Scoped: true,
		Levels: []seataccess.Level{{ID: "10101", Name: "Developers"}}}
	if refusal := notOffered(integration.KindAtlassian, seataccess.LaneJira, unread, "",
		seataccess.Desired{Enabled: true, Level: "10101", Scope: []string{"web"}}); refusal != nil {
		t.Fatalf("an entry was judged against a list nobody read: %v", refusal)
	}
	required := unread
	required.ScopeRequired = true
	refusal := notOffered(integration.KindAtlassian, seataccess.LaneJira, required, "",
		seataccess.Desired{Enabled: true, Level: "10101"})
	if refusal == nil || refusal.Field != seataccess.FieldScope || !strings.Contains(refusal.Detail, "can reach nothing") {
		t.Fatalf("an agent on with no scope = %v, want it refused", refusal)
	}
	for name, want := range map[string]seataccess.Desired{
		"off":           {Level: "10101"},
		"with an entry": {Enabled: true, Level: "10101", Scope: []string{"ENG"}},
	} {
		if refusal := notOffered(integration.KindAtlassian, seataccess.LaneJira, required, "", want); refusal != nil {
			t.Errorf("%s: refused: %v", name, refusal)
		}
	}
}

// A REFUSAL'S FIELDS ON THE WIRE ARE THE TYPE THE DASHBOARD'S GATE HOLDS: every
// one under its JSON name, the optional ones only when set. Mutation: rename a
// key in refusalFields, and the body no longer matches the gated type.
func TestARefusalsFieldsAreTheGatedType(t *testing.T) {
	t.Parallel()
	full := seataccess.OfferRefusal{Seat: "sre", Lane: seataccess.LaneJira, Field: seataccess.FieldScope,
		Detail: "d", Offered: []string{"ENG"}, Required: "read:space"}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := refusalFields(full)
	if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))) {
		t.Fatalf("fields %v, want the type's %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	bare := refusalFields(seataccess.OfferRefusal{Seat: "sre", Lane: seataccess.LaneJira,
		Field: seataccess.FieldLevel, Detail: "d"})
	if _, ok := bare["offered"]; ok {
		t.Error("an unset offered list was written")
	}
	if _, ok := bare["required"]; ok {
		t.Error("an unset required level was written")
	}
}
