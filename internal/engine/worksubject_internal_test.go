package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Which work item a turn is on — the rules in worksubject.go, driven through
// the frames that publish a turn's opening and closing records, because what a
// reader sees is those records rather than the resolver.

// nativeTask is the item the tracker cases wake a seat about.
var nativeTask = types.WorkItem{
	Backend: types.WorkNative, ID: "task-1", Key: "ENG-1", Project: "ENG",
}

// trackerWake is a native tracker wake about task-1, as the parser writes it.
func trackerWake() *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: tracker.Source, SourceEventType: "assigned",
		Subject: "ENG-1: fix the build", Addressed: true,
		Metadata: map[string]string{
			tracker.MetaTaskID: "task-1", tracker.MetaTaskKey: "ENG-1",
			tracker.MetaProject: "ENG", tracker.MetaObject: "task",
			tracker.MetaObjectID: "task-1",
		},
	}, events.TraceContext{})
}

// turnRecords runs one dispatch and returns its three turn-level records.
func turnRecords(t *testing.T, trigger *events.Event) (
	types.AgentTurnStarted, types.AgentTurnCompleted, types.TurnCompleted,
) {
	t.Helper()
	e, p := starting(t, refusingModels(t))
	if _, err := e.runTurn(t.Context(), Request{
		RunID: "run-1", Handle: "swe", WorkKey: "wk-1",
		// PAST THE DEPTH CAP, so the loop ends at its guard with no model
		// asked anything — see [starting].
		Depth:  3,
		Events: []*events.Event{trigger},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	start := only[*types.AgentTurnStarted](t, p, "agent_turn_started")
	summary := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	return *start, *summary, *done
}

// charged asserts every record of a turn names want under basis.
func charged(t *testing.T, want types.WorkItem, basis types.WorkItemBasis,
	start types.AgentTurnStarted, summary types.AgentTurnCompleted, done types.TurnCompleted,
) {
	t.Helper()
	for name, got := range map[string]struct {
		item  *types.WorkItem
		basis types.WorkItemBasis
	}{
		"agent_turn_started":   {start.WorkItem, start.WorkItemBasis},
		"agent_turn_completed": {summary.WorkItem, summary.WorkItemBasis},
		"turn_completed":       {done.WorkItem, done.WorkItemBasis},
	} {
		if got.item == nil || *got.item != want || got.basis != basis {
			t.Errorf("%s names %+v (%q), want %+v (%q)", name, got.item, got.basis, want, basis)
		}
	}
}

// A TRACKER WAKE IS ON THE TASK IT IS ABOUT, from the turn's first record.
//
// The trigger names the item by construction, so the turn is charged to it at
// dispatch and says so on its start — before any phase has run — rather than
// being discovered at the end.
func TestATrackerWakeNamesItsItem(t *testing.T) {
	t.Parallel()
	start, summary, done := turnRecords(t, trackerWake())
	charged(t, nativeTask, types.BasisTrigger, start, summary, done)
}

// A WAKE ABOUT A PERSON'S OWN LIST NAMES NO ITEM.
//
// The parser writes a task only when the wake is about one; a wake about
// somebody's priorities carries the person as its object, and charging the
// turn to that id would charge it to a handle.
func TestAPersonWakeHasNoItem(t *testing.T) {
	t.Parallel()
	start, summary, done := turnRecords(t, events.New(types.ExternalNotification{
		NotificationSource: tracker.Source, SourceEventType: "prioritised",
		Metadata: map[string]string{
			tracker.MetaObject: "person", tracker.MetaObjectID: "ada",
		},
	}, events.TraceContext{}))
	if start.WorkItem != nil || summary.WorkItem != nil || done.WorkItem != nil {
		t.Errorf("a person wake was charged to %+v / %+v / %+v",
			start.WorkItem, summary.WorkItem, done.WorkItem)
	}
}

// A VENDOR WAKE IS ON ITS ISSUE, BY THE IDENTITY THAT SURVIVES A RENAME.
//
// Each source's own prompt says what its metadata names. The identity is the
// part that does not move — a Jira issue id rather than its key, a code host's
// repository or project id rather than its path — and a delivery missing that
// part names no item rather than one keyed on a label.
func TestAVendorWakeNamesItsIssue(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		source string
		meta   map[string]string
		want   *types.WorkItem
	}{
		"a jira issue": {jira.Backend, map[string]string{
			"issue_key": "ENG-12", "issue_id": "10042", "project": "ENG",
		}, &types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-12", Project: "ENG"}},
		"a jira delivery with no issue id": {jira.Backend, map[string]string{
			"issue_key": "ENG-12", "project": "ENG",
		}, nil},
		"a github pull request": {github.Backend, map[string]string{
			"repo": "acme/api", github.RepoIDField: "777", "pr_number": "88",
		}, &types.WorkItem{Backend: types.WorkGitHub, ID: "777#88", Key: "acme/api#88", Project: "acme/api"}},
		"a github issue": {github.Backend, map[string]string{
			"repo": "acme/api", github.RepoIDField: "777", "issue_number": "12",
		}, &types.WorkItem{Backend: types.WorkGitHub, ID: "777#12", Key: "acme/api#12", Project: "acme/api"}},
		"a github build on a branch": {github.Backend, map[string]string{
			"repo": "acme/api", github.RepoIDField: "777",
		}, nil},
		"a gitlab merge request": {gitlab.Backend, map[string]string{
			"project": "acme/api", gitlab.ProjectIDField: "31", "mr_iid": "4",
		}, &types.WorkItem{Backend: types.WorkGitLab, ID: "31!4", Key: "acme/api!4", Project: "acme/api"}},
		"a gitlab issue": {gitlab.Backend, map[string]string{
			"project": "acme/api", gitlab.ProjectIDField: "31", "issue_iid": "4",
		}, &types.WorkItem{Backend: types.WorkGitLab, ID: "31#4", Key: "acme/api#4", Project: "acme/api"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			item, basis := workItemOfEvent(events.New(types.ExternalNotification{
				NotificationSource: tc.source, Metadata: tc.meta,
			}, events.TraceContext{}))
			switch {
			case tc.want == nil && item != nil:
				t.Fatalf("named %+v, want no item", item)
			case tc.want == nil:
				return
			case item == nil || *item != *tc.want || basis != types.BasisTrigger:
				t.Fatalf("named %+v (%q), want %+v (trigger)", item, basis, tc.want)
			}
		})
	}
}

// A CHAT WAKE IS ON NOTHING AT DISPATCH, and its records say so by leaving the
// key out rather than writing a null.
func TestAChatWakeHasNoItemAtDispatch(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	if _, err := e.runTurn(t.Context(), Request{
		RunID: "run-1", Handle: "swe", Depth: 3,
		Events: []*events.Event{chatTrigger("D0ANA")},
	}); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	for _, ev := range p.published() {
		if ev.Type != "agent_turn_started" && ev.Type != "agent_turn_completed" &&
			ev.Type != "turn_completed" {
			continue
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"work_item`) {
			t.Errorf("%s of a chat wake names an item: %s", ev.Type, raw)
		}
	}
}

// AN ASK INHERITS THE ASKER'S ITEM.
//
// Help given on a task is work on that task, and the answering seat's own
// trigger names no item — the ask is the only thing that knows which one the
// colleague was on. So the answering turn is charged to it, under its own
// basis, whatever basis the asker had.
func TestAnAskInheritsTheAskersItem(t *testing.T) {
	t.Parallel()
	asked := nativeTask
	start, summary, done := turnRecords(t, events.New(types.A2ARequest{
		ChannelID: "a2a-1", Requester: "lead", Content: "which test is flaky?",
		WorkItem: &asked, WorkItemBasis: types.BasisTrigger,
	}, events.TraceContext{}))
	charged(t, nativeTask, types.BasisAskedBy, start, summary, done)
}

// A RESUMED TURN KEEPS THE ITEM IT PARKED WITH.
//
// The event that resumes a run — a box's completion, or a person's answer on
// chat — names no item, so the row is the only place the item can come from.
// Read off the event instead, a coding run's second half would be charged to
// nothing.
func TestAResumedTurnKeepsTheItemItParkedWith(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	parked := nativeTask
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", WorkKey: "wk-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", WorkItem: &parked,
			DelegationDepth: 3,
		},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "use the main branch",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	charged(t, nativeTask, types.BasisResume,
		*only[*types.AgentTurnStarted](t, p, "agent_turn_started"),
		*only[*types.AgentTurnCompleted](t, p, "agent_turn_completed"),
		*only[*types.TurnCompleted](t, p, "turn_completed"))
}

// completing publishes one completion for a turn that wrote to items, with
// nothing named at dispatch.
func completing(t *testing.T, written []types.WorkItem, res turn.Result) (
	types.AgentTurnCompleted, types.TurnCompleted,
) {
	t.Helper()
	e, p := starting(t, refusingModels(t))
	tel := turnTelemetry{handle: "swe", role: "SWE", runID: "run-1",
		written: &turnctx.Written{}}
	for _, item := range written {
		tel.written.Add(item)
	}
	e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, res, nil)
	return *only[*types.AgentTurnCompleted](t, p, "agent_turn_completed"),
		*only[*types.TurnCompleted](t, p, "turn_completed")
}

