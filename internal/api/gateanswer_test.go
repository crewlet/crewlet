package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// gateAnswerGolden is the gate routes' rendering — every 200 shape and every
// refusal — committed, and read by the dashboard's own suite as its fixture:
// see [api.GateAnswer].
const gateAnswerGolden = "testdata/gate_answer.json"

// gateLogs is every log a node's gate gesture writes: each identity-claiming
// domain of the register, in its order.
//
// DERIVED FROM THE REGISTER rather than typed in each fake: the fakes and the
// golden stood for a node with two identity logs while every node writes three,
// so the dashboard's dialog and the command line were never shown the identity
// estate's line, and a domain the register gains would have stayed out of them
// too. [TestTheGateAnswerMatchesItsGoldenFile] holds the count.
func gateLogs() []statelog.Domain {
	var out []statelog.Domain
	for _, domain := range engine.Domains() {
		if domain.ClaimsIdentity() {
			out = append(out, domain)
		}
	}
	return out
}

// appliedOn is the i-th of [gateLogs] applied by one gesture on node, under the
// operation id the engine derives for that log, at a position of its own: the
// first log's at the fixtures' usual height, every later one's a multiple of
// the knowledge base's.
func appliedOn(i int, domain statelog.Domain, gesture, verb, node string) engine.DomainGate {
	stream := domain.Stream().Name
	seq := uint64(918280002)
	if i > 0 {
		seq = uint64(4410 * i)
	}
	return engine.DomainGate{Domain: domain.Name(), Stream: stream,
		OpID:     statelog.StepOpID(gesture, verb, domain.Name()+":"+node),
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: stream, Generation: 1, Seq: seq}}
}

// gateAnswerScenarios is one gesture per shape a log's answer can take, each
// over every identity-claiming log a node writes ([gateLogs]): every log
// applied but the SECOND, which takes the scenario's answer — a gesture that
// reached the logs before it and the logs after it and not this one is what
// every scenario but `applied` describes, and an index the dashboard's suite
// reads by.
//
// The ids are the engine's own grammar and DERIVED, so the file is stable: a
// dashboard fixture carrying `op-7` is an id no node sends and the route
// refuses sent back, and a test of a Finish button over it proves nothing.
func gateAnswerScenarios() map[string]engine.GateResult {
	gesture := statelog.DeriveOpID(time.UnixMilli(1_790_000_000_000), "evict-node-4",
		"internal/api", "gate_answer.json")
	logs := gateLogs()
	result := func(answer engine.DomainGate) engine.GateResult {
		domains := make([]engine.DomainGate, 0, len(logs))
		for i, domain := range logs {
			applied := appliedOn(i, domain, gesture, "evict", "node-4")
			if i == 1 {
				answer.Domain, answer.Stream, answer.OpID =
					applied.Domain, applied.Stream, applied.OpID
				applied = answer
			}
			domains = append(domains, applied)
		}
		return engine.GateResult{Node: "node-4", OpID: gesture, Domains: domains}
	}
	at := appliedOn(1, logs[1], gesture, "evict", "node-4").Position
	return map[string]engine.GateResult{
		"applied": result(engine.DomainGate{
			Outcome: statelog.OutcomeApplied, Position: at}),
		"pending": result(engine.DomainGate{
			Outcome: statelog.OutcomePending, Position: at}),
		"unknown": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown}),
		"unvouched": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown,
			Unvouched: true}),
		"log_full": result(engine.DomainGate{Err: fmt.Errorf("%s: %w", logs[1].Name(),
			&statelog.Unavailable{Reason: statelog.ReasonLogFull,
				Detail: "the broker refused to store it"})}),
		"superseded": result(engine.DomainGate{Err: fmt.Errorf("%s: %w", logs[1].Name(),
			&statelog.Unavailable{Reason: statelog.ReasonSuperseded,
				Detail: "a later gate record on node-4 has undone it"})}),
	}
}

