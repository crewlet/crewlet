package workapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THIS SURFACE ANSWERS A PERSON AS THE ACT ROUTE DOES (ADR-0060), through the
// same two functions: a write that landed answers its caveats in a person's
// words under `caveats` and never the tool's `warnings`, written for a model;
// a refusal answers a person's words or its code's own sentence, never the
// tool's. The route answered the model's `warnings` beside every receipt, and
// a receipt it could not read with the tool's own words as its `detail`.
// Every sentence each answer carries, at any depth, is held to a person's
// words. Mutation: answer the tool's receipt as it wrote it and the first
// half goes red on `warnings`.
func TestTheWorkSurfaceAnswersAPersonInAPersonsWords(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.writes.caveats = []caveat.Caveat{caveat.DefaultAssigneeGone("ENG", "bo"),
		caveat.BodyLinksCapped(31, 25)}
	got := r.do(as(admin("ana")), http.MethodPost, "/worklet/items",
		map[string]any{"title": "Rotate the key", "project": "ENG"})
	if got.status != http.StatusOK {
		t.Fatalf("a create answered %d %v", got.status, got.body)
	}
	if said, carried := got.body["warnings"]; carried {
		t.Errorf("the answer carries the model's warnings: %v", said)
	}
	if caveats, _ := got.body["caveats"].([]any); len(caveats) != 2 {
		t.Errorf("the answer carries caveats %v, want both in a person's words",
			got.body["caveats"])
	}
	for _, problem := range caveattest.AnswerProblems(worklet.CreateWorkItemTool, got.body) {
		t.Errorf("a create: %s", problem)
	}

	refused := newRig(t, chart{})
	refused.writes.err = fmt.Errorf("%w: the `labels` name no tag; declare them "+
		"with write_project first", worklet.ErrInvalid)
	got = refused.do(as(admin("ana")), http.MethodPost, "/worklet/items",
		map[string]any{"title": "Rotate the key", "project": "ENG"})
	if got.status != http.StatusUnprocessableEntity ||
		got.body["message"] != httpjson.CodeInvalid.Message() || got.detail() != "" {
		t.Errorf("a refused create answered %d %v, want its code's own sentence",
			got.status, got.body)
	}
	for _, problem := range caveattest.AnswerProblems(worklet.CreateWorkItemTool, got.body) {
		t.Errorf("a refused create: %s", problem)
	}
}
