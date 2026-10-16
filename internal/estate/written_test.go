package estate

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// reportingWriter is a node's writer for one actor, reporting the task a
// create committed to into the actor's turn's set as the tracker's write
// authority does: only for a write it KNOWS committed, so an unvouched answer
// names nothing.
type reportingWriter struct {
	*fakeNode
	log tracker.WriteLog
}

func (w reportingWriter) CreateTask(ctx context.Context, opID string, task tracker.Task,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := w.fakeNode.CreateTask(ctx, opID, task, notify)
	if err == nil && w.log != nil && out.Outcome == statelog.OutcomeApplied {
		w.log.Add(types.WorkItem{Backend: types.WorkNative, ID: "task-" + out.Key, Key: out.Key})
	}
	return out, err
}

// EditComment is the TRACKER's remark edit — the node's own EditComment is the
// knowledge base's, and a writer for one actor answers the tracker's.
func (w reportingWriter) EditComment(_ context.Context, opID, _, _, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	w.note("task_comment_edit")
	w.mu.Lock()
	w.opIDs = append(w.opIDs, opID)
	w.mu.Unlock()
	return tracker.WriteResult{}, nil
}

// turnSet is an asking turn's own set of the items it wrote, in its process.
type turnSet struct {
	mu    sync.Mutex
	items []types.WorkItem
}

func (s *turnSet) Add(item types.WorkItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
}

func (s *turnSet) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Key)
	}
	return out
}

// keyed records the key a page write arrived with — its actor's operation key,
// or a comment's turn key.
func (f *fakeNode) keyed(key string) {
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
}

// seeds is every key this node's page writes arrived with.
func (f *fakeNode) seeds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys)
}

func (f *fakeNode) Create(_ context.Context, actor pages.Actor, _ pages.NewPage) (pages.Written, error) {
	f.note("page_create")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) SavePage(_ context.Context, actor pages.Actor, _ string,
	_ pages.Save) (pages.Written, error) {
	f.note("page_save")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) Rename(_ context.Context, actor pages.Actor, _, _ string,
	_ bool) (pages.Written, error) {
	f.note("page_rename")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) EditComment(_ context.Context, actor pages.Actor, _, _,
	_ string) (pages.Comment, pages.Written, error) {
	f.note("comment_edit")
	f.keyed(actor.OpKey)
	return pages.Comment{}, pages.Written{}, nil
}

func (f *fakeNode) Trash(_ context.Context, actor pages.Actor, _ string) (pages.Written, error) {
	f.note("page_trash")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) Restore(_ context.Context, actor pages.Actor, _ string) (pages.Written, error) {
	f.note("page_restore")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) Purge(_ context.Context, actor pages.Actor, _, _ string) (pages.Written, error) {
	f.note("page_purge")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

func (f *fakeNode) RemoveComment(_ context.Context, actor pages.Actor, _, _ string,
	_ pages.CommentAuthority) (pages.Written, error) {
	f.note("comment_remove")
	f.keyed(actor.OpKey)
	return pages.Written{}, nil
}

// PlaceTask is a drop whose lane step lands and whose order step this node's
// ledger cannot vouch for where it says so.
func (f *fakeNode) PlaceTask(_ context.Context, opID string, place tracker.Place,
	_ *tracker.Notify) (tracker.PlaceResult, error) {
	f.note("place")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	orderUnvouched := f.unvouched
	f.mu.Unlock()
	res := tracker.PlaceResult{Version: 4}
	res.Lane.Result = statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID + "/lane",
		Position: statelog.Position{Stream: trackerStream, Generation: 1, Seq: 3}}
	res.Order.Result = statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID + "/order",
		Position: statelog.Position{Stream: trackerStream, Generation: 1, Seq: 4}}
	if orderUnvouched {
		res.Order.Result = statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID + "/order",
			Unvouched: true}
	}
	return res, nil
}