// gateRefusalScenarios is every refusal the routes answer before anything is
// written, wrapped the way the engine wraps them.
func gateRefusalScenarios() map[string]error {
	live := statelog.PermitEviction("node-4", []statelog.Presence{{NodeID: "node-4"}}, false)
	return map[string]error{
		"eviction_refused": fmt.Errorf("engine: evict node node-4: %w", live),
		"not_publishing": fmt.Errorf("%w: an eviction appends a record to the "+
			"state log, and this node runs in seal mode", engine.ErrNotPublishing),
		"eviction_unjudged": &engine.GateUnjudged{Node: "node-4",
			Err: errors.New("list the live nodes: coordination is unreachable")},
		"readmission_unjudged": fmt.Errorf("engine: readmit node node-4: %w",
			&engine.ReadmissionUnjudged{Node: "node-4", Log: "tracker",
				Err: errors.New("read its readmission bound: engine: read tracker's " +
					"first surviving sequence: the stream has no leader")}),
		"readmission_unjudged_register": fmt.Errorf("engine: readmit node node-4: %w",
			&engine.ReadmissionUnjudged{Node: "node-4",
				Err: errors.New("read the positions register: coordination is unreachable")}),
		"readmission_refused": fmt.Errorf("engine: readmit node node-4: %w",
			&statelog.ReadmissionRefusal{NodeID: "node-4", Domain: "tracker",
				Published: true, Generation: 1, Seq: 1200,
				ReportedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
				Bound: statelog.ReadmissionBound{Domain: "tracker", Generation: 1,
					Floor: 9000, First: 8800}}),
	}
}

// refusalOpID is the operation a refusal scenario was sent under: derived, in
// the engine's own grammar, for the reason [gateAnswerScenarios] gives.
func refusalOpID(name string) string {
	verb := "evict"
	if strings.HasPrefix(name, "readmission") {
		verb = "readmit"
	}
	return statelog.DeriveOpID(time.UnixMilli(1_790_000_000_000), verb+"-node-4",
		"internal/api", "gate_answer.json", name)
}

// goldenRefusal is one refusal as the golden records it.
type goldenRefusal struct {
	Status int            `json:"status"`
	Body   map[string]any `json:"body"`
}

