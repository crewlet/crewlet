package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRetentionNode answers the retention read and the two gate routes, recording
// what it was asked for.
type fakeRetentionNode struct {
	server *httptest.Server
	report map[string]any

	acked    map[string]string
	gated    string
	confirm  string
	gateKind string
}

func newFakeRetentionNode(t *testing.T) *fakeRetentionNode {
	t.Helper()
	n := &fakeRetentionNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /query/retention", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := n.report
		if body == nil {
			body = blockedReport()
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("POST /work/retention/ack", func(w http.ResponseWriter, r *http.Request) {
		n.acked = map[string]string{
			"stream":   r.URL.Query().Get("stream"),
			"position": r.URL.Query().Get("position"),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stream": r.URL.Query().Get("stream"), "position": 918100000,
			"generation": 2,
		})
	})
	for _, verb := range []string{"evict", "readmit"} {
		mux.HandleFunc("POST /work/retention/"+verb+"/{node}",
			func(w http.ResponseWriter, r *http.Request) {
				n.gated, n.confirm = r.PathValue("node"), r.URL.Query().Get("confirm")
				n.gateKind = verb
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"node": r.PathValue("node"), "evicted": verb == "evict",
					"outcome": "applied",
					"position": map[string]any{
						"stream": "CREWLET_TRACKER_LOG", "seq": 918280002,
					},
				})
			})
	}
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// blockedReport is a fleet whose trim is held by the backup term — the state a
// fresh company is in, and the one an operator most often runs this for.
func blockedReport() map[string]any {
	return map[string]any{
		"node_id":           "node-1",
		"at":                "2031-04-02T03:14:00Z",
		"backup_owner":      "platform-oncall",
		"register_readable": true,
		"domains": []map[string]any{{
			"domain": "tracker", "stream": "CREWLET_TRACKER_LOG",
			"generation": 0, "replay": "strict",
			"first_seq": 918100000, "last_seq": 918280001,
			"bytes": 67108864, "max_bytes": 4294967296,
			"headroom_fraction": 0.984,
			"trim_floor":        918100000,
			"blocked_by":        "backup_floor",
			"blocked_since":     "2031-03-30T02:00:00Z",
			"prose": "Nothing is being trimmed on tracker: the newest complete " +
				"backup is 3 days old (backup_max_age is 24h).",
			"terms": []map[string]any{
				{"name": "applied", "state": "known", "seq": 918279004},
				{"name": "backup_floor", "state": "unknown",
					"detail": "no backup has been taken"},
				{"name": "feed_ack_floor", "state": "absent"},
			},
		}},
		"nodes": []map[string]any{{
			"node_id": "node-1", "counted": true, "live": true,
			"at": "2031-04-02T03:13:58Z",
			"domains": []map[string]any{{
				"domain": "tracker", "seq": 918280001,
				"applied_through": 918280001, "lag": 0,
			}},
		}, {
			"node_id": "node-7", "counted": true, "live": true,
			"note": "counted · no position yet",
		}},
		"snapshots": []map[string]any{
			{"node_id": "node-1", "at": "2031-04-02T01:00:00Z", "bytes": 10415140864,
				"domains": []map[string]any{{"domain": "tracker", "seq": 918279900}}},
			{"node_id": "node-4", "skip": "lagging"},
		},
		"replica": map[string]any{
			"store_bytes": 10415140864, "projected_join_seconds": 308,
			"rejoin_window_seconds": 1800,
		},
	}
}

