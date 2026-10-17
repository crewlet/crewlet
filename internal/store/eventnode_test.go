package store_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A ROW NAMES THE NODE THAT PUBLISHED IT, not the node holding it.
//
// A data node keeps the events of a node without `data` in custody (ADR-0025),
// so its log holds rows it never published — and "which store answered" stopped
// saying where anything happened. The origin rides on every row instead, read
// back by every listing and by the one question that is ABOUT it: where a turn
// ran. Here one turn has a row this node published, two it keeps for a
// stateless host, and one a test harness wrote with no origin at all.
//
// Mutation: drop `node` from the insert, or from the listing columns, and every
// row reads back with no origin; drop the term that leaves out an empty origin from
// [store.EventLog.TurnNodes] and the harness's row names a node called "".
func TestARowNamesTheNodeThatPublishedItNotTheOneHoldingIt(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	ctx := t.Context()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	row := func(id, origin string, i int) store.EventRecord {
		return store.EventRecord{ID: id, Type: "agent_phase_started", Category: "task",
			Time: at.Add(time.Duration(i) * time.Second), Node: origin,
			Tags: map[string]string{"turn_id": "tn-1"}, Payload: json.RawMessage(`{}`)}
	}
	if err := log.Append(ctx, row("own", "node-a", 0)); err != nil {
		t.Fatal(err)
	}
	if err := log.WriteCustody(ctx, store.CustodyBatch{ID: "batch-1", Origin: "agent-1",
		Records: []store.EventRecord{row("kept-1", "agent-1", 1), row("kept-2", "agent-1", 2)},
	}, at); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(ctx, row("harness", "", 3)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"own": "node-a", "kept-1": "agent-1", "kept-2": "agent-1", "harness": ""}

	listed, err := log.List(ctx, store.ListQuery{TurnID: "tn-1"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := log.Turn(ctx, "tn-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for name, rows := range map[string][]store.EventRecord{"the listing": listed, "the turn": turn} {
		if len(rows) != len(want) {
			t.Fatalf("%s returned %d rows, want %d", name, len(rows), len(want))
		}
		for _, r := range rows {
			if r.Node != want[r.ID] {
				t.Errorf("%s reads %s as published by %q, want %q", name, r.ID, r.Node, want[r.ID])
			}
		}
	}
	one, err := log.ByID(ctx, "kept-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if one.Node != "agent-1" {
		t.Errorf("the kept row reads as published by %q, want agent-1 — its keeper only holds it", one.Node)
	}

	nodes, err := log.TurnNodes(ctx, "tn-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"agent-1", "node-a"}; !slices.Equal(nodes, want) {
		t.Errorf("the turn ran on %v, want %v — each node that published a row of it, "+
			"once, sorted, and no node called \"\"", nodes, want)
	}
	none, err := log.TurnNodes(ctx, "tn-absent", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Errorf("a turn this log holds none of ran on %#v, want an empty list", none)
	}
}
