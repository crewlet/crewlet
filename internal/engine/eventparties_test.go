package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ENGINE FILES AN EVENT UNDER EVERY AGENT IT NAMES, BY AGENT ID.
//
// The event log's related-agent index is keyed on agent ids, and an event
// names its participants by HANDLE — an A2A channel's two ends, the seat a
// vendor delivery was addressed to. The store cannot turn one into the other:
// an id is derived from the company's name and the handle a seat was created
// under, which only a process holding the chart knows. So the engine installs
// the resolution, off its live epoch, and a record appended to this node's log
// by any writer is filed under the agent it reaches.
//
// A human seat has no agent id and a stranger none either; neither is a party.
//
// Mutation: drop the SetEventSeats call in engine.New and the delivery is
// filed under nobody.
func TestTheEngineFilesAnEventUnderTheAgentsItNames(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	ctx := t.Context()
	org := e.Company().Org
	cto, ok := org.AgentIDFor(org.AgentSeatByHandle("cto"))
	if !ok {
		t.Fatal("the fixture's cto seat has no agent id")
	}
	log := e.Backends().Store.Events()
	at := time.Now().UTC().Add(-time.Minute)
	for _, rec := range []store.EventRecord{
		{ID: "to-cto", Type: "webhook:push", Source: "github", Time: at,
			Category: "webhook", Actor: "github",
			Tags: map[string]string{"recipient": "cto"}},
		{ID: "to-founder", Type: "webhook:push", Source: "github",
			Time: at.Add(time.Second), Category: "webhook", Actor: "github",
			Tags: map[string]string{"recipient": "founder"}},
	} {
		if err := log.Append(ctx, rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	got, err := log.List(ctx, store.ListQuery{RelatedAgent: cto.String()})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed []string
	for _, row := range got {
		listed = append(listed, row.ID)
	}
	if !slices.Contains(listed, "to-cto") || slices.Contains(listed, "to-founder") {
		t.Errorf("the cto's related events are %v, want the delivery addressed "+
			"to it and not the founder's", listed)
	}
}
