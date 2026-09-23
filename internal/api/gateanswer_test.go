package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// gateAnswerScenarios is one gesture per shape a log's answer can take, each
// over the two identity-claiming logs a real node runs.
//
// The ids are the engine's own grammar and DERIVED, so the file is stable: a
// dashboard fixture carrying `op-7` is an id no node sends and the route
// refuses sent back, and a test of a Finish button over it proves nothing.
func gateAnswerScenarios() map[string]engine.GateResult {
	gesture := statelog.DeriveOpID(time.UnixMilli(1_790_000_000_000), "evict-node-4",
		"internal/api", "gate_answer.json")
	step := func(domain string) string {
		return statelog.StepOpID(gesture, "evict", domain+":node-4")
	}
	tracker := engine.DomainGate{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG",
		OpID: step("tracker"), Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1,
			Seq: 918280002}}
	pages := func(d engine.DomainGate) engine.DomainGate {
		d.Domain, d.Stream, d.OpID = "pages", "CREWLET_PAGES_LOG", step("pages")
		return d
	}
	at := statelog.Position{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 4410}
	result := func(pagesAnswer engine.DomainGate) engine.GateResult {
		return engine.GateResult{Node: "node-4", OpID: gesture,
			Domains: []engine.DomainGate{tracker, pages(pagesAnswer)}}
	}
	return map[string]engine.GateResult{
		"applied": result(engine.DomainGate{
			Outcome: statelog.OutcomeApplied, Position: at}),
		"pending": result(engine.DomainGate{
			Outcome: statelog.OutcomePending, Position: at}),
		"unknown": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown}),
		"log_full": result(engine.DomainGate{Err: fmt.Errorf("pages: %w",
			&statelog.Unavailable{Reason: statelog.ReasonLogFull,
				Detail: "the broker refused to store it"})}),
		"superseded": result(engine.DomainGate{Err: fmt.Errorf("pages: %w",
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
		"eviction_unjudged": &engine.GateUnjudged{Node: "node-4",
			Err: errors.New("list the live nodes: coordination is unreachable")},
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
		refusals[name] = goldenRefusal{Status: refusal.Status, Body: refusal.Body}
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
	pages := unknown.Domains[1]
	if pages.Position != nil {
		t.Errorf("the unknown log carries position %+v — a zero position reads as a "+
			"record at the log's origin", *pages.Position)
	}
	if len(pages.Actions) == 0 || pages.Actions[0] != statelog.GateRetrySameOp {
		t.Errorf("the unknown log offers %v, want the same gesture again first",
			pages.Actions)
	}
	tracker := unknown.Domains[0]
	if tracker.Position == nil || tracker.Position.Seq != 918280002 {
		t.Errorf("the applied log's position is %+v", tracker.Position)
	}
	if tracker.Actions != nil || tracker.Hint != "" {
		t.Errorf("the applied log carries a remedy: %v %q", tracker.Actions, tracker.Hint)
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
	}
	for name, err := range gateRefusalScenarios() {
		refusal, ok := api.RenderGateRefusal("node-4", refusalOpID(name), err)
		if !ok {
			t.Fatalf("%s is not rendered as a refusal", name)
		}
		// AND THE OPERATION IT WAS SENT UNDER, which is what "the same
		// request with the answer's op_id" reads — a refusal offering
		// `retry_same_op` without one named a value it did not hold.
		if got, _ := refusal.Body["op_id"].(string); got != refusalOpID(name) {
			t.Errorf("%s carries op_id %q, want the one it was sent under %q",
				name, got, refusalOpID(name))
		}
		got, _ := refusal.Body["actions"].([]statelog.GateAction)
		if !slices.Equal(got, want[name]) {
			t.Errorf("%s offers %v, want %v", name, got, want[name])
		}
		if hint, _ := refusal.Body["hint"].(string); hint == "" ||
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
