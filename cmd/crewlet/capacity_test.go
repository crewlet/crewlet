package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx"
)

// fakeCapacityNode answers the maintenance surface, recording what it was
// asked for.
type fakeCapacityNode struct {
	server *httptest.Server

	stream, bytes, confirm string
	assert                 bool
	status                 map[string]any
	operation              map[string]any
	reanchorConfirm        string

	// abandonConfirm and excluded are what the last abandon and exclusion
	// were asked with, and gesture is the operation either answers: nil
	// for a window cleared outright.
	abandonConfirm, excluded string
	gesture                  map[string]any

	// left is what a set-capacity or abandonment answer names as still
	// open: `open_windows` and `unread_windows`, none when unset.
	left map[string]any

	// reanchorCase is the case the node's status reports — "recreated" when
	// unset, and none at all when it is "-" — with reanchorCursor the
	// sequence it would put the checkpoint at.
	reanchorCase   string
	reanchorCursor uint64

	// reanchorDiscards makes the status name a record written after a
	// restore that the node's rows do not hold, and reanchorDiscard is
	// whether the transition was asked to discard it.
	reanchorDiscards bool
	reanchorDiscard  bool

	// reanchorTakes is how long the transition takes to answer — a node
	// rebuilding its consumer on a slow metadata group.
	reanchorTakes time.Duration

	// statusUnread makes the status read answer that the node couldn't read
	// the window, as a node does while coordination isn't answering.
	statusUnread bool
}

func newFakeCapacityNode(t *testing.T) *fakeCapacityNode {
	t.Helper()
	n := &fakeCapacityNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worklet/retention/capacity", func(w http.ResponseWriter, r *http.Request) {
		n.stream = r.URL.Query().Get("stream")
		n.bytes = r.URL.Query().Get("bytes")
		n.confirm = r.URL.Query().Get("confirm")
		n.assert = r.URL.Query().Get("assert_excluded") == "true"
		reply(w, n.withLeft(map[string]any{"mode": "maintenance", "operation": n.op()}))
	})
	mux.HandleFunc("GET /worklet/retention/maintenance", func(w http.ResponseWriter, r *http.Request) {
		n.stream = r.URL.Query().Get("stream")
		if n.statusUnread {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "maintenance_unreadable",
				"detail": "Couldn't read whether a capacity window is open on " +
					n.stream + " just now. Ask again in a moment, and if it keeps " +
					"failing, this node's log says why.",
			})
			return
		}
		body := n.status
		if body == nil {
			body = map[string]any{
				"stream": n.stream, "open": true, "mode": "seal",
				"sealed":    false,
				"blocking":  "node-2 has not acknowledged",
				"operation": n.op(),
				"acks": []map[string]any{{
					"node_id": "node-1", "operation_id": "op-1", "attempt": 1,
					"incarnation": "node-1:new", "mode": "seal",
				}},
				"admissions": []map[string]any{{
					"node_id": "node-3", "incarnation": "node-3:x",
				}},
			}
		}
		reply(w, body)
	})
	mux.HandleFunc("POST /worklet/retention/maintenance/abandon", func(w http.ResponseWriter, r *http.Request) {
		n.abandonConfirm = r.URL.Query().Get("confirm")
		reply(w, n.withLeft(map[string]any{"operation": n.gesture}))
	})
	mux.HandleFunc("POST /worklet/retention/maintenance/exclude", func(w http.ResponseWriter, r *http.Request) {
		n.excluded = r.URL.Query().Get("node")
		reply(w, map[string]any{"operation": n.gesture})
	})
	mux.HandleFunc("GET /worklet/retention/reanchor", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"stream": r.URL.Query().Get("stream"), "generation": 0,
			"created_at": "2031-04-02T03:00:00Z",
		}
		switch n.reanchorCase {
		case "-":
			body["nothing_to_reanchor"] = "it still holds every record the rows are missing"
		case "":
			body["case"], body["cursor"] = "recreated", n.reanchorCursor
		default:
			body["case"], body["cursor"] = n.reanchorCase, n.reanchorCursor
		}
		if n.reanchorDiscards {
			body["discards"] = discardedRecord()
			body["discarding"] = "statelog: reanchor refused: it holds records this " +
				"node's rows do not; re-run with the discard flag"
		}
		reply(w, body)
	})
	mux.HandleFunc("POST /worklet/retention/reanchor", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(n.reanchorTakes)
		n.reanchorConfirm = r.URL.Query().Get("confirm")
		n.reanchorDiscard = r.URL.Query().Get("discard") == "true"
		which := n.reanchorCase
		if which == "" {
			which = "recreated"
		}
		answer := map[string]any{
			"stream": r.URL.Query().Get("stream"), "generation": 1,
			"case": which, "cursor": n.reanchorCursor,
		}
		if n.reanchorDiscard {
			answer["discarded"] = discardedRecord()
		}
		reply(w, answer)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

func (n *fakeCapacityNode) op() map[string]any {
	if n.operation != nil {
		return n.operation
	}
	return map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "operation_id": "op-1",
		"target_max_bytes": 8589934592, "original_max_bytes": 4294967296,
		"phase": "applied", "attempt": 1,
		"participants":       []string{"node-1", "node-2"},
		"write_incarnations": map[string]string{"node-1": "node-1:old", "node-2": "node-2:old"},
		"entered_at":         "2031-04-02T03:14:00Z", "by": "ops-3",
		"journal": []map[string]any{{
			"attempt": 1, "by": "node-1:a", "state": "issued",
			"at": "2031-04-02T03:14:01Z",
		}},
	}
}

