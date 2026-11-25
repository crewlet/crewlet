package executor

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HERMES SUMMARY IS READ AS THE TRANSCRIPT JOINS ITS ROW, ON THE RECEIPTS'
// WALK: the newest `messages` row flagged `_compressed_summary` and still
// active, of any launch, whatever order its members stand in, its content a
// string or its `text` parts' texts joined, each part's last; an inactive
// summary, a row of another table, a row one member refuses, a null content
// and one of no shape the row's decode joins keep the one before; a row past
// the row bound and a summary past the read's bound cannot be said.
//
// Mutation: take an inactive summary, a part of another type, a row one
// member refuses or a content of no shape, and a case goes red.
func TestAHermesSummaryIsReadAsTheTranscriptJoinsItsRow(t *testing.T) {
	t.Parallel()
	const earlier = `{"t": "messages", "r": {"id": 1, "role": "user", "_compressed_summary": 1, ` +
		`"content": "the earlier"}}`
	row := func(members string) string { return `{"t": "messages", "r": {"id": 9, ` + members + `}}` }
	for _, c := range []struct {
		name, line, text, unread string
	}{
		{name: "a string", line: row(`"role": "user", "_compressed_summary": 1, "content": "the newest"`),
			text: "the newest"},
		{name: "its flag after its content", line: row(`"content": "the newest", "_compressed_summary": 1`),
			text: "the newest"},
		{name: "active", line: row(`"_compressed_summary": 1, "active": 1, "content": "the newest"`),
			text: "the newest"},
		{name: "its text parts joined", line: row(`"_compressed_summary": 1, "content": [{"type": "text", ` +
			`"text": "the "}, {"type": "image", "text": "no"}, {"text": "newest", "type": "text"}]`),
			text: "the newest"},
		{name: "a part's last text", line: row(`"_compressed_summary": 1, "content": [{"type": "text", ` +
			`"text": "no", "text": "the newest"}]`), text: "the newest"},
		{name: "an inactive summary", line: row(`"_compressed_summary": 1, "active": 0, "content": "no"`),
			text: "the earlier"},
		{name: "no summary", line: row(`"role": "user", "content": "no"`), text: "the earlier"},
		{name: "another table's", line: `{"t": "sessions", "r": {"id": 9, "_compressed_summary": 1, ` +
			`"content": "no"}}`, text: "the earlier"},
		{name: "a row one member refuses", line: row(`"_compressed_summary": 1, "role": 7, "content": "no"`),
			text: "the earlier"},
		{name: "a null content", line: row(`"_compressed_summary": 1, "content": null`), text: "the earlier"},
		{name: "a content of no shape", line: row(`"_compressed_summary": 1, "content": 7`),
			text: "the earlier"},
		{name: "a part of no shape", line: row(`"_compressed_summary": 1, "content": ["the newest"]`),
			text: "the earlier"},
		{name: "a part's text of no shape", line: row(`"_compressed_summary": 1, "content": [{"type": "text", ` +
			`"text": 7}]`), text: "the earlier"},
		{name: "a list holding a part of no shape", line: row(`"_compressed_summary": 1, "content": [{"type": ` +
			`"text", "text": "the newest"}, 5]`), text: "the earlier"},
		{name: "a summary past the read's bound", line: row(`"_compressed_summary": 1, "content": "` +
			strings.Repeat("x", summaryReadMaxBytes+1) + `"`), unread: errSummaryPast.Error()},
		{name: "a row past the row bound", line: row(`"_compressed_summary": 1, "content": "` +
			strings.Repeat("x", hermesRowMaxBytes) + `"`), unread: "past the"},
	} {
		var s summaryRead
		if _, err := hermesNotesHeld(t.Context(), strings.NewReader(earlier+"\n"+c.line+"\n"), nil, 0,
			&s); err != nil {
			s.fail(err.Error())
		}
		got := s.reading()
		switch {
		case c.unread != "" && !strings.Contains(got.Unread, c.unread):
			t.Errorf("%s: read %q (unread %q), want it unsaid: %s", c.name, got.Text, got.Unread, c.unread)
		case c.unread == "" && (got.Text != c.text || got.Unread != ""):
			t.Errorf("%s: read %q (unread %q), want %q", c.name, got.Text, got.Unread, c.text)
		}
	}
}

