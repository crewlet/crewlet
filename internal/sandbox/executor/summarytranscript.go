package executor

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/words"
)

// SummaryTranscript is a session's newest summary as it was kept apart, text,
// read as a transcript of one [EntrySummary] entry recorded at at: from the
// entry from on, held to by's bounds ([Reader.Bounds]) and narrowed by filter,
// redacted as every entry is. ITS END, not its opening, where it is past the
// entry's bound ([TranscriptEntry.Tail]): what to do next comes last in every
// summary. A match is the window around it, as any entry's.
func SummaryTranscript(by Reader, filter TranscriptFilter, from int, at time.Time, text string) (Transcript, error) {
	switch {
	case !by.Valid():
		return Transcript{}, fmt.Errorf("executor: a transcript is read for %s, not %q", words.Or(Readers), by)
	case from < 0:
		return Transcript{}, fmt.Errorf("executor: a transcript is read from its entry 0 on, or a later one, "+
			"not from %d", from)
	}
	bounds := by.Bounds()
	b := newTranscriptBuilder(from, bounds, filter)
	shown, start, whole := text, 0, 0
	if len(filter.Terms) == 0 {
		redacted := redact.Secrets(text)
		whole = len(redacted)
		shown, start = executorkeep.TailWithin(redacted, bounds.EntryBytes)
	}
	if err := b.add(TranscriptEntry{Kind: EntrySummary, At: carriedInstant(at)}, shown); err != nil &&
		!errors.Is(err, errTranscriptPage) {
		return Transcript{}, err
	}
	if start > 0 && len(b.entries) == 1 {
		e := &b.entries[0]
		e.Bytes, e.Cut, e.Tail = whole, true, true
		b.places[0].TextFrom = start
	}
	return Transcript{Entries: b.entries, Places: b.places, From: b.from, Next: b.next, Cut: b.cut}, nil
}
