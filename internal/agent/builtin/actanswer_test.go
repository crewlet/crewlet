package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The operator catalogue's WRITES are what the dashboard's act transport and
// an operator's assistant both call, and both read the same two things off a
// successful answer: the call's `outcome` and its `position`, at the TOP of
// the answer. A tool that stated them only per record handed the act
// transport nothing, and it reported `applied` at no position over records
// whose outcome may have been `unknown`. And both retry an `unknown` write
// under the same request id, which is one write only if every operation the
// call derives is derived from that id.
//
// So these walk EVERY write the catalogue serves rather than the ones
// somebody remembered: a tool added without an entry in [everyWriteCall]
// fails here until it has one.

// everyWriteCall is one successful call of every operator write, against the
// fakes [writeSurface] builds.
var everyWriteCall = map[string]map[string]any{
	tracker.CreateWorkItemTool: {"title": "Rotate the key", "project": "ENG",
		"labels": []any{"ops"}, "labels_create_missing": true, "waiting_on": []any{"ENG-2"}},
	tracker.UpdateWorkItemTool: {"item": "ENG-1", "status": "in_progress",
		"waiting_on": map[string]any{"add": []any{"ENG-3"}}},
	tracker.CommentOnWorkTool:      {"item": "ENG-1", "body": "on it"},
	tracker.MergeWorkItemTool:      {"item": "ENG-2", "into": "ENG-1"},
	tracker.SaveWorkViewTool:       {"container": "project:ENG", "name": "Mine", "type": "list"},
	tracker.WriteWorkCatalogueTool: {"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}, "fields": []any{}},
	tracker.SetPrioritiesTool:      {"items": []any{"ENG-1"}},
	tracker.SetPinsTool:            {"views": map[string]any{"add": []any{"v-1"}}},
	tracker.MarkInboxTool:          {"read": []any{"r-1"}},
	tracker.WriteProjectTool: {"project": "ENG",
		"tags_add": []any{map[string]any{"slug": "ops", "label": "Ops"}}, "default_assignee": ""},
	tracker.RemoveWorkItemTool:  {"item": "ENG-1"},
	tracker.RestoreWorkItemTool: {"item": "ENG-5"},
	tracker.MoveWorkItemTool: {"item": "ENG-1", "before": "ENG-2",
		"status": "in_progress", "if_match": 7},
	builtin.WritePageTool:     {"title": "Runbook", "body": "step one", "container": "ENG"},
	builtin.SavePageTool:      {"page": "p1", "base_version": 4, "body": "step two", "title": "Deploy Book"},
	builtin.CommentOnPageTool: {"page": "p1", "body": "is this current?"},
}

// EVERY WRITE ANSWERS FOR EVERY RECORD IT APPENDED, AT THE TOP. The writers
// here answer `pending`, each at a later position than the last, so a tool
// that lifted its first record's position — or answered per record only — is
// caught: the top of its answer must be `pending` at the LAST position the
// call was handed.
func TestEveryOperatorWriteAnswersForEveryRecordItAppended(t *testing.T) {
	t.Parallel()
	w := newLedgerFake(statelog.OutcomePending)
	catalogue := writeSurface(w)
	for name, tool := range catalogue {
		t.Run(name, func(t *testing.T) {
			answer, last := w.call(t, tool, "a1b2c3d4-0000-4000-8000-000000000001", everyWriteCall[name])
			if answer["outcome"] != string(statelog.OutcomePending) {
				t.Errorf("%s answered outcome %v at the top, want pending — its "+
					"records were all pending, and the transport lifts this key "+
					"as the call's answer: %v", name, answer["outcome"], answer)
			}
			if answer["position"] != last.String() {
				t.Errorf("%s answered position %v at the top, want %s — the "+
					"LAST record it appended, which is what a caller barriers "+
					"on before its next read: %v", name, answer["position"], last, answer)
			}
		})
	}
}

// AND A RETRY IS THE SAME OPERATIONS. Every write sent twice under one request
// key derives exactly the operations (and, through the knowledge base, the
// call keys) it derived the first time — so the broker collapses the second
// into the first — and under another request key derives none of them.
func TestARetriedOperatorWriteDerivesTheSameOperations(t *testing.T) {
	t.Parallel()
	w := newLedgerFake(statelog.OutcomeApplied)
	catalogue := writeSurface(w)
	for name, tool := range catalogue {
		t.Run(name, func(t *testing.T) {
			first := w.opsOf(t, tool, "11111111-0000-4000-8000-000000000001", everyWriteCall[name])
			retry := w.opsOf(t, tool, "11111111-0000-4000-8000-000000000001", everyWriteCall[name])
			other := w.opsOf(t, tool, "22222222-0000-4000-8000-000000000002", everyWriteCall[name])
			if len(first) == 0 {
				t.Fatalf("%s reached no writer at all, so this case certifies nothing", name)
			}
			if !slices.Equal(first, retry) {
				t.Errorf("%s derived %v and then, retried under the same request "+
					"id, %v — every operation that differs is written twice",
					name, first, retry)
			}
			for _, op := range other {
				if slices.Contains(first, op) {
					t.Errorf("%s wrote %q under two different request ids — the "+
						"second gesture is collapsed into the first and answered "+
						"`applied` without landing", name, op)
				}
			}
		})
	}
}

// THE LESS CERTAIN OUTCOME WINS, over both of write_project's records and
// both of write_work_catalogue's. The first record is `unknown` (and, as an
// unknown has, at no position) and the second is applied: the call is
// `unknown` at the second's position, never `applied`.
func TestATwoRecordWriteAnswersItsLessCertainOutcome(t *testing.T) {
	t.Parallel()
	for _, name := range []string{tracker.WriteProjectTool, tracker.WriteWorkCatalogueTool} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			w.script = []statelog.Outcome{statelog.OutcomeUnknown, statelog.OutcomeApplied}
			answer, last := w.call(t, writeSurface(w)[name], "33333333-0000-4000-8000-000000000003",
				everyWriteCall[name])
			if answer["outcome"] != string(statelog.OutcomeUnknown) {
				t.Errorf("%s answered %v over an unknown record and an applied one, "+
					"want unknown — an unknown write is never reported applied: %v",
					name, answer["outcome"], answer)
			}
			if answer["position"] != last.String() {
				t.Errorf("%s answered position %v, want the applied record's %s",
					name, answer["position"], last)
			}
		})
	}
}

