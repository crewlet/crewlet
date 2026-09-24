package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx"
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

	// gateQuery is everything the last gate request carried, and
	// gateResult what the gate answers with — every log applied when nil.
	// gateErr, when it is one of the gate's refusals, is answered instead.
	gateQuery  url.Values
	gateResult *engine.GateResult
	gateErr    error

	// hangUp makes a gate request go unanswered: the connection is closed
	// with no response.
	hangUp bool

	// gateReply, when set, writes the gate's whole response instead of the
	// route's renderer — what something in front of the node sends.
	gateReply func(w http.ResponseWriter)

	// mu guards the gate fields for a reader that no response synchronises
	// with — a request [fakeRetentionNode.hangUp] never answered.
	mu sync.Mutex
}

// lastGate is the query the last gate request carried, read under the lock.
func (n *fakeRetentionNode) lastGate() url.Values {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gateQuery
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
				n.mu.Lock()
				n.gated, n.confirm = r.PathValue("node"), r.URL.Query().Get("confirm")
				n.gateKind = verb
				n.gateQuery = r.URL.Query()
				n.mu.Unlock()
				if n.gateReply != nil {
					n.gateReply(w)
					return
				}
				if n.hangUp {
					// NO ANSWER AT ALL: the connection goes the way a
					// client timeout or a dropped link takes it.
					if hj, ok := w.(http.Hijacker); ok {
						if conn, _, err := hj.Hijack(); err == nil {
							_ = conn.Close()
						}
					}
					return
				}
				opID := r.URL.Query().Get("op_id")
				if opID == "" {
					opID = statelog.NewOpID(time.Now(), verb+"-"+r.PathValue("node"))
				}
				w.Header().Set("Content-Type", "application/json")
				// A REFUSAL, RENDERED BY THE ROUTE'S OWN RENDERER, so the
				// actions this command turns into flags are the ones a real
				// node sends.
				if refusal, ok := api.RenderGateRefusal(r.PathValue("node"),
					opID, n.gateErr); ok {
					w.WriteHeader(refusal.Status)
					_ = json.NewEncoder(w).Encode(refusal.Body)
					return
				}
				result := engine.GateResult{Node: r.PathValue("node"), OpID: opID,
					Domains: []engine.DomainGate{
						{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG",
							OpID: opID + ".evict.tracker", Outcome: statelog.OutcomeApplied,
							Position: statelog.Position{
								Stream: "CREWLET_TRACKER_LOG", Seq: 918280002}},
						{Domain: "pages", Stream: "CREWLET_PAGES_LOG",
							OpID: opID + ".evict.pages", Outcome: statelog.OutcomeApplied,
							Position: statelog.Position{
								Stream: "CREWLET_PAGES_LOG", Seq: 4410}},
					}}
				if n.gateResult != nil {
					result.Domains = n.gateResult.Domains
				}
				// THE ROUTE'S OWN RENDERER, never a copy of it here: the copy
				// this replaced claimed it "cannot drift" and already sent a
				// position for an unknown log that no node sends.
				_ = json.NewEncoder(w).Encode(api.RenderGate(verb == "evict", result))
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
			Bytes: 67108864, MaxBytes: 4294967296, ReserveBytes: 268435456,
			HeadroomFraction: &headroom,
			TrimFloor:        918100000,
			TrimFloorState:   statelog.TrimFloorPublished,
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
		// THE GATE RESERVE, beside the ceiling it is kept under: without
		// it a headroom of 0% reads as a log nothing can be written to,
		// when an eviction still lands there.
		"RESERVE", "256.0 MiB",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never mentions %q:\n%s", want, stdout)
		}
	}
}

