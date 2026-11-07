package executorkeep

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
)

// THE BOUNDS ON WHAT IS KEPT. The executor log's writer is where a value is
// refused (or, for prose, cut) before it is published, and the pieces that
// capture and restore sessions stop at the same numbers: so each is declared
// once, here, where both read it (see the package doc).
const (
	// SessionMaxBytes bounds one captured session's ARCHIVE, before it is
	// sealed: a harness's transcript and its sidecars, packed.
	//
	// THIRTY-TWO MEBIBYTES. A Claude Code or OpenCode session of a day's
	// work compresses to one or two mebibytes, and the long ones that read
	// whole files into their context reach the low tens; past thirty-two a
	// transcript is a context the harness could not resume within its own
	// window anyway. The bound is also what one capture or restore costs a
	// node while it streams (one chunk, [secrets.StreamChunkBytes]) and the
	// object store at rest ([SealedMaxBytes]), and it is pinned against
	// objstore.RecordWithin at the objstore.MiBPace floor in
	// internal/objstore/references.
	SessionMaxBytes = 32 << 20

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

	// UnkeptReasonMaxBytes bounds why the turn's newest capture was not kept
	// ([Unkept]): the engine's sentence, quoting the refusal it met, as a
	// failure's account does.
	UnkeptReasonMaxBytes = 1 << 10

	// ExhaustedMaxSessions bounds the sessions an ending names as having run
	// out of context ([Ending.Exhausted]). EIGHT, from what a launch can name: at
	// most two sessions (the one it continued and the one its script started
	// beside it), so a whole turn of four launches that each ran out, far past
	// what a turn does before its reviewer gives up on it. A turn past the bound
	// keeps the first, the one its conversation's earlier rows can name.
	ExhaustedMaxSessions = 8

	// WordMaxBytes bounds the ending's engine words: the reviewer's
	// decision, the surface a reply was delivered to and the engine's own
	// answer outcome. Each is a short identifier the engine chooses, so one
	// longer than this is refused rather than cut.
	WordMaxBytes = 32

	// ActorMaxBytes bounds who stopped a turn: a seat's handle, a person's
	// login or a machine's `kind:name`, each at most iam.MaxLogin, with
	// room for the kind.
	ActorMaxBytes = 128

	// HarnessVersionMaxBytes bounds the harness's version as the box
	// reported it, and SessionIDMaxBytes the harness's own session id:
	// Claude Code's is a UUID, OpenCode's a `ses_` prefix and twenty-six
	// characters. The executor log refuses a row past either, and the
	// sandbox holds what a box names to the same bounds where its runner
	// reads the box and again as a launch's record is published
	// (sandbox.ValidRelease, sandbox.ValidSessionID): declared here, the
	// leaf both read, because the sandbox links no state log.
	HarnessVersionMaxBytes = 64
	SessionIDMaxBytes      = 128

	// TurnIDMaxBytes bounds a turn's id: a run id is a UUIDv7, thirty-six
	// bytes, and the bound refuses anything that is plainly not one.
	//
	// HarnessMaxBytes bounds the harness's name as the box reported it.
	//
	// PathMaxBytes bounds the writer box's home and workspace paths, which a
	// resumed launch maps onto its own box. The engine lays a box out under
	// paths of a few dozen bytes; a kibibyte bounds a seat's own image
	// moving them without admitting a path no box would mount.
	//
	// HERE, BESIDE THE VERSION'S AND THE SESSION ID'S, for their reason: the
	// executor log refuses a row past any of them, and the sandbox holds a
	// capture's stamp on a run's row to the same bounds before the row
	// carries it (sandbox.SessionCapture.Bounded) — the launch it was taken
	// in, the harness, and the paths the box names.
	TurnIDMaxBytes  = 64
	HarnessMaxBytes = 32
	PathMaxBytes    = 1024
)

// SealedMaxBytes is the largest object a session is stored as: an archive of
// [SessionMaxBytes] sealed under a keyring key of the longest id a sealed
// stream can name. Every object is uploaded under this limit, and it is what
// the object store's pacing is held to.
var SealedMaxBytes = secrets.SealedStreamSize(SessionMaxBytes,
	strings.Repeat("k", secrets.MaxStreamKeyIDBytes))

// ErrSessionTooLarge reports an archive past [SessionMaxBytes]: nothing is
// recorded, and the turn's next launch starts from the session the row already
// names, or fresh.
var ErrSessionTooLarge = errors.New("executorkeep: the session is over its size bound")

// SealedObject is an uploaded session: the object a capture names and the
// keyring key it was sealed under, which the capture records as its content
// key.
type SealedObject struct {
	Object     objstore.Object
	ContentKey string
}

// SealedSpool is a session sealed for an object the executor log's session
// store minted, waiting for its upload: the key the seal's associated data
// names, the keyring key that sealed it, and the sealed bytes' count.
type SealedSpool struct {
	Key        objstore.Key
	ContentKey string
	Size       int64
}
