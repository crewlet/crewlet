// One person's inbox, and the rule for whose inbox a caller may read.

package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// workInbox answers a page of one person's notices.
//
// Each notice (`worklet.Reader.Inbox`, kept 365 days) names the ONE reason of
// eighteen under which it found the person, whether it ASKS something or
// merely informs, whether it arrived only because nobody better was found, and
// the person's own read and snooze marks.
//
// THE TRACKER'S, AND SCOPED. It is a question of the tracker's records, so it
// takes the tracker; see [Sources.recordHandle]: a caller reads their own
// record, and naming anybody else's is [authz.ActionInboxRead] (theirs,
// whoever leads them, or an admin). Gated on admin alone, the landing screen
// would be the most-gated in the product and the human teammate fictional.
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
		reason := worklet.Reason(name)
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
	out := make([]string, 0, len(inbox.SnoozeScopes))
	for _, scope := range inbox.SnoozeScopes {
		out = append(out, string(scope))
	}
	return out
}

// reasonNames is the nineteen, for a refusal that says what would have worked.
func reasonNames() []string {
	out := make([]string, 0, len(worklet.Reasons))
	for _, r := range worklet.Reasons {
		out = append(out, string(r))
	}
	return out
}

// composedInbox answers `inbox`: one page of a person's notices from EVERY
// native domain that told them something — the tracker's, Gitlet's and the
// artifacts' comments — interleaved by when each happened ([inbox.Compose]).
//
// OPEN TO EVERY SIGNED-IN PERSON, each source under its own permission. Whose
// inbox is decided by [authz.ActionInbox] WITH NO APP: a person who uses no
// tracker still hears of changes they must review. Worklet's source needs
// [authz.ActionInboxRead]; Gitlet's the gitlet permission (ADR-0110) and a grant
// on each repository; the artifacts' each artifact's access. A source not read
// is named in `sources`. ONE FLOOR PER LOG: "mark all read" writes on the tracker's and the code log,
// so the read takes a floor on each ([statelog.Freshness.Floors]), none other.
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
		Limit: p.Int("limit", 0), Cursor: strings.TrimSpace(p.String("cursor")), Snoozed: inbox.SnoozeExclude,
		Record: strings.TrimSpace(p.String("record")), Cards: p.Bool("cards", false)}
	// ONE NOTICE HAS NO NEXT PAGE, so a cursor beside a record names nothing.
	if q.Record != "" && q.Cursor != "" {
		return nil, fmt.Errorf("%w: a read of one record has no next page, so it takes no cursor: drop "+
			"cursor or record", ErrBadParams)
	}
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
	if err = fresh.OnlyOn(topics.TrackerLogStream, topics.CodeLogStream, topics.ArtifactsLogStream); err != nil {
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
	// THE CODE HALF IS GITLET'S, as the tracker's is Worklet's: a caller
	// without the app is listed and counted no code notice (ADR-0110). Decided
	// as Gitlet's rows are ([authz.ActionCodeRead]) — whose inbox it is was
	// decided for the whole inbox above, so the app is all that is left.
	codeHalf := authz.Decide(ctx, principal, authz.ActionCodeRead, authz.Object{}, s.Chart, s.clock())
	if codeHalf.Unknown() {
		return nil, fmt.Errorf("%w: this node cannot decide whether the caller uses Gitlet yet: %w",
			ErrUnavailable, codeHalf.Err)
	}
	if s.Code != nil && codeHalf.Allowed {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
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
	if s.Artifacts != nil && s.Artifacts.Reads != nil {
		gate := s.Artifacts.Gate
		gate.Now = s.clock
		sources = append(sources, artifactsInbox{reads: s.Artifacts.Reads, gate: gate, asker: principal,
			fresh: fresh.On(topics.ArtifactsLogStream), now: s.clock()})
	}
	page, err := inbox.Compose(ctx, q, sources)
	var refused codeReadRefused
	switch {
	case errors.Is(err, inbox.ErrBadCursor), errors.Is(err, worklet.ErrBadQuery),
		errors.Is(err, artifactsdomain.ErrBadCursor):
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
	answer, err := t.work.Inbox(ctx, worklet.InboxQuery{Handle: q.Handle, Unread: q.Unread,
		PrimaryOnly: q.PrimaryOnly, Snoozed: q.Snoozed, Record: q.Record, Limit: q.Limit, Cursor: q.Cursor,
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

// codeInbox is Gitlet's half of a composed inbox: a page of its
// `inbox` read, asked as the caller — each notice held to their grant on its
// repository — at the floor the caller named on the code log.
type codeInbox struct {
	reads CodeReads
	asker gitlet.Principal
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
	answer, err := c.reads.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: c.asker, Cursor: q.Cursor,
		Limit: q.Limit, Fresh: c.fresh, Inbox: &gitlet.InboxQuery{Handle: q.Handle, Unread: q.Unread,
			PrimaryOnly: q.PrimaryOnly, Snoozed: q.Snoozed, Record: q.Record, Now: c.now}})
	if err != nil {
		return inbox.SourcePage{}, codeReadRefused{err: err}
	}
	if answer.Inbox == nil {
		return inbox.SourcePage{}, errors.New("queries: Gitlet answered an inbox read with no page")
	}
	got := answer.Inbox
	// A CODE READ IS COMPLETE OR REFUSED: the code reader answers no partial
	// page, so the level it was served at is the whole of its coverage.
	page := inbox.SourcePage{Next: answer.Next, SeenThrough: got.SeenThrough, PrimaryReasons: []string{},
		Notices: make([]inbox.Notice, 0, len(got.Notices)), Level: answer.Level, Complete: true,
		At: answer.Position}
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

// Cards is Gitlet's cards of notices, a composed page's code notices:
// ONE `notice_cards` read of their changes, asked as the caller and floored
// at the position Gitlet's page was read at ([inbox.Carded]), each
// change's card in place of what its notices carried of it. A change the
// card read does not answer — gone since the page, or on a repository the
// caller may no longer read — keeps what its notice carried.
func (c codeInbox) Cards(ctx context.Context, page inbox.SourcePage, notices []inbox.Notice) ([]inbox.Notice,
	error) {

	var changes []string
	saids := make([]gitlet.InboxNotice, len(notices))
	for i, n := range notices {
		said, ok := n.Extra.(gitlet.InboxNotice)
		if !ok {
			return nil, fmt.Errorf("queries: a code notice carries %T, not Gitlet's notice", n.Extra)
		}
		saids[i] = said
		if !slices.Contains(changes, said.ChangeID) {
			changes = append(changes, said.ChangeID)
		}
	}
	if len(changes) == 0 {
		return notices, nil
	}
	// NO OLDER THAN THE PAGE: the ask's level and bounds, its floor raised
	// to where the page was read — at or past any floor the caller named on
	// the code log, which the page was read past.
	fresh := c.fresh
	if !page.At.IsZero() {
		fresh.Floors = []statelog.Position{page.At}
	}
	answer, err := c.reads.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadNoticeCards, Asker: c.asker, Fresh: fresh,
		NoticeCards: changes})
	if err != nil {
		return nil, codeReadRefused{err: err}
	}
	out := make([]inbox.Notice, len(notices))
	for i, n := range notices {
		said := saids[i]
		if card, found := answer.NoticeCards[said.ChangeID]; found && card != nil {
			said.Change = card
			n.Subject.Title = card.Title
			n.Extra = said
		}
		out[i] = n
	}
	return out, nil
}

// artifactsInbox is the artifacts' half of a composed inbox: a page of the
// person's comment notices, each held to the caller's access to its artifact
// in the page's snapshot, so nothing of an artifact they cannot read shows.
type artifactsInbox struct {
	reads ArtifactsReader
	gate  artifactsapi.Gate
	asker iam.Principal
	fresh statelog.Freshness
	now   time.Time
}

func (artifactsInbox) Name() string { return artifactsdomain.Domain{}.Name() }

func (a artifactsInbox) Notices(ctx context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	got, err := a.reads.Inbox(ctx, artifactsdomain.InboxQuery{Handle: q.Handle, Unread: q.Unread,
		PrimaryOnly: q.PrimaryOnly, Snoozed: q.Snoozed, Record: q.Record, Limit: q.Limit, Cursor: q.Cursor,
		Now: a.now, Fresh: a.fresh})
	if err != nil {
		return inbox.SourcePage{}, err
	}
	// A PAGE MAY LEAVE NOTICES OUT, and resumes below them at its own cursor
	// (internal/inbox's head): one decision an artifact the page names.
	shown := map[string]bool{}
	for id, head := range got.Heads {
		kept, err := a.gate.Visible(ctx, a.asker, []artifactsdomain.ListItem{{Head: head}})
		if err != nil {
			return inbox.SourcePage{}, fmt.Errorf("%w: decide who reads the artifacts an inbox page names: %w",
				ErrUnavailable, err)
		}
		shown[id] = len(kept) == 1
	}
	page := inbox.SourcePage{Next: got.Next, SeenThrough: got.SeenThrough, PrimaryReasons: []string{},
		Notices: make([]inbox.Notice, 0, len(got.Notices)), Level: got.Level, Complete: got.Complete,
		At: got.Position}
	for _, r := range got.PrimaryReasons {
		page.PrimaryReasons = append(page.PrimaryReasons, string(r))
	}
	for _, n := range got.Notices {
		if !shown[n.Artifact] {
			continue
		}
		page.Notices = append(page.Notices, inbox.Notice{RecordID: n.RecordID,
			Position: inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq},
			At:       n.At, Reason: string(n.Reason), Primary: n.Primary, Addressed: n.Addressed,
			Kind: string(artifactsdomain.KindComment), Subject: inbox.Subject{ID: n.Artifact, Title: n.Title},
			Excerpt: n.Excerpt, Actor: n.Actor, ActorKind: string(n.ActorKind), Read: n.Read, Snoozed: n.Snoozed,
			SnoozedUntil: n.SnoozedUntil, Extra: n})
	}
	return page, nil
}

// Cards is each kept notice with its thread as it stands: ONE read of the
// page's threads, floored at the position the page was read at. A thread no
// row holds any more keeps what its notice carried.
func (a artifactsInbox) Cards(ctx context.Context, page inbox.SourcePage, notices []inbox.Notice) ([]inbox.Notice,
	error) {

	saids := make([]artifactsdomain.InboxNotice, len(notices))
	var refs []artifactsdomain.ThreadRef
	for i, n := range notices {
		said, ok := n.Extra.(artifactsdomain.InboxNotice)
		if !ok {
			return nil, fmt.Errorf("queries: an artifacts notice carries %T, not the artifacts' notice", n.Extra)
		}
		saids[i] = said
		ref := artifactsdomain.ThreadRef{Artifact: said.Artifact, Thread: said.Thread}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return notices, nil
	}
	fresh := a.fresh
	if !page.At.IsZero() {
		fresh.Floors = []statelog.Position{page.At}
	}
	threads, err := a.reads.NoticeThreads(ctx, refs, fresh)
	if err != nil {
		return nil, err
	}
	byRef := make(map[artifactsdomain.ThreadRef]*artifactsdomain.NoticeThread, len(refs))
	for i, ref := range refs {
		if i < len(threads) {
			byRef[ref] = threads[i]
		}
	}
	out := make([]inbox.Notice, len(notices))
	for i, n := range notices {
		said := saids[i]
		if t := byRef[artifactsdomain.ThreadRef{Artifact: said.Artifact, Thread: said.Thread}]; t != nil {
			said.ThreadState = t
			n.Extra = said
		}
		out[i] = n
	}
	return out, nil
}
