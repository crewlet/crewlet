package queries

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE ADVISOR'S DUTY OVER THIS NODE'S SOURCES (ADR-0164): what a run reads is
// what the advisor read on every request before it kept its findings, the
// reads [Sources.advisorRead] makes, with the atlas read as the engine reads it.

// AdvisorDuty is the Advisor's duty over this node's sources, writing to log,
// holding claim (nil on a node with no fleet); its deep run asks the
// engine's questions through ask (ADR-0166).
func (s Sources) AdvisorDuty(log duty.Log, claim func(context.Context) (bool, error), ask AskQuestion) *duty.Duty {
	d := &duty.Duty{Log: log, Read: advisorObserver{s: s, ask: ask}, Claim: claim, NewID: uuid.NewString,
		Now: s.clock, Zone: s.zone}
	if s.AdvisorModel != nil {
		d.Model = &modelpass.Service{Models: s.AdvisorModel, Read: s.advisorModelData}
		d.Deep = &modelpass.DeepService{Models: s.AdvisorModel, Read: s.advisorDeepData(ask),
			Bounds: advisorDeepBounds}
	}
	return d
}

// advisorObserver is the duty's reading of the company through s.
type advisorObserver struct {
	s Sources
	// ask answers the questions a finding's premise reads.
	ask AskQuestion
}

var (
	_ duty.Reader    = advisorObserver{}
	_ duty.DayReader = advisorObserver{}
)

// Observe reads the company at now: every finding the rule groups raise, each
// over the span its group reads, the sources it could not read, and the
// subjects the company still has.
func (o advisorObserver) Observe(ctx context.Context, now time.Time) (duty.Observed, error) {
	answer, company, err := o.s.advisorReading(ctx, now)
	if err != nil {
		return duty.Observed{}, err
	}
	if company == nil {
		return duty.Observed{Unread: []string{}}, nil
	}
	answer = o.s.advisedOfAtlas(ctx, answer, now)
	week := advisor.Span{From: now.Add(-advisor.Window), To: now}
	read := advisor.Span{From: now, To: now}
	out := duty.Observed{Unread: answer.Unread, Silent: answer.Silent, Partial: answer.Partial,
		Held: func(subject advisor.Subject) bool { return advisor.SubjectHeld(company, subject) }}
	for _, f := range answer.Findings {
		span := read
		if advisor.GroupOf(f) == advisor.GroupEvidence {
			span = week
		}
		out.Observations = append(out.Observations, advisor.Observe(f, span, answer.Revision))
	}
	return out, nil
}

// setupSignature is everything the setup group reads that a change moves.
type setupSignature struct {
	Revision     string            `json:"revision"`
	Integrations map[string]string `json:"integrations"`
	Paused       []string          `json:"paused"`
}

// Signature is a digest of the company's revision, each integration's last
// reconcile and every paused seat: a change to any is a change to the setup.
// An integration's findings are read by the digest its status names, never
// themselves: the signature is read every duty tick.
func (o advisorObserver) Signature(ctx context.Context) (string, error) {
	if o.s.Config == nil {
		return "", nil
	}
	_, revision, err := o.s.Config.FleetCompany(ctx)
	if err != nil {
		return "", err
	}
	sig := setupSignature{Revision: revision, Integrations: map[string]string{}, Paused: []string{}}
	if states, known := o.s.reconcileStatuses(ctx); known {
		for kind, state := range states {
			sig.Integrations[kind] = state.FindingsRecord.SHA256
		}
	}
	if o.s.Pauses != nil {
		var pauses []coord.SeatPause
		if pauses, err = o.s.Pauses.ListSeatPauses(ctx); err != nil {
			return "", err
		}
		for _, p := range pauses {
			sig.Paused = append(sig.Paused, p.Seat.String())
		}
		slices.Sort(sig.Paused)
	}
	raw, err := json.Marshal(sig)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// advisorModelData is what a model pass reads at now: the reads the rule pass
// makes, over the redacted document, and the rule findings they raise, which
// the model never repeats.
func (s Sources) advisorModelData(ctx context.Context, now time.Time) (modelpass.Data, error) {
	in, ev, company, err := s.advisorInputs(ctx, now)
	if err != nil {
		return modelpass.Data{}, err
	}
	if company == nil {
		return modelpass.Data{}, errNoCompanyYet
	}
	answer := advisor.ReadWithEvidence(in, ev)
	return modelpass.Data{Company: in.Company, Revision: in.Revision, Evidence: ev, Rules: answer.Findings,
		Unread: answer.Unread}, nil
}

// errNoCompanyYet is a model pass asked of a deployment before its first import.
var errNoCompanyYet = errors.New("queries: this deployment has imported no company yet, so there is nothing to read")

// Holds asks a finding's premise again, as the Advisor: whether the number at
// its path still compares as it did. A number no longer there is a premise
// that no longer holds; a read off the allowlist, or one that fails, says
// nothing either way.
func (o advisorObserver) Holds(ctx context.Context, p advisor.Premise) (bool, error) {
	if !slices.ContainsFunc(advisorReads, func(q modelpass.Question) bool { return q.Name == p.Read }) {
		return false, errors.New("queries: a premise reads " + p.Read + ", which is not a read the Advisor may ask")
	}
	answer, err := deepReads{ask: o.ask}.Answer(ctx, p.Read, p.Params)
	if err != nil {
		return false, err
	}
	v, err := advisor.DecodeAnswer(answer)
	if err != nil {
		return false, err
	}
	n, found := advisor.ReadPath(v, p.Path)
	return found && p.Holds(n), nil
}