// A DOMAIN THE TRIM HAS CONCLUDED NOTHING ABOUT PRINTS NO FLOOR, AND SAYS SO.
//
// After every reanchor the report fills nothing from the old generation's
// floor row until the trim's first tick on the adopted stream. The command
// printed TRIM FLOOR 0 — indistinguishable from a published floor of zero —
// and an empty WATERMARKS block with no blocking term, which reads as a trim
// nothing is holding.
func TestAReanchoredDomainPrintsNoFloorAndSaysWhy(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	d := &report.Domains[0]
	d.Generation, d.TrimFloor, d.TrimTo = 1, 0, 0
	d.TrimFloorState = statelog.TrimFloorNoneAtGeneration
	d.BlockedBy, d.BlockedSince, d.Prose, d.Terms = "", time.Time{}, "", []statelog.TermReport{}
	node.report = report

	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	if !strings.Contains(stdout, "the trim has concluded nothing about generation 1 yet") {
		t.Errorf("the reanchored domain's watermarks never say nothing is concluded:\n%s",
			stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "tracker" && fields[len(fields)-1] != "-" {
			t.Errorf("the domain row's TRIM FLOOR is %q, want - for no conclusion: %q",
				fields[len(fields)-1], line)
		}
	}

	// AND AN UNREADABLE REGISTER SAYS SO RATHER THAN NOTHING.
	d.TrimFloorState = statelog.TrimFloorUnreadable
	stdout, _, err = cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	if !strings.Contains(stdout, "could not be read") {
		t.Errorf("an unreadable floor register is not said:\n%s", stdout)
	}
}

// A DOMAIN THIS NODE REFUSES LEADS THE STATUS, NAMING THE FINDING — AND A NODE
// ON ANOTHER GENERATION OR WITH A DIVERGED LOG IS MARKED.
//
// The retention guide sends an operator here after a restore to find the
// not-ready domain and the diverged peer, and the report carried neither: the
// domain row looked ordinary, and a peer on a generation the log had left read
// lag 0 against a sequence space that no longer exists.
func TestRetentionStatusNamesARefusedDomainAndAStalePeer(t *testing.T) {
	node := newFakeRetentionNode(t)
	report := blockedReport()
	report.Domains[0].Generation = 1
	report.Domains[0].NotReady = &statelog.DomainRefusal{
		Code:   string(statelog.RefuseWrongStream),
		Causes: []statelog.IdentityCause{statelog.CauseRecreated},
		Detail: "tracker's rows are keyed to the stream created at 2031-04-01",
	}
	report.Domains[0].WritesRefused = &statelog.DomainRefusal{
		Code:   string(statelog.ReasonLogTruncated),
		Detail: "peer node-4 stands at sequence 918000000",
	}
	report.Nodes[1].Domains["tracker"] = statelog.NodeDomainReport{
		Generation: 0, Seq: 918000000, AppliedThrough: 918000000,
		GenerationState: statelog.GenerationLeft, LogDiverged: true,
	}
	node.report = report

	stdout, _, err := cli(t, "retention", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(stdout), "\n", 2)[0]
	if first != "NOT READY tracker: wrong_stream (recreated) — tracker's rows are "+
		"keyed to the stream created at 2031-04-01" {
		t.Errorf("the first line is %q, want the refusal naming its finding", first)
	}
	for _, want := range []string{
		"WRITES REFUSED tracker: log_truncated — peer node-4",
		"left gen 0",
		"LOG DIVERGED",
		"GEN",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the status never says %q:\n%s", want, stdout)
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

// A GESTURE THAT DID NOT REACH EVERY LOG EXITS NON-ZERO, SAYS WHICH LOG, AND
// SAYS HOW TO FINISH IT — WHERE RUNNING IT AGAIN CAN.
//
// An eviction is a record on every identity-claiming log and each log answers
// on its own. A gesture that landed on the tracker's log and not the pages log
// has lifted one pin and left the other, and the only honest thing to print is
// both answers and what finishes the missing one. For an `unknown` outcome that
// is the same gesture under the operation id it answered with — carrying
// -force, or the retry is judged again against the lease the operator
// overrode. For a full log it is NOT: the same command is refused the same way
// for ever, and advising it anyway sent the operator round a loop.
func TestAGateGestureThatMissedALogSaysHowToFinishIt(t *testing.T) {
	node := newFakeRetentionNode(t)
	base := bootstrapForURL(t, node.server.URL)
	gesture := statelog.NewOpID(time.Now().Add(-time.Minute), "evict-node-4")
	applied := engine.DomainGate{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG",
		OpID: gesture + ".evict.tracker", Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 918280002}}
	node.gateResult = &engine.GateResult{Domains: []engine.DomainGate{applied,
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: gesture + ".evict.pages",
			Outcome: statelog.OutcomeUnknown}}}

	stdout, _, err := cli(t, "retention", "evict", "node-4", base,
		"-confirm", "node-4", "-op-id", gesture, "-force")
	if err == nil {
		t.Fatalf("a gesture that missed the pages log exited zero:\n%s", stdout)
	}
	if got := node.gateQuery.Get("op_id"); got != gesture {
		t.Errorf("the node was sent op_id %q, want the operator's own %s — a "+
			"retry under a fresh id is a second gesture", got, gesture)
	}
	if got := node.gateQuery.Get("force"); got != "true" {
		t.Errorf("the node was sent force=%q for an eviction run with -force", got)
	}
	again := "-op-id " + gesture + " -force"
	for _, want := range []string{
		"tracker: applied at CREWLET_TRACKER_LOG 918280002",
		"pages: unknown",
		again,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output never says %q:\n%s", want, stdout)
		}
	}
	// NO POSITION FOR AN UNKNOWN LOG: the route sends none, and a line
	// printing one — "unknown at 0" — reads as a record at the log's origin.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "pages: unknown") && strings.Contains(line, " at ") {
			t.Errorf("the unknown log's line prints a position: %q", line)
		}
	}
	if !strings.Contains(err.Error(), again) {
		t.Errorf("the error never names the command that finishes it: %v", err)
	}
	// AND IT DOES NOT CLAIM THE NODE STOPS BEING COUNTED, which is only true
	// once every log holds the record.
	if strings.Contains(stdout, "stays COUNTED") {
		t.Errorf("an incomplete eviction printed the fence window as though it "+
			"had taken effect:\n%s", stdout)
	}

	// A FULL LOG IS NOT OFFERED THE SAME COMMAND AGAIN, and says what makes
	// room instead — as this command's own verb, which the node's sentence
	// no longer spells, because the dashboard renders it too.
	node.gateResult = &engine.GateResult{Domains: []engine.DomainGate{applied,
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: gesture + ".evict.pages",
			Err: fmt.Errorf("pages: %w", &statelog.Unavailable{
				Reason: statelog.ReasonLogFull, Detail: "the broker refused to store it"})}}}
	stdout, _, err = cli(t, "retention", "evict", "node-4", base,
		"-confirm", "node-4", "-op-id", gesture)
	if err == nil {
		t.Fatalf("a gesture a full log refused exited zero:\n%s", stdout)
	}
	if !strings.Contains(stdout, "pages: not written (log_full)") ||
		!strings.Contains(stdout, "crewlet retention set-capacity CREWLET_PAGES_LOG") {
		t.Errorf("a full log's refusal never names the capacity verb:\n%s", stdout)
	}
	// NOT THE SAME COMMAND NOW — it is refused the same way until the
	// ceiling moves — AND NOT A FRESH ONE AFTER: once there is room, this
	// gesture's own id is what finishes it, because a fresh one writes the
	// tracker's log again and re-dates the eviction there.
	if strings.Contains(stdout, "The gesture has not reached every log. Run it again") {
		t.Errorf("a full log was advised the retry it refuses until its ceiling "+
			"moves:\n%s", stdout)
	}
	if !strings.Contains(stdout, "-confirm <bytes>, then run this again with -op-id "+gesture) ||
		!strings.Contains(err.Error(), "then run it again with -op-id "+gesture) {
		t.Errorf("a full log was not told to finish this gesture once it has "+
			"room:\n%s\n%v", stdout, err)
	}

	// A SUPERSEDED OPERATION IS ADVISED A NEW GESTURE, and not this one again.
	node.gateResult = &engine.GateResult{Domains: []engine.DomainGate{applied,
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: gesture + ".evict.pages",
			Err: fmt.Errorf("pages: %w", &statelog.Unavailable{
				Reason: statelog.ReasonSuperseded, Detail: "undone since"})}}}
	stdout, _, err = cli(t, "retention", "evict", "node-4", base,
		"-confirm", "node-4", "-op-id", gesture)
	if err == nil || !strings.Contains(stdout, "run it again without -op-id") {
		t.Errorf("a superseded log was not advised a fresh gesture (%v):\n%s", err, stdout)
	}
	if strings.Contains(stdout, "-op-id "+gesture) {
		t.Errorf("a superseded log was advised the operation it can never finish:\n%s",
			stdout)
	}

	// A READMISSION HAS NO -force: it is refused as a flag rather than sent.
	if _, _, err := cli(t, "retention", "readmit", "node-4", base,
		"-confirm", "node-4", "-force"); err == nil {
		t.Error("readmit accepted -force, which it has no meaning for")
	}
}

