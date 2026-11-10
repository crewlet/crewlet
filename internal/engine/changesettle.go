package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A linked change reaches its item (ADR-0121): a pull request a task waits on,
// merged or closed on its host, is recorded on the task by the engine, with no
// agent's turn, learned by the webhook as it arrives or by the poll of the open
// ones, and the task's holder is told to update it.

// changeSettleName names a settle's operation.
const changeSettleName = "change-settled"

// settleChange records ref's settle on every live task waiting on it, through
// this node's own tracker: false where this node holds none, whose settle the
// poll of a node that does makes instead.
func (e *Engine) settleChange(ctx context.Context, ref string, s worklet.ChangeSettlement) (int, bool, error) {
	n := e.native.Load()
	if n == nil || n.writer == nil || n.trackerReader == nil {
		return 0, false, nil
	}
	tasks, err := n.trackerReader.WaitingOnChange(ctx, ref, statelog.DefaultReadLevel(statelog.SurfaceSeat))
	if err != nil {
		return 0, true, err
	}
	s.Ref = ref
	settled := 0
	for _, task := range tasks {
		// KEYED ON THE HOST'S OWN INSTANT, which the webhook and the poll
		// both read, so the two (and every app's copy of one delivery) write once.
		op := statelog.DeriveOpID(s.At, changeSettleName, task.ID, ref, s.State)
		if _, err := n.writer.SettleChange(ctx, op, task, s); err != nil {
			return settled, true, fmt.Errorf("engine: record %s %s on task %s: %w", ref, s.State, task.ID, err)
		}
		settled++
	}
	return settled, true, nil
}

// observeInbound settles the pull request a GitHub delivery says merged or
// closed, on the tasks waiting on it. A failure is logged and left to the
// poll, which settles what a delivery could not.
func (e *Engine) observeInbound(ctx context.Context, source string, w types.RawWebhook) {
	if source != github.Backend {
		return
	}
	page, merged, at, ok := github.SettledPull(w)
	if !ok {
		return
	}
	ref, ok := e.ChangeRef(page)
	if !ok {
		return
	}
	state := worklet.ChangeClosed
	if merged {
		state = worklet.ChangeMerged
	}
	settled, held, err := e.settleChange(ctx, ref, worklet.ChangeSettlement{State: state, At: at,
		Source: worklet.SettledByWebhook})
	switch {
	case err != nil:
		log.WarnContext(ctx, "change_settle_failed", "change", ref, "state", state, "error", err,
			"detail", "the poll of open changes settles it instead")
	case held && settled > 0:
		log.InfoContext(ctx, "change_settled", "change", ref, "state", state, "tasks", settled,
			"source", worklet.SettledByWebhook)
	}
}

// The poll of open changes: what no webhook delivered is learned from the host.
const (
	// changePollBatch bounds one pass: the open changes it asks the host about.
	changePollBatch = 50
	// changePollGrace leaves a change settled this recently to its webhook,
	// so a settle by the poll is one a delivery missed for at least this long.
	changePollGrace = 10 * time.Minute
)

// changePoll is where the last pass stopped, so the next resumes after it.
type changePoll struct {
	mu    sync.Mutex
	after string
}

// openChangesPending is the poll's gate: a GitHub wiring, and a change some
// task still waits on.
func (e *Engine) openChangesPending(ctx context.Context) (bool, error) {
	n := e.native.Load()
	if n == nil || n.trackerReader == nil || e.githubPulls.current() == nil {
		return false, nil
	}
	open, err := n.trackerReader.OpenChanges(ctx, "", 1, statelog.ReadStale)
	return len(open) > 0, err
}

// pollChanges asks the host about up to changePollBatch open changes, after
// the last pass's, through the agents' apps' cached tokens, and settles each
// that merged or closed more than changePollGrace ago.
func (e *Engine) pollChanges(ctx context.Context, now, _ time.Time) (int64, error) {
	n, lookup := e.native.Load(), e.githubPulls.current()
	if n == nil || n.trackerReader == nil || lookup == nil {
		return 0, nil
	}
	e.changePoll.mu.Lock()
	after := e.changePoll.after
	e.changePoll.mu.Unlock()
	refs, err := n.trackerReader.OpenChanges(ctx, after, changePollBatch, statelog.ReadStale)
	if err != nil {
		return 0, err
	}
	next := ""
	if len(refs) == changePollBatch {
		next = refs[len(refs)-1]
	}
	var settled int64
	for _, ref := range refs {
		pull, _, ok := github.ChangeRef(lookup.Opts.WebBase, ref)
		if !ok {
			continue
		}
		pr, err := e.githubPulls.read(ctx, pull)
		var refused *github.APIError
		switch {
		case errors.As(err, &refused) && refused.RateLimited:
			// THE HOST ASKED FOR A WAIT: the rest wait for the next pass.
			next = ""
		case err != nil:
			log.DebugContext(ctx, "change_poll_unread", "change", ref, "error", err)
			continue
		}
		if err != nil {
			break
		}
		s, done := settledState(pr)
		if !done || now.Sub(s.At) < changePollGrace {
			continue
		}
		count, _, err := e.settleChange(ctx, ref, s)
		if err != nil {
			return settled, err
		}
		settled += int64(count)
	}
	e.changePoll.mu.Lock()
	e.changePoll.after = next
	e.changePoll.mu.Unlock()
	return settled, nil
}

// settledState is a pull request's settle as the poll read it, and whether it
// has settled.
func settledState(pr github.PullRequest) (worklet.ChangeSettlement, bool) {
	switch {
	case pr.Merged && pr.MergedAt != nil:
		return worklet.ChangeSettlement{State: worklet.ChangeMerged, At: pr.MergedAt.UTC(),
			Source: worklet.SettledByPoll}, true
	case pr.State == "closed" && pr.ClosedAt != nil:
		return worklet.ChangeSettlement{State: worklet.ChangeClosed, At: pr.ClosedAt.UTC(),
			Source: worklet.SettledByPoll}, true
	}
	return worklet.ChangeSettlement{}, false
}
