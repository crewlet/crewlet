package stream_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD READS AN INBOX FRAME AS THE ENGINE SENDS IT: every key the
// engine puts on an `inbox_changed` frame's data is one the dashboard's
// `InboxChange` declares, and every key it declares is one the engine sends,
// always.
//
// The frame is read by key: a key the engine sends that the client does not
// declare is one no screen can tell apart from noise — the `source` that says
// which domain's half of an inbox moved was exactly that the moment it was
// added — and a key the client declares that the engine never sends is one
// every screen trusting the type reads as undefined. Every key is sent on
// every frame, a re-read's included, so the client declares each REQUIRED.
//
// Mutation: drop or rename a field of [stream.InboxChange], or a member of the
// dashboard's InboxChange, and this fails.
func TestTheDashboardReadsAnInboxFrameAsTheEngineSendsIt(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(stream.InboxChange{})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	members, err := clientsource.Interface(clientsource.Tree(t), "InboxChange")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one.
		t.Fatal(err)
	}
	var declared []string
	for _, m := range members {
		declared = append(declared, m.Name)
		if m.Optional {
			t.Errorf("the dashboard declares %s optional, and the engine sends it "+
				"on every frame", m.Name)
		}
	}
	slices.Sort(declared)
	if want := slices.Sorted(maps.Keys(sent)); !slices.Equal(declared, want) {
		t.Errorf("the dashboard's InboxChange declares %v; the engine sends %v",
			declared, want)
	}
}