// withLeft is body with the windows this node names as still open.
func (n *fakeCapacityNode) withLeft(body map[string]any) map[string]any {
	for k, v := range n.left {
		body[k] = v
	}
	return body
}

func reply(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// TestSetCapacityRepeatsTheCeilingAndSaysWhatIsNext.
//
// The verb restarts the whole fleet three times and changes what the broker
// will accept, so the byte count is not a value to get from a shell history —
// and an operator who has just run it needs the next gesture, not a summary.
func TestSetCapacityRepeatsTheCeilingAndSaysWhatIsNext(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	base := bootstrapForURL(t, node.server.URL)

	if _, _, err := cli(t, "retention", "set-capacity",
		"CREWLET_TRACKER_LOG", "8589934592", base); err == nil {
		t.Error("a capacity change with no confirmation was accepted")
	}
	if _, _, err := cli(t, "retention", "set-capacity",
		"CREWLET_TRACKER_LOG", "8589934592", base, "-confirm", "8589934593"); err == nil {
		t.Error("a capacity change confirming a different number was accepted")
	}
	if node.stream != "" {
		t.Fatalf("the node was contacted anyway, for %q", node.stream)
	}

	stdout, _, err := cli(t, "retention", "set-capacity",
		"CREWLET_TRACKER_LOG", "8589934592", base, "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if node.stream != "CREWLET_TRACKER_LOG" || node.bytes != "8589934592" ||
		node.confirm != "8589934592" {
		t.Fatalf("the node was asked for %q %q (confirm %q)",
			node.stream, node.bytes, node.confirm)
	}
	// THE PHASE IS `applied`, WHICH IS NOT DONE — another request may
	// still be in flight, and an operator who stopped here would return a
	// fleet to service with an unresolved configuration write.
	for _, want := range []string{"applied", "NOT yet sealed", "-mode seal"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never mentions %q:\n%s", want, stdout)
		}
	}
}

// A WINDOW AT `opened` IS SENT TO MAINTENANCE MODE, NOT TO THE SEAL.
//
// Nothing has been sent from `opened`, so its next step is the baseline, which
// only maintenance mode takes. The report sent it to `-mode seal` as it does
// `baselined`, where a restart changes nothing: the operator paid a fleet-wide
// restart to come back to the same phase. An admission still held leaves a
// window there, so this is the report a crashed node's fleet reads.
// Mutation: fold "opened" back into "baselined" and this goes red.
func TestAWindowAtOpenedIsSentToMaintenanceMode(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "opened"
	op["write_incarnations"] = nil
	op["journal"] = nil
	node.operation = op
	stdout, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", bootstrapForURL(t, node.server.URL), "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if !strings.Contains(stdout, "-mode maintenance") || strings.Contains(stdout, "-mode seal") {
		t.Errorf("a window that has sent nothing is not sent back to maintenance "+
			"mode:\n%s", stdout)
	}
	// THE VERB IS NAMED IN FULL, so a reader of any page that prints this
	// knows which command runs next: "this verb" read as the page's own.
	for _, want := range []string{
		"Nothing has been sent yet.",
		"run `crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 " +
			"-confirm 8589934592` again",
		"`crewlet retention maintenance status -stream CREWLET_TRACKER_LOG` " +
			"names anything that stops it",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never says %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "this verb") {
		t.Errorf("the report says \"this verb\" rather than naming it:\n%s", stdout)
	}
}

// A SEALING WINDOW POINTS AT WHAT THE SEAL STILL WAITS ON, which is not only
// who hasn't acknowledged: a node that acknowledged from seal mode while still
// the process recorded as the request left is waited on too, and the status
// page names it. Mutation: say "who hasn't acknowledged" again and this fails.
func TestASealingWindowPointsAtWhatTheSealWaitsOn(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "sealing"
	node.operation = op
	stdout, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", bootstrapForURL(t, node.server.URL), "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if want := "`crewlet retention maintenance status -stream CREWLET_TRACKER_LOG` " +
		"names who it's still waiting on."; !strings.Contains(stdout, want) {
		t.Errorf("a sealing window never says %q:\n%s", want, stdout)
	}
}

// A LATER ATTEMPT AT OPENED SAYS WHAT THE LAST ONE LEFT.
//
// A verify that reads a ceiling other than the target starts a new attempt
// back at opened, after the last attempt's request was sent and applied. "Nothing
// has been sent yet" is true of the new attempt only, and the reader has just
// watched a request change the ceiling. Mutation: print the first attempt's
// sentence for every attempt and this fails.
func TestALaterAttemptAtOpenedSaysWhatTheLastOneLeft(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "opened"
	op["attempt"] = 2
	op["observed_max_bytes"] = 6442450944
	op["write_incarnations"] = nil
	op["journal"] = nil
	node.operation = op
	stdout, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", bootstrapForURL(t, node.server.URL), "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if strings.Contains(stdout, "Nothing has been sent yet") {
		t.Errorf("a second attempt says nothing has been sent:\n%s", stdout)
	}
	for _, want := range []string{"This attempt hasn't sent anything yet",
		"left the ceiling at 6.0 GiB, not the target"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never says %q:\n%s", want, stdout)
		}
	}
}

