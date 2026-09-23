package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeBackupRegister records the point the acknowledgement wrote.
type fakeBackupRegister struct {
	point coord.BackupPoint
	calls int
}

func (f *fakeBackupRegister) PutBackupPoint(_ context.Context, p coord.BackupPoint) error {
	f.calls++
	f.point = p
	return nil
}

// fakeStateLog answers for the domains one node is running, keyed by STREAM —
// which is the key space an acknowledgement is filed under, and the one
// neither the published floors nor the positions register has.
//
// PER-STREAM GENERATIONS, because that is the estate a reanchor produces: the
// verb names one stream, so a fleet that has re-anchored its tracker and not
// its pages log stands at two different generations at once.
type fakeStateLog struct {
	generations map[string]uint32
	asked       string

	// unreadable is the broker's answer to a LIVE read of a stream's
	// instant, nil while it answers.
	unreadable error

	// view is the case the status reports, beside the generation above.
	view engine.ReanchorView
}

func (f *fakeStateLog) ReanchorStatus(_ context.Context, stream string) (
	engine.ReanchorView, error) {

	generation, err := f.StreamGeneration(stream)
	if err != nil {
		return engine.ReanchorView{}, err
	}
	if f.unreadable != nil {
		return engine.ReanchorView{}, fmt.Errorf("engine: read %s's creation "+
			"instant: %w", stream, f.unreadable)
	}
	view := f.view
	view.CreatedAt, view.Generation = time.Unix(1700000000, 0).UTC(), generation
	return view, nil
}

func (f *fakeStateLog) StreamGeneration(stream string) (uint32, error) {
	f.asked = stream
	generation, runs := f.generations[stream]
	if !runs {
		return 0, fmt.Errorf("%w: %q", engine.ErrUnknownStream, stream)
	}
	return generation, nil
}

func (f *fakeStateLog) Reanchor(context.Context, engine.ReanchorRequest) (
	statelog.ReanchorPlan, error) {

	return statelog.ReanchorPlan{}, errors.New("not exercised here")
}

func (f *fakeStateLog) SetCapacity(context.Context, engine.CapacityRequest) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) AbandonCapacity(context.Context, string) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) ExcludeParticipant(context.Context, string, string) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) CapacityStatus(context.Context, string) (
	coord.MaintenanceOperation, []coord.MaintenanceAck, []coord.Admission, bool, error) {

	return coord.MaintenanceOperation{}, nil, nil, false, errors.New("not exercised here")
}

func (f *fakeStateLog) Mode() statelog.MaintenanceMode { return statelog.ModeNormal }

// ackApp mounts the retention routes over the two seams the ack needs.
func ackApp(t *testing.T, register *fakeBackupRegister, log *fakeStateLog) *api.App {
	t.Helper()
	b := closedPosture()
	return newApp(t, api.Options{Bootstrap: &b, Retention: register, Capacity: log})
}

// postAck runs one authenticated acknowledgement.
func postAck(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.NewDecoder(rec.Result().Body).Decode(&body)
	return rec.Code, body
}

