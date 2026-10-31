package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
)

// fakeSource is a domain's inbox over a fixed list of notices, newest first
// in its own order, its cursor the position of the last notice a page took —
// each page cut after its cursor, as a domain's scan is.
type fakeSource struct {
	name    string
	stream  string
	notices []inbox.Notice
	asked   []inbox.SourceQuery
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Notices(_ context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	f.asked = append(f.asked, q)
	start := 0
	if q.Cursor != "" {
		i := slices.IndexFunc(f.notices, func(n inbox.Notice) bool { return n.Position.String() == q.Cursor })
		if i < 0 {
			return inbox.SourcePage{}, fmt.Errorf("%s: no notice at %s", f.name, q.Cursor)
		}
		start = i + 1
	}
	end := min(start+q.Limit, len(f.notices))
	page := inbox.SourcePage{Notices: slices.Clone(f.notices[start:end]),
		SeenThrough: inbox.Position{Stream: f.stream, Generation: 1, Seq: 1}}
	if end < len(f.notices) {
		page.Next = f.notices[end-1].Position.String()
	}
	return page, nil
}

// noticesAt is a source's notices at the given minutes past base, newest
// first as given, each at the next position down on stream.
func noticesAt(name, stream string, base time.Time, minutes ...int) *fakeSource {
	f := &fakeSource{name: name, stream: stream}
	for i, m := range minutes {
		f.notices = append(f.notices, inbox.Notice{RecordID: fmt.Sprintf("%s-%d", name, m),
			Position: inbox.Position{Stream: stream, Generation: 1, Seq: uint64(100 - i)},
			At:       base.Add(time.Duration(m) * time.Minute)})
	}
	return f
}

// ids is a page's notices as source-minute.
func ids(p inbox.Page) []string {
	var out []string
	for _, n := range p.Notices {
		out = append(out, n.RecordID)
	}
	return out
}

var composeBase = time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)

// THE INBOX INTERLEAVES BOTH DOMAINS BY WHEN THEY HAPPENED: a tracker notice at
// 9:50, a code notice at 9:40, a tracker one at 9:30 and a code one at 9:20
// read in that order, each naming its source; a tie of instants goes by the
// source's name; and each source is asked the same narrowing.
//
// Mutation: append one source's page after the other's — the 9:40 code notice
// reads after the 9:30 tracker one, red.
func TestTheInboxInterleavesBothDomainsByWhenTheyHappened(t *testing.T) {
	t.Parallel()
	tracker := noticesAt("tracker", "CREWLET_TRACKER_LOG", composeBase, 50, 30, 10)
	code := noticesAt("code", "CREWLET_CODE_LOG", composeBase, 40, 20, 10)
	page, err := inbox.Compose(t.Context(), inbox.Query{Handle: "founder", Unread: true,
		Snoozed: inbox.SnoozeExclude, Limit: 10}, []inbox.Source{tracker, code})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	want := []string{"tracker-50", "code-40", "tracker-30", "code-20", "code-10", "tracker-10"}
	if got := ids(page); !slices.Equal(got, want) {
		t.Fatalf("the composed inbox reads %v, want %v", got, want)
	}
	for _, n := range page.Notices {
		if n.Source == "" || n.RecordID[:len(n.Source)] != n.Source {
			t.Errorf("notice %s names source %q", n.RecordID, n.Source)
		}
	}
	for _, s := range []*fakeSource{tracker, code} {
		if len(s.asked) != 1 || !s.asked[0].Unread || s.asked[0].Snoozed != inbox.SnoozeExclude ||
			s.asked[0].Handle != "founder" {
			t.Errorf("%s was asked %+v, want founder's unread notices, snoozes hidden", s.name, s.asked)
		}
	}
	if page.NextCursor != "" || page.Unread != 6 || len(page.Sources) != 2 {
		t.Errorf("a page holding both sources whole answers cursor %q, %d unread, sources %+v", page.NextCursor,
			page.Unread, page.Sources)
	}
}

// A CURSOR RESUMES EACH SOURCE: pages of three over a tracker and a code inbox
// read every notice once, in the composed order, each page's cursor carrying
// one position per source — the last of that source's notices it showed — and
// a source read to its end is asked nothing more; a cursor naming a source the
// inbox does not read is refused.
//
// Mutation: resume every source from the page's last notice — the source that
// did not supply it skips or repeats, red.
func TestACursorResumesEachSource(t *testing.T) {
	t.Parallel()
	tracker := noticesAt("tracker", "CREWLET_TRACKER_LOG", composeBase, 55, 54, 53, 20, 10)
	code := noticesAt("code", "CREWLET_CODE_LOG", composeBase, 50, 40, 5)
	var all []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatalf("the inbox did not end after %d pages: %v", pages, all)
		}
		page, err := inbox.Compose(t.Context(), inbox.Query{Handle: "founder", Snoozed: inbox.SnoozeInclude,
			Limit: 3, Cursor: cursor}, []inbox.Source{tracker, code})
		if err != nil {
			t.Fatalf("Compose at %q: %v", cursor, err)
		}
		all = append(all, ids(page)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"tracker-55", "tracker-54", "tracker-53", "code-50", "code-40", "tracker-20", "tracker-10",
		"code-5"}
	if !slices.Equal(all, want) {
		t.Fatalf("paging read %v, want %v", all, want)
	}
	if _, err := inbox.Compose(t.Context(), inbox.Query{Handle: "founder", Snoozed: inbox.SnoozeInclude,
		Cursor: "pages=X"}, []inbox.Source{tracker, code}); !errors.Is(err, inbox.ErrBadCursor) {
		t.Errorf("a cursor naming another source answered %v, want ErrBadCursor", err)
	}
}
