package estate

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What a value its type's own IsZero calls zero becomes on its way to a data
// node: the router's wire leaves a field at its zero off ([encodeInput]), and a
// pointer at such a value is such a field — so each field the wire gate found
// pointing at one (zeroThroughPointer) is held here to reaching the data node
// meaning what it meant, asked from a node without data and from a data node's
// own seat alike.

// CreateTaskAsking files a task with its ask, recording the task.
func (f *fakeNode) CreateTaskAsking(_ context.Context, opID string, task worklet.Task, _ worklet.Comment,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	f.note("create_asking")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	f.created = append(f.created, task)
	f.mu.Unlock()
	return worklet.WriteResult{Key: task.Project + "-1"}, nil
}

// MarkInbox records the gesture it was asked to make.
func (f *fakeNode) MarkInbox(_ context.Context, opID, _ string, _ statelog.Position,
	g inbox.Gesture[worklet.Reason], _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	f.note("mark_inbox")
	f.mu.Lock()
	f.opIDs = append(f.opIDs, opID)
	f.gestures = append(f.gestures, g)
	f.mu.Unlock()
	return worklet.WriteResult{}, nil
}

// A DATE CLEARED THROUGH THE ROUTER REACHES THE DATA NODE AS A CLEAR. Spelled
// as a pointer at the zero instant, as a patch once spelled it, a clear crossed
// as no date named at all — the wire leaves a pointer at a value its type calls
// zero off — and the task kept the date the tool said it had cleared, while the
// same patch made on a data node's own seat cleared it. Spelled as its flag it
// crosses as itself; and a patch setting a date to the zero instant is refused
// before it is asked, naming the flag, so no data node is ever handed the
// pointer's absence in its place.
//
// Mutation: drop refusedBeforeAsking from opUpdateTask and the patch setting the
// due date to the zero instant reaches the data node as a patch naming no date.
func TestADateClearedThroughTheRouterReachesTheDataNodeAsAClear(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	w := f.client.WriterAs(swe)
	if _, err := w.UpdateTask(t.Context(), "op-clear", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{ClearStart: true, ClearDue: true}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("clear the dates: %v", err)
	}
	node.mu.Lock()
	patches := slices.Clone(node.patches)
	node.mu.Unlock()
	if len(patches) != 1 || !patches[0].ClearStart || !patches[0].ClearDue || patches[0].StartAt != nil ||
		patches[0].DueAt != nil {
		t.Fatalf("the data node was handed %+v, want one patch clearing both dates and setting neither", patches)
	}
	zero := time.Time{}
	_, err := w.UpdateTask(t.Context(), "op-zero", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{DueAt: &zero}, worklet.ChangeFields, nil)
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), "clear_due") {
		t.Errorf("a patch setting the due date to the zero instant was answered %v, want it refused "+
			"naming clear_due", err)
	}
	node.mu.Lock()
	handed := len(node.patches)
	node.mu.Unlock()
	if handed != 1 {
		t.Errorf("the data node was handed %d patches, want the clear alone", handed)
	}
}

// A WRITE HOLDING THE ZERO INSTANT IS REFUSED BEFORE IT IS ASKED, FROM EVERY
// NODE. A task, an ask or a patch's comment holding the zero instant as one of
// its instants is no write any task holds, and a pointer at it would reach a
// data node routed as no instant at all — accepted there — while the same write
// on a data node's own seat met the writer's refusal: one write, two answers,
// by which node asked. Refused where it is asked, it is refused alike in
// process and routed, and no data node is asked anything.
//
// Mutation: drop refusedBeforeAsking from opCreateTask and the create holding a
// zero due date is run, routed and in process.
func TestAWriteHoldingTheZeroInstantIsRefusedBeforeItIsAskedFromEveryNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	zero := time.Time{}
	writes := map[string]func(WorkWriter) error{
		"a create due at the zero instant": func(w WorkWriter) error {
			_, err := w.CreateTask(t.Context(), "op-create", worklet.Task{Project: "ENG", DueAt: &zero}, nil)
			return err
		},
		"a create whose ask was resolved at the zero instant": func(w WorkWriter) error {
			_, err := w.CreateTaskAsking(t.Context(), "op-ask", worklet.Task{Project: "ENG"},
				worklet.Comment{ID: "cm-1", Ask: "ana", ResolvedAt: &zero}, nil)
			return err
		},
		"a patch posting a comment resolved at the zero instant": func(w WorkWriter) error {
			_, err := w.UpdateTask(t.Context(), "op-comment", "t-1", "ENG", worklet.NoIfMatch,
				worklet.TaskPatch{Comment: &worklet.Comment{ID: "cm-2", ResolvedAt: &zero}},
				worklet.ChangeFields, nil)
			return err
		},
	}
	for asker, r := range map[string]*Router{
		"a node without data":      f.client,
		"the data node's own seat": f.router(t, "data-a", node),
	} {
		for name, write := range writes {
			if err := write(r.WriterAs(swe)); !errors.Is(err, worklet.ErrInvalid) {
				t.Errorf("from %s, %s was answered %v, want it refused as its content", asker, name, err)
			}
		}
	}
	for _, op := range []string{"create", "create_asking", "update"} {
		if node.askedFor(op) {
			t.Errorf("the data node was asked to %s a write holding the zero instant", op)
		}
	}
}

