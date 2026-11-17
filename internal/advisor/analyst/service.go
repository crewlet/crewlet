package analyst

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
)

// Records is the fleet's analyst records ([coord.AdvisorRecords]).
type Records interface {
	AdvisorRecord(ctx context.Context, name string) ([]byte, uint64, bool, error)
	CreateAdvisorRecord(ctx context.Context, name string, value []byte) (bool, error)
	UpdateAdvisorRecord(ctx context.Context, name string, value []byte, revision uint64) (bool, error)
}

// Models is what a run runs on, for the company it reads: the engine's.
type Models interface {
	Deps(ctx context.Context, c *config.Company) (Deps, error)
}

var log = logging.Get("advisor.analyst")

// ErrRunning is a run asked for while another is going anywhere in the fleet.
var ErrRunning = errors.New("analyst: a run is already going")

// ErrNoFinding is an answer to a finding the last run does not list.
var ErrNoFinding = errors.New("analyst: the last run lists no such finding")

// runLease is how long a running record holds the fleet off: past it, the node
// that claimed it is taken to have stopped, and another may run.
const runLease = 30 * time.Minute

// tick is how often a node asks whether a scheduled run is due; the claim
// makes one node run it however many ask.
const tick = 5 * time.Minute

// Service runs the analyst on a person's press and on the company's cadence,
// one run at a time across the fleet, and keeps the answers to its findings.
type Service struct {
	Records Records
	Models  Models
	// Read is what a run reads, at now.
	Read func(ctx context.Context, now time.Time) (Data, error)
	// Company is the document the node runs now, nil before its first.
	Company func() *config.Company
	Now     func() time.Time
	Zone    func() *time.Location

	mu   sync.Mutex
	base context.Context
	runs sync.WaitGroup
}

