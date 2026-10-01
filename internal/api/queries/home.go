// The three questions the landing screen asks and nothing else answered:
// the company's work as a series (`work_flow`), one merged feed of what the
// company did (`company_feed`), and what is waiting on the person reading
// (`decisions`).

package queries

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// DefaultFlowPoints is a fortnight of days: the landing screen's
// "completed per day" chart, and enough weeks behind a week-on-week delta.
const DefaultFlowPoints = 14

// workFlow answers the status and completion series — see [tracker.Reader.Flow].
func (s Sources) workFlow(ctx context.Context, p Params) (any, error) {
	bucket := period.Period(strings.TrimSpace(p.String("bucket")))
	if bucket == "" {
		bucket = period.Day
	}
	if bucket != period.Day && bucket != period.Week {
		return nil, badParams("bucket", string(bucket), []string{"day", "week"})
	}
	points := p.Int("points", DefaultFlowPoints)
	if points < 1 || points > tracker.MaxFlowPoints {
		return nil, badParams("points", p.String("points"),
			[]string{fmt.Sprintf("1 to %d", tracker.MaxFlowPoints)})
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	return s.Work.Flow(ctx, tracker.FlowQuery{
		Project: strings.TrimSpace(p.String("project")),
		Bucket:  bucket, Points: points,
		Level: fresh.Level, MaxLag: fresh.MaxLag, MaxLagSeq: fresh.MaxLagSeq,
		MinPosition: fresh.MinPosition,
	}, s.clock(), s.zone())
}

// ---- company_feed ---------------------------------------------------------

// FeedKind is one kind of row the company feed merges.
type FeedKind string

// The five kinds: three from the tracker, one from each of the pages and the
// schedules' runs.
const (
	FeedCompleted FeedKind = "completed"
	FeedCreated   FeedKind = "created"
	FeedHandoff   FeedKind = "handoff"
	FeedSchedule  FeedKind = "schedule"
	FeedPage      FeedKind = "page"
)

// FeedKinds are the five, in the order the wire documents them.
var FeedKinds = []FeedKind{FeedCompleted, FeedCreated, FeedHandoff, FeedSchedule, FeedPage}

// Valid reports whether a kind off the wire is one of the five.
func (k FeedKind) Valid() bool { return slices.Contains(FeedKinds, k) }

// DefaultFeedPage is what one screen of the landing feed shows before "Load
// older"; the ceiling is the tracker's [tracker.MaxFeedPage], shared by every
// source so one merged page never needs more than one page of any of them.
const DefaultFeedPage = 20

// The three sources, as the cursor names them.
const (
	feedSourceWork     = "work"
	feedSourcePages    = "pages"
	feedSourceSchedule = "schedule"
)

// feedEnd marks a source a cursor has read to its end.
const feedEnd = "end"

// FeedEntry is one merged row. Exactly one of the three bodies is set, the
// one its kind belongs to.
type FeedEntry struct {
	Kind FeedKind  `json:"kind"`
	At   time.Time `json:"at"`

	Work     *tracker.FeedRow `json:"work,omitempty"`
	Page     *FeedPageChange  `json:"page,omitempty"`
	Schedule *FeedScheduleRun `json:"schedule,omitempty"`
}

// FeedPageChange is a page created or saved.
type FeedPageChange struct {
	ID        string           `json:"id"`
	PageID    string           `json:"page_id"`
	Title     string           `json:"title,omitempty"`
	Container string           `json:"container,omitempty"`
	Change    pages.ChangeKind `json:"change"`
	Actor     string           `json:"actor,omitempty"`
	ActorKind string           `json:"actor_kind,omitempty"`
	TurnID    string           `json:"turn_id,omitempty"`
}

// FeedScheduleRun is one run of one schedule — or several in a row.
//
// A RUN IS A FIRE THE SCHEDULER DISPATCHED. The ledger also records the ticks
// it deliberately did NOT run (`skipped_catchup`, `skipped_paused`), so that
// "why did the standup not run" has an answer — but a tick not run is not
// something the company did, and it is answered where the ledger is read
// (`schedule_runs`, Agents › Schedules), not here.
//
// CONSECUTIVE RUNS OF ONE SCHEDULE ARE ONE ROW. A sweep every ten minutes is
// 144 rows a day, and a feed that printed each would bury every completion
// and filing under one schedule's heartbeat — which is what "what did the
// company do" is least about. So runs of the same schedule for the same
// runner that nothing else in the feed falls between are folded into the
// newest of them: `runs` counts them and `since` is the oldest one's instant.
// The fold is decided here, where the merge knows what falls between, rather
// than on a screen that sees one page at a time.
type FeedScheduleRun struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	Name      string `json:"name"`
	Target    string `json:"target,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`

	// Runs is how many consecutive runs this row stands for, at least one;
	// Since is the oldest one's instant, set only when Runs is above one.
	Runs  int        `json:"runs"`
	Since *time.Time `json:"since,omitempty"`
}

