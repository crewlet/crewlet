package estate

import (
	"context"
	"errors"
	"reflect"
	"slices"
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
	cpus   CPUs
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

func (d *desk) CPUs() *CPUs { return &d.cpus }

func (d *desk) For(context.Context, statelog.PartitionID) (Backend, bool, error) {
	return Backend{
		Tracker: deskBoard{d: d}, Pages: deskPages{},
		Writer: func(a Actor) TrackerWriter { return deskWriter{d: d, a: a} },
	}, true, nil
}

type deskBoard struct {
	TrackerReader
	d *desk
}

func (b deskBoard) Workload(_ context.Context, q tracker.WorkloadQuery, _ time.Time) (
	tracker.WorkloadAnswer, error) {
	if q.Units != b.d.units {
		return tracker.WorkloadAnswer{}, errors.New("the serving node's chart was not attached")
	}
	return tracker.WorkloadAnswer{Truncated: q.Unit == "eng"}, nil
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

func (w deskWriter) WriteInbox(_ context.Context, opID, _ string, _, _, _ []tracker.InboxEntry,
	_ []tracker.Reason, _ tracker.Position) (tracker.WriteResult, error) {
	return w.d.note("write_inbox", opID, w.a), nil
}

func (w deskWriter) WritePins(_ context.Context, opID, _ string, _ []string,
	_ []tracker.Favorite) (tracker.WriteResult, error) {
	return w.d.note("write_pins", opID, w.a), nil
}

func (w deskWriter) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return w.d.note("write_priorities", opID, w.a), nil
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
// node from a node that does not serve the partition with their arguments,
// their answers (a revision's found-or-not included) and the operator who
// made them, and the workload is rendered against the SERVING node's chart,
// which is the one that can cross no wire.
func TestEveryOperatorOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	d := &desk{units: chartOf("data-d")}
	stop, err := Serve(t.Context(), f.start(t), "data-d", d, f.servers, ServerSeams{Units: d.units})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	f.servers.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx, now := t.Context(), time.Now()

	work := f.client.Work()
	if load, err := work.Workload(ctx, tracker.WorkloadQuery{Unit: "eng", Units: chartOf("asker")},
		now); err != nil || !load.Truncated {
		t.Errorf("workload = (%+v, %v), want the serving node's answer for unit eng", load, err)
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
		{"write_inbox", func() (tracker.WriteResult, error) {
			return w.WriteInbox(ctx, "op-4", "founder", nil, nil, nil, nil, tracker.Position{})
		}},
		{"write_pins", func() (tracker.WriteResult, error) { return w.WritePins(ctx, "op-5", "founder", nil, nil) }},
		{"write_priorities", func() (tracker.WriteResult, error) {
			return w.WritePriorities(ctx, "op-6", "founder", nil, tracker.PersonAuthority{})
		}},
		{"remove_task", func() (tracker.WriteResult, error) { return w.RemoveTask(ctx, "op-7", "t-1", "ENG", false, nil) }},
		{"restore_task", func() (tracker.WriteResult, error) { return w.RestoreTask(ctx, "op-8", "t-1", "ENG", nil) }},
		{"purge_task", func() (tracker.WriteResult, error) { return w.PurgeTask(ctx, "op-9", "t-1", "ENG", "spam") }},
	}
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
		wantIDs = append(wantIDs, "op-"+string(rune('1'+i)))
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
