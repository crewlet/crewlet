package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakePurgeNode answers the purge route, recording what it was asked.
type fakePurgeNode struct {
	server  *httptest.Server
	query   url.Values
	task    string
	outcome string
}

func newFakePurgeNode(t *testing.T) *fakePurgeNode {
	t.Helper()
	n := &fakePurgeNode{outcome: "applied"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /work/{id}/purge", func(w http.ResponseWriter, r *http.Request) {
		n.task, n.query = r.PathValue("id"), r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task": r.PathValue("id"), "key": r.URL.Query().Get("confirm"),
			"project": r.URL.Query().Get("project"), "outcome": n.outcome,
			"position": map[string]any{
				"stream": "CREWLET_TRACKER_LOG", "seq": 918280009,
			},
			"op_id": "op-abc",
		})
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// THE ONE OPERATION NOTHING UNDOES HAD NO CALLER AT ALL.
//
// `PurgeTask` existed, its record applied, its marker stopped a redelivery
// resurrecting anything — and no CLI verb, no route and no tool reached it. A
// company could not destroy a task under any circumstances: an erasure request
// had no mechanism, and a credential pasted into a task body stayed in the
// durable rows of every node for ever.
func TestWorkPurgeReachesTheOneOperationNothingUndoes(t *testing.T) {
	node := newFakePurgeNode(t)
	stdout, stderr, err := cli(t, "work", "purge", "t-1",
		"-project", "ENG", "-reason", "an erasure request", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("work purge: %v\n%s", err, stderr)
	}
	if node.task != "t-1" {
		t.Errorf("the route was asked to purge %q", node.task)
	}
	for key, want := range map[string]string{
		"confirm": "ENG-42", "project": "ENG", "reason": "an erasure request",
	} {
		if got := node.query.Get(key); got != want {
			t.Errorf("?%s= is %q, want %q", key, got, want)
		}
	}
	// WHAT A PURGE DOES NOT REACH, printed where the gesture is run. An
	// operator acting on an erasure request needs to know that an offline
	// disk keeps its copy, and a docs page they have not opened is not
	// where they learn it.
	if !strings.Contains(stdout, "offline or evicted") {
		t.Errorf("the purge never says what it does not reach:\n%s", stdout)
	}
}

// A CONFIRMATION THAT REPEATS THE ID CONFIRMS NOTHING, because the id is on
// the command line already. The KEY has to be looked up, which is the point.
func TestAPurgeWithoutTheTasksKeyIsRefused(t *testing.T) {
	node := newFakePurgeNode(t)
	for _, args := range [][]string{
		{"work", "purge", "t-1", "-project", "ENG", "-reason", "why"},
		{"work", "purge", "-project", "ENG", "-reason", "why", "-confirm", "ENG-42"},
	} {
		if _, _, err := cli(t, append(args,
			bootstrapForURL(t, node.server.URL))...); err == nil {
			t.Errorf("%v ran without a confirmation", args)
		}
	}
	if node.task != "" {
		t.Errorf("a refused purge still reached the node as %q", node.task)
	}
}

// A REASON IS REQUIRED because it is the only thing that survives: the rows
// are destroyed, and the marker's reason is the entire account of what used to
// be at that key for whoever reads it a year later.
func TestAPurgeWithNoReasonIsRefusedBeforeItIsSent(t *testing.T) {
	node := newFakePurgeNode(t)
	if _, _, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL)); err == nil {
		t.Fatal("a purge with no reason ran")
	}
	if node.task != "" {
		t.Errorf("a purge with no reason still reached the node as %q", node.task)
	}
}

// `unknown` IS THE ONE OUTCOME TO RETRY, and a retry with a fresh operation id
// would append a SECOND purge of a task the first one may already have
// destroyed — so the id is printed and the flag that reuses it exists.
func TestAnUnknownPurgeNamesTheIdToRetryWith(t *testing.T) {
	node := newFakePurgeNode(t)
	node.outcome = "unknown"
	stdout, _, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("work purge: %v", err)
	}
	if !strings.Contains(stdout, "op-abc") {
		t.Errorf("an unknown outcome never named the operation id:\n%s", stdout)
	}

	// AND THE FLAG REACHES THE ROUTE, or the advice above is a sentence
	// pointing at a flag that does nothing.
	if _, _, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42", "-op-id", "op-abc",
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retrying with the printed id failed: %v", err)
	}
	if got := node.query.Get("op_id"); got != "op-abc" {
		t.Errorf("?op_id= is %q — a retry with a fresh id appends a second "+
			"purge of a task the first one may already have destroyed", got)
	}
}

// `pending` IS NOT A FAILURE AND MUST NOT BE RETRIED: the record is on the log
// and every node applies it as it reaches it.
func TestAPendingPurgeSaysNotToRunItAgain(t *testing.T) {
	node := newFakePurgeNode(t)
	node.outcome = "pending"
	stdout, _, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("work purge: %v", err)
	}
	if !strings.Contains(stdout, "Do not run this again") {
		t.Errorf("a pending purge read as something to retry:\n%s", stdout)
	}
}
