package executordomain

import (
	"strings"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE BOUNDS ON WHAT IS STORED. They are this package's because the writer is
// where a value is refused (or, for prose, cut) before it is published, and the
// pieces that capture and restore sessions read them from here rather than
// stating their own: a bound written twice is one the two sides disagree about,
// and the side that lost would be a record the log refuses `record_too_large`
// after the turn already ended.
const (
	// SessionMaxBytes bounds one captured session's ARCHIVE, before it is
	// sealed: a coding CLI's transcript and its sidecars, packed.
	//
	// THIRTY-TWO MEBIBYTES. A Claude Code or OpenCode session of a day's
	// work compresses to one or two mebibytes, and the long ones that read
	// whole files into their context reach the low tens; past thirty-two a
	// transcript is a context the CLI could not resume within its own
	// window anyway. The bound is also what one capture or restore costs a
	// node while it streams (one chunk, [secrets.StreamChunkBytes]) and the
	// object store at rest ([SealedMaxBytes]), and it is pinned against
	// objstore.RecordWithin at the objstore.MiBPace floor in
	// internal/objstore/references.
	SessionMaxBytes = 32 << 20

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

	// CLIMaxBytes and CLIVersionMaxBytes bound the CLI's name and its
	// version as the box reported it.
	CLIMaxBytes        = 32
	CLIVersionMaxBytes = 64

	// SessionIDMaxBytes bounds the CLI's own session id: Claude Code's is a
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

	// CompletedWorkMaxBytes and BlockedOnMaxBytes bound the reviewer's
	// account of what landed and the executor's evidence of what stopped a
	// blocked round: a paragraph each, four kibibytes, cut on a rune
	// boundary. They are model-written prose the next turn reads as the
	// ending of this one, so the cut is marked rather than silent.
	CompletedWorkMaxBytes = 4 << 10
	BlockedOnMaxBytes     = 4 << 10

	// StopReasonMaxBytes and FailureMaxBytes bound a stop's reason (a
	// person's sentence or two) and the engine's guard or loss account (one
	// of its own sentences).
	StopReasonMaxBytes = 1 << 10
	FailureMaxBytes    = 1 << 10

	// WordMaxBytes bounds the ending's engine words: the reviewer's
	// decision, the surface a reply was delivered to and the engine's own
	// answer outcome. Each is a short identifier the engine chooses, so one
	// longer than this is refused rather than cut.
	WordMaxBytes = 32

	// ActorMaxBytes bounds who stopped a turn: a seat's handle, a person's
	// login or a machine's `kind:name`, each at most iam.MaxLogin, with
	// room for the kind.
	ActorMaxBytes = 128

	// MaxEndings is how many endings newer than the head [Reader.Head]
	// returns, the newest first, beside a count of the rest: the account a
	// next turn is told of turns that ended without leaving a session. A
	// conversation whose every turn failed to capture keeps them until
	// retention, and sixteen is more than any next turn needs to read to know
	// what happened; the count says how many more there were.
	MaxEndings = 16

	// MaxSiblings is how many sibling sessions [Reader.Head] returns, the
	// newest first, beside a count of the rest: ended turns of the
	// conversation, with sessions, that no later turn absorbed because they
	// ran at once. Each is a session a launch may restore beside the head,
	// so the bound is on what a launch opens: eight.
	MaxSiblings = 8

	// MaxPageSize is the most rows [Reader.Sessions] returns at once, and
	// DefaultPageSize what a page that names none takes.
	MaxPageSize     = 100
	DefaultPageSize = 25
)

// SealedMaxBytes is the largest object a session is stored as: an archive of
// [SessionMaxBytes] sealed under a keyring key of the longest id a sealed
// stream can name. Every object is uploaded under this limit, and it is what
// the object store's pacing is held to.
var SealedMaxBytes = secrets.SealedStreamSize(SessionMaxBytes,
	strings.Repeat("k", secrets.MaxStreamKeyIDBytes))
