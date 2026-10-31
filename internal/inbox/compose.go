package inbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE INBOX FROM EVERY NATIVE DOMAIN: a person's notices from the tracker and
// from the code host, in one list, newest first.
//
// # Each domain keeps its own, and is read on its own terms
//
// A domain files its notices on its own log, keeps the person's marks over
// them there, and reads a page of them filtered in its own scan by those
// marks ([Classifier]). So a composed inbox is never one table and never one
// read: it is a page from each [Source], each asked the same narrowing and
// each held to its own permission by whoever builds the list of sources —
// a source the caller may not read is not in it — then INTERLEAVED.
//
// # Interleaved by when each happened, each source a prefix
//
// The pages are merged by the AUTHORED instant (a notice's [Notice.At]), then
// the source's name, then the record id — so two notices of one instant
// always come in one order. It is a MERGE OF HEADS, never a sort of the union:
// each source's page is in that source's own order (its log's, newest first),
// and the merge takes the newer of the sources' next notices one at a time, so
// what it takes of each source is always a PREFIX of that source's page. That
// is what lets the composed cursor carry ONE POSITION PER SOURCE — the last
// notice of that source the page took — and a next page resume each source
// exactly where it stopped: a sort of the union could take a source's third
// notice and leave its second, and no single position of that source would
// then say what was shown. Each source is asked for the whole page's limit, so
// the merge never runs past a source's page while that source has more.

// Source is one native domain's inbox, as the composition reads it.
type Source interface {
	// Name is the source's name: what a composed notice and the cursor name
	// it by ("tracker", "code").
	Name() string

	// Notices is one page of the person's notices from this source, newest
	// first in the source's own order, after the source's own cursor.
	Notices(ctx context.Context, q SourceQuery) (SourcePage, error)
}

// SourceQuery is what a composed read asks of each source: the same narrowing
// of every one, and that source's own cursor.
type SourceQuery struct {
	Handle      string
	Unread      bool
	PrimaryOnly bool
	Snoozed     SnoozeScope
	Limit       int
	// Cursor is the source's own cursor — the position of the last notice
	// of this source a page took — "" for its first page.
	Cursor string
}

// SourcePage is one page of a source's notices, with what the source says
// about the person's marks over them.
type SourcePage struct {
	Notices []Notice
	// Next is the source's own cursor past this page, "" when it holds no
	// more.
	Next string
	// SeenThrough is where the person's marks over this source say they
	// have read to, on this source's log; PrimaryReasons the split that
	// applied.
	SeenThrough    Position
	PrimaryReasons []string
	// Level is the level the source's page was read at, and Complete
	// whether it holds every notice its scan reached — a record its domain's
	// copy could not decode holds none back.
	Level    statelog.ReadLevel
	Complete bool
}

// Subject is what a notice is about, as a card names it.
type Subject struct {
	ID    string `json:"id"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
}

// Notice is one thing the company told a person, as every source tells it.
//
// THE FIELDS EVERY CARD READS ARE HERE, and what only one domain says rides
// in Extra — the source's own notice, whole — so a screen draws any notice
// from these and a domain's card the rest from its own shape, and the
// composition never learns a domain's vocabulary.
type Notice struct {
	Source   string `json:"source"`
	RecordID string `json:"record_id"`
	// Position is where the notice sits on its source's log — what a mark
	// reads through and the source's cursor names.
	Position Position  `json:"position"`
	At       time.Time `json:"at"`

	Reason    string `json:"reason"`
	Primary   bool   `json:"primary"`
	Addressed bool   `json:"addressed"`
	Fallback  bool   `json:"fallback,omitempty"`

	// Kind is what the notice is about in its source's words (a task, a
	// change), and Subject which one.
	Kind    string  `json:"kind"`
	Subject Subject `json:"subject"`
	Excerpt string  `json:"excerpt,omitempty"`

	// Actor is who made what the notice reports, where its source records
	// it.
	Actor     string `json:"actor,omitempty"`
	ActorKind string `json:"actor_kind,omitempty"`

	Read         bool       `json:"read"`
	Snoozed      bool       `json:"snoozed,omitempty"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitzero"`

	// Extra is the source's own notice, whole.
	Extra any `json:"extra,omitempty"`
}

// Query is a composed read: whose inbox, the narrowing every source is asked,
// how many notices, and the composed cursor of the page before ("" for the
// first).
type Query struct {
	Handle      string
	Unread      bool
	PrimaryOnly bool
	Snoozed     SnoozeScope
	Limit       int
	Cursor      string
}

// SourceState is what one source said of the person's marks over it, and
// whether it is read to its end.
type SourceState struct {
	Name           string   `json:"name"`
	SeenThrough    Position `json:"seen_through,omitzero"`
	PrimaryReasons []string `json:"primary_reasons"`
	// Newest is the newest notice of this source the page carries — what a
	// screen's "mark all read" reads this source through — absent where the
	// page carries none of its.
	Newest *Position `json:"newest,omitempty"`
}

// Page is a page of a person's composed inbox.
type Page struct {
	Handle  string        `json:"handle"`
	Notices []Notice      `json:"notices"`
	Sources []SourceState `json:"sources"`
	// Unread and Primary count the PAGE's notices.
	Unread  int `json:"unread"`
	Primary int `json:"primary"`
	// NextCursor is the composed cursor past this page, "" when every
	// source is read to its end.
	NextCursor string `json:"next_cursor,omitempty"`
	// ReadLevel is the level the page was read at: the WEAKEST its sources
	// were read at ([statelog.Weakest]), since a page is no fresher than its
	// stalest half; Complete whether every source read held every notice.
	ReadLevel statelog.ReadLevel `json:"read_level,omitempty"`
	Complete  bool               `json:"complete"`
}