// A WINDOW SET-CAPACITY CLOSED SAYS WHAT RETURNS THE FLEET TO SERVICE, and one
// still open at `confirmed` names the verb that closes it.
//
// The verb's last answer printed the table and nothing after it, so the one
// gesture left, restarting every node in service, was in no answer the
// operator was given. A confirmed window the status page or an exclusion shows
// has not been closed, so there the verb is the next step. Mutation: drop the
// confirmed case and the first half fails; print the closed sentence on every
// page and the second does.
func TestAConfirmedWindowSaysWhatComesNext(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "confirmed"
	op["observed_max_bytes"] = 8589934592
	node.operation, node.gesture = op, op
	base := bootstrapForURL(t, node.server.URL)

	closed, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", base, "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if want := "the window is closed. Restart every node with no -mode to " +
		"return the fleet to service."; !strings.Contains(closed, want) {
		t.Errorf("the closed window never says %q:\n%s", want, closed)
	}

	open, _, err := cli(t, "retention", "maintenance", "exclude", base,
		"-stream", "CREWLET_TRACKER_LOG", "-node", "node-a", "-confirm", "node-a")
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if strings.Contains(open, "window is closed") {
		t.Errorf("a window an exclusion left open is said to be closed:\n%s", open)
	}
	if want := "still open. Run `crewlet retention set-capacity " +
		"CREWLET_TRACKER_LOG 8589934592 -confirm 8589934592` again to close " +
		"it."; !strings.Contains(open, want) {
		t.Errorf("a confirmed window still open never says %q:\n%s", want, open)
	}
}

// A CLOSED WINDOW SENDS THE FLEET TO SERVICE ONLY ONCE NOTHING ELSE HOLDS IT.
//
// A node won't start in service while any log has a window open, or while it
// can't read whether one has, and a fleet resizing two logs closes one window
// before the other. The closing line said to restart in service whatever was
// still open, and every node refused the restart. Now it names every window
// still open with the command that finishes it, and every log the node
// couldn't read, for set-capacity's close and an abandonment's alike.
// Mutation: say the return to service whatever the answer names and this
// fails.
func TestAClosedWindowWaitsOnEveryOtherBeforeService(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "confirmed"
	op["observed_max_bytes"] = 8589934592
	node.operation = op
	node.left = map[string]any{
		"open_windows": []map[string]any{{
			"stream": "CREWLET_PAGES_LOG", "operation_id": "op-2",
			"phase": "applied", "target_max_bytes": 8589934592,
		}},
		"unread_windows": []string{"CREWLET_VECTORS_LOG"},
	}
	base := bootstrapForURL(t, node.server.URL)
	closed, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", base, "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	cleared, _, err := cli(t, "retention", "maintenance", "abandon", base,
		"-stream", "CREWLET_TRACKER_LOG", "-confirm", "op-1")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	for page, stdout := range map[string]string{"set-capacity": closed, "abandon": cleared} {
		if strings.Contains(stdout, "return the fleet to service") {
			t.Errorf("%s sends the fleet to service with a window still open:\n%s",
				page, stdout)
		}
		for _, want := range []string{
			"Don't restart in service yet",
			"CREWLET_PAGES_LOG's window is still open, at applied. Finish it with " +
				"`crewlet retention set-capacity CREWLET_PAGES_LOG 8589934592 " +
				"-confirm 8589934592`.",
			"couldn't read whether CREWLET_VECTORS_LOG has a window open. Check " +
				"with `crewlet retention maintenance status -stream " +
				"CREWLET_VECTORS_LOG`.",
			"Once every window is closed, restart every node with no -mode.",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%s never says %q:\n%s", page, want, stdout)
			}
		}
	}
}

// AN ABANDONED WINDOW IS CLOSED AT THE CEILING ITS SEAL FOUND, and never
// said to be verified at the target it no longer tries to reach. Mutation:
// print the verified sentence for an abandoned window and this fails.
func TestAnAbandonedWindowSaysTheCeilingItClosedAt(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "confirmed"
	op["blocked"] = coord.BlockAbandoned
	op["observed_max_bytes"] = 4294967296
	node.operation, node.gesture = op, op
	base := bootstrapForURL(t, node.server.URL)
	closed, _, err := cli(t, "retention", "set-capacity", "CREWLET_TRACKER_LOG",
		"8589934592", base, "-confirm", "8589934592")
	if err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	open, _, err := cli(t, "retention", "maintenance", "exclude", base,
		"-stream", "CREWLET_TRACKER_LOG", "-node", "node-a", "-confirm", "node-a")
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	for page, want := range map[string]string{
		closed: "The window was abandoned and is closed, with the ceiling at 4.0 GiB.",
		open: "The window was abandoned, and its seal found the ceiling at 4.0 GiB. " +
			"Run `crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 " +
			"-confirm 8589934592` again to close it there.",
	} {
		if !strings.Contains(page, want) || strings.Contains(page, "verified at the target") {
			t.Errorf("an abandoned window never says %q, or says it was verified:\n%s",
				want, page)
		}
	}
}

