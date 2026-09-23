package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeRetentionNode answers the retention read and the two gate routes, recording
// what it was asked for.
type fakeRetentionNode struct {
	server *httptest.Server

	// report is served as the WRITER'S OWN TYPE, encoded as the node
	// encodes it, never as JSON typed here: a fixture spelling this
	// command's idea of the shape agrees with the command whatever the
	// node sends, and that is how `status` came to decode nothing from any
	// real node while every case here passed.
	report *statelog.Report

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
func blockedReport() *statelog.Report {
	stamp := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return at
	}
	headroom, caughtUp := 0.984, uint64(0)
	return &statelog.Report{
		V:                statelog.ReportVersion,
		NodeID:           "node-1",
		At:               stamp("2031-04-02T03:14:00Z"),
		ReadLevel:        statelog.ReadStale,
		BackupOwner:      "platform-oncall",
		RegisterReadable: true,
		Domains: []statelog.DomainReport{{
			Domain: "tracker", Stream: "CREWLET_TRACKER_LOG",
			Replay:   statelog.ReplayStrict,
			FirstSeq: 918100000, LastSeq: 918280001,
			Bytes: 67108864, MaxBytes: 4294967296,
			HeadroomFraction: &headroom,
			TrimFloor:        918100000,
			BlockedBy:        statelog.TermBackupFloor,
			BlockedSince:     stamp("2031-03-30T02:00:00Z"),
			Prose: "Nothing is being trimmed on tracker: the newest complete " +
				"backup is 3 days old (backup_max_age is 24h).",
			Terms: []statelog.TermReport{
				{Name: statelog.TermApplied, State: statelog.TermKnown, Seq: 918279004},
				{Name: statelog.TermBackupFloor, State: statelog.TermUnreadable,
					Detail: "no backup has been taken"},
				{Name: statelog.TermFeedAckFloor, State: statelog.TermAbsent},
			},
		}},
		Nodes: []statelog.NodeReport{{
			NodeID: "node-1", Counted: true, Live: true,
			At: stamp("2031-04-02T03:13:58Z"),
			Domains: map[string]statelog.NodeDomainReport{"tracker": {
				Seq: 918280001, AppliedThrough: 918280001, Lag: &caughtUp,
			}},
		}, {
			NodeID: "node-4", Counted: false, Live: false,
			At: stamp("2031-03-28T09:00:00Z"),
			Domains: map[string]statelog.NodeDomainReport{"tracker": {
				Seq: 918000000, AppliedThrough: 918000000,
			}},
			Evicted: &statelog.EvictionReport{
				By: "sre@example.com", At: stamp("2031-04-01T12:00:00Z"),
				EffectiveAt: stamp("2031-04-01T12:01:00Z"), Effective: true,
			},
		}, {
			NodeID: "node-7", Counted: true, Live: true,
		}},
		Snapshots: []statelog.SnapshotReport{
			{NodeID: "node-1", At: stamp("2031-04-02T01:00:00Z"), Bytes: 10415140864,
				Domains: map[string]uint64{"tracker": 918279900}},
			{NodeID: "node-4", Skip: statelog.SkipLagging},
		},
		Replica: statelog.ReplicaReport{
			StoreBytes: 10415140864, ProjectedJoinSeconds: 308,
			RejoinWindowSeconds: 1800,
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
		"no position yet",     // and says why it has no figures
		"platform-oncall",     // who owns the backup
		// AND THE FIGURES THE WRITER KEYS OR SPELLS DIFFERENTLY FROM A
		// GUESS: a node's progress is a map by domain, a readable term's
		// state is `ok`, and a tombstone is `evicted` — each of which
		// this command once declared otherwise, and then either failed
		// to decode or printed as nothing.
		"918280001",                  // node-1's own tracker position
		"918279004",                  // the applied term's sequence
		"evicted by sre@example.com", // node-4's tombstone
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
	report.Alarms = []statelog.Alarm{{
		Kind:   statelog.KindBackupAge,
		Detail: "the newest verified backup is 3 days old",
		Remedy: "take a backup, or name the owner in retention.backup_owner",
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
	report.Maintenance = &statelog.MaintenanceReport{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-9",
		Phase: "observe", Attempt: 2,
		TargetMaxBytes: 2000000000, OriginalMaxBytes: 1000000000,
		Since: time.Date(2031, 4, 2, 2, 0, 0, 0, time.UTC), By: "sre@example.com",
		ParticipantsMissing: []string{"node-4", "node-7"},
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
	report.Maintenance = &statelog.MaintenanceReport{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "op-9",
		Phase: "sealed", Attempt: 1,
		Since: time.Date(2031, 4, 2, 2, 0, 0, 0, time.UTC),
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
	report.Domains[0].BlockedBy = ""
	report.Domains[0].Prose = ""
	report.Domains[0].TrimTo = 918279004
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
	// AND THE WATERMARK IT PROMISES IS PRINTED — a read of the node's
	// answer that failed used to print nothing, silently.
	if !strings.Contains(stdout, "tracker trim floor 918100000 → 918100000") {
		t.Errorf("the eviction printed no watermark:\n%s", stdout)
	}
}

// `crewlet retention readmit` IS REFUSED FOR A NODE BELOW THE TRIM FLOOR, and
// the refusal prints the node's position beside the floor — end to end, from
// the verb through the route and the writer to a real engine's register,
// floors and log.
//
// This is what the reference and the retention guide promised and what nothing
// did: the verb wrote the inverse record for any node and printed `applied`,
// so an operator readmitting a machine that was still offline put back the
// very pin the eviction had lifted and was told it had worked.
func TestReadmittingANodeBelowTheFloorIsRefused(t *testing.T) {
	e := testEngine(t)
	boot := bootstrapFor(t, 0)
	boot.API.Port = freePort(t)
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: "a-test-token"}}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	node := []string{"-url", base, "-token", "a-test-token"}
	deadline := time.Now().Add(20 * time.Second)
	for !e.NativeHydrated() {
		if time.Now().After(deadline) {
			t.Fatal("the node never established its state log")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// TWO EVICTIONS, so the log is far enough along for a floor to pass a
	// node that has applied nothing.
	for _, away := range []string{"node-away", "node-spare"} {
		if _, _, err := cli(t, append([]string{"retention", "evict", away,
			"-confirm", away}, node...)...); err != nil {
			t.Fatalf("evict %s: %v", away, err)
		}
	}
	client := &nodeClient{base: base, token: "a-test-token", http: httpxtest.Pool(t)}
	var report retentionReport
	if err := client.get(t.Context(), "/query/retention", &report); err != nil {
		t.Fatalf("read the retention report: %v", err)
	}
	var tracker retentionDomain
	for _, d := range report.Domains {
		if d.Domain == "tracker" {
			tracker = d
		}
	}
	if tracker.LastSeq < 2 {
		t.Fatalf("the tracker log ends at %d, too near its start for a floor to "+
			"pass a node at zero", tracker.LastSeq)
	}

	// THE FLEET MOVED ON WITHOUT IT: the trim published a floor at the log's
	// end while node-away's last heartbeat said it had applied nothing.
	fleet := e.Backends().Fleet
	if err := fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: "tracker", Generation: tracker.Generation,
		TrimTo: tracker.LastSeq, Floor: tracker.LastSeq,
		At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish the floor: %v", err)
	}
	reportAt := func(seq uint64) {
		t.Helper()
		if err := fleet.PutPositions(t.Context(), coord.NodePositions{
			NodeID: "node-away", At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{"tracker": {
				Generation: tracker.Generation, Seq: seq, AppliedThrough: seq}},
		}); err != nil {
			t.Fatalf("publish node-away's position: %v", err)
		}
	}
	readmit := append([]string{"retention", "readmit", "node-away",
		"-confirm", "node-away"}, node...)

	reportAt(0)
	stdout, _, err := cli(t, readmit...)
	if err == nil {
		t.Fatalf("a node at 0 against a floor of %d was readmitted:\n%s",
			tracker.LastSeq, stdout)
	}
	floor := strconv.FormatUint(tracker.LastSeq, 10)
	for _, want := range []string{"409", "readmission_refused", "node-away",
		"is 0", "below " + floor, "crewlet retention snapshots"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never says %q:\n%v", want, err)
		}
	}
	if strings.Contains(stdout, "applied") {
		t.Errorf("a refused readmission printed an outcome:\n%s", stdout)
	}

	// AND ONCE IT HAS APPLIED EVERY RECORD UP TO THE ONE BEFORE THE FLOOR,
	// IT IS TAKEN BACK.
	reportAt(tracker.LastSeq - 1)
	stdout, _, err = cli(t, readmit...)
	if err != nil {
		t.Fatalf("a node one record short of the floor was refused: %v", err)
	}
	if !strings.Contains(stdout, "readmit node-away: applied") {
		t.Errorf("the readmission did not report landing:\n%s", stdout)
	}
	// AND THE WATERMARK IS READ FROM A REAL NODE'S ANSWER, which is the
	// one encoding every fixture here can drift from.
	if want := "tracker trim floor " + floor; !strings.Contains(stdout, want) {
		t.Errorf("the readmission printed no watermark from the node's own "+
			"report, want %q:\n%s", want, stdout)
	}
	if stdout, _, err := cli(t, append([]string{"retention", "status"}, node...)...); err != nil &&
		!strings.Contains(err.Error(), "alarm") {
		t.Fatalf("retention status against a real node: %v\n%s", err, stdout)
	} else if !strings.Contains(stdout, "node-away") {
		t.Errorf("retention status against a real node never lists node-away:\n%s", stdout)
	}
}
