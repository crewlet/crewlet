package configapi_test

import (
	"net/http"
	"testing"
)

// A WRITE OF PART OF THE DOCUMENT THAT CHANGES NOTHING STORES NOTHING.
//
// A patch or an entity's PUT is "change these fields". One that changes none
// was stored as a second revision under a summary saying what the first one
// did ("Add three human seats" twice, a diff of nothing between them) and its
// activation rebuilt every node's seats. It is answered `200 changed: false`
// with the revision that already says it, needs no summary, and moves
// nothing; a check of it says `changed: false` too. The whole document sent
// back with PUT /config is the activation a caller who renders the document
// makes, and stays one: the control.
//
// Mutation: drop the `unchanged` answers in the PATCH and entity handlers,
// and each unchanged write lands as a new revision again.
func TestAPartialWriteThatChangesNothingStoresNothing(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, entityDoc)
	patchOnly(t, s, `{"mission": "ship the thing"}`, summaryHeader)
	before, count := s.activeID(t), s.revisionCount(t)

	unchanged := func(name string, code int, body map[string]any) {
		t.Helper()
		if code != http.StatusOK || body["changed"] != false || body["revision_id"] != before {
			t.Errorf("%s = %d %v, want 200 changed: false naming %s", name, code, body, before)
		}
	}

	// THE SAME PATCH AGAIN, and without a summary: no revision records one.
	res := s.do(t, http.MethodPatch, "/config", `{"mission": "ship the thing"}`, nil)
	unchanged("the same patch again", res.Code, decode(t, res))

	// AN ENTITY SENT BACK AS IT WAS READ, under its own tag.
	read := s.do(t, http.MethodGet, "/config/mcp-servers/tracker", "", nil)
	res = s.do(t, http.MethodPut, "/config/mcp-servers/tracker", read.Body.String(),
		map[string]string{"X-Summary": "send it back", "If-Match": read.Header().Get("ETag")})
	unchanged("the entity sent back", res.Code, decode(t, res))
	if tag := res.Header().Get("ETag"); tag != read.Header().Get("ETag") {
		t.Errorf("the unchanged entity answered ETag %s, want the read's %s",
			tag, read.Header().Get("ETag"))
	}

	if after, n := s.activeID(t), s.revisionCount(t); after != before || n != count {
		t.Fatalf("a write that changed nothing stored a revision: active %s (was %s), "+
			"%d revisions (were %d)", after, before, n, count)
	}

	// A CHECK SAYS WHETHER ITS WRITE WOULD CHANGE ANYTHING.
	for _, tc := range []struct {
		name, body string
		changed    bool
	}{
		{"a check of the same patch", `{"mission": "ship the thing"}`, false},
		{"a check of a new mission", `{"mission": "ship the next thing"}`, true},
	} {
		res := s.do(t, http.MethodPatch, "/config?dry_run=true", tc.body, nil)
		body := decode(t, res)
		if res.Code != http.StatusOK || body["valid"] != true || body["changed"] != tc.changed {
			t.Errorf("%s = %d %v, want 200 valid, changed: %v", tc.name, res.Code, body, tc.changed)
		}
	}

	// THE CONTROL: a whole document sent back is published again, as the
	// activation it is for a caller who renders the document.
	document := s.do(t, http.MethodGet, "/config?format=yaml", "", nil)
	res = s.do(t, http.MethodPut, "/config", document.Body.String(),
		map[string]string{"X-Summary": "publish it again", "If-Match": document.Header().Get("ETag")})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT of the document as it stands = %d %s, want 201", res.Code, res.Body.String())
	}
	if s.activeID(t) == before {
		t.Error("PUT of the document as it stands did not publish it again")
	}
}
