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

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// desk is a data node's copy as the operator's surfaces reach it: each read
// answers a value naming what it was asked, and each write records the
// operation, its id and who wrote it.
type desk struct {
	mu     sync.Mutex
	wrote  []string
	opIDs  []string
	actors []Actor
	units  worklet.Units

	// paged is every page write the desk took, with the actor it was made as.
	paged      []string
	pageActors []doclet.Actor
}

func (d *desk) page(op string, a doclet.Actor) doclet.Written {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paged = append(d.paged, op)
	d.pageActors = append(d.pageActors, a)
	return doclet.Written{Outcome: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: a.OpKey}}
}

func (d *desk) note(op, opID string, a Actor) worklet.WriteResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wrote = append(d.wrote, op)
	d.opIDs = append(d.opIDs, opID)
	d.actors = append(d.actors, a)
	return worklet.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID}}
}

func (d *desk) For(context.Context) (Backend, bool) {
	return Backend{
		Tracker: deskBoard{d: d}, Pages: deskPages{}, PageWriter: deskPageWriter{d: d},
		Writer: func(a Actor) TrackerWriter { return deskWriter{d: d, a: a} },
	}, true
}

type deskBoard struct {
	TrackerReader
	d *desk
}

func (b deskBoard) Workload(_ context.Context, q worklet.WorkloadQuery, _ time.Time,
	loc *time.Location) (worklet.WorkloadAnswer, error) {
	if q.Units != b.d.units {
		return worklet.WorkloadAnswer{}, errors.New("the serving node's chart was not attached")
	}
	// THE ASKER'S CLOCK, which a days-cut read must be answered on: the
	// serving node's own would cut "this week" at another midnight.
	if loc == nil || loc.String() != "Europe/Berlin" {
		return worklet.WorkloadAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return worklet.WorkloadAnswer{Truncated: q.Unit == "eng"}, nil
}

func (b deskBoard) EveryView(_ context.Context, q worklet.EveryViewQuery) (worklet.ViewListing, error) {
	if q.Units != b.d.units {
		return worklet.ViewListing{}, errors.New("the serving node's chart was not attached")
	}
	return worklet.ViewListing{Views: []worklet.ViewRow{{ID: "v-" + q.Viewer}}}, nil
}

func (deskBoard) Flow(_ context.Context, q worklet.FlowQuery, _ time.Time,
	loc *time.Location) (worklet.FlowAnswer, error) {
	if loc == nil || loc.String() != "Europe/Berlin" {
		return worklet.FlowAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return worklet.FlowAnswer{Project: q.Project, Points: []worklet.FlowPoint{}}, nil
}

func (deskBoard) CompanyFeed(_ context.Context, q worklet.FeedQuery) (worklet.FeedPage, error) {
	return worklet.FeedPage{More: q.Limit == 7, Rows: []worklet.FeedRow{}}, nil
}

func (deskBoard) Decisions(_ context.Context, q worklet.DecisionsQuery, _ time.Time,
	loc *time.Location) (worklet.DecisionsAnswer, error) {
	if loc == nil || loc.String() != "Europe/Berlin" {
		return worklet.DecisionsAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return worklet.DecisionsAnswer{Asks: []worklet.AskRow{}, Complete: q.Handle == "founder"}, nil
}

func (deskBoard) AnsweredAsks(_ context.Context, q worklet.DecisionsQuery, _ time.Time,
	loc *time.Location) (worklet.AnsweredAnswer, error) {
	if loc == nil || loc.String() != "Europe/Berlin" {
		return worklet.AnsweredAnswer{}, fmt.Errorf("the asker's zone did not cross: %v", loc)
	}
	return worklet.AnsweredAnswer{Asks: []worklet.AnsweredAsk{}, Complete: q.Handle == "founder"}, nil
}

func (deskBoard) TurnsOf(_ context.Context, ref, cursor string, limit int,
	_ statelog.Freshness) (worklet.TaskTurns, error) {
	return worklet.TaskTurns{Task: ref, Next: cursor + "+" + strconv.Itoa(limit)}, nil
}

func (deskBoard) TurnPlaces(_ context.Context, runs []string,
	_ statelog.Freshness) (map[string]worklet.TurnPlace, error) {
	out := map[string]worklet.TurnPlace{}
	for i, run := range runs {
		out[run] = worklet.TurnPlace{TaskID: "t-" + run, Ordinal: i + 1}
	}
	return out, nil
}

func (deskBoard) Inbox(_ context.Context, q worklet.InboxQuery, _ time.Time) (worklet.InboxAnswer, error) {
	return worklet.InboxAnswer{Unread: len(q.Reasons)}, nil
}

func (deskBoard) Routing(_ context.Context, q worklet.RoutingQuery, _ time.Time) (worklet.RoutingAnswer, error) {
	return worklet.RoutingAnswer{RecordID: q.RecordID, Held: true}, nil
}

type deskPages struct{ PageReader }

func (deskPages) Containers(context.Context, statelog.Freshness) ([]doclet.ContainerListing, error) {
	return []doclet.ContainerListing{{Pages: 4}}, nil
}

func (deskPages) Activity(_ context.Context, q doclet.PageActivityQuery) (doclet.PageActivity, error) {
	return doclet.PageActivity{NextCursor: q.Page}, nil
}

func (deskPages) Revision(_ context.Context, pageID string, version int,
	_ statelog.Freshness) (doclet.Revision, bool, error) {
	return doclet.Revision{PageID: pageID, Version: version}, version > 0, nil
}

type deskWriter struct {
	TrackerWriter
	d *desk
	a Actor
}

// ViewPrior answers a view the founder owns, which a save must hand back.
func (w deskWriter) ViewPrior(_ context.Context, id string) (worklet.ViewPrior, error) {
	return worklet.ViewPrior{Held: id == "v-1", Owner: "founder"}, nil
}

func (w deskWriter) WriteView(_ context.Context, opID string, _ worklet.View,
	prior worklet.ViewPrior) (worklet.WriteResult, error) {
	if !prior.Held || prior.Owner != "founder" {
		return worklet.WriteResult{}, fmt.Errorf("the prior did not cross: %+v", prior)
	}
	return w.d.note("write_view", opID, w.a), nil
}

func (w deskWriter) WriteTypes(_ context.Context, opID string, _ []worklet.TaskType) (worklet.WriteResult, error) {
	return w.d.note("write_types", opID, w.a), nil
}

func (w deskWriter) WriteFields(_ context.Context, opID string, _ []worklet.FieldDef) (worklet.WriteResult, error) {
	return w.d.note("write_fields", opID, w.a), nil
}

func (w deskWriter) MarkInbox(_ context.Context, opID, _ string,
	g worklet.InboxGesture, authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	if !slices.Equal(g.Read, []string{"rec-1"}) {
		return worklet.WriteResult{}, fmt.Errorf("the gesture did not cross: %+v", g)
	}
	if !authority.Authorized {
		return worklet.WriteResult{}, fmt.Errorf("the authority did not cross: %+v", authority)
	}
	return w.d.note("mark_inbox", opID, w.a), nil
}

func (w deskWriter) WritePins(_ context.Context, opID, _ string,
	g worklet.PinGesture, authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	if !slices.Equal(g.Views.Add, []string{"v-1"}) {
		return worklet.WriteResult{}, fmt.Errorf("the gesture did not cross: %+v", g)
	}
	if !authority.Authorized {
		return worklet.WriteResult{}, fmt.Errorf("the authority did not cross: %+v", authority)
	}
	return w.d.note("write_pins", opID, w.a), nil
}

func (w deskWriter) WritePriorities(_ context.Context, opID, _ string, _ []string,
	ifMatch *uint64, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	if ifMatch == nil || *ifMatch != 9 {
		return worklet.WriteResult{}, fmt.Errorf("the condition did not cross: %v", ifMatch)
	}
	return w.d.note("write_priorities", opID, w.a), nil
}

// PlaceTask changes the card's lane and refuses its place, as a drop does when
// the card moved between its two appends — the refusal is what has to cross
// with its identity, since the tool classifies it.
func (w deskWriter) PlaceTask(_ context.Context, opID string, place worklet.Place,
	_ *worklet.Notify) (worklet.PlaceResult, error) {
	lane := w.d.note("place_task", opID, w.a)
	return worklet.PlaceResult{Lane: lane, Version: 12, Unplaced: fmt.Errorf(
		"%w: task %s moved under the drop", worklet.ErrStaleVersion, place.Task)}, nil
}

func (w deskWriter) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return w.d.note("remove_task", opID, w.a), nil
}

func (w deskWriter) RestoreTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return w.d.note("restore_task", opID, w.a), nil
}

func (w deskWriter) PurgeTask(_ context.Context, opID, _, _, _ string) (worklet.WriteResult, error) {
	return w.d.note("purge_task", opID, w.a), nil
}

func (w deskWriter) EditComment(_ context.Context, opID, _, _, commentID, body string,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	if commentID != "c-1" || body != "reworded" {
		return worklet.WriteResult{}, fmt.Errorf("the edit did not cross: %q %q", commentID, body)
	}
	return w.d.note("edit_comment", opID, w.a), nil
}

// deskPageWriter is the knowledge base's operator writes on the desk.
type deskPageWriter struct {
	PageWriter
	d *desk
}

func (w deskPageWriter) Trash(_ context.Context, a doclet.Actor, _ string) (doclet.Written, error) {
	return w.d.page("trash", a), nil
}

func (w deskPageWriter) Restore(_ context.Context, a doclet.Actor, _ string) (doclet.Written, error) {
	return w.d.page("restore", a), nil
}

func (w deskPageWriter) Purge(_ context.Context, a doclet.Actor, _, reason string) (doclet.Written, error) {
	if reason != "spam" {
		return doclet.Written{}, fmt.Errorf("the reason did not cross: %q", reason)
	}
	return w.d.page("purge", a), nil
}

func (w deskPageWriter) RemoveComment(_ context.Context, a doclet.Actor, _, _ string,
	authority doclet.CommentAuthority) (doclet.Written, error) {
	if !authority.Moderate {
		return doclet.Written{}, fmt.Errorf("the authority did not cross: %+v", authority)
	}
	return w.d.page("remove_comment", a), nil
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
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: d, Seams: ServerSeams{Units: d.units}, Envelopes: fleetEnvelopes,
	})
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
	if load, err := work.Workload(ctx, worklet.WorkloadQuery{Unit: "eng", Units: chartOf("asker")},
		now, berlin); err != nil || !load.Truncated {
		t.Errorf("workload = (%+v, %v), want the serving node's answer for unit eng", load, err)
	}
	if views, err := work.EveryView(ctx, worklet.EveryViewQuery{
		Viewer: "founder", Units: chartOf("asker")}); err != nil ||
		len(views.Views) != 1 || views.Views[0].ID != "v-founder" {
		t.Errorf("every view = (%+v, %v), want the founder's, on the serving chart", views, err)
	}
	if flow, err := work.Flow(ctx, worklet.FlowQuery{Project: "ENG"}, now, berlin); err != nil ||
		flow.Project != "ENG" {
		t.Errorf("flow = (%+v, %v), want ENG's on the asker's clock", flow, err)
	}
	if feed, err := work.CompanyFeed(ctx, worklet.FeedQuery{Limit: 7}); err != nil || !feed.More {
		t.Errorf("feed = (%+v, %v), want the query's limit answered", feed, err)
	}
	if asks, err := work.Decisions(ctx, worklet.DecisionsQuery{Handle: "founder"},
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
	if inbox, err := work.Inbox(ctx, worklet.InboxQuery{Reasons: []worklet.Reason{"mention", "asked"}},
		now); err != nil || inbox.Unread != 2 {
		t.Errorf("inbox = (%+v, %v), want the query's two reasons answered", inbox, err)
	}
	if routing, err := work.Routing(ctx, worklet.RoutingQuery{RecordID: "rec-9"}, now); err != nil ||
		routing.RecordID != "rec-9" || !routing.Held {
		t.Errorf("routing = (%+v, %v), want rec-9 held", routing, err)
	}
	kb := f.client.Pages()
	if containers, err := kb.Containers(ctx, statelog.Freshness{}); err != nil ||
		len(containers) != 1 || containers[0].Pages != 4 {
		t.Errorf("containers = (%+v, %v), want the serving node's one", containers, err)
	}
	if activity, err := kb.Activity(ctx, doclet.PageActivityQuery{Page: "p-7"}); err != nil ||
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

	operator := Actor{Handle: "founder", Kind: worklet.AuthorOperator,
		Provenance: worklet.Provenance{OperatorID: "founder"}}
	w := f.client.WriterAs(operator)
	authorized := worklet.PersonAuthority{Authorized: true}
	prior, err := w.ViewPrior(ctx, "v-1")
	if err != nil || !prior.Held || prior.Owner != "founder" {
		t.Fatalf("view prior = (%+v, %v), want the founder's stored view", prior, err)
	}
	writes := []struct {
		op    string
		write func() (worklet.WriteResult, error)
	}{
		{"write_view", func() (worklet.WriteResult, error) {
			return w.WriteView(ctx, "op-1", worklet.View{ID: "v-1"}, prior)
		}},
		{"write_types", func() (worklet.WriteResult, error) { return w.WriteTypes(ctx, "op-2", nil) }},
		{"write_fields", func() (worklet.WriteResult, error) { return w.WriteFields(ctx, "op-3", nil) }},
		{"mark_inbox", func() (worklet.WriteResult, error) {
			return w.MarkInbox(ctx, "op-4", "founder", worklet.InboxGesture{Read: []string{"rec-1"}},
				authorized)
		}},
		{"write_pins", func() (worklet.WriteResult, error) {
			return w.WritePins(ctx, "op-5", "founder", worklet.PinGesture{
				Views: worklet.SetChange[string]{Add: []string{"v-1"}}}, authorized)
		}},
		{"write_priorities", func() (worklet.WriteResult, error) {
			match := uint64(9)
			return w.WritePriorities(ctx, "op-6", "founder", nil, &match, worklet.PersonAuthority{})
		}},
		{"remove_task", func() (worklet.WriteResult, error) { return w.RemoveTask(ctx, "op-7", "t-1", "ENG", false, nil) }},
		{"restore_task", func() (worklet.WriteResult, error) { return w.RestoreTask(ctx, "op-8", "t-1", "ENG", true, nil) }},
		{"purge_task", func() (worklet.WriteResult, error) { return w.PurgeTask(ctx, "op-9", "t-1", "ENG", "spam") }},
		{"edit_comment", func() (worklet.WriteResult, error) {
			return w.EditComment(ctx, "op-10", "t-1", "ENG", "c-1", "reworded", nil)
		}},
	}
	writes = append(writes, struct {
		op    string
		write func() (worklet.WriteResult, error)
	}{"place_task", func() (worklet.WriteResult, error) {
		// A DROP WHOSE PLACE WAS REFUSED crosses with the refusal's
		// identity, which the tool classifies — an error is an interface
		// no encoder writes — beside the lane it did change.
		placed, err := w.PlaceTask(ctx, "op-11", worklet.Place{Task: "t-1", Project: "ENG",
			After: "t-2"}, nil)
		if !errors.Is(placed.Unplaced, worklet.ErrStaleVersion) || placed.Version != 12 {
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
	// AND THE KNOWLEDGE BASE'S, each made as the operator who made it and
	// under the key they brought.
	editor := doclet.Actor{Handle: "founder", Kind: doclet.AuthorOperator,
		OperatorID: "founder", OpKey: statelog.NewOpID(now, "page")}
	kb2 := f.client.Pages()
	for _, write := range []struct {
		op    string
		write func() (doclet.Written, error)
	}{
		{"trash", func() (doclet.Written, error) { return kb2.Trash(ctx, editor, "p-7") }},
		{"restore", func() (doclet.Written, error) { return kb2.Restore(ctx, editor, "p-7") }},
		{"purge", func() (doclet.Written, error) { return kb2.Purge(ctx, editor, "p-7", "spam") }},
		{"remove_comment", func() (doclet.Written, error) {
			return kb2.RemoveComment(ctx, editor, "p-7", "c-1", doclet.CommentAuthority{Moderate: true})
		}},
	} {
		if written, err := write.write(); err != nil || written.Outcome.Outcome != statelog.OutcomeApplied {
			t.Errorf("%s = (%+v, %v), want applied", write.op, written.Outcome, err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if want := []string{"trash", "restore", "purge", "remove_comment"}; !slices.Equal(d.paged, want) {
		t.Errorf("the data node's knowledge base took %v, want %v", d.paged, want)
	}
	for i, a := range d.pageActors {
		if !reflect.DeepEqual(a, editor) {
			t.Errorf("page %s was written as %+v, want the operator and their key", d.paged[i], a)
		}
	}
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
