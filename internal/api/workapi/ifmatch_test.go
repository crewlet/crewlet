package workapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
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
// Mutation: answer a conditioned call through [operator.Fail], or fold the
// header without saying so, and every header row answers 409.
func TestAVersionTheHeaderStatedIsAFailedPrecondition(t *testing.T) {
	t.Parallel()
	stale := func(r *rig) { r.writes.err = tracker.ErrStaleVersion }
	pageStale := func(r *rig) {
		r.kb.err = fmt.Errorf("%w: the base is 2 and the page is at 3", pages.ErrStaleVersion)
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
		{"an edit under If-Match", http.MethodPatch, "/work/items/ENG-1",
			map[string]any{"status": "done"}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"an edit stating its version in the body", http.MethodPatch, "/work/items/ENG-1",
			map[string]any{"status": "done", "if_match": 3}, "", stale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"a placement under If-Match", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2"}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a placement stating its version in the body", http.MethodPost, "/work/items/ENG-1/rank",
			map[string]any{"after": "ENG-2", "if_match": 3}, "", stale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"a dependency under If-Match", http.MethodPost, "/work/items/ENG-1/depend",
			map[string]any{"waiting_on": map[string]any{"add": []any{"ENG-2"}}}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a relation under If-Match", http.MethodPost, "/work/items/ENG-1/relate",
			map[string]any{"linked": map[string]any{"add": []any{"ENG-2"}}}, "3", stale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a page save under If-Match", http.MethodPut, "/pages/p-1",
			map[string]any{"body": "new steps"}, "2", pageStale,
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"a page save stating its base in the body", http.MethodPut, "/pages/p-1",
			map[string]any{"body": "new steps", "base_version": 2}, "", pageStale,
			http.StatusConflict, httpjson.CodeStaleVersion},
		{"another refusal under If-Match is its own", http.MethodPatch, "/work/items/ENG-1",
			map[string]any{"status": "done"}, "3",
			func(r *rig) { r.writes.err = fmt.Errorf("%w: no such status", tracker.ErrInvalid) },
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
			got := r.do(as(admin("ana")), c.method, c.target, c.body, headers...)
			if got.status != c.want || got.body["error"] != string(c.code) {
				t.Fatalf("answered %d %v, want %d %s", got.status, got.body, c.want, c.code)
			}
			if got.body["detail"] == nil || got.body["tool"] == nil {
				t.Errorf("the refusal dropped the tool's sentence or what it was about: %v",
					got.body)
			}
		})
	}
}