// A REFUSED EVICTION SAYS HOW TO GET PAST IT IN THIS COMMAND'S OWN FLAGS.
//
// The node names what to do as actions and a sentence spelling no flag, since
// the dashboard renders the same refusal; this command turns `force` into
// -force — and does not offer it to an operator who already passed it.
func TestARefusedEvictionNamesTheFlagThatForcesIt(t *testing.T) {
	node := newFakeRetentionNode(t)
	base := bootstrapForURL(t, node.server.URL)
	for name, refusal := range map[string]error{
		"a live lease": fmt.Errorf("engine: evict node node-4: %w",
			statelog.PermitEviction("node-4",
				[]statelog.Presence{{NodeID: "node-4"}}, false)),
		"an unreadable listing": &engine.GateUnjudged{Node: "node-4",
			Err: fmt.Errorf("list the live nodes: coordination is unreachable")},
	} {
		node.gateErr = refusal
		_, _, err := cli(t, "retention", "evict", "node-4", base, "-confirm", "node-4")
		if err == nil {
			t.Fatalf("%s: a refused eviction exited zero", name)
		}
		if !strings.Contains(err.Error(), "evict it with -force") {
			t.Errorf("%s: the refusal never names -force:\n%v", name, err)
		}
	}
	node.gateErr = fmt.Errorf("engine: evict node node-4: %w",
		statelog.PermitEviction("node-4", []statelog.Presence{{NodeID: "node-4"}}, false))
	_, _, err := cli(t, "retention", "evict", "node-4", base, "-confirm", "node-4")
	if err == nil || !strings.Contains(err.Error(), "LIVE column") {
		t.Errorf("a live node's refusal never says how to tell its lease lapsed: %v", err)
	}
}