// A TURN WOKEN FOR NOTHING THAT WROTE ONE ITEM IS CHARGED TO IT, AT THE END.
//
// A founder's "tidy up ENG-7" in chat names no item, and the turn then edits
// exactly one task: that is what the turn was on, and the completion — the
// only frame where "exactly one" is a fact — says so, under the basis that
// tells a reader it was inferred from what the turn did.
func TestASoleWriteNamesTheItemAtCompletion(t *testing.T) {
	t.Parallel()
	summary, done := completing(t, []types.WorkItem{nativeTask, nativeTask}, turn.Result{})
	for name, got := range map[string]struct {
		item  *types.WorkItem
		basis types.WorkItemBasis
	}{
		"agent_turn_completed": {summary.WorkItem, summary.WorkItemBasis},
		"turn_completed":       {done.WorkItem, done.WorkItemBasis},
	} {
		if got.item == nil || *got.item != nativeTask || got.basis != types.BasisSoleWrite {
			t.Errorf("%s names %+v (%q), want the one item written, as a sole write",
				name, got.item, got.basis)
		}
	}
}

// TWO WRITES NAME NO ITEM: a turn is never split, and never charged to
// whichever of its items happened to be written first.
func TestTwoWritesNameNoItem(t *testing.T) {
	t.Parallel()
	other := types.WorkItem{Backend: types.WorkNative, ID: "task-2", Key: "ENG-2", Project: "ENG"}
	summary, done := completing(t, []types.WorkItem{nativeTask, other}, turn.Result{})
	if summary.WorkItem != nil || done.WorkItem != nil {
		t.Errorf("a turn that wrote two items was charged to %+v / %+v",
			summary.WorkItem, done.WorkItem)
	}
}