// AN ACKNOWLEDGEMENT IS STAMPED WITH THE NAMED STREAM'S OWN GENERATION.
//
// The point is filed under a stream and the trim looks its reach up by stream,
// but the fleet's registers are keyed by DOMAIN and carry no stream name at
// all — so the route used to stamp the point with the first row of whichever
// register answered. On an estate where one domain has been re-anchored and
// another has not, that is a different log's number: the trim discards a point
// BELOW its generation and reads one at or above it as covering the log, so a
// neighbour's higher generation counts, at a sequence from a number space that
// is not this log's, and the trim deletes records the copy does not contain.
func TestAnAcknowledgementCarriesTheNamedStreamsOwnGeneration(t *testing.T) {
	t.Parallel()
	register := &fakeBackupRegister{}
	// The tracker log is listed first and stands one reanchor behind the
	// pages log, which is exactly the estate the old first-row read got
	// wrong.
	node := &fakeStateLog{generations: map[string]uint32{
		"CREWLET_TRACKER_LOG": 3,
		"CREWLET_PAGES_LOG":   7,
	}}

	code, body := postAck(t, ackApp(t, register, node),
		"/work/retention/ack?stream=CREWLET_PAGES_LOG&position=918100000")
	if code != http.StatusOK {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	if node.asked != "CREWLET_PAGES_LOG" {
		t.Errorf("the generation was read for %q rather than the stream the "+
			"caller named", node.asked)
	}
	if body["generation"] != float64(7) {
		t.Errorf("the answer echoes generation %v, so an operator cannot see "+
			"which number space was acknowledged", body["generation"])
	}
	at, covers := register.point.Streams["CREWLET_PAGES_LOG"]
	if !covers {
		t.Fatalf("the point covers %v rather than the stream named",
			register.point.Streams)
	}
	if at.Generation != 7 {
		t.Errorf("the point was stamped at generation %d, which is another "+
			"log's number space — the trim reads a point at or above its own "+
			"generation as covering the log and would delete records this "+
			"backup does not contain", at.Generation)
	}
	if at.Seq != 918100000 || at.Stream != "CREWLET_PAGES_LOG" {
		t.Errorf("the point reaches %s@%d", at.Stream, at.Seq)
	}
	if !register.point.Verified || register.point.Owner != coord.OperatorBackupOwner {
		t.Errorf("the point is owned by %q (verified: %v), and under "+
			"backup_floor: operator only a verified operator point counts",
			register.point.Owner, register.point.Verified)
	}
}

// A STREAM THIS NODE DOES NOT RUN IS REFUSED, rather than acknowledged at some
// other log's generation.
//
// The registers answered a mistyped stream name as confidently as a real one,
// so the acknowledgement came back 200 with a generation — and the point it
// wrote was one nothing would ever read.
func TestAnAcknowledgementNamingAnUnknownStreamIsRefused(t *testing.T) {
	t.Parallel()
	register := &fakeBackupRegister{}
	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3}}

	code, body := postAck(t, ackApp(t, register, node),
		"/work/retention/ack?stream=CREWLET_TRAKCER_LOG&position=918100000")
	// 404 rather than 503: nothing here is transient, and a 503 tells an
	// operator who typed the name wrong to send the identical request
	// again in a minute.
	if code != http.StatusNotFound {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	if body["error"] != "unknown_stream" {
		t.Errorf("the refusal reads %q, and the CLI prints only this field",
			body["error"])
	}
	if register.calls != 0 {
		t.Errorf("a point was written anyway: %v", register.point)
	}
}

// fakeNodeGate answers the two gestures with what it was built with and
// remembers what it was asked.
type fakeNodeGate struct {
	readmit error
	evict   error

	// result is what a gesture past the judgement answers; empty is every
	// log applied.
	result *engine.GateResult

	asked []engine.GateRequest
}

func (f *fakeNodeGate) answer(req engine.GateRequest, refusal error) (engine.GateResult, error) {
	f.asked = append(f.asked, req)
	if refusal != nil {
		return engine.GateResult{}, refusal
	}
	if f.result != nil {
		out := *f.result
		out.Node, out.OpID = req.Node, req.OpID
		return out, nil
	}
	return engine.GateResult{Node: req.Node, OpID: req.OpID, Domains: []engine.DomainGate{
		{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG", OpID: req.OpID + ".evict.tracker",
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 12}},
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: req.OpID + ".evict.pages",
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Seq: 7}},
	}}, nil
}

func (f *fakeNodeGate) Evict(_ context.Context, req engine.GateRequest) (engine.GateResult, error) {
	return f.answer(req, f.evict)
}

func (f *fakeNodeGate) Readmit(_ context.Context, req engine.GateRequest) (engine.GateResult, error) {
	return f.answer(req, f.readmit)
}

