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

func (filing) CallForTurn(_ context.Context, turn *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	turn.Written.Commit(types.CommittedItem)
	return tools.Result{Output: "filed"}, nil
}

// A BRIDGED CALL'S RECORD SAYS WHAT IT COMMITTED to the run's turn, which the
// run's collection settles the turn's outcome by; a read commits nothing.
//
// Mutation: record no Committed on the bridge's Call, and the filing call's
// record is empty.
func TestABridgedCallRecordsWhatItCommitted(t *testing.T) {
	t.Parallel()
	g := newGated(t, filing{})
	session := dial(t, g.url)
	callTool(t, session, "read_page", map[string]any{"text": "one"})
	callTool(t, session, "create_work_item", map[string]any{})
	callTool(t, session, "create_work_item", map[string]any{})

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
}