// A SECOND RECORD REFUSED AFTER A FIRST THAT LANDED says so. The first list is
// replaced on every node, so "the change was NOT made" would send a caller to
// redo — or to report undone — a change that happened.
func TestARefusedSecondRecordReportsTheFirstLanded(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]string{
		tracker.WriteProjectTool:       "The tag change was written",
		tracker.WriteWorkCatalogueTool: "The types were replaced",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			w.failAt = 2
			got, err := writeSurface(w)[name].Call(withKey(t.Context(), "44444444-0000-4000-8000-000000000004"),
				everyWriteCall[name])
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !got.Failed || got.Refusal != tools.RefusalConflict {
				t.Fatalf("%s answered %+v, want the second record's conflict", name, got)
			}
			if !strings.Contains(got.Output, landed) || !strings.Contains(got.Output, "CREWLET_TRACKER_LOG@1:1") {
				t.Errorf("%s's refusal does not say the first record landed, and where: %q",
					name, got.Output)
			}
		})
	}
}

// ---- the surface ------------------------------------------------------- //

type keyCtx struct{}

func withKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyCtx{}, key)
}

func keyOf(ctx context.Context) string {
	key, _ := ctx.Value(keyCtx{}).(string)
	return key
}

// writeSurface is the operator catalogue's writes over one recording fake,
// with the operator actor carrying the request key the context names — the
// shape the operator surface builds.
func writeSurface(w *ledgerFake) map[string]tools.Callable {
	trk := newFakeTracker()
	trk.tasks["ENG-5"] = tracker.TaskDetail{
		Task: tracker.Task{ID: "id-5", Key: "ENG-5", Project: "ENG",
			Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
			Removed: &tracker.Tombstone{By: "ops", At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}},
		Complete: true,
	}
	kb := newFakeKB()
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: func(builtin.Actor) builtin.WorkWriter { return w },
			Merges:          func(builtin.Actor) builtin.WorkMerger { return w },
			Dependencies:    func(builtin.Actor) builtin.WorkDepender { return w },
			Search:          trk,
			ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return w },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return w },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return w },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return w },
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return w },
			Mover:           func(builtin.Actor) builtin.WorkMover { return w },
			Inbox:           trk,
			Actor: func(ctx context.Context, _ *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator,
					OperatorID: "ops", RequestKey: keyOf(ctx)}, nil
			},
		},
		Pages: builtin.PageDeps{
			Reader: kb, Writer: w,
			Actor: func(context.Context, *turnctx.Turn) (pages.Actor, error) {
				return pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops"}, nil
			},
			RequestKey: keyOf,
		},
	}) {
		if !mcp.ReadOnlyProven(builtin.AnnotationsFor(tool.Name())) {
			out[tool.Name()] = tool
		}
	}
	return out
}