// A WRITE ANOTHER NODE RAN IS IN THE ASKING TURN'S SET, because the set is in
// the asking process and no wire carries it: the router asks the holder to name
// what it committed to and adds each item itself. Without it a seat on a node
// without `data` that filed one task was charged to nothing, whatever it spent
// on it — the dispatch-time rules name no item for a turn that created its own.
func TestAWriteAnotherNodeRanIsInTheAskingTurnsSet(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	set := &turnSet{}
	actor := swe
	actor.Provenance.Written = set
	if _, err := f.client.WriterAs(actor).CreateTask(t.Context(), "op-1",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := set.keys(); !slices.Equal(got, []string{"ENG-1"}) {
		t.Fatalf("the asking turn's set holds %v, want the task the holder committed to", got)
	}

	// A TURN THAT HOLDS NO SET ASKS FOR NONE: the holder collects nothing
	// for a writer that is not a turn's.
	node := f.nodes["data-a"]
	if _, err := f.client.WriterAs(swe).CreateTask(t.Context(), "op-2",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	node.mu.Lock()
	asked := slices.Clone(node.actors)
	node.mu.Unlock()
	if len(asked) != 2 || !asked[0].Records || asked[1].Records {
		t.Fatalf("the holder was asked to record %v, want only the turn holding a set",
			[]bool{len(asked) > 0 && asked[0].Records, len(asked) > 1 && asked[1].Records})
	}
}

// A WALK THAT ASKED PAST AN UNVOUCHED HOLDER COLLECTS FROM EVERY HOLDER THAT
// RAN IT, not only the one whose answer is kept — and a holder that could not
// say whether it committed names nothing, so the item is in the set exactly
// because the holder that vouched for it committed it.
func TestAWalkCollectsWhatEveryHolderCommittedTo(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.unvouched = true })
	set := &turnSet{}
	actor := swe
	actor.Provenance.Written = set
	written, err := f.client.WriterAs(actor).CreateTask(t.Context(), "op-w",
		tracker.Task{Project: "ENG"}, nil)
	if err != nil || written.Outcome != statelog.OutcomeApplied {
		t.Fatalf("create = (%+v, %v), want the second holder's applied answer", written.Result, err)
	}
	if got := set.keys(); !slices.Equal(got, []string{"ENG-1"}) {
		t.Fatalf("the asking turn's set holds %v, want the one item the vouching holder committed", got)
	}
}

// A WRITE THIS NODE ANSWERS ITSELF REPORTS INTO THE TURN'S OWN SET directly —
// and once, never again through the answer.
func TestALocalWriteReportsIntoTheTurnsSetOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	local := &fakeNode{name: "data-self", units: chartOf("self")}
	r := f.router(t, "data-self", local)
	set := &turnSet{}
	actor := swe
	actor.Provenance.Written = set
	if _, err := r.WriterAs(actor).CreateTask(t.Context(), "op-l",
		tracker.Task{Project: "ENG"}, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := set.keys(); !slices.Equal(got, []string{"ENG-1"}) {
		t.Fatalf("the turn's set holds %v, want the one item, once", got)
	}
	if f.nodes["data-a"].askedFor("create") {
		t.Error("a data node whose copy serves asked a peer")
	}
}

