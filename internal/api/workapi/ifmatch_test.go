package workapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/modelnote"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A VERSION THE REQUEST STATED AS AN If-Match HEADER IS HTTP'S PRECONDITION,
// and a stale one is answered as HTTP answers a failed one: `412
// version_moved` (RFC 9110 §13.1.1), the status a project file's write and an
// artifact's publish answer a moved version with. The SAME version stated in
// the body is the tool's own argument, refused `409 stale_version` by the one
// class mapping every transport shares — the act route's, which the dashboard
// writes through, included. A route that takes the header was answering its
// failure 409, so the one surface where a person's HTTP client sent the
// standard precondition was the one that did not answer it the standard way.
//
// Every route that folds the header is a row, and each refused for something
// else under the header is still that refusal: only a stale version changes.
//
// EITHER WAY THE ANSWER IS A PERSON'S (ADR-0060): what the call was about and
// its code's own sentence, every sentence it carries worded for a person, and
// the tool's own sentence told to the model that made the request, where one
// did, and to nobody else (internal/modelnote), exactly as a refusal of a
// version stated in the body is: the header moves the status of a stale
// version, and nothing about who hears which words.
//
// Mutation: answer a conditioned call through [operator.FailTool], or fold the
// header without saying so, and every header row answers 409; leave the
// model's half out of [operator.FailToolIfMatched] and every header row's
// model hears nothing.
func TestAVersionTheHeaderStatedIsAFailedPrecondition(t *testing.T) {
	t.Parallel()
	stale := func(r *rig) { r.writes.err = worklet.ErrStaleVersion }
	pageStale := func(r *rig) {
		r.kb.err = fmt.Errorf("%w: the base is 2 and the page is at 3", doclet.ErrStaleVersion)
	}
	for _, c := range []struct {
		name    string
		method  string
		target  string
		body    map[string]any
		ifMatch string
		setup   func(*rig)
		want    int
		code    httpjson.Code
	}{
		{"an edit under If-Match", http.MethodPatch, "/worklet/items/ENG-1",
			map[string]any{"status": "done"}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"an edit stating its version in the body", http.MethodPatch, "/worklet/items/ENG-1",
			map[string]any{"status": "done", "if_match": 3}, "", stale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"a placement under If-Match", http.MethodPost, "/worklet/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a placement stating its version in the body", http.MethodPost, "/worklet/items/ENG-1/rank",
			map[string]any{"after": "ENG-2", "if_match": 3}, "", stale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"a dependency under If-Match", http.MethodPost, "/worklet/items/ENG-1/depend",
			map[string]any{"waiting_on": map[string]any{"add": []any{"ENG-2"}}}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a relation under If-Match", http.MethodPost, "/worklet/items/ENG-1/relate",
			map[string]any{"linked": map[string]any{"add": []any{"ENG-2"}}}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a page save under If-Match", http.MethodPut, "/doclet/pages/p-1",
			map[string]any{"body": "new steps"}, "2", pageStale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a page save stating its base in the body", http.MethodPut, "/doclet/pages/p-1",
			map[string]any{"body": "new steps", "base_version": 2}, "", pageStale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"another refusal under If-Match is its own", http.MethodPatch, "/worklet/items/ENG-1",
			map[string]any{"status": "done"}, "3",
			func(r *rig) { r.writes.err = fmt.Errorf("%w: no such status", worklet.ErrInvalid) },
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			c.setup(r)
			var headers []string
			if c.ifMatch != "" {
				headers = []string{"If-Match", c.ifMatch}
			}
			var heard *modelnote.Note
			expecting := func(ctx context.Context) context.Context {
				ctx, heard = modelnote.Expect(as(admin("ana"))(ctx))
				return ctx
			}
			got := r.do(expecting, c.method, c.target, c.body, headers...)
			if got.status != c.want || got.body["error"] != string(c.code) {
				t.Fatalf("answered %d %v, want %d %s", got.status, got.body, c.want, c.code)
			}
			tool, _ := got.body["tool"].(string)
			if tool == "" || got.body["message"] != c.code.Message() {
				t.Errorf("the refusal dropped what it was about or its code's own "+
					"sentence: %v", got.body)
			}
			for _, problem := range caveattest.AnswerProblems(tool, got.body) {
				t.Errorf("the refusal: %s", problem)
			}
			said := heard.Said()
			if said == "" {
				t.Fatalf("the model that made the request heard nothing of the refusal")
			}
			if strings.Contains(fmt.Sprint(got.body), said) {
				t.Errorf("the person's answer carries the model's sentence %q: %v",
					said, got.body)
			}
		})
	}
}
