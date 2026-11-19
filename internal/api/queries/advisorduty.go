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
)

// THE ADVISOR'S DUTY OVER THIS NODE'S SOURCES (ADR-0164): what a run reads is
// what the advisor read on every request before it kept its findings, the
// reads [Sources.advisorRead] makes, with the atlas read as the engine reads it.

// AdvisorDuty is the Advisor's duty over this node's sources, writing to log,
// holding claim (nil on a node with no fleet).
func (s Sources) AdvisorDuty(log duty.Log, claim func(context.Context) (bool, error)) *duty.Duty {
	d := &duty.Duty{Log: log, Read: advisorObserver{s: s}, Claim: claim, NewID: uuid.NewString,
		Now: s.clock, Zone: s.zone}
	if s.AdvisorModel != nil {
		d.Model = &modelpass.Service{Models: s.AdvisorModel, Read: s.advisorModelData}
	}
	return d
}

// advisorObserver is the duty's reading of the company through s.
type advisorObserver struct{ s Sources }

var _ duty.Reader = advisorObserver{}

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
	out := duty.Observed{Unread: answer.Unread,
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
func (o advisorObserver) Signature(ctx context.Context) (string, error) {
	if o.s.Config == nil {
		return "", nil
	}
	_, revision, err := o.s.Config.FleetCompany(ctx)
	if err != nil {
		return "", err
	}
	sig := setupSignature{Revision: revision, Integrations: map[string]string{}, Paused: []string{}}
	if states, known := o.s.reconcileStates(ctx); known {
		for kind, state := range states {
			raw, err := json.Marshal(state.Findings)
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256(raw)
			sig.Integrations[kind] = hex.EncodeToString(sum[:])
		}
	}
	if o.s.Pauses != nil {
		pauses, err := o.s.Pauses.ListSeatPauses(ctx)
		if err != nil {
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
