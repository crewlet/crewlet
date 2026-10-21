package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// refusingWork is a tracker writer whose every write is refused with err, so
// the real create_work_item turns it into a refusal through its own
// sentences, exactly as it does for a seat's model.
type refusingWork struct{ err error }

func (r refusingWork) writer(builtin.Actor) builtin.WorkWriter { return r }

func (r refusingWork) CreateTask(context.Context, string, worklet.Task,
	*worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, r.err
}

func (r refusingWork) CreateTaskAsking(context.Context, string, worklet.Task,
	worklet.Comment, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, r.err
}

func (r refusingWork) UpdateTask(context.Context, string, string, string, uint64,
	worklet.TaskPatch, worklet.ChangeKind, *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{}, r.err
}

// refusingSurface is the act route over a tracker whose every write is
// refused with err.
func refusingSurface(t *testing.T, err error) http.Handler {
	t.Helper()
	return guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: refusingWork{err}.writer},
		}),
		Org: company,
	}))
}

// machineFields are the keys of an act answer a client reads as values, never
// as a sentence: codes, names, keys and the facts a screen branches on.
var machineFields = []string{"error", "tool", "item", "page", "project", "handle",
	"op_id", "field", "fields", "action", "outcome", "reason", "window"}

// wordedForAPerson fails t for every sentence in an act answer that is not
// worded for a person: every string the answer carries that is not one of
// [machineFields], and its `message` wherever the dashboard draws the
// engine's own words rather than its own (a code it shows the detail for).
//
// AN UNKNOWN OUTCOME'S `detail` IS THE KEY'S RETRY RULE, for a client
// (opkey.UnknownDetail, held by its own suite): it is not a refusal, the
// dashboard draws an unknown in its own words (`protocol/writeOutcome.ts`,
// held by its suite), and the tool's sentence about the write is no longer on
// it at all.
func wordedForAPerson(t *testing.T, what string, answer map[string]any) {
	t.Helper()
	unknown := answer["outcome"] == string(statelog.OutcomeUnknown)
	for key, value := range answer {
		if slices.Contains(machineFields, key) || (unknown && key == "detail") {
			continue
		}
		if key == "message" && !drawsTheEnginesWords(answer) {
			continue
		}
		var texts []string
		switch v := value.(type) {
		case string:
			texts = []string{v}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && key != "grants" && key != operator.KeyFields {
					texts = append(texts, s)
				}
			}
		}
		for _, text := range texts {
			for _, problem := range caveattest.Problems(text) {
				t.Errorf("%s: the answer's %s is not worded for a person: %s: %q",
					what, key, problem, text)
			}
		}
	}
}

// drawsTheEnginesWords reports whether the dashboard shows this answer in the
// engine's words: the four refusal classes its `contract/errors.ts` keeps no
// sentence for, because only the engine's words say what to change, whom to
// ask, the state that refused the change, or what is already there. Every
// other code it shows in a sentence of its own.
func drawsTheEnginesWords(answer map[string]any) bool {
	code, _ := answer["error"].(string)
	return code == string(httpjson.CodeInvalid) || code == string(httpjson.CodeForbidden) ||
		code == string(httpjson.CodeConflict) || code == string(httpjson.CodeExists)
}

