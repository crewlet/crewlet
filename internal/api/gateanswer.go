package api

import (
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GateAnswer is what the eviction and readmission routes answer once the
// gesture is past its judgement: one entry per identity-claiming log, and
// whether every one of them now holds the record.
//
// # One typed value, because the shape had three hand copies
//
// The route built this out of maps, the command line's tests built their own
// copy "as the route renders it", and the dashboard declared a third in
// TypeScript. The three had already drifted — the test copy sent a position for
// an unknown log that no node sends, and the dashboard still read a top-level
// outcome the route stopped writing, so every answer it got rendered as "no
// acknowledgement". The route renders through [RenderGate] and nothing else;
// the command line's tests serve what it returns; and the dashboard's suite
// reads the golden file this package's test writes from it
// (`internal/api/testdata/gate_answer.json`), so a change here is a failing
// test on both sides until each follows it.
//
// EXPORTED only so a test in another package can serve the real rendering —
// the command line's fake node is the one that did not. Nothing outside the
// module can import it.
type GateAnswer struct {
	Node    string `json:"node"`
	Evicted bool   `json:"evicted"`

	// OpID is the GESTURE's operation id — the caller's, or one minted for
	// a caller that brought none — which is what finishes it: the same
	// request sent again with it.
	OpID string `json:"op_id"`

	// Complete reports whether every log holds the record durably.
	Complete bool `json:"complete"`

	Domains []GateDomainAnswer `json:"domains"`
}

// GateDomainAnswer is one log's answer to a gesture.
//
// Either an Outcome, or an Error with the Reason that names it — never both,
// because a refusal is not one of the three outcomes — and Actions with Hint
// on exactly the logs the gesture did not finish.
type GateDomainAnswer struct {
	Domain string `json:"domain"`
	Stream string `json:"stream"`
	OpID   string `json:"op_id"`

	// Outcome is the write's own three-valued answer, and ABSENT on a log
	// whose write answered with an error.
	Outcome statelog.Outcome `json:"outcome,omitempty"`

	// Unvouched is set on an `unknown` this node CANNOT settle: its
	// operation ledger may have lost the row the operation needs, so it
	// published nothing and answers the same way to the same gesture every
	// time. Absent otherwise. A surface says so beside the outcome — "the
	// record may or may not be on the log" is true of both, and only this
	// one is not finished here.
	Unvouched bool `json:"unvouched,omitempty"`

	// Position is where the record is durable, and NIL FOR UNKNOWN — which
	// is the whole content of unknown: a zero position reads as a record
	// at the log's origin, and the gesture may never have reached the log.
	Position *statelog.Position `json:"position,omitempty"`

	// Error is why the log gave no outcome, and Reason the refusal's name
	// in the vocabulary every write refusal uses, so a caller can tell
	// `log_full` from `evicted` without reading the sentence. Reason is
	// absent for a failure that was not a refusal.
	Error  string          `json:"error,omitempty"`
	Reason statelog.Reason `json:"reason,omitempty"`

	// Actions is what the operator does about a log the gesture did not
	// finish — [statelog.GateRetrySameOp] where the same request again
	// with the gesture's op_id finishes it — and Hint the sentence saying
	// why, in no surface's vocabulary. Both absent on a finished log; a
	// Hint with no Actions is a condition no gesture clears.
	Actions []statelog.GateAction `json:"actions,omitempty"`
	Hint    string                `json:"hint,omitempty"`
}

// RenderGate is the one rendering of a gesture's result — see [GateAnswer].
func RenderGate(evict bool, result engine.GateResult) GateAnswer {
	out := GateAnswer{
		Node: result.Node, Evicted: evict, OpID: result.OpID,
		Complete: result.Complete(),
		Domains:  make([]GateDomainAnswer, 0, len(result.Domains)),
	}
	for _, d := range result.Domains {
		entry := GateDomainAnswer{Domain: d.Domain, Stream: d.Stream, OpID: d.OpID}
		if d.Err != nil {
			entry.Error = d.Err.Error()
			var refused *statelog.Unavailable
			if errors.As(d.Err, &refused) {
				entry.Reason = refused.Reason
			}
		} else {
			// THE THREE-VALUED OUTCOME, whole. A gate the caller believes
			// landed and which is only `pending` is the difference between
			// a node that has stopped writing and one that is about to.
			entry.Outcome, entry.Unvouched = d.Outcome, d.Unvouched
			if d.Outcome != statelog.OutcomeUnknown {
				position := d.Position
				entry.Position = &position
			}
		}
		// WHAT TO DO ABOUT A LOG THE GESTURE DID NOT FINISH — the engine's
		// one judgement, so this route and the command never advise two
		// different things.
		if remedy := d.Remedy(); !remedy.IsZero() {
			entry.Actions, entry.Hint = remedy.Actions, remedy.Detail
		}
		out.Domains = append(out.Domains, entry)
	}
	return out
}

// GateRefusal is a gesture refused before anything was written: the status,
// the code and the facts the route answers with.
//
// IN THE ENGINE'S ENVELOPE, like every other refusal this surface writes — the
// code is on httpjson's table and carries its sentence as `message` — and a
// 503 carries the Retry-After its remedy implies. It was a bare map written
// with no sentence, whose one 503 (`eviction_unjudged`) said `retry_same_op`
// in its body and, by carrying no Retry-After, "do not come back" in its
// header.
type GateRefusal struct {
	Status int
	Code   httpjson.Code
	Fields httpjson.Detail

	// RetryAfter is the Retry-After in whole seconds, on a 503 whose cause
	// waiting can clear; zero writes none.
	RetryAfter int
}

// Envelope is the refusal's body as the route sends it — [httpjson.Envelope]'s,
// so the golden file records exactly what a client reads. A server standing in
// for a node answers with [GateRefusal.Write] rather than encoding this itself,
// or its 503 goes out with no Retry-After.
func (g GateRefusal) Envelope() map[string]any { return httpjson.Envelope(g.Code, g.Fields) }

// Write answers the refusal: a 503 through [httpjson.UnavailableWith], which
// sets the header and the envelope together, and anything else through
// [httpjson.FailWithFields].
func (g GateRefusal) Write(w http.ResponseWriter) {
	if g.Status == http.StatusServiceUnavailable {
		httpjson.UnavailableWith(w, g.Code, g.RetryAfter, g.Fields)
		return
	}
	httpjson.FailWithFields(w, g.Status, g.Code, g.Fields)
}

// RenderGateRefusal renders err if it is one of the gate's refusals, reporting
// false for anything else — a failure the route answers `500 gate_failed`.
//
// EVERY REFUSAL CARRIES ITS REMEDY AS `actions` BESIDE `hint`: what to do as a
// closed set ([statelog.GateAction]) and the sentence saying why in no
// surface's vocabulary. `force` among the actions is what tells a surface with
// no -force flag — the dashboard — to offer the control that sends
// `force=true`; before it, the only word for it was a sentence naming the flag.
//
// AND THE OPERATION IT WAS SENT UNDER, as `op_id`, on every refusal that
// carries actions: `retry_same_op` means the same request with the answer's
// `op_id`, and a refusal that carried none sent a reader looking for a value
// the body did not hold. Nothing was written under it, so reusing it and
// minting another are both safe — carrying it is what makes "the same request"
// something a client can read off the answer rather than remember.
//
// A MAP OF FACTS rather than a struct for one reason: a readmission refusal
// carries numbers whose zero is a real value — a node that never published, a
// position of zero — and an omitempty struct would drop the facts the refusal
// is about.
func RenderGateRefusal(node, opID string, err error) (GateRefusal, bool) {
	var readmission *statelog.ReadmissionRefusal
	var eviction *statelog.EvictionRefusal
	var unjudged *engine.GateUnjudged
	switch {
	case err == nil:
		return GateRefusal{}, false
	case errors.Is(err, engine.ErrInvalidGate):
		// A NODE ID NO NODE COULD RUN UNDER is a typo rather than a fault,
		// and it is the caller's to fix: nothing was judged and nothing
		// was written.
		return GateRefusal{Status: http.StatusBadRequest, Code: httpjson.CodeInvalidGate,
			Fields: httpjson.Detail{"detail": err.Error()}}, true
	case errors.As(err, &unjudged):
		// COORDINATION COULD NOT BE READ HERE, which is this node's
		// condition rather than the target's — so a 503 carrying the way
		// past it, not a 500 an operator reads as an engine bug. The
		// Retry-After is the UNDECIDED scale ([authz.RetryUndecidedSeconds]):
		// what the caller waits for is one read of the presence leases
		// coming back, a store blip rather than an outage.
		remedy := unjudged.Remedy()
		return GateRefusal{Status: http.StatusServiceUnavailable,
			Code: httpjson.CodeEvictionUnjudged, RetryAfter: authz.RetryUndecidedSeconds,
			Fields: httpjson.Detail{
				"detail": unjudged.Error(), "hint": remedy.Detail,
				"actions": remedy.Actions, "node": node, "op_id": opID,
			}}, true
	case errors.As(err, &readmission):
		// 409 WITH THE NUMBERS, because the inequality is the reason — an
		// operator told only "500 gate_failed" would read an engine problem
		// where there is a node still catching up.
		remedy := readmission.Remedy()
		return GateRefusal{Status: http.StatusConflict,
			Code: httpjson.CodeReadmissionRefused, Fields: httpjson.Detail{
				"detail": readmission.Error(),
				"hint":   remedy.Detail, "actions": remedy.Actions,
				"node": node, "op_id": opID, "domain": readmission.Domain,
				"published":        readmission.Published,
				"position":         readmission.Seq,
				"generation":       readmission.Generation,
				"floor":            readmission.Bound.Floor,
				"first_seq":        readmission.Bound.First,
				"floor_generation": readmission.Bound.Generation,
			}}, true
	case errors.As(err, &eviction):
		// A NODE STILL RENEWING ITS PRESENCE LEASE is still reaching the
		// fleet, and nothing was written anywhere.
		remedy := eviction.Remedy()
		return GateRefusal{Status: http.StatusConflict,
			Code: httpjson.CodeEvictionRefused, Fields: httpjson.Detail{
				"detail": eviction.Error(), "hint": remedy.Detail,
				"actions": remedy.Actions, "node": node, "op_id": opID,
			}}, true
	}
	return GateRefusal{}, false
}

// logGateRefusal is the route's own line for a refusal it answered, naming who
// pressed as every other line on this surface does: the author and the
// credential they pressed it through.
func logGateRefusal(by iam.Actor, node string, err error) {
	var readmission *statelog.ReadmissionRefusal
	var eviction *statelog.EvictionRefusal
	var unjudged *engine.GateUnjudged
	switch {
	case errors.As(err, &unjudged):
		log.Warn("retention_eviction_unjudged", "by", by.Name,
			"operator", by.OperatorID, "node", node, "error", unjudged.Err)
	case errors.As(err, &readmission):
		log.Info("retention_readmission_refused", "by", by.Name,
			"operator", by.OperatorID, "node", node, "domain", readmission.Domain,
			"position", readmission.Seq, "floor", readmission.Bound.Held())
	case errors.As(err, &eviction):
		log.Info("retention_eviction_refused", "by", by.Name,
			"operator", by.OperatorID, "node", node, "detail", eviction.Detail)
	}
}