// renderGateScenarios is the golden's content: every scenario, rendered by the
// route's own renderer.
func renderGateScenarios(t *testing.T) []byte {
	t.Helper()
	answers := map[string]api.GateAnswer{}
	for name, result := range gateAnswerScenarios() {
		answers[name] = api.RenderGate(true, result)
	}
	refusals := map[string]goldenRefusal{}
	for name, err := range gateRefusalScenarios() {
		refusal, ok := api.RenderGateRefusal("node-4", refusalOpID(name), err)
		if !ok {
			t.Fatalf("%s is not rendered as a refusal", name)
		}
		refusals[name] = goldenRefusal{Status: refusal.Status, Body: refusal.Envelope()}
	}
	out, err := json.MarshalIndent(map[string]any{
		"answers": answers, "refusals": refusals,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode the gate answers: %v", err)
	}
	return append(out, '\n')
}

// THE GATE ANSWER'S SHAPE IS PINNED IN ONE FILE BOTH SIDES READ.
//
// The dashboard's evict dialog read a top-level `outcome` for as long as the
// route had stopped writing one, so every gesture — one applied on both logs
// included — rendered as "no acknowledgement, this is the case to retry", and
// its suite passed on fixtures it had typed itself. The golden is what the
// route renders; the dashboard's suite loads the same file, so a change to the
// rendering is a failing test until the golden is regenerated (`make
// gate-answer`) and a failing Vitest suite until the dialog follows it.
func TestTheGateAnswerMatchesItsGoldenFile(t *testing.T) {
	t.Parallel()
	// A FLOOR, so a register that lost a domain reads as a failure here
	// rather than as a golden that quietly lost its line.
	if logs := gateLogs(); len(logs) < 6 {
		t.Fatalf("the register holds %d identity-claiming log(s), fewer than the "+
			"tracker's, the knowledge base's, the identity estate's, the "+
			"artifacts log's, the artifact datastore's and the assistant's", len(logs))
	}
	got := renderGateScenarios(t)
	if os.Getenv("CREWLET_REGENERATE_GATE_ANSWER") == "1" {
		if err := os.MkdirAll(filepath.Dir(gateAnswerGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gateAnswerGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(gateAnswerGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make gate-answer` writes it", gateAnswerGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the gate answer no longer matches %s. Run `make gate-answer`, read "+
			"the diff, and follow it in the dashboard's gate dialog — its suite "+
			"loads this file.\n--- rendered now ---\n%s", gateAnswerGolden, got)
	}
}

// AN UNKNOWN LOG CARRIES NO POSITION, AND A FINISHED ONE NO REMEDY.
//
// The golden pins the bytes; this names the two properties a reader of them is
// most likely to break — the command line's own fixture had already started
// sending a zero position for an unknown log that no real node sends.
func TestTheGateAnswerOmitsWhatALogDidNotSay(t *testing.T) {
	t.Parallel()
	scenarios := gateAnswerScenarios()
	for name, result := range scenarios {
		for _, d := range api.RenderGate(true, result).Domains {
			for _, flag := range []string{"-force", "-op-id", "-url", "crewlet "} {
				if strings.Contains(d.Hint, flag) {
					t.Errorf("%s: %s's hint names %q, which only the command line "+
						"has — the dashboard renders it word for word: %s",
						name, d.Domain, flag, d.Hint)
				}
			}
		}
	}
	unknown := api.RenderGate(true, scenarios["unknown"])
	if unknown.Complete {
		t.Error("a gesture with an unknown log reads complete")
	}
	missed := unknown.Domains[1]
	if missed.Position != nil {
		t.Errorf("the unknown log carries position %+v — a zero position reads as a "+
			"record at the log's origin", *missed.Position)
	}
	if len(missed.Actions) == 0 || missed.Actions[0] != statelog.GateRetrySameOp {
		t.Errorf("the unknown log offers %v, want the same gesture again first",
			missed.Actions)
	}
	// EVERY OTHER LOG, before the missed one and after it, is applied at a
	// position of its own and carries no remedy.
	if len(unknown.Domains) != len(gateLogs()) {
		t.Errorf("the gesture answers for %d log(s), want every identity-claiming "+
			"one (%d)", len(unknown.Domains), len(gateLogs()))
	}
	for i, applied := range unknown.Domains {
		if i == 1 {
			continue
		}
		if applied.Position == nil || applied.Position.Seq == 0 {
			t.Errorf("the applied log %s's position is %+v", applied.Domain, applied.Position)
		}
		if applied.Actions != nil || applied.Hint != "" {
			t.Errorf("the applied log %s carries a remedy: %v %q", applied.Domain,
				applied.Actions, applied.Hint)
		}
	}

	// AN UNKNOWN THIS NODE CANNOT SETTLE SAYS SO, and is sent elsewhere:
	// the same gesture here answers the same way every time.
	unvouched := api.RenderGate(true, scenarios["unvouched"]).Domains[1]
	if !unvouched.Unvouched || unvouched.Position != nil ||
		!slices.Equal(unvouched.Actions, []statelog.GateAction{statelog.GateOtherNode}) {
		t.Errorf("the unvouched log rendered %+v, want unvouched, no position and "+
			"only another node", unvouched)
	}
	if missed.Unvouched {
		t.Error("a lost acknowledgement renders as unvouched")
	}

	full := api.RenderGate(true, scenarios["log_full"]).Domains[1]
	if full.Outcome != "" || full.Position != nil || full.Reason != statelog.ReasonLogFull {
		t.Errorf("the full log rendered %+v, want no outcome, no position and "+
			"reason log_full", full)
	}
	if len(full.Actions) != 1 || full.Actions[0] != statelog.GateSetCapacity {
		t.Errorf("the full log offers %v, want only raising its ceiling — no "+
			"retry refills a spent gate reserve", full.Actions)
	}
}

// EVERY REFUSAL SAYS WHAT TO DO AS AN ACTION, AND FORCE WHERE FORCE IS THE WAY.
//
// A live node and an unreadable lease listing are the two evictions only force
// gets past, and the dashboard could do neither: its dialog rendered a hint
// telling the operator to pass -force, which a browser cannot. The action is
// what a surface with no flags offers a control for.
func TestEveryGateRefusalCarriesItsActions(t *testing.T) {
	t.Parallel()
	want := map[string][]statelog.GateAction{
		"eviction_refused":    {statelog.GateWait, statelog.GateForce},
		"eviction_unjudged":   {statelog.GateRetrySameOp, statelog.GateForce},
		"readmission_refused": {statelog.GateWait},
		"not_publishing":      {statelog.GateWait},
		// WHATEVER A JUDGEMENT COULD NOT READ — a log's bound or the
		// fleet's register — is asked again, here or through another node.
		"readmission_unjudged":          {statelog.GateRetrySameOp, statelog.GateOtherNode},
		"readmission_unjudged_register": {statelog.GateRetrySameOp, statelog.GateOtherNode},
	}
	for name, err := range gateRefusalScenarios() {
		refusal, ok := api.RenderGateRefusal("node-4", refusalOpID(name), err)
		if !ok {
			t.Fatalf("%s is not rendered as a refusal", name)
		}
		// AND THE OPERATION IT WAS SENT UNDER, which is what "the same
		// request with the answer's op_id" reads — a refusal offering
		// `retry_same_op` without one named a value it did not hold.
		body := refusal.Envelope()
		if got, _ := body["op_id"].(string); got != refusalOpID(name) {
			t.Errorf("%s carries op_id %q, want the one it was sent under %q",
				name, got, refusalOpID(name))
		}
		got, _ := body["actions"].([]statelog.GateAction)
		if !slices.Equal(got, want[name]) {
			t.Errorf("%s offers %v, want %v", name, got, want[name])
		}
		if hint, _ := body["hint"].(string); hint == "" ||
			strings.Contains(hint, "-force") || strings.Contains(hint, "-op-id") ||
			strings.Contains(hint, "-url") {
			t.Errorf("%s's hint %q is empty or names a command-line flag — the "+
				"dashboard renders it word for word", name, hint)
		}
	}
	if _, ok := api.RenderGateRefusal("node-4", refusalOpID("x"),
		errors.New("unreachable")); ok {
		t.Error("an ordinary failure was rendered as a refusal rather than a 500")
	}
}

// EVERY GATE REFUSAL IS IN THE ENGINE'S ENVELOPE, AND ONLY THE ONE WAITING CAN
// CLEAR SAYS WHEN TO COME BACK.
//
// The refusals were bare maps with no `message`, so the dialog had no sentence
// of the engine's for any of them — and the one 503, a lease listing this node
// could not read, said `retry_same_op` in its body while its header, carrying
// no Retry-After, said "do not come back". Asserted on the wire, through the
// route's own writer, because the header is not in the golden.
func TestEveryGateRefusalIsInTheEnvelope(t *testing.T) {
	t.Parallel()
	for name, err := range gateRefusalScenarios() {
		refusal, ok := api.RenderGateRefusal("node-4", refusalOpID(name), err)
		if !ok {
			t.Fatalf("%s is not rendered as a refusal", name)
		}
		if !refusal.Code.Valid() {
			t.Errorf("%s answers %q, a code not on httpjson's table", name, refusal.Code)
		}
		rec := httptest.NewRecorder()
		refusal.Write(rec)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: the refusal is not JSON: %v", name, err)
		}
		// A SCENARIO IS NAMED BY ITS CODE, qualified where one code has two
		// cases: `readmission_unjudged_register` is the readmission whose
		// fleet-wide read failed rather than one log's.
		code := string(refusal.Code)
		if name != code && !strings.HasPrefix(name, code+"_") {
			t.Errorf("%s answers %q, which is not the code it is named by", name, code)
		}
		if rec.Code != refusal.Status || body["error"] != code ||
			body["message"] != refusal.Code.Message() {
			t.Errorf("%s answered %d %v, want %d with the table's sentence",
				name, rec.Code, body, refusal.Status)
		}
		retry := rec.Header().Get("Retry-After")
		switch unavailable := refusal.Status == http.StatusServiceUnavailable; {
		case unavailable && retry == "":
			t.Errorf("%s is a 503 whose remedy is asking again, and carries no "+
				"Retry-After — which tells a client not to", name)
		case !unavailable && retry != "":
			t.Errorf("%s is a %d carrying Retry-After %s, which no wait clears",
				name, refusal.Status, retry)
		}
	}
}
