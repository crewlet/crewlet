// One person's inbox, and the rule for whose inbox a caller may read.

package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// workInbox answers a page of one person's notices.
//
// THE READER HAS ALWAYS EXISTED — `tracker.Reader.Inbox`, written, tested and
// swept on a 365-day retention — and nothing registered it as a question, so
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
	q := tracker.InboxQuery{
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
	q.Snoozed = inbox.SnoozeExclude
	if raw := strings.TrimSpace(p.String("snoozed")); raw != "" {
		q.Snoozed = inbox.SnoozeScope(raw)
		if !q.Snoozed.Valid() {
			return nil, badParams("snoozed", raw, snoozeScopeNames())
		}
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
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	q.Level, q.MaxLag, q.MaxLagSeq = fresh.Level, fresh.MaxLag, fresh.MaxLagSeq
	q.Floors = fresh.Floors
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
	case errors.Is(err, tracker.ErrBadQuery):
		// THE CALLER'S, like a malformed `since` above: a cursor that is
		// not a log position, or a position from another log. The cursor
		// is parsed by the reader, and it used to be parsed inside the
		// read's transaction, so a pasted-wrong cursor was a 500.
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, err
	}
	return answer, nil
}

// snoozeScopeNames is the three, for the same kind of refusal.
func snoozeScopeNames() []string {
	out := make([]string, 0, len(inbox.SnoozeScopes))
	for _, scope := range inbox.SnoozeScopes {
		out = append(out, string(scope))
	}
	return out
}

// reasonNames is the eighteen, for a refusal that says what would have worked.
func reasonNames() []string {
	out := make([]string, 0, len(tracker.Reasons))
	for _, r := range tracker.Reasons {
		out = append(out, string(r))
	}
	return out
}