// A QUERY BOUNDING THE FINISHED SET BY AN INSTANT CROSSES WITH ITS BOUND. The
// bound was a pointer whose zero instant meant every finished task, and a
// pointer at the zero instant crosses as nil, which bounds nothing and leaves
// every finished task out; it is a value now, its zero unset and refused as a
// bound by the parse (worklet's TestAClosedSinceAtTheZeroInstantIsRefused), so
// what a read asks reaches the data node as asked.
//
// Mutation: tag worklet.ShowClosed.Since `json:"-"` and the read reaches the
// data node with no bound.
func TestAQueryBoundingTheFinishedSetByAnInstantCrossesWithItsBound(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	since := time.Date(2031, 4, 14, 0, 0, 0, 0, time.UTC)
	if _, err := f.client.Work().Tasks(t.Context(), worklet.Query{ShowClosed: worklet.ShowClosed{Since: since}},
		time.Now()); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	asked := slices.Clone(node.taskQueries)
	node.mu.Unlock()
	if len(asked) != 1 || !asked[0].ShowClosed.Since.Equal(since) || !asked[0].ShowClosed.Finished() {
		t.Errorf("the data node was asked %+v, want the finished set bounded at %s", asked, since)
	}
}

// A TOLD RECORD'S READ POSITION THAT NAMES NOTHING CROSSES AS NONE. A position
// at generation and sequence zero is one its type calls zero whatever stream
// it says, so the wire leaves it off and the serving node decodes the zero
// position, which names no stream — and the access writer judges a position
// that names nothing on no stream (accessdomain's
// TestAToldRecordsReadPositionThatNamesNothingIsJudgedOnNoStream), so the
// record routed is taken as the same record is in process.
//
// Mutation: drop OmitZeroStructFields from inputEncoding and the position
// crosses naming the tracker's log.
func TestAToldRecordsReadPositionThatNamesNothingCrossesAsNone(t *testing.T) {
	t.Parallel()
	told := accessdomain.NewTold{Handle: "swe", Turn: "019a0c2e-0000-7000-8000-000000000001",
		Digest: "sha256:" + strings.Repeat("0", 64), Snapshot: json.RawMessage(`{"v":1}`), Baseline: true,
		ReadThrough: statelog.Position{Stream: "CREWLET_TRACKER_LOG"}}
	raw, err := encodeInput(accessTellArgs{Told: told})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CREWLET_TRACKER_LOG") {
		t.Errorf("a told record's read position naming nothing crossed as %s, want it left off: the access "+
			"writer judges a position on its stream only where it names something", raw)
	}
}

// AN INBOX GESTURE READING THROUGH A POSITION THAT NAMES NOTHING IS DECIDED THE
// SAME WHEREVER IT IS ASKED. Routed, the wire leaves the pointer at a position
// its type calls zero off and the data node meets none; asked on the data
// node's own seat, it meets the pointer. Every reader of a gesture takes a
// position that names nothing for none (inbox's Gesture.readThrough), so the
// two are checked and applied alike — a gesture that was refused on a data
// node's own seat for naming another stream, and taken from every other node.
//
// Mutation: drop the IsZero from inbox's Gesture.readThrough and the gesture
// asked on the data node's own seat is refused where the routed one is taken.
func TestAGestureReadingThroughNothingIsDecidedTheSameWhereverItIsAsked(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	operator := Actor{Handle: "founder", Kind: worklet.AuthorOperator}
	gesture := inbox.Gesture[worklet.Reason]{Read: []string{"rec-1"},
		ReadThrough: &statelog.Position{Stream: "CREWLET_SOMETHING_ELSE"}}
	for _, r := range []*Router{f.client, f.router(t, "data-a", node)} {
		if _, err := r.WriterAs(operator).MarkInbox(t.Context(), "op-mark", "founder", statelog.Position{},
			gesture, worklet.PersonAuthority{Authorized: true}); err != nil {
			t.Fatal(err)
		}
	}
	node.mu.Lock()
	got := slices.Clone(node.gestures)
	node.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the data node decided %d gestures, want the routed one and its own seat's", len(got))
	}
	if got[0].ReadThrough != nil {
		t.Errorf("the routed gesture reached the data node reading through %v, want nothing: the wire "+
			"leaves a pointer at a zero position off", got[0].ReadThrough)
	}
	now := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	positions := map[string]uint64{"rec-1": 9}
	var decided []inbox.Marks[worklet.Reason]
	for i, g := range got {
		if err := inbox.CheckGesture(g, "CREWLET_TRACKER_LOG", worklet.Reasons, now); err != nil {
			t.Errorf("gesture %d (%+v) was refused: %v — the other was taken", i, g, err)
		}
		var m inbox.Marks[worklet.Reason]
		if err := inbox.Apply(&m, g, positions, now); err != nil {
			t.Fatal(err)
		}
		decided = append(decided, m)
	}
	if !slices.EqualFunc(decided[0].Read, decided[1].Read, func(a, b inbox.Entry) bool { return a == b }) ||
		decided[0].SeenThrough != decided[1].SeenThrough || decided[0].Generation != decided[1].Generation {
		t.Errorf("the two gestures moved the marks to %+v and %+v, want them alike", decided[0], decided[1])
	}
}