// A KEYED PAGE WRITE NOBODY ANSWERED IS ASKED AGAIN, UNDER ITS KEY — and an
// unkeyed one never is.
//
// The key is what makes a repeat the same operation: the pages store derives
// the operation id — and a new page's id, and a comment's — from it and what
// the write says, so the next holder's ledger collapses a write the silent one
// may have made. Without one the store mints a fresh id per call and a second
// holder would write a second page or a second comment. EVERY PAGE WRITE says
// where its key is — the actor's operation key, or for a comment made from a
// turn the turn's own — and a predicate reading the wrong one either repeats an
// unkeyed write or strands a keyed one.
func TestAKeyedPageWriteNobodyAnsweredIsAskedAgainUnderItsKey(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	key := statelog.NewOpID(since, "page")
	for _, tc := range []struct {
		name  string
		asked string
		write func(ctx context.Context, p Pages, key string) error
	}{
		{"create", "page_create", func(ctx context.Context, p Pages, key string) error {
			_, err := p.Create(ctx, pages.Actor{OpKey: key},
				pages.NewPage{Container: "ENG", Title: "Runbook"})
			return err
		}},
		{"save", "page_save", func(ctx context.Context, p Pages, key string) error {
			body := "v2"
			_, err := p.SavePage(ctx, pages.Actor{OpKey: key}, "p1",
				pages.Save{BaseVersion: 1, Body: &body})
			return err
		}},
		{"rename", "page_rename", func(ctx context.Context, p Pages, key string) error {
			_, err := p.Rename(ctx, pages.Actor{OpKey: key}, "p1", "Runbook v2", false)
			return err
		}},
		{"comment", "comment", func(ctx context.Context, p Pages, key string) error {
			_, _, err := p.Comment(ctx, pages.Actor{OpKey: key}, "p1", pages.NewComment{Body: "hi"})
			return err
		}},
		{"edit comment", "comment_edit", func(ctx context.Context, p Pages, key string) error {
			_, _, err := p.EditComment(ctx, pages.Actor{OpKey: key}, "p1", "c1", "hello")
			return err
		}},
		{"trash", "page_trash", func(ctx context.Context, p Pages, key string) error {
			_, err := p.Trash(ctx, pages.Actor{OpKey: key}, "p1")
			return err
		}},
		{"restore", "page_restore", func(ctx context.Context, p Pages, key string) error {
			_, err := p.Restore(ctx, pages.Actor{OpKey: key}, "p1")
			return err
		}},
		{"purge", "page_purge", func(ctx context.Context, p Pages, key string) error {
			_, err := p.Purge(ctx, pages.Actor{OpKey: key}, "p1", "spam")
			return err
		}},
		{"remove comment", "comment_remove", func(ctx context.Context, p Pages, key string) error {
			_, err := p.RemoveComment(ctx, pages.Actor{OpKey: key}, "p1", "c1",
				pages.CommentAuthority{Moderate: true})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A FLEET PER HALF: the holder that answered last is asked
			// first, so the second half on the first's fleet would never
			// meet the silent holder at all.
			silentFirst := func(t *testing.T) (*fleet, *fakeNode) {
				f := newFleet(t, "data-a", "data-b")
				_, first := f.first(t, f.client)
				first.set(func(n *fakeNode) { n.silent = true })
				return f, f.other(first)
			}

			f, second := silentFirst(t)
			if err := tc.write(t.Context(), f.client.Pages(), key); err != nil {
				t.Fatalf("a keyed %s with its first holder silent = %v, want it written "+
					"by the next", tc.name, err)
			}
			if !second.askedFor(tc.asked) || !slices.Equal(second.seeds(), []string{key}) {
				t.Fatalf("the next holder ran %v under keys %v, want the %s under %s",
					second.asked, second.seeds(), tc.asked, key)
			}

			f, second = silentFirst(t)
			err := tc.write(t.Context(), f.client.Pages(), "")
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("an unkeyed %s with its first holder silent = %v, want ErrOutcomeUnknown",
					tc.name, err)
			}
			if second.askedFor(tc.asked) {
				t.Errorf("an unkeyed %s was asked of a second holder", tc.name)
			}
		})
	}
}

// A DROP WHOSE ORDER STEP A HOLDER COULD NOT VOUCH FOR IS ASKED OF THE NEXT,
// under the same operation: the unvouched outcome is on the SECOND of the
// gesture's two steps, inside a named field of the answer, and a router that
// read only the first would hand the board a placement nobody knows landed.
func TestADropWhoseOrderStepIsUnvouchedIsAskedOfTheNextHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b")
	_, first := f.first(t, f.client)
	first.set(func(n *fakeNode) { n.unvouched = true })
	second := f.other(first)

	placed, err := f.client.WriterAs(swe).PlaceTask(t.Context(), "op-drop",
		tracker.Place{Task: "t-1", Project: "ENG"}, nil)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if placed.Order.Outcome != statelog.OutcomeApplied || placed.Order.Unvouched {
		t.Fatalf("the drop's order step answered %+v, want the vouching holder's", placed.Order.Result)
	}
	for _, n := range []*fakeNode{first, second} {
		if got := n.ops(); !slices.Equal(got, []string{"op-drop"}) {
			t.Errorf("%s ran %v, want [op-drop] — both holders under the one operation", n.name, got)
		}
	}
}