// A READMISSION REFUSED BELOW THE FLOOR IS A 409 CARRYING BOTH NUMBERS, and a
// failure stays a failure.
//
// The refusal is the answer the operator documentation promises, and `crewlet
// retention readmit` prints the error, the detail and the hint beneath the
// status. Reported as the route's generic `500 gate_failed`, it read as an
// engine fault where there is a node still catching up — and the numbers that
// are the whole reason sat unlabelled in a wrapped error string.
func TestAReadmissionBelowTheFloorIsRefusedAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	refusal := &statelog.ReadmissionRefusal{
		NodeID: "node-4", Domain: "tracker", Published: true,
		Generation: 2, Seq: 1_200,
		Bound: statelog.ReadmissionBound{Domain: "tracker", Generation: 2,
			Floor: 9_000, First: 8_800},
	}
	gate := &fakeNodeGate{readmit: fmt.Errorf("engine: readmit node node-4: %w", refusal)}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})

	code, body := postAck(t, a, "/work/retention/readmit/node-4?confirm=node-4")
	if code != http.StatusConflict {
		t.Fatalf("a refused readmission answered %d: %v", code, body)
	}
	if body["error"] != "readmission_refused" {
		t.Errorf("error = %v, want readmission_refused", body["error"])
	}
	detail, _ := body["detail"].(string)
	if detail != refusal.Error() {
		t.Errorf("detail = %q, want the refusal's own sentence %q", detail, refusal.Error())
	}
	if hint, _ := body["hint"].(string); hint != refusal.Remedy() {
		t.Errorf("hint = %q, want the refusal's remedy", hint)
	}
	for field, want := range map[string]any{
		"node": "node-4", "domain": "tracker", "position": float64(1_200),
		"floor": float64(9_000), "first_seq": float64(8_800), "published": true,
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %v", field, body[field], want)
		}
	}

	// AND A JUDGEMENT NOBODY COULD MAKE IS NOT A REFUSAL OF THE NODE.
	gate.readmit = errors.New("engine: read the positions register: unreachable")
	if code, body := postAck(t, a, "/work/retention/readmit/node-4?confirm=node-4"); code !=
		http.StatusInternalServerError || body["error"] != "gate_failed" {
		t.Fatalf("an unreadable register answered %d %v, want 500 gate_failed", code, body)
	}
}

// AN EVICTION OF A NODE STILL HOLDING ITS PRESENCE LEASE IS A 409, and
// `force=true` is what the engine is asked to override it with — never
// assumed, and never sent on a readmission.
//
// The judgement existed and nothing called it: the route evicted whatever node
// id it was given, so a mistyped id took a running machine out of the fleet.
func TestAnEvictionOfALiveNodeIsRefusedAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	refusal := &statelog.EvictionRefusal{NodeID: "node-4", Detail: "it holds a live presence lease"}
	gate := &fakeNodeGate{evict: fmt.Errorf("engine: evict node node-4: %w", refusal)}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})

	code, body := postAck(t, a, "/work/retention/evict/node-4?confirm=node-4")
	if code != http.StatusConflict || body["error"] != "eviction_refused" {
		t.Fatalf("a refused eviction answered %d %v, want 409 eviction_refused", code, body)
	}
	if hint, _ := body["hint"].(string); hint != refusal.Remedy() {
		t.Errorf("hint = %q, want the refusal's remedy %q", hint, refusal.Remedy())
	}
	if len(gate.asked) != 1 || gate.asked[0].Force {
		t.Fatalf("the engine was asked %+v, want one unforced eviction", gate.asked)
	}

	gate.evict = nil
	if code, body := postAck(t, a,
		"/work/retention/evict/node-4?confirm=node-4&force=true"); code != http.StatusOK {
		t.Fatalf("a forced eviction answered %d %v", code, body)
	}
	if !gate.asked[1].Force {
		t.Fatal("force=true never reached the engine")
	}
	if code, _ := postAck(t, a,
		"/work/retention/readmit/node-4?confirm=node-4&force=true"); code != http.StatusOK ||
		gate.asked[2].Force {
		t.Fatalf("a readmission carried force (%+v), which it has no meaning for", gate.asked[2])
	}
}

