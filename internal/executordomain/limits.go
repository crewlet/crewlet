package executordomain

import "time"

// The bounds on what is stored, stated once here because the writer refuses
// (or, for prose, cuts) a value before publishing it: a bound written twice is
// one two sides disagree about, and the loser is a record the log refuses
// `record_too_large` after the turn ended. Bounds the sandbox stops at too are
// internal/executorkeep's (its package doc says why).
const (
	// ConversationKeyMaxBytes bounds a conversation's key, a surface's own id
	// for a thread, issue or channel (a GitHub `owner/repo#n` is under 200
	// bytes). A longer key is refused, not cut: a cut key would merge two
	// conversations.
	ConversationKeyMaxBytes = 512

	// WorkKeyMaxBytes bounds a turn's work key: internal/workkey's
	// thirty-two hex digits, with room for the whole SHA-256 should it widen.
	WorkKeyMaxBytes = 64

	// AskExcerptMaxBytes bounds what the turn was asked, as a list and a
	// search show it: the first kibibyte, cut on a rune boundary.
	AskExcerptMaxBytes = 1024

	// MaxEndings is how many endings [Reader.Head] returns beside the head
	// (ended turns no ended turn was shown), the newest, with a count of the
	// rest. Sixteen is more than a next turn
	// needs to know what happened; the ones left out reach the turn after,
	// since only what a read handed out is absorbed ([MaxAbsorbs]).
	MaxEndings = 16

	// MaxSiblings is how many of the endings [Reader.Head] returns may also
	// be offered as sibling sessions (ended turns that ran beside the head),
	// the newest, with a count of the rest. Each is a session a launch may
	// restore, so it bounds what a launch opens.
	MaxSiblings = 8

	// MaxAbsorbs is the most turns a capture may name as its first read's
	// ([Capture.Absorbs]): the head and every ending the read handed out.
	MaxAbsorbs = 1 + MaxEndings

	// MaxPageSize is the most rows [Reader.Sessions] returns at once, and
	// DefaultPageSize what a page that names none takes.
	MaxPageSize     = 100
	DefaultPageSize = 25

	// SearchScanRows is the most rows one page of [Reader.Search] examines:
	// a reference seat's whole history (thirty turns a day, see
	// [LinearizableReadsPerSeatDay], over the default thirty-day retention is
	// 900 rows, and absorption keeps far fewer). At about two KiB of words a
	// row (eleven at most), a page reads about two MiB in one transaction.
	// A larger seat is searched a page at a time, each page saying so.
	SearchScanRows = 1000

	// SearchSnippetBytes is the most of a hit's words its snippet shows:
	// about three lines, enough to show the matched word in its sentence
	// while a page of hits stays skimmable; the hit's row has the whole field.
	SearchSnippetBytes = 300
)

// LostAfter is how long after its last record (newest capture, else begin) a
// turn that never ended, with no live run in the fleet (one parked on a
// person's answer is live), is taken as one whose
// end was lost and ended `lost` by the retention duty ([Writer.EndLost]).
//
// A day: a collected run's review ends its turn within minutes, so a day with
// no run left means the end was never recorded. Until ended, its session holds
// the seat's byte budget (two near the archive bound fill the default) and a
// turn with no capture holds its base; a day bounds that, where waiting for
// retention would hold them the whole conversation retention.
const LostAfter = 24 * time.Hour