// hermesSummaryWhole is the whole-row read of a record's newest summary: the
// last `messages` row flagged a summary and active, its words as the whole
// row's text ([hermesMessageWhole.text]), held to [summaryReadMaxBytes]; a
// row past the row bound makes it one that cannot be said.
func hermesSummaryWhole(r io.Reader) (sandbox.ThreadSummary, bool) {
	var s summaryRead
	err := readLines(bufio.NewReaderSize(r, 64<<10), hermesRowMaxBytes, func(line []byte) error {
		row, ok := hermesRowOfWhole(line)
		m := row.Message
		switch {
		case !ok || row.Table != "messages" || !m.summary() || m.Active != nil && *m.Active == 0:
		case len(m.text()) > summaryReadMaxBytes:
			s.unshaped(errSummaryPast.Error())
		default:
			s.take(m.text())
		}
		return nil
	}, func(int64, []byte, []byte) error {
		s.unshaped(fmt.Sprintf("a row of the session runs past the %d bytes one row is read to", hermesRowMaxBytes))
		return nil
	})
	return s.reading(), err == nil
}

// A HERMES SUMMARY ON THE WALK IS ITS WHOLE-ROW READ'S: over every recorded
// session, every line a person's read reads, a summary row of every content a
// collection's read reads and of each of its flags, each after a summary and
// torn, and every line at once, the receipts' walk, read whole and in pieces
// of seventeen bytes, from the start and past a row, answers the summary the
// whole-row read answers.
//
// Mutation: take a part written before its type as of no type, or a summary
// row's copy as no summary: a case fails.
func TestAHermesSummaryOnTheWalkIsItsWholeRowRead(t *testing.T) {
	t.Parallel()
	q := boundedQuoted
	summary := func(members string) string {
		return `{"t": "messages", "r": {"id": 9, "role": "user", ` + members + `, "message_uid": "s9"}}`
	}
	records := map[string]string{}
	for _, fixture := range hermesRecordFixtures(t) {
		records[fixture] = string(hermesFixture(t, "0.23.3", fixture))
	}
	lines := hermesPersonVariants()
	for _, content := range []string{q("the newest"), q("  "), `[{"type": "text", "text": "the "}, {"type": ` +
		`"image", "text": "x"}, {"text": "newest", "type": "text"}]`, `[{"type": "text", "text": "a", "text": "b"}]`,
		`[{"type": "text", "type": "image", "text": "x"}]`, `[{"type": "text", "text": null}]`, `[null]`, `["x"]`,
		`[{"type": 5, "text": "x"}]`, `[{"type": "text", "text": 5}]`, `[{"type": "text", "text": "x"}, 5]`,
		`5`, `null`, `{}`,
		q(strings.Repeat("x", summaryReadMaxBytes+1))} {
		for _, flags := range []string{`"_compressed_summary": 1`, `"_compressed_summary": 1, "active": 0`,
			`"_compressed_summary": 1, "active": 1`, `"_compressed_summary": 1, "active": null`,
			`"_compressed_summary": 0`, `"_compressed_summary": null`, `"_compressed_summary": "1"`} {
			lines = append(lines, summary(flags+`, "content": `+content),
				summary(`"content": `+content+`, `+flags))
		}
	}
	base := summary(`"_compressed_summary": 1, "content": "the earlier"`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = base + "\n" + l + "\n"
		records[fmt.Sprintf("torn line %d", i)] = base + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	for name, record := range records {
		want, read := hermesSummaryWhole(strings.NewReader(record))
		for _, c := range []struct {
			walk string
			r    io.Reader
			from int64
		}{
			{"whole", strings.NewReader(record), 0},
			{"in pieces, past a row", &exportPieces{r: strings.NewReader(record), n: 17}, 5},
		} {
			var s summaryRead
			if _, err := hermesNotesHeld(t.Context(), c.r, nil, c.from, &s); err != nil {
				s.fail(err.Error())
			}
			switch got := s.reading(); {
			case !read && got.Unread == "":
				t.Errorf("%s, %s: read %q, want it unread as the whole-row read fails", name, c.walk, got.Text)
			case read && got != want:
				t.Errorf("%s, %s: read %+v, want %+v", name, c.walk, got, want)
			}
		}
	}
}
