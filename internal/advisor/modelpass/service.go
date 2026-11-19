package modelpass

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/config"
)

// Models is what a pass runs on: the engine's model for the Advisor.
type Models interface {
	// Assigned reports whether the running company assigns the Advisor a model.
	Assigned() bool
	// Deps is the pass's model, tool host and meter, for the company it reads.
	Deps(ctx context.Context, c *config.Company) (Deps, error)
}

// Service is the model pass as the duty runs it ([duty.ModelPass]).
type Service struct {
	Models Models
	// Read is what a pass reads at now.
	Read func(ctx context.Context, now time.Time) (Data, error)
}

var _ duty.ModelPass = (*Service)(nil)

// Assigned reports whether the company assigns the Advisor a model.
func (s *Service) Assigned() bool { return s.Models.Assigned() }

// Run is one pass at now. Only a pass that read and submitted concludes: one
// cut short at its ceiling listed what it reached, never all it would have.
func (s *Service) Run(ctx context.Context, now time.Time, dismissed []advisor.Stored) (duty.ModelRun, error) {
	failed := func(reason string, err error) (duty.ModelRun, error) {
		pass := advisor.Pass{Status: advisor.PassFailed, Reason: reason + err.Error(),
			Dropped: map[advisor.Drop]int{}, Unread: []string{}}
		return duty.ModelRun{Summary: pass}, fmt.Errorf("modelpass: %s%w", reason, err)
	}
	d, err := s.Read(ctx, now)
	if err != nil {
		return failed("the company's records could not be read: ", err)
	}
	d.Answers = AnswersOf(dismissed)
	deps, err := s.Models.Deps(ctx, d.Company)
	if err != nil {
		return failed("the Advisor's model could not start: ", err)
	}
	if deps.DryRun == nil {
		deps.DryRun = func(ch advisor.Change) error { return advisor.CheckChange(d.Company, ch) }
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return now }
	}
	pass := Run(ctx, deps, d, Ceiling)
	window := advisor.Span{From: now.Add(-advisor.Window), To: now}
	out := duty.ModelRun{Summary: pass, Concluded: pass.Status == advisor.PassDone}
	for _, f := range pass.Findings {
		out.Observations = append(out.Observations, advisor.Observe(f, window, d.Revision))
	}
	return out, nil
}

// AnswersOf is what the next pass is told of the model findings people
// dismissed as wrong or already known.
func AnswersOf(dismissed []advisor.Stored) advisor.Answers {
	out := advisor.Answers{}
	for _, s := range dismissed {
		f := s.Observation.Finding
		var verdict advisor.Verdict
		switch s.Reason {
		case advisor.ReasonDismissedWrong:
			verdict = advisor.VerdictWrong
		case advisor.ReasonDismissedKnown, advisor.ReasonDismissedWontFix:
			verdict = advisor.VerdictKnown
		default:
			continue
		}
		if f.Source != advisor.SourceModel {
			continue
		}
		out = append(out, advisor.AnswerState{Fingerprint: f.Focus, Verdict: verdict, By: s.ClosedBy,
			At: s.ClosedAt, Title: f.Title, Subject: f.Subject})
	}
	return out
}
