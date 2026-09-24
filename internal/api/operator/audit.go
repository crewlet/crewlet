package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
// publishes through [Audit] too — one construction of the envelope rather
// than two that could disagree about its source.
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

// BoundSeat is the human seat a token id is bound to by
// `contact.crewlet_operator_id`, or "" for a token nobody bound — the person
// an audit record names beside the credential. Exported for the API's backup
// route, which has the same question about the same caller.
func BoundSeat(chart func() *org.Organization, operatorID string) string {
	return seatFor(chart, operatorID)
}

// dispatch is the one path every transport takes into a tool, and the one
// place a call is audited — so the dashboard and a person's assistant cannot
// differ in what is recorded about them.
//
// A PROVEN READ IS NOT AUDITED: it changes nothing, and a row per question an
// assistant asked would bury the writes an audit is read for. A tool whose
// read-only hint is unknown is audited as the write it may be, the same rule
// the act transport admits it by. A name the catalogue does not hold is not a
// call at all.
func (s *Server) dispatch(ctx context.Context, transport, requestID, name string,
	args map[string]any) (tools.Result, bool, error) {

	result, served, err := s.catalogue.call(ctx, name, args)
	if !served || readOnly(name) {
		return result, served, err
	}
	operatorID, _ := auth.OperatorFrom(ctx)
	record := types.OperatorActed{
		OperatorID: operatorID,
		ActorSeat:  seatFor(s.chart, operatorID),
		Transport:  transport,
		Tool:       name,
		RequestID:  requestID,
	}
	record.Outcome, record.Position, record.Refusal = auditOutcome(name, result, err)
	Audit(ctx, s.audit, types.NewOperatorActed(record))
	return result, served, err
}

// auditOutcome reads what became of one call, in the audit's vocabulary.
//
// THE SAME READING THE ACT TRANSPORT ANSWERS WITH, so the record and the
// answer the caller was given cannot disagree: an interruption is `unknown`,
// because whether the write before it landed is exactly what nobody knows; a
// classified refusal is `refused` with its class; an unclassified failure is
// `failed`; and a success is the tool's own stated outcome and position
// ([receiptOf]), `unknown` for an outcome this build does not know.
func auditOutcome(name string, result tools.Result, err error) (
	types.AuditOutcome, string, string) {

	if err != nil {
		return types.AuditUnknown, "", ""
	}
	if result.Failed {
		if _, known := RefusalStatus(result.Refusal); known {
			return types.AuditRefused, "", string(result.Refusal)
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
