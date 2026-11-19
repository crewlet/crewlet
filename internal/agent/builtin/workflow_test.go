package builtin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fixedPosture is a live projection that always answers one posture.
type fixedPosture worklet.Posture

func (p fixedPosture) SeatPostures() worklet.Posture { return worklet.Posture(p) }

// flowReader is a real tracker reader over one node's estate holding one item
// in progress, ENG-1, on the turn's own seat.
func flowReader(t *testing.T, posture worklet.Posture, touched time.Time) *worklet.Reader {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	body, err := json.Marshal(worklet.Task{
		V: worklet.DocumentVersion, ID: "t-1", Key: "ENG-1", Project: "ENG", Type: "task",
		Title: "ship the release", Status: worklet.StatusInProgress, StatusGroup: worklet.GroupActive,
		Priority: worklet.PriorityUrgent, Rank: worklet.RankOrigin, Assignee: "eng",
		CreatedAt: touched, UpdatedAt: touched,
	})
	if err != nil {
		t.Fatalf("encode the item: %v", err)
	}
	rec := worklet.MutationRecord{
		Kind: worklet.ChangeCreated,
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-1", Subject: worklet.TaskSubject("t-1"),
			Op: worklet.OpCreate, CreatedAt: touched, Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "pm", ActorKind: worklet.AuthorAgent,
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	stream := worklet.Domain{}.Stream().Name
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := worklet.NewApplier(nil).Apply(t.Context(), tx, statelog.Record{
			Envelope: statelog.Envelope{V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
				Op:      string(rec.Op), OpID: rec.OpID, Writer: rec.Writer},
			Position: statelog.Position{Stream: stream, Generation: 1, Seq: 1},
			Payload:  payload, StoredAt: touched,
		}, statelog.ApplyOptions{Now: touched, StoredAt: touched, MaxVariables: db.Caps().MaxVariables})
		return err
	}); err != nil {
		t.Fatalf("apply the item: %v", err)
	}
	log, err := statelogtest.LocalReader(worklet.Domain{}, db,
		statelog.Position{Stream: stream, Generation: 1, Seq: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(db.Reader(), log, fixedPosture(posture))
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	return reader
}

// THE WORK TOOLS SAY WHY AN ITEM IS NOT MOVING (ADR-0162): an urgent item in
// progress on a seat out of its model limit reads held/limit until the reset in
// every tool a model reads it through, the list, the item and its own day, and
// each answer says whether its flow read the seats' live state. Without the
// company's clock the item's detail carries no flow at all, and without
// `flow_live` a list read with no live fact would read as one that found every
// seat free.
//
// Mutation: read the item without its clock in get_work_item, and its flow is
// null; drop `flow_live` from list_work_items, and the list cannot say which.
func TestTheWorkToolsSayWhyAnItemIsNotMoving(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 23, 20, 0, 0, time.UTC)
	since, reset := now.Add(-4*time.Minute), now.Add(10*time.Minute)
	limited := worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"eng": {Stop: worklet.FlowCauseLimit, StopSince: since, StopUntil: reset},
	}}
	want := map[string]any{"state": "held", "cause": "limit", "reason": "",
		"since": since.Format(time.RFC3339), "until": reset.Format(time.RFC3339)}

	for _, c := range []struct {
		name    string
		posture worklet.Posture
		want    map[string]any
	}{
		{"a seat at its limit", limited, want},
		// UNSEEDED: nothing live is claimed, and the answer says so.
		{"a node with no live state", worklet.Posture{}, map[string]any{"state": "moving",
			"cause": "", "reason": "", "since": now.Add(-time.Hour).Format(time.RFC3339),
			"until": nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			reader := flowReader(t, c.posture, now.Add(-time.Hour))
			reg := workRegistry(t, builtin.WorkDeps{Reader: estateTracker{reader}, Writer: newFakeTracker().as,
				Now: func() time.Time { return now }})
			reads := map[string]struct {
				args map[string]any
				flow func(map[string]any) (any, any)
			}{
				builtin.ListWorkItemsTool: {map[string]any{"project": "ENG"},
					func(a map[string]any) (any, any) { return firstRow(a, "items"), a["flow_live"] }},
				builtin.GetWorkItemTool: {map[string]any{"item": "ENG-1"},
					func(a map[string]any) (any, any) { return a["flow"], a["flow_live"] }},
				worklet.MyWorkTool: {nil,
					func(a map[string]any) (any, any) { return firstRow(a, "assigned"), a["flow_live"] }},
			}
			for tool, read := range reads {
				res := callWork(t, reg, tool, read.args)
				if res.Failed {
					t.Fatalf("%s failed: %s", tool, res.Output)
				}
				flow, live := read.flow(answerOf(t, res))
				if !sameJSON(t, flow, c.want) {
					t.Errorf("%s reads ENG-1's flow as %v, want %v", tool, flow, c.want)
				}
				if live != c.posture.Live {
					t.Errorf("%s says flow_live %v, want %v", tool, live, c.posture.Live)
				}
			}
		})
	}

	// AND EACH TOOL TELLS A MODEL WHAT THE FLOW MEANS, or a held item is
	// read as one somebody is moving.
	reg := workRegistry(t, builtin.WorkDeps{Reader: newFakeTracker()})
	for _, tool := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool, worklet.MyWorkTool} {
		entry, _ := reg.Lookup(tool)
		if d := entry.Tool.Description(); !strings.Contains(d, "`flow`") ||
			!strings.Contains(d, "`flow_live: false`") {
			t.Errorf("%s's description never says what an item's flow is: %s", tool, d)
		}
	}
}

// firstRow is the flow of the first row in an answer's list at key.
func firstRow(answer map[string]any, key string) any {
	rows, _ := answer[key].([]any)
	if len(rows) == 0 {
		return "no rows under " + key
	}
	row, _ := rows[0].(map[string]any)
	return row["flow"]
}

// estateTracker is the tracker's real reader as a seat's tools reach it, read
// at the rows this node holds: a seat reads linearizably, which takes a
// broker's read index (internal/statelog's cases), and what is under test here
// is the flow on the answer. The carried strips are the estate's to add, and
// nothing here reads them.
type estateTracker struct{ *worklet.Reader }

func (e estateTracker) Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error) {
	q.Level = statelog.ReadStale
	return e.Reader.Tasks(ctx, q, now)
}

func (e estateTracker) Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
	_ statelog.Freshness) (worklet.TaskDetail, error) {
	return e.Reader.Task(ctx, idOrKey, want, statelog.Freshness{Level: statelog.ReadStale})
}

func (e estateTracker) MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
	loc *time.Location) (worklet.MyWork, error) {
	q.Level = statelog.ReadStale
	return e.Reader.MyWork(ctx, q, now, loc)
}

func (estateTracker) CarriedViews(context.Context, worklet.ViewQuery) (worklet.ViewListing, error) {
	return worklet.ViewListing{}, nil
}

func (estateTracker) CarriedCatalogue(context.Context, worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return worklet.CatalogueAnswer{}, nil
}
