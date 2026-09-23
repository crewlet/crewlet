package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
}

func newFakeCapacityNode(t *testing.T) *fakeCapacityNode {
	t.Helper()
	n := &fakeCapacityNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /work/retention/capacity", func(w http.ResponseWriter, r *http.Request) {
		n.stream = r.URL.Query().Get("stream")
		n.bytes = r.URL.Query().Get("bytes")
		n.confirm = r.URL.Query().Get("confirm")
		n.assert = r.URL.Query().Get("assert_excluded") == "true"
		reply(w, map[string]any{"mode": "maintenance", "operation": n.op()})
	})
	mux.HandleFunc("GET /work/retention/maintenance", func(w http.ResponseWriter, r *http.Request) {
		n.stream = r.URL.Query().Get("stream")
		body := n.status
		if body == nil {
			body = map[string]any{
				"stream": n.stream, "open": true, "mode": "seal",
				"sealed":              false,
				"blocking":            "node-2 has not acknowledged",
				"admissions_blocking": []string{"node-3"},
				"operation":           n.op(),
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
	mux.HandleFunc("GET /work/retention/reanchor", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /work/retention/reanchor", func(w http.ResponseWriter, r *http.Request) {
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

// TestTheExternalAssertionIsExplicit: on a broker the engine does not run,
// stopping every Crewlet node establishes nothing, and the operator says so in
// their own words rather than a check quietly proving nothing.
func TestTheExternalAssertionIsExplicit(t *testing.T) {
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
// incarnation is unchanged, and which admission is blocking activation.
func TestMaintenanceStatusNamesWhatIsHoldingTheSeal(t *testing.T) {
	node := newFakeCapacityNode(t)
	stdout, _, err := cli(t, "retention", "maintenance", "status",
		bootstrapForURL(t, node.server.URL), "-stream", "CREWLET_TRACKER_LOG")
	if err != nil {
		t.Fatalf("maintenance status: %v", err)
	}
	for _, want := range []string{
		"op-1",                        // the operation
		"attempt 1",                   // and its attempt
		"node-1:old",                  // what each participant was baselined as
		"node-1:new",                  // and what it acknowledged as
		"node-2",                      // the one that has not
		"node-2 has not acknowledged", // said in words
		"node-3",                      // and the admission blocking activation
		"issued",                      // the unresolved write attempt
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the status never mentions %q:\n%s", want, stdout)
		}
	}
}

// TestMaintenanceStatusOnAFleetWithNoOperationSaysSo: "no window" and "a
// window in phase opened" are different facts with different next steps.
func TestMaintenanceStatusOnAFleetWithNoOperationSaysSo(t *testing.T) {
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
	if !strings.Contains(stdout, "No capacity operation is open") {
		t.Fatalf("a fleet with no window does not say so:\n%s", stdout)
	}
	if !strings.Contains(stdout, "normal for a fleet in service") {
		t.Errorf("the admissions are reported without saying they are expected "+
			"on a fleet that is running:\n%s", stdout)
	}
}

// TestAReanchorPrintsTheValueItWillConfirmAgainst.
//
// The confirmation means "I looked at the thing I am re-anchoring", so the
// verb prints it and refuses, rather than reading it and feeding it straight
// back — which would be confirming against its own output.
func TestAReanchorPrintsTheValueItWillConfirmAgainst(t *testing.T) {
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
	root := t.TempDir()
	taken := time.Date(2031, 3, 1, 3, 0, 0, 0, time.UTC)
	writeArtefact(t, filepath.Join(root, "nightly"), taken)

	// INSIDE THE CADENCE.
	var out strings.Builder
	if err := verifyRestore(root, 30*24*time.Hour, taken.Add(72*time.Hour), &out); err != nil {
		t.Fatalf("a three-day-old artefact was refused: %v", err)
	}
	for _, want := range []string{"CREWLET_TRACKER_LOG", "generation 2", "sequence 918280001"} {
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