// composedInbox answers `inbox`: one page of a person's notices from EVERY
// native domain that told them something — the tracker's and the code
// host's — interleaved by when each happened ([inbox.Compose]).
//
// # Open to every signed-in person, each source under its own permission
//
// Whose inbox is decided first, by the composed question's own verb
// ([authz.ActionInbox]): a caller reads their own, and naming anybody else's
// is the owner-or-lead rule — theirs, whoever leads them, or an admin — WITH
// NO APP, since an inbox is no app's: a person who uses no tracker still
// hears about the changes they are asked to review. Then each source is read
// only where its own permission holds: the tracker's notices where the
// company keeps its tracker here and the caller holds the tracker app on that
// record ([authz.ActionInboxRead], `work_inbox`'s own rule); the code host's
// where the company hosts code, each notice held by the code host to the
// caller's grant on its repository. A source the caller may not read is not
// asked, and the answer's `sources` says which were.
//
// # One floor per log
//
// A screen reads its own marks back at `session` with a floor on EACH log it
// wrote — a "mark all read" is a record on the tracker's log and one on the
// code log — so the read takes a floor on each ([statelog.Freshness.Floors]),
// refuses one on any other log, and hands each half the floor on its own.
//
// `work_inbox` stays the tracker's own question, and its tool.
func (s Sources) composedInbox(ctx context.Context, p Params) (any, error) {
	principal, err := caller(ctx, "inbox")
	if err != nil {
		return nil, err
	}
	handle, err := s.recordHandle(ctx, authz.ActionInbox, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	q := inbox.Query{Handle: handle, Unread: p.Bool("unread", false), PrimaryOnly: p.Bool("primary_only", false),
		Limit: p.Int("limit", 0), Cursor: strings.TrimSpace(p.String("cursor")), Snoozed: inbox.SnoozeExclude}
	if raw := strings.TrimSpace(p.String("snoozed")); raw != "" {
		q.Snoozed = inbox.SnoozeScope(raw)
		if !q.Snoozed.Valid() {
			return nil, badParams("snoozed", raw, snoozeScopeNames())
		}
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	if err := fresh.OnlyOn(topics.TrackerLogStream, topics.CodeLogStream); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	var sources []inbox.Source
	kept, err := keptHere(s.Work)
	if err != nil {
		return nil, err
	}
	if kept {
		d := authz.Decide(ctx, principal, authz.ActionInboxRead,
			authz.Object{Kind: authz.KindPerson, ID: handle, Owner: handle}, s.Chart, s.clock())
		switch {
		case d.Unknown():
			return nil, fmt.Errorf("%w: this node cannot decide whether %s's work notices are the caller's to "+
				"read yet: %w", ErrUnavailable, handle, d.Err)
		case d.Allowed:
			sources = append(sources, trackerInbox{work: s.Work, fresh: fresh.On(topics.TrackerLogStream),
				now: s.clock()})
		}
	}
	if s.Code != nil {
		reads, err := s.Code.CodeReads()
		var notFound *NotFound
		switch {
		case errors.As(err, &notFound):
			// A COMPANY THAT HOSTS NO CODE has no code notices, and its
			// inbox is the rest.
		case err != nil:
			return nil, err
		default:
			sources = append(sources, codeInbox{reads: reads, asker: codeAsker(principal),
				fresh: fresh.On(topics.CodeLogStream), now: s.clock()})
		}
	}
	page, err := inbox.Compose(ctx, q, sources)
	var refused codeReadRefused
	switch {
	case errors.Is(err, inbox.ErrBadCursor), errors.Is(err, tracker.ErrBadQuery):
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case errors.As(err, &refused):
		return nil, codeRefusal("", refused.err)
	case err != nil:
		return nil, err
	}
	return page, nil
}

// trackerInbox is the tracker's half of a composed inbox: a page of
// `work_inbox`'s reader at the floor the caller named on the tracker's log.
type trackerInbox struct {
	work  WorkReader
	fresh statelog.Freshness
	now   time.Time
}

func (trackerInbox) Name() string { return "tracker" }

func (t trackerInbox) Notices(ctx context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	answer, err := t.work.Inbox(ctx, tracker.InboxQuery{Handle: q.Handle, Unread: q.Unread,
		PrimaryOnly: q.PrimaryOnly, Snoozed: q.Snoozed, Limit: q.Limit, Cursor: q.Cursor,
		Level: t.fresh.Level, MaxLag: t.fresh.MaxLag, MaxLagSeq: t.fresh.MaxLagSeq, Floors: t.fresh.Floors}, t.now)
	if err != nil {
		return inbox.SourcePage{}, err
	}
	page := inbox.SourcePage{Next: answer.NextCursor, SeenThrough: answer.SeenThrough,
		PrimaryReasons: []string{}, Notices: make([]inbox.Notice, 0, len(answer.Notices)),
		Level: answer.Level, Complete: answer.Complete}
	for _, r := range answer.PrimaryReasons {
		page.PrimaryReasons = append(page.PrimaryReasons, string(r))
	}
	for _, n := range answer.Notices {
		page.Notices = append(page.Notices, inbox.Notice{RecordID: n.RecordID,
			Position: inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq},
			At:       n.At, Reason: string(n.Reason), Primary: n.Primary, Addressed: n.Addressed,
			Fallback: n.Fallback, Kind: string(n.Kind),
			Subject: inbox.Subject{ID: n.SubjectID, Key: n.SubjectKey}, Excerpt: n.Excerpt,
			Actor: n.Actor, ActorKind: string(n.ActorKind), Read: n.Read, Snoozed: n.Snoozed,
			SnoozedUntil: n.SnoozedUntil, Extra: n})
	}
	return page, nil
}

// codeInbox is the code host's half of a composed inbox: a page of its
// `inbox` read, asked as the caller — each notice held to their grant on its
// repository — at the floor the caller named on the code log.
type codeInbox struct {
	reads CodeReads
	asker code.Principal
	fresh statelog.Freshness
	now   time.Time
}

func (codeInbox) Name() string { return "code" }

// codeReadRefused is a code read's own failure, kept whole through the
// composition so the question answers it in the code questions' vocabulary
// ([codeRefusal]).
type codeReadRefused struct{ err error }

func (e codeReadRefused) Error() string { return e.err.Error() }
func (e codeReadRefused) Unwrap() error { return e.err }

func (c codeInbox) Notices(ctx context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	answer, err := c.reads.Read(ctx, code.ReadQuery{Kind: code.ReadInbox, Asker: c.asker, Cursor: q.Cursor,
		Limit: q.Limit, Fresh: c.fresh, Inbox: &code.InboxQuery{Handle: q.Handle, Unread: q.Unread,
			PrimaryOnly: q.PrimaryOnly, Snoozed: q.Snoozed, Now: c.now}})
	if err != nil {
		return inbox.SourcePage{}, codeReadRefused{err: err}
	}
	if answer.Inbox == nil {
		return inbox.SourcePage{}, errors.New("queries: the code host answered an inbox read with no page")
	}
	got := answer.Inbox
	// A CODE READ IS COMPLETE OR REFUSED: the code reader answers no partial
	// page, so the level it was served at is the whole of its coverage.
	page := inbox.SourcePage{Next: answer.Next, SeenThrough: got.SeenThrough, PrimaryReasons: []string{},
		Notices: make([]inbox.Notice, 0, len(got.Notices)), Level: answer.Level, Complete: true}
	for _, r := range got.PrimaryReasons {
		page.PrimaryReasons = append(page.PrimaryReasons, string(r))
	}
	for _, n := range got.Notices {
		subject := inbox.Subject{ID: n.ChangeID, Key: n.ChangeRef}
		if n.Change != nil {
			subject.Title = n.Change.Title
		}
		page.Notices = append(page.Notices, inbox.Notice{RecordID: n.RecordID,
			Position: inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq},
			At:       n.At, Reason: string(n.Reason), Primary: n.Primary, Addressed: n.Addressed,
			Fallback: n.Fallback, Kind: "change", Subject: subject, Excerpt: n.Excerpt, Read: n.Read,
			Snoozed: n.Snoozed, SnoozedUntil: n.SnoozedUntil, Extra: n})
	}
	return page, nil
}