// EVERY REFUSAL THE ACT ROUTE ANSWERS IS WORDED FOR A PERSON (ADR-0060).
//
// The act route answers a person: the dashboard draws a refusal's `detail`
// where the control that caused it is. It used to be the tool's sentence,
// written for a model: "comment_on_work_item was refused: … change the
// argument this names (`answers`, or `choice`)". This walks the route through
// every arm of the refusal mapping, each produced by the REAL tool from a
// writer's error (so each comes with the tool's own model-phrased sentence
// on Output), and the route's own refusals, and holds every sentence each
// answer carries to what a person can read: no tool, no argument, no
// backtick, no `name: value`, no internal id, no engine vocabulary.
//
// Mutation, run when this was written: answer `result.Output` as the
// detail again (the code before ADR-0060) and every tool-refusal row goes red,
// on the tool's name, its backticked arguments and its engine words.
func TestEveryRefusalTheActRouteAnswersIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	writes := []struct {
		name string
		err  error
	}{
		{"a stale version", fmt.Errorf("%w: at version 4, not 3", worklet.ErrStaleVersion)},
		{"a rule of the domain", fmt.Errorf("%w: only ENG's lead may file it", worklet.ErrForbidden)},
		{"an argument the domain refused", fmt.Errorf("%w: the `labels` name no tag", worklet.ErrInvalid)},
		{"a title over its cap", &worklet.TextCapError{Field: worklet.TextTitle, Size: 400, Limit: 256}},
		{"a domain refusal worded for a person", caveat.Saying(
			fmt.Errorf("%w: the choice %q is not an option", worklet.ErrInvalid, "ship"),
			caveat.NotAnOption())},
		{"a missing item", fmt.Errorf("%w: ENG-404", worklet.ErrNoTask)},
		{"a collision", fmt.Errorf("%w: two writers", statelog.ErrConflict)},
		{"an existing record", fmt.Errorf("%w: ENG-9", statelog.ErrExists)},
		{"an answered question", fmt.Errorf("%w: by @ana", worklet.ErrAlreadyAnswered)},
		{"a spent hand-off budget", worklet.ErrReassignmentBudget},
		{"a full inbox", worklet.ErrInboxFull},
		{"a tag clash", &worklet.TagClash{Project: "ENG", Slug: "bug-fix", Label: "Bug fix",
			Other: worklet.Tag{Slug: "bugfix", Label: "Bugfix"}}},
		{"a full tag set", &worklet.TagsFull{Project: "ENG", Slug: "infra"}},
		{"a step nobody can confirm", fmt.Errorf("walk: %w", worklet.ErrStepUnresolved)},
		{"a step nobody can vouch for", fmt.Errorf("walk: %w", worklet.ErrStepUnvouched)},
		{"a condition of the node", fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)},
		{"a fault of the node", errors.New(diskFault)},
		{"a write nobody answered", fmt.Errorf("%w (asked of 10.0.0.7)", estate.ErrOutcomeUnknown)},
		{"a reused operation", &statelog.Unavailable{Reason: statelog.ReasonOpReused,
			Detail: "the operation already wrote ENG-3"}},
		{"a purged record", &statelog.Unavailable{Reason: statelog.ReasonDeleted,
			Detail: "ENG-9 was purged"}},
	}
	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, answer := act(t, refusingSurface(t, tc.err), "founder",
				builtin.CreateWorkItemTool, "application/json", createBody(), newKey())
			if status < 400 {
				t.Fatalf("a refused write answered %d %v", status, answer)
			}
			wordedForAPerson(t, tc.name, answer)
		})
	}

	// THE TOOL'S OWN REFUSALS, before any write: a title it needs, and an
	// argument it does not take.
	for name, args := range map[string]map[string]any{
		"no title":           {"title": " ", "project": "ENG"},
		"no such priority":   {"title": "Rotate the key", "project": "ENG", "priority": "asap"},
		"an undeclared arg":  {"title": "Rotate the key", "project": "ENG", "colour": "red"},
		"a status not known": {"title": "Rotate the key", "project": "ENG", "status": "shipping"},
	} {
		raw, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		status, answer := act(t, guarded(actSurface(t, &recordingWork{})), "founder",
			builtin.CreateWorkItemTool, "application/json", string(raw), newKey())
		if status < 400 {
			t.Fatalf("%s answered %d %v", name, status, answer)
		}
		wordedForAPerson(t, name, answer)
	}

	// THE AUTHORITY TABLE'S REFUSAL, which names what would admit the caller
	// as grants and says nothing of the verb.
	status, answer := act(t, guarded(actSurface(t, &recordingWork{})), "reader",
		builtin.CreateWorkItemTool, "application/json", createBody(), newKey())
	if status != http.StatusForbidden {
		t.Fatalf("a caller without access answered %d %v", status, answer)
	}
	wordedForAPerson(t, "an authority refusal", answer)

	// THE ROUTE'S OWN REFUSALS: a tool it does not serve, a read, a body
	// that is not JSON or not the envelope, and an operation named twice.
	h := guarded(actSurface(t, &recordingWork{}))
	opArg, err := json.Marshal(map[string]any{"args": map[string]any{
		"title": "Rotate the key", operator.OperationArg: newKey()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		name, tool, contentType, body string
	}{
		{"a tool it does not serve", "drop_database", "application/json", createBody()},
		{"a read", "list_work_items", "application/json", `{"args":{}}`},
		{"a form post", builtin.CreateWorkItemTool, "text/plain", createBody()},
		{"a body that is not the envelope", builtin.CreateWorkItemTool, "application/json",
			`{"op_id":"x","args":{}}`},
		{"a body that is not JSON", builtin.CreateWorkItemTool, "application/json", `{"args":`},
		{"an operation named twice", builtin.CreateWorkItemTool, "application/json", string(opArg)},
	} {
		status, answer := act(t, h, "founder", route.tool, route.contentType, route.body, newKey())
		if status < 400 {
			t.Fatalf("%s answered %d %v", route.name, status, answer)
		}
		wordedForAPerson(t, route.name, answer)
	}
}

// A REFUSAL WORDED FOR A PERSON IS ANSWERED IN THOSE WORDS, AND ONE NOBODY
// WORDED IN ITS CODE'S: the tool's sentence never reaches the answer either
// way. A title over its cap carries the writer's phrasing; a domain refusal
// without one carries no detail, and its code's sentence stands alone.
func TestAnActRefusalIsAPersonsWordsOrItsCodes(t *testing.T) {
	t.Parallel()
	_, answer := act(t, refusingSurface(t, &worklet.TextCapError{Field: worklet.TextTitle,
		Size: 400, Limit: 256}), "founder", builtin.CreateWorkItemTool, "application/json",
		createBody(), newKey())
	if answer["error"] != string(httpjson.CodeInvalid) ||
		answer["detail"] != caveat.TitleTooLong().Person() {

		t.Errorf("a title over its cap answered %v, want invalid in the person's words", answer)
	}
	_, answer = act(t, refusingSurface(t, fmt.Errorf("%w: the `labels` name no tag",
		worklet.ErrInvalid)), "founder", builtin.CreateWorkItemTool, "application/json",
		createBody(), newKey())
	if _, has := answer["detail"]; has || answer["message"] != httpjson.CodeInvalid.Message() {
		t.Errorf("a refusal nobody worded for a person answered %v, want its code's "+
			"own sentence and no detail", answer)
	}
	_, answer = act(t, guarded(actSurface(t, &recordingWork{})), "founder",
		builtin.CreateWorkItemTool, "application/json",
		`{"args":{"title":" ","project":"ENG"}}`, newKey())
	if answer["detail"] != caveat.TitleMissing().Person() {
		t.Errorf("a create with no title answered %v, want the person's words", answer)
	}
}

// placingWork is a tracker whose one card, ENG-1 in ENG, is placed by every
// drag in its new lane and not at the position it was dropped at.
type placingWork struct{ stubWorkReader }

func (placingWork) Task(context.Context, string, worklet.DetailWants,
	statelog.Freshness) (worklet.TaskDetail, error) {

	return worklet.TaskDetail{Task: worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG",
		Title: "Rotate the key", Status: worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted, Version: 7}}, nil
}

