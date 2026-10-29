package host

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// EVERY PERSON'S PUSH IS ONE RECORD ON THE NODE THAT RECEIVED IT —
// operator_acted, transport git, the caller as every record names them
// (iam.ActorFor: the author — the seat for a person — its kind, and the
// credential as the operator id) — whatever became of it: a refused push writes
// no row in the code domain, so this record is the only one that says it
// happened, and where. A CODING BOX'S PUSH is no operator's act and publishes
// none: what it landed is its push row, which names its run, and what was
// refused its code_push and code_box_refused lines. A fetch is a proven read
// and is not audited here; a fetch of a `visibility: unit` repository that sent
// its contents is recorded as a READ instead (code_fetched, fetch.go), which
// the usage domain counts — a box's under its seat.
//
// EVERY PUSH THE NODE ADMITS, HOWEVER ITS REQUEST ENDS (pushAudit): the record
// is the REQUEST'S, settled once by whichever way the request ends, never
// written exit by exit — so an exit nobody thought to record is a push
// recorded failed rather than a push that never happened. Written exit by
// exit, every request that failed on its way to its run — its sync, its
// scratch, a fault, a rule this build cannot read, a body it cannot decode,
// a failed advertisement — was answered and recorded by nobody, which left
// ADR-0055's audit and the repository's heat (its pushes counted here) a
// push short of what the node did. Before admission there is nobody to name:
// a request that is not git's, a credential refused, a repository the node
// does not have and a host stopping are answered before the node knows who
// pushed, or where, and are not recorded.

// AuditTool is the operator_acted tool a git push is recorded as.
const AuditTool = "git_push"

// AuditRefusal is the refusal class of a push recorded `refused`: every
// code.Refusal, and the four only the receiver can name. A closed set.
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
	// may carry (code.Proposal.ValidateCommands).
	RefusalCommands AuditRefusal = "commands"
	// RefusalPushTooLarge is a push whose request declared more than one push
	// may send — code.limits.max_push_bytes beside its command section's bound
	// — refused 413 before a byte of it was read. The same limit git's own
	// receive.maxInputSize holds a push to that declared no length (git sends
	// one only under its post buffer), which git refuses itself (RefusalGit).
	RefusalPushTooLarge AuditRefusal = "push_too_large"
)

// AuditRefusals is every class: each code.Refusal, then the receiver's four.
func AuditRefusals() []AuditRefusal {
	var out []AuditRefusal
	for _, r := range code.Refusals() {
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

// pushAudit is ONE PUSH REQUEST'S RECORD, from its admission to its handler's
// return, so that every request an admitted push makes ends in exactly one of
// three ways:
//
//   - RECORDED by the exit that knows what became of the push (record):
//     refused, or as its run and its session concluded it;
//   - NONE, a request that is no push's end (none): an advertisement served,
//     and git's probe before a push past its post buffer, each followed by the
//     POST that is the push's end;
//   - FAILED, every other exit — the push's sync failing, a fault, its
//     scratch, a protection rule this build cannot read, a body this node
//     cannot decode, git failing before the push was judged, a handler cut
//     short by a panic — recorded failed when the handler returns (settle),
//     with what the request had learnt of the push by then (failed).
//
// So no return of a push's handler is a push nobody recorded, and none is
// recorded twice. Each handler makes one with its admission (newPushAudit)
// and defers its settle at once. A fetch makes none: every method of a nil
// pushAudit does nothing.
type pushAudit struct {
	s   *Server
	ctx context.Context
	adm admission
	// failed is the record settle writes when no exit recorded the push:
	// outcome failed, the request's arrival, and what the request has learnt
	// of the push since (its commands, once read).
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

// audit records one push: its operator_acted record through Options.Audit, its
// code_push line, its duration under its outcome, and the push in its
// repository's heat — counted here because this is the one place every push
// reaches exactly once, whatever became of it: called only by a pushAudit,
// which calls it once a request.
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
	// A PERSON'S PUSH IS AN OPERATOR'S ACT (ADR-0024): its operator_acted
	// record, published here. A CODING BOX'S IS NONE — no person acted, and
	// its push IS its record, the push row naming its run
	// (code.RunProvenance.RunID) — so it is counted, logged and timed below
	// like every push, and published as nobody's act.
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