// TestTheWriteTableCoversEveryOperatorWrite keeps [everyWriteCall] honest in
// both directions: a write the catalogue serves with no entry is a write the
// two cases above never call, and an entry the catalogue does not serve is a
// case certifying a tool nobody can reach.
func TestTheWriteTableCoversEveryOperatorWrite(t *testing.T) {
	t.Parallel()
	served := writeSurface(newLedgerFake(statelog.OutcomeApplied))
	var missing, extra []string
	for name := range served {
		if _, ok := everyWriteCall[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range everyWriteCall {
		if _, ok := served[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("the operator writes and this table disagree: served with no "+
			"call %v, a call for nothing served %v", missing, extra)
	}
}

// ---- the recording writer ---------------------------------------------- //

// ledgerFake is every write seam the operator catalogue has, recording the
// operation (or, for a page, the call key) each write arrives with and
// answering each at the next position on its log.
type ledgerFake struct {
	mu      sync.Mutex
	outcome statelog.Outcome
	// script, when set, is the outcome of the Nth write in order.
	script []statelog.Outcome
	// failAt refuses the Nth write with a conflict, 1-based; 0 never.
	failAt int
	seq    uint64
	calls  int
	ops    []string
}

func newLedgerFake(outcome statelog.Outcome) *ledgerFake {
	return &ledgerFake{outcome: outcome}
}

// call runs one tool under a request key and answers its decoded result and
// the last position any write in the call was handed.
func (f *ledgerFake) call(t *testing.T, tool tools.Callable, key string,
	args map[string]any) (map[string]any, statelog.Position) {

	t.Helper()
	got, err := tool.Call(withKey(t.Context(), key), args)
	if err != nil || got.Failed {
		t.Fatalf("%s failed: %v %q", tool.Name(), err, got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered something that is not JSON: %q", tool.Name(), got.Output)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return answer, f.position(f.seq)
}

// opsOf runs one tool under a request key and answers the operations it
// derived.
func (f *ledgerFake) opsOf(t *testing.T, tool tools.Callable, key string,
	args map[string]any) []string {

	t.Helper()
	f.mu.Lock()
	from := len(f.ops)
	f.mu.Unlock()
	if got, err := tool.Call(withKey(t.Context(), key), args); err != nil || got.Failed {
		t.Fatalf("%s failed: %v %q", tool.Name(), err, got.Output)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops[from:])
}

func (f *ledgerFake) position(seq uint64) statelog.Position {
	return statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: seq}
}

// record takes one write and answers it.
func (f *ledgerFake) record(op string) (statelog.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return statelog.Result{}, fmt.Errorf("%w: ENG kept changing", statelog.ErrConflict)
	}
	f.ops = append(f.ops, op)
	outcome := f.outcome
	if f.calls <= len(f.script) {
		outcome = f.script[f.calls-1]
	}
	if outcome == statelog.OutcomeUnknown {
		// AN UNKNOWN HAS NO POSITION, which is the whole content of it.
		return statelog.Result{Outcome: outcome, OpID: op}, nil
	}
	f.seq++
	at := f.position(f.seq)
	return statelog.Result{Outcome: outcome, Position: at, OpID: op, Version: at.Packed()}, nil
}

func (f *ledgerFake) written(op string) (tracker.WriteResult, error) {
	r, err := f.record(op)
	return tracker.WriteResult{Result: r}, err
}

func (f *ledgerFake) CreateTask(_ context.Context, opID string, _ tracker.Task,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	r, err := f.written(opID)
	r.Key = "ENG-9"
	return r, err
}

func (f *ledgerFake) CreateTaskAsking(ctx context.Context, opID string, task tracker.Task,
	_ tracker.Comment, notify *tracker.Notify) (tracker.WriteResult, error) {
	return f.CreateTask(ctx, opID, task, notify)
}

func (f *ledgerFake) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	_ tracker.TaskPatch, _ tracker.ChangeKind, _ *tracker.Notify) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) Depend(_ context.Context, opID string, _ tracker.DependencyChange,
	_ tracker.Leads) (tracker.DependencyResult, error) {
	r, err := f.written(opID)
	return tracker.DependencyResult{WriteResult: r}, err
}

func (f *ledgerFake) MergeDuplicates(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) RestoreTask(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return f.written(opID)
}

// MoveTask appends the two records a drag across lanes is, each at its own
// derived step — the way the tracker's own sequence derives them.
func (f *ledgerFake) MoveTask(_ context.Context, opID string, move tracker.Move,
	_ *tracker.Notify) (tracker.MoveResult, error) {
	var out tracker.MoveResult
	if move.Status != nil {
		lane, err := f.written(opID + ".lane")
		if err != nil {
			return tracker.MoveResult{}, err
		}
		out.Lane = lane
	}
	order, err := f.written(opID + ".order")
	if err != nil {
		return tracker.MoveResult{}, err
	}
	out.Order, out.Rank = order, "a0V"
	return out, nil
}

func (f *ledgerFake) WriteView(_ context.Context, opID string, _ tracker.View) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteTypes(_ context.Context, opID string, _ []tracker.TaskType) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteFields(_ context.Context, opID string, _ []tracker.FieldDef) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) MarkInbox(_ context.Context, opID, _ string,
	_ tracker.InboxGesture) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WritePins(_ context.Context, opID, _ string,
	_ tracker.PinGesture) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ *uint64, _ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteProject(_ context.Context, opID, _ string, _ tracker.ProjectEdit,
	_ tracker.ProjectAuthority) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteTags(_ context.Context, opID, _ string, _ tracker.TagEdit,
	_ tracker.TagAuthority) (tracker.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) EnsureTags(_ context.Context, opID, _ string, tags []string) ([]string, []string, error) {
	if _, err := f.record(opID); err != nil {
		return nil, nil, err
	}
	return tags, nil, nil
}

