// One person's inbox, and the rule for whose inbox a caller may read.

package queries

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// workInbox answers a page of one person's notices.
//
// THE READER HAS ALWAYS EXISTED — `tracker.Reader.Inbox`, written, tested and
// swept on a 365-day retention — and nothing registered it as a question, so
// the richest record this engine keeps about a person reached no screen. Each
// notice names the ONE reason of nineteen under which it found them, whether it
// ASKS something or merely informs, whether it arrived only because nobody
// better was found, and the person's own read and snooze marks. No commercial
// tracker records why a notification reached you; this one always has.
//
// SCOPED, NOT OPERATOR-GATED. See [Sources.viewerParty]: a caller reads the
// seat their own token is bound to, and naming anybody else's needs an
// operator credential. Registering it operator-only would make the landing
// screen the most-gated screen in the product, and the human teammate — one of
// the two readers this dashboard is for — fictional.
func (s Sources) workInbox(ctx context.Context, p Params) (any, error) {
	who, err := s.viewerParty(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	q := tracker.InboxQuery{
		// BOTH OF THIS PERSON'S NAMES — see [Sources.viewerParty]. The
		// notices a founder's own assistant produced name the token,
		// and an inbox asked about the seat alone showed none of them.
		Who: who,
		// A SNOOZE MEANS "NOT NOW", so the default hides them and the
		// reader asks for them explicitly — the reader returns one whose
		// time has come either way.
		IncludeSnoozed: p.Bool("include_snoozed", false),
		Unread:         p.Bool("unread", false),
		PrimaryOnly:    p.Bool("primary_only", false),
		Limit:          p.Int("limit", 0),
		Cursor:         strings.TrimSpace(p.String("cursor")),
	}
	// REASONS FILTER, they do not classify: the primary split is a
	// classification of the same rows, and these narrow which rows are
	// returned at all.
	for _, name := range splitList(p.String("reasons")) {
		reason := tracker.Reason(name)
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
		at, err := tracker.ParseLogPosition(since)
		if err != nil {
			return nil, badParams("since", since, nil)
		}
		q.Since = at
	}
	answer, err := s.Work.Inbox(ctx, q, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// reasonNames is the nineteen, for a refusal that says what would have worked.
func reasonNames() []string {
	out := make([]string, 0, len(tracker.Reasons))
	for _, r := range tracker.Reasons {
		out = append(out, string(r))
	}
	return out
}
