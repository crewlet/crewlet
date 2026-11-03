package executordomain

import "time"

// THE BOUNDS ON WHAT IS STORED. They are this package's because the writer is
// where a value is refused (or, for prose, cut) before it is published, and
// whatever else reads one reads it from here rather than stating its own: a
// bound written twice is one the two sides disagree about, and the side that
// lost would be a record the log refuses `record_too_large` after the turn
// already ended. The bounds the sandbox stops at too — a session's archive and
// its sealed object, and an ending's fields — are internal/executorkeep's, the
// leaf both read (its package doc says why).
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

	// TurnIDMaxBytes bounds a turn's id: a run id is a UUIDv7, thirty-six
	// bytes, and the bound refuses anything that is plainly not one.
	TurnIDMaxBytes = 64

	// HarnessMaxBytes and HarnessVersionMaxBytes bound the harness's name and its
	// version as the box reported it.
	HarnessMaxBytes        = 32
	HarnessVersionMaxBytes = 64

	// SessionIDMaxBytes bounds the harness's own session id: Claude Code's is a
	// UUID, OpenCode's a `ses_` prefix and twenty-six characters.
	SessionIDMaxBytes = 128

	// PathMaxBytes bounds the writer box's home and workspace paths, which a
	// resumed launch maps onto its own box. The engine lays a box out under
	// paths of a few dozen bytes; a kibibyte bounds a seat's own image
	// moving them without admitting a path no box would mount.
	PathMaxBytes = 1024

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
)

// LostAfter is how long after its last capture a turn that never ended, and of
// which the fleet holds no live run, is taken for one whose end was lost, and
// ended `lost` by the retention duty ([Writer.EndLost]).
//
// A DAY. A turn's run record is held for as long as its executor runs or is
// parked on a person's answer, and once the run is collected the turn's review
// ends it within minutes; so a day past its last capture with no run left is a
// turn whose end was never recorded — its process lost between the run and
// the record, or an end that answered unknown and was never asked again. Until
// it is ended its session holds the seat's byte budget, which releases only
// ended turns', and two such sessions near the archive's bound fill the default
// budget: so a day bounds how long a lost end can keep a seat from capturing,
// where waiting for the retention would be the whole conversation retention.
const LostAfter = 24 * time.Hour