// A BLOCKED WINDOW SAYS WHY IN THE NODE'S WORDS, NEVER ITS CODE, ON A LINE OF
// ITS OWN, AND NOTHING BESIDE IT CONTRADICTS IT.
//
// The BLOCKED line printed `capacity_operation_unsealed`, a code that told a
// person nothing they could act on. The node carries a sentence beside every
// code, and the page prints that. Put in a table cell, the sentence set the
// width of its column for every row, so `on <log>`, `attempt N` and `by` sat
// some 280 characters to the right; and the phase's own next step ("Run
// set-capacity again to verify and confirm") contradicted it on a window out of
// attempts, where set-capacity is blocked again.
//
// Mutation: print the code, put the sentence back in the table, or print the
// phase's line beside it, and this fails.
func TestABlockedWindowSaysWhyInWordsNotItsCode(t *testing.T) {
	t.Parallel()
	detail := "Crewlet stopped after 3 attempts at 8.0 GiB, because the broker " +
		"still reports 4.0 GiB, and it won't send the change again."
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "sealed"
	op["blocked"] = coord.BlockUnsealed
	op["blocked_detail"] = detail
	node.operation = op
	page, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if strings.Contains(page, string(coord.BlockUnsealed)) {
		t.Errorf("the page prints the block's code:\n%s", page)
	}
	if want := "\nThis window needs a person. " + detail + "\n"; !strings.Contains(page, want) {
		t.Errorf("the page never says %q on a line of its own:\n%s", want, page)
	}
	if strings.Contains(page, "Run `crewlet retention set-capacity") {
		t.Errorf("the page offers set-capacity beside a block that refuses it:\n%s", page)
	}
	for line := range strings.SplitSeq(page, "\n") {
		if strings.HasPrefix(line, "OPERATION") && len(line) > 60 {
			t.Errorf("the table's OPERATION row is %d characters wide: %q", len(line), line)
		}
	}
}

// AN EXCLUSION SENDS ITS READER TO SET-CAPACITY, AND SAYS WHAT IT WITHDREW.
//
// The lines after an exclusion are the ones every gesture prints, which told
// the reader to run "this verb" again: the exclusion, which only re-records
// itself. Its own line never said that the admission it was run for stops
// counting. Mutation: drop the verb's name from the opened line, or the
// admission from the exclusion's line, and this fails.
func TestAnExclusionSendsItsReaderToSetCapacity(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "opened"
	op["write_incarnations"] = nil
	op["journal"] = nil
	node.gesture = op
	stdout, _, err := cli(t, "retention", "maintenance", "exclude",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG",
		"-node", "node-a", "-confirm", "node-a")
	if err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if node.excluded != "node-a" {
		t.Fatalf("the node was asked to exclude %q", node.excluded)
	}
	for _, want := range []string{
		"node-a is excluded: it no longer has to acknowledge, and an admission " +
			"it holds no longer stops the window.",
		"run `crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 " +
			"-confirm 8589934592` again",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the exclusion never says %q:\n%s", want, stdout)
		}
	}
}

// AN ABANDONMENT SENDS ITS CONFIRMATION, AND THE NODE JUDGES IT.
//
// The CLI used to abandon with no confirmation and compare -confirm with the
// operation that came back, so a wrong one was reported after the window had
// already been abandoned, and a window cleared outright was never compared.
// Mutation: drop the confirmation from the request and this fails.
func TestAnAbandonmentSendsItsConfirmation(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	stdout, _, err := cli(t, "retention", "maintenance", "abandon",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG",
		"-confirm", "op-1")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if node.abandonConfirm != "op-1" {
		t.Errorf("the node was asked to abandon confirming %q, want op-1", node.abandonConfirm)
	}
	if want := "The window on CREWLET_TRACKER_LOG is closed, and nothing was " +
		"sent. Restart every node with no -mode"; !strings.Contains(stdout, want) {
		t.Errorf("a window cleared outright never says %q:\n%s", want, stdout)
	}
}

// THE STATUS NAMES BOTH WAYS PAST AN ADMISSION, the exclusion in full.
//
// It named `crewlet retention maintenance exclude` with none of the flags that
// command refuses to run without, and a dead node's admission is the one no
// restart can clear: the exclusion is the only way past it.
//
// AT OPENED IT NAMES WHAT THE BASELINE WAITS ON, never the seal's list: the
// node answers which participants have no restart recorded and which
// admissions hold the window, and the page prints those, each for one node in
// the singular, and names the verb to run rather than sending its reader to the
// page they are reading. Mutation: print "Not sealed" at opened, drop either
// line, or keep the status clause on the status page, and this fails.
func TestTheStatusNamesTheWayPastAnAdmission(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "opened"
	op["write_incarnations"] = nil
	op["journal"] = nil
	node.status = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "open": true, "mode": "maintenance",
		"operation":           op,
		"restarts_missing":    []string{"node-2"},
		"admissions_blocking": []string{"node-3"},
	}
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for _, want := range []string{
		`No restart into a maintenance mode is recorded for "node-2"`,
		"`crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG " +
			"-node node-2 -confirm node-2`",
		`"node-3" holds an admission, so nothing is sent while it stands`,
		"`crewlet retention maintenance exclude -stream CREWLET_TRACKER_LOG " +
			"-node node-3 -confirm node-3`",
		"-mode maintenance",
		"run `crewlet retention set-capacity CREWLET_TRACKER_LOG 8589934592 " +
			"-confirm 8589934592` again.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the status never says %q:\n%s", want, stdout)
		}
	}
	for _, unwanted := range []string{"Not sealed", "These nodes",
		"`crewlet retention maintenance status"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("the status at opened says %q:\n%s", unwanted, stdout)
		}
	}
}

