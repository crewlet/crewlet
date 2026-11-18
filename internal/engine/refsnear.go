package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// refsNearLimit reads code_refs_near_limit's figure from this node's code log
// rows at now ([gitlet.Reader.RefsNearLimit]), and false where this node
// measures none: no data role, or a company that hosts no code.
//
// EVERY DATA NODE, never only a Gitlet node: the figure is a fact of the estate
// each holds, as an estate's own alarms are, and a fleet with no Gitlet node
// live still has repositories near the bound.
func (e *Engine) refsNearLimit(ctx context.Context, now time.Time) (gitlet.RefsNear, bool, error) {
	n := e.native.Load()
	c := e.Company()
	if n == nil || n.codeReader == nil || c == nil || c.Config == nil || c.Config.CodeBackendFor() != config.CodeGitlet {
		return gitlet.RefsNear{}, false, nil
	}
	near, err := n.codeReader.RefsNearLimit(ctx, now)
	if err != nil {
		return gitlet.RefsNear{}, false, err
	}
	return near, true, nil
}

// measureRefs takes code_refs_near_limit's figure on the trim's tick and keeps
// it for the tick's reports, as [retention.measureCoverage] does.
//
// ON THE TICK, NEVER ON A REPORT: the reading walks every repository's live
// refs and tombstones (up to gitlet.MaxRefsPerRepo +
// gitlet.MaxTombstonesPerRepo index entries each), a report is built on every
// alarm beat, operator request and dashboard poll, and the figure moves over
// days, so a quarter-hour resolution is honest.
//
// A FAILED MEASUREMENT LEAVES NOTHING rather than the last figure, logged once
// a tick (`code_refs_near_limit_unread`): an unread estate is not one near the
// bound, and the push decide refuses a create at it (repo_refs) regardless.
func (r *retention) measureRefs(ctx context.Context) {
	if r.refs == nil {
		return
	}
	near, known, err := r.refs(ctx, r.clock())
	if err != nil {
		r.logs().WarnContext(ctx, "code_refs_near_limit_unread", "error", err.Error(),
			"detail", "code_refs_near_limit is not raised until the next trim tick reads the rows again")
		near, known = gitlet.RefsNear{}, false
	}
	r.mu.Lock()
	r.refsNear, r.refsNearKnown = near, known
	r.mu.Unlock()
}

// refsReading copies the tick's code_refs_near_limit figure into out, with the
// bound and the lifecycle's window it was judged at, which the detail names
// and statelog cannot import. Nothing where the tick measured none.
func (r *retention) refsReading(out *statelog.Reading) {
	r.mu.Lock()
	near, known := r.refsNear, r.refsNearKnown
	r.mu.Unlock()
	if !known {
		return
	}
	out.CodeRefsNearLimit = near.Repos
	out.CodeRefsNearLimitRepo = near.Worst
	out.CodeRefsNearLimitLive = near.Live
	out.CodeRefsNearLimitRecent = near.Recent
	out.CodeRefsLimit = gitlet.MaxRefsPerRepo
	out.CodeRefsWindow = gitlet.RefExpiry
}
