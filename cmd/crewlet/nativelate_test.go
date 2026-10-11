package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A COMPANY APPLIED AFTER BOOT IS SERVED WHOLE: every native surface the API
// carries answers for it without a restart, because each is asked of the
// engine per request rather than captured when the API was assembled.
//
// The node boots with no company — the quickstart's live bootstrap, and the
// dashboard's company creation — so its native runtime is built by the first
// apply, after the API already exists. Every surface resolved at assembly
// read "no tracker" off a runtime that did not exist yet, and kept reading it
// for the life of the process: the board's questions came back
// `unknown_query` until somebody restarted the node.
func TestNativeSurfacesServeACompanyAppliedAfterBoot(t *testing.T) {
	t.Parallel()
	n := bootUnconfigured(t, "a-test-token")
	n.reconcile()
	if err := n.settle(n.put(companyYAML + `  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ops
`)); err != nil {
		t.Fatalf("the company: %v", err)
	}

	// THE READS: the board's and the knowledge base's questions, and the
	// state log's own retention answer, which a node running no state log
	// at boot did not register either.
	for _, question := range []string{"work_items", "pages", "retention"} {
		status, body := n.get(t, "/query/"+question)
		if status != http.StatusOK {
			t.Errorf("GET /query/%s = %d %s, want it answered for the company applied "+
				"after boot", question, status, body)
		}
	}

	// THE ROUTES: a file's bytes, the purge and the eviction gate, each
	// reaching its own handler — which answers with a refusal of its own
	// that writes nothing — where a route resolved at assembly answered the
	// mux's `no_route` (the file bytes and the purge, never mounted) or
	// `no_state_log` (the gate, handed no state log) for the life of the
	// process.
	for _, route := range []struct {
		method, path string
		status       int
		code         string
	}{
		// The tracker answered: there is no such project.
		{http.MethodGet, "/work/files/NOSUCH/no-such.txt", http.StatusNotFound, "no_project"},
		// And the upload, the write half on its own, through the same
		// tracker read — before a byte of the body is stored.
		{http.MethodPut, "/work/files/NOSUCH/no-such.txt", http.StatusNotFound, "no_project"},
		// The purge's own refusal, before anything is judged.
		{http.MethodPost, "/work/no-such/purge?confirm=X", http.StatusBadRequest,
			"project_required"},
		// The gate's own refusal, before anything is judged.
		{http.MethodPost, "/work/retention/evict/no-such-node", http.StatusBadRequest,
			"confirm_required"},
	} {
		status, body := n.send(t, route.method, route.path)
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &answer)
		if status != route.status || answer.Error != route.code {
			t.Errorf("%s %s = %d %s, want %d %s from its own handler for the company "+
				"applied after boot", route.method, route.path, status, body,
				route.status, route.code)
		}
	}
}
