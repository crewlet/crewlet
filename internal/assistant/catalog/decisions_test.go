package catalog_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// aDecision is a question's decision as the tracker and a parked run answer it
// (decision.Decision), on made-up data.
var aDecision = map[string]any{
	"question": "Which way should the edge route library pages?",
	"options": []any{
		map[string]any{"id": "origin-rule", "label": "Origin rule with a Host override",
			"detail": "Needs a plan upgrade"},
		map[string]any{"id": "worker", "label": "A Worker route on the free tier"},
	},
	"recommended": "origin-rule",
	"rationale":   "One rule, one plan change.",
	"evidence": []any{map[string]any{"kind": "url",
		"ref": "https://docs.example.com/rules/origin", "label": "Origin rules"}},
	"role": "approver",
}

// whatAnswersIt is what the model must read of [aDecision] to answer it: every
// option by the id a choice names, the recommendation and why, what it cites
// and what the person is asked as.
var whatAnswersIt = []string{
	"origin-rule: Origin rule with a Host override (Needs a plan upgrade)",
	"worker: A Worker route on the free tier",
	"Which way should the edge route library pages?",
	"One rule, one plan change.",
	"url: https://docs.example.com/rules/origin (Origin rules)",
	"approver",
}

// A QUESTION REACHES THE MODEL WITH WHAT ANSWERS IT. Each read that lists a
// question hands the model, in its card's summary, the decision it carries and
// the ids the answering call names: a task's question its comment id
// (answer_decision's `answers`), an executor's its turn and launch ids
// (answer_executor's), and on a thread whom a question asks and the option an
// answer chose. Read as their titles and bodies alone, the model had to guess
// those ids or fall back to the raw answer, cut at what one step reads.
// Mutation: drop any decision column, the comment, turn or launch column, or
// an item fact, and this names the read and what it lost.
func TestAQuestionReachesTheModelWithWhatAnswersIt(t *testing.T) {
	t.Parallel()
	ask := map[string]any{"key": "OPS-7", "title": "Route the library", "comment": "c-9",
		"asked_by": "sre", "body": "Which way?", "decision": aDecision}
	run := map[string]any{"agent_handle": "sre", "question": "Merge it?", "turn_id": "t-41",
		"launch_id": "l-7", "question_context": "The branch is green.", "decision": aDecision}
	for _, tc := range []struct {
		tool   string
		args   map[string]any
		answer any
		want   []string
	}{
		{"list_decisions", nil, map[string]any{"items": []any{
			map[string]any{"kind": "ask", "ask": ask}}}, append([]string{"c-9"}, whatAnswersIt...)},
		{"list_decisions", nil, map[string]any{"items": []any{
			map[string]any{"kind": "run", "run": run}}},
			append([]string{"t-41", "l-7", "The branch is green."}, whatAnswersIt...)},
		{"get_my_work", map[string]any{"section": "asked"},
			map[string]any{"asked_of_me": []any{ask}}, append([]string{"c-9"}, whatAnswersIt...)},
		{"list_executors", nil, map[string]any{"runs": []any{run}},
			append([]string{"t-41", "l-7", "The branch is green."}, whatAnswersIt...)},
		{"get_task_thread", map[string]any{"item": "OPS-7"}, map[string]any{"key": "OPS-7",
			"title": "Route the library", "comments": []any{
				map[string]any{"id": "c-9", "author": "sre", "body": "Which way?", "ask": "ana",
					"decision": aDecision},
				map[string]any{"id": "c-10", "author": "ana", "body": "Go with the Worker.",
					"answers": "c-9", "choice": "worker"},
			}}, append([]string{`"Asks":"ana"`, `"Answers":"c-9"`, `"Chose":"worker"`},
			whatAnswersIt...)},
	} {
		body, _ := json.Marshal(tc.answer)
		res, err := catalog.Shape(mustTool(t, tc.tool), tc.args, exec.Response{
			Status: http.StatusOK, Body: body}, provenance.NewBoundary(), evals.Epoch,
			cards.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		read := modelRead(t, res.Model)
		for _, w := range tc.want {
			if !strings.Contains(read, w) {
				t.Errorf("%s hands the model no %q: %s", tc.tool, w, read)
			}
		}
	}
}

// THE QUESTIONS WAITING ARE READ A PAGE AT A TIME, ten unless the call asks for
// more, and a page that says more lie past it says so to the model: the
// route's own page of fifty, each question with its body and decision whole,
// can pass what one step reads and leave the model nothing. Mutation: drop the
// limit or the card's More, and this fails.
func TestTheQuestionsWaitingAreReadAPageAtATime(t *testing.T) {
	t.Parallel()
	tool := mustTool(t, "list_decisions")
	for _, tc := range []struct {
		args map[string]any
		want string
	}{{nil, "10"}, {map[string]any{"limit": 3}, "3"}} {
		req, err := tool.Request(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Query.Get("limit"); got != tc.want {
			t.Errorf("list_decisions %v asks for %q, want %s", tc.args, got, tc.want)
		}
	}
	body, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"kind": "ask",
		"ask": map[string]any{"key": "OPS-7", "comment": "c-9"}}}, "more": true})
	res, err := catalog.Shape(tool, nil, exec.Response{Status: http.StatusOK, Body: body},
		provenance.NewBoundary(), evals.Epoch, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if read := modelRead(t, res.Model); !strings.Contains(read, `"more_exist":true`) {
		t.Errorf("a page with more past it reads to the model as the whole: %s", read)
	}
}

// modelRead is the text the model reads of a result: its fenced answer.
func modelRead(t *testing.T, model string) string {
	t.Helper()
	var m struct{ Answer string }
	if err := json.Unmarshal([]byte(model), &m); err != nil || m.Answer == "" {
		t.Fatalf("the model's text is no fenced answer (%v): %s", err, model)
	}
	return m.Answer
}
