package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// mergeArgs is a merge of site!7 at the head its caller read.
func mergeArgs() map[string]any {
	return map[string]any{"change": "site!7", "expected_head": revHead(3)}
}

// A MERGE REFUSAL NAMES EVERY UNMET PRECONDITION: a merge its target's gate
// does not allow yet is answered not_ready — its own class, never invalid —
// naming each precondition the committer found unmet and whom each waits on,
// not only the first, and telling the caller a ready_to_merge wake follows.
// The merge itself is ONE engine write of the change's target, named by its
// number, from the head its caller read, by the method it asked.
//
// Mutations: answer a gate_unmet as invalid; name only the first unmet
// precondition → red.
func TestAMergeRefusalNamesEveryUnmetPrecondition(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Merge: &code.MergeAnswer{Refusal: code.MergeRefusalGateUnmet,
			Detail: "site!7 does not meet main's merge gate", Unmet: []code.Precondition{
				{Kind: code.PreconditionApproval, State: code.PreconditionUnmet, Detail: "no approval counts",
					WaitsOn: []code.Party{{Handle: "agent-ceo", Kind: code.PartyAgent}}},
				{Kind: code.PreconditionBlocker, State: code.PreconditionUnmet, Detail: "a blocker stands",
					WaitsOn: []code.Party{{Handle: "agent-qa", Kind: code.PartyAgent}}},
			}}}, nil
	}}
	args := mergeArgs()
	args["method"] = "squash"
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
		writerTurn(t), builtin.CodeMergeChangeTool, args)
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalNotReady {
		t.Fatalf("a gate the change does not meet answered %q as %s, want not_ready", got.Output,
			tools.RefusalOf(got))
	}
	for _, says := range []string{"no approval counts", "agent-ceo", "a blocker stands", "agent-qa",
		"ready_to_merge"} {
		if !strings.Contains(got.Output, says) {
			t.Errorf("the refusal %q does not name %q", got.Output, says)
		}
	}
	writes, _ := writer.asked()
	if len(writes) != 1 {
		t.Fatalf("%d writes were sent, want one merge", len(writes))
	}
	w := writes[0]
	if w.Kind != code.WriteMergeChange || w.Ref != "refs/heads/main" || w.ExpectedHead != revHead(3) ||
		w.Change != 7 || w.Method != code.MergeMethodSquash || w.DeleteSource || w.CloseTask || w.Validate() != nil {
		t.Errorf("the merge is %+v, want site!7 squashed into main at revision 3's head, its branch kept", w)
	}
}

// A QUEUED MERGE ANSWERS ITS POSITION: a change its target's queue took is
// answered queued, with its entry and its place in the queue from 1 — and a
// close of its work item asked of it is recorded on its entry, never made by
// this call (the landing closes it).
//
// Mutation: answer a queued merge as merged; drop the place → red.
func TestAQueuedMergeAnswersItsPosition(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{write: func(w code.EngineWrite) (code.EngineWriteResult, error) {
		return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Position: codeAt(18),
			Merge: &code.MergeAnswer{Queued: true, EntryID: "entry-1", Place: 3}}, nil
	}}
	closer := &closes{}
	deps := writeDeps(newChangeHost(workedChange()), writer)
	deps.Tasks, deps.Closer = closer, closer.as
	args := mergeArgs()
	args["close_task"] = true
	got := callWrite(t, everyGrant(), codeRegistry(t, deps), writerTurn(t), builtin.CodeMergeChangeTool, args)
	if got.Failed {
		t.Fatalf("the queued merge answered %q", got.Output)
	}
	out := decoded(t, got)
	if out["state"] != "queued" || out["place"] != float64(3) || out["entry_id"] != "entry-1" ||
		out["close_task"] != true || out["commit"] != nil {
		t.Errorf("answered %q, want queued at place 3 with its entry and its recorded close", got.Output)
	}
	if writes, _ := writer.asked(); len(writes) != 1 || !writes[0].CloseTask {
		t.Errorf("the merge sent %+v, want one carrying its close", writes)
	}
	if n := closer.count(); n != 0 {
		t.Errorf("a queued merge closed its work item %d times, want none: its landing closes it", n)
	}
}

