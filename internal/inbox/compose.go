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
// from Gitlet, in one list, newest first.
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
// then say what was shown. Each source is asked for the whole page's limit.
//
// # A source's page may end past its last notice
//
// A source may scan rows it does not answer: Gitlet leaves out a
// notice its asker may not read — every notice on a repository deleted and
// not yet purged among them — and its own cursor is still the last row it
// scanned ([SourcePage.Next]). So where the page took EVERY notice a source
// answered, that source resumes at its own cursor, never at its last notice:
// a page the source answered empty, or one whose last rows it left out,
// resumed at the last notice taken (or at the cursor it was asked with,
// where it answered none) would be asked for the same rows again on every
// page, and the inbox would never reach what lies below them. Such a short
// page is the one place the merge runs past what a source has yet to show:
// its next notices, below what it scanned, may come on a later page after an
// older notice of another source — but every notice still comes, and once.
//
// # A page closes between two positions of a source
//
// A source's cursor is a position on its log, and a source may hold SEVERAL
// notices at one position: Gitlet's record that tells one person about
// two changes — a batched push, a landing of several entries — is one record,
// one position, and a notice each change. A cursor that resumed below a
// position the page took only part of would never show the rest. So a
// source's page never ends inside a position (its contract, [Source]), and
// the merge, having taken a notice, takes every notice of the same source at
// the same position with it — a page may run past its limit by them, and by
// no more, since a record's notices to one person are bounded where the
// record is written.
//
// # Cards are read after the cut, and only where they are drawn
//
// A notice carries what a row of a list and a count are drawn from. What its
// CARD draws beyond that — Gitlet's judgment of where a change's merge
// stands, its description, its open findings — costs a source a read of its
// own, and most reads of an inbox draw no card: a count of what waits, a
// widget's few rows. So a query asks for cards ([Query.Cards]), the zero
// value asking none, and a source whose cards draw more than its notices
// carry is asked for them only AFTER THE MERGE, for the notices of it the
// page took ([Carded]): each source is asked the page's whole limit, and a
// card read before the cut would be judged for every notice another source's
// newer ones then push off the page.

// Source is one native domain's inbox, as the composition reads it.
type Source interface {
	// Name is the source's name: what a composed notice and the cursor name
	// it by ("tracker", "code").
	Name() string

	// Notices is one page of the person's notices from this source, newest
	// first in the source's own order, below the source's own cursor — never
	// ending inside one position, so a page holds every notice at its last
	// one (this file's head).
	Notices(ctx context.Context, q SourceQuery) (SourcePage, error)
}

// Carded is a source whose cards draw more than its notices carry (this
// file's head).
type Carded interface {
	Source

	// Cards is notices — the notices of this source a composed page took,
	// in the page's order, from page, the source's own page they were read
	// in — each with what its card draws, in the same order. Read no older
	// than the page ([SourcePage.At]), so a card never describes what its
	// notice is about as it stood before the notice was read.
	Cards(ctx context.Context, page SourcePage, notices []Notice) ([]Notice, error)
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
	// of this source a page took, or the source's own [SourcePage.Next]
	// where the page took every notice it answered — "" for its first page.
	Cursor string
}

// SourcePage is one page of a source's notices, with what the source says
// about the person's marks over them.
type SourcePage struct {
	Notices []Notice
	// Next is the source's own cursor past this page, "" when it holds no
	// more: past every row the source SCANNED for it, which may lie below
	// its last notice — or name a page of none — where the source left out
	// rows its asker may not read (this file's head).
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
	// At is the position on the source's log its page was read at, which a
	// [Carded] source reads its cards no older than; zero from a source that
	// draws no card of its own.
	At statelog.Position
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
	// Cards asks every [Carded] source for the cards of its notices the page
	// takes — a read that draws them, as a list a person opens a notice from
	// does. The zero value asks none: a count, a badge or a row draws a
	// notice from what it carries (this file's head).
	Cards bool
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
	page := Page{Handle: q.Handle, Notices: []Notice{}, Sources: []SourceState{}, Complete: true}
	var heads []*sourceHead
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
		heads = append(heads, &sourceHead{source: s, name: s.Name(), page: got})
	}
	page.ReadLevel = statelog.Weakest(levels...)
	for len(page.Notices) < limit {
		var next *sourceHead
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
		// EVERY NOTICE OF THE SOURCE AT THIS POSITION, the first and those
		// that share it — this file's head.
		at := next.page.Notices[next.taken].Position
		for next.taken < len(next.page.Notices) && next.page.Notices[next.taken].Position == at {
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
	}
	// ONE POSITION PER SOURCE: a source read to its end, every notice of its
	// last page taken and nothing after it, is done; one whose page was
	// taken whole resumes at its own cursor, past every row it scanned —
	// those it left out included (this file's head); one taken in part, at
	// the last notice of its the page took; and one the page took none of,
	// at the cursor it was asked with.
	after := map[string]string{}
	for name, cursor := range cursors {
		after[name] = cursor
	}
	more := false
	for _, h := range heads {
		switch {
		case h.taken == len(h.page.Notices) && h.page.Next == "":
			after[h.name] = sourceDone
		case h.taken == len(h.page.Notices):
			after[h.name] = h.page.Next
			more = true
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
	if q.Cards {
		if err := cardsOf(ctx, &page, heads); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

// sourceHead is one source's page as a composition merges it: how many of its
// notices the page has taken.
type sourceHead struct {
	source Source
	name   string
	page   SourcePage
	taken  int
}

// cardsOf asks each [Carded] source of heads for the cards of the notices of
// it the page took, after the merge (this file's head), and puts each in its
// notice's place. A source's failure is the read's, as its page's is.
func cardsOf(ctx context.Context, page *Page, heads []*sourceHead) error {
	for _, h := range heads {
		carded, ok := h.source.(Carded)
		if !ok || h.taken == 0 {
			continue
		}
		var at []int
		var took []Notice
		for i, n := range page.Notices {
			if n.Source == h.name {
				at = append(at, i)
				took = append(took, n)
			}
		}
		cards, err := carded.Cards(ctx, h.page, took)
		switch {
		case err != nil:
			return fmt.Errorf("inbox: read the cards of the %s notices: %w", h.name, err)
		case len(cards) != len(took):
			return fmt.Errorf("inbox: the %s source answered %d cards for %d notices", h.name, len(cards),
				len(took))
		}
		for i, card := range cards {
			card.Source = h.name
			page.Notices[at[i]] = card
		}
	}
	return nil
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
		return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
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