// THE GESTURE ANSWERS PER LOG, NAMES WHO RAN IT, AND CARRIES THE OPERATION ID
// A RETRY FINISHES IT WITH.
//
// An eviction is a record on every identity-claiming log, and the logs answer
// independently — so the route reports each one's three-valued outcome, or the
// refusal that stopped it, and `complete` says whether every log holds the
// record. A gesture that reached the tracker's log and not the pages log is a
// 200 with `complete: false`, not a failure: the tracker's record stands, and
// the same request sent again with the `op_id` it answered with is what writes
// the rest.
func TestTheGateAnswersPerLogAndCarriesItsOperation(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	gate := &fakeNodeGate{result: &engine.GateResult{Domains: []engine.DomainGate{
		{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG", OpID: "op-1.evict.tracker",
			Outcome:  statelog.OutcomePending,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 41}},
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: "op-1.evict.pages",
			Err: fmt.Errorf("pages: %w", &statelog.Unavailable{
				Reason: statelog.ReasonLogFull, Detail: "the broker refused"})},
	}}}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})
	op := statelog.NewOpID(time.Now().Add(-time.Minute), "evict-node-4")

	code, body := postAck(t, a, "/work/retention/evict/node-4?confirm=node-4&op_id="+
		url.QueryEscape(op))
	if code != http.StatusOK {
		t.Fatalf("a partial gesture answered %d %v, want 200 — the logs that "+
			"answered hold their record", code, body)
	}
	if body["complete"] != false || body["op_id"] != op {
		t.Fatalf("complete = %v, op_id = %v, want false and the caller's own %s",
			body["complete"], body["op_id"], op)
	}
	if len(gate.asked) != 1 || gate.asked[0].OpID != op || gate.asked[0].By != "founder" {
		t.Fatalf("the engine was asked %+v, want %s run by the token's operator "+
			"founder — every log's record names who ran it", gate.asked, op)
	}
	domains, _ := body["domains"].([]any)
	if len(domains) != 2 {
		t.Fatalf("domains = %v, want one entry per log", body["domains"])
	}
	tracker, _ := domains[0].(map[string]any)
	pages, _ := domains[1].(map[string]any)
	if tracker["outcome"] != "pending" || tracker["op_id"] != "op-1.evict.tracker" {
		t.Errorf("the tracker's entry is %v, want its pending outcome under its "+
			"own derived operation", tracker)
	}
	if position, _ := tracker["position"].(map[string]any); position["seq"] != float64(41) {
		t.Errorf("the tracker's entry carries position %v, want seq 41", tracker["position"])
	}
	if _, has := pages["outcome"]; has || pages["reason"] != "log_full" ||
		pages["error"] == nil {
		t.Errorf("the pages entry is %v, want no outcome, reason log_full and "+
			"the error — a refusal is not one of the three outcomes", pages)
	}

	// AND WITHOUT ONE, A FRESH OPERATION — which it answers with, because a
	// retry needs it.
	gate.result = nil
	code, body = postAck(t, a, "/work/retention/evict/node-4?confirm=node-4")
	if code != http.StatusOK || body["complete"] != true {
		t.Fatalf("a complete gesture answered %d %v", code, body)
	}
	if opID, _ := body["op_id"].(string); opID == "" || opID != gate.asked[1].OpID {
		t.Fatalf("the answer's op_id %v is not the one the engine ran, %q",
			body["op_id"], gate.asked[1].OpID)
	}
	if _, minted := statelog.OpMintedAt(gate.asked[1].OpID); !minted {
		t.Errorf("the route minted %q, which carries no mint instant — every "+
			"log's id derived from it would be refused on a swept ledger",
			gate.asked[1].OpID)
	}
}