// CLOSE TASK IS STEP TWO: a direct merge asked to close its change's work
// item closes it — status done, as the caller — under the merge's operation's
// second step, so the same call made again is the same close; and a close
// that fails leaves the merge merged and says why beside it, never a refusal
// of the call.
//
// Mutations: close under the merge's own operation id; answer a failed close
// as the call's failure → red.
func TestCloseTaskIsStepTwo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		fail    error
		residue string
	}{
		"closed":         {},
		"a failed close": {fail: errors.New("the tracker refused it"), residue: "merged; ENG-42 not closed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			closer := &closes{fail: c.fail}
			deps := writeDeps(newChangeHost(workedChange()), writer)
			deps.Tasks, deps.Closer = closer, closer.as
			args := mergeArgs()
			args["close_task"] = true
			got := callWrite(t, everyGrant(), codeRegistry(t, deps), writerTurn(t), builtin.CodeMergeChangeTool,
				args)
			if got.Failed {
				t.Fatalf("the merge answered %q", got.Output)
			}
			out := decoded(t, got)
			if out["state"] != "merged" || out["commit"] != writtenOID {
				t.Errorf("answered %q, want merged at its commit", got.Output)
			}
			writes, _ := writer.asked()
			ops := closer.ops()
			if len(writes) != 1 || len(ops) != 1 || ops[0] != statelog.StepOpID(writes[0].OpID, "2") {
				t.Fatalf("the merge %+v closed under %v, want its operation's step 2", writes, ops)
			}
			if got := closer.status(); got != tracker.StatusDone {
				t.Errorf("the item was set %q, want done", got)
			}
			residue, _ := out["residue"].(string)
			switch {
			case c.residue == "" && (residue != "" || out["work_item"] != "ENG-42"):
				t.Errorf("answered %q, want ENG-42 closed", got.Output)
			case c.residue != "" && !strings.HasPrefix(residue, c.residue):
				t.Errorf("answered %q, want the residue %q", got.Output, c.residue)
			}
		})
	}
}

// CLOSE TASK IS DECIDED AS THE TRACKER'S WRITE: a merge's row admits
// everybody, but the close its step two makes is update_work_item's, the
// tracker app's — so a person who does not use the tracker merges the change
// and is told the item was not closed and why, the item untouched, while one
// who does closes it.
//
// Mutation: closeTask asking no decision before it closes → red.
func TestCloseTaskIsDecidedAsTheTrackersWrite(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		grants []iam.Grant
		closed bool
	}{
		"a person using the tracker":     {grants: iam.Apps, closed: true},
		"a person not using the tracker": {grants: []iam.Grant{iam.GrantKnowledge, iam.GrantArtifacts}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer, closer := &codeWrites{}, &closes{}
			deps := writeDeps(newChangeHost(workedChange()), writer)
			deps.Tasks, deps.Closer, deps.Actor = closer, closer.as, builtin.PrincipalActor
			operator := builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)}
			person := iam.WithPrincipal(context.Background(), iam.Principal{
				ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee5"),
				Kind: iam.KindPerson, Login: "jane.founder", Seat: "founder", Via: "pat:jane-laptop",
				Stage: iam.StageActive, Grants: c.grants,
			})
			args := mergeArgs()
			args["close_task"] = true
			got, err := operatorToolOver(t, operator, builtin.CodeMergeChangeTool).CallForTurn(person, nil, args)
			if err != nil || got.Failed {
				t.Fatalf("the person's merge answered %v %q", err, got.Output)
			}
			out := decoded(t, got)
			residue, _ := out["residue"].(string)
			switch {
			case out["state"] != "merged":
				t.Errorf("answered %q, want merged whatever became of the close", got.Output)
			case c.closed && (closer.count() != 1 || residue != ""):
				t.Errorf("answered %q after %d closes, want ENG-42 closed", got.Output, closer.count())
			case !c.closed && (closer.count() != 0 || !strings.HasPrefix(residue, "merged; ENG-42 not closed") ||
				!strings.Contains(residue, builtin.UpdateWorkItemTool)):
				t.Errorf("answered %q after %d closes, want ENG-42 left open, naming update_work_item's refusal",
					got.Output, closer.count())
			}
		})
	}
}

