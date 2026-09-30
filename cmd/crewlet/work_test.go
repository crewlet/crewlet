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

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
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

	mu    sync.Mutex
	query url.Values
	item  string
	key   string

	// status and outcome are what the next purge answers, and unvouched
	// marks an unknown the node says it cannot settle.
	status    int
	outcome   string
	unvouched bool
	// hangUp drops the connection without an answer, after recording the
	// request — a node that did the work and whose answer never arrived.
	hangUp bool
}

func newFakePurgeNode(t *testing.T) *fakePurgeNode {
	t.Helper()
	n := &fakePurgeNode{status: http.StatusOK, outcome: "applied"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /work/items/{key}/purge", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.item, n.query = r.PathValue("key"), r.URL.Query()
		n.key = r.Header.Get(opkey.Header)
		status, outcome, unvouched, hangUp := n.status, n.outcome, n.unvouched, n.hangUp
		n.mu.Unlock()
		if hangUp {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		// THE ROUTE'S OWN SHAPE: the op_id is the key the request carried,
		// and an unknown is a 503 carrying it beside the receipt, with no
		// position.
		body := map[string]any{
			"task": "t-1", "key": r.URL.Query().Get("confirm"),
			"project": "ENG", "outcome": outcome,
			"op_id": r.Header.Get(opkey.Header),
		}
		switch {
		case status == http.StatusServiceUnavailable && outcome == "unknown":
			body["error"] = "unavailable"
			body["detail"] = "this node cannot establish what happened to this change"
			if unvouched {
				body["unvouched"] = true
			}
		case status == http.StatusServiceUnavailable:
			// A REFUSAL, which carries the key and no receipt.
			body = map[string]any{"error": "unavailable",
				"op_id":  r.Header.Get(opkey.Header),
				"detail": "the log is full"}
		default:
			body["position"] = "CREWLET_TRACKER_LOG:1:918280009"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// asked is the item, query and operation key the node last received.
func (n *fakePurgeNode) asked() (string, url.Values, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.item, n.query, n.key
}

// answers sets what the next purge answers.
func (n *fakePurgeNode) answers(status int, outcome string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status, n.outcome = status, outcome
}

// set changes the node's behaviour behind its lock, for the fields
// [fakePurgeNode.answers] does not take.
func (n *fakePurgeNode) set(change func(*fakePurgeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change(n)
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
	item, query, key := node.asked()
	if item != "t-1" {
		t.Errorf("the route was asked to purge %q", item)
	}
	for name, want := range map[string]string{
		"confirm": "ENG-42", "reason": "an erasure request",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("?%s= is %q, want %q", name, got, want)
		}
	}
	// THE PROJECT IS THE NODE'S TO KNOW: the stored row says which it is,
	// and a parameter the caller could set wrong filed a purge under a
	// project it was not about.
	if query.Has("project") {
		t.Errorf("the command still names a project: %v", query)
	}
	// AN OPERATION KEY ON EVERY PURGE, minted here when the operator brought
	// none, so the command holds the handle on what it asked for whether or
	// not an answer comes back — and minted as a node would, carrying the
	// instant a retry is judged by, or the node refuses it.
	if err := statelog.CheckCallerOpID(key); err != nil {
		t.Errorf("the %s is %q, want an id the command minted as a node "+
			"would: %v", opkey.Header, key, err)
	}
	if !strings.Contains(stdout, "(operation "+key+")") {
		t.Errorf("the purge never names the operation it ran under:\n%s", stdout)
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
	if item, _, _ := node.asked(); item != "" {
		t.Errorf("a refused purge still reached the node as %q", item)
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
	if item, _, _ := node.asked(); item != "" {
		t.Errorf("a purge with no reason still reached the node as %q", item)
	}
}

// AN -op-id THE ENGINE COULD NOT HAVE MINTED IS REFUSED BEFORE ANYTHING IS
// SENT, naming the flag: the node refuses it `400 op_id_invalid` anyway, and
// an id with no instant is one no ledger could vouch for.
func TestAPurgeUnderAnIDTheEngineNeverMintedIsRefusedBeforeItIsSent(t *testing.T) {
	node := newFakePurgeNode(t)
	_, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", "-op-id", "op-abc", bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "-op-id") {
		t.Fatalf("a purge under a hand-made id = %v, want a refusal naming -op-id", err)
	}
	if item, _, _ := node.asked(); item != "" {
		t.Errorf("a purge under a hand-made id still reached the node as %q", item)
	}
}

// AN UNKNOWN OUTCOME IS THE ONE TO RETRY, and a retry with a fresh operation
// id would append a SECOND purge of an item the first one may already have
// destroyed — so the id is printed, and the flag that reuses it carries it to
// the node as the operation key.
func TestAnUnknownPurgeNamesTheIdToRetryWith(t *testing.T) {
	node := newFakePurgeNode(t)
	node.answers(http.StatusServiceUnavailable, "unknown")
	_, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL))
	_, _, sent := node.asked()
	if err == nil || sent == "" || !strings.Contains(err.Error(), "-op-id "+sent) {
		t.Fatalf("an unknown outcome did not name the id %q to retry with: %v", sent, err)
	}
	// NOT A REFUSAL: a write nobody can account for may well have landed.
	if !strings.Contains(err.Error(), "could not establish whether this landed") {
		t.Errorf("an unknown outcome read as a refusal: %v", err)
	}

	// AND THE FLAG REACHES THE ROUTE, or the advice above is a sentence
	// pointing at a flag that does nothing.
	node.answers(http.StatusOK, "applied")
	if _, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", "-op-id", sent,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retrying with the printed id failed: %v", err)
	}
	if _, _, key := node.asked(); key != sent {
		t.Errorf("the retry carried the key %q — a retry with a fresh one "+
			"appends a second purge of an item the first may already have "+
			"destroyed", key)
	}
}

// EVERY 503 THAT NAMES AN OPERATION SAYS HOW TO RETRY IT UNDER THAT ONE, and
// the three say it differently because they send the operator different ways:
// an unknown this node can settle is retried here, an unvouched one through
// another node, and a refusal once the node can take it.
func TestEveryPurgeThatNamesItsOperationIsRetriedUnderIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcome   string
		unvouched bool
		says      string
	}{
		{"an unknown", "unknown", false, "No acknowledgement"},
		{"an unvouched unknown", "unknown", true, "another node with -url"},
		{"a refusal", "", false, "This attempt wrote nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newFakePurgeNode(t)
			node.answers(http.StatusServiceUnavailable, tc.outcome)
			node.set(func(n *fakePurgeNode) { n.unvouched = tc.unvouched })
			_, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
				"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL))
			_, _, sent := node.asked()
			if err == nil || sent == "" || !strings.Contains(err.Error(), "-op-id "+sent) ||
				!strings.Contains(err.Error(), tc.says) {
				t.Errorf("the purge answered %v, want %q and -op-id %s", err, tc.says, sent)
			}
		})
	}
}

// `pending` IS NOT A FAILURE AND MUST NOT BE RETRIED: the record is on the log
// and every node applies it as it reaches it. The node says so with a 202,
// which the command used to read as an error — the ordinary next move after
// which is running it again.
func TestAPendingPurgeSaysNotToRunItAgain(t *testing.T) {
	node := newFakePurgeNode(t)
	node.answers(http.StatusAccepted, "pending")
	stdout, _, err := cli(t, "work", "purge", "t-1", "-reason", "why",
		"-confirm", "ENG-42", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("a pending purge was reported as a failure: %v", err)
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
	node.set(func(n *fakePurgeNode) { n.hangUp = true })
	_, stderr, err := cli(t, "work", "purge", "t-1",
		"-reason", "why", "-confirm", "ENG-42",
		bootstrapForURL(t, node.server.URL))
	if err == nil {
		t.Fatal("a purge the node never answered reported success")
	}
	_, _, sent := node.asked()
	if sent == "" || !strings.Contains(stderr, "-op-id "+sent) {
		t.Fatalf("the unanswered purge sent the key %q and printed:\n%s\nwant the "+
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
	stdout, stderr, err := cli(t, "work", "purge", "t-1",
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
