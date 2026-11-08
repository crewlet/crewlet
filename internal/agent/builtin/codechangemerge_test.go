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
	"github.com/crewlet/crewlet/internal/worklet"
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
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
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
		w.Change != 7 || w.Method != code.MergeMethodSquash || w.DeleteSource != nil || w.CloseTask ||
		w.Validate() != nil {
		t.Errorf("the merge is %+v, want site!7 squashed into main at revision 3's head, its branch's "+
			"deletion left to the Gitlet node", w)
	}
}

// AN UNSAID DELETE_BRANCH IS THE GITLET NODE'S: a merge of a seat's branch whose
// caller says nothing of the branch sends no word of it — the Gitlet node that
// judges the merge decides the default on that snapshot, from the gate it
// reads for the merge anyway — reads no merge gate for it, and answers the
// deletion the Gitlet node decided; a caller that says keep sends keep, and is
// answered what the Gitlet node did.
//
// Mutations: the tool deciding the default itself — sending false, or
// reading the target's gates first — red; the answer ignoring the Gitlet node's
// word, red.
func TestAnUnsaidDeleteBranchIsTheGitletNodes(t *testing.T) {
	t.Parallel()
	keep := false
	for name, c := range map[string]struct {
		said    map[string]any
		sent    *bool
		deleted bool
	}{
		"unsaid":    {deleted: true},
		"said keep": {said: map[string]any{"delete_branch": false}, sent: &keep},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seats := siteChange()
			seats.detail.SourceRef = "refs/heads/seats/agent-ceo/retry"
			host := newChangeHost(seats)
			writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) {
				return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
					Merge: &code.MergeAnswer{DeleteSource: c.deleted}}, nil
			}}
			args := mergeArgs()
			for k, v := range c.said {
				args[k] = v
			}
			got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
				builtin.CodeMergeChangeTool, args)
			if got.Failed {
				t.Fatalf("the merge answered %q", got.Output)
			}
			writes, _ := writer.asked()
			if len(writes) != 1 {
				t.Fatalf("%d writes were sent, want one merge", len(writes))
			}
			if sent := writes[0].DeleteSource; (sent == nil) != (c.sent == nil) || (sent != nil && *sent != *c.sent) {
				t.Errorf("the merge sent delete_source %v, want %v", sent, c.sent)
			}
			reads, _, _ := host.asked()
			if slices.ContainsFunc(reads, func(q code.ReadQuery) bool { return q.Gates }) {
				t.Error("the merge read the target's gates: the default is the Gitlet node's to decide")
			}
			if out := decoded(t, got); (out["deleted_branch"] == true) != c.deleted {
				t.Errorf("answered %q, want deleted_branch %v as the Gitlet node answered it", got.Output, c.deleted)
			}
		})
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
	got := callWrite(everyGrant(), t, codeRegistry(t, deps), writerTurn(t), builtin.CodeMergeChangeTool, args)
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
		"a failed close": {fail: errors.New("Worklet refused it"), residue: "merged; ENG-42 not closed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			closer := &closes{fail: c.fail}
			deps := writeDeps(newChangeHost(workedChange()), writer)
			deps.Tasks, deps.Closer = closer, closer.as
			args := mergeArgs()
			args["close_task"] = true
			got := callWrite(everyGrant(), t, codeRegistry(t, deps), writerTurn(t), builtin.CodeMergeChangeTool,
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
			if got := closer.status(); got != worklet.StatusDone {
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

// CLOSE TASK IS DECIDED AS WORKLET'S WRITE: a merge's row takes Gitlet,
// but the close its step two makes is update_work_item's, the Worklet
// app's — so a person who uses Gitlet and not Worklet merges the change
// and is told the item was not closed and why, the item untouched, while one
// who does closes it.
//
// Mutation: closeTask asking no decision before it closes → red.
func TestCloseTaskIsDecidedAsWorkletsWrite(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		grants []iam.Grant
		closed bool
	}{
		"a person using Worklet": {grants: iam.Apps, closed: true},
		"a person not using Worklet": {grants: []iam.Grant{iam.GrantDoclet, iam.GrantArtifacts, iam.GrantChatlet,
			iam.GrantGitlet}},
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

// A DEQUEUE TAKES THE CHANGE'S OWN ENTRY OUT, FROM ITS ONE READ: the call's
// read of the change names its live entry on the queue precondition, and
// code_dequeue_change asks ONE dequeue of that entry — as its caller, naming
// no run — with no read of the queue and no read of the change but the
// conversation's one; a change that read names in no queue is refused
// conflict, nothing asked.
//
// Mutations: page the target's queue for the entry, as the tool once did —
// the read the fake answers names no entry and the dequeue is refused, and a
// queue read is counted, red; keep the turn's run on the asker → red.
func TestADequeueTakesTheChangesOwnEntryOut(t *testing.T) {
	t.Parallel()
	host := newChangeHost(siteChange().queued())
	writer := &codeWrites{}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
		builtin.CodeDequeueChangeTool, map[string]any{"change": "site!7"})
	if got.Failed {
		t.Fatalf("the dequeue answered %q", got.Output)
	}
	asked := writer.queuesAsked()
	if len(asked) != 1 || asked[0].Dequeue == nil || asked[0].Dequeue.EntryID != siteEntryID ||
		asked[0].Dequeue.RepoID != siteID || asked[0].Asker.Run.TurnID != "" || asked[0].Validate() != nil {
		t.Errorf("the dequeue asked %+v, want site!7's own entry out as its caller", asked)
	}
	reads, _, _ := host.asked()
	if n, q := readsOf(reads, code.ReadChange), readsOf(reads, code.ReadQueue); n != 1 || q != 0 {
		t.Errorf("the dequeue read the change %d times and the queue %d, want the change once and the queue "+
			"never: its one read names the entry", n, q)
	}

	writer = &codeWrites{}
	got = callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
		writerTurn(t), builtin.CodeDequeueChangeTool, map[string]any{"change": "site!7"})
	// NOT FOUND, NEVER CONFLICT: an entry that is not live never becomes
	// live again by itself, so a caller told to retry would ask for ever; and
	// it carries the committer's own word, not_queued, as its refusal would.
	var word *builtin.HostRefusal
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalNotFound || len(writer.queuesAsked()) != 0 ||
		!errors.As(got.Cause, &word) || word.Refusal != string(code.MergeRefusalNotQueued) {
		t.Errorf("a change in no queue answered %q (%s, %v) after %d dequeues, want not_found under "+
			"not_queued and none asked", got.Output, tools.RefusalOf(got), got.Cause, len(writer.queuesAsked()))
	}
}

// A MERGE OF AN ENDED CHANGE IS A CONFLICT, as every code tool's write of one
// is: the change is in a state that refuses the merge until somebody reopens
// it (or, merged, for good), which internal/mcp's one rule classes conflict,
// never invalid — the request names the change it means — nor anything a wait
// clears. It is refused before anything is built, under the committer's own
// word, change_not_open, so a surface answering in its own words reads the
// word and not the sentence.
//
// Mutation: refuse it as invalid again → red.
func TestAMergeOfAnEndedChangeIsAConflict(t *testing.T) {
	t.Parallel()
	for _, state := range []code.ChangeState{code.ChangeStateClosed, code.ChangeStateMerged} {
		ended := siteChange()
		ended.detail.State = state
		writer := &codeWrites{}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(ended), writer)),
			writerTurn(t), builtin.CodeMergeChangeTool, mergeArgs())
		writes, _ := writer.asked()
		var host *builtin.HostRefusal
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict || len(writes) != 0 ||
			!errors.As(got.Cause, &host) || host.Refusal != string(code.MergeRefusalChangeNotOpen) {
			t.Errorf("a merge of a %s change answered %q (%s, %v) after %d writes, want conflict under "+
				"change_not_open and none sent", state, got.Output, tools.RefusalOf(got), got.Cause, len(writes))
		}
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
	f.detail.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42", Project: "ENG"}
	return f
}

