package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The runtime audit: one persisted event per call a person makes through a
// running node that may have changed something. See [types.OperatorActed] for
// why it exists beside the history every tracker and page write already keeps.

// AuditPublisher is where an audit record goes: the node's own queue, whose
// publish listener writes the event store row on this node.
//
// Declared here, by the consumer, and exported because the API's backup route
// and the human write surface's tool-less verbs publish through [Audit] too —
// one construction of the envelope rather than several that could disagree
// about its source.
type AuditPublisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// Audit publishes one runtime audit record, with the envelope's source set to
// [types.OperatorSource].
//
// # It outlives the request
//
// WithoutCancel, because the record has to be written EXACTLY when the caller
// has gone: a call interrupted by a closed tab may still have landed, and that
// is the call an audit is opened to find. It is still bounded — the broker
// gives a publish with no deadline its own default one — so a node that
// cannot reach its broker answers a moment late rather than never.
//
// # A failure is logged, never answered
//
// The call it describes has already happened. Failing its answer would tell
// the caller a write that landed did not, and a retry would not make the
// audit any more publishable. The write's own history row, where it has one,
// is unaffected.
func Audit(ctx context.Context, pub AuditPublisher, data events.Payload) {
	ev := events.NewFrom(data, tracing.TraceOf(ctx))
	ev.Source = types.OperatorSource
	ctx = context.WithoutCancel(ctx)
	if err := pub.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.ErrorContext(ctx, "operator_audit_not_published", "type", ev.Type,
			"actor", ev.Actor(), "error", err,
			"detail", "the call was made and its answer sent; only its "+
				"runtime audit record is missing from this node's event log")
	}
}

// Acted is one call's runtime audit record, built from the request's
// principal: who made it, through which credential, on which transport, the
// verb, the operation and what became of it.
//
// THE PRINCIPAL IS THE ONE THE REQUEST CARRIES, written down by
// [auth.AttributionOf] — [iam.ActorFor]'s three halves — so the audit row and
// every history row the call wrote name the same author, kind and credential.
//
// EXPORTED for the human write surface's tool-less verbs — a purge, a rename,
// a take-down — which make their writes without a tool and record the call
// through this same constructor rather than a copy of it.
func Acted(ctx context.Context, transport, verb, requestID string,
	outcome types.AuditOutcome, position, refusal string) types.OperatorActed {

	by := auth.AttributionOf(ctx)
	return types.NewOperatorActed(types.OperatorActed{
		ActorName: by.Name, ActorKind: string(by.Kind), OperatorID: by.OperatorID,
		Transport: transport, Tool: verb, RequestID: requestID,
		Outcome: outcome, Position: position, Refusal: refusal,
	})
}

// AuditGesture publishes the runtime audit record of one call a surface made
// WITHOUT a tool — the human write surface's purge, rename, trash, restore,
// comment take-down and remark edit — read exactly as [Server.Dispatch] reads
// a tool's: a writer error through [ClassifyWrite], so it is `unknown` where
// the write may have landed, `refused` with its class where the domain or the
// authority table said no, and `failed` where the node broke — a fault, or
// nothing classified it; and the writer's own outcome and position where it
// answered.
func AuditGesture(ctx context.Context, pub AuditPublisher, transport, verb,
	key string, outcome statelog.Outcome, position statelog.Position, err error) {

	if err != nil {
		result := tools.Result{Failed: true, Cause: ClassifyWrite(err)}
		got, _, refusal := auditOutcome(verb, result, nil)
		Audit(ctx, pub, Acted(ctx, transport, verb, key, got, "", refusal))
		return
	}
	got := types.AuditOutcome(outcome)
	if outcome == "" {
		// A WRITE THAT CHANGED NOTHING APPENDED NOTHING, and is applied:
		// the state asked for already holds.
		got = types.AuditApplied
	}
	if !got.Valid() {
		got = types.AuditUnknown
	}
	at := ""
	if !position.IsZero() {
		at = position.String()
	}
	Audit(ctx, pub, Acted(ctx, transport, verb, key, got, at, ""))
}

// AuditRefusal publishes the runtime audit record of a call a surface refused
// on an authority decision it took ITSELF, before any writer or tool ran: a
// decision this node could not take is `refused` as `unavailable`, and every
// other is `refused` as `forbidden` — the classes a tool's own gate states for
// the same two answers.
func AuditRefusal(ctx context.Context, pub AuditPublisher, transport, verb,
	key string, undecided bool) {

	class := crewletmcp.RefusalForbidden
	if undecided {
		class = crewletmcp.RefusalUnavailable
	}
	Audit(ctx, pub, Acted(ctx, transport, verb, key, types.AuditRefused, "",
		string(class)))
}

// auditCall publishes one dispatched call's record.
//
// A PROVEN READ IS NOT AUDITED, and the dispatch does not call this for one: it
// changes nothing, and a row per question an assistant asked would bury the
// writes an audit is read for. A tool whose read-only hint is unknown is
// audited as the write it may be.
func (s *Server) auditCall(ctx context.Context, c Call, result tools.Result, err error) {
	outcome, position, refusal := auditOutcome(c.Tool, result, err)
	Audit(ctx, s.audit, Acted(ctx, c.Transport, c.Tool, c.Key, outcome,
		position, refusal))
}

// auditOutcome reads what became of one call, in the audit's vocabulary.
//
// THE SAME READING THE TRANSPORTS ANSWER WITH, so the record and the answer the
// caller was given cannot disagree: an interruption is `unknown`, because
// whether the write before it landed is exactly what nobody knows; a write the
// tool could not vouch for is `unknown` too ([crewletmcp.UnknownOf]); a
// classified refusal is `refused` with its class; an unclassified failure is
// `failed`, and so is a FAULT ([crewletmcp.RefusalInternalError]) — the node
// broke rather than turned the call down, which is the outcome `failed` names,
// and its reason is in the log rather than stated; and a success is the tool's
// own stated outcome and position ([receiptOf]), `unknown` for an outcome this
// build does not know.
func auditOutcome(name string, result tools.Result, err error) (
	types.AuditOutcome, string, string) {

	if err != nil {
		return types.AuditUnknown, "", ""
	}
	if crewletmcp.UnknownOf(result) {
		return types.AuditUnknown, "", ""
	}
	if result.Failed {
		class := crewletmcp.RefusalOf(result)
		if class.Valid() && class != crewletmcp.RefusalInternalError {
			return types.AuditRefused, "", string(class)
		}
		return types.AuditFailed, "", ""
	}
	answer := receiptOf(name, result.Output)
	position := ""
	if answer.Position != nil {
		position = *answer.Position
	}
	outcome := types.AuditOutcome(answer.Outcome)
	if !outcome.Valid() {
		outcome = types.AuditUnknown
	}
	return outcome, position, ""
}