// A GESTURE THE NODE NEVER ANSWERED STILL NAMES THE OPERATION THAT FINISHES
// IT — WHICH THE COMMAND MINTED BEFORE ASKING.
//
// The node minted the id and sent it back in the answer, so a request that
// timed out or dropped — after the node had very likely written every log —
// left the operator no handle on the gesture but a second one under a fresh id.
// And the wait was the ten seconds every other verb takes, which two of the
// five-second resolutions a gesture legitimately makes use up between them.
func TestAGateTheNodeNeverAnsweredNamesItsOperation(t *testing.T) {
	node := newFakeRetentionNode(t)
	base := bootstrapForURL(t, node.server.URL)
	node.hangUp = true

	_, stderr, err := cli(t, "retention", "evict", "node-4", base,
		"-confirm", "node-4", "-force")
	if err == nil {
		t.Fatal("a gesture the node never answered exited zero")
	}
	sent := node.lastGate().Get("op_id")
	if sent == "" {
		t.Fatal("the request carried no op_id, so an unanswered gesture has no " +
			"operation to finish it under")
	}
	// MINTED THE STATE LOG'S WAY, carrying its instant: the node refuses an
	// id that carries none (`op_id_invalid`), since no ledger could vouch
	// for it, so an id minted any other way is one the retry cannot use.
	if _, minted := statelog.OpMintedAt(sent); !minted {
		t.Errorf("the command sent op_id %q, which carries no mint instant — "+
			"the node refuses it", sent)
	}
	if want := "-op-id " + sent + " -force"; !strings.Contains(stderr, want) {
		t.Errorf("the unanswered gesture never says %q:\n%s", want, stderr)
	}

	// AND THE NODE'S OWN BOUND IS INSIDE THE WAIT, so its answer — not a
	// client timeout that knows none of it — is what reaches the operator.
	if gateRequestTimeout <= engine.GateBudget {
		t.Fatalf("the command waits %s for a gesture the node bounds at %s",
			gateRequestTimeout, engine.GateBudget)
	}
}