// TestRetentionStatusLeadsWithTheBlockingTermInProse is the whole point of the
// verb: an operator runs it because the log is growing, and the answer to that
// question must not be a column they have to find.
func TestRetentionStatusLeadsWithTheBlockingTermInProse(t *testing.T) {
	node := newFakeRetentionNode(t)
	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(stdout), "\n", 2)[0]
	if !strings.Contains(first, "Nothing is being trimmed on tracker") {
		t.Fatalf("the first line is %q — the blocking term is what this command "+
			"is run for, and a table an operator has to read a column out of "+
			"is a table they read second", first)
	}
	for _, want := range []string{
		"CREWLET_TRACKER_LOG", // the log this is about
		"backup_floor",        // the term holding it
		"2031-03-30",          // how long it has been holding
		"node-7",              // a counted node with no position still gets a row
		"platform-oncall",     // who owns the backup
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never mentions %q:\n%s", want, stdout)
		}
	}
}

// TestRetentionStatusExitsNonZeroOnAnAlarm is the hook a cron watches. The
// rule is the REPORT's — a second rule here would be a second definition of
// "is something wrong", and the shell script would be watching the one nobody
// maintained.
func TestRetentionStatusExitsNonZeroOnAnAlarm(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	report["alarms"] = []map[string]any{{
		"kind":   "backup_age",
		"detail": "the newest verified backup is 3 days old",
		"remedy": "take a backup, or name the owner in retention.backup_owner",
	}}
	node.report = report

	_, stderr, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err == nil {
		t.Fatal("an active alarm exited zero — the cron watching this exit code " +
			"would never fire")
	}
	// ON STDERR, so a cron capturing stdout for a dashboard still puts the
	// reason in its own mail.
	for _, want := range []string{"backup_age", "3 days old", "retention.backup_owner"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr never mentions %q:\n%s", want, stderr)
		}
	}
}

// MAINTENANCE IS PRINTED ABOVE EVERYTHING, because while it is open every
// number below it describes a fleet in which nothing is running: no seats, no
// duties, no scheduler, no write routes. It was visible on no surface at all —
// an operator watching a company go completely quiet had nothing to read that
// said why, and the alarm that names it only fires after an hour of it.
func TestRetentionStatusLeadsWithAnOpenMaintenanceWindow(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	report["maintenance"] = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "operation_id": "op-9",
		"phase": "observe", "attempt": 2,
		"target_max_bytes": 2000000000, "original_max_bytes": 1000000000,
		"since": "2031-04-02T02:00:00Z", "by": "sre@example.com",
		"participants_missing": []string{"node-4", "node-7"},
	}
	node.report = report

	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(stdout), "\n", 2)[0]
	if !strings.Contains(first, "MAINTENANCE IS OPEN") {
		t.Errorf("the first line is %q — a blocked trim read without knowing "+
			"nothing is running sends an operator after the wrong thing", first)
	}
	for _, want := range []string{"observe", "attempt 2", "sre@example.com",
		"node-4, node-7"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never mentions %q:\n%s", want, stdout)
		}
	}
}

// NOBODY OUTSTANDING IS NOT PROGRESS. It is the operation waiting on whoever
// ran the verb, and printing nothing there reads as "nearly done" — which is
// the one reading that stops somebody finishing it.
func TestAnOperationWithNobodyOutstandingSaysWhoItIsWaitingFor(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	report["maintenance"] = map[string]any{
		"stream": "CREWLET_TRACKER_LOG", "operation_id": "op-9",
		"phase": "sealed", "attempt": 1,
		"since": "2031-04-02T02:00:00Z",
	}
	node.report = report

	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	if !strings.Contains(stdout, "waiting on its operator") {
		t.Errorf("an operation with nobody outstanding printed no reason:\n%s",
			stdout)
	}
}

// AND A FLEET WITH NO OPERATION SAYS NOTHING ABOUT ONE, or the banner above
// would be a line every operator learns to skip.
func TestAHealthyFleetPrintsNoMaintenanceBanner(t *testing.T) {
	node := newFakeRetentionNode(t)
	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	if strings.Contains(stdout, "MAINTENANCE") {
		t.Errorf("a fleet with no open operation printed a maintenance "+
			"banner:\n%s", stdout)
	}
}

