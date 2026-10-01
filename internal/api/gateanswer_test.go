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
	"github.com/crewlet/crewlet/internal/estate"
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
	// THE ESTATE MAP'S PART, which a gesture under a divided layout carries
	// beside its logs: an out that landed, and one the store did not answer.
	withMap := func(m engine.MapGate) engine.GateResult {
		r := result(engine.DomainGate{Outcome: statelog.OutcomeApplied, Position: at})
		r.Map = &m
		return r
	}
	return map[string]engine.GateResult{
		"map_out": withMap(engine.MapGate{Gesture: "out", Landed: true}),
		"map_unwritten": withMap(engine.MapGate{Gesture: "out",
			Err: engine.ErrEstateUnavailable}),
		"applied": result(engine.DomainGate{
			Outcome: statelog.OutcomeApplied, Position: at}),
		"pending": result(engine.DomainGate{
			Outcome: statelog.OutcomePending, Position: at}),
		"unknown": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown}),
		"unvouched": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown,
			Unvouched: true}),
		// A LOG THIS NODE DOES NOT SERVE, sent to a holder of its partition
		// that wrote it on this node's behalf: the answer names that node,
		// and so does any refusal or unknown it gave.
		"written_elsewhere": result(engine.DomainGate{Outcome: statelog.OutcomeApplied,
			Position: at, Writer: "node-q"}),
		"unvouched_elsewhere": result(engine.DomainGate{Outcome: statelog.OutcomeUnknown,
			Unvouched: true, Writer: "node-q"}),
		"log_full": result(engine.DomainGate{Err: fmt.Errorf("pages: %w",
			&statelog.Unavailable{Reason: statelog.ReasonLogFull,
				Detail: "the broker refused to store it"})}),
		"superseded": result(engine.DomainGate{Err: fmt.Errorf("pages: %w",
			&statelog.Unavailable{Reason: statelog.ReasonSuperseded,
				Detail: "a later gate record on node-4 has undone it"})}),
	}
}

// gateReadmitScenarios is a readmission's answers with the estate map's part —
// the in, which lifts an eviction's bar — rendered as readmissions: one that
// put the node back, and one whose every log took the node back while the map
// could not be written, which leaves it barred until the same operation is
// finished. A readmission carries a map part only under a divided layout, and
// the estate screen readmits a node its map bars, so these are that screen's
// fixtures as much as the gate dialog's.
func gateReadmitScenarios() map[string]engine.GateResult {
	gesture := statelog.DeriveOpID(time.UnixMilli(1_790_000_000_000), "readmit-node-4",
		"internal/api", "gate_answer.json")
	applied := func(domain, stream string, seq uint64) engine.DomainGate {
		return engine.DomainGate{Domain: domain, Stream: stream,
			OpID:     statelog.StepOpID(gesture, "readmit", domain+":node-4"),
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: stream, Generation: 1, Seq: seq}}
	}
	withMap := func(m engine.MapGate) engine.GateResult {
		return engine.GateResult{Node: "node-4", OpID: gesture, Map: &m,
			Domains: []engine.DomainGate{
				applied("tracker", "CREWLET_TRACKER_LOG", 918280007),
				applied("pages", "CREWLET_PAGES_LOG", 4415)}}
	}
	return map[string]engine.GateResult{
		"readmit_map_in": withMap(engine.MapGate{Gesture: "in", Landed: true}),
		"readmit_map_unwritten": withMap(engine.MapGate{Gesture: "in",
			Err: engine.ErrEstateUnavailable}),
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
			&engine.ReadmissionUnjudged{Node: "node-4", Log: "tracker@tracker.007",
				Err: fmt.Errorf("read its readmission bound: %w",
					&estate.ErrPartitionUnserved{Partition: "tracker.007"})}),
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
	for name, result := range gateReadmitScenarios() {
		answers[name] = api.RenderGate(false, result)
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

	// AN UNKNOWN THIS NODE CANNOT SETTLE SAYS SO, and is sent elsewhere:
	// the same gesture here answers the same way every time.
	unvouched := api.RenderGate(true, scenarios["unvouched"]).Domains[1]
	if !unvouched.Unvouched || unvouched.Position != nil ||
		!slices.Equal(unvouched.Actions, []statelog.GateAction{statelog.GateOtherNode}) {
		t.Errorf("the unvouched log rendered %+v, want unvouched, no position and "+
			"only another node", unvouched)
	}
	if pages.Unvouched {
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

// THE ESTATE MAP IS THE GESTURE'S OWN PART, AND ONLY WHERE THERE IS ONE.
//
// Under a divided layout an eviction takes the node out of the estate map; a
// map the gesture could not write leaves it unfinished however every log
// answered, with the same gesture again as the remedy — and a map that landed
// carries no remedy at all. Under layout 0 there is no map, and no key for one:
// every answer the routes give today is byte for byte what it was.
func TestTheGateAnswerCarriesTheMapsPartOnlyWhereThereIsOne(t *testing.T) {
	t.Parallel()
	scenarios := gateAnswerScenarios()
	unwritten := api.RenderGate(true, scenarios["map_unwritten"])
	if unwritten.Complete || unwritten.Map == nil {
		t.Fatalf("an unwritten map rendered complete %v, map %+v", unwritten.Complete, unwritten.Map)
	}
	if unwritten.Map.Landed || unwritten.Map.Error == "" ||
		!slices.Contains(unwritten.Map.Actions, statelog.GateRetrySameOp) || unwritten.Map.Hint == "" {
		t.Errorf("the unwritten map rendered %+v, want its error and the same gesture again",
			*unwritten.Map)
	}
	out := api.RenderGate(true, scenarios["map_out"])
	if !out.Complete || out.Map == nil || !out.Map.Landed || out.Map.Gesture != "out" ||
		out.Map.Actions != nil || out.Map.Hint != "" {
		t.Errorf("a landed map rendered complete %v, %+v", out.Complete, out.Map)
	}

	// A READMISSION'S IN IS ITS LAST PART: every log took the node back and
	// the map could not be written, so the node is still barred and the
	// gesture is unfinished — finished by the same operation, never a
	// fresh one.
	readmits := gateReadmitScenarios()
	stuck := api.RenderGate(false, readmits["readmit_map_unwritten"])
	if stuck.Evicted || stuck.Complete || stuck.Map == nil || stuck.Map.Gesture != "in" ||
		stuck.Map.Landed || !slices.Equal(stuck.Map.Actions,
		[]statelog.GateAction{statelog.GateRetrySameOp}) {
		t.Errorf("a readmission whose map was not written rendered %+v, map %+v",
			stuck, stuck.Map)
	}
	in := api.RenderGate(false, readmits["readmit_map_in"])
	if in.Evicted || !in.Complete || in.Map == nil || in.Map.Gesture != "in" ||
		!in.Map.Landed || in.Map.Actions != nil {
		t.Errorf("a readmission that put the node back rendered %+v, map %+v", in, in.Map)
	}
	raw, err := json.Marshal(api.RenderGate(true, scenarios["applied"]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"map"`) {
		t.Errorf("an answer with no map carries a key for one: %s", raw)
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
		// A LOG NO NODE SERVES is waited out — no retry serves it — and
		// anything else a judgement could not read is asked again, here
		// or through another node.
		"readmission_unjudged":          {statelog.GateWait},
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
