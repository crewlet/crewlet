package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakePurgeNode answers the purge route, recording what it was asked.
//
// ITS FIELDS ARE READ BEHIND A LOCK, because a request it hangs up on gives the
// command nothing to synchronise with: the handler's writes and the test's
// reads are otherwise ordered only by a closed socket, which the race detector
// cannot see.
type fakePurgeNode struct {
	server *httptest.Server

	mu      sync.Mutex
	query   url.Values
	task    string
	outcome string
	// hangUp drops the connection without an answer, after recording the
	// request — a node that did the work and whose answer never arrived.
	hangUp bool
}

func newFakePurgeNode(t *testing.T) *fakePurgeNode {
	t.Helper()
	n := &fakePurgeNode{outcome: "applied"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /work/{id}/purge", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.task, n.query = r.PathValue("id"), r.URL.Query()
		outcome, hangUp := n.outcome, n.hangUp
		n.mu.Unlock()
		if hangUp {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		answer := map[string]any{
			"task": r.PathValue("id"), "key": r.URL.Query().Get("confirm"),
			"project": r.URL.Query().Get("project"), "outcome": outcome,
			"op_id": r.URL.Query().Get("op_id"),
		}
		// THE ROUTE'S OWN RULE: no position for an unknown outcome.
		if outcome != "unknown" {
			answer["position"] = map[string]any{
				"stream": "CREWLET_TRACKER_LOG", "seq": 918280009,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// asked is the task and query the node last received.
func (n *fakePurgeNode) asked() (string, url.Values) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.task, n.query
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
	task, query := node.asked()
	if task != "t-1" {
		t.Errorf("the route was asked to purge %q", task)
	}
	for key, want := range map[string]string{
		"confirm": "ENG-42", "project": "ENG", "reason": "an erasure request",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("?%s= is %q, want %q", key, got, want)
		}
	}
	// AN OPERATION ID ON EVERY PURGE, minted here when the operator brought
	// none, so the command holds the handle on what it asked for whether or
	// not an answer comes back.
	if _, minted := statelog.OpMintedAt(query.Get("op_id")); !minted {
		t.Errorf("?op_id= is %q, want an id the command minted as a node "+
			"would, carrying the instant a retry is judged by", query.Get("op_id"))
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
	if task, _ := node.asked(); task != "" {
		t.Errorf("a refused purge still reached the node as %q", task)
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
	if task, _ := node.asked(); task != "" {
		t.Errorf("a purge with no reason still reached the node as %q", task)
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
	_, query := node.asked()
	if sent := query.Get("op_id"); sent == "" || !strings.Contains(stdout, "-op-id "+sent) {
		t.Errorf("an unknown outcome never named the operation id %q:\n%s", sent, stdout)
	}
	// NO POSITION, which is the whole content of unknown: "at 0" read as a
	// record landed at the log's origin.
	if !strings.Contains(stdout, "unknown — the record may or may not be on the log") ||
		strings.Contains(stdout, "unknown at") {
		t.Errorf("an unknown outcome was printed with a position:\n%s", stdout)
	}

	// AND THE FLAG REACHES THE ROUTE, or the advice above is a sentence
	// pointing at a flag that does nothing.
	printed := statelog.NewOpID(time.Now().Add(-time.Minute), "purge-t-1")
	if _, _, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42", "-op-id", printed,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retrying with the printed id failed: %v", err)
	}
	if _, query := node.asked(); query.Get("op_id") != printed {
		t.Errorf("?op_id= is %q — a retry with a fresh id appends a second "+
			"purge of a task the first one may already have destroyed", query.Get("op_id"))
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

// A PURGE THE NODE NEVER ANSWERED NAMES THE OPERATION THAT FINISHES IT.
//
// The id was the node's to mint, so a request that timed out or dropped — which
// may well have landed — left the operator nothing to retry under but a fresh
// id, and a second purge. The command mints it before it asks and prints it,
// with the flag, when no answer comes back. And it waits past the write path's
// own waits rather than the ten seconds every other verb gives a network round
// trip.
func TestAPurgeTheNodeNeverAnsweredNamesItsOperation(t *testing.T) {
	node := newFakePurgeNode(t)
	node.hangUp = true
	_, stderr, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err == nil {
		t.Fatal("a purge the node never answered reported success")
	}
	_, query := node.asked()
	sent := query.Get("op_id")
	if sent == "" || !strings.Contains(stderr, "-op-id "+sent) {
		t.Fatalf("the unanswered purge sent op_id %q and printed:\n%s\nwant the "+
			"retry that names it", sent, stderr)
	}
	if purgeRequestTimeout <= 3*statelog.DefaultResolveBudget ||
		purgeRequestTimeout <= nodeRequestTimeout {
		t.Errorf("work purge waits %s, not past the three %s waits a purge can "+
			"make and the %s a round trip gets", purgeRequestTimeout,
			statelog.DefaultResolveBudget, nodeRequestTimeout)
	}
}

// A PURGE A NODE DOES NOT SERVE IS REFUSED, not left unknown.
//
// The route is absent on a node with no native tracker, and the mux answered
// it with net/http's text/plain 404 — which carries no engine code, so this
// command read it as a gateway that might have swallowed the purge: "whether
// the purge landed is unknown", and an -op-id retry that met the same 404 for
// ever. The node's own mux ([httpjson.Mux]) answers JSON with a code now, and
// that is a refusal: nothing was done.
func TestAPurgeTheNodeDoesNotServeIsRefusedNotUnknown(t *testing.T) {
	server := httptest.NewServer(httpjson.Mux(http.NewServeMux()))
	t.Cleanup(server.Close)
	stdout, stderr, err := cli(t, "work", "purge", "t-1", "-project", "ENG",
		"-reason", "why", "-confirm", "ENG-42", bootstrapForURL(t, server.URL))
	if err == nil {
		t.Fatalf("a purge the node does not serve exited zero:\n%s", stdout)
	}
	var lost noAnswer
	if errors.As(err, &lost) || strings.Contains(stderr, "-op-id") ||
		strings.Contains(stderr+stdout, "unknown") {
		t.Errorf("the node's own 404 was read as no answer (%v):\n%s%s", err,
			stdout, stderr)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no_route") {
		t.Errorf("the refusal does not say what the node answered: %v", err)
	}
}
