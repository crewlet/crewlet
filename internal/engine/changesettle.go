package engine

import (
	"cmp"
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
	ref, refused := e.ChangeRef(page)
	if ref == "" {
		if refused != "" {
			log.DebugContext(ctx, "change_settle_not_the_companys", "change", page)
		}
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

// changePoll is where the last pass stopped, so the next resumes after it,
// and the changes the host would not let it read, each asked again later.
type changePoll struct {
	mu         sync.Mutex
	after      string
	unreadable map[string]unreadableChange
	// batch overrides changePollBatch, for a test.
	batch int
}

// unreadableChange is when a change the host refused is next asked about, and
// how long the wait before that was.
type unreadableChange struct {
	until time.Time
	wait  time.Duration
}

// The back-off of a change the host refused to show: an hour, doubling to a day.
const (
	unreadableFirst = time.Hour
	unreadableMost  = 24 * time.Hour
)

// openChangesPending is the poll's gate: a GitHub wiring, and a change some
// task still waits on.
func (e *Engine) openChangesPending(ctx context.Context) (bool, error) {
	n := e.native.Load()
	if lookup, _ := e.githubPulls.current(); n == nil || n.trackerReader == nil || lookup == nil {
		return false, nil
	}
	open, err := n.trackerReader.OpenChanges(ctx, "", 1, statelog.ReadStale)
	return len(open) > 0, err
}

// pollChanges asks the host about up to changePollBatch open changes under
// the company's owners, after the last pass's, through the agents' apps'
// cached tokens, and settles each that merged or closed more than
// changePollGrace ago. A rate limit ends the pass where it stood.
func (e *Engine) pollChanges(ctx context.Context, now, _ time.Time) (int64, error) {
	n := e.native.Load()
	if lookup, _ := e.githubPulls.current(); n == nil || n.trackerReader == nil || lookup == nil {
		return 0, nil
	}
	e.changePoll.mu.Lock()
	after, batch := e.changePoll.after, cmp.Or(e.changePoll.batch, changePollBatch)
	e.changePoll.mu.Unlock()
	refs, err := n.trackerReader.OpenChanges(ctx, after, batch, statelog.ReadStale)
	if err != nil {
		return 0, err
	}
	next := ""
	if len(refs) == batch {
		next = refs[len(refs)-1]
	}
	reached, settled := after, int64(0)
	for _, ref := range refs {
		s, done, limited := e.pollChange(ctx, ref, now)
		if limited {
			// THE HOST ASKED FOR A WAIT: the next pass starts at this change.
			next = reached
			break
		}
		reached = ref
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

// pollChange reads one open change: its settle where it has settled, and
// whether the host asked for a wait. A change outside the company's owners is
// never read, and one the host refused waits out its back-off, said once.
func (e *Engine) pollChange(ctx context.Context, ref string, now time.Time) (worklet.ChangeSettlement, bool, bool) {
	lookup, _ := e.githubPulls.current()
	pull, _, ok := github.ChangeRef(lookup.Opts.WebBase, ref)
	if !ok || !e.ownedChange(ref) {
		return worklet.ChangeSettlement{}, false, false
	}
	e.changePoll.mu.Lock()
	held, waiting := e.changePoll.unreadable[ref]
	e.changePoll.mu.Unlock()
	if waiting && now.Before(held.until) {
		return worklet.ChangeSettlement{}, false, false
	}
	pr, err := e.githubPulls.read(ctx, pull)
	var refused *github.APIError
	switch {
	case errors.As(err, &refused) && refused.RateLimited:
		return worklet.ChangeSettlement{}, false, true
	case err != nil:
		wait := unreadableFirst
		if waiting {
			wait = min(2*held.wait, unreadableMost)
		} else {
			log.WarnContext(ctx, "change_poll_unreadable", "change", ref, "error", err,
				"detail", "the tasks waiting on it stay blocked until the agents' apps can read it "+
					"or a person removes the wait; it is asked again after a growing wait")
		}
		e.changePoll.mu.Lock()
		if e.changePoll.unreadable == nil {
			e.changePoll.unreadable = map[string]unreadableChange{}
		}
		e.changePoll.unreadable[ref] = unreadableChange{until: now.Add(wait), wait: wait}
		e.changePoll.mu.Unlock()
		return worklet.ChangeSettlement{}, false, false
	}
	if waiting {
		e.changePoll.mu.Lock()
		delete(e.changePoll.unreadable, ref)
		e.changePoll.mu.Unlock()
	}
	s, done := settledState(pr)
	return s, done, false
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
