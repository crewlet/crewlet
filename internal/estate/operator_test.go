package estate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// desk is a data node's copy as the operator's surfaces reach it: each read
// answers a value naming what it was asked, and each write records the
// operation, its id and who wrote it.
type desk struct {
	mu     sync.Mutex
	wrote  []string
	opIDs  []string
	actors []Actor
	units  tracker.Units
}

func (d *desk) note(op, opID string, a Actor) tracker.WriteResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wrote = append(d.wrote, op)
	d.opIDs = append(d.opIDs, opID)
	d.actors = append(d.actors, a)
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID}}
}

func (d *desk) For(context.Context) (Backend, bool) {
	return Backend{
		Tracker: deskBoard{d: d}, Pages: deskPages{},
		Writer: func(a Actor) TrackerWriter { return deskWriter{d: d, a: a} },
	}, true
}

type deskBoard struct {
	TrackerReader
	d *desk
}

func (b deskBoard) Workload(_ context.Context, q tracker.WorkloadQuery, _ time.Time,
	loc *time.Location) (tracker.WorkloadAnswer, error) {
	if q.Units != b.d.units {
		return tracker.WorkloadAnswer{}, errors.New("the serving node's chart was not attached")
	}
	// THE ASKER'S CLOCK, which a days-cut read must be answered on: the
	// serving node's own would cut "this week" at another midnight.
	if loc == nil || loc.String() != "Europe/Berlin" {
		return tracker.WorkloadAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return tracker.WorkloadAnswer{Truncated: q.Unit == "eng"}, nil
}

func (b deskBoard) EveryView(_ context.Context, q tracker.EveryViewQuery) (tracker.ViewListing, error) {
	if q.Units != b.d.units {
		return tracker.ViewListing{}, errors.New("the serving node's chart was not attached")
	}
	return tracker.ViewListing{Views: []tracker.ViewRow{{ID: "v-" + q.Viewer.Handle}}}, nil
}

func (deskBoard) Flow(_ context.Context, q tracker.FlowQuery, _ time.Time,
	loc *time.Location) (tracker.FlowAnswer, error) {
	if loc == nil || loc.String() != "Europe/Berlin" {
		return tracker.FlowAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return tracker.FlowAnswer{Project: q.Project, Points: []tracker.FlowPoint{}}, nil
}

func (deskBoard) CompanyFeed(_ context.Context, q tracker.FeedQuery) (tracker.FeedPage, error) {
	return tracker.FeedPage{More: q.Limit == 7, Rows: []tracker.FeedRow{}}, nil
}

func (deskBoard) Decisions(_ context.Context, q tracker.DecisionsQuery, _ time.Time,
	loc *time.Location) (tracker.DecisionsAnswer, error) {
	if loc == nil || loc.String() != "Europe/Berlin" {
		return tracker.DecisionsAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return tracker.DecisionsAnswer{Asks: []tracker.AskRow{}, Complete: q.Who.Handle == "founder"}, nil
}

func (deskBoard) TurnsOf(_ context.Context, ref, cursor string, limit int,
	_ statelog.Freshness) (tracker.TaskTurns, error) {
	return tracker.TaskTurns{Task: ref, Next: cursor + "+" + strconv.Itoa(limit)}, nil
}

func (deskBoard) TurnPlaces(_ context.Context, runs []string,
	_ statelog.Freshness) (map[string]tracker.TurnPlace, error) {
	out := map[string]tracker.TurnPlace{}
	for i, run := range runs {
		out[run] = tracker.TurnPlace{TaskID: "t-" + run, Ordinal: i + 1}
	}
	return out, nil
}

func (deskBoard) Inbox(_ context.Context, q tracker.InboxQuery, _ time.Time) (tracker.InboxAnswer, error) {
	return tracker.InboxAnswer{Unread: len(q.Reasons)}, nil
}

func (deskBoard) Routing(_ context.Context, q tracker.RoutingQuery, _ time.Time) (tracker.RoutingAnswer, error) {
	return tracker.RoutingAnswer{RecordID: q.RecordID, Held: true}, nil
}

type deskPages struct{ PageReader }

func (deskPages) Containers(context.Context, statelog.Freshness) ([]pages.ContainerListing, error) {
	return []pages.ContainerListing{{Pages: 4}}, nil
}

func (deskPages) Activity(_ context.Context, q pages.PageActivityQuery) (pages.PageActivity, error) {
	return pages.PageActivity{NextCursor: q.Page}, nil
}

func (deskPages) Revision(_ context.Context, pageID string, version int,
	_ statelog.Freshness) (pages.Revision, bool, error) {
	return pages.Revision{PageID: pageID, Version: version}, version > 0, nil
}

type deskWriter struct {
	TrackerWriter
	d *desk
	a Actor
}

func (w deskWriter) WriteView(_ context.Context, opID string, _ tracker.View) (tracker.WriteResult, error) {
	return w.d.note("write_view", opID, w.a), nil
}

func (w deskWriter) WriteTypes(_ context.Context, opID string, _ []tracker.TaskType) (tracker.WriteResult, error) {
	return w.d.note("write_types", opID, w.a), nil
}

func (w deskWriter) WriteFields(_ context.Context, opID string, _ []tracker.FieldDef) (tracker.WriteResult, error) {
	return w.d.note("write_fields", opID, w.a), nil
}

func (w deskWriter) MarkInbox(_ context.Context, opID, _ string,
	g tracker.InboxGesture) (tracker.WriteResult, error) {
	if !slices.Equal(g.Read, []string{"rec-1"}) {
		return tracker.WriteResult{}, fmt.Errorf("the gesture did not cross: %+v", g)
	}
	return w.d.note("mark_inbox", opID, w.a), nil
}

func (w deskWriter) WritePins(_ context.Context, opID, _ string,
	g tracker.PinGesture) (tracker.WriteResult, error) {
	if !slices.Equal(g.Views.Add, []string{"v-1"}) {
		return tracker.WriteResult{}, fmt.Errorf("the gesture did not cross: %+v", g)
	}
	return w.d.note("write_pins", opID, w.a), nil
}

func (w deskWriter) WritePriorities(_ context.Context, opID, _ string, _ []string,
	ifMatch *uint64, _ tracker.PersonAuthority) (tracker.WriteResult, error) {
	if ifMatch == nil || *ifMatch != 9 {
		return tracker.WriteResult{}, fmt.Errorf("the condition did not cross: %v", ifMatch)
	}
	return w.d.note("write_priorities", opID, w.a), nil
}

// PlaceTask changes the card's lane and refuses its place, as a drop does when
// the card moved between its two appends — the refusal is what has to cross
// with its identity, since the tool classifies it.
func (w deskWriter) PlaceTask(_ context.Context, opID string, place tracker.Place,
	_ *tracker.Notify) (tracker.PlaceResult, error) {
	lane := w.d.note("place_task", opID, w.a)
	return tracker.PlaceResult{Lane: lane, Version: 12, Unplaced: fmt.Errorf(
		"%w: task %s moved under the drop", tracker.ErrStaleVersion, place.Task)}, nil
}

func (w deskWriter) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return w.d.note("remove_task", opID, w.a), nil
}

func (w deskWriter) RestoreTask(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return w.d.note("restore_task", opID, w.a), nil
}

func (w deskWriter) PurgeTask(_ context.Context, opID, _, _, _ string) (tracker.WriteResult, error) {
	return w.d.note("purge_task", opID, w.a), nil
}

// EVERY OPERATOR OPERATION CROSSES WHOLE: the reads the dashboard, the REST
// routes and the operator's MCP ask, and the writes only they make — views,
// the catalogue, a person's own state, the trash, the purge — reach a data
// node from a node that holds no data with their arguments,
// their answers (a revision's found-or-not included) and the operator who
// made them, and the workload is rendered against the SERVING node's chart,
// which is the one that can cross no wire.
func TestEveryOperatorOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	d := &desk{units: chartOf("data-d")}
	stop, err := Serve(t.Context(), f.start(t), "data-d", d, ServerSeams{Units: d.units})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx, now := t.Context(), time.Now()

	work := f.client.Work()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if load, err := work.Workload(ctx, tracker.WorkloadQuery{Unit: "eng", Units: chartOf("asker")},
		now, berlin); err != nil || !load.Truncated {
		t.Errorf("workload = (%+v, %v), want the serving node's answer for unit eng", load, err)
	}
	if views, err := work.EveryView(ctx, tracker.EveryViewQuery{
		Viewer: tracker.Party{Handle: "founder"}, Units: chartOf("asker")}); err != nil ||
		len(views.Views) != 1 || views.Views[0].ID != "v-founder" {
		t.Errorf("every view = (%+v, %v), want the founder's, on the serving chart", views, err)
	}
	if flow, err := work.Flow(ctx, tracker.FlowQuery{Project: "ENG"}, now, berlin); err != nil ||
		flow.Project != "ENG" {
		t.Errorf("flow = (%+v, %v), want ENG's on the asker's clock", flow, err)
	}
	if feed, err := work.CompanyFeed(ctx, tracker.FeedQuery{Limit: 7}); err != nil || !feed.More {
		t.Errorf("feed = (%+v, %v), want the query's limit answered", feed, err)
	}
	if asks, err := work.Decisions(ctx, tracker.DecisionsQuery{Who: tracker.Party{Handle: "founder"}},
		now, berlin); err != nil || !asks.Complete {
		t.Errorf("decisions = (%+v, %v), want the founder's on the asker's clock", asks, err)
	}
	if turns, err := work.TurnsOf(ctx, "t-1", "c-3", 5, statelog.Freshness{}); err != nil ||
		turns.Task != "t-1" || turns.Next != "c-3+5" {
		t.Errorf("turns = (%+v, %v), want t-1's from c-3 by 5", turns, err)
	}
	if places, err := work.TurnPlaces(ctx, []string{"r-1", "r-2"}, statelog.Freshness{}); err != nil ||
		places["r-2"].TaskID != "t-r-2" || places["r-2"].Ordinal != 2 {
		t.Errorf("turn places = (%+v, %v), want both runs placed", places, err)
	}
	if inbox, err := work.Inbox(ctx, tracker.InboxQuery{Reasons: []tracker.Reason{"mention", "asked"}},
		now); err != nil || inbox.Unread != 2 {
		t.Errorf("inbox = (%+v, %v), want the query's two reasons answered", inbox, err)
	}
	if routing, err := work.Routing(ctx, tracker.RoutingQuery{RecordID: "rec-9"}, now); err != nil ||
		routing.RecordID != "rec-9" || !routing.Held {
		t.Errorf("routing = (%+v, %v), want rec-9 held", routing, err)
	}
	kb := f.client.Pages()
	if containers, err := kb.Containers(ctx, statelog.Freshness{}); err != nil ||
		len(containers) != 1 || containers[0].Pages != 4 {
		t.Errorf("containers = (%+v, %v), want the serving node's one", containers, err)
	}
	if activity, err := kb.Activity(ctx, pages.PageActivityQuery{Page: "p-7"}); err != nil ||
		activity.NextCursor != "p-7" {
		t.Errorf("activity = (%+v, %v), want p-7's", activity, err)
	}
	if rev, found, err := kb.Revision(ctx, "p-7", 3, statelog.Freshness{}); err != nil || !found ||
		rev.PageID != "p-7" || rev.Version != 3 {
		t.Errorf("revision = (%+v, %v, %v), want p-7 at 3, found", rev, found, err)
	}
	if _, found, err := kb.Revision(ctx, "p-7", 0, statelog.Freshness{}); err != nil || found {
		t.Errorf("a revision the page has none of = (%v, %v), want not found", found, err)
	}

	operator := Actor{Handle: "founder", Kind: tracker.AuthorOperator,
		Provenance: tracker.Provenance{OperatorID: "founder"}}
	w := f.client.WriterAs(operator)
	writes := []struct {
		op    string
		write func() (tracker.WriteResult, error)
	}{
		{"write_view", func() (tracker.WriteResult, error) { return w.WriteView(ctx, "op-1", tracker.View{}) }},
		{"write_types", func() (tracker.WriteResult, error) { return w.WriteTypes(ctx, "op-2", nil) }},
		{"write_fields", func() (tracker.WriteResult, error) { return w.WriteFields(ctx, "op-3", nil) }},
		{"mark_inbox", func() (tracker.WriteResult, error) {
			return w.MarkInbox(ctx, "op-4", "founder", tracker.InboxGesture{Read: []string{"rec-1"}})
		}},
		{"write_pins", func() (tracker.WriteResult, error) {
			return w.WritePins(ctx, "op-5", "founder", tracker.PinGesture{
				Views: tracker.SetChange[string]{Add: []string{"v-1"}}})
		}},
		{"write_priorities", func() (tracker.WriteResult, error) {
			match := uint64(9)
			return w.WritePriorities(ctx, "op-6", "founder", nil, &match, tracker.PersonAuthority{})
		}},
		{"remove_task", func() (tracker.WriteResult, error) { return w.RemoveTask(ctx, "op-7", "t-1", "ENG", false, nil) }},
		{"restore_task", func() (tracker.WriteResult, error) { return w.RestoreTask(ctx, "op-8", "t-1", "ENG", nil) }},
		{"purge_task", func() (tracker.WriteResult, error) { return w.PurgeTask(ctx, "op-9", "t-1", "ENG", "spam") }},
	}
	writes = append(writes, struct {
		op    string
		write func() (tracker.WriteResult, error)
	}{"place_task", func() (tracker.WriteResult, error) {
		// A DROP WHOSE PLACE WAS REFUSED crosses with the refusal's
		// identity, which the tool classifies — an error is an interface
		// no encoder writes — beside the lane it did change.
		placed, err := w.PlaceTask(ctx, "op-10", tracker.Place{Task: "t-1", Project: "ENG",
			After: "t-2"}, nil)
		if !errors.Is(placed.Unplaced, tracker.ErrStaleVersion) || placed.Version != 12 {
			t.Errorf("place = (%+v, %v), want the lane changed and a stale-version refusal "+
				"of its place", placed, placed.Unplaced)
		}
		return placed.Lane, err
	}})
	for _, write := range writes {
		if res, err := write.write(); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Errorf("%s = (%+v, %v), want applied", write.op, res, err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var want, wantIDs []string
	for i, write := range writes {
		want = append(want, write.op)
		wantIDs = append(wantIDs, "op-"+strconv.Itoa(i+1))
	}
	if !slices.Equal(d.wrote, want) || !slices.Equal(d.opIDs, wantIDs) {
		t.Errorf("the data node wrote %v under %v, want %v under %v", d.wrote, d.opIDs, want, wantIDs)
	}
	for i, a := range d.actors {
		if !reflect.DeepEqual(a, operator) {
			t.Errorf("%s was written as %+v, want the operator", d.wrote[i], a)
		}
	}
}