// sameSchedule reports whether two runs are of one schedule for one runner,
// and so fold into one row when nothing falls between them.
func (r *FeedScheduleRun) sameSchedule(o *FeedScheduleRun) bool {
	return r.ScopeType == o.ScopeType && r.ScopeID == o.ScopeID &&
		r.Name == o.Name && r.Target == o.Target
}

// feedCursor is where one reader's scroll stopped, per source: each source's
// own keyset position, because each source is ordered by its own total order
// and one instant shared across three logs would repeat or skip a row at
// every tie.
type feedCursor map[string]string

func (c feedCursor) String() string {
	body, _ := json.Marshal(map[string]string(c))
	return base64.RawURLEncoding.EncodeToString(body)
}

func parseFeedCursor(raw string) (feedCursor, error) {
	out := feedCursor{}
	if raw == "" {
		return out, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(body, (*map[string]string)(&out)) != nil {
		return nil, badParams("cursor", raw, []string{"a next_cursor this answer returned"})
	}
	return out, nil
}

// feedPageOf is one page a source read: its rows newest first, the position
// after each, and whether the source holds more past the last.
type feedPageOf struct {
	entries   []FeedEntry
	positions []string
	more      bool
}

// feedSource is one source as the merge walks it: the page it holds, and how
// to read the page after a position when that one is used up.
type feedSource struct {
	name string
	feedPageOf
	next func(position string) (feedPageOf, error)
}

// feedSourceReads bounds how many pages of one source a single merged page
// may read. A folded schedule row consumes many rows of its source while
// adding one to the answer, so a source can run dry before the answer is
// full; it is then read again from where it stopped, up to this many pages
// in all — at the 50-row ceiling, 200 rows of one source per request, which
// is a day and a half of a sweep every ten minutes folded into one row. Past
// it the page is answered SHORT, never out of order: its cursor resumes
// exactly where the reads stopped.
const feedSourceReads = 4

// companyFeed answers one merged page of what the company did — completions,
// creates and hand-offs from the tracker, pages created or saved, and the
// schedules' runs — newest first.
//
// MERGED BY INSTANT, CURSORED PER SOURCE. Each source is read one page past
// its own cursor in its own order; the pages are merged by instant and cut at
// the limit; and the next cursor records, for each source, the position of
// the last row of it this page consumed. So a row is never repeated and never
// skipped however the three interleave, and a source that has nothing more
// stops being asked.
func (s Sources) companyFeed(ctx context.Context, p Params) (any, error) {
	kinds, err := feedKinds(p.String("kinds"), s.feedKept())
	if err != nil {
		return nil, err
	}
	for _, kind := range kinds {
		if (kind == FeedPage && s.Pages == nil) || (kind == FeedSchedule && s.Usage == nil) {
			return nil, fmt.Errorf("%w: kinds=%s is not kept on this node — the "+
				"company has no native %s here", ErrBadParams, kind,
				map[FeedKind]string{FeedPage: "knowledge base", FeedSchedule: "usage domain"}[kind])
		}
	}
	limit := Clamp(p.Int("limit", 0), DefaultFeedPage, tracker.MaxFeedPage)
	cursor, err := parseFeedCursor(strings.TrimSpace(p.String("cursor")))
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	actor := strings.TrimSpace(p.String("actor"))

	var sources []*feedSource
	complete := true
	var level statelog.ReadLevel
	open := func(name string, next func(string) (feedPageOf, error)) error {
		first, readErr := next(cursor[name])
		if readErr != nil {
			return readErr
		}
		sources = append(sources, &feedSource{name: name, feedPageOf: first, next: next})
		return nil
	}
	if work := trackerKinds(kinds); len(work) > 0 && cursor[feedSourceWork] != feedEnd {
		if openErr := open(feedSourceWork, func(position string) (feedPageOf, error) {
			page, done, lvl, readErr := s.feedWork(ctx, work, actor, position, limit, fresh)
			complete = complete && done
			if level == "" {
				level = lvl
			}
			return page, readErr
		}); openErr != nil {
			return nil, openErr
		}
	}
	if slices.Contains(kinds, FeedPage) && cursor[feedSourcePages] != feedEnd {
		// THE CALLER'S FLOOR NAMES THE TRACKER'S LOG — `company_feed` is a
		// tracker session question (`SESSION_QUERIES.tracker`), and a
		// position in one domain's log is no bound at all on another's —
		// so the pages half is read at the surface's own default level.
		pagesFresh := statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)}
		if openErr := open(feedSourcePages, func(position string) (feedPageOf, error) {
			page, done, lvl, readErr := s.feedPages(ctx, actor, position, limit, pagesFresh)
			complete = complete && done
			if level == "" {
				level = lvl
			}
			return page, readErr
		}); openErr != nil {
			return nil, openErr
		}
	}
	if slices.Contains(kinds, FeedSchedule) && cursor[feedSourceSchedule] != feedEnd {
		if openErr := open(feedSourceSchedule, func(position string) (feedPageOf, error) {
			return s.feedSchedules(ctx, actor, position, limit)
		}); openErr != nil {
			return nil, openErr
		}
	}

	entries, next, err := mergeFeed(sources, cursor, limit)
	if err != nil {
		return nil, err
	}
	answer := map[string]any{
		"rows":     entries,
		"complete": complete,
	}
	if level != "" {
		answer["read_level"] = level
	}
	if next != nil {
		answer["next_cursor"] = next.String()
	}
	return answer, nil
}