// A FLEET ON ITS WAY INTO A WINDOW IS TOLD WHO STILL HOLDS AN ADMISSION, before
// it opens one.
//
// Every boot withdraws its node's last process's admission, so a fleet in
// maintenance mode holds none but a node still in service or one that crashed
// and has not booted since. Called "normal for a fleet in service" on a node
// that is not in service, they read as nothing to act on.
func TestNoWindowInMaintenanceModeNamesTheAdmissionsHeld(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	node.status = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "open": false, "mode": "maintenance",
		"admissions": []map[string]any{{"node_id": "node-4", "incarnation": "x"}},
	}
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if !strings.Contains(stdout, `"node-4" holds an admission: it's still running`) ||
		strings.Contains(stdout, "normal for a fleet") {
		t.Errorf("a fleet in maintenance mode is not told which node still holds "+
			"an admission:\n%s", stdout)
	}

	// AND TWO NODES ARE SAID IN THE PLURAL.
	node.status["admissions"] = []map[string]any{
		{"node_id": "node-4", "incarnation": "x"}, {"node_id": "node-5", "incarnation": "y"}}
	stdout, _, err = cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if !strings.Contains(stdout, `"node-4" and "node-5" hold an admission: each`) {
		t.Errorf("two nodes holding an admission are not said in the plural:\n%s", stdout)
	}
}

// THE MAINTENANCE SYNOPSIS SAYS WHAT AN EXCLUSION WITHDRAWS: the one gesture
// that frees a window from a node that will never boot again.
func TestTheMaintenanceSynopsisSaysWhatAnExclusionWithdraws(t *testing.T) {
	t.Parallel()
	_, stderr, err := cli(t, "retention", "maintenance")
	if err == nil {
		t.Fatal("a bare `retention maintenance` succeeded")
	}
	for _, want := range []string{"-node <node-id> -confirm <node-id>", "withdraws its admission",
		"stopped and holds no\n           outstanding request"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the synopsis never says %q:\n%s", want, stderr)
		}
	}
}

// TestTheExternalAssertionIsExplicit: on a broker the engine does not run,
// stopping every Crewlet node establishes nothing, and the operator says so in
// their own words rather than a check quietly proving nothing.
func TestTheExternalAssertionIsExplicit(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	base := bootstrapForURL(t, node.server.URL)
	if _, _, err := cli(t, "retention", "set-capacity",
		"CREWLET_TRACKER_LOG", "8589934592", base, "-confirm", "8589934592"); err != nil {
		t.Fatalf("set-capacity: %v", err)
	}
	if node.assert {
		t.Fatal("the assertion was sent without the operator making it")
	}
	if _, _, err := cli(t, "retention", "set-capacity",
		"CREWLET_TRACKER_LOG", "8589934592", base, "-confirm", "8589934592",
		"-i-have-excluded-all-publishers"); err != nil {
		t.Fatalf("set-capacity with the assertion: %v", err)
	}
	if !node.assert {
		t.Fatal("the operator's assertion did not reach the node")
	}
}

// TestMaintenanceStatusNamesWhatIsHoldingTheSeal is the one page an operator
// can see why a fleet is still excluded: who has not acknowledged, whose
// incarnation is unchanged, and which request is unresolved and when it was
// sent.
func TestMaintenanceStatusNamesWhatIsHoldingTheSeal(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for _, want := range []string{
		"op-1",                        // the operation
		"attempt 1",                   // and its attempt
		"node-1:old",                  // what each participant ran as the request left
		"node-1:new",                  // and what it acknowledged as
		"node-2",                      // the one that has not
		"node-2 has not acknowledged", // said in words
		"issued",                      // the unresolved request
		"2031-04-02T03:14:01Z",        // and when it was sent
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the status never mentions %q:\n%s", want, stdout)
		}
	}
}

// A RESTART THAT BOUND NO WINDOW IS PRINTED AS ONE, not as attempt 0.
//
// A node restarted into maintenance mode while no window was open records its
// restart bound to none, and a node acknowledged an earlier window carries
// that window's attempt. Neither is an acknowledgement of this window, and
// printed as numbers they read as one. Mutation: print the attempt as a
// number again and this goes red.
func TestTheStatusPrintsARestartBeforeTheWindowAsOne(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	node.status = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "open": true, "mode": "maintenance",
		"sealed": false, "operation": node.op(),
		"acks": []map[string]any{
			{"node_id": "node-1", "operation_id": "", "attempt": 0,
				"incarnation": "node-1:early", "mode": "maintenance"},
			{"node_id": "node-2", "operation_id": "op-0", "attempt": 3,
				"incarnation": "node-2:earlier", "mode": "seal"},
		},
	}
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for incarnation, want := range map[string]string{
		"node-1:early": "no window", "node-2:earlier": "another window",
	} {
		printed := false
		for _, line := range strings.Split(stdout, "\n") {
			printed = printed || strings.Contains(line, incarnation) && strings.Contains(line, want)
		}
		if !printed {
			t.Errorf("the row acknowledged as %s never says %q:\n%s", incarnation, want, stdout)
		}
	}
}

