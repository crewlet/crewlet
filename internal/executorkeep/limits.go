package executorkeep

import (
	"errors"
	"fmt"
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
	// FORTY-EIGHT MEBIBYTES: one whole-file read at a harness's limit beside
	// about 8 MiB of ordinary work. Claude Code keeps a whole 20 MiB PDF read
	// as 40.13 MiB (gzip level 7), OpenCode the same file as an image as
	// 20.1 MiB; a session holding more media than that is still not kept
	// (crewlet/crewlet#1626): a pack's trim drops lines, never media, and a
	// text session meets [SessionPlainMaxBytes] first. A capture or restore
	// streams it a chunk at a time ([secrets.StreamChunkBytes]), the store keeps
	// [SealedMaxBytes], and it is pinned against objstore.RecordWithin at
	// objstore.MiBPace in internal/objstore/references.
	SessionMaxBytes = 48 << 20

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

	// SpentMaxSessions bounds [Ending.SpentSessions]. EIGHT: a launch names at most
	// two sessions (the one it continued, the one its script started beside it), so
	// four launches of one turn that each spent theirs, past what a reviewer lets a
	// turn do.
	SpentMaxSessions = 8

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

	// TurnIDMaxBytes bounds a turn's id (a 36-byte UUIDv7) and HarnessMaxBytes
	// the harness's name as the box reported it.
	//
	// PathMaxBytes bounds the writer box's home and workspace paths, which a
	// resumed launch maps onto its own box: the engine's are a few dozen
	// bytes, and a kibibyte admits a seat's own image without a path no box
	// would mount.
	//
	// HERE, BESIDE THE VERSION'S AND THE SESSION ID'S, for their reason: the
	// executor log refuses a row past any of them, and the sandbox holds a
	// capture's stamp to the same bounds first (sandbox.SessionCapture.Bounded).
	TurnIDMaxBytes  = 64
	HarnessMaxBytes = 32
	PathMaxBytes    = 1024

	// EntryMaxBytes bounds the providers.llm entry a session ran under, an
	// identity of 32 hex characters (internal/providers/llm/harness's
	// Provider.EntryID): twice that, so the bound is never what refuses a
	// well-formed one.
	EntryMaxBytes = 64
)

// SealedMaxBytes is the largest object a session is stored as: an archive of
// [SessionMaxBytes] sealed under a keyring key of the longest id a sealed
// stream can name. Every object is uploaded under this limit, and it is what
// the object store's pacing is held to.
var SealedMaxBytes = secrets.SealedStreamSize(SessionMaxBytes,
	strings.Repeat("k", secrets.MaxStreamKeyIDBytes))

const (
	// SessionKeptMaxBytes bounds what one capture keeps in all: its archive's
	// text object and every media object it names ([MediaObject]), before
	// they are sealed. The log refuses a capture past it, sealed
	// ([KeptSealedMaxBytes]).
	//
	// NINETY-FOUR MEBIBYTES, ruled with the drain of crewlet/crewlet#2020:
	// a take of a session at it, its scrub spool drained into its sealed
	// spools, peaks at 95.14 MiB before its objects' seal headers and
	// members, inside one take's room (internal/sandbox's takeRoomBytes,
	// 96.23 MiB); 95 MiB would leave them under a tenth of a mebibyte. Two
	// whole-file Claude Code reads (80.3 MiB kept) fit beside 13.7 MiB of text.
	SessionKeptMaxBytes = 94 << 20

	// MediaMaxCount bounds the media objects one capture names. THIRTY-TWO:
	// each object begun costs a whole objstore.MiBPace of its upload's
	// pace, sixteen minutes at the cap, and about 8 KiB of record.
	MediaMaxCount = 32
)

// KeptSealedMaxBytes is the most a capture's objects hold together, and so
// the most any one of its media objects holds: [SessionKeptMaxBytes] sealed,
// and the header and last chunk's tag each of [MediaMaxCount] more objects
// adds. It is what a media object is uploaded under and what the object
// store's pacing is held to for a capture's uploads.
var KeptSealedMaxBytes = secrets.SealedStreamSize(SessionKeptMaxBytes,
	strings.Repeat("k", secrets.MaxStreamKeyIDBytes)) +
	MediaMaxCount*secrets.SealedStreamSize(0, strings.Repeat("k", secrets.MaxStreamKeyIDBytes))

// GzipMostBytes is the most Go's gzip writes of n bytes it cannot shrink, at
// any level, in one member: flate stores such input as it came, five bytes of
// header to a block, and its levels end a block of literals at 32 KiB (seven
// to nine) or 64 KiB (one to six); counted here at 16 KiB, twice as often as
// any does, beside the member's own [GzipMemberBytes]. On 48 MiB of random
// bytes level seven writes 7,700 bytes more than it was given and one 3,865;
// internal/sandbox/executor's spool test holds it at the spool's level.
func GzipMostBytes(n int64) int64 {
	const block = 16 << 10
	return n + (n+block-1)/block*5 + GzipMemberBytes
}

// GzipMemberBytes is what one gzip member adds to its data: its 10-byte header,
// its 8-byte trailer and flate's last empty block, five.
const GzipMemberBytes = 10 + 8 + 5

// SpoolMembersMostBytes is the most a take's scrub spool adds in gzip members
// past its first: one for each unit that outgrows the 64 KiB a way of it is
// held in, and so at most one for each 64 KiB of the 256 MiB of plaintext a
// reading reads, each [GzipMemberBytes]: 92 KiB. A unit that writes more than
// it reads opens more, which the take's room then counts, closed.
// internal/sandbox/executor's wayRoomBytes and scrubPlainMax are those bounds,
// which its test holds together with these.
const SpoolMembersMostBytes = (256<<20/(64<<10) + 1) * GzipMemberBytes

// ErrSessionTooLarge reports an archive past [SessionMaxBytes]: nothing is
// recorded, and the turn's next launch starts from the session the row already
// names, or fresh.
var ErrSessionTooLarge = errors.New("executorkeep: the session is over its size bound")

// SessionPlainMaxBytes bounds one captured session's archive as it expands:
// 256 MiB of plaintext, what its take's scrub reads (internal/sandbox/executor's
// scrubPlainMax, which is this and says why). Text gzips past the ratio of the
// two bounds, so a text session meets this one before [SessionMaxBytes].
const SessionPlainMaxBytes = 256 << 20

// ErrSessionPlainTooLarge reports an archive that expands past
// [SessionPlainMaxBytes]: an [ErrSessionTooLarge], refused as one.
var ErrSessionPlainTooLarge = fmt.Errorf("%w: it expands past %d bytes of plaintext", ErrSessionTooLarge,
	SessionPlainMaxBytes)

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