// mergeFeed takes up to limit rows across the sources, newest first, and the
// cursor that resumes after them — nil when every source is read to its end.
//
// A SOURCE THAT RUNS DRY WHILE IT HOLDS MORE IS READ AGAIN, and where it may
// not be (see [feedSourceReads]) the page ends there: its next row could be
// newer than every row the others still hold, so nothing past that point can
// be placed.
func mergeFeed(sources []*feedSource, from feedCursor, limit int) ([]FeedEntry, feedCursor, error) {
	next := feedCursor{}
	for name, position := range from {
		next[name] = position
	}
	heads := make([]int, len(sources))
	reads := make([]int, len(sources))
	entries := []FeedEntry{}
merge:
	for len(entries) < limit {
		pick := -1
		for i, src := range sources {
			if heads[i] >= len(src.entries) && src.more {
				if reads[i]+1 >= feedSourceReads || len(src.positions) == 0 {
					break merge
				}
				page, err := src.next(src.positions[len(src.positions)-1])
				if err != nil {
					return nil, nil, err
				}
				src.feedPageOf, heads[i] = page, 0
				reads[i]++
			}
			if heads[i] >= len(src.entries) {
				continue
			}
			if pick < 0 || src.entries[heads[i]].At.After(sources[pick].entries[heads[pick]].At) {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		src := sources[pick]
		entry := src.entries[heads[pick]]
		next[src.name] = src.positions[heads[pick]]
		heads[pick]++
		if n := len(entries); n > 0 && foldRun(&entries[n-1], entry) {
			continue
		}
		if entry.Schedule != nil {
			run := *entry.Schedule
			run.Runs, run.Since = 1, nil
			entry.Schedule = &run
		}
		entries = append(entries, entry)
	}
	for i, src := range sources {
		if heads[i] >= len(src.entries) && !src.more {
			next[src.name] = feedEnd
		}
	}
	for _, position := range next {
		if position != feedEnd {
			return entries, next, nil
		}
	}
	return entries, nil, nil
}

// foldRun folds a schedule run into the row before it when that row is a run
// of the same schedule for the same runner — see [FeedScheduleRun].
func foldRun(last *FeedEntry, entry FeedEntry) bool {
	if last.Schedule == nil || entry.Schedule == nil || !last.Schedule.sameSchedule(entry.Schedule) {
		return false
	}
	since := entry.At
	last.Schedule.Runs++
	last.Schedule.Since = &since
	return true
}

// feedKept is the kinds this node keeps a source for: the tracker's three
// always (the question is registered on the tracker), and the page and
// schedule kinds where their domains are here.
func (s Sources) feedKept() []FeedKind {
	out := []FeedKind{FeedCompleted, FeedCreated, FeedHandoff}
	if s.Usage != nil {
		out = append(out, FeedSchedule)
	}
	if s.Pages != nil {
		out = append(out, FeedPage)
	}
	return out
}

// feedKinds reads `kinds=`, a comma list; empty is every kind this node
// keeps.
func feedKinds(raw string, kept []FeedKind) ([]FeedKind, error) {
	if strings.TrimSpace(raw) == "" {
		return kept, nil
	}
	var out []FeedKind
	for _, part := range strings.Split(raw, ",") {
		kind := FeedKind(strings.TrimSpace(part))
		if !kind.Valid() {
			return nil, badParams("kinds", string(kind), names(FeedKinds))
		}
		if !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}
	return out, nil
}

// trackerKinds is the tracker's share of the asked kinds.
func trackerKinds(kinds []FeedKind) []tracker.FeedKind {
	var out []tracker.FeedKind
	for _, kind := range kinds {
		if k := tracker.FeedKind(kind); k.Valid() {
			out = append(out, k)
		}
	}
	return out
}

func (s Sources) feedWork(ctx context.Context, kinds []tracker.FeedKind, actor,
	position string, limit int, fresh statelog.Freshness) (
	feedPageOf, bool, statelog.ReadLevel, error) {

	var src feedPageOf
	if s.Work == nil {
		return src, true, "", nil
	}
	q := tracker.FeedQuery{
		Kinds: kinds, Actor: actor, Limit: limit,
		Level: fresh.Level, MaxLag: fresh.MaxLag, MaxLagSeq: fresh.MaxLagSeq,
		MinPosition: fresh.MinPosition,
	}
	if position != "" {
		before, err := tracker.ParseFeedCursor(position)
		if err != nil {
			return src, false, "", badParams("cursor", position, nil)
		}
		q.Before = &before
	}
	page, err := s.Work.CompanyFeed(ctx, q)
	if err != nil {
		return src, false, "", err
	}
	for i := range page.Rows {
		row := page.Rows[i]
		src.entries = append(src.entries, FeedEntry{Kind: FeedKind(row.Kind), At: row.At, Work: &row})
		src.positions = append(src.positions, row.Cursor)
	}
	src.more = page.More
	return src, page.Complete, page.Level, nil
}

func (s Sources) feedPages(ctx context.Context, actor, position string, limit int,
	fresh statelog.Freshness) (feedPageOf, bool, statelog.ReadLevel, error) {

	var src feedPageOf
	q := pages.PageActivityQuery{
		Kinds: []pages.ChangeKind{pages.ChangeCreated, pages.ChangeSaved},
		Actor: actor, Limit: limit, Freshness: fresh,
	}
	if position != "" {
		var cursor uint64
		if _, err := fmt.Sscan(position, &cursor); err != nil || cursor == 0 {
			return src, false, "", badParams("cursor", position, nil)
		}
		q.Cursor = cursor
	}
	activity, err := s.Pages.Activity(ctx, q)
	if err != nil {
		return src, false, "", err
	}
	for _, change := range activity.Changes {
		src.entries = append(src.entries, FeedEntry{Kind: FeedPage, At: change.At,
			Page: &FeedPageChange{
				ID: change.ID, PageID: change.PageID, Title: change.Title,
				Container: change.Container, Change: change.Kind,
				Actor: change.Actor, ActorKind: change.ActorKind, TurnID: change.TurnID,
			}})
		src.positions = append(src.positions, fmt.Sprint(change.LogSeq))
	}
	src.more = activity.NextCursor != ""
	return src, activity.Complete, activity.Level, nil
}

// feedSchedules reads one page of the schedules' RUNS — the fires the
// scheduler dispatched, never a tick it skipped (see [FeedScheduleRun]).
func (s Sources) feedSchedules(ctx context.Context, actor, position string,
	limit int) (feedPageOf, error) {

	var src feedPageOf
	q := usage.ScheduleRunsQuery{Target: actor, Outcome: string(schedule.OutcomeFired), Limit: limit}
	if position != "" {
		at, key, ok := strings.Cut(position, "|")
		when, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil {
			return src, badParams("cursor", position, nil)
		}
		q.BeforeAt, q.BeforeKey = when, key
	}
	runs, more, err := usage.ScheduleRuns(ctx, s.Usage, q)
	if err != nil {
		return src, err
	}
	for _, run := range runs {
		src.entries = append(src.entries, FeedEntry{Kind: FeedSchedule, At: run.FiredAt,
			Schedule: &FeedScheduleRun{
				ScopeType: run.ScopeType, ScopeID: run.ScopeID, Name: run.Name,
				Target: run.Target, Outcome: run.Outcome, TraceID: run.TraceID,
				TurnID: run.TurnID, Runs: 1,
			}})
		src.positions = append(src.positions,
			run.FiredAt.UTC().Format(time.RFC3339Nano)+"|"+run.Key())
	}
	src.more = more
	return src, nil
}

// ---- decisions -----------------------------------------------------------

// DecisionItem is one thing waiting on a person's decision: a structured ask
// on a work item, or a coding run parked on a question put to them.
type DecisionItem struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`

	Ask *tracker.AskRow `json:"ask,omitempty"`
	Run map[string]any  `json:"run,omitempty"`
}

// decisions answers what is waiting on one person — the open asks put to
// them and the coding runs parked on a question to them — newest first, at
// most [tracker.MaxDecisions], with the full count and the oldest one's
// instant beside the page.
//
// THE SAME SCOPE RULE AS `work_my_work` — see [Sources.viewerParty]: a caller
// reads the person their own token is bound to, and naming anybody else needs
// an operator credential.
func (s Sources) decisions(ctx context.Context, p Params) (any, error) {
	who, err := s.viewerParty(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	var items []DecisionItem
	total, capped := 0, false
	var oldest *time.Time
	older := func(at time.Time) {
		if oldest == nil || at.Before(*oldest) {
			oldest = &at
		}
	}
	answer := map[string]any{"handle": who.Handle}
	if s.Work != nil {
		fresh, err := freshness(p)
		if err != nil {
			return nil, err
		}
		asks, err := s.Work.Decisions(ctx, tracker.DecisionsQuery{
			Who:   who,
			Level: fresh.Level, MaxLag: fresh.MaxLag, MaxLagSeq: fresh.MaxLagSeq,
			MinPosition: fresh.MinPosition,
		}, s.clock(), s.zone())
		if err != nil {
			return nil, err
		}
		for i := range asks.Asks {
			ask := asks.Asks[i]
			items = append(items, DecisionItem{Kind: "ask", At: ask.AskedAt, Ask: &ask})
		}
		total += asks.Total.Total
		capped = asks.Total.Capped
		if asks.OldestAt != nil {
			older(*asks.OldestAt)
		}
		answer["read_level"] = asks.Level
		answer["complete"] = asks.Complete
	}
	if s.Sandbox != nil {
		runs, err := s.Sandbox.ListActive(ctx)
		if err != nil {
			return nil, err
		}
		handles := who.Handles()
		for _, run := range runs {
			// WAITING ON AN ANSWER — [sandbox.Awaiting], a reseeded run
			// included, since its answer can still arrive — and put to
			// this person.
			if !slices.Contains(sandbox.Awaiting, run.Status) || !putTo(run, handles) {
				continue
			}
			at := waitingSince(run)
			items = append(items, DecisionItem{Kind: "run", At: at, Run: serialiseRun(run)})
			total++
			older(at)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > tracker.MaxDecisions {
		items = items[:tracker.MaxDecisions]
	}
	if items == nil {
		items = []DecisionItem{}
	}
	answer["items"] = items
	answer["total"] = total
	answer["capped"] = capped
	if oldest != nil {
		answer["oldest_at"] = oldest.UTC()
	}
	return answer, nil
}

// waitingSince is when a parked run began waiting on its question: when its
// box was held, or — for a run whose hold was never stamped — its last update.
func waitingSince(run sandbox.PendingRun) time.Time {
	if held, ok := run.HeldSince(); ok {
		return held
	}
	return run.UpdatedAt
}
