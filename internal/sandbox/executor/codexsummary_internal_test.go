package executor

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexSummaryWalks are the two walks of a rollout that read its newest
// summary, holding no line: the count's of the session's archive
// ([codexMeter]) and the receipts' of the rollout past from ([codexNotesHeld]).
func codexSummaryWalks(t *testing.T, record string, from int) map[string]sandbox.ThreadSummary {
	t.Helper()
	var counted, held summaryRead
	if _, err := codexMeter(t.Context(), bytes.NewReader(codexArchiveOf(t, record)), codexCarryRelease,
		&counted); err != nil {
		counted.fail(err.Error())
	}
	if _, err := codexNotesHeld(t.Context(), strings.NewReader(record), nil, from, &held); err != nil {
		held.fail(err.Error())
	}
	return map[string]sandbox.ThreadSummary{"the count's walk": counted.reading(),
		"the receipts' walk": held.reading()}
}

// A CODEX SUMMARY IS READ AS THE TRANSCRIPT READS A COMPACTION, ON EITHER WALK:
// the count's and the receipts' walk each answer the newest `compacted` line's
// message ([codexReading.commit]), wherever its type stands in the line, from
// the rollout's first line whatever the run's start; a null message or payload
// keeps the one before; a message or payload of another kind, or none, cannot
// be said; a fragment (an envelope member of another kind), another line's
// message and a torn last line are no compaction; a message past the read's
// bound cannot be said.
//
// Mutation: read another line's message, take a fragment, leave a payload of
// no shape said, or read the compaction only past the run's start, and a case
// goes red.
func TestACodexSummaryIsReadAsTheTranscriptReadsACompaction(t *testing.T) {
	t.Parallel()
	const earlier = `{"timestamp":"2026-11-23T09:00:00.000Z","type":"compacted","payload":{"message":"the earlier"}}`
	for _, c := range []struct {
		name, line, text, unread string
		torn                     bool
		from                     int
	}{
		{name: "a message", line: `{"type":"compacted","payload":{"message":"the newest"}}`, text: "the newest"},
		{name: "a message before the run's start", line: `{"type":"compacted","payload":{"message":"the newest"}}`,
			text: "the newest", from: 5},
		{name: "its type after its payload", line: `{"payload":{"message":"the newest"},"type":"compacted"}`,
			text: "the newest"},
		{name: "a null message", line: `{"type":"compacted","payload":{"message":null}}`, text: "the earlier"},
		{name: "a null payload", line: `{"type":"compacted","payload":null}`, text: "the earlier"},
		{name: "a message of no text", line: `{"type":"compacted","payload":{"message":7}}`,
			unread: codexCompactionUnshaped},
		{name: "a payload of no object", line: `{"type":"compacted","payload":"x"}`, unread: codexCompactionUnshaped},
		{name: "no payload", line: `{"type":"compacted"}`, unread: codexCompactionUnshaped},
		{name: "a fragment by its time", line: `{"timestamp":7,"type":"compacted","payload":{"message":"a fragment"}}`,
			text: "the earlier"},
		{name: "a fragment by its metadata", line: `{"type":"compacted","metadata":{"compaction_output":"yes"},` +
			`"payload":{"message":"a fragment"}}`, text: "the earlier"},
		{name: "another line's message", line: `{"type":"event_msg","payload":{"type":"agent_message",` +
			`"message":"no summary"}}`, text: "the earlier"},
		{name: "another line's message, its type after it", line: `{"payload":{"type":"agent_message",` +
			`"message":"no summary"},"type":"event_msg"}`, text: "the earlier"},
		{name: "a torn last line", line: `{"type":"compacted","payload":{"message":"torn"}}`, torn: true,
			text: "the earlier"},
		{name: "a message past the read's bound", line: `{"type":"compacted","payload":{"message":"` +
			strings.Repeat("x", summaryReadMaxBytes+1) + `"}}`, unread: errSummaryPast.Error()},
	} {
		record := earlier + "\n" + c.line
		if !c.torn {
			record += "\n"
		}
		for walk, got := range codexSummaryWalks(t, record, c.from) {
			switch {
			case c.unread != "" && !strings.Contains(got.Unread, c.unread):
				t.Errorf("%s, %s: read %q (unread %q), want it unsaid: %s", c.name, walk, got.Text, got.Unread,
					c.unread)
			case c.unread == "" && (got.Text != c.text || got.Unread != ""):
				t.Errorf("%s, %s: read %q (unread %q), want %q", c.name, walk, got.Text, got.Unread, c.text)
			}
		}
	}
}

