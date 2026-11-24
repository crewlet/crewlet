package executorkeep

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A SESSION'S NEWEST SUMMARY, KEPT APART: the harness's own account of the
// session before its newest compaction, read in the box at every box read
// (internal/sandbox's readBox) and kept as a sealed object of its own beside,
// never inside, the session's, so a session refused, too large or lost keeps
// it. Its words are never on a record: the object holds them, scrubbed and
// redacted, at most [SummaryMaxBytes]. The run's row, the turn's end
// ([Ending.Summary]) and the executor log's row each NAME it ([Summary]).

// SummaryMaxBytes bounds the summary a box read keeps, as kept: past it the
// read keeps its LAST part ([SummaryTail]), where every summary shape puts
// what to do next.
//
// SIXTY-FOUR KIBIBYTES.
const SummaryMaxBytes = 64 << 10

// SummarySealedMaxBytes is the largest object a summary is stored as:
// [SummaryMaxBytes] sealed under a keyring key of the longest id a sealed
// stream can name.
var SummarySealedMaxBytes = secrets.SealedStreamSize(SummaryMaxBytes,
	strings.Repeat("k", secrets.MaxStreamKeyIDBytes))

// Summary names a kept summary: the object, never its words.
type Summary struct {
	// Object is the sealed summary, and ContentKey the keyring key that
	// sealed it.
	Object     objstore.Object `json:"object"`
	ContentKey string          `json:"content_key"`

	// At is when the box read that found it kept it, by the reading node's
	// clock.
	At time.Time `json:"at"`

	// Bytes is the whole summary's size as kept, scrubbed and redacted, and
	// Cut says only its last [SummaryMaxBytes] were kept ([SummaryTail]).
	Bytes int64 `json:"bytes"`
	Cut   bool  `json:"cut,omitempty"`

	// Harness wrote it, and Session is the harness's session it summarises.
	Harness string `json:"harness"`
	Session string `json:"session"`
}

// Validate refuses a summary no reader could open or place: an object no row
// can name or past [SummarySealedMaxBytes], no keyring key, no instant, a size
// that is none, a harness or session id outside its bound.
func (s Summary) Validate() error {
	if err := s.Object.Validate(); err != nil {
		return fmt.Errorf("the summary's object: %w", err)
	}
	switch {
	case s.Object.Size <= 0 || s.Object.Size > SummarySealedMaxBytes:
		return fmt.Errorf("the summary's object is %d bytes, and a sealed summary is 1 to %d",
			s.Object.Size, SummarySealedMaxBytes)
	case s.ContentKey == "" || len(s.ContentKey) > secrets.MaxStreamKeyIDBytes ||
		!utf8.ValidString(s.ContentKey):
		return fmt.Errorf("the summary's content key is not 1 to %d bytes of UTF-8", secrets.MaxStreamKeyIDBytes)
	case s.At.IsZero():
		return fmt.Errorf("the summary names no instant it was kept at")
	case s.Bytes <= 0:
		return fmt.Errorf("the summary is %d bytes, and a kept one holds some", s.Bytes)
	case s.Harness == "" || len(s.Harness) > HarnessMaxBytes || !utf8.ValidString(s.Harness):
		return fmt.Errorf("the summary's harness is not 1 to %d bytes of UTF-8", HarnessMaxBytes)
	case len(s.Session) > SessionIDMaxBytes || !utf8.ValidString(s.Session):
		return fmt.Errorf("the summary's session is not at most %d bytes of UTF-8", SessionIDMaxBytes)
	}
	return nil
}

// summaryCutMark opens a summary kept from past its start ([SummaryTail]).
const summaryCutMark = "…"

// SummaryTail is a summary, already scrubbed and redacted, as it is kept: whole
// within [SummaryMaxBytes], else its LAST part, the mark counted, cut on a rune
// boundary and never inside a marker the redaction wrote, which the cut moves
// on past ([redact.MarkersAround]), and marked at its start; cut says it was.
func SummaryTail(text string) (kept string, cut bool) {
	if len(text) <= SummaryMaxBytes {
		return text, false
	}
	start := len(text) - (SummaryMaxBytes - len(summaryCutMark))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	if _, end, inside := redact.MarkersAround(text, start); inside {
		start = end
	}
	return summaryCutMark + text[start:], true
}
