// The company's atlas, read: its entries as a list, as a map, one entry, a
// trace of what depends on one, the counts a filter menu offers, its own
// log as lines and as daily counts, and what waits for a person. Every
// question takes the atlas app (ADR-0127) and reads the atlas as its asker:
// Gitlet's repositories only for a holder of Gitlet, and an integration's
// standing or a connection test only for an administrator, so the atlas is
// never a way round another screen (ADR-0126).

package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AtlasReads is the atlas as this node reads it for one asker: the settings
// it runs, the engine's records, and Gitlet as asker where reads is set or
// why it is not read for them (unread); false where it runs no company yet.
// builtin.AtlasDeps serves it, as it serves the atlas tools.
type AtlasReads interface {
	AtlasRead(ctx context.Context, reads gather.GitletReads, unread string, asker gitlet.Principal,
		fresh statelog.Freshness, now time.Time) (gather.Answer, bool)
	// AtlasLog and AtlasActivity read the atlas's own log, every line of it:
	// the questions here hold each line to the atlas its asker reads.
	AtlasLog(ctx context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error)
	AtlasActivity(ctx context.Context, since time.Time, fresh statelog.Freshness) (atlasdomain.Activity, error)
}

// atlasFresh is the read level every atlas question reads at: the
// dashboard's, which carries its lag rather than waiting on a leader.
var atlasFresh = statelog.Freshness{Level: statelog.ReadStale}

// AtlasRead is what every atlas answer carries beside its own: the settings
// revision it read ("" where this node cannot tell), which sources it read
// in full and why not, and how many values it left out for being shaped like
// a credential, counted and never shown.
type AtlasRead struct {
	Revision string            `json:"revision"`
	Coverage []gather.Coverage `json:"coverage"`
	Withheld int               `json:"withheld"`
}