// THE STATUS NAMES WHO EXCLUDED A NODE AND WHO ABANDONED THE WINDOW.
//
// An exclusion is the one fact the seal takes on somebody's word, and it
// printed as "excluded by an operator" whoever it was; an abandonment printed
// nothing at all, while the window named whoever OPENED it. A window nobody
// abandoned prints no abandonment. Mutation: print the participant line without
// its party and the first want goes red.
func TestMaintenanceStatusNamesWhoExcludedAndWhoAbandoned(t *testing.T) {
	node := newFakeCapacityNode(t)
	op := node.op()
	op["phase"] = "sealing"
	op["excluded"] = []string{"node-2"}
	op["excluded_by"] = map[string]any{"node-2": map[string]any{
		"by": "dana.sre", "operator_id": "session:0192f00d-0000-7000-8000-0000000000dd"}}
	op["abandoned_by"] = map[string]any{"by": "ana.admin",
		"operator_id": "pat:0192f00d-0000-7000-8000-00000000000a"}
	node.operation = op
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for _, want := range []string{
		"excluded by dana.sre (through session:0192f00d-0000-7000-8000-0000000000dd)",
		"by ana.admin (through pat:0192f00d-0000-7000-8000-00000000000a)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the status never says %q:\n%s", want, stdout)
		}
	}

	// AND A WINDOW NOBODY ABANDONED SAYS NOTHING ABOUT AN ABANDONMENT.
	delete(op, "abandoned_by")
	stdout, _, err = cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if strings.Contains(stdout, "ABANDONED") {
		t.Errorf("a window nobody abandoned printed an abandonment:\n%s", stdout)
	}
}

// TestMaintenanceStatusOnAFleetWithNoOperationSaysSo: "no window" and "a
// window in phase opened" are different facts with different next steps.
func TestMaintenanceStatusOnAFleetWithNoOperationSaysSo(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	node.status = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "open": false, "mode": "normal",
		"admissions": []map[string]any{{"node_id": "node-1", "incarnation": "x"}},
	}
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if !strings.Contains(stdout, "No capacity window is open") {
		t.Fatalf("a fleet with no window does not say so:\n%s", stdout)
	}
	if !strings.Contains(stdout, "normal for a fleet in service") {
		t.Errorf("the admissions are reported without saying they are expected "+
			"on a fleet that is running:\n%s", stdout)
	}
}

// A WINDOW THE NODE COULDN'T READ IS THE NODE'S ANSWER ABOUT THE WINDOW, not a
// node that can't serve the page: framed as the second, "this node cannot serve
// that" sent an operator to find another node during an outage every node
// shares, and the sentence beside it said to ask again. Mutation: drop the
// framing and the first want goes red.
func TestMaintenanceStatusSaysItCouldNotReadTheWindow(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	node.statusUnread = true
	_, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err == nil {
		t.Fatal("a window the node couldn't read exited zero")
	}
	if strings.Contains(err.Error(), "cannot serve") {
		t.Errorf("the status read is framed as a node that can't serve it: %v", err)
	}
	if want := "Couldn't read whether a capacity window is open on " +
		"CREWLET_TRACKER_LOG"; !strings.Contains(err.Error(), want) {
		t.Errorf("the status read never says %q: %v", want, err)
	}
}

// WHAT THE STATUS COULDN'T READ BESIDE THE WINDOW IS SAID AS THAT. The node
// reads the window and then its acknowledgements and the admissions, and one of
// those failing is answered beside the window rather than as the window's own
// 503. Printed as empty, an unread acknowledgement register read as no node
// having restarted, and unread admissions as none holding the window.
//
// Mutation: print a dash for an acknowledgement nobody could read, or drop
// either line, and this fails.
func TestMaintenanceStatusSaysWhatItCouldNotReadBesideTheWindow(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	opened := node.op()
	opened["phase"] = "opened"
	node.status = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "open": true, "mode": "maintenance",
		"operation": opened, "acks_unread": true, "admissions_unread": true,
	}
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for _, want := range []string{"node-1:old          couldn't read",
		"This node couldn't read which nodes have restarted for the window just now",
		"This node couldn't read the admissions just now"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the page never says %q:\n%s", want, stdout)
		}
	}

	// WITH NO WINDOW, unread admissions are said too, never as none held.
	node.status = map[string]any{"stream": "CREWLET_TRACKER_LOG", "open": false,
		"mode": "maintenance", "admissions_unread": true}
	stdout, _, err = cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	if want := "It couldn't read the admissions just now"; !strings.Contains(stdout, want) {
		t.Errorf("the page never says %q:\n%s", want, stdout)
	}
}

// TestAReanchorPrintsTheValueItWillConfirmAgainst.
//
// The confirmation means "I looked at the thing I am re-anchoring", so the
// verb prints it and refuses, rather than reading it and feeding it straight
// back — which would be confirming against its own output.
func TestAReanchorPrintsTheValueItWillConfirmAgainst(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	base := bootstrapForURL(t, node.server.URL)

	stdout, _, err := cli(t, "retention", "reanchor", base,
		"-stream", "CREWLET_TRACKER_LOG")
	if err == nil {
		t.Fatal("a reanchor with no confirmation was accepted")
	}
	if !strings.Contains(stdout, "2031-04-02T03:00:00Z") {
		t.Fatalf("the verb did not print the value to confirm against:\n%s", stdout)
	}
	if !strings.Contains(stdout, "does NOT recover records") {
		t.Errorf("the verb does not say what a reanchor loses:\n%s", stdout)
	}
	for _, want := range []string{"RECREATED", "first surviving record"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the recreated prompt never says %q:\n%s", want, stdout)
		}
	}
	if node.reanchorConfirm != "" {
		t.Fatalf("the node was asked to re-anchor anyway, confirming %q",
			node.reanchorConfirm)
	}

	stdout, _, err = cli(t, "retention", "reanchor", base,
		"-stream", "CREWLET_TRACKER_LOG", "-confirm", "2031-04-02T03:00:00Z")
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	if node.reanchorConfirm != "2031-04-02T03:00:00Z" {
		t.Fatalf("the node was asked to re-anchor confirming %q", node.reanchorConfirm)
	}
	for _, want := range []string{"generation 1", "(recreated)", "first surviving record"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never says %q:\n%s", want, stdout)
		}
	}
}