func (placingWork) PlaceTask(context.Context, string, worklet.Place,
	*worklet.Notify) (worklet.PlaceResult, error) {

	at := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 8}
	return worklet.PlaceResult{
		Lane: worklet.WriteResult{Result: statelog.Result{
			Outcome: statelog.OutcomeApplied, Position: at, Version: 8}},
		Version:  8,
		Unplaced: fmt.Errorf("%w: task t-1 moved", worklet.ErrStaleVersion),
	}, nil
}

// EVERY ANSWER THE ACT ROUTE GIVES IS WORDED FOR A PERSON, a write that landed
// included (ADR-0060).
//
// A tool's receipt is its answer to a model, passed on to the dashboard, and
// any field of it can carry a model's sentence: `place_work_item` reported a
// card that missed its place in an `unplaced` field holding the refusal it
// would have answered a model ("… call get_work_item again"), and the board
// drew it. The act route used to be held only on `warnings`, so a sentence
// under any other name passed. This walks every string of each answer, at any
// depth, outside the value fields a screen reads as values, and holds it to a
// person's words: a new field of prose fails here whatever it is named.
//
// Mutation, run when this was written: answer the missed place as the
// receipt's `unplaced` again and the drag's row goes red on the tool's name.
func TestEveryAnswerTheActRouteGivesIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	created := &recordingWork{caveats: []caveat.Caveat{
		caveat.DefaultAssigneeGone("ENG", "bo"), caveat.BodyLinksCapped(31, 25)}}
	placing := guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{Work: builtin.WorkDeps{
			Reader: placingWork{}, Writer: (&recordingWork{}).writer,
			Placer: func(builtin.Actor) builtin.WorkPlacer { return placingWork{} },
		}}),
		Org: company,
	}))
	for _, tc := range []struct {
		name, tool, body string
		h                http.Handler
	}{
		{"a create with caveats", builtin.CreateWorkItemTool, createBody(),
			guarded(actSurface(t, created))},
		{"a drag that missed its place", worklet.PlaceWorkItemTool,
			`{"args":{"item":"ENG-1","status":"in_progress","if_match":7}}`, placing},
	} {
		status, answer := act(t, tc.h, "founder", tc.tool, "application/json", tc.body, newKey())
		if status != http.StatusOK {
			t.Fatalf("%s answered %d %v", tc.name, status, answer)
		}
		if caveats, _ := answer["caveats"].([]any); len(caveats) == 0 {
			t.Errorf("%s answered no caveats: %v", tc.name, answer)
		}
		for _, problem := range caveattest.AnswerProblems(tc.tool, answer) {
			t.Errorf("%s: %s", tc.name, problem)
		}
	}
}