// AtlasEntriesAnswer is one page of the atlas as a list.
type AtlasEntriesAnswer struct {
	AtlasRead
	Rows       []atlas.Summary `json:"rows"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Total      int             `json:"total"`
}

// AtlasMapAnswer is the atlas drawn as its lanes.
type AtlasMapAnswer struct {
	AtlasRead
	atlas.Map
}

// AtlasEntryAnswer is one entry in full, what its open signals add up to as
// the reader may read them, and what relies on it.
type AtlasEntryAnswer struct {
	AtlasRead
	Entry  atlas.Entry  `json:"entry"`
	Health atlas.Health `json:"health"`
	Vendor atlas.Vendor `json:"vendor,omitempty"`
	UsedBy []atlas.Link `json:"used_by"`
}

// AtlasTraceAnswer is what depends on one entry, or what it depends on.
type AtlasTraceAnswer struct {
	AtlasRead
	atlas.Trace
}

// AtlasFacetsAnswer is how many entries each filter value leaves.
type AtlasFacetsAnswer struct {
	AtlasRead
	atlas.Facets
	Total int `json:"total"`
}

// AtlasLogAnswer is one page of the atlas's log, newest first, as its asker
// may read it.
type AtlasLogAnswer struct {
	AtlasRead
	Lines      []atlas.Line `json:"lines"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// AtlasEntryActivity is one entry's part of the log: how many lines it took
// on each day of the window, oldest first (absent where it took none), and
// its newest line however old.
type AtlasEntryActivity struct {
	ID   uuid.UUID   `json:"id"`
	Days []int       `json:"days,omitempty"`
	Last *atlas.Line `json:"last,omitempty"`
}

// AtlasActivityAnswer is the atlas's log counted: the lines of each of the
// last [atlas.ActivityDays] UTC days, oldest first and today last, for the
// company and for each entry the log holds a line of.
type AtlasActivityAnswer struct {
	AtlasRead
	From    time.Time            `json:"from"`
	Days    []int                `json:"days"`
	Entries []AtlasEntryActivity `json:"entries"`
}

// AtlasReviewAnswer is one page of what waits for a person in the atlas:
// what its sources disagree on, and the entries nobody stands behind yet.
// Total counts every item of the asked kind, Counts each kind's.
type AtlasReviewAnswer struct {
	AtlasRead
	Items      []atlas.ReviewItem       `json:"items"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Total      int                      `json:"total"`
	Counts     map[atlas.ReviewKind]int `json:"counts"`
}

// registerAtlas registers the atlas's questions, each on the atlas app.
func registerAtlas(r *Registry, s Sources) {
	app := Requires(iam.GrantAtlas)
	r.Register("atlas_entries", app, s.atlasEntries)
	r.Register("atlas_map", app, s.atlasMap)
	r.Register("atlas_entry", app, s.atlasEntry)
	r.Register("atlas_trace", app, s.atlasTrace)
	r.Register("atlas_facets", app, s.atlasFacets)
	r.Register("atlas_log", app, s.atlasLog)
	r.Register("atlas_activity", app, s.atlasActivity)
	r.Register("atlas_review", app, s.atlasReview)
}

// atlasView is the atlas as the question's asker reads it, and what the read
// says of itself.
func (s Sources) atlasView(ctx context.Context, what string) (atlas.View, AtlasRead, error) {
	principal, err := caller(ctx, what)
	if err != nil {
		return atlas.View{}, AtlasRead{}, err
	}
	reads, why := s.atlasGitlet(principal)
	answer, running := s.Atlas.AtlasRead(ctx, reads, why, codeAsker(principal), atlasFresh, s.clock())
	if !running {
		return atlas.View{}, AtlasRead{}, fmt.Errorf("%w: this node runs no company yet, so there is no "+
			"atlas to read", ErrUnavailable)
	}
	read := AtlasRead{Revision: answer.Revision, Coverage: answer.Coverage, Withheld: len(answer.Withheld)}
	return atlas.For(answer.View, atlas.Reader{Admin: principal.Can(iam.GrantAdmin)}), read, nil
}

// atlasGitlet is Gitlet as the asker reads it, or none and why: the atlas
// lists a repository only to a holder of Gitlet (ADR-0110), and only where
// this node serves code.
func (s Sources) atlasGitlet(p iam.Principal) (gather.GitletReads, string) {
	if !p.Can(iam.GrantGitlet) {
		return nil, "you do not use Gitlet, so no repository was read"
	}
	if s.Code == nil {
		return nil, ""
	}
	reads, err := s.Code.CodeReads()
	var absent *NotFound
	switch {
	case errors.As(err, &absent):
		return nil, ""
	case err != nil:
		return nil, "Gitlet could not be read on this node right now: " + err.Error()
	}
	return reads, ""
}

// atlasQuery is the filters a question names.
func atlasQuery(p Params) (atlas.Query, error) {
	unconfirmed, err := atlasFlag(p, "unconfirmed")
	if err != nil {
		return atlas.Query{}, err
	}
	q := atlas.Query{
		Text: p.String("q"), Kind: atlas.Kind(p.String("kind")), Lane: atlas.Lane(p.String("lane")),
		Vendor: atlas.Vendor(p.String("vendor")), Word: atlas.Word(p.String("confidence")),
		Health: atlas.Health(p.String("health")), Show: atlas.Show(p.String("show")),
		Unconfirmed: unconfirmed,
	}
	if err := q.Validate(); err != nil {
		return atlas.Query{}, badAtlas(err)
	}
	return q, nil
}

// atlasFlag is a yes or no parameter, false where it is absent. Anything but
// true or false is refused rather than read as no, so a misspelt flag never
// hides what it asked to show.
func atlasFlag(p Params, key string) (bool, error) {
	if !p.Has(key) {
		return false, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(p.String(key)))
	if err != nil {
		return false, fmt.Errorf("%w: `%s` is true or false", ErrBadParams, key)
	}
	return b, nil
}

// atlasRelations is which relations a map or a trace follows: those the map
// draws at rest unless the question asks for all.
func atlasRelations(p Params) (all bool, err error) {
	switch p.String("relations") {
	case "", "structural":
		return false, nil
	case "all":
		return true, nil
	}
	return false, fmt.Errorf("%w: `relations` is \"structural\" or \"all\"", ErrBadParams)
}

// badAtlas is an atlas read's refusal of its parameters, in this surface's
// vocabulary.
func badAtlas(err error) error {
	return fmt.Errorf("%w: %s", ErrBadParams, strings.TrimPrefix(err.Error(), "atlas: bad read: "))
}

// atlasID is the entry a question names by id.
func atlasID(p Params) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(p.String("id")))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: `id` is an atlas entry's id", ErrBadParams)
	}
	return id, nil
}

// noAtlasEntry is an id no entry of the atlas answers to.
func noAtlasEntry() error {
	return &NotFound{Reason: "no_atlas_entry", Detail: "no entry of the atlas has this id: it was never in it, " +
		"or what named it stopped naming it"}
}

func (s Sources) atlasEntries(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_entries")
	if err != nil {
		return nil, err
	}
	limit := Clamp(p.Int("limit", atlas.PageDefault), atlas.PageDefault, atlas.PageMax)
	page, err := atlas.List(view.Select(q), view.Summaries(q.ShowsUnconfirmed()),
		atlas.Sort(p.String("sort")), p.String("cursor"), limit)
	if err != nil {
		return nil, badAtlas(err)
	}
	return AtlasEntriesAnswer{AtlasRead: read, Rows: page.Rows, NextCursor: page.Next, Total: page.Total}, nil
}

func (s Sources) atlasMap(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	all, err := atlasRelations(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_map")
	if err != nil {
		return nil, err
	}
	var closed []string
	for _, key := range strings.Split(p.String("closed"), ",") {
		if key = strings.TrimSpace(key); key != "" {
			closed = append(closed, key)
		}
	}
	return AtlasMapAnswer{AtlasRead: read, Map: view.Map(q, closed, all)}, nil
}

func (s Sources) atlasEntry(ctx context.Context, p Params) (any, error) {
	id, err := atlasID(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_entry")
	if err != nil {
		return nil, err
	}
	e, found := view.Find(id)
	if !found {
		return nil, noAtlasEntry()
	}
	return AtlasEntryAnswer{AtlasRead: read, Entry: e, Health: e.Health(), Vendor: view.Vendors()[e.ID],
		UsedBy: view.UsedBy(e.ID)}, nil
}

func (s Sources) atlasTrace(ctx context.Context, p Params) (any, error) {
	id, err := atlasID(p)
	if err != nil {
		return nil, err
	}
	direction := atlas.Direction(p.String("direction"))
	if direction == "" {
		direction = atlas.DirectionUp
	}
	all, err := atlasRelations(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_trace")
	if err != nil {
		return nil, err
	}
	e, found := view.Find(id)
	if !found {
		return nil, noAtlasEntry()
	}
	trace, _, err := view.Trace(e.ID, direction, p.Int("depth", atlas.TraceDepthMax), all, atlas.TraceMax)
	if err != nil {
		return nil, badAtlas(err)
	}
	return AtlasTraceAnswer{AtlasRead: read, Trace: trace}, nil
}

func (s Sources) atlasFacets(ctx context.Context, p Params) (any, error) {
	q, err := atlasQuery(p)
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_facets")
	if err != nil {
		return nil, err
	}
	return AtlasFacetsAnswer{AtlasRead: read, Facets: view.Facets(q), Total: len(view.Select(q))}, nil
}

// atlasReview is what waits for a person, computed on this read and stored
// nowhere: the facts and relations the sources disagree on, then the entries
// only an agent reported. It reads the atlas as its asker does, so it never
// lists an entry the asker's catalogue would not.
//
// `mine` narrows it to what contradicts the asker's own word: a fact or a
// relation they set or confirmed that a newer report differs from. It is what
// a person's Inbox is told of, so a credential acting as no seat, whose word
// no claim carries, is answered none.
func (s Sources) atlasReview(ctx context.Context, p Params) (any, error) {
	mine, err := atlasFlag(p, "mine")
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_review")
	if err != nil {
		return nil, err
	}
	q := atlas.ReviewQuery{Kind: atlas.ReviewKind(p.String("kind")), After: p.String("cursor"),
		Limit: Clamp(p.Int("limit", atlas.PageDefault), atlas.PageDefault, atlas.PageMax)}
	if mine {
		var principal iam.Principal
		if principal, err = caller(ctx, "atlas_review"); err != nil {
			return nil, err
		}
		if principal.Seat == "" {
			return AtlasReviewAnswer{AtlasRead: read, Items: []atlas.ReviewItem{},
				Counts: map[atlas.ReviewKind]int{}}, nil
		}
		q.ContradictedFor = principal.Seat
	}
	page, err := view.ReviewOf(q)
	if err != nil {
		return nil, badAtlas(err)
	}
	return AtlasReviewAnswer{AtlasRead: read, Items: page.Items, NextCursor: page.Next, Total: page.Total,
		Counts: page.Counts}, nil
}

// atlasLogScans is how many pages of the log one question reads to fill its
// own: a page drops the lines its asker is not shown, and five of the log's
// largest pages are a thousand lines, past which the answer is a short page
// that says there is more rather than a long wait.
const atlasLogScans = 5

// atlasLogRead is a read of the atlas's log that failed, in this surface's
// vocabulary: a parameter the log refuses is the asker's, anything else the
// node's.
func atlasLogRead(err error) error {
	if errors.Is(err, atlasdomain.ErrInvalid) {
		return fmt.Errorf("%w: `cursor` is the `next_cursor` a page of the log answered", ErrBadParams)
	}
	return fmt.Errorf("%w: the atlas's log could not be read on this node right now: %s", ErrUnavailable, err.Error())
}

// atlasLog is the atlas's log, newest first: the company's, or with `id` one
// entry's. A LINE IS SHOWN ONLY TO A READER WHO IS SHOWN ITS ENTRY and the
// entry it names, so the log never tells of a repository its reader may not
// read, nor of an unconfirmed entry a read that leaves them out.
func (s Sources) atlasLog(ctx context.Context, p Params) (any, error) {
	unconfirmed, err := atlasFlag(p, "unconfirmed")
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	if p.Has("id") {
		if id, err = atlasID(p); err != nil {
			return nil, err
		}
	}
	limit := Clamp(p.Int("limit", atlasdomain.DefaultPageSize), atlasdomain.DefaultPageSize, atlasdomain.MaxPageSize)
	view, read, err := s.atlasView(ctx, "atlas_log")
	if err != nil {
		return nil, err
	}
	query := atlasdomain.LogQuery{Cursor: p.String("cursor"), Limit: atlasdomain.MaxPageSize, Fresh: atlasFresh}
	index := view.Index()
	if p.Has("id") {
		// An entry asked for by its id is shown as its own answer shows it,
		// unconfirmed or not, where the reader is shown its lines at all.
		e, found := view.Find(id)
		if found {
			_, found = view.Logged(index, e.Ref, true)
		}
		if !found {
			return nil, noAtlasEntry()
		}
		unconfirmed = unconfirmed || e.Word == atlas.WordUnconfirmed
		for _, m := range e.Members {
			query.Entries = append(query.Entries, m.ID().String())
		}
	}
	answer := AtlasLogAnswer{AtlasRead: read, Lines: []atlas.Line{}}
	for range atlasLogScans {
		page, err := s.Atlas.AtlasLog(ctx, query)
		if err != nil {
			return nil, atlasLogRead(err)
		}
		for i, l := range page.Lines {
			line, shown := view.Line(index, l.Claim, l.ID, l.At, l.Withdrawn, unconfirmed)
			if !shown {
				continue
			}
			answer.Lines = append(answer.Lines, line)
			if len(answer.Lines) == limit {
				if i < len(page.Lines)-1 || page.Next != "" {
					answer.NextCursor = l.Cursor()
				}
				return answer, nil
			}
		}
		if answer.NextCursor = page.Next; page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	return answer, nil
}

// atlasActivity is the atlas's log counted over its last two weeks, and each
// entry's newest line: what a week's bars, a row's sparkline and its "last
// change" are drawn from. It counts exactly the lines atlas_log would show
// its asker.
func (s Sources) atlasActivity(ctx context.Context, p Params) (any, error) {
	unconfirmed, err := atlasFlag(p, "unconfirmed")
	if err != nil {
		return nil, err
	}
	view, read, err := s.atlasView(ctx, "atlas_activity")
	if err != nil {
		return nil, err
	}
	const day = 24 * time.Hour
	from := s.clock().UTC().Truncate(day).Add(-(atlas.ActivityDays - 1) * day)
	activity, err := s.Atlas.AtlasActivity(ctx, from, atlasFresh)
	if err != nil {
		return nil, atlasLogRead(err)
	}
	index := view.Index()
	// shown is the entry of the reader's atlas an id of the log names, where
	// the reader is shown its lines.
	shown := func(id string) (uuid.UUID, bool) {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return uuid.UUID{}, false
		}
		i, ok := index[parsed]
		if !ok {
			return uuid.UUID{}, false
		}
		for _, m := range view.Entries[i].Members {
			if m.ID() != parsed {
				continue
			}
			if e, ok := view.Logged(index, m, unconfirmed); ok {
				return e.ID, true
			}
		}
		return uuid.UUID{}, false
	}
	answer := AtlasActivityAnswer{AtlasRead: read, From: from, Days: make([]int, atlas.ActivityDays),
		Entries: []AtlasEntryActivity{}}
	byEntry := map[uuid.UUID]*AtlasEntryActivity{}
	of := func(id uuid.UUID) *AtlasEntryActivity {
		a, ok := byEntry[id]
		if !ok {
			a = &AtlasEntryActivity{ID: id}
			byEntry[id] = a
		}
		return a
	}
	for _, d := range activity.Days {
		entry, ok := shown(d.Entry)
		if !ok {
			continue
		}
		if d.Target != "" {
			if _, ok := shown(d.Target); !ok {
				continue
			}
		}
		at := int(d.Day.Sub(from) / day)
		if at < 0 || at >= atlas.ActivityDays {
			continue
		}
		a := of(entry)
		if a.Days == nil {
			a.Days = make([]int, atlas.ActivityDays)
		}
		a.Days[at] += d.Lines
		answer.Days[at] += d.Lines
	}
	for _, l := range activity.Last {
		line, ok := view.Line(index, l.Claim, l.ID, l.At, l.Withdrawn, unconfirmed)
		if !ok {
			continue
		}
		if a := of(line.Entry); a.Last == nil || newerLine(line, *a.Last) {
			a.Last = &line
		}
	}
	for _, a := range byEntry {
		answer.Entries = append(answer.Entries, *a)
	}
	slices.SortFunc(answer.Entries, func(a, b AtlasEntryActivity) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return answer, nil
}

// newerLine reports whether a is the line to show over b as an entry's last
// change: the later one, and of one record's lines the entry's own before a
// fact's, a fact's before a relation's, so the pick is the same on every read.
func newerLine(a, b atlas.Line) bool {
	return cmp.Or(a.At.Compare(b.At), cmp.Compare(slices.Index(atlas.Changes, b.Change), slices.Index(atlas.Changes, a.Change)),
		cmp.Compare(b.ID, a.ID)) > 0
}