// The knowledge base's writes. Their operation is derived INSIDE the store
// from the call key, so the key is what is recorded — keyed with the verb, so
// a save and its rename under one key are still two entries.

func (f *ledgerFake) pageWrite(verb string, key pages.CallKey) (pages.Written, error) {
	r, err := f.record(verb + ":" + key.String())
	return pages.Written{Page: pages.Page{ID: "p1", Version: 5}, Revision: uint64(r.Position.Packed()),
		ChangeID: r.OpID, Outcome: r}, err
}

func (f *ledgerFake) Create(_ context.Context, _ pages.Actor, in pages.NewPage) (pages.Written, error) {
	return f.pageWrite("create", in.CallKey)
}

func (f *ledgerFake) SavePage(_ context.Context, _ pages.Actor, _ string, save pages.Save) (pages.Written, error) {
	return f.pageWrite("save", save.CallKey)
}

func (f *ledgerFake) Rename(_ context.Context, _ pages.Actor, _, _ string, _ bool,
	key pages.CallKey) (pages.Written, error) {
	return f.pageWrite("rename", key)
}

func (f *ledgerFake) EditComment(_ context.Context, _ pages.Actor, _, commentID, _ string,
	key pages.CallKey) (pages.Comment, pages.Written, error) {
	w, err := f.pageWrite("edit", key)
	return pages.Comment{ID: commentID}, w, err
}

func (f *ledgerFake) Comment(_ context.Context, _ pages.Actor, _ string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	w, err := f.pageWrite("comment", in.CallKey)
	return pages.Comment{ID: "m1"}, w, err
}

// TWO DIFFERENT CALLS UNDER ONE KEY ARE TWO OPERATIONS. A turn's calls share
// its work key, and a verb that writes one shared object — a view, a
// project's tags or policy, the catalogue's lists — derived its operation
// from the object alone, so inside the broker's duplicate window the second
// change was collapsed into the first and answered `applied` without landing.
// save_work_view did it for EVERY caller: `view-<id>` was one fixed operation
// for the life of the view. The same call sent again is still one operation.
func TestTwoDifferentCallsUnderOneKeyAreTwoOperations(t *testing.T) {
	t.Parallel()
	cases := map[string][2]map[string]any{
		tracker.SaveWorkViewTool: {
			{"id": "v-1", "container": "project:ENG", "name": "Mine", "type": "list"},
			{"id": "v-1", "container": "project:ENG", "name": "Mine, this week", "type": "list"},
		},
		tracker.WriteProjectTool: {
			{"project": "ENG", "tags_add": []any{map[string]any{"slug": "ops", "label": "Ops"}}},
			{"project": "ENG", "tags_archive": []any{"ops"}},
		},
		tracker.WriteWorkCatalogueTool: {
			{"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}},
			{"types": []any{map[string]any{"slug": "bug", "name": "Defect"}}},
		},
		tracker.CreateWorkItemTool: {
			{"title": "One", "project": "ENG", "labels": []any{"ops"}, "labels_create_missing": true},
			{"title": "Two", "project": "ENG", "labels": []any{"sre"}, "labels_create_missing": true},
		},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			tool := writeSurface(w)[name]
			const key = "55555555-0000-4000-8000-000000000005"
			first := w.opsOf(t, tool, key, calls[0])
			second := w.opsOf(t, tool, key, calls[1])
			again := w.opsOf(t, tool, key, calls[1])
			for _, op := range second {
				if slices.Contains(first, op) {
					t.Errorf("two different %s calls under one key both wrote %q — "+
						"the broker collapses the second and it never lands", name, op)
				}
			}
			if !slices.Equal(second, again) {
				t.Errorf("one %s call sent twice under one key wrote under %v and %v",
					name, second, again)
			}
		})
	}
}