// A SUSPENSION SAYS IT IS NOT AN END.
//
// A parked segment completes, and the same turn completes again when its run
// is collected — so the record says it parked, or a reader lists a parked
// turn as finished. And it concludes no sole write, because the turn has not
// finished writing: the resumed half may write a second item.
func TestASuspensionSaysItIsNotAnEnd(t *testing.T) {
	t.Parallel()
	summary, done := completing(t, []types.WorkItem{nativeTask}, turn.Result{Suspended: true})
	if !summary.Suspended || !done.Suspended {
		t.Errorf("suspended = %v / %v, want both records to say the segment parked",
			summary.Suspended, done.Suspended)
	}
	if summary.WorkItem != nil || done.WorkItem != nil {
		t.Errorf("a parked segment concluded a sole write: %+v / %+v",
			summary.WorkItem, done.WorkItem)
	}
}

// THE WRITES BEFORE A PARK COUNT AT THE END.
//
// The segment that finishes the turn judges "exactly one" over the whole turn,
// so what the turn wrote before it detached rides the suspension and seeds the
// resumed segment's set. Without it a turn that filed its task and then handed
// the code to a box ended having "written nothing", and was charged to nothing.
func TestTheWritesBeforeAParkCountAtTheEnd(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3,
		},
		State: execstate.State{Written: []types.WorkItem{nativeTask}},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "done",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	if done.WorkItem == nil || *done.WorkItem != nativeTask ||
		done.WorkItemBasis != types.BasisSoleWrite {
		t.Errorf("the finishing segment names %+v (%q), want the item written "+
			"before the park, as a sole write", done.WorkItem, done.WorkItemBasis)
	}
}

