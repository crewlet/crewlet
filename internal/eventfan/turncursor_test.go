package eventfan_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// TWO TURNS THAT START AT ONE MICROSECOND ARE BOTH ON THE WALK, once each and
// in one order, whichever nodes hold them and whichever side of a page's cut
// they fall.
//
// A webhook that wakes two seats starts two turns at one instant, and the two
// seats are often held by two nodes. The cursor was the instant alone and every
// node resumed strictly below it, so a page cut between two such turns sent the
// walk past the second for good. It is the instant and the turn id now, in the
// order every node's page and the merge share — newest start first, the higher
// id first within one — so every page resumes after exactly the turn the last
// one ended on.
//
// Mutation: drop the id from the cursor the merge hands back, from the merge's
// cut at the cursor or at a full node's last turn, or from the wire, and a walk
// of pages of one, two or three loses a turn at the shared instant or lists one
// twice.
func TestTwoTurnsAtOneMicrosecondAreBothOnTheWalkAcrossNodes(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := newNode(t, broker, "node-a"), newNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, w := range []struct {
		n    node
		turn string
		at   time.Time
	}{
		{a, "t-a", at}, {b, "t-b", at}, {a, "t-c", at}, {b, "t-d", at},
		{a, "t-early", at.Add(-time.Hour)},
	} {
		phaseOn(t, w.n, w.turn+"-p", w.turn, w.at, 10, "m")
	}
	want := []string{"t-d", "t-c", "t-b", "t-a", "t-early"}
	for _, self := range []node{a, b} {
		for _, limit := range []int{1, 2, 3} {
			fan := fanFrom(self, "node-a", "node-b")
			fan.Clock = func() time.Time { return at.Add(time.Hour) }
			var order []string
			q := store.TurnQuery{SinceDays: 1, Limit: limit}
			for range 20 {
				page, coverage, err := fan.Turns(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				if !coverage.Complete {
					t.Fatalf("coverage %+v", coverage)
				}
				order = append(order, turnIDs(page)...)
				if page.Next == nil {
					break
				}
				q.Before = page.Next
			}
			if !slices.Equal(order, want) {
				t.Errorf("asked from %s in pages of %d, the walk listed %v, want %v — each once, "+
					"the four at one instant by id", self.id, limit, order, want)
			}
		}
	}
}

// A NODE'S PAGE CUT SHORT STOPS THE MERGE AT ITS LAST TURN, start and id.
//
// A reply too large for the transport is cut and says it holds more, so the
// turns it did not send sit below its last one — and, at that turn's own
// instant, below its id. Another node's turn at that instant with a lower id
// can therefore not be placed on this page: one the cut node did not send may
// lie between the two, and the cursor after the placed turn would be past it.
// So the merge stops at the cut node's last turn by the whole key.
//
// Mutation: stop the merge at the cut node's last START alone, and the turn
// between the two is on no page.
func TestACutPageStopsTheMergeAtItsLastTurnByStartAndID(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a := newNode(t, broker, "node-a")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	phaseOn(t, a, "t-d-p", "t-d", at, 10, "m")
	phaseOn(t, a, "t-early-p", "t-early", at.Add(-time.Minute), 10, "m")
	// node-x holds t-e and t-dd at the same instant and sends one turn a
	// page, saying it holds more — a reply cut to fit the transport.
	held := []store.TurnPartial{
		{TurnID: "t-e", AgentRole: "Lead", StartedAt: at, EndedAt: at, Phases: 1, TotalTokens: 10},
		{TurnID: "t-dd", AgentRole: "Lead", StartedAt: at, EndedAt: at, Phases: 1, TotalTokens: 10},
	}
	q := client(t, broker)
	stop, err := q.Serve(t.Context(), eventfan.Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req struct {
			Version  int             `json:"version"`
			Question string          `json:"question"`
			Params   json.RawMessage `json:"params"`
			TurnIDs  []string        `json:"turn_ids"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		var p struct {
			Before   time.Time `json:"before"`
			BeforeID string    `json:"before_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err
		}
		part := map[string]any{"turns": []store.TurnPartial{}, "full": false}
		switch {
		case req.Question != "turns":
			return json.Marshal(map[string]any{"version": req.Version, "node": "node-x",
				"error": "node-x answers turns alone"})
		case len(req.TurnIDs) > 0:
			var shares []store.TurnPartial
			for _, h := range held {
				if slices.Contains(req.TurnIDs, h.TurnID) {
					shares = append(shares, h)
				}
			}
			part["turns"] = shares
		default:
			var below []store.TurnPartial
			for _, h := range held {
				cursor := store.TurnCursor{Start: p.Before, TurnID: p.BeforeID}
				if p.Before.IsZero() || cursor.Before(h.StartedAt, h.TurnID) {
					below = append(below, h)
				}
			}
			if len(below) > 0 {
				part["turns"], part["full"] = below[:1], len(below) > 1
			}
		}
		answer, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"version": req.Version, "node": "node-x", "answer": json.RawMessage(answer)})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	fan := fanFrom(a, "node-a", "node-x")
	fan.Clock = func() time.Time { return at.Add(time.Hour) }
	var order []string
	query := store.TurnQuery{SinceDays: 1, Limit: 10}
	for range 10 {
		page, coverage, err := fan.Turns(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		if !coverage.Complete {
			t.Fatalf("coverage %+v", coverage)
		}
		order = append(order, turnIDs(page)...)
		if page.Next == nil {
			break
		}
		query.Before = page.Next
	}
	if want := []string{"t-e", "t-dd", "t-d", "t-early"}; !slices.Equal(order, want) {
		t.Errorf("the walk listed %v, want %v — node-x's unsent turn between its last and "+
			"node-a's at the same instant on a page of its own", order, want)
	}
}
