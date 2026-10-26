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

// EVERY PUSH IS ONE RECORD ON THE NODE THAT RECEIVED IT — operator_acted,
// transport git, the caller as every record names them (iam.ActorFor: the
// author — the seat for a person — its kind, and the credential as the
// operator id) — whatever became of it: a refused push writes no row in the code
// domain, so this record is the only one that says it happened, and where. A
// fetch is a proven read and is not audited.

// AuditTool is the operator_acted tool a git push is recorded as.
const AuditTool = "git_push"

// AuditRefusal is the refusal class of a push recorded `refused`: every
// code.Refusal, and the three only the receiver can name. A closed set.
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
)

// AuditRefusals is every class: each code.Refusal, then the receiver's three.
func AuditRefusals() []AuditRefusal {
	var out []AuditRefusal
	for _, r := range code.Refusals() {
		out = append(out, AuditRefusal(r))
	}
	return append(out, RefusalUnbound, RefusalGit, RefusalCommands)
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

// audit records one push: its operator_acted record through Options.Audit, its
// code_push line, its duration under its outcome, and the push in its
// repository's heat — counted here because this is the one place every push
// reaches exactly once, whatever became of it.
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
	// THE PUBLISH OUTLIVES THE REQUEST: a client that hung up is still a
	// push that happened.
	s.o.Audit(context.WithoutCancel(ctx), types.NewOperatorActed(types.OperatorActed{
		ActorName: adm.actor.Name, ActorKind: string(adm.actor.Kind), OperatorID: adm.operatorID,
		Transport: types.TransportGit, Tool: AuditTool, RequestID: rec.pushID, Repo: adm.name,
		Outcome: rec.outcome, Position: position, Refusal: refusal,
	}))
	var took time.Duration
	if !rec.started.IsZero() {
		took = s.now().Sub(rec.started)
	}
	attrs := []any{"repo", adm.name, "push_id", rec.pushID, "operator_id", adm.operatorID,
		"seat", adm.seat, "outcome", string(rec.outcome), "refusal", refusal, "commands", rec.commands,
		"ok", rec.ok, "bytes", rec.bytes, "position", position, "took", took}
	if rec.git != "" {
		attrs = append(attrs, "git", rec.git)
	}
	s.log.Info("code_push", attrs...)
	if s.o.Metrics != nil && !rec.started.IsZero() {
		s.o.Metrics.Observe(metrics.CodePushDuration, took, metrics.Attrs{"outcome": string(rec.outcome)})
	}
}