// closes is Worklet as a merge's step two reads and closes one
// task: ENG-42 in progress at version 4, its close failing with fail.
type closes struct {
	fail  error
	mu    sync.Mutex
	asked []string
	set   worklet.Status
}

func (c *closes) Task(_ context.Context, idOrKey string, _ worklet.DetailWants,
	_ statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{Task: worklet.Task{ID: idOrKey, Key: "ENG-42", Project: "ENG", Version: 4,
		Status: worklet.StatusInProgress}}, nil
}

func (c *closes) as(builtin.Actor) builtin.WorkWriter { return c }

func (c *closes) CreateTask(context.Context, string, worklet.Task, *worklet.Notify) (worklet.WriteResult, error) {
	return worklet.WriteResult{}, errors.New("a merge creates no task")
}

func (c *closes) CreateTaskAsking(context.Context, string, worklet.Task, worklet.Comment,
	*worklet.Notify) (worklet.WriteResult, error) {
	return worklet.WriteResult{}, errors.New("a merge creates no task")
}

func (c *closes) UpdateTask(_ context.Context, opID, _, _ string, _ uint64, patch worklet.TaskPatch,
	_ worklet.ChangeKind, _ *worklet.Notify) (worklet.WriteResult, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, opID)
	if patch.Status != nil {
		c.set = *patch.Status
	}
	if c.fail != nil {
		return worklet.WriteResult{}, c.fail
	}
	return worklet.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (c *closes) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

func (c *closes) count() int { return len(c.ops()) }

func (c *closes) status() worklet.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.set
}