// A NODE RUNNING NO STATE LOG ANSWERS 503 NAMING THAT, rather than 404 —
// the route exists on this build, and telling an operator it does not sends
// them looking for a version mismatch.
func TestAGateOnANodeWithNoStateLogSaysSo(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})
	for _, verb := range []string{"evict", "readmit"} {
		code, body := postAck(t, a, "/work/retention/"+verb+"/node-4?confirm=node-4")
		if code != http.StatusServiceUnavailable || body["error"] != "no_state_log" {
			t.Errorf("%s on a node with no state log answered %d %v, want 503 "+
				"no_state_log", verb, code, body)
		}
	}
}

// THE REANCHOR STATUS TELLS A NAME NOBODY RUNS FROM A LOG NOBODY COULD READ.
//
// The instant it answers is read LIVE from the broker — it is the one the
// confirmation is checked against — so a broker that did not answer is a
// failure of its own: worth retrying, and nothing to do with the name. Folded
// into `unknown_stream`, it sent an operator looking for a typo in a stream
// name that was right. And the acknowledgement route, which only needs the
// generation, must not depend on that broker read at all.
func TestTheReanchorStatusTellsAnUnknownStreamFromAnUnreadableOne(t *testing.T) {
	t.Parallel()
	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_PAGES_LOG": 1}}
	register := &fakeBackupRegister{}
	a := ackApp(t, register, node)
	get := func(path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.NewDecoder(rec.Result().Body).Decode(&body)
		return rec.Code, body
	}

	if code, body := get("/work/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["created_at"] == nil || body["generation"] != float64(1) {
		t.Fatalf("a readable stream answered %d: %v", code, body)
	}
	// THE CASE A REANCHOR WOULD ANSWER is in the same answer, so the
	// operator confirms knowing whether the log is followed from its first
	// record or from its end — and, with no case, why there is nothing to do.
	node.view = engine.ReanchorView{Case: statelog.ReanchorRestored, Cursor: 7_000}
	if code, body := get("/work/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["case"] != "restored" || body["cursor"] != float64(7_000) {
		t.Fatalf("a restored stream's status answered %d: %v, want the restored "+
			"case at 7000", code, body)
	}
	node.view = engine.ReanchorView{Refusal: "nothing to re-anchor: the applier follows it"}
	if code, body := get("/work/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["case"] != nil || body["nothing_to_reanchor"] != node.view.Refusal {
		t.Fatalf("a stream with nothing to re-anchor answered %d: %v, want no case "+
			"and the reason", code, body)
	}
	node.view = engine.ReanchorView{}
	if code, body := get("/work/retention/reanchor?stream=CREWLET_PAGSE_LOG"); code != http.StatusNotFound ||
		body["error"] != "unknown_stream" {
		t.Fatalf("a stream this node does not run answered %d: %v", code, body)
	}

	node.unreadable = errors.New("nats: timeout")
	code, body := get("/work/retention/reanchor?stream=CREWLET_PAGES_LOG")
	if code != http.StatusServiceUnavailable || body["error"] != "stream_unreadable" {
		t.Fatalf("a stream the broker did not answer for answered %d: %v — "+
			"reported as unknown, the operator goes looking for a typo", code, body)
	}
	// THE ACKNOWLEDGEMENT DOES NOT ASK THE BROKER FOR AN INSTANT IT DOES NOT
	// USE: the generation is the node's own.
	if code, body := postAck(t, a,
		"/work/retention/ack?stream=CREWLET_PAGES_LOG&position=918100000"); code != http.StatusOK {
		t.Fatalf("an acknowledgement while the broker could not answer a status "+
			"read was refused %d: %v", code, body)
	}
	if register.calls != 1 {
		t.Fatalf("%d point(s) written, want one", register.calls)
	}
}
