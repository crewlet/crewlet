package executordomain

import "time"

// THE BOUNDS ON WHAT IS STORED. They are this package's because the writer is
// where a value is refused (or, for prose, cut) before it is published, and
// whatever else reads one reads it from here rather than stating its own: a
// bound written twice is one the two sides disagree about, and the side that
// lost would be a record the log refuses `record_too_large` after the turn
// already ended. The bounds the sandbox stops at too — a session's archive and
// its sealed object, an ending's fields, and what a capture's stamp names of
// its box — are internal/executorkeep's, the leaf both read (its package doc
// says why).
const (
	// ConversationKeyMaxBytes bounds a conversation's key: a surface's own
	// identity for a thread, an issue or a channel (a GitHub issue's
	// `owner/repo#n` is under two hundred bytes, a chat thread's two ids
	// under a hundred). A longer key is refused rather than cut, because a
	// cut key would merge two conversations.
	ConversationKeyMaxBytes = 512

	// WorkKeyMaxBytes bounds a turn's work key: internal/workkey's
	// thirty-two hex digits, with room for the whole SHA-256 should it widen.
	WorkKeyMaxBytes = 64

	// AskExcerptMaxBytes bounds what the turn was asked, as a list and a
	// search show it: the first kibibyte, cut on a rune boundary.
	AskExcerptMaxBytes = 1024

	// MaxEndings is how many endings [Reader.Head] returns beside the head,
	// the newest of them, with a count of the rest: the account a next turn
	// is told of the conversation's ended turns no ended turn was shown. A
	// conversation whose turns keep failing before they capture keeps them
	// until a turn that was shown them ends, and sixteen is more than any
	// next turn needs to read to know what happened; the count says how many
	// more there were, and the ones left out are shown to the turn after it,
	// because only what a read handed out is absorbed ([MaxAbsorbs]).
	MaxEndings = 16

	// MaxSiblings is how many of the endings [Reader.Head] returns may also
	// be offered as sibling sessions, the newest of them, beside a count of
	// the rest: ended turns of the conversation, with sessions, that no
	// ended turn was shown because they ran beside the head. Each is a
	// session a launch may restore beside the head, so the bound is on what
	// a launch opens: eight.
	MaxSiblings = 8

	// MaxAbsorbs is the most turns a capture may name as its first read's
	// ([Capture.Absorbs]): the head and every ending the read handed out.
	MaxAbsorbs = 1 + MaxEndings

	// MaxPageSize is the most rows [Reader.Sessions] returns at once, and
	// DefaultPageSize what a page that names none takes.
	MaxPageSize     = 100
	DefaultPageSize = 25

	// SearchScanRows is the most rows one page of [Reader.Search] examines.
	//
	// A THOUSAND, A REFERENCE SEAT'S WHOLE HISTORY: the reference company's
	// thirty turns a seat a day (see [LinearizableReadsPerSeatDay]) over the
	// thirty days the default retention keeps a row past its last record are
	// nine hundred rows, and a seat keeps far fewer, since an end absorbs
	// the turns its read handed out — about one row a conversation. A row's
	// words are at most about eleven KiB (an ask's [AskExcerptMaxBytes] and
	// an ending's prose at its bounds) and typically about two, so a page
	// reads about two MiB through the seat's index in one transaction of the
	// node that holds the rows, and answers only its hits. A seat past it is
	// searched a thousand rows a page, each page saying so.
	SearchScanRows = 1000

	// SearchSnippetBytes is the most of a hit's words its snippet shows.
	//
	// THREE HUNDRED BYTES, about three lines of prose: enough to show the
	// word a session was found by in its sentence, and small enough that a
	// page of hits is read in passing — the field it is cut from is on the
	// hit's row whole.
	SearchSnippetBytes = 300
)

// LostAfter is how long after its last record — its newest capture, or the
// begin of a turn that captured nothing — a turn that never ended, and of which
// the fleet holds no live run, is taken for one whose end was lost, and ended
// `lost` by the retention duty ([Writer.EndLost]).
//
// A DAY. A turn's run record is held for as long as its executor runs or is
// parked on a person's answer, and once the run is collected the turn's review
// ends it within minutes; so a day past its last record with no run left is a
// turn whose end was never recorded — its process lost between the run and
// the record, or an end that answered unknown and was never asked again. Until
// it is ended its session holds the seat's byte budget, which releases only
// ended turns', and two such sessions near the archive's bound fill the default
// budget; and a turn that captured nothing holds the base its begin named
// against every release but a retirement: so a day bounds how long a lost end
// can keep a seat from capturing, or a base from going, where waiting for the
// retention would be the whole conversation retention.
const LostAfter = 24 * time.Hour
