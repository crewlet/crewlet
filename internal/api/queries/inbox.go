// One person's inbox, and the rule for whose inbox a caller may read.

package queries

import (
	"context"
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// workInbox answers a page of one person's notices.
//
// THE READER HAS ALWAYS EXISTED (`worklet.Reader.Inbox`, written, tested and
// swept on a 365-day retention), and nothing registered it as a question, so
// the richest record this engine keeps about a person reached no screen. Each
// notice names the ONE reason of eighteen under which it found them, whether it
// ASKS something or merely informs, whether it arrived only because nobody
// better was found, and the person's own read and snooze marks. No commercial
// tracker records why a notification reached you; this one always has.
//
// THE TRACKER'S, AND SCOPED. It is a question of the tracker's records, so it
// takes the tracker; within it, see [Sources.recordHandle]: a caller reads
// their own record, and naming anybody else's is [authz.ActionInboxRead] —
// theirs, whoever leads them, or an admin. Gated on admin alone, the landing
// screen would be the most-gated screen in the product, and the human teammate
// — one of the two readers this dashboard is for — fictional.
func (s Sources) workInbox(ctx context.Context, p Params) (any, error) {
	handle, err := s.recordHandle(ctx, authz.ActionInboxRead,
		strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	q := worklet.InboxQuery{
		Handle:      handle,
		Unread:      p.Bool("unread", false),
		PrimaryOnly: p.Bool("primary_only", false),
		Limit:       p.Int("limit", 0),
		Cursor:      strings.TrimSpace(p.String("cursor")),
	}
	// A SNOOZE MEANS "NOT NOW", so the default hides them; `include` keeps
	// them marked and `only` is the list of what was put off. A value this
	// build does not know is refused naming the three rather than read as
	// the default — `snoozed=ony` answering the whole inbox is the exact
	// defect the scope replaced.
	q.Snoozed = worklet.SnoozeExclude
	if raw := strings.TrimSpace(p.String("snoozed")); raw != "" {
		q.Snoozed = worklet.SnoozeScope(raw)
		if !q.Snoozed.Valid() {
			return nil, badParams("snoozed", raw, snoozeScopeNames())
		}
	}
	// REASONS FILTER, they do not classify: the primary split is a
	// classification of the same rows, and these narrow which rows are
	// returned at all.
	for _, name := range splitList(p.String("reasons")) {
		reason := worklet.Reason(name)
		if !reason.Valid() {
			return nil, badParams("reasons", name, reasonNames())
		}
		q.Reasons = append(q.Reasons, reason)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	q.Level, q.MaxLag, q.MaxLagSeq = fresh.Level, fresh.MaxLag, fresh.MaxLagSeq
	q.MinPosition = fresh.MinPosition
	if since := strings.TrimSpace(p.String("since")); since != "" {
		// A POSITION, not an instant, and the WHOLE triple.
		//
		// The caller resumes from where they marked read, and that
		// watermark is a log position — `<stream>@<generation>:<sequence>`,
		// exactly what the answer's own `seen_through` renders and what
		// `work_activity` already takes. A bare sequence number was the
		// first shape here and it is unusable: the comparison is on the
		// PACKED form, which is `(generation << 40) | seq`, so a sequence
		// carrying generation 0 sorts below every position on a stream
		// that has been reanchored — and the resume that was meant to skip
		// what somebody read re-delivers all of it instead.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		at, err := statelog.ParsePosition(since)
		if err != nil {
			return nil, badParams("since", since, nil)
		}
		q.Since = at
	}
	answer, err := s.Work.Inbox(ctx, q, s.clock())
	switch {
	case errors.Is(err, worklet.ErrBadQuery):
		// THE CALLER'S, like a malformed `since` above: a cursor that is
		// not a log position, or a position from another log. The cursor
		// is parsed by the reader, and it used to be parsed inside the
		// read's transaction, so a pasted-wrong cursor was a 500.
		return nil, refusedBy(err, builtin.ReadRefusalSentence(err))
	case err != nil:
		return nil, err
	}
	return answer, nil
}

// snoozeScopeNames is the three, for the same kind of refusal.
func snoozeScopeNames() []string {
	out := make([]string, 0, len(worklet.SnoozeScopes))
	for _, scope := range worklet.SnoozeScopes {
		out = append(out, string(scope))
	}
	return out
}

// reasonNames is the eighteen, for a refusal that says what would have worked.
func reasonNames() []string {
	out := make([]string, 0, len(worklet.Reasons))
	for _, r := range worklet.Reasons {
		out = append(out, string(r))
	}
	return out
}
