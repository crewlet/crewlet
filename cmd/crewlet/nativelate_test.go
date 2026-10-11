package main

import (
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
}