// ErrBadCursor is a composed cursor this build did not issue.
var ErrBadCursor = errors.New("inbox: not a cursor an inbox page issued")

// sourceDone is the cursor value of a source read to its end.
const sourceDone = "-"

// Compose reads one page of q.Handle's inbox from sources and interleaves it
// (this file's head). A source's failure is the read's: a page that left a
// source out without saying so would be an inbox with notices missing.
func Compose(ctx context.Context, q Query, sources []Source) (Page, error) {
	cursors, err := ParseCursor(q.Cursor)
	if err != nil {
		return Page{}, err
	}
	for name := range cursors {
		if !slices.ContainsFunc(sources, func(s Source) bool { return s.Name() == name }) {
			return Page{}, fmt.Errorf("%w: it names the source %q, which this inbox does not read", ErrBadCursor,
				name)
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxRows {
		limit = MaxRows
	}
	type head struct {
		name  string
		page  SourcePage
		taken int
	}
	page := Page{Handle: q.Handle, Notices: []Notice{}, Sources: []SourceState{}, Complete: true}
	var heads []*head
	var levels []statelog.ReadLevel
	for _, s := range sources {
		state := SourceState{Name: s.Name(), PrimaryReasons: []string{}}
		cursor, seen := cursors[s.Name()]
		if seen && cursor == sourceDone {
			page.Sources = append(page.Sources, state)
			continue
		}
		got, err := s.Notices(ctx, SourceQuery{Handle: q.Handle, Unread: q.Unread, PrimaryOnly: q.PrimaryOnly,
			Snoozed: q.Snoozed, Limit: limit, Cursor: cursor})
		if err != nil {
			return Page{}, fmt.Errorf("inbox: read the %s notices: %w", s.Name(), err)
		}
		// EVERY NOTICE NAMES ITS SOURCE before the merge compares them.
		for i := range got.Notices {
			got.Notices[i].Source = s.Name()
		}
		levels = append(levels, got.Level)
		page.Complete = page.Complete && got.Complete
		state.SeenThrough = got.SeenThrough
		if got.PrimaryReasons != nil {
			state.PrimaryReasons = got.PrimaryReasons
		}
		page.Sources = append(page.Sources, state)
		heads = append(heads, &head{name: s.Name(), page: got})
	}
	page.ReadLevel = statelog.Weakest(levels...)
	for len(page.Notices) < limit {
		var next *head
		for _, h := range heads {
			if h.taken == len(h.page.Notices) {
				continue
			}
			if next == nil || newer(h.page.Notices[h.taken], next.page.Notices[next.taken]) {
				next = h
			}
		}
		if next == nil {
			break
		}
		n := next.page.Notices[next.taken]
		next.taken++
		page.Notices = append(page.Notices, n)
		if !n.Read {
			page.Unread++
		}
		if n.Primary {
			page.Primary++
		}
	}
	// ONE POSITION PER SOURCE: the last notice of its the page took, or the
	// cursor it was asked with where it took none — and a source read to
	// its end, every notice of its last page taken and nothing after it, is
	// done.
	after := map[string]string{}
	for name, cursor := range cursors {
		after[name] = cursor
	}
	more := false
	for _, h := range heads {
		switch {
		case h.taken == len(h.page.Notices) && h.page.Next == "":
			after[h.name] = sourceDone
		case h.taken > 0:
			after[h.name] = h.page.Notices[h.taken-1].Position.String()
			more = true
		default:
			more = true
		}
		if h.taken > 0 {
			newest := h.page.Notices[0].Position
			for i := range page.Sources {
				if page.Sources[i].Name == h.name {
					page.Sources[i].Newest = &newest
				}
			}
		}
	}
	if more {
		page.NextCursor = formatCursor(after)
	}
	return page, nil
}

// newer reports whether a comes before b in a composed inbox: the later
// authored instant first, then the source's name, then the record id.
func newer(a, b Notice) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	if c := cmp.Compare(a.Source, b.Source); c != 0 {
		return c < 0
	}
	return a.RecordID < b.RecordID
}

// String is the position as every cursor and mark spells it —
// [statelog.Position.String]'s `<stream>@<generation>:<sequence>`, the one
// spelling a domain's own cursor parses.
func (p Position) String() string {
	return statelog.Position{Stream: p.Stream, Generation: p.Generation, Seq: p.Seq}.String()
}

// ParseCursor reads a composed cursor: one `<source>=<cursor>` pair a source,
// query-encoded, a source read to its end spelled `-`. "" is the first page.
func ParseCursor(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	for name, v := range values {
		if name == "" || len(v) != 1 || v[0] == "" {
			return nil, fmt.Errorf("%w: %q names a source without one position", ErrBadCursor, raw)
		}
		out[name] = v[0]
	}
	return out, nil
}

// formatCursor is cursors as one composed cursor, in source order.
func formatCursor(cursors map[string]string) string {
	values := url.Values{}
	for name, cursor := range cursors {
		if cursor != "" {
			values.Set(name, cursor)
		}
	}
	return values.Encode()
}
