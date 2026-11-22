package runner

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// runsAround is the runs in flight around this turn's run as its launch reads
// them ([Config.Runs], ADR-0176): none where it reads none, or where the read
// fails, which is said; the run then launches alone, told of none and offered
// no tool for them, as a seat with one run is.
func (r *Runner) runsAround(ctx context.Context) []prompts.RunAround {
	if r.cfg.Runs == nil || r.cfg.Turn.RunID == "" {
		return nil
	}
	around, err := r.cfg.Runs.Around(ctx, r.cfg.Turn.RunID)
	if err != nil {
		log.WarnContext(ctx, "executor_runs_around_unread", "turn_id", r.cfg.Turn.RunID, "error", err.Error(),
			"detail", "the runs in flight around this run could not be read, so its brief lists none and it "+
				"is offered no tool to reach them")
		return nil
	}
	return around
}

// runTools is the names of the tools the run is offered for around: every
// one of [RunsAround.Tools] where around holds a run, else none.
func (r *Runner) runTools(around []prompts.RunAround) []string {
	if len(around) == 0 || r.cfg.Runs == nil {
		return nil
	}
	var names []string
	for _, tool := range r.cfg.Runs.Tools() {
		names = append(names, tool.Name())
	}
	return names
}
