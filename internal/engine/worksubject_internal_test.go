package engine

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Which work item a turn is on — the rules in worksubject.go, driven through
// the frames that publish a turn's opening and closing records, because what a
// reader sees is those records rather than the resolver.

// workletTask is the item the Worklet cases wake a seat about.
var workletTask = types.WorkItem{
	Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1", Project: "ENG",
}

// trackerWake is a Worklet wake about task-1, as the parser writes it.
func trackerWake() *events.Event {
	return events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "assigned",
		Subject: "ENG-1: fix the build", Addressed: true,
		Metadata: map[string]string{
			worklet.MetaTaskID: "task-1", worklet.MetaTaskKey: "ENG-1",
			worklet.MetaProject: "ENG", worklet.MetaObject: "task",
			worklet.MetaObjectID: "task-1",
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
	charged(t, workletTask, types.BasisTrigger, start, summary, done)
}

// codeWake is the wake swe is given of a comment that mentions it on a change
// that implements item — nil for a change that names none — as the code log's
// feed relays the record and the code parser routes it.
func codeWake(t *testing.T, item *types.WorkItem) *events.Event {
	t.Helper()
	const repo, change = "repo-1", "change-1"
	mutation, err := json.Marshal(code.Comment{RepoID: repo, ChangeID: change, CommentID: "comment-1",
		ThreadID: "comment-1", Body: "@swe is the retry bounded?", Mentions: []string{"swe"},
		Routing: &code.ChangeRouting{ChangeID: change, ChangeRef: "web!42", RepoName: "web",
			Title: "Retry the upload", Actor: "cto", Owner: "dev", OwnerKind: code.PartyAgent,
			WorkItem: item, Mentions: []string{"swe"}}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := code.Encode(code.MutationRecord{
		RecordEnvelope: code.RecordEnvelope{OpID: "op-comment", Subject: code.ChangeSubject(change),
			Op: code.OpComment, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: code.Scope{code.RepoPath(repo), code.ChangePath(change)}},
		Mutation: mutation, Actor: "node-a", ActorKind: code.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, relayed, err := code.NewTranslator().Translate(t.Context(), changefeed.Record{
		Key: "crewlet.code.log.change." + change, Payload: payload, Position: 7,
		Stream: code.Domain{}.Stream().Name})
	if err != nil || !relayed {
		t.Fatalf("the code feed relays the comment: %v, %v", relayed, err)
	}
	routed, err := code.NewParser(code.ParserOptions{}).Parse(t.Context(), types.RawWebhook{Body: d.Body}, nil)
	if err != nil || len(routed) != 1 || routed[0].To.Handle != "swe" {
		t.Fatalf("the comment wakes %+v (%v), want swe", routed, err)
	}
	return events.New(types.ExternalNotification{
		NotificationSource: routed[0].Source, SourceEventType: routed[0].EventType,
		Subject: routed[0].Subject, Metadata: routed[0].Metadata,
	}, events.TraceContext{})
}

// A CODE WAKE NAMES ITS WORK ITEM (ADR-0090): a change implements the item its
// owner named, its routing carries it, and every wake about the change names
// it — so the turn a review request, a mention or a finding's answer starts is
// charged to the item the change implements, at dispatch, under the trigger
// rule, whatever backend the item is on. A change that names no item names
// none, and its wake's turn is on nothing until a sole write says otherwise.
//
// Mutation: leave code.Prompt out of workItemNamers, or the work item out of
// the wake's metadata, and the turn is on nothing.
func TestACodeWakeNamesItsWorkItem(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]types.WorkItem{
		"a Worklet task": workletTask,
		"a jira issue":   {Backend: types.WorkJira, ID: "10042", Key: "ENG-12", Project: "ENG"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start, summary, done := turnRecords(t, codeWake(t, &item))
			charged(t, item, types.BasisTrigger, start, summary, done)
		})
	}
	t.Run("a change that names none", func(t *testing.T) {
		t.Parallel()
		if item, basis := workItemOfEvent(codeWake(t, nil)); item != nil || basis != "" {
			t.Errorf("a wake about a change that names no item is on %+v (%q)", item, basis)
		}
	})
}

// A WAKE ABOUT A PERSON'S OWN LIST NAMES NO ITEM.
//
// The parser writes a task only when the wake is about one; a wake about
// somebody's priorities carries the person as its object, and charging the
// turn to that id would charge it to a handle.
func TestAPersonWakeHasNoItem(t *testing.T) {
	t.Parallel()
	start, summary, done := turnRecords(t, events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "prioritised",
		Metadata: map[string]string{
			worklet.MetaObject: "person", worklet.MetaObjectID: "ada",
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
	asked := workletTask
	start, summary, done := turnRecords(t, events.New(types.A2ARequest{
		ChannelID: "a2a-1", Requester: "lead", Content: "which test is flaky?",
		WorkItem: &asked, WorkItemBasis: types.BasisTrigger,
	}, events.TraceContext{}))
	charged(t, workletTask, types.BasisAskedBy, start, summary, done)
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
	parked := workletTask
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
	charged(t, workletTask, types.BasisResume,
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
	e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, res, nil, time.Now())
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
	summary, done := completing(t, []types.WorkItem{workletTask, workletTask}, turn.Result{})
	for name, got := range map[string]struct {
		item  *types.WorkItem
		basis types.WorkItemBasis
	}{
		"agent_turn_completed": {summary.WorkItem, summary.WorkItemBasis},
		"turn_completed":       {done.WorkItem, done.WorkItemBasis},
	} {
		if got.item == nil || *got.item != workletTask || got.basis != types.BasisSoleWrite {
			t.Errorf("%s names %+v (%q), want the one item written, as a sole write",
				name, got.item, got.basis)
		}
	}
}

// TWO WRITES NAME NO ITEM: a turn is never split, and never charged to
// whichever of its items happened to be written first.
func TestTwoWritesNameNoItem(t *testing.T) {
	t.Parallel()
	other := types.WorkItem{Backend: types.WorkWorklet, ID: "task-2", Key: "ENG-2", Project: "ENG"}
	summary, done := completing(t, []types.WorkItem{workletTask, other}, turn.Result{})
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
	summary, done := completing(t, []types.WorkItem{workletTask}, turn.Result{Suspended: true})
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
		State: execstate.State{Written: []types.WorkItem{workletTask}},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "done",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	if done.WorkItem == nil || *done.WorkItem != workletTask ||
		done.WorkItemBasis != types.BasisSoleWrite {
		t.Errorf("the finishing segment names %+v (%q), want the item written "+
			"before the park, as a sole write", done.WorkItem, done.WorkItemBasis)
	}
}

// AND WHAT THE SEGMENTS BEFORE A PARK MEASURED, read back off the parked
// conversation by the real resume path: the record ending the resumed turn
// states at least what they carried. Read back by hand, a resume that dropped
// the field would still pass every case built on its telemetry.
//
// Mutation: stop the resume reading State.SoFar, and this goes red.
func TestTheMeasurementBeforeAParkCountsAtTheEnd(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	carried := execstate.TurnSoFar{DurationMS: 60_000, Reviews: 2, SentBack: 1, Unaccepted: 1}
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3},
		State: execstate.State{SoFar: &carried},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "done",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	if done.Turn == nil || done.Turn.DurationMS < carried.DurationMS ||
		done.Turn.Reviews < carried.Reviews || done.Turn.SentBack < carried.SentBack ||
		done.Turn.Unaccepted < carried.Unaccepted {
		t.Errorf("the record ending a resumed turn states %+v, want at least the %+v "+
			"its parked segments measured", done.Turn, carried)
	}
}

// A RESUMED TURN WHOSE PARK NOBODY MEASURED STATES NO TALLY, through the real
// resume path.
//
// A build before the tally parked it, so its conversation carries no `so_far`
// and what its first segment ran is unknown. Stated from this segment alone,
// the tally would be read as the whole turn — short of the first segment's span
// and reviews for good — so the end states none and the usage reader sums the
// turn from its records, the first segment's included.
//
// Mutation: state the tally on every end that is not a suspension, and this
// goes red.
func TestAResumeOfAnUnmeasuredParkStatesNoTally(t *testing.T) {
	t.Parallel()
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3},
		State: execstate.State{},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "done",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	if done := only[*types.TurnCompleted](t, p, "turn_completed"); done.Turn != nil {
		t.Errorf("the record ending a turn an earlier build parked states %+v — a tally "+
			"of the last segment alone, read as the whole turn", done.Turn)
	}
}

// A SCHEDULED TURN PARKS WITH ITS DEADLINE, and launches its run with it. The
// fire's cap is fixed when the turn begins, on the turn and not on any one
// segment: the segment that collects the run reads it off the park, and the
// run's row carries it for the poll. A turn with no cap carries none.
func TestAScheduledTurnParksWithItsDeadline(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	began := time.Now().UTC()
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-fire", WorkSince: began, RunID: "run-fire",
		Events: []*events.Event{chatTrigger("D0ANA")}, TimeoutSeconds: 1800,
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park, so this asserts nothing", res, err)
	}
	row := rig.row(t, "run-fire")
	state, ok, err := execstate.Decode(row.ExecuteState)
	if err != nil || !ok {
		t.Fatalf("decode the suspension: ok=%v err=%v", ok, err)
	}
	ends := began.Add(30 * time.Minute)
	if state.Deadline.Before(ends) || state.Deadline.After(ends.Add(time.Minute)) {
		t.Errorf("the turn parks deadline %v, want its cap from when it began, about %v",
			state.Deadline, ends)
	}
	if !row.Deadline.Equal(state.Deadline) {
		t.Errorf("the run carries deadline %v, want the turn's %v", row.Deadline, state.Deadline)
	}
}

// THE PARK CARRIES WHAT THE TURN ASKED AND SPENT, through the real launch every
// turn's executor takes: the turn parks the moment its run is launched, and the
// segment that finishes it is woken by the run's completion — which carries
// none of this.
func TestAParkCarriesWhatTheTurnAskedAndSpent(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	// UNDER THE FIXTURE'S DEPTH CAP, so the turn reaches its launch.
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	// THE WORK'S OWN START, as every dispatch derives it from the inbox: a
	// work key with none is a start nobody knows, which reads as older than
	// the operation ledger and is refused on a node with nowhere to record a
	// rebase (rebase.go).
	began := time.Now().UTC()
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-code", WorkSince: began, RunID: "run-code",
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park, so this asserts nothing", res, err)
	}
	state, ok, err := execstate.Decode(rig.row(t, "run-code").ExecuteState)
	if err != nil || !ok {
		t.Fatalf("decode the suspension: ok=%v err=%v", ok, err)
	}
	// WHAT THE TURN WAS ASKED, for the segment that finishes it to record:
	// that segment is woken by the run's completion, whose ask is not the
	// turn's.
	if want := turnAsk([]*events.Event{chatTrigger("D0ANA")}); want == "" || state.Ask != want {
		t.Errorf("the suspension parks the ask %q, want %q", state.Ask, want)
	}
	// AND WHO WROTE IT, for that segment's record of the ask to say so
	// (ADR-0040): nothing it reads could decide the label again.
	asked := e.askOf(Request{Handle: "swe", Events: []*events.Event{chatTrigger("D0ANA")}})
	if want := asked.Label(); want == nil || !reflect.DeepEqual(state.AskProvenance, want) {
		t.Errorf("the suspension parks the ask's label %+v, want %+v", state.AskProvenance, want)
	}
	// AND WHO SAID IT, each body under the label the turn read it with, for
	// the segment that finishes the turn: its completed-turn event is the one
	// the post-turn workers that wait for a settled turn read, and its
	// refresh_memory calls read who asked off these. That segment re-reads no
	// trigger.
	if want := e.interactionsOf([]*events.Event{chatTrigger("D0ANA")}, "swe"); len(want) == 0 ||
		!reflect.DeepEqual(state.Interactions, want) {
		t.Errorf("the suspension parks the interactions %+v, want %+v", state.Interactions, want)
	}
	// AND WHAT THE HALF BEFORE THE PARK SPENT, charged to nothing yet: a
	// chat wake names no item, so the segment that finishes the turn is the
	// one that may charge it by its sole write — and it pays this half too,
	// turn count included (see turnspend.go).
	if state.Uncharged == nil || state.Uncharged.Turns != 1 {
		t.Errorf("the suspension hands on %+v, want the parked segment's spend "+
			"and its one turn", state.Uncharged)
	}
	// AND WHAT THE TURN HAS MEASURED OF ITSELF, for the record that ends it to
	// state the whole turn (types.TurnTally) — the usage domain reads a turn's
	// span and reviews off that one record. Presence, not a span: a fake
	// segment can end inside the millisecond it started.
	if state.SoFar == nil {
		t.Errorf("the suspension hands on no measurement of the turn so far, " +
			"so the record ending it states only its last segment")
	}
}

// AND THE PARK CARRIES WHAT THE TURN WROTE, onto the row the segment that
// finishes the turn seeds its write set from — so "exactly one item written"
// is judged over the whole turn rather than over its last segment.
func TestAParkCarriesWhatTheTurnWrote(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	rig := rigRuns(t, e, nil)
	company := e.Company()
	seat := company.Org.AgentSeatByHandle("swe")
	turnContext := &turnctx.Turn{RunID: "run-code", Seat: seat, Org: company.Org}
	r, err := company.RunnerFor(t.Context(), "swe", nil, RunnerInput{
		Reply: turn.NoReply(), Launcher: launcherStub{}, Sessions: e.sessions,
		Turn: runner.Turn{RunID: "run-code", Context: turnContext},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	res, err := turn.Run(t.Context(), r, company.TurnSettings(time.Time{}),
		turn.Input{RunID: "run-code", Reply: turn.NoReply()})
	if err != nil || !res.Suspended {
		t.Fatalf("turn.Run = %+v, %v; the turn did not park", res, err)
	}
	// THE ROW THE LAUNCH WOULD HAVE OPENED, which the stub opened none of.
	if _, err := e.sandbox.Load().pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "run-code", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	written := &turnctx.Written{}
	written.Add(workletTask)
	deadline := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	if resumable, err := e.persistSuspension(t.Context(), r, "run-code", turnTelemetry{
		written: written, ask: "the ask", deadline: deadline,
	}, segmentCharge{}); err != nil || !resumable {
		t.Fatalf("persistSuspension = %v, %v; want the run recorded", resumable, err)
	}
	state, ok, err := execstate.Decode(rig.row(t, "run-code").ExecuteState)
	if err != nil || !ok {
		t.Fatalf("decode the suspension: ok=%v err=%v", ok, err)
	}
	// AND WHEN THE TURN'S CAP ENDS: the segment that collects the run is
	// held to the turn's deadline, not to one measured from its own start.
	if !state.Deadline.Equal(deadline) {
		t.Errorf("the suspension carries deadline %v, want the turn's %v", state.Deadline, deadline)
	}
	if len(state.Written) != 1 || state.Written[0] != workletTask || state.WrittenMany {
		t.Errorf("the suspension carries %+v (many %v), want the task written before the park",
			state.Written, state.WrittenMany)
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
	tel, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{trackerWake()},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	ctx := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context
	if ctx.Written == nil || ctx.Written != tel.written {
		t.Fatal("the turn context's write set is not the one the completion reads")
	}
	if ctx.WorkItem == nil || *ctx.WorkItem != workletTask || ctx.WorkItemBasis != types.BasisTrigger {
		t.Errorf("the turn context names %+v (%q), want the trigger's item",
			ctx.WorkItem, ctx.WorkItemBasis)
	}
}

// WHO WOKE THE TURN REACHES THE TURN CONTEXT, which is the one frame a coding
// run it detaches copies onto its row — and a resumed turn keeps the one the
// row recorded rather than whoever answered it.
func TestTheTurnContextCarriesWhoWokeIt(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	// THE ASKER IS A SEAT OF THE COMPANY: a requester the chart does not
	// hold is nobody to put a question to, and is recorded as nobody.
	company.Org.Roles = append(company.Org.Roles, &org.Role{Name: "CEO", DeclaredHandle: "ceo"})
	ask := events.New(types.A2ARequest{ChannelID: "c1", Requester: "ceo", Content: "why?"},
		events.TraceContext{})
	tel, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{ask},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	if got := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Requester; got != "ceo" {
		t.Errorf("the turn context's requester = %q, want the asking seat", got)
	}
	// THE RESUMED TURN AS THE COORDINATOR BUILDS IT ([resumedTurn]), which
	// is where a resume reads its identity from rather than the row.
	run := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Requester: "ceo"}
	resumed, err := e.describeResume(t.Context(), company, resumeInput{
		Run:  run,
		Turn: resumedTurn(run, company.Org.AgentSeatByHandle("swe"), company.Org, turnctx.NewHolds(run.CodeHolds)),
	})
	if err != nil {
		t.Fatalf("describeResume: %v", err)
	}
	if got := resumed.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Requester; got != "ceo" {
		t.Errorf("a resumed turn's requester = %q, want the one its row recorded", got)
	}
}

// A SETTLED TURN'S COMPLETION CARRIES WHO SAID WHAT, though the segment that
// settles it re-reads no trigger.
//
// Every agent seat's executor is a detached run, so every such turn parks at
// its launch and is settled by the segment that collects the run — woken by the
// run's completion, not by the person who asked. The post-turn workers that
// wait for a settled turn read that segment's turn_completed, and the persist
// decider attributes each fact it keeps to the interaction that stated it: a
// settling event carrying none attributes nothing to anybody, on every turn.
//
// Mutation: park no interactions, and the settling event carries none.
func TestASettledTurnCarriesTheInteractionsItParkedWith(t *testing.T) {
	t.Parallel()
	// A MODEL THAT REVIEWS, so the collected run's review settles the turn.
	e, _ := indicatingWith(t, notify.StatusAlways, billingModel())
	// UNDER THE FIXTURE'S DEPTH CAP, so the turn reaches its launch.
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	trigger := chatTrigger("D0ANA")
	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-chat", RunID: "run-chat", WorkSince: time.Now().UTC(),
		Events: []*events.Event{trigger},
	})

	want := e.interactionsOf([]*events.Event{trigger}, "swe")
	if len(want) == 0 {
		t.Fatal("the trigger resolves to no interaction, so this asserts nothing")
	}
	var parked, settled []*types.TurnCompleted
	for _, ev := range e.backends.Queue.(*memory.Queue).History() {
		if done, ok := events.DataAs[*types.TurnCompleted](ev); ok && done.TurnID == "run-chat" {
			if done.Suspended {
				parked = append(parked, done)
			} else {
				settled = append(settled, done)
			}
		}
	}
	if len(parked) != 1 || len(settled) != 1 {
		t.Fatalf("published %d parked and %d settled completions for the turn, want one "+
			"of each: the case needs the turn to park and then settle", len(parked), len(settled))
	}
	if !reflect.DeepEqual(parked[0].Interactions, want) {
		t.Errorf("the park's completion carries %+v, want the dispatch's %+v",
			parked[0].Interactions, want)
	}
	if !reflect.DeepEqual(settled[0].Interactions, want) {
		t.Errorf("the settling completion carries %+v, want the %+v its turn parked with",
			settled[0].Interactions, want)
	}
}

// WHO ASKED REACHES THE TURN CONTEXT ON EVERY SEGMENT, which is what a
// refresh_memory call tells the memory filter. A dispatch reads them off its
// interactions; a resumed segment re-reads no trigger, so it reads them off
// the interactions its turn parked with — without those, every resumed
// segment told the filter nobody was asking, and a note about one person could
// be applied to a turn somebody else started.
func TestTheTurnContextCarriesWhoAskedOnEverySegment(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	tel, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	spoke := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Senders
	if len(spoke) == 0 {
		t.Fatal("a chat turn's context names no sender, so this asserts nothing")
	}
	run := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe"}
	resumed, err := e.describeResume(t.Context(), company, resumeInput{
		Run:   run,
		State: execstate.State{Interactions: tel.interactions},
		Turn:  resumedTurn(run, company.Org.AgentSeatByHandle("swe"), company.Org, turnctx.NewHolds(run.CodeHolds)),
	})
	if err != nil {
		t.Fatalf("describeResume: %v", err)
	}
	if got := resumed.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Senders; !slices.Equal(got, spoke) {
		t.Errorf("a resumed segment's senders = %+v, want the %+v its turn parked with", got, spoke)
	}
}

// THE CHAT SURFACE A TURN WAS WOKEN ON REACHES THE TURN CONTEXT, which is where
// a task it files reads its origin from — and a turn no chat message woke
// names none.
func TestTheWakingChatSurfaceReachesTheTurnContext(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	for _, tc := range []struct {
		wake *events.Event
		want string
	}{
		{chatTrigger("C0PRODUCT"), slack.Backend},
		{trackerWake(), ""},
	} {
		tel, err := e.describeTurn(t.Context(), company, Request{
			RunID: "run-1", Handle: "swe", Events: []*events.Event{tc.wake},
		})
		if err != nil {
			t.Fatalf("describeTurn: %v", err)
		}
		if got := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context.Transport; got != tc.want {
			t.Errorf("a turn woken by %s carries transport %q, want %q", tc.wake.Source, got, tc.want)
		}
	}
}
