package configapi_test

import (
	"net/http"
	"testing"
)

// NO /config RESPONSE MAY BE STORED, whatever it answered.
//
// A /config body is the company document: the org chart, contact identities
// and the ${VAR} name behind every credential. With an ETag and no
// Cache-Control a browser writes it to its disk cache, where it outlives the
// session and the token that read it. So every answer carries no-store: a
// read, a 304, a write, every kind of refusal, and the 404 and 405 the surface
// answers for a path or method it does not serve. The last two are the cases a
// per-handler header would miss, and the reason the whole surface sits behind
// one wrapper.
func TestNoConfigResponseIsCacheable(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	revision := s.seed(t, companyDoc, nil)
	tag := s.do(t, http.MethodGet, "/config", "", nil).Header().Get("ETag")
	if tag == "" {
		t.Fatal("GET /config carries no ETag, so the 304 case cannot be exercised")
	}

	for _, tc := range []struct {
		name, method, path, body string
		headers                  map[string]string
		status                   int
	}{
		{"the document", http.MethodGet, "/config", "", nil, http.StatusOK},
		{"the document as YAML", http.MethodGet, "/config?format=yaml", "", nil, http.StatusOK},
		{"a revalidation", http.MethodGet, "/config", "", map[string]string{"If-None-Match": tag}, http.StatusNotModified},
		{"the reference index", http.MethodGet, "/config/references", "", nil, http.StatusOK},
		{"the history", http.MethodGet, "/config/revisions", "", nil, http.StatusOK},
		{"one revision", http.MethodGet, "/config/revisions/" + revision, "", nil, http.StatusOK},
		{"a diff", http.MethodGet, "/config/revisions/" + revision + "/diff?against=active", "", nil, http.StatusOK},
		{"an unknown revision", http.MethodGet, "/config/revisions/00000000-0000-0000-0000-000000000000", "", nil, http.StatusNotFound},
		{"one entity", http.MethodGet, "/config/llm-providers/zulu", "", nil, http.StatusOK},
		{"a chart entity this door does not write", http.MethodPut, "/config/roles/ceo",
			`{"name":"CEO","handle":"ceo"}`, map[string]string{"X-Summary": "x"}, http.StatusBadRequest},
		{"the accepted patch formats", http.MethodOptions, "/config", "", nil, http.StatusNoContent},
		{"a write refused for its summary", http.MethodPut, "/config", companyDoc, nil, http.StatusBadRequest},
		{"a write refused for its document", http.MethodPut, "/config", "name: [", map[string]string{"X-Summary": "broken"}, http.StatusBadRequest},
		{"a patch in the wrong format", http.MethodPatch, "/config", "{}", map[string]string{"Content-Type": "text/plain", "X-Summary": "x"}, http.StatusUnsupportedMediaType},
		{"a write", http.MethodPut, "/config", companyDoc, map[string]string{"X-Summary": "rewrite"}, http.StatusCreated},
		{"a method the surface does not serve", http.MethodDelete, "/config", "", nil, http.StatusMethodNotAllowed},
		{"a path the surface does not serve", http.MethodGet, "/config/nothing-here", "", nil, http.StatusNotFound},
	} {
		res := s.do(t, tc.method, tc.path, tc.body, tc.headers)
		if res.Code != tc.status {
			t.Errorf("%s: %s %s answered %d, want %d, so the case is not the one named: %s",
				tc.name, tc.method, tc.path, res.Code, tc.status, res.Body)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: %s %s answered %d with Cache-Control %q, want no-store",
				tc.name, tc.method, tc.path, res.Code, got)
		}
	}
}