// codexSummaryWhole is the whole-line read of a rollout's newest summary:
// each line decoded as its envelope, a `compacted` one's payload as its
// message, held to [summaryReadMaxBytes]; one whose payload does not decode
// cannot be said.
func codexSummaryWhole(r io.Reader) (sandbox.ThreadSummary, bool) {
	var s summaryRead
	_, err := codexLines(r, func(_ int, body []byte) error {
		var line codexTranscriptLine
		if !wireDecodes(body, &line) || line.Type != "compacted" {
			return nil
		}
		var c struct {
			Message *string `json:"message"`
		}
		switch {
		case !wireDecodes(line.Payload, &c):
			s.unshaped(codexCompactionUnshaped)
		case c.Message == nil:
		case len(*c.Message) > summaryReadMaxBytes:
			s.unshaped(errSummaryPast.Error())
		default:
			s.take(*c.Message)
		}
		return nil
	}, func(n int) error { return fmt.Errorf("%w: line %d", errCodexRolloutLine, n) })
	return s.reading(), err == nil
}

// A CODEX SUMMARY ON EITHER WALK IS ITS WHOLE-LINE READ'S: over every recorded
// rollout, every line a person's read and the count read (a compaction of each
// shape among them), each after a compaction and torn, and every line at once,
// the count's walk and the receipts' walk, read whole and in pieces of
// seventeen bytes, from the start and past the first line, answer the summary
// the whole-line read answers, and say it unread where that read fails.
//
// Mutation: read a compaction's message whatever its line's type, or a
// fragment's: a case fails.
func TestACodexSummaryOnEitherWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	records := map[string]string{}
	for _, fixture := range codexRolloutFixtures(t) {
		records[fixture] = string(codexFixture(t, fixture))
	}
	lines := append(codexPersonVariants(),
		`{"type":"compacted","payload":{"message":"the newest"}}`,
		`{"payload":{"message":"the newest"},"type":"compacted"}`,
		`{"type":"compacted","payload":{"message":null}}`, `{"type":"compacted","payload":null}`,
		`{"type":"compacted","payload":{"message":7}}`, `{"type":"compacted","payload":"x"}`,
		`{"type":"compacted"}`, `{"type":"compacted","payload":{"message":"   "}}`,
		`{"timestamp":7,"type":"compacted","payload":{"message":"a fragment"}}`,
		`{"type":"compacted","metadata":{"compaction_output":"yes"},"payload":{"message":"a fragment"}}`,
		`{"type":7,"payload":{"message":"a fragment"}}`,
		`{"type":"compacted","payload":{"message":"the first"},"payload":{"window_id":"w"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"no summary"}}`,
		`{"payload":{"type":"agent_message","message":"no summary"},"type":"event_msg"}`,
		`{"type":"compacted","payload":{"message":`+boundedQuoted(strings.Repeat("x", summaryReadMaxBytes+1))+`}}`)
	base := `{"type":"compacted","payload":{"message":"the earlier"}}`
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = base + "\n" + l + "\n"
		records[fmt.Sprintf("torn line %d", i)] = base + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	for name, record := range records {
		want, read := codexSummaryWhole(strings.NewReader(record))
		check := func(walk string, got sandbox.ThreadSummary) {
			t.Helper()
			switch {
			case !read && got.Unread == "":
				t.Errorf("%s, %s: read %q, want it unread as the whole-line read fails", name, walk, got.Text)
			case read && got != want:
				t.Errorf("%s, %s: read %+v, want %+v", name, walk, got, want)
			}
		}
		for walk, got := range codexSummaryWalks(t, record, 0) {
			check(walk, got)
		}
		var held summaryRead
		if _, err := codexNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, nil, 1,
			&held); err != nil {
			held.fail(err.Error())
		}
		check("the receipts' walk in pieces, past the first line", held.reading())
	}
}