// Serve runs the cadence until ctx ends, then waits for any run it started.
func (s *Service) Serve(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	defer s.Wait()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if due, err := s.due(ctx); err != nil {
			log.WarnContext(ctx, "advisor_analysis_due_unread", "error", err.Error())
		} else if due {
			if _, err := s.Start(ctx, "schedule", ""); err != nil && !errors.Is(err, ErrRunning) {
				log.WarnContext(ctx, "advisor_analysis_not_started", "error", err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait returns once every run Start began has ended and been recorded.
func (s *Service) Wait() { s.runs.Wait() }

// due reports whether the company's cadence asks for a run now: on, and no run
// started since the cadence's last slot.
func (s *Service) due(ctx context.Context) (bool, error) {
	c := s.Company()
	if c == nil || !c.Advisor.Analyst.On() || c.Advisor.Analyst.Every() == config.AnalystOff {
		return false, nil
	}
	last, found, err := s.Last(ctx)
	if err != nil {
		return false, err
	}
	slot := Slot(s.Now().In(s.Zone()), c.Advisor.Analyst)
	return !found || last.Started.Before(slot), nil
}

// Slot is the latest scheduled start at or before now: today's hour, or this
// week's Monday's for a weekly cadence, on now's clock.
func Slot(now time.Time, a config.AdvisorAnalyst) time.Time {
	slot := time.Date(now.Year(), now.Month(), now.Day(), a.StartHour(), 0, 0, 0, now.Location())
	if a.Every() == config.AnalystWeekly {
		slot = slot.AddDate(0, 0, -((int(slot.Weekday()) + 6) % 7))
		if slot.After(now) {
			slot = slot.AddDate(0, 0, -7)
		}
		return slot
	}
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// Last is the last run's record, false where none ran.
func (s *Service) Last(ctx context.Context) (advisor.Analysis, bool, error) {
	a, _, found, err := s.last(ctx)
	return a, found, err
}

func (s *Service) last(ctx context.Context) (advisor.Analysis, uint64, bool, error) {
	raw, revision, found, err := s.Records.AdvisorRecord(ctx, coord.AdvisorLastRun)
	if err != nil || !found {
		return advisor.Analysis{}, 0, false, err
	}
	var a advisor.Analysis
	if err := json.Unmarshal(raw, &a); err != nil {
		return advisor.Analysis{}, 0, false, fmt.Errorf("analyst: decode the last run: %w", err)
	}
	return a, revision, true, nil
}

// Start claims a run and runs it in the background, answering the claimed
// record: [ErrRunning] where one is going, [config.ErrAnalystOff] where the
// company names no model.
func (s *Service) Start(ctx context.Context, trigger, by string) (advisor.Analysis, error) {
	c := s.Company()
	if c == nil {
		return advisor.Analysis{}, errors.New("analyst: this node runs no company yet")
	}
	keys, err := c.AnalystChain()
	if err != nil {
		return advisor.Analysis{}, err
	}
	now := s.Now()
	last, revision, found, err := s.last(ctx)
	if err != nil {
		return advisor.Analysis{}, err
	}
	if found && last.Status == advisor.StatusRunning && now.Sub(last.Started) < runLease {
		return last, ErrRunning
	}
	claim := advisor.Analysis{ID: uuid.NewString(), Status: advisor.StatusRunning, Started: now.UTC(),
		Trigger: trigger, By: by, Model: keys[0], Ceiling: c.Advisor.Analyst.Ceiling(),
		Findings: []advisor.Finding{}, Dropped: map[advisor.Drop]int{}, Unread: []string{}}
	won, err := s.write(ctx, coord.AdvisorLastRun, claim, revision, found)
	if err != nil {
		return advisor.Analysis{}, err
	}
	if !won {
		return last, ErrRunning
	}
	s.mu.Lock()
	base := s.base
	s.mu.Unlock()
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		s.finish(base, claim)
	}()
	return claim, nil
}

// finish runs the claimed run and records how it ended, where the record is
// still the claim's.
func (s *Service) finish(ctx context.Context, claim advisor.Analysis) {
	out := s.run(ctx, claim)
	out.ID, out.Started, out.Trigger, out.By, out.Ended = claim.ID, claim.Started, claim.Trigger, claim.By, s.Now().UTC()
	out.Model = claim.Model
	// A CONTEXT THAT OUTLIVES THE NODE'S STOP, so a run cut short still says so.
	ctx = context.WithoutCancel(ctx)
	current, revision, found, err := s.last(ctx)
	if err != nil || !found || current.ID != claim.ID {
		log.WarnContext(ctx, "advisor_analysis_not_recorded", "run", claim.ID, "error", fmt.Sprint(err))
		return
	}
	if _, err := s.write(ctx, coord.AdvisorLastRun, out, revision, true); err != nil {
		log.WarnContext(ctx, "advisor_analysis_not_recorded", "run", claim.ID, "error", err.Error())
	}
}

func (s *Service) run(ctx context.Context, claim advisor.Analysis) advisor.Analysis {
	failed := func(reason string) advisor.Analysis {
		return advisor.Analysis{Status: advisor.StatusFailed, Ceiling: claim.Ceiling, Reason: reason,
			Findings: []advisor.Finding{}, Dropped: map[advisor.Drop]int{}, Unread: []string{}}
	}
	d, err := s.Read(ctx, claim.Started)
	if err != nil {
		return failed("The company's records could not be read: " + err.Error())
	}
	answers, err := s.Answers(ctx)
	if err != nil {
		return failed("The answers to earlier findings could not be read: " + err.Error())
	}
	d.Answers = answers
	deps, err := s.Models.Deps(ctx, d.Company)
	if err != nil {
		return failed("The analyst's model could not start: " + err.Error())
	}
	if deps.DryRun == nil {
		deps.DryRun = func(ch advisor.Change) error { return advisor.CheckChange(d.Company, ch) }
	}
	if deps.Now == nil {
		deps.Now = s.Now
	}
	return Run(ctx, deps, d, claim.Ceiling)
}

// Answers is every answer kept.
func (s *Service) Answers(ctx context.Context) (advisor.Answers, error) {
	a, _, _, err := s.answers(ctx)
	return a, err
}

func (s *Service) answers(ctx context.Context) (advisor.Answers, uint64, bool, error) {
	raw, revision, found, err := s.Records.AdvisorRecord(ctx, coord.AdvisorAnswers)
	if err != nil || !found {
		return advisor.Answers{}, 0, false, err
	}
	var a advisor.Answers
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, 0, false, fmt.Errorf("analyst: decode the answers: %w", err)
	}
	return a, revision, true, nil
}

// Answer records by's verdict on the last run's finding with fingerprint.
func (s *Service) Answer(ctx context.Context, fingerprint string, v advisor.Verdict, by string) error {
	if !v.Valid() {
		return fmt.Errorf("analyst: %q is not an answer", v)
	}
	last, found, err := s.Last(ctx)
	if err != nil {
		return err
	}
	var finding *advisor.Finding
	for i := range last.Findings {
		if found && advisor.Fingerprint(last.Findings[i]) == fingerprint {
			finding = &last.Findings[i]
		}
	}
	if finding == nil {
		return ErrNoFinding
	}
	state := advisor.AnswerState{Fingerprint: fingerprint, Verdict: v, By: by, At: s.Now().UTC(),
		Title: finding.Title, Subject: finding.Subject}
	// TWO TRIES: an answer another person gave in between is kept, and this one beside it.
	for range 2 {
		kept, revision, exists, err := s.answers(ctx)
		if err != nil {
			return err
		}
		won, err := s.write(ctx, coord.AdvisorAnswers, kept.With(state), revision, exists)
		if err != nil || won {
			return err
		}
	}
	return errors.New("analyst: the answers kept changing; send the answer again")
}

// write stores v under name, at revision where the record exists, and
// reports whether this caller's write landed.
func (s *Service) write(ctx context.Context, name string, v any, revision uint64, exists bool) (bool, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	if exists {
		return s.Records.UpdateAdvisorRecord(ctx, name, raw, revision)
	}
	return s.Records.CreateAdvisorRecord(ctx, name, raw)
}
