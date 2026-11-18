package host

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// EVERY PERSON'S PUSH IS ONE operator_acted RECORD ON THE NODE THAT RECEIVED
// IT (transport git, the caller as iam.ActorFor names them), whatever became of
// it: a refused push writes no code-domain row, so this record is the only one.
// A CODING BOX'S PUSH is no operator's act: its push row names its run, and a
// refusal its code_push and code_box_refused lines. A fetch is not audited
// here; one of a `visibility: unit` repository is a READ (code_fetched,
// fetch.go), counted in the usage domain.
//
// The record is the REQUEST'S, settled once however it ends (pushAudit), never
// written exit by exit, so an exit nobody thought to record is a push recorded
// failed, not one missing from ADR-0085's audit and the repository's heat.
// Nothing before admission is recorded: there is nobody to name.

// AuditTool is the operator_acted tool a git push is recorded as.
const AuditTool = "git_push"

// AuditRefusal is the refusal class of a push recorded `refused`: every
// gitlet.Refusal, and the four only the receiver can name. A closed set.
type AuditRefusal string

// The receiver's own refusals.
const (
	// RefusalUnbound is a push with a token bound to nobody.
	RefusalUnbound AuditRefusal = "unbound"
	// RefusalGit is a push git refused itself before the engine saw it —
	// fsck, connectivity, an unpack failure, a shallow update.
	RefusalGit AuditRefusal = "git"
	// RefusalCommands is a command section past its bounds, one that is not
	// pkt-lines or carries a push certificate, or commands more than one push
	// may carry (gitlet.Proposal.ValidateCommands).
	RefusalCommands AuditRefusal = "commands"
	// RefusalPushTooLarge is a push whose request declared more than
	// code.limits.max_push_bytes beside its command section's bound, refused
	// 413 before a byte is read. One declaring no length (past git's post
	// buffer) is held to the same limit by receive.maxInputSize (RefusalGit).
	RefusalPushTooLarge AuditRefusal = "push_too_large"
)

// AuditRefusals is every class: each gitlet.Refusal, then the receiver's four.
func AuditRefusals() []AuditRefusal {
	var out []AuditRefusal
	for _, r := range gitlet.Refusals() {
		out = append(out, AuditRefusal(r))
	}
	return append(out, RefusalUnbound, RefusalGit, RefusalCommands, RefusalPushTooLarge)
}

// Valid reports whether r is one of AuditRefusals.
func (r AuditRefusal) Valid() bool { return slices.Contains(AuditRefusals(), r) }

// pushRecord is what one push's record and its log line say.
type pushRecord struct {
	// pushID is the push's id — minted at its scratch, or here for a push
	// refused before it had one, so every record names one.
	pushID   string
	outcome  types.AuditOutcome
	refusal  AuditRefusal
	position statelog.Position
	commands int
	ok       int
	bytes    int64
	// started is when the push's request arrived, for its duration.
	started time.Time
	// git is the last line git printed, for a push git refused itself.
	git string
}

// pushAudit is ONE PUSH REQUEST'S RECORD, from admission to the handler's
// return, which ends in exactly one of three ways:
//
//   - RECORDED by the exit that knows what became of the push (record);
//   - NONE, no push's end (none): a served advertisement, or git's probe
//     before a push past its post buffer, each followed by the POST;
//   - FAILED, every other exit, a panic included: recorded when the handler
//     returns (settle), with what was learnt of the push by then (failed).
//
// Each handler makes one at admission (newPushAudit) and defers settle at
// once. A fetch makes none: a nil pushAudit's methods do nothing.
type pushAudit struct {
	s   *Server
	ctx context.Context
	adm admission
	// failed is the record settle writes when no exit recorded the push,
	// filled in as the request learns of it (its commands, once read).
	failed pushRecord
	// done: the push is recorded, or the request is none of a push's ends.
	done bool
}

// newPushAudit is the record of the push adm admitted, whose request arrived
// at started.
func (s *Server) newPushAudit(ctx context.Context, adm admission, started time.Time) *pushAudit {
	return &pushAudit{s: s, ctx: ctx, adm: adm, failed: pushRecord{outcome: types.AuditFailed, started: started}}
}

// record writes rec as the push's one record — its started the request's
// arrival — unless the request is recorded already or is none of a push's
// ends.
func (a *pushAudit) record(rec pushRecord) {
	if a == nil || a.done {
		return
	}
	a.done = true
	rec.started = a.failed.started
	a.s.audit(a.ctx, a.adm, rec)
}

// none ends the request as no push's end: settle records nothing.
func (a *pushAudit) none() {
	if a != nil {
		a.done = true
	}
}

// settle records the push failed unless an exit recorded it or ended the
// request as none: deferred by every push's handler from its admission, so it
// runs however the handler returns, a panic's unwinding included.
func (a *pushAudit) settle() {
	if a != nil {
		a.record(a.failed)
	}
}

// audit records one push: its operator_acted record, its code_push line, its
// duration, and the push in its repository's heat, counted here because only a
// pushAudit calls it, once per request.
func (s *Server) audit(ctx context.Context, adm admission, rec pushRecord) {
	if rec.pushID == "" {
		rec.pushID = uuid.Must(uuid.NewV7()).String()
	}
	s.o.Cache.Heat().Add(adm.repo.ID, adm.name, cache.Use{Pushes: 1})
	position := ""
	if !rec.position.IsZero() {
		position = rec.position.String()
	}
	refusal := ""
	if rec.outcome == types.AuditRefused {
		refusal = string(rec.refusal)
	}
	// A PERSON'S PUSH IS AN OPERATOR'S ACT (ADR-0024). A coding box's is not:
	// its push row, naming its run, is its record; it is still counted, logged
	// and timed below.
	if adm.box == nil {
		// THE PUBLISH OUTLIVES THE REQUEST: a client that hung up is still
		// a push that happened.
		s.o.Audit(context.WithoutCancel(ctx), types.NewOperatorActed(types.OperatorActed{
			ActorName: adm.actor.Name, ActorKind: string(adm.actor.Kind), OperatorID: adm.operatorID,
			Transport: types.TransportGit, Tool: AuditTool, RequestID: rec.pushID, Repo: adm.name,
			Outcome: rec.outcome, Position: position, Refusal: refusal,
		}))
	}
	var took time.Duration
	if !rec.started.IsZero() {
		took = s.now().Sub(rec.started)
	}
	p := adm.principal()
	attrs := []any{"repo", adm.name, "push_id", rec.pushID, "operator_id", p.OperatorID,
		"seat", p.Seat, "outcome", string(rec.outcome), "refusal", refusal, "commands", rec.commands,
		"ok", rec.ok, "bytes", rec.bytes, "position", position, "took", took}
	if p.Run.RunID != "" {
		attrs = append(attrs, "turn_id", p.Run.TurnID, "launch_id", p.Run.RunID)
	}
	if rec.git != "" {
		attrs = append(attrs, "git", rec.git)
	}
	s.log.Info("code_push", attrs...)
	if s.o.Metrics != nil && !rec.started.IsZero() {
		s.o.Metrics.Observe(metrics.CodePushDuration, took, metrics.Attrs{"outcome": string(rec.outcome)})
	}
}