// TestAReanchorNamesTheCaseTheOperatorConfirms: a restored log is followed from
// its END and a recreated one from its first record, which is the one fact
// about the transition the operator has to agree with — so the prompt says
// which, before the confirmation, and the report says which again after. And a
// log with nothing to re-anchor offers no command to run.
func TestAReanchorNamesTheCaseTheOperatorConfirms(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	base := bootstrapForURL(t, node.server.URL)
	node.reanchorCase, node.reanchorCursor = "restored", 7000

	stdout, _, err := cli(t, "retention", "reanchor", base, "-stream", "CREWLET_TRACKER_LOG")
	if err == nil {
		t.Fatal("a reanchor with no confirmation was accepted")
	}
	for _, want := range []string{"RESTORED", "7000", "END", "none of them is replayed",
		"-confirm 2031-04-02T03:00:00Z"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the restored prompt never says %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = cli(t, "retention", "reanchor", base,
		"-stream", "CREWLET_TRACKER_LOG", "-confirm", "2031-04-02T03:00:00Z")
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	for _, want := range []string{"(restored)", "from its end, after sequence 7000"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the restored report never says %q:\n%s", want, stdout)
		}
	}

	node.reanchorCase, node.reanchorConfirm = "-", ""
	stdout, _, err = cli(t, "retention", "reanchor", base, "-stream", "CREWLET_TRACKER_LOG")
	if err == nil || !strings.Contains(stdout, "nothing to re-anchor") {
		t.Fatalf("a log with nothing to re-anchor = %v:\n%s", err, stdout)
	}
	if strings.Contains(stdout, "-confirm") {
		t.Errorf("a log with nothing to re-anchor was offered a command to run:\n%s", stdout)
	}
}

// discardedRecord is the record the fake node names as written after a restore.
func discardedRecord() map[string]any {
	return map[string]any{
		"seq": 7100, "kind": "task", "subject": "task.t-1",
		"writer": "node-b", "op_id": "op-b-1", "stored_at": "2031-04-02T04:00:00Z",
	}
}

// TestAReanchorThatWouldDiscardSaysSoAndOffersTheFlag: a restored log holding
// records written after the restore that this node's rows do not hold is one a
// reanchor would apply nowhere, so the prompt says so — naming the node's own
// refusal — and the command it offers carries -discard, which the transition
// then passes on and whose answer names what it discarded.
func TestAReanchorThatWouldDiscardSaysSoAndOffersTheFlag(t *testing.T) {
	t.Parallel()
	node := newFakeCapacityNode(t)
	base := bootstrapForURL(t, node.server.URL)
	node.reanchorCase, node.reanchorCursor, node.reanchorDiscards = "restored", 7100, true

	stdout, _, err := cli(t, "retention", "reanchor", base, "-stream", "CREWLET_TRACKER_LOG")
	if err == nil {
		t.Fatal("a reanchor with no confirmation was accepted")
	}
	for _, want := range []string{"discard flag", "-confirm 2031-04-02T03:00:00Z -discard"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the prompt never says %q:\n%s", want, stdout)
		}
	}

	_, _, err = cli(t, "retention", "reanchor", base,
		"-stream", "CREWLET_TRACKER_LOG", "-confirm", "2031-04-02T03:00:00Z")
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	if node.reanchorDiscard {
		t.Fatal("the node was asked to discard although the flag was not given")
	}

	stdout, _, err = cli(t, "retention", "reanchor", base,
		"-stream", "CREWLET_TRACKER_LOG", "-confirm", "2031-04-02T03:00:00Z", "-discard")
	if err != nil {
		t.Fatalf("reanchor -discard: %v", err)
	}
	if !node.reanchorDiscard {
		t.Fatal("-discard never reached the node")
	}
	for _, want := range []string{"applied on no node", "sequence 7100", "node-b", "op-b-1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never says %q:\n%s", want, stdout)
		}
	}
}

// TestVerifyRestoreExitsNonZeroPastItsCadence is what turns a lapsed restore
// test into a failing check somebody's cron notices, rather than a paragraph
// in a runbook nobody read.
func TestVerifyRestoreExitsNonZeroPastItsCadence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	taken := time.Date(2031, 3, 1, 3, 0, 0, 0, time.UTC)
	writeArtefact(t, filepath.Join(root, "nightly"), taken)

	// INSIDE THE CADENCE.
	var out strings.Builder
	if err := verifyRestore(root, 30*24*time.Hour, taken.Add(72*time.Hour), &out); err != nil {
		t.Fatalf("a three-day-old artefact was refused: %v", err)
	}
	// THE STORE COPY BY ITS ESTATE, as `crewlet backup` names it.
	for _, want := range []string{"CREWLET_TRACKER_LOG", "generation 2", "sequence 918280001",
		"STORE replicated"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report never mentions %q:\n%s", want, out.String())
		}
	}

	// PAST IT.
	out.Reset()
	err := verifyRestore(root, 30*24*time.Hour, taken.Add(40*24*time.Hour), &out)
	if err == nil {
		t.Fatal("a forty-day-old artefact passed a thirty-day cadence — a lapsed " +
			"restore test that exits zero is a paragraph in a runbook")
	}
	if !strings.Contains(err.Error(), "replay window") {
		t.Errorf("the refusal does not say why the cadence matters: %v", err)
	}
}

