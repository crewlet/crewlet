package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// toolAnswerText is what a box's harness reads of a call's answer: its text.
func toolAnswerText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// heldBoxCall makes a call to name from a box's session and hands back its
// answer once it returns, or its failure as a failed answer that names it.
func heldBoxCall(ctx context.Context, session *mcp.ClientSession, name string) <-chan *mcp.CallToolResult {
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "the call failed: " + err.Error()}}}
		}
		answered <- res
	}()
	return answered
}

// A SEAT HANDED ON WHILE A CALL OF ITS RUN IS HELD IS RELEASED AT ONCE. A node
// gives a seat's lease back once its release of the seat returns, and the
// release ends the bridge sessions of the seat's executor runs: an end that
// waited out a call still running — a delegate up to its call cap, a routed
// code write — held the seat's lease for as long, so no peer could take the
// seat over and the box's next calls waited for a holder. So the release
// returns while the call is held, and the box is answered at once that its
// call is still running, as a run that goes on, never told to end its turn.
//
// Mutation: let an opening's end answer no call still running (`false &&`
// before its cut), and the box is not answered while its call is held.
func TestASeatHandedOnWhileACallOfItsRunIsHeldIsReleasedAtOnce(t *testing.T) {
	t.Parallel()
	held := newHeldTool("delegate")
	rig := bridgedRig(t, "t-handed-on", held)
	if err := rig.engine.backends.Queue.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	run := rig.launch(t, nil)
	// FREED FIRST of everything the case built, so no call of it is still
	// held as the rest is torn down.
	t.Cleanup(func() { close(held.release) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	answered := heldBoxCall(ctx, run.session, "delegate")
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	id, err := run.engine.seatID(run.handle)
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		run.engine.releaseSeat(ctx, placement.Seat{ID: id, Handle: run.handle})
	}()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("the seat's release waited for a call of its run still running")
	}
	select {
	case res := <-answered:
		if said := toolAnswerText(res); res.IsError || !strings.Contains(said, "still running") ||
			strings.Contains(said, "End your turn") {
			t.Errorf("the box's held call was answered %v %q, want it told the call is still running "+
				"and not to end its turn", res.IsError, said)
		}
	case <-ctx.Done():
		t.Fatal("the box was not answered while its call was held")
	}
}
