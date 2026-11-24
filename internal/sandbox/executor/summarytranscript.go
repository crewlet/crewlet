package executor

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/words"
)

// SummaryTranscript is a session's newest summary as it was kept apart, text,
// read as a transcript of one [EntrySummary] entry recorded at at: from the
// entry from on, held to by's bounds ([Reader.Bounds]) and narrowed by filter,
// redacted and cut as every entry is. What a read surface hands out of a turn
// whose session is not kept and whose summary is, so the two read alike.
func SummaryTranscript(by Reader, filter TranscriptFilter, from int, at time.Time, text string) (Transcript, error) {
	switch {
	case !by.Valid():
		return Transcript{}, fmt.Errorf("executor: a transcript is read for %s, not %q", words.Or(Readers), by)
	case from < 0:
		return Transcript{}, fmt.Errorf("executor: a transcript is read from its entry 0 on, or a later one, "+
			"not from %d", from)
	}
	b := newTranscriptBuilder(from, by.Bounds(), filter)
	if err := b.add(TranscriptEntry{Kind: EntrySummary, At: carriedInstant(at)}, text); err != nil &&
		!errors.Is(err, errTranscriptPage) {
		return Transcript{}, err
	}
	return Transcript{Entries: b.entries, Places: b.places, From: b.from, Next: b.next, Cut: b.cut}, nil
}