// writingDetach is run_sandbox for a turn that wrote a task in the same call it
// detached from — what the real tools report through the turn's write set.
type writingDetach struct{ suspendingTool }

func (w writingDetach) CallDetached(ctx context.Context, t *turnctx.Turn,
	args map[string]any,
) (tools.DetachedResult, error) {
	t.Written.Add(nativeTask)
	return w.suspendingTool.CallDetached(ctx, t, args)
}

// AND THE PARK WRITES THEM ONTO THE ROW, through the suspended conversation
// the resume re-enters — the other half of the case above, through the real
// suspension path.
func TestAParkCarriesWhatTheTurnWrote(t *testing.T) {
	e, _ := indicatingWith(t, notify.StatusAlways, suspendingModel{})
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-code", WorkKey: "wk-code", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	equipForCodeWith(t, e, store, writingDetach{})

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-code", RunID: "run-code",
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park, so this asserts nothing", res, err)
	}
	row, found, err := store.Get(t.Context(), "run-code")
	if err != nil || !found {
		t.Fatalf("the parked row: found=%v err=%v", found, err)
	}
	state, ok, err := execstate.Decode(row.ExecuteState)
	if err != nil || !ok {
		t.Fatalf("decode the suspension: ok=%v err=%v", ok, err)
	}
	if len(state.Written) != 1 || state.Written[0] != nativeTask {
		t.Errorf("the suspension carries %+v, want the task written before the park",
			state.Written)
	}
	// AND WHAT THE HALF BEFORE THE PARK SPENT, charged to nothing yet: a
	// chat wake names no item, so the segment that finishes the turn is the
	// one that may charge it by its sole write — and it pays this half too,
	// turn count included (see turnspend.go).
	if state.Uncharged == nil || state.Uncharged.Turns != 1 {
		t.Errorf("the suspension hands on %+v, want the parked segment's spend "+
			"and its one turn", state.Uncharged)
	}
}

// THE TURN'S TOOLS AND ITS COMPLETION SHARE ONE SET.
//
// The completion reads the set the telemetry holds, and a tool reports into
// the set the turn context points at. Two sets would compile, run and charge
// every turn to nothing.
func TestTheToolsAndTheCompletionShareOneWriteSet(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	tel := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{trackerWake()},
	})
	ctx := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}).Context
	if ctx.Written == nil || ctx.Written != tel.written {
		t.Fatal("the turn context's write set is not the one the completion reads")
	}
	if ctx.WorkItem == nil || *ctx.WorkItem != nativeTask || ctx.WorkItemBasis != types.BasisTrigger {
		t.Errorf("the turn context names %+v (%q), want the trigger's item",
			ctx.WorkItem, ctx.WorkItemBasis)
	}
}
