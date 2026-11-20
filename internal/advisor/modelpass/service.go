package modelpass

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
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
func (s *Service) Run(ctx context.Context, now time.Time, dismissed, live []advisor.Stored) (duty.ModelRun, error) {
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
	d.Unexplained = advisor.Unexplained(live)
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
	out := duty.ModelRun{Summary: pass, Concluded: pass.Status == advisor.PassDone, Explained: pass.Explained}
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

// DeepService is the Advisor's deep run over the duty (ADR-0166).
type DeepService struct {
	Models Models
	// Read is what one deep run reads at now: the company, its rule
	// findings, its snapshot and the reads it may ask.
	Read func(ctx context.Context, now time.Time) (DeepData, error)
	// Bounds are the run's bounds for company c.
	Bounds func(c *config.Company) DeepBounds
}

var _ duty.DeepPass = (*DeepService)(nil)

// Assigned reports whether the company assigns the Advisor a model.
func (s *DeepService) Assigned() bool { return s.Models.Assigned() }

// Deep is one deep run at now: what it found, grounded, as observations, and
// its record. It concluded only where it submitted within its bounds, so a
// run cut short leaves earlier model findings as they stand.
func (s *DeepService) Deep(ctx context.Context, now time.Time, dismissed, live []advisor.Stored,
	io duty.DeepIO) (duty.ModelRun, error) {
	failed := func(reason string, err error) (duty.ModelRun, error) {
		run := DeepRun{Pass: advisor.Pass{Status: advisor.PassFailed, Reason: reason + err.Error(),
			Dropped: map[advisor.Drop]int{}, Unread: []string{}}, Progress: []Progress{}}
		return duty.ModelRun{Summary: run.Kept()}, fmt.Errorf("modelpass: %s%w", reason, err)
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
		deps.Now = time.Now
	}
	bounds := DefaultDeepBounds
	if s.Bounds != nil {
		bounds = s.Bounds(d.Company)
	}
	// A CHECKPOINT THAT CANNOT BE READ starts the run over, as before resume.
	var resume *Checkpoint
	if len(io.Resume) > 0 {
		var c Checkpoint
		if err := json.Unmarshal(io.Resume, &c); err == nil {
			resume = &c
		}
	}
	var keep func(Checkpoint)
	if io.Checkpoint != nil {
		keep = func(c Checkpoint) {
			body, err := json.Marshal(c)
			if err == nil && len(body) > advisordomain.MaxCheckpointBytes {
				// PAST ITS BOUND THE READS GO FIRST: the verdicts are what a
				// takeover keeps.
				c.Reads = []KeptRead{}
				body, err = json.Marshal(c)
			}
			if err == nil && len(body) <= advisordomain.MaxCheckpointBytes {
				io.Checkpoint(deps.Now(), body)
			}
		}
	}
	run := RunDeepFrom(ctx, deps, d, bounds, func(p Progress) {
		if io.Progress != nil {
			io.Progress(p.At, p.Text)
		}
	}, resume, keep)
	// ITS RECORD KEEPS WHAT FITS: the lines and verdicts are on its progress.
	out := duty.ModelRun{Summary: run.Kept(), Concluded: run.Status == advisor.PassDone && run.StoppedBy == StoppedDone,
		Partial: run.Status == advisor.PassCeiling && run.Listed > 0}
	window := advisor.Span{From: now.Add(-advisor.Window), To: now}
	for _, f := range run.Findings {
		out.Observations = append(out.Observations, advisor.Observe(f, window, d.Revision))
	}
	return out, nil
}