// TestRetentionStatusIsSilentAndZeroOnAHealthyFleet is the other half: an
// alarm that fires on a fleet doing exactly what it was asked to do is one
// nobody believes the second time.
func TestRetentionStatusIsSilentAndZeroOnAHealthyFleet(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	domains := report["domains"].([]map[string]any)
	domains[0]["blocked_by"] = ""
	domains[0]["prose"] = ""
	domains[0]["trim_to"] = 918279004
	node.report = report

	stdout, stderr, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("a healthy fleet exited non-zero: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, "Nothing is being trimmed") {
		t.Errorf("an advancing trim still printed a blocked sentence:\n%s", stdout)
	}
}

// TestRetentionSnapshotsNamesWhyANodeHoldsNone is what the verb exists for: a
// node with no artefact and no reason renders as a node nobody asked, and the
// absence is the answer to "why did the join fail".
func TestRetentionSnapshotsNamesWhyANodeHoldsNone(t *testing.T) {
	node := newFakeRetentionNode(t)
	stdout, _, err := cli(t, "retention", "snapshots", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention snapshots: %v", err)
	}
	if !strings.Contains(stdout, "node-4") || !strings.Contains(stdout, "lagging") {
		t.Fatalf("a node holding no snapshot is not reported with its reason:\n%s",
			stdout)
	}
	if !strings.Contains(stdout, "tracker 918279900") {
		t.Errorf("the donor's own position per domain is missing:\n%s", stdout)
	}
}

// TestAnAcknowledgementNamesBothTheLogAndTheSequence: an acknowledgement moves
// the floor the trim deletes against, so there is no value to guess.
func TestAnAcknowledgementNamesBothTheLogAndTheSequence(t *testing.T) {
	node := newFakeRetentionNode(t)
	if _, _, err := cli(t, "retention", "ack",
		bootstrapForURL(t, node.server.URL), "-position", "918100000"); err == nil {
		t.Error("an acknowledgement with no stream was accepted")
	}
	if _, _, err := cli(t, "retention", "ack",
		bootstrapForURL(t, node.server.URL), "-stream", "S"); err == nil {
		t.Error("an acknowledgement with no position was accepted")
	}
	if node.acked != nil {
		t.Fatalf("the node was contacted anyway: %v", node.acked)
	}

	if _, _, err := cli(t, "retention", "ack", bootstrapForURL(t, node.server.URL),
		"-stream", "CREWLET_TRACKER_LOG", "-position", "918100000"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if node.acked["stream"] != "CREWLET_TRACKER_LOG" || node.acked["position"] != "918100000" {
		t.Fatalf("the node was asked for %v", node.acked)
	}
}

// TestAGateGestureRequiresTheNodeIdTwice: an eviction stops a machine's
// records applying anywhere in the fleet, which is not a value to get from a
// shell history.
func TestAGateGestureRequiresTheNodeIdTwice(t *testing.T) {
	node := newFakeRetentionNode(t)
	base := bootstrapForURL(t, node.server.URL)

	if _, _, err := cli(t, "retention", "evict", "node-4", base); err == nil {
		t.Error("an eviction with no confirmation was accepted")
	}
	if _, _, err := cli(t, "retention", "evict", "node-4", base,
		"-confirm", "node-5"); err == nil {
		t.Error("an eviction confirming a different node was accepted")
	}
	if node.gated != "" {
		t.Fatalf("the node was contacted anyway, for %q", node.gated)
	}

	stdout, _, err := cli(t, "retention", "evict", "node-4", base, "-confirm", "node-4")
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if node.gated != "node-4" || node.confirm != "node-4" {
		t.Fatalf("the node was asked to evict %q confirming %q", node.gated, node.confirm)
	}
	// THE WINDOW IS SAID OUT LOUD, because an operator who does not know
	// the node stays counted reads the unchanged floor as a failed gesture.
	if !strings.Contains(stdout, "stays COUNTED") {
		t.Errorf("the eviction never says the node stays counted:\n%s", stdout)
	}
}
