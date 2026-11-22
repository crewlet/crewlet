package mcpbridge_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tools"
)

// filing is a seat tool that commits a work item to the turn it is called for.
type filing struct{}

func (filing) Name() string               { return "create_work_item" }
func (filing) Description() string        { return "files a work item" }
func (filing) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (filing) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "unreachable: a seat tool is called for its turn", Failed: true}, nil
}

func (filing) CallForTurn(_ context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	turn.Written.Commit(types.CommittedItem)
	id, _ := args["id"].(string)
	turn.Written.Add(types.WorkItem{Backend: "worklet", ID: id})
	return tools.Result{Output: "filed"}, nil
}

// A BRIDGED CALL'S RECORD SAYS WHAT IT COMMITTED to the run's turn, which the
// run's collection settles the turn's outcome by, AND WHICH RECORDS IT WROTE,
// which the run's router reads (ADR-0167): each call the records its own writes
// added; a read commits and writes nothing.
//
// Mutation: record no Committed or no Wrote on the bridge's Call, or every
// record the turn holds rather than the call's own, and a call's record is off.
func TestABridgedCallRecordsWhatItCommitted(t *testing.T) {
	t.Parallel()
	g := newGated(t, filing{})
	session := dial(t, g.url)
	callTool(t, session, "read_page", map[string]any{"text": "one"})
	callTool(t, session, "create_work_item", map[string]any{"id": "task-1"})
	callTool(t, session, "create_work_item", map[string]any{"id": "task-2"})

	rows := g.ledger.rows()
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	if len(rows[0].Committed) != 0 {
		t.Errorf("the read is recorded as committing %v", rows[0].Committed)
	}
	if !slices.Equal(rows[1].Committed, []types.CommittedChange{types.CommittedItem}) {
		t.Errorf("the filing call is recorded as committing %v, want the item", rows[1].Committed)
	}
	// THE KIND ONCE PER TURN: the second filing adds no kind the set lacked.
	if len(rows[2].Committed) != 0 {
		t.Errorf("the second filing is recorded as committing %v", rows[2].Committed)
	}
	if len(rows[0].Wrote) != 0 {
		t.Errorf("the read is recorded as writing %v", rows[0].Wrote)
	}
	for i, id := range []string{"task-1", "task-2"} {
		want := []types.WrittenRecord{types.ItemWritten(types.WorkItem{Backend: "worklet", ID: id})}
		if got := rows[i+1].Wrote; !slices.Equal(got, want) {
			t.Errorf("filing %s is recorded as writing %v, want %v", id, got, want)
		}
	}
}
