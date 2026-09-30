package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A WAKE REACHES A RENAMED SEAT AND NAMES IT AS IT IS CALLED NOW.
//
// A record names every person by their seat's IDENTITY — the handle the seat
// was created under (people.go) — because the writer resolved it once and no
// applier may read a chart. So a change assigning work to the seat that was
// `cto` and answers to `chief` carries `cto`. The party registry answers that
// handle as well as the current one, so the wake is routed; and what the woken
// seat READS — the assignee line, and the change it was told about — names the
// handle it would type back, not one it has given up.
//
// Mutation: render the snapshot's assignee as it came, or render the deltas
// without resolving them, and the prompt tells `chief` about `cto`.
func TestAWakeReachesARenamedSeatAndNamesItAsItIsCalledNow(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Chief", DeclaredHandle: "chief", OriginHandle: "cto",
			FormerHandles: []string{"cto"}},
		{Name: "Bob", DeclaredHandle: "bob"},
	}}
	o.Normalize()
	parties := notify.NewRegistry(o, nil)
	record := parseRecord(&tracker.Notify{
		Kind:    tracker.ChangeAssignee,
		Excerpt: "over to you",
		Fields:  map[string]tracker.Delta{"assignee": {From: "bob", To: "cto"}},
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: tracker.StatusTodo, Assignee: "cto", PrevAssignee: "bob",
		},
	})

	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), parties)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var reached *notify.Routed
	for i := range routed {
		if routed[i].To.Handle == "cto" {
			reached = &routed[i]
		}
	}
	if reached == nil {
		t.Fatalf("the change reached %+v, and not the seat created as cto", routed)
	}
	if deltas := reached.Metadata[tracker.MetaDeltas]; !strings.Contains(deltas, "chief") ||
		strings.Contains(deltas, "cto") {
		t.Errorf("the change is described as %q, want the seat named chief", deltas)
	}

	prompt := tracker.Prompt{}.Build(reached.Inbound, parties)
	if !strings.Contains(prompt, "**Assignee:** chief") {
		t.Errorf("the prompt does not name the assignee as chief:\n%s", prompt)
	}
}
