package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// refsNearLimit reads code_refs_near_limit's figure from this node's code log
// rows at now ([code.Reader.RefsNearLimit]), and false where this node
// measures none: a node with no runtime holding the code log's rows — no data
// role — and one whose company hosts no code, whose repositories take no push
// for the bound to refuse.
//
// EVERY DATA NODE, never only a git node: the figure is a fact of the estate
// each holds, so every data node reports the same repositories, as an estate's
// own alarms are reported — and a fleet with no git node live still has
// repositories near the bound, whose pushes wait on one.
func (e *Engine) refsNearLimit(ctx context.Context, now time.Time) (code.RefsNear, bool, error) {
	n := e.native.Load()
	c := e.Company()
	if n == nil || n.codeReader == nil || c == nil || c.Config == nil || !c.Config.Code.Native {
		return code.RefsNear{}, false, nil
	}
	near, err := n.codeReader.RefsNearLimit(ctx, now)
	if err != nil {
		return code.RefsNear{}, false, err
	}
	return near, true, nil
}

// measureRefs takes code_refs_near_limit's figure on the trim's tick and keeps
// it for the tick's reports, the vector coverage's idiom
// ([retention.measureCoverage]).
//
// ON THE TICK AND NEVER ON A REPORT: the reading is a walk of the
// repositories and a count over each one's refs near the bound — up to
// code.MaxRefsPerRepo index entries a repository — while a report is assembled
// on every alarm beat, operator request and dashboard poll, and the figure
// moves over days: a quarter-hour is a resolution it is honest at.
//
// A MEASUREMENT THAT FAILS LEAVES NOTHING rather than the last figure, and is
// said once a tick (`code_refs_near_limit_unread`): an unread estate is not one
// near the bound, and the push decide still refuses a create past it
// repo_refs, naming the bound, whatever the alarm knows.
func (r *retention) measureRefs(ctx context.Context) {
	if r.refs == nil {
		return
	}
	near, known, err := r.refs(ctx, r.clock())
	if err != nil {
		r.logs().WarnContext(ctx, "code_refs_near_limit_unread", "error", err.Error(),
			"detail", "code_refs_near_limit is not raised until the next trim tick reads the rows again")
		near, known = code.RefsNear{}, false
	}
	r.mu.Lock()
	r.refsNear, r.refsNearKnown = near, known
	r.mu.Unlock()
}

// refsReading copies the tick's code_refs_near_limit figure into out, with the
// two decisions it was judged at — the bound and the lifecycle's window — which
// the detail names and this framework cannot import. Nothing where the tick
// measured none.
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
	out.CodeRefsLimit = code.MaxRefsPerRepo
	out.CodeRefsWindow = code.RefExpiry
}
