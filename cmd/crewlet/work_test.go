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
	server *httptest.Server
	query  url.Values
	item   string
	key    string

	// status and outcome are what the next purge answers.
	status  int
	outcome string
}

func newFakePurgeNode(t *testing.T) *fakePurgeNode {
	t.Helper()
	n := &fakePurgeNode{status: http.StatusOK, outcome: "applied"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /work/items/{key}/purge", func(w http.ResponseWriter, r *http.Request) {
		n.item, n.query = r.PathValue("key"), r.URL.Query()
		n.key = r.Header.Get(idempotencyHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(n.status)
		body := map[string]any{
			"task": "t-1", "key": r.URL.Query().Get("confirm"),
			"project": "ENG", "outcome": n.outcome,
			"position": "CREWLET_TRACKER_LOG:1:918280009",
			"op_id":    "op-abc",
		}
		if n.status == http.StatusServiceUnavailable {
			body = map[string]any{"error": "unavailable", "op_id": "op-abc",
				"detail": "this node cannot establish what happened to this change"}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// THE ONE OPERATION NOTHING UNDOES REACHES THE WRITE SURFACE'S OWN ROUTE.
//
// `PurgeTask` existed, its record applied, its marker stopped a redelivery
// resurrecting anything — and no CLI verb, no route and no tool reached it. A
// company could not destroy a task under any circumstances: an erasure request
// had no mechanism, and a credential pasted into a task body stayed in the
// durable rows of every node for ever.
func TestWorkPurgeReachesTheOneOperationNothingUndoes(t *testing.T) {
	node := newFakePurgeNode(t)
	stdout, stderr, err := cli(t, "work", "purge", "t-1",
		"-reason", "an erasure request", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("work purge: %v\n%s", err, stderr)
	}
	if node.item != "t-1" {
		t.Errorf("the route was asked to purge %q", node.item)
	}
	for key, want := range map[string]string{
		"confirm": "ENG-42", "reason": "an erasure request",
	} {
		if got := node.query.Get(key); got != want {
			t.Errorf("?%s= is %q, want %q", key, got, want)
		}
	}
	// THE PROJECT IS THE NODE'S TO KNOW: the stored row says which it is,
	// and a parameter the caller could set wrong filed a purge under a
	// project it was not about.
	if node.query.Has("project") {
		t.Errorf("the command still names a project: %v", node.query)
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
func TestAPurgeWithoutTheItemsKeyIsRefused(t *testing.T) {
	node := newFakePurgeNode(t)
	for _, args := range [][]string{
		{"work", "purge", "t-1", "-reason", "why"},
		{"work", "purge", "-reason", "why", "-confirm", "ENG-42"},
	} {
		if _, _, err := cli(t, append(args,
			bootstrapForURL(t, node.server.URL))...); err == nil {
			t.Errorf("%v ran without a confirmation", args)
		}
	}
	if node.item != "" {
		t.Errorf("a refused purge still reached the node as %q", node.item)
	}
}

// A REASON IS REQUIRED because it is the only thing that survives: the rows
// are destroyed, and the marker's reason is the entire account of what used to
// be at that key for whoever reads it a year later.
func TestAPurgeWithNoReasonIsRefusedBeforeItIsSent(t *testing.T) {
	node := newFakePurgeNode(t)
	if _, _, err := cli(t, "work", "purge", "t-1",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL)); err == nil {
		t.Fatal("a purge with no reason ran")
	}
	if node.item != "" {
		t.Errorf("a purge with no reason still reached the node as %q", node.item)
	}
}

// AN UNKNOWN OUTCOME IS THE ONE TO RETRY, and a retry with a fresh operation
// id would append a SECOND purge of an item the first one may already have
// destroyed — so the id is printed, and the flag that reuses it carries it to
// the node as the operation key.
func TestAnUnknownPurgeNamesTheIdToRetryWith(t *testing.T) {
	node := newFakePurgeNode(t)
	node.status = http.StatusServiceUnavailable
	_, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "-op-id op-abc") {
		t.Fatalf("an unknown outcome did not name the id to retry with: %v", err)
	}

	// AND THE FLAG REACHES THE ROUTE, or the advice above is a sentence
	// pointing at a flag that does nothing.
	node.status = http.StatusOK
	if _, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", "-op-id", "op-abc",
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retrying with the printed id failed: %v", err)
	}
	if node.key != "op-abc" {
		t.Errorf("the retry carried the key %q — a retry with a fresh one "+
			"appends a second purge of an item the first may already have "+
			"destroyed", node.key)
	}
}

// `pending` IS NOT A FAILURE AND MUST NOT BE RETRIED: the record is on the log
// and every node applies it as it reaches it. The node says so with a 202,
// which the command used to read as an error — the ordinary next move after
// which is running it again.
func TestAPendingPurgeSaysNotToRunItAgain(t *testing.T) {
	node := newFakePurgeNode(t)
	node.status, node.outcome = http.StatusAccepted, "pending"
	stdout, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("a pending purge was reported as a failure: %v", err)
	}
	if !strings.Contains(stdout, "Do not run this again") {
		t.Errorf("a pending purge read as something to retry:\n%s", stdout)
	}
}
