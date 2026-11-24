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
// read keeps its LAST part ([SummaryTail]), where every summary shape puts what
// to do next. The largest output a runner's harness sets on its compaction
// call ([summaryCapTokens]), at the four bytes a token the engine estimates a
// model's output by (internal/providers/llm/harness's EstimateTokens).
const SummaryMaxBytes = summaryCapTokens * 4

// summaryCapTokens is OpenCode 2.3.2's ceiling on its compaction call's
// maxTokens (`qz`: the model's output limit, else 32,000, never past 256,000;
// bin/opencode). Below it: Kimi Code 2.7.2's 131,072
// (DEFAULT_COMPACTION_MAX_COMPLETION_TOKENS, dist/main.mjs) and Qwen Code
// 0.28.4's 20,000 (COMPACT_MAX_OUTPUT_TOKENS). None of their own: Claude Code
// 2.1.322, Codex 0.171.0 (core/src/compact.rs), Gemini CLI 0.70.0, Hermes Agent
// 0.23.3 ("NEVER add a max_tokens wire cap") and Pi 1.6.0. Not read, in native
// code: Copilot CLI 1.0.114, Grok Build 1.0.92, Muse Code 1.4.25. Each is the
// release its runner pins.
const summaryCapTokens = 256_000

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

	// Bytes is the whole summary's size, scrubbed and redacted, Kept the bytes
	// kept of it, and Cut says those are its last part ([SummaryTail]).
	Bytes int64 `json:"bytes"`
	Kept  int64 `json:"kept"`
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
	case s.Kept <= 0 || s.Kept > SummaryMaxBytes || s.Kept > s.Bytes:
		return fmt.Errorf("the summary keeps %d bytes of %d, and a kept one holds 1 to %d of its whole",
			s.Kept, s.Bytes, SummaryMaxBytes)
	case s.Harness == "" || len(s.Harness) > HarnessMaxBytes || !utf8.ValidString(s.Harness):
		return fmt.Errorf("the summary's harness is not 1 to %d bytes of UTF-8", HarnessMaxBytes)
	case len(s.Session) > SessionIDMaxBytes || !utf8.ValidString(s.Session):
		return fmt.Errorf("the summary's session is not at most %d bytes of UTF-8", SessionIDMaxBytes)
	}
	return nil
}

// Within reports whether s is one every reader can open and place
// ([Summary.Validate]): what an apply and a backup's reader of a record ask
// before they name it.
func (s Summary) Within() bool { return s.Validate() == nil }

// summaryCutMark opens a summary kept from past its start ([SummaryTail]).
const summaryCutMark = "…"

// SummaryTail is a summary, already scrubbed and redacted, as it is kept: whole
// within [SummaryMaxBytes], else its LAST part ([TailWithin]); cut says it was.
func SummaryTail(text string) (kept string, cut bool) {
	kept, start := TailWithin(text, SummaryMaxBytes)
	return kept, start > 0
}

// TailWithin is text, redacted, whole within bound bytes, else its LAST part,
// the mark counted, cut on a rune boundary and never inside a marker the
// redaction wrote, which the cut moves on past ([redact.MarkersAround]), and
// marked at its start; start is where the part kept begins in text, 0 for the
// whole.
func TailWithin(text string, bound int) (kept string, start int) {
	if len(text) <= bound {
		return text, 0
	}
	start = len(text) - (bound - len(summaryCutMark))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	if _, end, inside := redact.MarkersAround(text, start); inside {
		start = end
	}
	return summaryCutMark + text[start:], start
}