// A DEQUEUE TAKES THE CHANGE'S OWN ENTRY OUT: code_dequeue_change finds the
// change's live entry in its target's queue, past a first page of others',
// and asks ONE dequeue of that entry — as its caller, naming no run.
//
// Mutation: dequeue the first entry of the page; keep the turn's run on the
// asker → red.
func TestADequeueTakesTheChangesOwnEntryOut(t *testing.T) {
	t.Parallel()
	host := newChangeHost(siteChange())
	changes := host.changes
	host.changes = func(q code.ReadQuery) (code.ReadAnswer, error) {
		if q.Kind != code.ReadQueue {
			return changes(q)
		}
		if q.Target != "refs/heads/main" {
			return code.ReadAnswer{}, errors.New("a queue read of another target")
		}
		if q.Cursor == "" {
			return code.ReadAnswer{Queue: []code.QueueRow{{EntryID: otherEntryID, ChangeID: "another"}},
				Next: "p2"}, nil
		}
		return code.ReadAnswer{Queue: []code.QueueRow{{EntryID: siteEntryID, ChangeID: siteChangeID}}}, nil
	}
	writer := &codeWrites{}
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
		builtin.CodeDequeueChangeTool, map[string]any{"change": "site!7"})
	if got.Failed {
		t.Fatalf("the dequeue answered %q", got.Output)
	}
	asked := writer.queuesAsked()
	if len(asked) != 1 || asked[0].Dequeue == nil || asked[0].Dequeue.EntryID != siteEntryID ||
		asked[0].Dequeue.RepoID != siteID || asked[0].Asker.Run.TurnID != "" || asked[0].Validate() != nil {
		t.Errorf("the dequeue asked %+v, want site!7's own entry out as its caller", asked)
	}
}

// THE MERGING GUIDANCE NAMES THE MERGE: every tool the executor's guidance on
// merging a change is gated on is one this package registers as a change
// write.
func TestTheCodeMergingGuidanceNamesTheMergeTool(t *testing.T) {
	t.Parallel()
	if len(prompts.CodeMergeTools()) == 0 {
		t.Fatal("the guidance names no tool, so this certifies nothing")
	}
	for _, name := range prompts.CodeMergeTools() {
		if !slices.Contains(builtin.ChangeWrites(), name) {
			t.Errorf("the executor's merging guidance is gated on %s, which is no change write", name)
		}
	}
}

// workedChange is site!7 carrying the native work item ENG-42.
func workedChange() *changeFixture {
	f := siteChange()
	f.detail.WorkItem = &types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42", Project: "ENG"}
	return f
}

// closes is the native tracker as a merge's step two reads and closes one
// task: ENG-42 in progress at version 4, its close failing with fail.
type closes struct {
	fail  error
	mu    sync.Mutex
	asked []string
	set   tracker.Status
}

func (c *closes) Task(_ context.Context, idOrKey string, _ tracker.DetailWants,
	_ statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{Task: tracker.Task{ID: idOrKey, Key: "ENG-42", Project: "ENG", Version: 4,
		Status: tracker.StatusInProgress}}, nil
}

func (c *closes) as(builtin.Actor) builtin.WorkWriter { return c }

func (c *closes) CreateTask(context.Context, string, tracker.Task, *tracker.Notify) (tracker.WriteResult, error) {
	return tracker.WriteResult{}, errors.New("a merge creates no task")
}

func (c *closes) CreateTaskAsking(context.Context, string, tracker.Task, tracker.Comment,
	*tracker.Notify) (tracker.WriteResult, error) {
	return tracker.WriteResult{}, errors.New("a merge creates no task")
}

func (c *closes) UpdateTask(_ context.Context, opID, _, _ string, _ uint64, patch tracker.TaskPatch,
	_ tracker.ChangeKind, _ *tracker.Notify) (tracker.WriteResult, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, opID)
	if patch.Status != nil {
		c.set = *patch.Status
	}
	if c.fail != nil {
		return tracker.WriteResult{}, c.fail
	}
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (c *closes) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

func (c *closes) count() int { return len(c.ops()) }

func (c *closes) status() tracker.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.set
}