// TestAnArtefactWithNoDomainPositionCannotBeVerified: a restore replays from
// that sequence, so an artefact without one is not restorable whatever else it
// contains.
func TestAnArtefactWithNoDomainPositionCannotBeVerified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "nightly")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"taken_at": time.Now().UTC(), "finished_at": time.Now().UTC(),
		"node_id": "node-0", "engine_version": "dev",
	})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := verifyRestore(root, time.Hour, time.Now().UTC(), &out); err == nil {
		t.Fatal("an artefact naming no domain position was verified — a restore " +
			"has no sequence to replay from")
	}
}

// TestADirectoryWithNoManifestIsDebrisRatherThanABackup.
func TestADirectoryWithNoManifestIsDebrisRatherThanABackup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "half-finished"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := verifyRestore(root, time.Hour, time.Now().UTC(), &out)
	if err == nil {
		t.Fatal("a directory with no manifest was accepted as a backup")
	}
	if !strings.Contains(err.Error(), "debris") {
		t.Errorf("the refusal does not say what such a directory is: %v", err)
	}
}

// writeArtefact lays down one finished backup.
func writeArtefact(t *testing.T, dir string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"taken_at": at, "finished_at": at.Add(2 * time.Minute),
		"node_id": "node-0", "engine_version": "dev",
		"stores": []map[string]any{{
			"estate": "replicated", "bytes": 10415140864,
			"migrations": []string{"0001_the_state_log_lands.sql"},
		}},
		"domains": map[string]any{
			"CREWLET_TRACKER_LOG": map[string]any{
				"stream": "CREWLET_TRACKER_LOG", "generation": 2, "seq": 918280001,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A REANCHOR IS WAITED FOR PAST THE ORDINARY TEN SECONDS.
//
// Once its generation record is on the log a reanchor rebuilds the node's
// consumer — a delete and a create of a replicated object — and on a fleet
// whose metadata group is slow that outlasts the ten seconds every other call
// gets. The CLI gave up there and reported a failure for a transition the node
// went on to finish; the node had also been running it on the request's own
// context, so the cancellation stopped it halfway.
//
// Against a client whose ORDINARY timeout is two seconds and a node that
// answers the transition after four: the status read before it is answered
// at once, so only a transition sent through [nodeClient.patiently] can be
// waited for. Two seconds rather than half of one, because "at once" is a
// loopback round trip under the race detector on a runner the rest of the
// suite shares, and at half a second a starved machine timed the status read
// out and failed the case on the call it does not test. The command's own wiring — the
// flags, the client nodeClientFor builds at nodeRequestTimeout — is the other
// reanchor cases' and [TestAnOperatorCallIsGivenTenSeconds]'s; this one would
// otherwise sit out the real ten seconds to prove a relation.
func TestAReanchorIsWaitedForPastTheOrdinaryTimeout(t *testing.T) {
	t.Parallel()
	const ordinary = 2 * time.Second
	node := newFakeCapacityNode(t)
	node.reanchorCase, node.reanchorCursor = "restored", 7000
	node.reanchorTakes = 2 * ordinary
	client := &nodeClient{base: node.server.URL, token: "t", http: httpx.Client(ordinary)}

	var stdout bytes.Buffer
	err := reanchor(t.Context(), client, reanchorAsk{
		stream: "CREWLET_TRACKER_LOG", confirm: "2031-04-02T03:00:00Z",
	}, &stdout)
	if err != nil {
		t.Fatalf("a reanchor the node answered after %s, past the client's own %s, was "+
			"reported as %v — the CLI gave up on a transition the node finished",
			node.reanchorTakes, ordinary, err)
	}
	if !strings.Contains(stdout.String(), "is re-anchored at generation") {
		t.Fatalf("the report does not say the log was re-anchored:\n%s", stdout.String())
	}
}

// AN OPERATOR CALL IS GIVEN TEN SECONDS, and a reanchor longer: the client
// every node-facing command builds waits nodeRequestTimeout, the figure that
// constant argues for, and the transition outlasts it. The case above proves
// the reanchor waits past whatever ordinary timeout its client has; this is
// what ties that to the ten seconds a real command gets.
func TestAnOperatorCallIsGivenTenSeconds(t *testing.T) {
	t.Parallel()
	if nodeRequestTimeout != 10*time.Second {
		t.Errorf("nodeRequestTimeout = %v, want 10s", nodeRequestTimeout)
	}
	if reanchorRequestTimeout <= nodeRequestTimeout {
		t.Errorf("reanchorRequestTimeout = %v, which does not outlast the ordinary %v",
			reanchorRequestTimeout, nodeRequestTimeout)
	}
	client, err := nodeClientFor(noEnviron, []string{"-api", "http://127.0.0.1:1"},
		"retention reanchor", io.Discard, nil)
	if err != nil {
		t.Fatalf("nodeClientFor: %v", err)
	}
	if client.http.Timeout != nodeRequestTimeout {
		t.Errorf("a node-facing command waits %v, want nodeRequestTimeout (%v)",
			client.http.Timeout, nodeRequestTimeout)
	}
}