// AN ANSWER THE NODE DID NOT WRITE IS NOT A REFUSAL. A reverse proxy's read
// timeout — a 504 with an HTML page, at a minute, which is also the budget the
// node gives a gesture past its judgement — and a 200 cut off part way through
// both leave what the node did unknown, and the node finishes a gesture
// whatever happens to the connection. Read as a refusal, the eviction printed
// no -op-id, and the only way on was a second gesture over every log the first
// one reached.
func TestAGateAnswerTheNodeDidNotWriteNamesItsOperation(t *testing.T) {
	for name, reply := range map[string]func(w http.ResponseWriter){
		"a gateway timeout's page": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("<html><body><h1>504 Gateway Time-out</h1></body></html>"))
		},
		"a bad gateway with a JSON body of its own": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"upstream closed the connection"}`))
		},
		"a 200 cut off part way through": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"node":"node-4","op_id":"01a0`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			node := newFakeRetentionNode(t)
			base := bootstrapForURL(t, node.server.URL)
			node.gateReply = reply

			_, stderr, err := cli(t, "retention", "evict", "node-4", base,
				"-confirm", "node-4")
			if err == nil {
				t.Fatal("a gesture whose answer never came from the node exited zero")
			}
			sent := node.lastGate().Get("op_id")
			if want := "-op-id " + sent; sent == "" || !strings.Contains(stderr, want) {
				t.Errorf("the gesture never says %q, so the only way on is a "+
					"second gesture:\n%s\n%v", want, stderr, err)
			}
		})
	}
}

// ONLY AN ANSWER CARRYING AN ENGINE ERROR CODE IS THE NODE'S REFUSAL. Every
// refusal the engine writes is JSON with an `error` code, so one without is
// something in front of it — and a refusal read as "no answer" would tell an
// operator a 409 that wrote nothing might have done what it refused.
func TestOnlyAnAnswerWithAnEngineCodeIsTheNodesRefusal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
		node   bool
	}{
		"an engine refusal":           {http.StatusConflict, `{"error":"eviction_refused","detail":"live"}`, true},
		"a drain":                     {http.StatusServiceUnavailable, `{"error":"draining"}`, true},
		"a gateway page":              {http.StatusGatewayTimeout, "<html>504</html>", false},
		"a 503 carrying no code":      {http.StatusServiceUnavailable, `{}`, false},
		"an empty 502":                {http.StatusBadGateway, "", false},
		"a 200 whose JSON is cut off": {http.StatusOK, `{"node":`, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client := &nodeClient{base: server.URL, token: "t", http: httpx.Client(nodeRequestTimeout)}
			var into map[string]any
			err := client.post(t.Context(), "/work/retention/evict/node-4", &into)
			var lost noAnswer
			if got := !errors.As(err, &lost); err == nil || got != tc.node {
				t.Errorf("%d %q = %v; the node's own answer %v, want %v", tc.status,
					tc.body, err, got, tc.node)
			}
		})
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
		"is 0", "below " + floor, "crewlet retention snapshots",
		"crewlet retention status"} {
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
	if !strings.Contains(stdout, "tracker: applied at CREWLET_TRACKER_LOG") ||
		!strings.Contains(stdout, "pages: applied at CREWLET_PAGES_LOG") {
		t.Errorf("the readmission did not report landing on both logs:\n%s", stdout)
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
