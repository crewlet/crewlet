package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CODING RUN'S ASK IS READ AT ONE TRUST THROUGH EVERY DOOR (ADR-0040).
//
// One parked ask (a question, the context the run gave with it, the options
// it offered) reaches a model by many doors: answer_run's receipt, which a
// person's model reads when it answers; the Dashboard Assistant's reads of the
// coding runs and of what waits on the person; its generic reads of the same
// routes and of the decisions the person answered, where the ask is kept once
// answered; and the receipt of its own write that answers a run, which the turn
// its approval resumes reads. Each used to decide for itself: answer_run read
// the run's words as a colleague's, the curated reads as an outside party's,
// and the generic read of the answered decisions as a colleague's again, so
// one ask was trusted by the door it came through. Every door now reads it by
// the one rule for a coding run's words ([provenance.CodingRun]).
//
// Mutation: fence answer_run's question by the hosted rule again, or read a
// decisions route's answer by its path alone, and the door that does is red.
func TestACodingRunsAskIsReadAtOneTrustThroughEveryDoor(t *testing.T) {
	t.Parallel()
	want := provenance.CodingRun("").Trust
	ask := &decision.Decision{Question: "Which database?", Role: decision.RoleApprover,
		Options: []decision.Option{{ID: "postgres", Label: "Postgres"}, {ID: "sqlite", Label: "SQLite"}}}
	const said = "The service writes about 2,000 rows a minute."
	run := parked(t, "t1", sandbox.StatusAwaiting)
	run.Question, run.QuestionContext, run.Decision = ask.Question, said, ask
	asked := map[string]any{"agent_handle": run.AgentHandle, "turn_id": run.TurnID,
		"question": run.Question, "question_context": run.QuestionContext, "decision": ask}

	got, err := answerRunTool(t, &deskFake{runs: map[string]sandbox.PendingRun{"t1": run}}).
		Call(t.Context(), map[string]any{"turn_id": "t1", "launch_id": run.LaunchID, "answer": "Postgres"})
	if err != nil || got.Failed {
		t.Fatalf("answer_run = %+v, %v", got, err)
	}
	seen, _ := fencesIn(t, boundaryIn(t, got.Output), got.Output)
	if len(seen) != 1 || !strings.Contains(seen[0].opening, " trust="+string(want)+" ") {
		t.Errorf("answer_run reads the question as %+v, want trust %q", seen, want)
	}

	answers := map[string]any{
		"GET /sandbox-runs":      map[string]any{"runs": []any{asked}},
		"GET /worklet/decisions": map[string]any{"items": []any{map[string]any{"kind": "run", "run": asked}}},
		"GET /worklet/decisions/answered": map[string]any{"items": []any{map[string]any{"at": time.Now().UTC(), "run": asked,
			"answer": map[string]any{"choice": "postgres", "body": "Postgres", "by": "founder"}}}},
	}
	read := func(tool catalog.Tool, args catalog.Args, answer any) provenance.Trust {
		t.Helper()
		body, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		res, err := catalog.Shape(tool, args, exec.Response{Status: 200, Body: body},
			provenance.NewBoundary(), time.Now(), cards.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		return res.Trust
	}
	curated := map[string]string{"list_coding_runs": "GET /sandbox-runs",
		"list_decisions": "GET /worklet/decisions"}
	for name, route := range curated {
		tool, ok := catalog.Lookup(name)
		if !ok {
			t.Fatalf("the catalogue has no %s", name)
		}
		if trust := read(tool, catalog.Args{}, answers[route]); trust != want {
			t.Errorf("%s reads the ask at %q, want %q", name, trust, want)
		}
	}
	// AND ONE CHANGE, which the generic tools are built over too.
	routes := []catalog.Route{{Pattern: "PUT /config/roles/{id}"}}
	for route := range answers {
		routes = append(routes, catalog.Route{Pattern: route})
	}
	table, err := catalog.NewRouteTable(routes)
	if err != nil {
		t.Fatal(err)
	}
	generic := catalog.GenericTools(table, iam.Principal{Login: "founder", Seat: "founder"}, nil)[1]
	for route, answer := range answers {
		if trust := read(generic, catalog.Args{"route": route}, answer); trust != want {
			t.Errorf("a generic read of %s reads the ask at %q, want %q", route, trust, want)
		}
	}

	if tool, ok := catalog.Lookup("answer_coding_run"); !ok || tool.Trust != want {
		t.Errorf("the assistant's write that answers a run reads its receipt, which "+
			"carries the question, at %q, want %q", tool.Trust, want)
	}
}
