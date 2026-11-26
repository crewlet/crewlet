package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// Each launch's harness session is kept here, and each turn's end recorded
// (ADR-0073). The argument is the package doc's, from "A launch's harness
// session is kept, and a turn's end recorded" to "The session switch", WHAT
// THE FENCE CANNOT REACH among it.

// SessionWrites is the executor log's writes a coordinator records through, in
// this package's words. The engine answers them over the estate router's
// writes (internal/estate's ExecutorWrites): a node without `data` records
// through a data node, one with it through its own copy, and each record is
// stated as the log's, with this package's author and the operation its place
// derives.
//
// IN THIS PACKAGE'S WORDS, NEVER THE LOG'S: the sandbox links no state log
// (its dependency gate), so the engine translates both ways — a record out,
// and the log's answer back as what a coordinator does next ([LogAnswer]).
// What both sides must agree on byte for byte, the turn's ending and a
// session's bounds, is internal/executorkeep's.
type SessionWrites interface {
	// Begin records a turn's first read of its conversation — where its head
	// was read, the turns the read handed out and the session it froze as the
	// turn's base — under the one operation the turn derives at
	// [BeginRecord.Minted], and answers whether the log, reading the turn's
	// row back, holds that base for it.
	Begin(ctx context.Context, b BeginRecord) (BeginAnswer, error)

	// Capture records a stamped, uploaded capture of a turn's session, under
	// the one operation its place derives — the turn, the launch it was
	// taken from and its ordinal there, at the instant the turn's
	// operations are minted at ([SessionCapture.Minted]) — so every attempt
	// at one capture is one operation, and the next capture another.
	Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error)

	// End records how a turn ended, and tombstones it, under the one
	// operation the turn derives at [EndRecord.Minted]: every path that ends
	// the turn derives the same one, so a turn ends once however many try.
	End(ctx context.Context, e EndRecord) (LogAnswer, error)
}

// CaptureRecord is one capture of a turn's session, asked of the executor log.
type CaptureRecord struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// Object is the sealed object the capture names, read off its stamp.
	Object objstore.Object

	// Stamp is the capture as the run's row stamps it: its place, the
	// harness's session it holds, the keyring key that sealed it and the
	// seat's budget the record carries.
	Stamp SessionCapture

	// ReadThrough and Absorbs are the turn's first read of its conversation
	// as its first launch froze it ([ConversationBase]): where the head was
	// read, packed, and the turns that read handed out, which the log keeps
	// where no begin of the turn landed first. ReadThrough is 0 for a turn
	// that read no conversation.
	ReadThrough int64
	Absorbs     []string
}

// BeginRecord is a turn's first read of its conversation, asked of the
// executor log as the turn's begin.
type BeginRecord struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// Minted is the instant the turn's derived operations are minted at
	// ([SessionKeep.MintedAt]), which the begin's operation derives from.
	Minted time.Time

	// ReadThrough is where the head was read on the executor log, packed;
	// Absorbs the turns the read handed out that the turn's brief told.
	ReadThrough int64
	Absorbs     []string

	// BaseTurn and BaseObject are the session the read froze as the turn's
	// base, which the begin holds against its release: "" and zero where the
	// read froze none.
	BaseTurn   string
	BaseObject objstore.Object
}

// BeginAnswer is how the executor log settled a turn's begin: the log's answer
// ([LogLanded] where it applied, or had the turn's first read already), and,
// where it landed, whether the turn's row, read back, holds the base, and why
// not where it holds none.
type BeginAnswer struct {
	Log  LogAnswer
	Held bool
	Why  string
}

// EndRecord is how one turn ended, asked of the executor log.
type EndRecord struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// Minted is the instant the turn's derived operations are minted at
	// ([SessionKeep.MintedAt]), which the end's operation derives from.
	Minted time.Time

	// Ask is what the turn was asked.
	Ask string

	Ending executorkeep.Ending

	// Served is [TurnEnd.Served].
	Served []string
}

// LogAnswer is how the executor log settled one record a coordinator asked of
// it: the four things a coordinator does differently next.
type LogAnswer string

const (
	// LogLanded is a record the log has: a capture the turn's row names now,
	// or an end the turn has — recorded now, or by another path that ended
	// the turn first, since the paths that end one turn derive one
	// operation and the log refuses a second end as already made.
	LogLanded LogAnswer = "landed"

	// LogPassedOver is a capture the log applied and the turn's row does not
	// name: an end, a newer capture, the seat's removal or the seat's budget
	// got there first. Never an end's answer.
	LogPassedOver LogAnswer = "passed_over"

	// LogRefused is a record the log refuses outright — a capture of a turn
	// that ended, of an ordinal not newer than the row's, of an object
	// uploaded too long ago, past the seat's budget or malformed; an end it
	// cannot take as written — which no retry changes. Its error says why.
	LogRefused LogAnswer = "refused"

	// LogUnsettled is a record no answer settles yet — a node that did not
	// answer, none that serves, an outcome still pending or unknown — which
	// may land if asked again under the same operation. Its error says what
	// was answered.
	LogUnsettled LogAnswer = "unsettled"
)

// SessionKeep is what keeping a session needs of the engine beside the log's
// writes, which this package cannot do for itself.
type SessionKeep interface {
	// Policy is whether sessions are kept, and the per-seat budget a capture
	// carries, as the revision the FLEET runs states them, or why this node
	// cannot know — every node applies the budget a capture carries, so no
	// node's own revision may decide it.
	Policy(ctx context.Context) (SessionPolicy, error)

	// Keeps is whether the revision THIS NODE runs keeps sessions: a hint,
	// read without a round trip, for the steps that only prepare a capture —
	// sealing what a launch handed its box, asking another node's host to
	// read a lost run's box — so a company that keeps none costs a launch
	// and a loss nothing for one. Never the word: a take keeps nothing until
	// Policy says so ([Coordinator.capturing]).
	Keeps() bool

	// SaveInterval is the shortest time between saves of a running job's
	// session, as the revision THIS NODE runs states it, read without a round
	// trip: a cadence binds no other node, each node paces only the boxes it
	// drives, and the poll that offers a save makes no round trip for it
	// ([Coordinator.SaveRunning]).
	SaveInterval() time.Duration

	// SealHanded seals the secrets a launch handed its box, encoded
	// ([HandedSecrets]), for the run's row, bound to its turn
	// ([PendingStore.Handed]); OpenHanded opens what it sealed, or says why it
	// cannot.
	SealHanded(turnID string, handed []byte) (string, error)
	OpenHanded(turnID, sealed string) ([]byte, error)

	// MintedAt is the instant a turn's derived operations are minted at: its
	// work's start, or the instant the turn was rebased onto where that start
	// is past the operation ledger's horizon (statelog.MintAt), as every
	// operation id a turn derives is.
	MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error)

	// Seal streams archive sealed under the fleet keyring into dst, under an
	// object key minted now (the executor log's session store, Seal), and
	// Upload puts what dst held into that object (its Upload). An archive
	// past [executorkeep.SessionMaxBytes] is [executorkeep.ErrSessionTooLarge].
	Seal(handle, turn string, archive io.Reader, dst io.Writer) (executorkeep.SealedSpool, error)
	Upload(ctx context.Context, spool executorkeep.SealedSpool, sealed io.Reader) (executorkeep.SealedObject, error)

	// SealMedia streams one payload's member sealed into dst under an object
	// key minted now, answering its digest with the spool (the session
	// store's SealMedia), and UploadMedia puts what dst held into that object
	// (its UploadMedia).
	SealMedia(handle string, member io.Reader, dst io.Writer) (executorkeep.MediaSpool, error)
	UploadMedia(ctx context.Context, spool executorkeep.MediaSpool, sealed io.Reader) (executorkeep.MediaObject, error)

	// PutSummary seals the summary text yields into a new object of its own,
	// under a key minted now, sealed apart from any session's (the executor
	// log's session store, PutSummary), and uploads it. A text past
	// [executorkeep.SummaryMaxBytes] is refused. CopySummary copies the
	// summary object holds into a new object the same way.
	PutSummary(ctx context.Context, handle, turn string, text io.Reader) (executorkeep.SealedObject, error)
	CopySummary(ctx context.Context, handle, turn string, object objstore.Object) (executorkeep.SealedObject, error)
}

// SessionPolicy is the company's word on keeping sessions.
type SessionPolicy struct {
	// Keep is `turn_engine.conversation_session.enabled`: false keeps no
	// session at all, and records every turn's end all the same.
	Keep bool

	// SeatBudget is `turn_engine.conversation_session.max_bytes_per_seat`,
	// carried by every capture so every node applies one number.
	SeatBudget int64

	// MediaApart says every node applying the executor log reads a capture
	// that names media, and every node that may settle a run's row asks its
	// stamp again whole ([CaptureStampVersion]), as read within
	// [MediaReadersRoom], so a take keeps its large payloads apart
	// ([SessionTake.MediaApart]); while one does not, or the nodes cannot be
	// read, a rolling upgrade's takes keep them inline, as the archive always
	// held them.
	MediaApart bool

	// MediaUnread is why the nodes could not be read, "" where they were and
	// MediaApart is their definite answer. Unknown keeps a take's media
	// inline, but never refuses a capture whose take already read them apart
	// ([Coordinator.readersBelow]).
	MediaUnread string

	// MediaBelow names, where MediaApart is a definite no, a node that falls
	// below and what it says: which version of which reading, beside the one
	// a capture naming media needs. The sentence a refused record says why
	// with ([Coordinator.readersBelow]); "" where the answer is yes or unknown.
	MediaBelow string

	// KeptBound says a take keeps a session up to
	// executorkeep.SessionKeptMaxBytes in all ([SessionCapture.KeptBound]):
	// every node that may settle a run's row says [KeptStampVersion], read in
	// MediaApart's pass and only beside it, since the text stays within
	// executorkeep.SessionMaxBytes only with its media out of it. While not, a
	// take keeps the text bound ([textBound]).
	KeptBound bool

	// KeptBelow names, where MediaApart holds and KeptBound is a definite no,
	// a node below [KeptStampVersion] and what it says: the sentence a refused
	// record says why with ([Coordinator.readersBelow]).
	KeptBelow string
}

// kept says a take under p keeps the kept bound: KeptBound, and never without
// its media apart.
func (p SessionPolicy) kept() bool { return p.KeptBound && p.MediaApart }

// CaptureStampVersion is the shape of a capture's stamp ([SessionCapture]) this
// build asks again whole and reads when it settles one
// ([Coordinator.settleCapture]): 2, its media included ([MediaStampVersion])
// and a stamp at the kept bound ([KeptStampVersion]). Every node records it
// (coord.CaptureStampRecord) and says it on its presence (coord.NodeStatus); a
// take keeps its media apart, or the kept bound, only once every node that may
// settle a run's row says that one's version. THE FORWARD MECHANISM
// (.claude/rules/compatibility.md): a build adding a key such an ask must carry,
// or a stamp an older build would misread, raises it and writes the stamp only
// once every such node says the new version; this build settles a stamp
// holding a key it does not know off the log rather than ask it short
// ([Coordinator.settleOffLog]).
const CaptureStampVersion = KeptStampVersion

// MediaStampVersion is the capture stamp version a node must say before a take
// keeps media apart ([SessionPolicy.MediaApart]): the first whose ask carries a
// capture's media. NAMED APART FROM [CaptureStampVersion], which a later stamp
// field raises while this threshold stands.
const MediaStampVersion = 1

// KeptStampVersion is the capture stamp version a node must say before a take
// keeps a session at the kept bound ([SessionPolicy.KeptBound]): the first that
// takes a stamp's archive up to executorkeep.SessionKeptMaxBytes
// ([SessionCapture.Bounded]) and reads a begun capture as kept for that bound's
// life ([SessionCaptureBudget]). A build below refuses its own write of such a
// stamp, and reads a keep still under way as abandoned and keeps it again.
const KeptStampVersion = 2

// SessionKept is what a box read took of the job's session, for the launch's
// own record (`executor_collected`), on whichever node collects: it rides the
// read's result back from the box's host.
//
// WHAT WAS TAKEN, NOT WHAT LANDED: the read answers once the session is out of
// the box and sealed, and its upload and record are made after, off the read
// ([Coordinator.keepCapture]); how they came out is stamped on the run's row
// and logged (`executor_session_kept`, `executor_session_not_kept`).
type SessionKept struct {
	// CaptureSeq is which of the launch's captures it was: for a read that
	// stood by for another's ([Coordinator.standBy]), the place of the
	// capture it stands by for, and in whose stead it keeps one in the next.
	CaptureSeq int64

	// Bytes is the size of the sealed session taken to be kept; zero where
	// none was.
	Bytes int64

	// NotKept is why the job's session was not taken to be kept — refused,
	// with why — and Loss that why in the one vocabulary. Both empty where it
	// was taken.
	NotKept string
	Loss    executorkeep.Loss
}

// CaptureState is how one capture of a run's session stands on the run's row.
type CaptureState string

const (
	// CaptureBegun is a capture whose place is reserved and whose archive
	// is on its way out of the box, or spooled and on its way to the store.
	CaptureBegun CaptureState = "begun"

	// CaptureLanded is a capture the executor log's row names.
	CaptureLanded CaptureState = "landed"

	// CaptureRefused is a capture no row will ever name, and why.
	CaptureRefused CaptureState = "refused"

	// CaptureUnresolved is a capture uploaded whose record was sent and not
	// answered: it may have landed. Asked again under its own operation by
	// the turn's end and a relaunch whatever its object's age, the log
	// refusing a record past objstore.RecordWithin by its own clock; read as
	// awaiting that answer until no node's clock can read its object within
	// the window ([pastRecordWithin]).
	CaptureUnresolved CaptureState = "unresolved"
)

// CaptureStates are the four.
var CaptureStates = []CaptureState{CaptureBegun, CaptureLanded, CaptureRefused, CaptureUnresolved}

// Valid reports whether this build knows s.
func (s CaptureState) Valid() bool { return slices.Contains(CaptureStates, s) }

// advancesTo reports whether a capture standing at s may come to stand at next:
// a begun one to any outcome, an unresolved one to an answer. Nothing moves a
// capture back, and nothing moves one that has its answer.
func (s CaptureState) advancesTo(next CaptureState) bool {
	switch s {
	case CaptureBegun:
		return next != CaptureBegun
	case CaptureUnresolved:
		return next == CaptureLanded || next == CaptureRefused
	}
	return false
}

// SessionCapture is one capture of the harness session a run's job worked in, as
// the run's row records it ([PendingRun.Capture], [PendingRun.Kept]).
//
// A WIRE FORMAT like the rest of the row: fields are added, never renamed, and a
// key a later build adds is carried in Extra (wirekeys.go).
type SessionCapture struct {
	// LaunchSeq and CaptureSeq are the capture's place: the launch it was
	// taken from ([PendingRun.LaunchSeq]) and its own among that launch's
	// captures, from 1 — the ordinals the executor log orders a turn's
	// captures by, so a late one never rolls the session back.
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

	// Launch is the job ([PendingRun.LaunchID]) whose box it was taken from.
	Launch string `json:"launch_id"`

	// State is how it stands, and Reason why a capture that did not land
	// did not, with Loss, that why in the one vocabulary
	// ([executorkeep.Loss]): every refusal names it, which the turn's end
	// carries ([unkeptOf]) and its next run says.
	State  CaptureState      `json:"state"`
	Reason string            `json:"reason,omitempty"`
	Loss   executorkeep.Loss `json:"loss,omitempty"`

	// Final says the job had ENDED when the capture was taken — it exited,
	// was ended by a stop, a loss or its turn's cap, or died with its host —
	// so nothing after it changes the session in its box, and a later read of
	// the same job takes nothing new. A job still working when it was taken
	// (one a box would not end) is not final.
	Final bool `json:"final,omitempty"`

	// Through is the session's SAVE POINT: how many calls the round's log had
	// taken ([BridgeLog.Appends]) at the one read that placed the capture. A job
	// whose harness ended its own run is placed by a read made once it was over
	// ([Coordinator.takeCapture]); every other capture is reserved at zero and
	// raised after its take (savepoint.go), since its harness may have made, and
	// answered, calls it never wrote. A continuing launch is told each call past
	// the read ([BridgeCall.Appended], [BridgeCall.Late], [BridgeLog.LateElided]),
	// more than once, never not at all (internal/agent/runner's calledAfter),
	// except a call whose result the session holds ([Coordinator.HoldingsOf]),
	// only to a box answering its release ([ExactTelling]). A POINTER: 0 holds no
	// calls, nil is "not stated" and is not continued ([followedCapture]); a
	// REFUSED stamp ([Coordinator.refuseUnplaced]) states its read's count, or nil.
	Through *int `json:"bridge_through,omitempty"`

	// Omitted is the reads at or below Through that the session holds only as
	// a count, by their places in the round's log ([BridgeCall.Appended]):
	// the reads the brief its job's run read counted without listing them, its
	// read cap spent ([BriefTold], [LaunchRecord.OmittedCalls]). A capture
	// placed by its own read states its job's; one raised to an earlier
	// capture's point, its launch's at or below that point
	// ([SessionCapture.placeAtNewest]). A launch that continues the session
	// tells each of them (internal/agent/runner's keptSession): placed by its
	// read, the session counts every call the log had taken as held, and
	// without this would never be told a read its brief only counted. Nil
	// where its brief left none out, and on a stamp an older build wrote.
	// Held to [MaxBridgeCalls] with LateOmitted ([SessionCapture.Bounded]).
	Omitted []int `json:"bridge_omitted,omitempty"`

	// LateOmitted is the late calls that brief counted without listing them,
	// apart from the reads it owes ([BriefTold.LateOmitted],
	// [LaunchRecord.LateOmittedCalls]), stated as Omitted is: a launch that
	// continues the session tells each of them as every late call is told, and
	// credits the session no telling of one (internal/agent/runner's
	// heldByTelling).
	LateOmitted []int `json:"bridge_late_omitted,omitempty"`

	// BridgeSeq and BridgeOpen are the same read's save point AS AN OLDER BUILD
	// READS IT — zero and nil on a capture no read places, so that build is
	// told every call — written for a fleet mid-upgrade and read by nothing of
	// this build: the last bridged call ([BridgeCall.Seq]) the log held
	// ([BridgeLog.LastSeq]), and the places at or below it holding no call
	// ([openPlaces]), since a call is recorded only once its tool answers and
	// a lower place can be a call in flight. That build tells a continuation
	// every call past BridgeSeq or at a BridgeOpen place; BridgeOpen keeps only
	// the highest [maxOpenPlaces], which a long round's dropped middle fills,
	// so a call in flight below them reads as answered, which only Through
	// mends. On a REFUSED stamp a BridgeSeq of zero is "not known".
	BridgeSeq  int   `json:"bridge_seq,omitempty"`
	BridgeOpen []int `json:"bridge_open,omitempty"`

	// NoteCursor is the last note read ([RunSteer.Reads]) the run's record says
	// the session holds ([notesHeld]): every note the record says was read when
	// the archive was packed, where its harness ended its own run, up to the
	// first read the record marks unanswered ([NoteRead.Unanswered]) or taken
	// before the session's restart ([NoteRead.Restarted]), or a hook's serve
	// unconfirmed, and not past it; none where it was cut off. READ ONLY where
	// the launch's own test says the copy holds its brief
	// (internal/agent/runner's keptSession): it hands every note read after it,
	// as notes the session may show where the cursor stopped early, and of a
	// copy that may lack its brief every note the turn has read.
	NoteCursor int `json:"note_cursor,omitempty"`

	// Interrupted says the session's harness was CUT OFF rather than exiting
	// cleanly: the archive was packed by the engine ([SessionTake.Pack]) — a
	// job still working, one lost with its engine, one that died before its
	// script could pack — or by the run's script after a harness that exited
	// otherwise than cleanly, killed by a signal or its memory limit
	// ([Result.HarnessCutOff]), or told to stop by an end a stop, a loss or its
	// turn's cap asked of it ([Result.JobEnd]). Its transcript may end
	// mid-record, its last tool call with no result. A launch that continues
	// it restores the archive over the copy in its box
	// ([CarriedSession.Replace]) and tells the run its transcript may end
	// mid-call.
	Interrupted bool `json:"interrupted,omitempty"`

	// ContextTokens is the whole input of the last request the session's
	// harness sent its model, cache included, as its own transcript recorded
	// it ([TakenSession.Context]): what a launch that continues the session
	// sends again before it reads a word of its brief, and so what that
	// launch's budget floor is made of. Zero where the transcript recorded
	// none this build reads.
	ContextTokens int64 `json:"context_tokens,omitempty"`

	// NextRequestTokens and NextRequestOlderTokens are the harness's own
	// estimate of the session's next request at each of the two rates Claude
	// Code counts text at ([NextRequest], [SessionCapture.NextRequest]), as the
	// take read its archive's main thread whole ([TakenSession.Next]): what a
	// launch that continues the session is held to with its brief, where its
	// last request's input (ContextTokens) under-counts it. Zero where the take
	// read none (a harness whose taker estimates none, a thread not read whole,
	// a stamp settled off the executor log, which carries ContextTokens alone,
	// a stamp an older build wrote), each read as ContextTokens.
	NextRequestTokens      int64 `json:"next_request_tokens,omitempty"`
	NextRequestOlderTokens int64 `json:"next_request_older_tokens,omitempty"`

	// ResumedCount is what its harness counts the session at before its next
	// request, as the take read its archive's main thread whole
	// ([TakenSession.Count], resumed.go), with the least window the run that
	// left it reported ([Result.ReportedWindow]): what a launch that continues
	// the capture judges its first request by (internal/agent/runner's
	// firstRequest). Nil where the take counted nothing (a harness whose taker
	// counts none, a stamp settled off the executor log, which carries the
	// context alone, a stamp an older build wrote), each read as a session
	// that may compact. Its mark is held to [maxCountMarkBytes]
	// ([SessionCapture.Bounded]).
	ResumedCount *ResumedCount `json:"resumed_count,omitempty"`

	// CountProvider is the provider of the reply an OpenCode ResumedCount is
	// anchored on, as the take read it ([TakenSession.CountProvider]): BESIDE
	// THE COUNT, NEVER IN IT, since ResumedCount is closed and an older build's
	// write carries this key in Extra ([LaunchRecord.CountProvider] has why a
	// count needs it). "" for a Claude Code count, where no reply anchored
	// one, and on a stamp an older build wrote. Held to [ValidCountProvider]
	// ([SessionCapture.Bounded]).
	CountProvider string `json:"count_provider,omitempty"`

	// At is when it was taken, on this node's clock.
	At time.Time `json:"at"`

	// Taker and TakerEpoch name the TAKER LEASE the capture was taken under
	// (see the package doc): its holder, the process incarnation that took
	// it — `<node>:<uuid>`, the taking coordinator's own
	// ([CoordinatorOptions.Incarnation]) — and the fencing token it held
	// then, on every stamp of the take. A wait for the capture settles it
	// from the executor log once that lease has lapsed or is held at another
	// epoch ([Coordinator.takerGone]), and the keep goes no further once its
	// own is ([takerLease.fence]). Empty on a stamp an older build wrote,
	// waited out by its age alone, and carried in [SessionCapture.Extra] by a
	// build that does not know them. The holder is held to
	// [maxIncarnationBytes] as the row's owner is ([SessionCapture.Bounded]).
	Taker      string `json:"taker,omitempty"`
	TakerEpoch int64  `json:"taker_epoch,omitempty"`

	// StandbyTaker, StandbyTakerEpoch and StandbyAt name ANOTHER READ OF THE
	// SAME JOB STANDING BY for this capture, where one does: a read that found
	// the capture's place taken took the job's session whole all the same,
	// spooled it on its own node, and keeps it in the next place should this
	// capture not land ([Coordinator.standBy]). Named by its taker lease's
	// holder and fencing token and the instant its take stamped
	// ([SessionCapture.At]). Written by [PendingStore.StandBy] alone, on the
	// newest capture not landed; carried by every later stamp of the take
	// ([stampOnto]); withdrawn by the read standing by or a wait that finds
	// its taker gone ([PendingStore.StandDown]); moot once the capture lands
	// or that read holds the next place ([PendingStore.TakeOver]). A wait
	// treats it as a capture being kept ([captureInFlight]) for [standbyBudget].
	StandbyTaker      string    `json:"standby_taker,omitempty"`
	StandbyTakerEpoch int64     `json:"standby_taker_epoch,omitempty"`
	StandbyAt         time.Time `json:"standby_at,omitzero"`

	// Harness, HarnessVersion and SessionID are the session as the box
	// described it; ArchiveFormat its layout.
	Harness        string `json:"harness,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ArchiveFormat  int    `json:"archive_format,omitempty"`

	// ArchiveSize is the length of the archive its take read out of its box, as
	// the box reported it before the scrub ([SessionArchive.Size]), stamped
	// with its place: what a relaunch waits for the capture by
	// ([captureRestFor]). Zero where no take read one.
	ArchiveSize int64 `json:"archive_size,omitempty"`

	// KeptBound is the take's bound ([SessionPolicy.KeptBound]): true, its
	// archive up to executorkeep.SessionKeptMaxBytes and its keep paced and
	// budgeted for it ([keptBound]), stamped with its place, so a takeover's
	// keep and a relaunch's wait ([captureRestFor]) size on the bound it took.
	// Written only once every node that may settle the row reads it.
	KeptBound bool `json:"kept_bound,omitempty"`

	// Entry is the providers.llm entry the session ran under: its job's
	// ([LaunchRecord.Entry]), never the box's word.
	Entry string `json:"entry,omitempty"`

	// RunBegan and RunAfter are where the run that packed the session began in
	// it, as the box described it ([SessionArchive.RunStart], read whole as
	// [SessionCapture.RunStart]): the session the run's own from its start, or
	// every message after the one RunAfter names, an id as the kept archive
	// spells it — the take passes it through the very rewrite every message id
	// of the archive passes, so the kept session holds that message under that
	// id — and none at all where that rewrite left no message id
	// ([ValidMessageID]). Neither, where the box said neither. Carried in
	// [SessionCapture.Extra] by a build that does not know them, and held to
	// [MaxMessageIDBytes] ([SessionCapture.Bounded]).
	RunBegan bool   `json:"run_began,omitempty"`
	RunAfter string `json:"run_after,omitempty"`

	// RunAfterLines is where a Gemini CLI or Qwen Code run began in its chat
	// file: after its first RunAfterLines lines, as the take found the box's
	// digest of them whole in the archive ([TakenSession.RunLines]); stamped
	// once the session is spooled, as the count is, never off the box's word
	// alone. Carried by an older build as RunBegan is, held to
	// [MaxRunStartLines].
	RunAfterLines int64 `json:"run_after_lines,omitempty"`

	// RunUnshadowed is the box's word that its run's harness could load no
	// other server spelling the engine's tools ([SessionArchive.Unshadowed]),
	// as the take found it; a Gemini CLI reading reads nothing without it.
	// Carried by an older build as RunBegan is.
	RunUnshadowed bool `json:"run_unshadowed,omitempty"`

	// Trimmed is the box's word that its pack cut the session to what its
	// harness's resume loads, past the bound ([SessionArchive.Trim]): the
	// history before its newest compaction is not in it, which Sessions and
	// read_session say. Carried by an older build as RunBegan is.
	Trimmed bool `json:"trimmed,omitempty"`

	// Object, ObjectHash, ObjectSize and ContentKey are the sealed object it
	// was uploaded into, once it was; Home and Workspace the box's paths,
	// which a later launch maps onto its own.
	Object     string `json:"object,omitempty"`
	ObjectHash string `json:"object_hash,omitempty"`
	ObjectSize int64  `json:"object_size,omitempty"`
	ContentKey string `json:"content_key,omitempty"`
	Home       string `json:"home,omitempty"`
	Workspace  string `json:"workspace,omitempty"`

	// Media is every media object the session's frames name, once uploaded
	// or carried ([CaptureMedia]), at most executorkeep.MediaMaxCount: none
	// for a session kept whole. ON THE STAMP, so a record asked again
	// ([Coordinator.settleCapture]) is the very record first asked; its
	// figure is [mediaRowBytes].
	Media []CaptureMedia `json:"media,omitempty"`

	// ArchiveBytes is the archive the session restores to, every member at
	// each frame naming it, as its take's body was read whole
	// ([TakenText.Restored]): stamped exactly where its media are spooled,
	// on the stamp for Media's reason, within executorkeep.ArchiveMaxBytes.
	ArchiveBytes int64 `json:"archive_bytes,omitempty"`

	// MediaCount is how many media objects the take's spool cut apart, every
	// first, carried or not, at most executorkeep.MediaMaxCount: stamped on
	// its BEGUN stamp as its keep starts ([Coordinator.stampMediaCount]), since
	// a relaunch waits for each object's paced upload ([captureRestFor]). Of
	// the stamp's shape at [MediaStampVersion].
	MediaCount int `json:"media_count,omitempty"`

	// SeatBudget and Minted are the record's: the per-seat budget it carries
	// and the instant its operation is derived at, kept so an unresolved
	// capture is asked again as the very operation it was.
	SeatBudget int64     `json:"seat_budget,omitempty"`
	Minted     time.Time `json:"minted,omitzero"`

	// Extra is every key of the capture this build does not know, kept
	// verbatim and written back by every write of the record (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`

	// Summary is the newest summary the take's read kept, which the write of
	// this stamp names on the run's row ([PendingRun.Summary]), where newer,
	// whether or not the stamp is taken: carried to the store, never kept in
	// the capture's section.
	Summary *SummaryStamp `json:"-"`
}

// ErrCaptureUnbounded is a capture stamp carrying a field past the bound the
// row holds it to ([SessionCapture.Bounded]), refused by
// [PendingStore.StampCapture] before anything is written.
var ErrCaptureUnbounded = errors.New("sandbox: a capture stamp carries a field past its bound")

// Bounded reports why c is past what a run's row carries of a capture, nil
// where it is within it: each text a box describes its session by, the paths
// it names and why a capture did not land, each within the bound the executor
// log holds it to (internal/executorkeep), the open places within
// [maxOpenPlaces], and the processes within [maxIncarnationBytes].
//
// WHAT A BOX SAYS OF ITS SESSION IS THE BOX'S OWN DOING: a file its run's own
// agent can write. Nothing else bounds it before the row, and the log's check
// runs only after the stamp has landed, on a row that then takes no later
// write, so never its delete. Refused here, the row never carries it; the
// reader of the box's record (internal/sandbox/executor) reads such a release
// as none.
func (c SessionCapture) Bounded() error {
	for _, f := range []struct {
		field, value string
		max          int
	}{
		{"launch_id", c.Launch, executorkeep.TurnIDMaxBytes},
		{"reason", c.Reason, executorkeep.UnkeptReasonMaxBytes},
		{"loss", string(c.Loss), executorkeep.WordMaxBytes},
		{"harness", c.Harness, executorkeep.HarnessMaxBytes},
		{"harness_version", c.HarnessVersion, executorkeep.HarnessVersionMaxBytes},
		{"entry", c.Entry, executorkeep.EntryMaxBytes},
		{"session_id", c.SessionID, executorkeep.SessionIDMaxBytes},
		{"run_after", c.RunAfter, MaxMessageIDBytes},
		{"home", c.Home, executorkeep.PathMaxBytes},
		{"workspace", c.Workspace, executorkeep.PathMaxBytes},
		{"taker", c.Taker, maxIncarnationBytes},
		{"standby_taker", c.StandbyTaker, maxIncarnationBytes},
	} {
		if len(f.value) > f.max {
			return fmt.Errorf("sandbox: its %s is %d bytes, past the %d it may be: %w", f.field,
				len(f.value), f.max, ErrCaptureUnbounded)
		}
	}
	if c.RunAfterLines < 0 || c.RunAfterLines > MaxRunStartLines {
		return fmt.Errorf("sandbox: its run's start is line %d, outside the 0 to %d it may be: %w",
			c.RunAfterLines, MaxRunStartLines, ErrCaptureUnbounded)
	}
	if bound := boundOf(c.KeptBound).archive; c.ArchiveSize < 0 || c.ArchiveSize > bound {
		return fmt.Errorf("sandbox: its archive is %d bytes, outside the 0 to %d it may be: %w",
			c.ArchiveSize, bound, ErrCaptureUnbounded)
	}
	if c.ArchiveBytes < 0 || c.ArchiveBytes > executorkeep.ArchiveMaxBytes {
		return fmt.Errorf("sandbox: its archive restores to %d bytes, outside the 0 to %d it may: %w",
			c.ArchiveBytes, executorkeep.ArchiveMaxBytes, ErrCaptureUnbounded)
	}
	if len(c.BridgeOpen) > maxOpenPlaces {
		return fmt.Errorf("sandbox: it names %d open places, past the %d it may: %w", len(c.BridgeOpen),
			maxOpenPlaces, ErrCaptureUnbounded)
	}
	if len(c.Media) > executorkeep.MediaMaxCount {
		return fmt.Errorf("sandbox: it names %d media objects, past the %d it may: %w", len(c.Media),
			executorkeep.MediaMaxCount, ErrCaptureUnbounded)
	}
	if c.MediaCount < 0 || c.MediaCount > executorkeep.MediaMaxCount {
		return fmt.Errorf("sandbox: it counts %d media objects, outside the 0 to %d it may: %w", c.MediaCount,
			executorkeep.MediaMaxCount, ErrCaptureUnbounded)
	}
	for _, m := range c.Media {
		_, keyErr := objstore.ParseKey(m.Object)
		if !executorkeep.ValidMediaDigest(m.Digest) || keyErr != nil ||
			len(m.ObjectHash) > 2*executorkeep.MediaDigestBytes || len(m.ContentKey) > secrets.MaxStreamKeyIDBytes {
			return fmt.Errorf("sandbox: its media %.64q is not one a record names: %w", m.Digest,
				ErrCaptureUnbounded)
		}
	}
	if n := len(c.Omitted) + len(c.LateOmitted); n > MaxBridgeCalls {
		return fmt.Errorf("sandbox: it names %d calls its brief left out, past the %d a round's log lists: %w",
			n, MaxBridgeCalls, ErrCaptureUnbounded)
	}
	if count := c.ResumedCount; count != nil {
		if len(count.Mark) > maxCountMarkBytes {
			return fmt.Errorf("sandbox: its count's mark is %d bytes, past the %d it may be: %w",
				len(count.Mark), maxCountMarkBytes, ErrCaptureUnbounded)
		}
		if count.Restart != nil && len(*count.Restart) > RestartIDMaxBytes {
			return fmt.Errorf("sandbox: its count's restart is %d bytes, past the %d it may be: %w",
				len(*count.Restart), RestartIDMaxBytes, ErrCaptureUnbounded)
		}
	}
	if !ValidCountProvider(c.CountProvider) {
		return fmt.Errorf("sandbox: its count's provider is %d bytes, past the %d it may be or holding a byte "+
			"a record escapes: %w", len(c.CountProvider), MaxCountProviderBytes, ErrCaptureUnbounded)
	}
	return nil
}

// CaptureMedia is one media object a capture names (the executor log's
// NewMedia): uploaded for it, or Carried, named live already by the turn's row
// or the base that row holds, which the log finds there or refuses the
// capture. ContentKey is the keyring key's id, as [SessionCapture.ContentKey]
// is: it names the key and holds none of it.
type CaptureMedia struct {
	Digest     string `json:"digest"`
	Object     string `json:"object"`
	ObjectHash string `json:"object_hash"`
	ObjectSize int64  `json:"object_size"`
	ContentKey string `json:"content_key"`
	Carried    bool   `json:"carried,omitempty"`
}

// rowMedia is the media a turn's row names as a capture names them, each
// carried where stamped names it carried: what a capture settled off the row
// is stamped with, so its seat's budget and its saves count them.
func rowMedia(named []executorkeep.MediaObject, stamped []CaptureMedia) []CaptureMedia {
	carried := map[string]bool{}
	for _, m := range stamped {
		carried[m.Digest] = m.Carried
	}
	var media []CaptureMedia
	for _, m := range named {
		media = append(media, captureMediaOf(m, carried[m.Digest]))
	}
	return media
}

// mediaBytes is what c's media objects hold sealed.
func (c SessionCapture) mediaBytes() int64 {
	var n int64
	for _, m := range c.Media {
		n += m.ObjectSize
	}
	return n
}

// NextRequest is what c's stamp says the session's next request carries
// ([SessionCapture.NextRequestTokens]).
func (c SessionCapture) NextRequest() NextRequest {
	return NextRequest{Tokens: c.NextRequestTokens, OlderTokens: c.NextRequestOlderTokens}
}

// RunStart is where the run that packed c's session began in it, as the stamp
// keeps it ([SessionCapture.RunBegan]).
func (c SessionCapture) RunStart() RunStart {
	return RunStart{Began: c.RunBegan, After: c.RunAfter, Lines: c.RunAfterLines}
}

// maxIncarnationBytes bounds a process incarnation as a run's row carries one:
// the process that owns the run's seat ([PendingRun.Owner]) and the one that
// took a capture ([SessionCapture.Taker]).
//
// ONE HUNDRED AND TWENTY-EIGHT BYTES, from the incarnation's shape,
// `<node>:<uuid>` (internal/config's NewIncarnation): a node id at its
// grammar's sixty-four bytes, the separator and a uuid's thirty-six are 101,
// and the rest is room. The engine writes both, never a box, and the row's
// budget counts each at this bound (TestTheWholeRowFitsItsBudget). The taker
// is held to it by name too: a stamp past it is refused, and a coordinator
// built with one is refused at construction ([NewCoordinator]).
const maxIncarnationBytes = 128

// placeAt places c's session in its round's calls as read — one read of the
// run's log, made before the take — holds them: its save point
// ([SessionCapture.Through]) and, off the same read, the places an older build
// reads it by ([SessionCapture.BridgeSeq], [SessionCapture.BridgeOpen]); and
// the calls at or below that point its session holds only as its job's brief's
// count, of the job's record launch ([SessionCapture.Omitted],
// [SessionCapture.LateOmitted]).
func (c *SessionCapture) placeAt(read BridgeLog, launch LaunchRecord) {
	c.Through = new(read.Appends())
	c.BridgeSeq = read.LastSeq()
	c.BridgeOpen = openPlaces(read, c.BridgeSeq)
	c.Omitted, c.LateOmitted = launch.OmittedCalls(), launch.LateOmittedCalls()
}

// SavePoint is the save point c states ([SessionCapture.Through]), and whether
// it states one: a refusal made before the log was read states none.
func (c SessionCapture) SavePoint() (int, bool) {
	if c.Through == nil {
		return 0, false
	}
	return *c.Through, true
}

// stampReason is why a capture came out as it did, as its stamp carries it:
// redacted, and held to the bound the executor log holds the same words to
// ([SessionCapture.Bounded]) by the rule every reason is ([boundReason]) — an
// error's text is the engine's and has no bound of its own.
func stampReason(why string) string { return boundReason(why, executorkeep.UnkeptReasonMaxBytes) }

// newerThan reports whether c's place is after o's.
func (c SessionCapture) newerThan(o SessionCapture) bool {
	if c.LaunchSeq != o.LaunchSeq {
		return c.LaunchSeq > o.LaunchSeq
	}
	return c.CaptureSeq > o.CaptureSeq
}

// samePlace reports whether c and o are the same capture.
func (c SessionCapture) samePlace(o SessionCapture) bool {
	return c.LaunchSeq == o.LaunchSeq && c.CaptureSeq == o.CaptureSeq
}

// sameTake reports whether c and o are stamps of ONE TAKE of a capture: its
// place, the job it was taken from and the instant the take stamped it at
// ([SessionCapture.At]), which every stamp a take makes carries — its
// reservation, its outcome, and its record's answer asked again by the turn's
// end. Two reads of one job at once — a stop's and a collection's — reserve
// the same place each at an instant of its own.
func (c SessionCapture) sameTake(o SessionCapture) bool {
	return c.samePlace(o) && c.Launch == o.Launch && c.At.Equal(o.At)
}

// holdsStamp reports whether run's row already holds capture as it is: the
// same take's stamp in the same state, in ANY of the row's capture sections:
// its newest ([PendingRun.Capture]), its save under way ([PendingRun.Saving])
// or, for a stamp that landed, its kept one ([PendingRun.Kept]). What a write
// the store applied and answered too late for its asker to hear leaves there.
// A save's outcome asked again after a final take moved the newest place on is
// held by the save's section, or by the kept one where it landed; one refused
// and let go is held by none, and answered not taken, which no asker of an
// outcome's stamp acts on ([Coordinator.stampSettled]).
func holdsStamp(run PendingRun, capture SessionCapture) bool {
	holds := func(section *SessionCapture) bool {
		return section != nil && capture.sameTake(*section) && section.State == capture.State
	}
	return holds(run.Capture) || holds(run.Saving) || (capture.State == CaptureLanded && holds(run.Kept))
}

// stampOnto stamps capture on run as [PendingStore.StampCapture] does, and
// reports whether it changed the row: the row's own rule, which an ending also
// applies to its copy ([Coordinator.publishEnd]). A capture placed after the
// newest is taken; one at the newest's place only where it is the same take
// moving on (begun to an outcome, unresolved to an answer), so no late write
// takes an answer back or settles a place another read reserved. A LANDED
// capture newer than the kept one is kept; a place held refused never is. The
// standby stays with its capture as the same take moves it on. A save under way
// keeps its own section until it can no longer be kept ([savingAfter]), decided
// on the newest capture as the stamp FOUND it, since a build without the
// section may have settled its take there; a stamp that did not take changes
// nothing of it.
func stampOnto(run *PendingRun, capture SessionCapture) bool {
	took := false
	found := run.Capture
	newest := run.Capture
	switch {
	case newest == nil || capture.newerThan(*newest):
		took = true
	case capture.sameTake(*newest):
		took = newest.State.advancesTo(capture.State) || newest.countsFewer(capture)
	}
	if took {
		stamped := capture
		if newest != nil && capture.sameTake(*newest) {
			stamped.StandbyTaker, stamped.StandbyTakerEpoch, stamped.StandbyAt =
				newest.StandbyTaker, newest.StandbyTakerEpoch, newest.StandbyAt
		}
		run.Capture = &stamped
	}
	landed := capture.State == CaptureLanded && (took || !capture.samePlace(*newest))
	if landed && (run.Kept == nil || capture.newerThan(*run.Kept)) {
		kept := capture
		kept.StandbyTaker, kept.StandbyTakerEpoch, kept.StandbyAt = "", 0, time.Time{}
		run.Kept, took = &kept, true
	}
	if save := run.Saving; save != nil && capture.sameTake(*save) &&
		(save.State.advancesTo(capture.State) || save.countsFewer(capture)) {
		moved := capture
		moved.StandbyTaker, moved.StandbyTakerEpoch, moved.StandbyAt = "", 0, time.Time{}
		run.Saving, took = &moved, true
	}
	if took {
		run.Saving = savingAfter(found, *run)
	}
	return took
}

// countsFewer reports whether c, a begun take, is stamped with fewer media
// objects than capture, the same take still begun, counts
// ([Coordinator.stampMediaCount]): the one change a begun stamp takes from its
// own take short of an outcome, which only grows.
func (c SessionCapture) countsFewer(capture SessionCapture) bool {
	return c.State == CaptureBegun && capture.State == CaptureBegun && capture.MediaCount > c.MediaCount
}

// standDownOnto withdraws standby, the read standing by for run's newest
// capture as it stood by ([SessionCapture.standbyOf]), from run as
// [PendingStore.StandDown] does, and reports whether it changed the row: only
// where that read stands by for the newest capture, so no read withdraws
// another's. The store applies it to the row as it stands, and a wait to its
// copy where the row cannot be read back after a withdrawal the store took
// ([Coordinator.standDownGone]).
func standDownOnto(run *PendingRun, standby SessionCapture) bool {
	newest := run.Capture
	if newest == nil || !newest.standsBy(standby) {
		return false
	}
	withdrawn := *newest
	withdrawn.StandbyTaker, withdrawn.StandbyTakerEpoch, withdrawn.StandbyAt = "", 0, time.Time{}
	run.Capture = &withdrawn
	return true
}

// standsBy reports whether take — a read's stamp of the place it stood by at
// ([PendingStore.StandBy]) — is the read standing by for c: the same instant
// and the same taker lease, at c's own place and of c's job.
func (c SessionCapture) standsBy(take SessionCapture) bool {
	return !c.StandbyAt.IsZero() && c.StandbyAt.Equal(take.At) && c.StandbyTaker == take.Taker &&
		c.StandbyTakerEpoch == take.TakerEpoch && c.samePlace(take) && c.Launch == take.Launch
}

// standing reports whether a read stands by for c as of now
// ([SessionCapture.StandbyAt]): c did not land, and the read stood by
// younger than [standbyBudget]. An older one's node stopped, or its keep ran
// out, before it took the next place or withdrew.
func (c SessionCapture) standing(now time.Time) bool {
	return c.State != CaptureLanded && !c.StandbyAt.IsZero() && now.Sub(c.StandbyAt) < standbyBudget
}

// keeping reports whether c is being kept as of now: BEGUN, and younger than a
// capture's budget ([SessionCaptureBudget]) from the instant its take stamped
// it. Past it, its taker's node stopped, or its keep ran out, before it stamped
// what came of it.
func (c SessionCapture) keeping(now time.Time) bool {
	return c.State == CaptureBegun && now.Sub(c.At) < SessionCaptureBudget
}

// unresolvedWithin reports whether c's record was sent and not answered, and
// may still land as of now: UNRESOLVED, and its object not past
// objstore.RecordWithin and twice objstore.AssumedClockSkew, past which no
// node's clock can read the record as one the log may still take
// ([pastRecordWithin]). An object key that does not parse is asked as any
// other is, as the ask itself asks it ([Coordinator.settleCapture]).
func (c SessionCapture) unresolvedWithin(now time.Time) bool {
	return c.State == CaptureUnresolved && !pastRecordWithin(c.Object, now)
}

// mayLand reports whether c may still become a kept capture as of now: being
// kept ([SessionCapture.keeping]), a read standing by to keep it in the next
// place ([SessionCapture.standing]), or its record sent and not yet answered
// ([SessionCapture.unresolvedWithin]). THE PREDICATE for "may still land" that
// a save's license holds off for ([PendingRun.capturesMayLand]).
func (c SessionCapture) mayLand(now time.Time) bool {
	return c.keeping(now) || c.standing(now) || c.unresolvedWithin(now)
}

// standbyOf is the read standing by for c as the stamp it stood by with: at c's
// place, of c's job, begun, at its own instant and under its own taker lease —
// what [PendingStore.StandDown] names it by, and what a wait asks of its taker
// ([Coordinator.takerGone]).
func (c SessionCapture) standbyOf() SessionCapture {
	return SessionCapture{LaunchSeq: c.LaunchSeq, CaptureSeq: c.CaptureSeq, Launch: c.Launch,
		State: CaptureBegun, At: c.StandbyAt, Taker: c.StandbyTaker, TakerEpoch: c.StandbyTakerEpoch}
}

// standbyWait is how long a read standing by waits for the capture it stands
// by for to come to something ([Coordinator.takeOver]), from the start of its
// keep: that capture's whole life ([captureLife]), which began before this
// read stood by ([PendingStore.StandBy] names a standby only on a capture
// already begun), and one more read of the run's row ([captureWaitTick]), the
// one in which it sees how that capture came out.
//
// ON ITS OWN MONOTONIC CLOCK, never against the capture's stamped instant,
// which another machine's clock wrote: past it, nothing that capture's taker
// sends lands, so a capture still begun is one whose taker stopped or whose
// stamp never reached the row, and the read takes the place after it
// ([Coordinator.waitedOut]). (6,780.08 + 2) s, 1 h 53 min 2 s today.
var standbyWait = captureLife + captureWaitTick

// standbyWindow is the longest a read standing by can still take the place
// after the capture it stands by for, from its own instant
// ([SessionCapture.StandbyAt]), on one clock: the rest of its take, its digest
// ([discardGrace]), its wait ([standbyWait]), five changes of the row
// ([captureStampBudget]: its reservation, a settle begun before the wait's
// end, the read of the turn's row past it and the settle it decides,
// [Coordinator.namedAtWaitsEnd], and its takeover) and its last read of the
// row ([captureReadBudget]), each at the slowest crossing, since the standby
// and the wait may be any two nodes. 600 + 30 + 6,782.08 + 5 × 810.03 + 270 =
// 11,732.21 s, three hours, fifteen minutes and thirty-two seconds today.
var standbyWindow = SessionTakeBudget + discardGrace + standbyWait + 5*captureStampBudget + captureReadBudget

// standbyBudget is how long after the instant a read stood by
// ([SessionCapture.StandbyAt]) any node reads it as standing
// ([SessionCapture.standing]): its window ([standbyWindow]) and twice the skew
// the engine allows any one machine's clock (objstore.AssumedClockSkew), since
// the instant is the standby's node's clock's and the reader any other node.
// Past it, a wait reads the read as gone; a lapsed taker lease withdraws it
// sooner ([Coordinator.standDownGone]). (11,732.21 + 7,200) s, FIVE HOURS,
// FIFTEEN MINUTES AND THIRTY-TWO SECONDS today. Against the window alone, a
// reader whose clock stood ahead read the standby as gone while its takeover
// could still land, and a turn's end tombstoned the turn ahead of the session
// kept. A turn's end holds a standby for its window from when it first sees it
// ([Coordinator.awaitEndCaptures]).
var standbyBudget = standbyWindow + 2*objstore.AssumedClockSkew

// kept is the capture as a box read reports it ([SessionKept]).
func (c SessionCapture) kept() *SessionKept {
	k := &SessionKept{CaptureSeq: c.CaptureSeq}
	switch c.State {
	case CaptureLanded:
		k.Bytes = c.ObjectSize + c.mediaBytes()
	case CaptureBegun, CaptureUnresolved:
		// Taken already, and on its way; how it lands is on the row.
	default:
		k.NotKept, k.Loss = c.Reason, c.Loss
	}
	return k
}

// MarshalJSON writes the capture with the keys a later build added to it.
func (c SessionCapture) MarshalJSON() ([]byte, error) {
	type plain SessionCapture
	return marshalCarrying(plain(c), wireKeys(reflect.TypeFor[SessionCapture]()), c.Extra)
}

// UnmarshalJSON reads the capture and keeps the keys this build does not know.
func (c *SessionCapture) UnmarshalJSON(raw []byte) error {
	type plain SessionCapture
	p := plain(*c)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[SessionCapture]()))
	if err != nil {
		return err
	}
	*c = SessionCapture(p)
	c.Extra = extra
	return nil
}

// sessionRecordBudget bounds one record of a capture or an end: every attempt
// at it, under its one operation, together. A stamp of a capture on its run's
// row is asked again within it ([Coordinator.askStamp]), beside the time the
// row takes to cross to the store ([RowBudget]): a stamp writes the row whole.
//
// THIRTY SECONDS. A write the log takes is answered in milliseconds; one its
// serving node has published and not yet applied within the publisher's own
// five-second wait (statelog.DefaultResolveBudget); and a write sent to a data
// node that died is asked of the next one by the router. Thirty seconds
// outlasts all of that and a broker's leader election. A log silent by then is
// an outage its own alarms name: an unresolved capture is asked again by the
// turn's end, and an end the log has not taken stays owed on the run's row.
const sessionRecordBudget = 30 * time.Second

// conversationTellBudget bounds telling a turn's conversation what its runs'
// sessions lost ([ConversationTeller]): one chat write on the turn's ending,
// which clears the end from the run's row only after it. [sessionRecordBudget]'s
// thirty seconds, for its reasons: a write the chat's log takes is answered in
// milliseconds, a published one within the publisher's five-second wait, and
// one sent to a data node that died is asked of the next by the router. Past
// it the conversation is not told, which the teller logs; the turn's page says
// it all the same.
const conversationTellBudget = sessionRecordBudget

// sessionRetryBase and sessionRetryCeiling space the attempts at one record: a
// second, doubling to eight — six attempts inside [sessionRecordBudget].
const (
	sessionRetryBase    = time.Second
	sessionRetryCeiling = 8 * time.Second
)

// boxReadPace is how long one mebibyte of a session's archive is given to come
// out of its box, at each reading of it: the BOX-READ FLOOR, a mebibyte a
// second, which the scrub's wait ([SessionScrubWait]) and a relaunch's wait for
// a capture ([captureRestFor]) are sized on. A floor, never a measure: a box's
// stream is its host's exec channel or its runtime's copy, which a working host
// reads far faster.
const boxReadPace = time.Second

// archiveReadFor is how long n bytes of archive are given to come out of a box
// once ([boxReadPace]): one pace for every mebibyte begun, as objstore.PaceFor
// counts an upload's.
func archiveReadFor(n int64) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration((n+objstore.MiB-1)/objstore.MiB) * boxReadPace
}

// scrubArchiveReads is the scrub's size term: an archive at the kept bound
// (executorkeep.SessionKeptMaxBytes) read twice out of its box at
// [boxReadPace], [archiveReadFor] of it twice, written out so
// [SessionScrubWait] stays a constant: 188 s today. Every take is given it, at
// either bound ([takeBound]): a reader cannot know which a take holds.
const scrubArchiveReads = 2 * ((executorkeep.SessionKeptMaxBytes + objstore.MiB - 1) / objstore.MiB) * boxReadPace

// scrubPlainSlowest is the scrub's plaintext term: the slowest session a
// harness writes at the 256 MiB of plaintext a reading reads
// (internal/sandbox/executor's scrubPlainMax), a string torn at its last line,
// 183 s: its 177.4 s on the base times the HIGHEST ratio of the alternated
// runs, 1.03 at the default against 1.00 under the memory limit and at GOGC=10
// (BenchmarkTheScrubAtThePlaintextBound). Not a size's: a small archive may
// inflate to the bound.
const scrubPlainSlowest = 183 * time.Second

// SessionScrubWait bounds the scrub of a session as it streams out of its box,
// both readings of its tar: the minute a take may wait for its spool's room
// ([spoolRoomWait]), the archive at its bound read twice
// ([scrubArchiveReads]) and the slowest plaintext ([scrubPlainSlowest]), 431 s
// apart, ROUNDED UP TO THE MINUTE: eight minutes today. A take still scrubbing
// at its end FAILS CLOSED: nothing is kept, its place is refused and the last
// kept copy stands, so a box costs only its own session (ADR-0073;
// docs/concepts/code-sandbox.md, "Eight minutes for the scrub").
const SessionScrubWait = (spoolRoomWait + scrubArchiveReads + scrubPlainSlowest + time.Minute - 1) /
	time.Minute * time.Minute

// SessionTakeBudget is the most a take adds to the read of a box it is made in:
// the pack ([SessionPackWait]) and the scrub ([SessionScrubWait]).
//
// SIZED ON THE BOUNDS, NOT ON THE ARCHIVE, because a node that asks another to
// read a box ([BoxReadBudget]) cannot know how large the session in it is
// before the read: a session of an ordinary day's work is a mebibyte or two and
// takes seconds, and the bound is what a host that is still working is given
// before the asker takes it for one that is not.
const SessionTakeBudget = SessionPackWait + SessionScrubWait

// keepBudgetFor is what a keep of a sealed session of n bytes needs on its
// longest path ([keepBudgetAt] of its pace).
func keepBudgetFor(n int64) time.Duration {
	return keepBudgetAt((n + objstore.MiB - 1) / objstore.MiB)
}

// keepBudgetAt is what a keep whose objects upload in paceMiB of the store's
// pace units (objstore.PaceFor) needs on its longest path: its four steps
// before its upload, each in a room of its own ([keepReadRoom]: its media
// count, the mint, the carry's listing, a save's amend), the upload at the
// slowest pace, the read again of who reads its media ([MediaReadersRoom]) and
// its record ([sessionRecordBudget]); and where the log finds a carry gone,
// the amend, the read again and the record once more. A keep that makes fewer
// is given every room all the same ([boxReadChanges]).
//
// THE ROOMS BESIDE THE UPLOAD'S PACE, never inside it: inside it, a keep whose
// reads answered slowly left a session at its bound less than its pace.
func keepBudgetAt(paceMiB int64) time.Duration {
	return 5*keepReadRoom + time.Duration(paceMiB)*objstore.MiBPace + 2*(MediaReadersRoom+sessionRecordBudget)
}

// takeBound is a take's bound ([SessionPolicy.KeptBound]) and what follows
// from it: the archive it admits of its box (SessionTake.MaxBytes), the most
// its objects hold sealed, the pace units they upload in (the take's media
// are paced to it, SessionTake.KeepPaceMiB) and its keep's budget, which that
// pace sets.
type takeBound struct {
	archive, sealed, paceMiB int64
	keep                     time.Duration
}

// textBound is a take's bound while a node that may settle a run's row reads
// no stamp at the kept bound: its archive within executorkeep.SessionMaxBytes
// and one object's pace for it all, media included (k members cost at most k+1
// paced mebibytes more than the archive whole, which the take's pacing holds).
// 29 minutes of keep, within which a build at [MediaStampVersion] reads the
// capture as kept.
var textBound = takeBound{archive: executorkeep.SessionMaxBytes, sealed: executorkeep.SealedMaxBytes,
	paceMiB: (executorkeep.SealedMaxBytes + objstore.MiB - 1) / objstore.MiB,
	keep:    keepBudgetFor(executorkeep.SealedMaxBytes)}

// keptBound is a take's bound once every such node reads it: what one capture
// keeps in all, executorkeep.SessionKeptMaxBytes, its text still within
// executorkeep.SessionMaxBytes, at executorkeep.KeptPaceMiB, every object's
// mebibyte begun counted. 68 minutes of keep.
var keptBound = takeBound{archive: executorkeep.SessionKeptMaxBytes, sealed: executorkeep.KeptSealedMaxBytes,
	paceMiB: executorkeep.KeptPaceMiB, keep: keepBudgetAt(executorkeep.KeptPaceMiB)}

// boundOf is the bound a take at kept holds.
func boundOf(kept bool) takeBound {
	if kept {
		return keptBound
	}
	return textBound
}

// SessionKeepBudget bounds every keep, off the read, whatever its session's
// size or its take's bound: the kept bound's ([keptBound]), within which an
// upload the store did not take is made again ([Coordinator.uploadSpool]). A
// keep is held to its own take's ([takeBound]). Sixty-eight minutes today.
var SessionKeepBudget = keptBound.keep

// captureRestFor is the most a capture of an archive of n bytes, its spool
// cutting media objects apart, taken at the kept bound or not, may still take
// once its place is reserved, each step at its floor: its scrub's two
// readings ([archiveReadFor]) and slowest plaintext ([scrubPlainSlowest]), the
// digest it leaves in its box ([discardGrace]), and a keep of the sealed
// session the archive may become ([countedFor], [keepBudgetFor]), each object
// uploaded beside it paced by the mebibyte begun, so one pace more
// ([SessionCapture.MediaCount]). What a relaunch waits for a capture it
// depends on ([Coordinator.relaunchWaitOf]): 8 min 35 s for an archive of
// half a mebibyte, 34 min 9 s at the text bound, 58 min 41 s at the kept
// bound, and 30 s more for each object.
func captureRestFor(n int64, media int, kept bool) time.Duration {
	return 2*archiveReadFor(n) + scrubPlainSlowest + discardGrace +
		keepBudgetFor(countedFor(n, boundOf(kept).sealed)) + time.Duration(media)*objstore.MiBPace
}

// SessionCaptureBudget is how long after its stamped instant
// ([SessionCapture.At]) any node reads a begun capture as being kept
// ([SessionCapture.keeping]): its life on one clock ([captureLife]) and twice
// the skew the engine allows any one machine's clock
// (objstore.AssumedClockSkew), since the instant is its taker's clock's and the
// reader any other node. A capture begun longer ago than this is one whose
// taker stopped, or whose stamp never reached the row. (6,780.08 + 7,200) s,
// THREE HOURS AND FIFTY-THREE MINUTES today. Counting less, or reading another
// machine's stamp as one's own, let a capture still being kept be read as
// abandoned: a standby took its place over and uploaded again, and a turn's
// end recorded over the session. No wait holds that long on a reader's word
// alone ([standbyWait], [Coordinator.awaitEndCaptures]).
var SessionCaptureBudget = captureLife + 2*objstore.AssumedClockSkew

// captureStampBudget is the most one change of a run's row that another node
// reads takes, as that node counts it: every attempt at it, within one record's
// budget ([sessionRecordBudget]) and the time a change of the run's row takes
// to cross to the store, its read and its write ([Coordinator.askStamp],
// [Coordinator.rowContext], [RowBudget]), counted at the slowest node's link
// (coord.MaxValueCrossing), since the change is made by whichever node takes
// or keeps the capture, or stands by for it, and read by any. A reservation of
// a capture's place, its stamp of what came of it, a takeover and a standby's
// withdrawal are each one; a read of the row is not ([captureReadBudget]).
// (30 + 780.03) s, thirteen and a half minutes at today's bounds.
var captureStampBudget = sessionRecordBudget + coord.MaxValueCrossing

// captureReadBudget is the most one READ of a run's row takes, as a node that
// waits on it counts it: the room the read is held to
// ([Coordinator.rowReadContext]), a record's reason and the reply's crossing,
// at the slowest node's link (coord.MaxValueReadCrossing), (30 + 240) s. A
// take's read of the row it places its capture by ([Coordinator.placingRow],
// counted in [BoxReadBudget]), a takeover's read back ([takeOverLife]), a
// standby's last read ([standbyWindow]) and a turn's end's
// ([Coordinator.awaitEndCaptures]) are each one. NEVER A CHANGE'S
// ([captureStampBudget]): a read sends no write, and a change's room gave each
// 540 s of sends it never makes.
var captureReadBudget = sessionRecordBudget + coord.MaxValueReadCrossing

// keepReadRoom is the room each of a keep's four steps before its upload is
// held to ([Coordinator.keepCapture]): its media count stamped
// ([Coordinator.stampMediaCount]), the turn's operations minted, the media its
// turn holds read with the seat's live runs listed for its budget
// ([Coordinator.overBudget]), and a save's charge amended (whose
// [saveLedgerWait] is this room). A record's budget ([sessionRecordBudget]):
// a store that has not answered a small read by then is in an outage its own
// alarms name. A mint past it refuses the capture; a count or a listing past
// it decides nothing, and the log decides again at the record. The listing
// reads every run of the fleet (crewlet/crewlet#1369), so on a large fleet
// across a slow link it can pass its room, and the seat's budget is then
// decided only at the record.
const keepReadRoom = sessionRecordBudget

// MediaReadersRoom is the room the read of the policy and who reads a capture
// naming media is held to ([SessionPolicy.MediaApart]): the activation pointer,
// four listings of small values (the live nodes' presences, the position
// register's rows, the nodes' capture stamp records and the retired node ids)
// and the executor log's evictions, this node's own rows. MEASURED, 24.7 s for
// a fleet at its widest (coord.MaxMaintenanceParticipants nodes, each record at
// its widest, each reply at the floor pace, a read term a read; internal/engine's
// TestTheMediaGatesReadsFitItsRoomAtTheFloor), so a record's budget
// ([sessionRecordBudget]) holds them. Past it the take keeps its media inline,
// logged with why. Read before the take's budget begins
// ([Coordinator.placingPolicy]), so [BoxReadBudget] counts it beside the take.
const MediaReadersRoom = sessionRecordBudget

// takeLife is the longest a take's begun stamp stands before its taker's last
// attempt at the stamp of what came of it ends, from the stamp's instant
// ([SessionCapture.At], stamped once the session is packed), at the slowest
// crossing: the rest of the take with its reservation's room
// ([SessionTakeBudget], [captureStampBudget], [Coordinator.takeContext]), the
// digest it leaves in the box ([discardGrace], [stampContext]), the keep, which
// starts as the take returns its spool ([SessionKeepBudget]), and the stamp of
// what came of it ([captureStampBudget]). An hour, forty-five and a half
// minutes and a fraction at today's bounds.
var takeLife = SessionTakeBudget + captureStampBudget + discardGrace + SessionKeepBudget + captureStampBudget

// takeOverLife is the same of a takeover's begun stamp, from its own instant
// ([Coordinator.takeOver]): the takeover ([PendingStore.TakeOver]), and where
// the store did not answer it the standby's withdrawal and its read of the row
// back ([Coordinator.tookOver]); its keep ([SessionKeepBudget]); and the stamp
// of what came of it. Each change of the row ([captureStampBudget]), the read
// back a read ([captureReadBudget]): 3 × 810.03 + 270 + 4,080 = 6,780.08 s,
// an hour, fifty-three minutes and a fraction at today's bounds.
var takeOverLife = 3*captureStampBudget + captureReadBudget + SessionKeepBudget

// captureLife is the longest life of a begun capture, a take's or a
// takeover's, from its instant to the end of its taker's last attempt at the
// stamp of what came of it, on one clock: past it, nothing its taker sends
// lands, so a capture still begun is one whose taker stopped or whose stamp
// never reached the row. What a standby's wait and a turn's end wait for, each
// on its own clock ([standbyWait], [Coordinator.awaitEndCaptures]); a reader
// of a stamp's age adds the clocks' skew ([SessionCaptureBudget]).
//
// EVERY STEP COUNTED, each at its bound at the slowest crossing: without the
// reservation's room, a keep's reads and a takeover's steps, a rule reading a
// begun stamp's age against it could read a capture still being kept as
// abandoned. max(6,330.05, 6,780.08) s, 1 h 53 min today, a takeover's.
var captureLife = max(takeLife, takeOverLife)

// sessionKeepDrain is how long a stopping coordinator lets keeps already under
// way finish before it cancels them ([Coordinator.Stop]): the teardown grace
// every box call is given ([discardGrace]). A keep cut there is stamped refused
// saying so, and the turn's next launch starts fresh.
const sessionKeepDrain = discardGrace

// captureWaitTick spaces the reads of a run's row an ending, or a relaunch,
// makes while a capture of its turn is still being kept
// ([Coordinator.awaitCapture]).
const captureWaitTick = 2 * time.Second

// heldRowPace spaces the reads of the turn's row on the executor log that a
// wait makes for a capture whose taker still holds its lease, or that the
// store cannot say of ([Coordinator.settleGone]'s readHeld): linearizable
// reads, made to settle LANDED a record the log took that its taker never
// stamped. ONE RECORD'S BUDGET, since every attempt at a record ends within
// it, so a sooner read settles at most that much sooner while costing a
// linearizable read at every tick of the upload: an end's wait for one
// capture's whole life reads the log at most about a hundred and forty-three
// times, where the wait's tick ([captureWaitTick]) would make it fifteen times
// as many.
const heldRowPace = sessionRecordBudget

// beganUnderTaker reports whether run's turn has a begun stamp in sight that
// names its taker: its newest capture, or its save under way
// ([PendingRun.savingTake]). A wait reads the turn's row for such a stamp
// whatever its taker's lease ([heldRowPace]); a stamp that names none, which
// only a coordinator built with no taker lease writes, is waited for by its
// age alone.
func beganUnderTaker(run PendingRun) bool {
	for _, stamp := range []*SessionCapture{run.Capture, run.savingTake()} {
		if stamp != nil && stamp.State == CaptureBegun && stamp.Taker != "" {
			return true
		}
	}
	return false
}

// capturePoint is where in a run's life a capture is taken.
type capturePoint struct {
	// name says which, for the log.
	name string
	// pack says the session is packed now rather than taken as the run's
	// script left it ([SessionTake.Pack]).
	pack bool
	// final says the job had ended ([SessionCapture.Final]).
	final bool
	// cutOff says the session's harness did not exit cleanly
	// ([SessionCapture.Interrupted]).
	cutOff bool
	// exhausted says the job ran out of its model's context ([ranOutOfContext]),
	// so its capture is refused untaken.
	exhausted bool
	// refused says the job's provider refused the request its session makes
	// ([requestRefused]), so its capture is refused untaken too.
	refused bool
	// window is the least context window the job's run reported
	// ([Result.ReportedWindow]), stamped beside the capture's count
	// ([SessionCapture.ResumedCount]); 0 where it reported none, or nothing
	// was collected, as a loss's read collects nothing.
	window int64
	// summary is the newest summary the read found, which the take keeps
	// ([readSummary]); nil for none.
	summary *readSummary
}

// readCapture is the capture a box read takes of a job that is OVER — by
// itself, or ended by the read ([Coordinator.endJob]), a stop's, a loss's or a
// collection's past its turn's cap — or still working: over, the archive its
// script packed where it left one and one packed now where it left none
// ([SessionTake.Pack]), FINAL; working (one a box would not end), one packed
// now as it stands, not final. CUT OFF wherever the engine packs it — result
// names no archive the script left, which is every loss's read, collecting
// nothing — where the script packed it after a harness that did not exit
// cleanly ([Result.HarnessCutOff]): a harness killed while its wrapper lived on
// is packed by the script all the same, its transcript ending wherever the kill
// found it; and where its job was ended on request ([Result.JobEnd]), a harness
// told to stop mid-step whatever its exit.
func readCapture(read BoxRead, result Result, over bool) capturePoint {
	return capturePoint{name: string(read), pack: !over, final: over,
		cutOff: !over || result.Captured == nil || result.HarnessCutOff || result.JobEnd != "",
		window: result.ReportedWindow}
}

// exhaustedNotKept is why a job that ran out of its model's context left no
// capture: what its launch's record and the log say.
const exhaustedNotKept = "the job ran out of its model's context, so no launch continues its session and it " +
	"is not kept"

// refusedNotKept is why a job whose provider refused its request left no
// capture: what its launch's record and the log say.
const refusedNotKept = "the provider refused the request the job's session makes, so no launch continues " +
	"its session and it is not kept"

// requestRefused reports whether a job FAILED because its provider refused the
// request its harness made as built ([Result.RequestRefused]): what the session
// sends again on every launch, so it is retired as one out of context is. A
// job stopped, lost or parked on a question did not fail, and one out of
// context is named for that.
func requestRefused(run PendingRun, result Result) bool {
	return result.RequestRefused && !result.ContextExhausted && !result.Success &&
		run.Stop == nil && run.Lost == nil && !run.asked()
}

// sessionSpent is how a job failed in a way every later launch of its session
// meets again, "" where it did not: out of context, or refused by its provider.
func sessionSpent(run PendingRun, result Result) executorkeep.Loss {
	switch {
	case ranOutOfContext(run, result):
		return executorkeep.LossContextExhausted
	case requestRefused(run, result):
		return executorkeep.LossRequestRefused
	}
	return ""
}

// ranOutOfContext reports whether a job FAILED because its conversation no longer
// fits its model's context, as its launch's record says ([collectedRecord]): a job
// stopped, lost or parked on a question did not fail, whatever its harness said.
func ranOutOfContext(run PendingRun, result Result) bool {
	return result.ContextExhausted && !result.Success && run.Stop == nil && run.Lost == nil && !run.asked()
}

// asCollected is run and its result as its collection records them
// ([Coordinator.collectClaimed]), for a box read that decides something on them
// before the collection does ([Coordinator.readBox]): a run its turn's cap
// ended is parked on no question ([withoutQuestion]) and is no success
// ([endedAtDeadline]), whatever its box said. Judged on the row as claimed, a
// capped run that asked on its way out and ran out of context had its session
// taken and kept by the read, and retired by the collection right after.
func asCollected(run PendingRun, result Result, ended bool) (PendingRun, Result) {
	if ended {
		run, result.Success = withoutQuestion(run), false
	}
	return run, result
}

// sessionsRanOutOfContext are the sessions a job whose session is spent
// ([sessionSpent]) may have run, in the row's word
// ([PendingRun.SessionRan]): both of its launch's where the box never said which, and
// the box's own only where the row holds none, for a harness that mints its id.
func sessionsRanOutOfContext(run PendingRun, result Result) []string {
	candidates := []string{run.SessionRan()}
	if candidates[0] == "" {
		candidates = []string{run.SessionID, run.Launch.FreshSessionID, result.SessionID}
	}
	sessions := make([]string, 0, len(candidates))
	for _, session := range candidates {
		if ValidSessionID(session) && !slices.Contains(sessions, session) {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

// withSpent is held with sessions added, spent by loss, no id twice (one held
// keeps how it was first spent), held to [executorkeep.SpentMaxSessions]. Past
// it the first stays, the one earlier rows of the conversation name, and the
// next oldest goes.
func withSpent(held []executorkeep.SpentSession, loss executorkeep.Loss,
	sessions []string) []executorkeep.SpentSession {

	out := slices.Clone(held)
	for _, session := range sessions {
		if !slices.ContainsFunc(out, func(s executorkeep.SpentSession) bool { return s.Session == session }) {
			out = append(out, executorkeep.SpentSession{Session: session, Loss: loss})
		}
	}
	for len(out) > executorkeep.SpentMaxSessions {
		out = slices.Delete(out, 1, 2)
	}
	return out
}

// maxOpenPlaces bounds [SessionCapture.BridgeOpen]: the most places below a
// capture's highest call it names, the highest of them — the save point an
// older build reads, which this one writes for it and does not read.
//
// SIXTY-FOUR, from what can be open at once: a call is in flight from its
// arrival to its tool's answer, and a harness has at most a handful to a few
// dozen in flight together. But the places the row's bound dropped from the
// middle of a long round ([MaxBridgeCalls]) are the HIGHEST it leaves
// unlisted, so in a long enough round they fill every slot and a call in
// flight below them is named in none, which that build reads as answered.
// This build reads a count instead ([SessionCapture.Through]); the bound
// stays for the row's arithmetic and that build's reading.
const maxOpenPlaces = 64

// openPlaces is the places at or below last that the run's log holds no call
// at in its round ([BridgeLog.Calls]) — calls in flight when its job's session
// was packed, places the bound dropped, and places that never held one — the
// highest [maxOpenPlaces] of them, ascending ([SessionCapture.BridgeOpen]),
// for an older build to read.
func openPlaces(calls BridgeLog, last int) []int {
	held := make(map[int]bool, len(calls.Calls))
	for _, call := range calls.Calls {
		held[call.Seq] = true
	}
	var open []int
	for seq := last; seq > 0 && len(open) < maxOpenPlaces; seq-- {
		if !held[seq] {
			open = append(open, seq)
		}
	}
	slices.Reverse(open)
	return open
}

// notesHeld is the last note read ([RunSteer.Reads]) run's record says a
// session packed from its job holds ([SessionCapture.NoteCursor]): every read
// where its harness ended its own run, up to the first read marked unanswered
// ([NoteRead.Unanswered]) or restarted ([NoteRead.Restarted]), or the place of
// a note or wake item a hook was served and nothing read ([RunNote.Place]);
// none where it was cut off. A continuation tells every note and wake past it
// as ones the session may already show (internal/agent/runner's keptSession).
// A late call's item stops nothing; a telling of its call is read against the
// cursor as stamped, at the place of the write that recorded it
// ([CallTelling.Place]). See the package doc, "A turn's notes".
func notesHeld(run PendingRun, cutOff bool) int {
	if cutOff {
		return 0
	}
	held := run.Steer.Reads
	for _, n := range run.Steer.Notes {
		stop := 0
		switch {
		case n.For != "" && !n.IsWake():
		case n.Read == nil:
			stop = n.Place
		case n.Read.Unanswered || n.Read.Restarted:
			stop = n.Read.Order
		}
		if stop > 0 && stop <= held {
			held = stop - 1
		}
	}
	return held
}

// spooledCapture is a capture taken out of its box and spooled, sealed, on this
// node's disk: its upload and its record are what is left ([Coordinator.keepCapture]).
type spooledCapture struct {
	run   PendingRun
	stamp SessionCapture
	spool executorkeep.SealedSpool
	file  string
	// media is each payload the take kept apart, sealed in a file of its own
	// in the order its frames name them.
	media []spooledMedia
	// hold is the room the spools hold in its process's ([spoolHold]), given
	// back with their files ([spooledCapture.remove]).
	hold *spoolHold

	// standby says the take holds no place of its own: it stands by for
	// another read's capture at stamp's place ([Coordinator.standBy]), and its
	// keep first waits to see whether to take the next one
	// ([Coordinator.takeOver]).
	standby bool

	// places is what the capture is raised by once its take is counted
	// ([SessionCapture.placeAtNewest]): set for a capture no read places, nil
	// for one its read placed.
	places *savePlaces

	// summary is the newest summary the take found, scrubbed of its values,
	// which its keep keeps beside it ([Coordinator.keepSummaryBeside]).
	summary *readSummary

	// charge is a save's entry on its seat's save ledger, which its keep
	// amends to what it uploads ([Coordinator.amendSave]); nil for a take.
	charge *saveCharge

	// begun is the stamp its place was reserved with, counting the media its
	// spool cut apart, which its keep stamps first ([Coordinator.stampMediaCount]).
	begun SessionCapture
}

// spooledMedia is one payload's member, sealed in file; carried is the object
// its keep carries in its stead, and uploaded the object its keep put it into.
// Its spool stays until the record lands ([Coordinator.keepCapture]).
type spooledMedia struct {
	spool    executorkeep.MediaSpool
	file     string
	carried  *executorkeep.MediaObject
	uploaded *executorkeep.MediaObject
}

// carry has s's keep carry each of its media held names, by digest, rather
// than upload it again. Its spool stays: a record the log refuses for a carry
// gone meanwhile uploads it after all ([spooledCapture.uncarry]).
func (s *spooledCapture) carry(held map[string]executorkeep.MediaObject) {
	for i := range s.media {
		if object, named := held[s.media[i].spool.Digest]; named {
			s.media[i].carried = &object
		}
	}
}

// uncarry has s's keep upload every media it carried, reporting whether it
// carried any: the log found a carried object no longer named.
func (s *spooledCapture) uncarry() bool {
	any := false
	for i := range s.media {
		any = any || s.media[i].carried != nil
		s.media[i].carried = nil
	}
	return any
}

// paceLeft is the store's slowest pace for the session, where from is 0, and
// each media from the from-th on it still uploads ([objstore.PaceFor]): an
// object begun costs a whole pace unit.
func (s *spooledCapture) paceLeft(from int) time.Duration {
	var pace time.Duration
	if from == 0 {
		pace, from = objstore.PaceFor(s.spool.Size), 1
	}
	for _, m := range s.media[from-1:] {
		if m.carried == nil && m.uploaded == nil {
			pace += objstore.PaceFor(m.spool.Size)
		}
	}
	return pace
}

// remove deletes the spools' files and gives back the room they held.
func (s *spooledCapture) remove() {
	_ = os.Remove(s.file)
	removeMedia(s.media)
	s.hold.release()
}

// removeMedia deletes the media spools' files.
func removeMedia(media []spooledMedia) {
	for _, m := range media {
		_ = os.Remove(m.file)
	}
}

// bytes is what the capture's spools hold sealed: its session and its media.
func (s *spooledCapture) bytes() int64 {
	n := s.spool.Size
	for _, m := range s.media {
		n += m.spool.Size
	}
	return n
}

// uploads is what the capture's keep uploads: its session and the media it
// does not carry, which a save is charged ([Coordinator.amendSave]).
func (s *spooledCapture) uploads() int64 {
	n := s.spool.Size
	for _, m := range s.media {
		if m.carried == nil {
			n += m.spool.Size
		}
	}
	return n
}

// spoolPrefix names a session's spool in its process's own spool directory
// ([Coordinator.spoolHere]).
const spoolPrefix = "session-"

// takeCapture takes the session of run's job out of box (the package doc's "Two
// halves") and stamps where it stands on the run's row, reporting what came of
// it for the read's result, and the spool its keep uploads (nil if none). A
// READ THAT FINDS ITS PLACE ANOTHER READ'S STANDS BY, NEVER DOWN: a stop's
// and a collection's read of one job may take its session at once; the one
// whose reservation is refused takes it all the same and stands by
// ([Coordinator.standBy]), keeping its own in the next place where that
// capture does not land ([Coordinator.takeOver]); walking away keeps nothing
// though it held the session whole. BEST EFFORT AND BOUNDED
// ([Coordinator.takeContext]): every failure is stamped and logged, the read
// goes on, and an unanswered reservation is stamped refused
// ([Coordinator.abandonPlace]).
func (c *Coordinator) takeCapture(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	at capturePoint) (*spooledCapture, *SessionKept) {

	if !c.keepsSessions() {
		return nil, nil
	}
	taker, ok := runner.(SessionTaker)
	if !ok {
		return nil, nil
	}
	c.sweepSpools()
	by := c.recordsBy(ctx)
	// THE ROW AS IT STANDS, for the ordinals, what the job was handed and
	// what it had been told: the reader's copy may be the claim's, from
	// before the job's last calls. ASKED AGAIN in a room of its own before
	// the take's budget begins ([Coordinator.placingRow]); a store that never
	// answers takes no refusal either, so the loss rides the read's answer
	// alone (`executor_collected`'s session_not_kept_loss).
	current, found, err := c.placingRow(ctx, by, run.TurnID)
	switch {
	case err != nil:
		why := fmt.Sprintf("the run's record could not be read to place the capture: %v", err)
		sayNotKept(ctx, slog.LevelWarn, executorkeep.LossKeepFailed, run.TurnID, "point", at.name,
			"detail", why)
		return nil, &SessionKept{NotKept: why, Loss: executorkeep.LossKeepFailed}
	case !found || current.LaunchID != run.LaunchID:
		// GONE, OR ANOTHER JOB'S NOW: the read was of a job the row no
		// longer names, and its box is that job's.
		return nil, nil
	}
	standing := false
	if last := current.Capture; last != nil && last.LaunchSeq == current.LaunchSeq &&
		last.Launch == run.LaunchID && last.Final {
		if last.State != CaptureBegun {
			// THE JOB WAS TAKEN ONCE IT HAD ENDED — its collection retried
			// after a resume that failed, or a stop's read made again — and
			// nothing has changed its session since, whatever that take came
			// to: taken again, it would be another upload of the same bytes,
			// holding the store's room for a day, or the same refusal.
			return nil, last.kept()
		}
		// BEING TAKEN BY ANOTHER READ NOW, or kept after it — a stop's beside
		// a collection's — whose capture has yet to come to anything: this
		// read takes the session too, and stands by for that capture at its
		// place rather than walk away from it.
		standing = true
	}
	// THE FLEET'S WORD ON KEEPING, read before the take's budget begins
	// ([Coordinator.placingPolicy]).
	policy, policyErr := c.placingPolicy(ctx, by)
	// THE READ'S OWN CONTEXT keeps a refused take's summary past the take's
	// time, which ends a summary's upload early where the read found one
	// ([Coordinator.summaryBy]).
	read := ctx
	ctx, cancel, _ := c.takeContext(ctx)
	if at.summary != nil {
		ctx, cancel = before(ctx, cancel, c.summaryBy(by))
	}
	defer cancel()
	// THE VALUES ITS CAPTURE IS SCRUBBED OF, read once for the take and the
	// summary it keeps ([readSummary]); the policy, read once above, is what
	// lets a refusal keep the summary at all.
	var scrubbed *struct {
		values []string
		err    error
	}
	scrubOf := func(ctx context.Context) ([]string, error) {
		if scrubbed == nil {
			values, readErr := c.scrubValues(ctx, current, box)
			scrubbed = &struct {
				values []string
				err    error
			}{values, readErr}
			if rs := at.summary; rs != nil && readErr == nil {
				rs.scrub, rs.scrubbed = values, true
			}
		}
		return scrubbed.values, scrubbed.err
	}
	keeps := false
	// A REFUSED TAKE KEEPS ITS SUMMARY BEFORE ITS STAMP, which names it
	// ([Coordinator.keepSummaryRefused]).
	keepRefused := func() *SummaryStamp {
		if !keeps || at.summary == nil {
			return nil
		}
		return c.keepSummaryRefused(read, by, at.summary, func(ctx context.Context) PendingRun {
			_, _ = scrubOf(ctx)
			return current
		})
	}
	// A REFUSAL BESIDE ANOTHER READ'S CAPTURE IS NO CAPTURE: a read standing
	// by holds no place, so what refuses its take is said, and that read's
	// capture stands as it is, never stamped over by a place this read never
	// held.
	refuse := func(calls *BridgeLog, loss executorkeep.Loss, why string, attrs ...any) *SessionKept {
		if standing {
			sayNotKept(ctx, slog.LevelInfo, loss, run.TurnID, append([]any{"point", at.name,
				"capture_seq", current.Capture.CaptureSeq, "detail", "another read of the run holds the " +
					"capture's place, and this one took nothing to stand by for it with: " + stampReason(why)},
				attrs...)...)
			return current.Capture.kept()
		}
		return c.refuseUnplaced(ctx, by, run, current, at, calls, keepRefused(), loss, why, attrs...)
	}
	// A MINTING JOB'S SESSION IS BOUND BEFORE IT IS TAKEN (minted.go).
	bound, err := c.bindMinted(ctx, current, box, runner)
	if err != nil {
		return nil, refuse(nil, executorkeep.LossKeepFailed, fmt.Sprintf("the job's session was not bound: %v", err))
	}
	current = bound
	if policyErr != nil {
		return nil, refuse(nil, executorkeep.LossKeepFailed, fmt.Sprintf("the company's session policy could "+
			"not be read off the revision the fleet runs: %v", policyErr))
	}
	if !policy.Keep {
		return nil, nil
	}
	keeps = true
	if at.exhausted || at.refused {
		// REFUSED UNTAKEN: no launch would continue it, and its upload could release
		// another conversation's session against the seat's budget. Stamped, so the
		// end names it ([unkeptOf]) and the row stops naming anything older.
		loss, why := executorkeep.LossContextExhausted, exhaustedNotKept
		if at.refused {
			loss, why = executorkeep.LossRequestRefused, refusedNotKept
		}
		return nil, refuse(nil, loss, why)
	}
	// A TAKER LEASE THE CAPTURE CAN BE TAKEN UNDER, asked before the take: a
	// begun stamp is waited on for as long as the lease it names holds, so
	// one this process could not name — its claim or its last renewal
	// unconfirmed, a store it cannot reach — is not begun at all, and the
	// box is spared a pack nothing would keep.
	if _, held := c.heldTaker(); !held {
		return nil, refuse(nil, executorkeep.LossKeepFailed, takerUnheld)
	}
	// THE SESSION'S PLACE IN THE RUN'S LOG, read BEFORE THE TAKE, and only
	// where its harness ended its own run, the one capture a read places
	// ([SessionCapture.Through]): every other capture is placed off the row
	// below and reads no log (savepoint.go). Such a job is over, its archive
	// packed once its harness exited, so this read comes after that pack and
	// every call it counts was written before; a call the log takes after it is
	// numbered past it, and told. A log the store could not read REFUSES the
	// capture: a session whose place cannot be stated is one a continuing
	// launch could not tell what came after. Read as the job stands to hold it
	// ([PendingStore.TurnLog]), so a log a relaunch has not yet opened places
	// the session where that opening will number from, never at 0.
	var calls *BridgeLog
	if !at.cutOff {
		read, readErr := c.pending.TurnLog(ctx, current)
		if readErr != nil {
			return nil, refuse(nil, executorkeep.LossKeepFailed,
				fmt.Sprintf("the run's bridged log could not be read: %v", readErr))
		}
		calls = &read
	}
	scrub, err := scrubOf(ctx)
	if err != nil {
		return nil, refuse(calls, executorkeep.LossKeepFailed, err.Error())
	}
	// ROOM FOR ITS SPOOLS AT THE BOUND, before anything the take holds, in a
	// save's order: room, the pack slot, then the take's own slots, so no take
	// waits for room holding what a save holding room waits for. Its scrub's
	// spool and its sealed spools share it, the scrub's drained into the seal,
	// and it shrinks once the archive's size and the scrub's spool are known
	// ([takeRoomBytes], [spoolHold.settleLocked]). None within the wait keeps
	// nothing, and is stamped refused as any other refusal is ([spoolRoomWait]).
	room, refused := c.awaitSpoolRoom(ctx, takeRoomBytes)
	if refused != nil {
		return nil, refuse(calls, executorkeep.LossNoSpoolRoom, refused.reason(), refused.logAttrs()...)
	}
	spooledHold := false
	defer func() {
		if !spooledHold {
			room.release()
		}
	}()
	dir, err := c.spoolHere()
	if err != nil {
		loss, why := takeRefusal(ctx, err)
		return nil, refuse(calls, loss, why)
	}
	// ONE PACK AT A TIME in a box that shares the node's memory
	// ([Coordinator.sharedPack]): a take packs on demand, and its pack is
	// made once TakeSession returns, before the stream the spool reads.
	give, err := c.packSlot(ctx, current)
	if err != nil {
		loss, why := takeRefusal(ctx, err)
		return nil, refuse(calls, loss, why)
	}
	limit := boundOf(policy.kept())
	taken, err := taker.TakeSession(ctx, box, SessionTake{Pack: at.pack, Scrub: scrub,
		MaxBytes: limit.archive, Scope: current.AgentID, Session: current.Launch.Minted,
		Spool: room.scrubbing(dir), MediaApart: policy.MediaApart, KeepPaceMiB: limit.paceMiB})
	give()
	if err != nil {
		loss, why := takeRefusal(ctx, err)
		return nil, refuse(calls, loss, why)
	}
	if taken.Body == nil {
		return nil, nil
	}
	defer func() { _ = taken.Body.Close() }()
	room.counting(countedFor(taken.Archive.Size, limit.sealed))
	// AND THE TENURE THE STAMP NAMES, read again once the session is in hand,
	// right before its place is reserved: the pack and the scrub can take
	// minutes ([SessionTakeBudget]), and a lease that lapsed and was claimed
	// again meanwhile is held at a new epoch. Stamped with the tenure read
	// before the pack, the capture named one every peer reads as gone at once
	// and its own keep fences, and a session taken while this process held its
	// lease again was lost.
	hold, held := c.heldTaker()
	if !held {
		return nil, refuse(calls, executorkeep.LossKeepFailed, takerUnheld)
	}
	// AT THE PLACE AFTER THE NEWEST, or, for a read standing by, at the place
	// of the capture it stands by for.
	place := nextCaptureSeq(current)
	if standing {
		place = current.Capture.CaptureSeq
	}
	stamp := captureStamp(current, run.LaunchID, place, at.final, at.cutOff, hold, taken, box, policy)
	// PLACED BY THE READ ABOVE, made ONLY WHERE ITS HARNESS ENDED ITS OWN RUN,
	// which wrote every call it made before it exited, with the reads its
	// job's brief counted without listing them ([LaunchRecord.OmittedCalls]).
	// EVERY OTHER CAPTURE, a job still working or one CUT OFF ([readCapture]),
	// IS RESERVED AT ZERO AS A SAVE IS, AND RAISED AFTER ITS TAKE
	// ([SessionCapture.placeAtNewest] says why): its harness may have had a
	// call answered that it never wrote (savepoint.go).
	var places *savePlaces
	if calls != nil {
		stamp.placeAt(*calls, current.Launch)
	} else {
		stamp.placeAtZero(current.Launch)
		places = &savePlaces{run: current, resumeRead: taken.ResumeRead,
			needsResumeRead: placesWithResumeRead(runner)}
	}
	stamp.At = c.now().UTC()
	// THE PLACE IS RESERVED WITHIN THE TAKE'S OWN TIME — the room its context
	// keeps for the reservation beside the pack's budget
	// ([Coordinator.reserveRoom]) — never past it: a reservation answered
	// after the take's deadline reserves a place for a take that can no
	// longer spool, and leaves the outcome stamp none of the room it is kept
	// before by ([Coordinator.takeContext]).
	reserveBy, _ := ctx.Deadline()
	standBy := func(stamp SessionCapture) (*spooledCapture, *SessionKept) {
		spooled, kept := c.standBy(ctx, by, reserveBy, run, stamp, taken, at, places, room)
		spooledHold = spooled != nil
		return spooled, kept
	}
	if standing {
		return standBy(stamp)
	}
	for {
		took, stampErr := c.reserveCapture(ctx, reserveBy, run.TurnID, stamp)
		switch {
		case stampErr != nil:
			// UNANSWERED WITHIN THE TAKE'S TIME, which a store may have taken all
			// the same — a write applied and answered too late, or applied once
			// its asker had given up — so the place is settled in the room the
			// read leaves the outcome stamp ([Coordinator.abandonPlace]); and
			// where another read holds it, this one goes on beside that read as
			// one refused outright does.
			stamp.Summary = keepRefused()
			kept, beside := c.abandonPlace(ctx, by, run, stamp, at, stampErr)
			if !beside {
				return nil, kept
			}
		case took:
			spooled, err := c.spool(ctx, run, stamp, taken, places, room)
			if err != nil {
				loss, why := takeRefusal(ctx, err)
				stamp.Summary = keepRefused()
				return nil, c.finishCapture(ctx, by, run, stamp, CaptureRefused, loss, why)
			}
			spooled.summary = at.summary
			spooledHold = true
			c.spooledWhole(ctx, by, run, spooled, taken, at.window)
			return spooled, &SessionKept{CaptureSeq: stamp.CaptureSeq, Bytes: spooled.bytes()}
		}
		// ANOTHER READ OF THE SAME RUN HOLDS THIS PLACE — a stop's and a
		// collection's, at once — and this one stands by for its capture, or
		// takes the place after it ([Coordinator.besideAnother]). Never this
		// take's own earlier ask, answered too late: the store answers a stamp
		// the row already holds as taken ([PendingStore.StampCapture]).
		placed, after := c.besideAnother(ctx, run, stamp)
		if !after {
			return standBy(placed)
		}
		stamp = placed
	}
}

// captureStamp is the begun stamp of a take of row's launch at place, with
// what the take found in box: unplaced ([SessionCapture.placeAt],
// [SessionCapture.placeAtZero]) and with no instant, which its caller stamps
// last, right before the reservation, so the instant a peer reads the take by
// ([SessionCapture.sameTake]) comes after every step the take made before it.
func captureStamp(row PendingRun, launch string, place int64, final, interrupted bool, hold takerHold,
	taken TakenSession, box Sandbox, policy SessionPolicy) SessionCapture {

	return SessionCapture{
		LaunchSeq: row.LaunchSeq, CaptureSeq: place, Launch: launch,
		State: CaptureBegun, Final: final, Interrupted: interrupted,
		NoteCursor: notesHeld(row, interrupted), Taker: hold.holder, TakerEpoch: hold.epoch,
		Harness: taken.Archive.Harness, HarnessVersion: taken.Archive.Version, Entry: row.Launch.Entry,
		SessionID: taken.Archive.SessionID, ArchiveFormat: taken.Archive.Format, ArchiveSize: taken.Archive.Size,
		RunBegan: taken.Archive.RunStart.Began, RunAfter: taken.Archive.RunStart.After,
		RunUnshadowed: taken.Archive.Unshadowed, Trimmed: taken.Archive.Trim.Trimmed(), Home: box.Home(),
		Workspace: path.Join(box.Home(), WorkspaceSubdir), SeatBudget: policy.SeatBudget,
		KeptBound: policy.kept(),
	}
}

// besideAnother is where a take goes on whose place another read of its job
// holds ([Coordinator.takeCapture]), decided off the run's row as it now
// stands: in the PLACE AFTER that read's capture where this take is of the job
// ended and that capture is not — the job's whole session in hand beside one
// taken as the job stood, a read whose end could not end it or poll it
// ([Coordinator.endJob]), which standing by, this take would have left kept
// wherever it landed — answered true; and otherwise standing by for that
// capture at its own place, false. The stamp as it was, standing by, where
// the row cannot be read or names that capture no more: the store decides
// ([PendingStore.StandBy]).
func (c *Coordinator) besideAnother(ctx context.Context, run PendingRun, stamp SessionCapture) (SessionCapture,
	bool) {

	current, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil || !found {
		return stamp, false
	}
	newest := current.Capture
	if newest == nil || newest.Launch != stamp.Launch || newest.LaunchSeq != stamp.LaunchSeq ||
		newest.CaptureSeq < stamp.CaptureSeq {
		return stamp, false
	}
	placed := stamp
	if stamp.Final && !newest.Final {
		placed.CaptureSeq = newest.CaptureSeq + 1
		return placed, true
	}
	placed.CaptureSeq = newest.CaptureSeq
	return placed, false
}

// spooledWhole is what a take does once its session is spooled: the last
// request's context and the harness's own estimate of the next request, read
// off the archive as it was spooled, for the floor of the launch that
// continues it, and what the harness counts the archive at with window, the
// least window the run reported ([capturePoint.window]), the provider an
// OpenCode count is anchored on and a chat file's run start, each stamped with the
// capture's outcome, which is the stamp that launch reads; and the kept
// archive's digest left in its box
// ([TakenSession.Keep]).
func (c *Coordinator) spooledWhole(ctx context.Context, by time.Time, run PendingRun, spooled *spooledCapture,
	taken TakenSession, window int64) {

	if taken.Context != nil {
		spooled.stamp.ContextTokens = taken.Context()
	}
	// AND THE HARNESS'S OWN ESTIMATE OF ITS NEXT REQUEST, off the same read:
	// what a launch that continues this capture is held to with its brief.
	if taken.Next != nil {
		next := taken.Next()
		spooled.stamp.NextRequestTokens, spooled.stamp.NextRequestOlderTokens = next.Tokens, next.OlderTokens
	}
	// AND WHAT THE HARNESS COUNTS THE ARCHIVE AT, as the take read it whole,
	// beside the least window the run reported and the provider of the reply
	// an OpenCode count is anchored on: what a launch that continues this
	// capture judges its first request by. Held to what a stamp carries
	// ([ResumedCount.provided]), so the take never loses its capture to a
	// count the stamp's bounds refuse.
	if taken.Count != nil {
		if count := taken.Count(); count != nil {
			count.Window = window
			var provider string
			if taken.CountProvider != nil {
				provider = taken.CountProvider()
			}
			spooled.stamp.ResumedCount, spooled.stamp.CountProvider = count.provided(provider)
		}
	}
	// AND WHERE ITS RUN BEGAN IN A CHAT FILE, as the take found the box's
	// digest of the lines before it whole: 0 where it did not.
	if taken.RunLines != nil {
		spooled.stamp.RunAfterLines = taken.RunLines()
	}
	// AND, FOR A CAPTURE NO READ PLACES, ITS POINT RAISED by that count, on
	// every path: a save, a take, a standby's spool, whose takeover reserves
	// at it ([SessionCapture.placeAtNewest]).
	if spooled.places != nil {
		spooled.stamp.placeAtNewest(*spooled.places)
	}
	digestCtx, digestCancel := stampContext(ctx, by)
	keepErr := taken.Keep(digestCtx)
	digestCancel()
	if keepErr != nil {
		// NOT A REFUSAL: the session is spooled. What is lost is the box's
		// knowledge that it holds this archive, so a later launch that carries
		// it back into this very box restores it over the copy the box held —
		// which went no further than this capture unless a later one of the
		// box failed, which is what the digest protects against.
		log.WarnContext(ctx, "executor_session_digest_unkept", "turn_id", run.TurnID,
			"session", spooled.stamp.SessionID, "error", keepErr.Error(),
			"detail", "the kept archive's digest could not be recorded in its box")
	}
}

// standBy is a take whose job's capture another read holds the place of (its
// reservation refused, or the capture found begun by that read) standing by
// for that capture rather than walking away: recorded on the run's row
// ([PendingStore.StandBy]), its session spooled whole, and kept in the next
// place only where that capture does not land ([Coordinator.takeOver]).
// Reports nothing where the store would not have it stand by. RECORDED BEFORE
// ITS SPOOL, so a turn's end waits for it at once ([captureInFlight]): the
// holder can fail while this one spools, and an end recorded in between
// tombstones the turn ahead of the session kept. A spool that fails withdraws
// it ([Coordinator.standDown]). A STANDBY THE STORE NEVER ANSWERED GOES ON AS
// ONE THAT MAY STAND: the keep reads the row first ([Coordinator.awaitStood]);
// not standing, it keeps nothing and withdraws.
func (c *Coordinator) standBy(ctx context.Context, by, reserveBy time.Time, run PendingRun, stamp SessionCapture,
	taken TakenSession, at capturePoint, places *savePlaces, hold *spoolHold) (*spooledCapture, *SessionKept) {

	stood, _, err := c.askStamp(ctx, reserveBy, run.TurnID, stamp, c.pending.StandBy)
	switch {
	case !stood && err == nil:
		log.InfoContext(ctx, "executor_session_stood_down", "turn_id", run.TurnID, "point", at.name,
			"capture_seq", stamp.CaptureSeq,
			"detail", "another read of the run holds the capture's place, and keeps the session")
		return nil, nil
	case !stood:
		log.WarnContext(ctx, "executor_session_standby_unanswered", "turn_id", run.TurnID, "point", at.name,
			"capture_seq", stamp.CaptureSeq, "error", err.Error(), "detail", "another read of the run holds "+
				"the capture's place, and the store never answered this one's standing by for it; it keeps "+
				"the session it took spooled until its keep reads whether the run's record names it")
	}
	spooled, err := c.spool(ctx, run, stamp, taken, places, hold)
	if err != nil {
		_, why := takeRefusal(ctx, err)
		c.standDown(ctx, by, run, stamp, why)
		return nil, nil
	}
	spooled.standby, spooled.summary = true, at.summary
	c.spooledWhole(ctx, by, run, spooled, taken, at.window)
	log.InfoContext(ctx, "executor_session_standing_by", "turn_id", run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "bytes", spooled.bytes(),
		"detail", "another read of the run holds the capture's place; this one keeps the session it took, "+
			"spooled, to keep in the next place should that capture not land")
	return spooled, &SessionKept{CaptureSeq: stamp.CaptureSeq, Bytes: spooled.bytes()}
}

// standDown withdraws standby — a read standing by, as the stamp it stood by
// with — from the run's row ([PendingStore.StandDown]), saying why: a read that
// cannot keep what it took, so no wait holds for it.
func (c *Coordinator) standDown(ctx context.Context, by time.Time, run PendingRun, standby SessionCapture,
	why string) {

	c.settleStamp(ctx, by, run.TurnID, standby, c.pending.StandDown)
	log.InfoContext(ctx, "executor_session_stood_down", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"launch_seq", standby.LaunchSeq, "capture_seq", standby.CaptureSeq, "detail", stampReason(why))
}

// takeOver is a standby's keep up to its own place ([spooledCapture.standby]):
// it waits for the capture it stands by for ([Coordinator.awaitStood]) and,
// where that did not land, takes the place after it, begun at a new instant
// under this process's taker lease, in a write the store makes only while that
// capture still names it and has not landed ([PendingStore.TakeOver]). It
// answers whether the standby now holds that place (spooled.stamp its stamp);
// otherwise it has stood down. A landed capture, or one the turn's row on the
// log names, keeps the session and the standby keeps nothing: one upload of a
// job's session, never two. The wait is that capture's whole life on this
// node's clock ([standbyWait]), never its stamped instant, which another
// machine's clock wrote.
func (c *Coordinator) takeOver(ctx context.Context, spooled *spooledCapture) bool {
	run, standby := spooled.run, spooled.stamp
	wait, cancel := context.WithTimeout(ctx, standbyWait)
	defer cancel()
	stood, keep, why := c.awaitStood(wait, run, standby)
	switch {
	case !keep && stood.State == CaptureLanded:
		log.InfoContext(ctx, "executor_session_stood_down", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", standby.LaunchSeq, "capture_seq", standby.CaptureSeq, "detail", why)
		return false
	case !keep:
		c.standDown(ctx, time.Time{}, run, standby, why)
		return false
	}
	hold, held := c.heldTaker()
	if !held {
		c.standDown(ctx, time.Time{}, run, standby, takerUnheld)
		return false
	}
	over := standby
	over.CaptureSeq, over.At, over.State, over.Reason = stood.CaptureSeq+1, c.now().UTC(), CaptureBegun, ""
	over.Taker, over.TakerEpoch = hold.holder, hold.epoch
	took, _, err := c.askStamp(ctx, time.Time{}, run.TurnID, over,
		func(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
			return c.pending.TakeOver(ctx, turnID, standby, stamp)
		})
	switch {
	case !took && err != nil:
		// UNANSWERED, which the store may have made all the same, its answer
		// lost: settled rather than walked away from ([Coordinator.tookOver]),
		// or the place it took stood begun with no keep behind it.
		if !c.tookOver(ctx, run, standby, over, err) {
			return false
		}
	case !took:
		c.standDown(ctx, time.Time{}, run, standby,
			"the run's record no longer names this read standing by for a capture that did not land")
		return false
	}
	spooled.stamp = over
	log.InfoContext(ctx, "executor_session_taken_over", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"launch_seq", over.LaunchSeq, "capture_seq", over.CaptureSeq, "stood_by", stood.CaptureSeq,
		"state", string(stood.State), "detail", "the capture this read stood by for did not land, so this one "+
			"keeps the session it took in the next place")
	return true
}

// tookOver settles a takeover the store never answered ([PendingStore.TakeOver]),
// over (the standby's own stamp in the next place), which it may have made all
// the same, its answer lost: the standby is withdrawn first
// ([PendingStore.StandDown]), so no takeover still on its way can be made
// after; where it is no longer there to withdraw, the row is read and answers
// whether it holds over, begun, its keep to go on under it. Otherwise the
// standby keeps nothing: walking away would leave a made takeover begun with
// no keep, and the turn's end waiting out a capture's whole budget. THE READ
// IS HELD TO A READ'S ROOM, never a change's ([Coordinator.rowReadContext]): a
// step of the takeover's life ([captureReadBudget]), while the keep's context
// has no deadline before its budget starts. A read so cut is a row unread.
func (c *Coordinator) tookOver(ctx context.Context, run PendingRun, standby, over SessionCapture, cause error) bool {
	why := "the next place could not be taken on the run's record: " + cause.Error()
	if c.settleStamp(ctx, time.Time{}, run.TurnID, standby, c.pending.StandDown) {
		log.InfoContext(ctx, "executor_session_stood_down", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", standby.LaunchSeq, "capture_seq", standby.CaptureSeq, "detail", stampReason(why))
		return false
	}
	read, cancel := c.rowReadContext(ctx, c.recordBudget)
	current, found, err := c.pending.Get(read, run.TurnID)
	cancel()
	if err == nil && found && current.Capture != nil && current.Capture.sameTake(over) &&
		current.Capture.State == CaptureBegun {
		return true
	}
	sayNotKept(ctx, slog.LevelWarn, executorkeep.LossKeepFailed, run.TurnID, "agent", run.AgentHandle,
		"launch_seq", standby.LaunchSeq, "capture_seq", standby.CaptureSeq, "detail", stampReason(why))
	return false
}

// awaitStood waits, for a read standing by, until the capture it stands by for
// has landed, been refused, or been begun past its budget
// ([SessionCaptureBudget]), reading the row every [Coordinator.captureTick]
// and settling a gone taker's capture from the log ([Coordinator.settleGone]).
// It answers that capture and whether the standby keeps its own in its stead:
// not where it landed, the row moved on or ctx ended, nor where the turn's row
// on the log names it, on the log's word alone whether or not the store took
// the landed settle. One unanswered is asked again and stood in for only once
// the log refuses it; one begun past its budget is read on the log once first,
// whatever its lease. Where its own time runs out, [Coordinator.waitedOut]
// decides. The package doc, "Two reads of one job", has the argument.
func (c *Coordinator) awaitStood(ctx context.Context, run PendingRun, standby SessionCapture) (SessionCapture,
	bool, string) {

	// readLog is whether the wait has read the turn's row on the log for a
	// capture begun past its budget: once, at its last decision.
	readLog := false
	for {
		current, found, err := c.pending.Get(ctx, run.TurnID)
		if err == nil {
			newest := current.Capture
			switch {
			case !found:
				return SessionCapture{}, false, "the run's record is gone"
			case newest == nil || !newest.standsBy(standby):
				return SessionCapture{}, false, "the run's record no longer names this read standing by for its capture"
			case newest.State == CaptureLanded:
				return *newest, false, "the capture this read stood by for landed, and keeps the session"
			case newest.State == CaptureUnresolved:
				switch settled := c.settleCapture(ctx, current, *newest); settled.State {
				case CaptureLanded:
					return settled, false, "the capture this read stood by for landed once its record was " +
						"asked again, and keeps the session"
				case CaptureRefused:
					return settled, true, ""
				}
			case newest.State == CaptureBegun && !newest.keeping(c.now()) && !readLog:
				// THE STANDBY'S LAST READ, before it takes the place after a
				// capture begun past its budget: the turn's row on the log,
				// once, whatever the store answers of that capture's taker's
				// lease, so one the log took is settled landed and this read
				// keeps nothing, on the row's word whether or not the store
				// takes that settle. Held to the standby's own wait (ctx,
				// [standbyWait] from its keep's start), so it never moves the
				// takeover past that wait's end; its settle is given a
				// stamp's room of its own past it
				// ([Coordinator.settleContext]), the settle a pass of the
				// wait began before its end that [standbyWindow] counts.
				readLog = true
				var named []SessionCapture
				if _, found, named = c.settleGone(ctx, current, newest.sameTake, true); !found {
					return SessionCapture{}, false, "the run's record is gone"
				}
				if slices.ContainsFunc(named, newest.sameTake) {
					return *newest, false, "the turn's row on the executor log names the capture this read " +
						"stood by for, so its record landed and keeps the session"
				}
				continue
			case newest.State != CaptureBegun || !newest.keeping(c.now()):
				return *newest, true, ""
			}
			// THE CAPTURE STOOD BY FOR ALONE, so a pass writes the one settle
			// [standbyWindow] counts: a save under way beside it decides
			// nothing here, and the end's and the relaunch's waits settle it.
			if _, found, _ = c.settleGone(ctx, current, newest.sameTake, false); !found {
				return SessionCapture{}, false, "the run's record is gone"
			}
		}
		tick := time.NewTimer(c.captureTick)
		select {
		case <-ctx.Done():
			tick.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return c.waitedOut(ctx, run, standby)
			}
			return SessionCapture{}, false, fmt.Sprintf("the read stood by until its wait ended, the capture it "+
				"stood by for still being kept: %v", context.Cause(ctx))
		case <-tick.C:
		}
	}
}

// waitedOut is what a read standing by decides once its own wait has run out
// ([standbyWait]), under contexts of its own. First, once, the turn's row on
// the log ([Coordinator.namedAtWaitsEnd]): where it names the capture still
// begun, the standby keeps nothing, on the log's word alone. Then one read of
// the run's row, in a read's room ([Coordinator.rowReadContext]): the capture
// still BEGUN or REFUSED and naming it, it takes the place after it; LANDED, it
// keeps nothing; anything else, unresolved included, since its record may
// still land, it keeps nothing, saying why. A capture begun past its whole
// life on this clock can send nothing more that lands, so what this decides
// turns on neither its stamped instant nor its lease (which the log's settle
// reads). Both reads and the settle are counted in [standbyWindow].
func (c *Coordinator) waitedOut(ctx context.Context, run PendingRun, standby SessionCapture) (SessionCapture,
	bool, string) {

	free, release := detach(ctx)
	defer release()
	if stood, named := c.namedAtWaitsEnd(free, run, standby); named {
		return stood, false, "the turn's row on the executor log names the capture this read stood by for, " +
			"so its record landed and keeps the session"
	}
	read, cancel := c.rowReadContext(free, c.recordBudget)
	defer cancel()
	current, found, err := c.pending.Get(read, run.TurnID)
	newest := current.Capture
	switch {
	case err != nil:
		return SessionCapture{}, false, fmt.Sprintf("the run's record could not be read at the end of the "+
			"wait: %v", err)
	case !found:
		return SessionCapture{}, false, "the run's record is gone"
	case newest == nil || !newest.standsBy(standby):
		return SessionCapture{}, false, "the run's record no longer names this read standing by for its capture"
	case newest.State == CaptureLanded:
		return *newest, false, "the capture this read stood by for landed, and keeps the session"
	case newest.State == CaptureBegun, newest.State == CaptureRefused:
		return *newest, true, ""
	}
	return SessionCapture{}, false, "the capture this read stood by for still awaits the log's answer to its " +
		"record"
}

// namedAtWaitsEnd is [Coordinator.waitedOut]'s read of the turn's row on the
// executor log: the run's row read under one change of the row's room
// ([Coordinator.rowContext]), and where it still names standby standing by
// for a capture begun, that capture settled as the wait's last decision
// settles it ([Coordinator.settleGone] with readHeld) under the same room. It
// answers that capture as it was read, and whether the log's row names it. A
// CHANGE'S ROOM, NOT A READ'S: the pass reads the run's row up to twice, the
// taker's lease and the log's row, more than one read's reply at its bound.
func (c *Coordinator) namedAtWaitsEnd(ctx context.Context, run PendingRun, standby SessionCapture) (SessionCapture,
	bool) {

	pass, cancel := c.rowContext(ctx, c.recordBudget)
	defer cancel()
	current, found, err := c.pending.Get(pass, run.TurnID)
	newest := current.Capture
	if err != nil || !found || newest == nil || !newest.standsBy(standby) || newest.State != CaptureBegun {
		return SessionCapture{}, false
	}
	_, _, named := c.settleGone(pass, current, newest.sameTake, true)
	return *newest, slices.ContainsFunc(named, newest.sameTake)
}

// takeContext is the context a take runs under once its row is read
// ([Coordinator.placingRow]), and the instant by which its own records must be
// made: its own budget ([SessionTakeBudget]) for its pack and scrub, and beside
// it the room its reservation of the capture's place is given
// ([Coordinator.reserveRoom]), never past where the read it is part of must
// stop ([Coordinator.takeBy]). by is zero where the read has no deadline. EVERY
// ROOM ON THE ROW IS BESIDE THE TAKE'S BUDGET, never inside it, the read before
// it as the reservation within it: each crosses the row whole, a pack at its
// bound left only the scrub's minutes, and a large row was cut while its bytes
// crossed a slow link (crewlet/crewlet#1151).
func (c *Coordinator) takeContext(ctx context.Context) (context.Context, context.CancelFunc, time.Time) {
	deadline := time.Now().Add(SessionTakeBudget + c.reserveRoom())
	by := c.recordsBy(ctx)
	if last := c.takeBy(by); !last.IsZero() && last.Before(deadline) {
		deadline = last
	}
	take, cancel := context.WithDeadline(ctx, deadline)
	return take, cancel, by
}

// takeBy is the instant a take, and its read of its row before it, end by, so
// the records the read makes after it keep their room
// ([Coordinator.takeRecordsRoom]): the stamp of what came of it, the digest
// left in the box ([stampContext]), how the harness ended
// ([Coordinator.markHarnessEnd]), and then the pause and its record
// ([Coordinator.pauseRoom]), so a take never costs the box its pause. Zero
// where by, the end of the read's records ([Coordinator.recordsBy]), is.
func (c *Coordinator) takeBy(by time.Time) time.Time {
	if by.IsZero() {
		return by
	}
	return by.Add(-c.takeRecordsRoom())
}

// reserveRoom is the room a take gives its reservation of the capture's place
// beside its own budget ([Coordinator.takeContext]): one record's reason
// ([sessionRecordBudget]), within which the reservation is asked again while
// the store does not answer ([Coordinator.askStamp]), and the time a change of
// the run's row at its bound takes to cross this node's connection to the
// store ([RowBudget]) — none in the store's own process.
func (c *Coordinator) reserveRoom() time.Duration { return RowBudget(c.pending, c.recordBudget) }

// takeRecordsRoom is the room a read leaves the records it makes after its
// take, before its pause ([Coordinator.recordsBy]): a teardown's grace
// ([discardGrace]) for EACH of the two, the take's own (the stamp where it was
// refused, [Coordinator.settleContext], or the digest left in the box where it
// spooled, [stampContext]) and how its job's harness ended
// ([Coordinator.markHarnessEnd]), and the crossing of each change of the run's
// row among them ([PendingStore.RowCrossing]): none in the store's own process.
//
// A GRACE EACH, since each is given a grace of its own from where it is asked:
// one grace for the pair let a take whose own record took its whole room leave
// the harness-end record a grace short, and none in the store's own process,
// so it was cut at recordsBy and said as unrecorded.
func (c *Coordinator) takeRecordsRoom() time.Duration {
	return 2*discardGrace + 2*c.pending.RowCrossing()
}

// pauseRoom is the room a read leaves its box's pause, the pause's record on
// the run's row ([PendingStore.MarkBoxPaused]) and its answer: a teardown's
// grace ([discardGrace]) and the crossing of that one change of the row at
// this node's connection to the store — none in the store's own process.
func (c *Coordinator) pauseRoom() time.Duration { return discardGrace + c.pending.RowCrossing() }

// recordsBy is the instant by which a read's records made after its take must
// be made — the take's own ([Coordinator.takeContext]) and how its job's
// harness ended ([Coordinator.markHarnessEnd]) — so its pause, the pause's
// record and its answer keep their room ([Coordinator.pauseRoom]); zero where
// the read has no deadline.
func (c *Coordinator) recordsBy(ctx context.Context) time.Time {
	d, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	return d.Add(-c.pauseRoom())
}

// stampContext is the context the one record a take makes only once is made
// under — the digest of its archive, left in the box: one of its own
// ([detached]), since the take's budget may be what ended the take, and never
// past by, where the read the take is part of needs the rest of its time for
// the box's pause ([Coordinator.takeContext]). by zero bounds it by its own
// grace alone. A stamp of the capture is asked again, bounded the same way
// ([Coordinator.settleContext]).
func stampContext(ctx context.Context, by time.Time) (context.Context, context.CancelFunc) {
	stamp, cancel := detached(ctx)
	return before(stamp, cancel, by)
}

// recordContext is [stampContext] for a record a read makes once on the run's
// row — how its job's harness ended ([Coordinator.markHarnessEnd]): one of its
// own within the grace and the time the row takes to cross to the store
// ([Coordinator.detachedRecord]), never past by. The grace alone was a box
// call's, which a row of megabytes across a slow link outlasts while its bytes
// are still crossing (crewlet/crewlet#1076).
func (c *Coordinator) recordContext(ctx context.Context, by time.Time) (context.Context, context.CancelFunc) {
	record, cancel := c.detachedRecord(ctx)
	return before(record, cancel, by)
}

// before is ctx, whose cancel is cancel, held to end no later than by as well;
// ctx itself for by zero.
func before(ctx context.Context, cancel context.CancelFunc, by time.Time) (context.Context, context.CancelFunc) {
	if by.IsZero() {
		return ctx, cancel
	}
	bounded, cancelBounded := context.WithDeadline(ctx, by)
	return bounded, func() {
		cancelBounded()
		cancel()
	}
}

// takeRefusal is why a take was refused, in the words its stamp keeps: the
// take's budget where that is what ended it, which says so plainly rather than
// as a deadline error.
func takeRefusal(ctx context.Context, err error) (executorkeep.Loss, string) {
	var unrestorable *UnrestorableError
	switch {
	case errors.As(err, &unrestorable):
		return unrestorable.Loss, "no box of this build continues the session, so it was not kept: " +
			unrestorable.Why
	case errors.Is(err, executorkeep.ErrSessionPlainTooLarge):
		return executorkeep.LossTooLarge, fmt.Sprintf("the session expands past the %d MiB of plaintext a "+
			"session is kept within", executorkeep.SessionPlainMaxBytes>>20)
	case errors.Is(err, executorkeep.ErrSessionTooLarge):
		return executorkeep.LossTooLarge, tooLargeWhy(err)
	case ctx.Err() != nil:
		return executorkeep.LossKeepFailed, fmt.Sprintf("the session could not be taken out of its box within "+
			"the read's budget (at most %s): %v", SessionTakeBudget, err)
	}
	return executorkeep.LossKeepFailed, fmt.Sprintf("the box's session could not be taken: %v", err)
}

// spool seals taken's body into a file of this node's own, as it streams, under
// an object key minted now, and each payload a framed body keeps apart
// ([TakenSession.Framed]) into a file of its own, never all of them past the
// room hold holds, which it shrinks to their sealed size; the caller gives
// hold back where it fails.
//
// THE CLOSE IS WHAT BOUNDS IT: a box's stream that stalls blocks a read for as
// long as it stalls, which no check of ctx between reads can interrupt, so the
// body is closed the moment ctx ends.
func (c *Coordinator) spool(ctx context.Context, run PendingRun, stamp SessionCapture,
	taken TakenSession, places *savePlaces, hold *spoolHold) (*spooledCapture, error) {

	// A TAKE WHOSE TIME HAS ENDED SPOOLS NOTHING: its close would race the
	// first read, and a box's stream is cut at that end whichever wins. Its
	// deadline is read off the clock too: ctx reports its end only once its
	// timer has run, after the instant the deadline names.
	ended := ctx.Err()
	if deadline, ok := ctx.Deadline(); ended == nil && ok && !time.Now().Before(deadline) {
		ended = context.DeadlineExceeded
	}
	if ended != nil {
		return nil, fmt.Errorf("sandbox: the take ended before its session was spooled: %w", ended)
	}
	dir, err := c.spoolHere()
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, spoolPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("sandbox: a spool for the session on this node's disk: %w", err)
	}
	var (
		media []spooledMedia
		split *TakenText
	)
	text := io.Reader(taken.Body)
	if taken.Framed {
		split = SplitTakenBody(taken.Body, func(member io.Reader) (string, error) {
			m, mediaErr := c.spoolMedia(dir, run.AgentHandle, member, hold)
			if mediaErr != nil {
				return "", mediaErr
			}
			media = append(media, m)
			return m.spool.Digest, nil
		})
		text = split
	}
	stop := context.AfterFunc(ctx, func() { _ = taken.Body.Close() })
	sealed, sealErr := c.keep.Seal(run.AgentHandle, run.TurnID, text, hold.sealing(file))
	stop()
	if err = errors.Join(sealErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		removeMedia(media)
		return nil, err
	}
	spooled := &spooledCapture{run: run, stamp: stamp, spool: sealed, file: file.Name(), media: media,
		hold: hold, places: places, begun: stamp}
	if len(media) > 0 {
		// THE ARCHIVE ITS RESTORE HANDS BACK, every repeat counted: its seal
		// read the body whole, so the count is whole; and how many objects
		// its keep uploads or carries.
		spooled.stamp.ArchiveBytes = split.Restored()
		spooled.stamp.MediaCount, spooled.begun.MediaCount = len(media), len(media)
	}
	hold.shrinkTo(spooled.bytes())
	return spooled, nil
}

// spoolMedia seals one payload's member, member, into a file of dir's own,
// within the take's room, hold.
func (c *Coordinator) spoolMedia(dir, handle string, member io.Reader, hold *spoolHold) (spooledMedia, error) {
	file, err := os.CreateTemp(dir, spoolPrefix+"*")
	if err != nil {
		return spooledMedia{}, fmt.Errorf("sandbox: a spool for the session's media on this node's disk: %w", err)
	}
	sealed, sealErr := c.keep.SealMedia(handle, member, hold.sealing(file))
	if err = errors.Join(sealErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return spooledMedia{}, err
	}
	return spooledMedia{spool: sealed, file: file.Name()}, nil
}

// keepLater keeps a spooled capture off the frame that took it, on the
// coordinator's own lifetime ([Coordinator.keepCapture]); nothing for nil. A
// coordinator that is stopping keeps nothing more, and says why on the stamp,
// made under ctx, the frame's: so a capture the coordinator's own stopped work
// took is stamped within the stop's one grace ([Coordinator.Stop]).
func (c *Coordinator) keepLater(ctx context.Context, spooled *spooledCapture) {
	if spooled == nil {
		return
	}
	if !c.joinKeeps() {
		c.dropSpool(ctx, spooled, executorkeep.LossKeepFailed, "the node stopped before the session could be uploaded")
		return
	}
	go func() {
		defer c.keeping.Done()
		c.keepCapture(c.keepLife, spooled)
	}()
}

// joinKeeps counts a keep about to start in the keeps [Coordinator.Stop] waits
// for, and reports false, counting nothing, once the coordinator is stopping:
// checked and counted under one lock, so no keep starts after Stop has begun
// its wait.
func (c *Coordinator) joinKeeps() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.keeping.Add(1)
	return true
}

// dropSpool refuses a spooled capture that will not be kept, saying why, and
// removes its spool: a standby, which holds no place to refuse, is withdrawn
// instead ([Coordinator.standDown]), so no wait holds for it. The stamp is made
// under a context of its own, keeping ctx's values ([detach]).
func (c *Coordinator) dropSpool(ctx context.Context, spooled *spooledCapture, loss executorkeep.Loss,
	why string) {

	defer spooled.remove()
	if spooled.standby {
		c.standDown(ctx, time.Time{}, spooled.run, spooled.stamp, why)
		return
	}
	c.finishCapture(ctx, time.Time{}, spooled.run, spooled.stamp, CaptureRefused, loss, why)
}

// keepCapture uploads a spooled capture into the objects it was sealed for,
// records it on the executor log and stamps what came of it within its take's
// bound's budget ([takeBound]), and removes the spool. A STANDBY first waits
// to see whether it keeps anything ([Coordinator.takeOver]), answering nil if
// the capture it stood by for landed. FENCED BY THE TAKER LEASE ITS STAMP
// NAMES before every attempt at its upload ([Coordinator.uploadSpool]) and its
// record, each record's cut at its window's end ([Coordinator.recordCapture]):
// a keep the lease no longer covers may be one a peer has already settled
// refused ([Coordinator.fenceKeep]). ITS FOUR STEPS BEFORE THE UPLOAD (its
// media count, the mint, the carry read with the listing, a save's amend) each
// have a room ([keepReadRoom]), and its read again of who reads its media one
// more ([MediaReadersRoom]), beside the upload's pace ([takeLife]).
func (c *Coordinator) keepCapture(ctx context.Context, spooled *spooledCapture) *SessionKept {
	defer spooled.remove()
	if spooled.standby && !c.takeOver(ctx, spooled) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, boundOf(spooled.stamp.KeptBound).keep)
	defer cancel()
	c.stampMediaCount(ctx, spooled)
	// ITS SUMMARY BESIDE IT, waited for before the keep's stamp, which names
	// it: the session's upload never waits on the summary's.
	summary := c.keepSummaryBeside(ctx, spooled)
	defer summary()
	run, stamp := spooled.run, spooled.stamp
	named := func(s SessionCapture) SessionCapture {
		s.Summary = summary()
		return s
	}
	mint, minting := context.WithTimeout(ctx, keepReadRoom)
	minted, err := c.keep.MintedAt(mint, run.TurnID, run.WorkKey, run.WorkSince)
	minting()
	if err != nil {
		return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused, executorkeep.LossKeepFailed,
			fmt.Sprintf("the turn's operations could not be minted: %v", err))
	}
	stamp.Minted = minted
	// WHICH MEDIA IT CARRIES, read in the listing's room: what the turn's row
	// and its held base name already is not uploaded again
	// ([Coordinator.mediaHeld]); and a save's charge amended to what it then
	// uploads, in a room of its own ([keepBudgetFor]).
	listing, listed := context.WithTimeout(ctx, keepReadRoom)
	spooled.carry(c.mediaHeld(listing, run, spooled.media))
	over := c.overBudget(listing, run, spooled.bytes(), stamp.SeatBudget)
	listed()
	if over == "" {
		over = c.amendSave(ctx, spooled)
	}
	if over != "" {
		return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused, executorkeep.LossStoreRefused,
			over)
	}
	fence := c.keepFence(stamp)
	sealed, ended := c.uploadSpool(ctx, spooled, stamp, fence, named)
	if ended != nil {
		return ended
	}
	stamp.Object, stamp.ObjectHash = sealed.Object.Key.String(), string(sealed.Object.Hash)
	stamp.ObjectSize, stamp.ContentKey = sealed.Object.Size, sealed.ContentKey
	if stamp.Media, ended = c.uploadMedia(ctx, spooled, stamp, fence, named); ended != nil {
		return ended
	}
	if below := c.readersBelow(ctx, run, stamp); below != "" {
		return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused, executorkeep.LossKeepFailed,
			below)
	}
	outcome, why, fenced, gone := c.recordCapture(ctx, run, stamp, fence)
	// A CARRY THE LOG FOUND GONE (another capture or a save of the turn moved
	// its row since the keep read it) is uploaded after all and recorded
	// again: the log refused the record whole, so its operation took nothing.
	if gone && !fenced && spooled.uncarry() {
		if refused := c.amendSave(ctx, spooled); refused != "" {
			return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused,
				executorkeep.LossStoreRefused, refused)
		}
		if stamp.Media, ended = c.uploadMedia(ctx, spooled, stamp, fence, named); ended != nil {
			return ended
		}
		if below := c.readersBelow(ctx, run, stamp); below != "" {
			return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused,
				executorkeep.LossKeepFailed, below)
		}
		outcome, why, fenced, _ = c.recordCapture(ctx, run, stamp, fence)
	}
	if fenced {
		return c.fenceKeep(ctx, run, named(stamp), "record", outcome, why)
	}
	return c.finishCapture(ctx, time.Time{}, run, named(stamp), outcome, executorkeep.LossLogRefused, why)
}

// stampMediaCount stamps on a capture's begun stamp, as its place was
// reserved, how many media objects its spool cut apart ([spooledCapture.begun])
// in a room of its own ([keepReadRoom], counted in [keepBudgetFor]): a relaunch
// waits for a capture by that count ([captureRestFor]), each object's upload
// paced by the mebibyte begun. A takeover's begun stamp carries it already,
// and a capture with none stamps nothing. Unanswered, the keep goes on: the
// count lengthens a wait, and decides nothing of the keep.
func (c *Coordinator) stampMediaCount(ctx context.Context, spooled *spooledCapture) {
	begun := spooled.begun
	if spooled.standby || begun.MediaCount == 0 {
		return
	}
	room, cancel := context.WithTimeout(ctx, keepReadRoom)
	defer cancel()
	if took, err := c.pending.StampCapture(room, spooled.run.TurnID, begun); err != nil || !took {
		log.InfoContext(ctx, "executor_media_count_unstamped", "turn_id", spooled.run.TurnID,
			"capture_seq", begun.CaptureSeq, "media", begun.MediaCount, "error", fmt.Sprint(err),
			"detail", "a relaunch waits for the capture as for one object until its count is read")
	}
}

// uploadSpool uploads spooled's session into the object it was sealed for,
// under ctx, the keep's: the object, or the stamp of why it was not uploaded
// ([Coordinator.uploadAgain]).
func (c *Coordinator) uploadSpool(ctx context.Context, spooled *spooledCapture, stamp SessionCapture,
	fence func() (time.Time, string), named func(SessionCapture) SessionCapture) (executorkeep.SealedObject,
	*SessionKept) {

	var sealed executorkeep.SealedObject
	ended := c.uploadAgain(ctx, spooled, stamp, fence, named, "session", spooled.file, spooled.paceLeft(0),
		func(body io.Reader) (err error) {
			sealed, err = c.keep.Upload(ctx, spooled.spool, body)
			return err
		})
	return sealed, ended
}

// uploadMedia uploads each of spooled's media its keep does not carry, in
// order, each as its session is ([Coordinator.uploadAgain]), and answers every
// media object the capture names, uploaded and carried; or the stamp of why
// one was not uploaded.
func (c *Coordinator) uploadMedia(ctx context.Context, spooled *spooledCapture, stamp SessionCapture,
	fence func() (time.Time, string), named func(SessionCapture) SessionCapture) ([]CaptureMedia, *SessionKept) {

	var media []CaptureMedia
	for i, m := range spooled.media {
		switch {
		case m.carried != nil:
			media = append(media, captureMediaOf(*m.carried, true))
			continue
		case m.uploaded != nil:
			media = append(media, captureMediaOf(*m.uploaded, false))
			continue
		}
		var put executorkeep.MediaObject
		ended := c.uploadAgain(ctx, spooled, stamp, fence, named, "media "+m.spool.Digest, m.file,
			spooled.paceLeft(i+1), func(body io.Reader) (err error) {
				put, err = c.keep.UploadMedia(ctx, m.spool, body)
				return err
			})
		if ended != nil {
			return nil, ended
		}
		spooled.media[i].uploaded = &put
		media = append(media, captureMediaOf(put, false))
	}
	return media, nil
}

// captureMediaOf is object as a capture names it, carried or uploaded for it.
func captureMediaOf(object executorkeep.MediaObject, carried bool) CaptureMedia {
	return CaptureMedia{Digest: object.Digest, Object: object.Object.Key.String(),
		ObjectHash: string(object.Object.Hash), ObjectSize: object.Object.Size, ContentKey: object.ContentKey,
		Carried: carried}
}

// uploadAgain uploads what of spooled's file holds, what, through put, under
// ctx, the keep's: nil, or the stamp of why it was not uploaded. A PUT THE
// STORE TOOK NONE OF (objstore.ErrUnanswered: nothing is stored, and a later
// put may be) IS MADE AGAIN, spaced as a record's attempts are, each begun
// only while the keep's budget left holds pace, the store's slowest for every
// object still to upload from this one, and its record's budget, so an
// attempt never takes the record's room; any other refusal is final. The
// keep's fence is asked before every attempt.
func (c *Coordinator) uploadAgain(ctx context.Context, spooled *spooledCapture, stamp SessionCapture,
	fence func() (time.Time, string), named func(SessionCapture) SessionCapture, what, file string,
	pace time.Duration, put func(io.Reader) error) *SessionKept {

	run := spooled.run
	deadline, _ := ctx.Deadline()
	room := pace + c.recordBudget
	for attempt := 1; ; attempt++ {
		if _, why := fence(); why != "" {
			return c.fenceKeep(ctx, run, named(stamp), "upload", CaptureRefused, why)
		}
		body, err := os.Open(file)
		if err != nil {
			return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused, executorkeep.LossKeepFailed,
				fmt.Sprintf("the spooled %s could not be read back on this node: %v", what, err))
		}
		err = put(body)
		_ = body.Close()
		if err == nil {
			return nil
		}
		wait := backoff.Doubling(attempt, c.sessionRetry, sessionRetryCeiling)
		if errors.Is(err, objstore.ErrUnanswered) && time.Until(deadline) >= wait+room {
			log.InfoContext(ctx, "executor_session_upload_unanswered", "turn_id", run.TurnID,
				"capture_seq", stamp.CaptureSeq, "attempt", attempt, "error", err.Error(), "spool", what,
				"detail", "the object store took none of it; it is uploaded again into the same object")
			select {
			case <-ctx.Done():
				err = fmt.Errorf("%w; the keep ended before it was uploaded again: %w", err, ctx.Err())
			case <-time.After(wait):
				continue
			}
		}
		loss, why := uploadRefusal(err)
		if attempt > 1 {
			why = fmt.Sprintf("%s (attempt %d)", why, attempt)
		}
		return c.finishCapture(ctx, time.Time{}, run, named(stamp), CaptureRefused, loss, why)
	}
}

// mediaHeld is the media objects the turn's row, and the base that row holds,
// name live, by digest: what a capture carries rather than uploads again
// (ADR-0027's "a session's media, carried"), never another seat's, whose AAD
// would not open. Nil where the take kept none apart, or a read fails within
// ctx: each is then uploaded fresh, which is always correct.
func (c *Coordinator) mediaHeld(ctx context.Context, run PendingRun,
	media []spooledMedia) map[string]executorkeep.MediaObject {

	if len(media) == 0 || c.restore == nil {
		return nil
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, run.TurnID)
	if err != nil {
		return nil
	}
	held := map[string]executorkeep.MediaObject{}
	for _, m := range row.Media {
		held[m.Digest] = m
	}
	if row.HoldsBase() {
		if base, baseErr := c.restore.Session(ctx, run.AgentHandle, row.BaseTurn); baseErr == nil {
			for _, m := range base.Media {
				if _, named := held[m.Digest]; !named {
					held[m.Digest] = m
				}
			}
		}
	}
	return held
}

// overBudget is why a capture of size bytes cannot fit the seat's budget beside
// the sessions its other LIVE turns hold, or "" where it may: asked before the
// upload, so a seat whose live turns fill its budget does not upload a session
// on every launch that the log then refuses. Read off the seat's runs in the
// coordination store: each live turn's kept capture, and for one that has kept
// nothing yet the session it froze as its base ([PendingRun.Base]), each such
// base once; the capturing turn's own is not counted, as its capture lifts its
// hold in the same apply. The log decides again at the record ([LogRefused]),
// which also counts a turn whose end was lost. A listing that cannot be read
// within [keepReadRoom] decides nothing here, since one held past its room
// left the upload less than its pace.
func (c *Coordinator) overBudget(ctx context.Context, run PendingRun, size, budget int64) string {
	runs, err := c.pending.ListActiveForSeat(ctx, run.AgentHandle)
	if err != nil {
		return ""
	}
	var held int64
	bases := map[string]bool{}
	for _, other := range runs {
		switch {
		case other.TurnID == run.TurnID:
		case other.Kept != nil:
			held += other.Kept.ObjectSize + other.Kept.mediaBytes()
		case other.Base != nil && other.Base.Session != nil && !bases[other.Base.Session.Turn]:
			bases[other.Base.Session.Turn] = true
			held += other.Base.Session.ObjectSize + other.Base.Session.MediaBytes
		}
	}
	if held+size <= budget {
		return ""
	}
	return fmt.Sprintf("the seat's other live turns hold %d bytes of sessions, and this one is %d: past "+
		"the seat's budget of %d (turn_engine.conversation_session.max_bytes_per_seat), so it was not "+
		"uploaded", held, size, budget)
}

// keepLostSession makes the last read of a lost run's box, where its box may
// still be read, before the ending reclaims it ([Coordinator.capturesLoss]):
// the job ended, as a stop's is ([Coordinator.endJob]), its served notes
// settled by its session and its session taken, here or asked of the box's
// host ([BoxReadLoss]). ATTACHED, NEVER CONNECTED: a box about to be reclaimed
// is not woken. Made by the ending's own retry ([Coordinator.finishEnding]).
//
// A STEP THE STORE DID NOT ANSWER ([ErrStoreSilent]) is returned and the
// ending kept, as is a read here that never started ([errReadUnstarted]); only
// the store's silence to another node's host, rowRead answered, is given up as
// a stop's is ([Coordinator.hostGivenUp]), the loss then ending with its notes
// unconfirmed. Every other failure is logged and answers nil.
func (c *Coordinator) keepLostSession(ctx context.Context, run PendingRun, rowRead bool) error {
	if !c.capturesLoss(run) {
		return nil
	}
	asked, local := c.now(), c.hosts(run)
	var err error
	if local {
		err = c.readLostHere(ctx, run)
	} else {
		if c.boxes == nil {
			return nil
		}
		var result Result
		result, _, err = c.boxes.ReadBox(ctx, run, BoxReadLoss)
		if kept := result.Kept; err == nil && kept != nil && kept.NotKept != "" {
			sayNotKept(ctx, slog.LevelInfo, kept.Loss, run.TurnID, "point", string(BoxReadLoss),
				"node", run.LaunchNode, "detail", kept.NotKept)
		}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errReadUnstarted):
		return fmt.Errorf("sandbox: reading lost run %s's box: %w", run.TurnID, err)
	case !errors.Is(err, ErrStoreSilent):
		sayNotKept(ctx, slog.LevelInfo, executorkeep.LossBoxLost, run.TurnID, "point", string(BoxReadLoss),
			"node", run.LaunchNode, "error", err.Error(),
			"detail", "the lost run's box is on another node's host, which did not read it before "+
				"its ending reclaims it")
		return nil
	case rowRead && !local && c.hostGivenUp(run, c.hostSilence(run, asked, err)):
		log.WarnContext(ctx, "steer_receipt_unsettled", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"point", string(BoxReadLoss), "given_up", true, "error", err.Error(),
			"detail", "the coordination store did not answer the lost job's read for the host grace by "+
				"this node's clock; the loss ends, and its served notes are reported unconfirmed")
		return nil
	}
	return fmt.Errorf("sandbox: reading lost run %s's box: %w", run.TurnID, err)
}

// readLostHere is keepLostSession's read of a box this node reaches itself
// ([Coordinator.hosts]), holding one of the node's collections for the read
// alone ([Coordinator.collectionSlot]), its session kept later.
func (c *Coordinator) readLostHere(ctx context.Context, run PendingRun) error {
	collecting, err := c.collectionSlot(ctx, run.TurnID)
	if err != nil {
		return err
	}
	defer collecting()
	spooled, _, err := c.readLost(ctx, run)
	c.keepLater(ctx, spooled)
	return err
}

// readLost is the read of a lost run's box on this node's host, for this
// node's own ending or asked by the node ending it ([BoxReadLoss]): the box
// attached, never woken; its job ended where it still works
// ([Coordinator.endJob]); the release that ran it collected, for the verdict
// alone; its served notes settled off its main thread (mainthread.go); and,
// for a company that keeps sessions, its session taken. The verdict's
// [ErrStoreSilent] is returned before the take; nothing else of the box is read.
func (c *Coordinator) readLost(ctx context.Context, run PendingRun) (*spooledCapture, *SessionKept, error) {
	manager := c.mgr()
	if manager == nil {
		return nil, nil, nil
	}
	box, runner, err := manager.Attach(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		why := fmt.Sprintf("the lost run's box could not be read before it was reclaimed: %v", err)
		sayNotKept(ctx, slog.LevelInfo, executorkeep.LossBoxLost, run.TurnID, "point", string(BoxReadLoss),
			"detail", why)
		return nil, &SessionKept{NotKept: why, Loss: executorkeep.LossBoxLost}, nil
	}
	over := c.endJob(ctx, run, box, runner, nil)
	// THE RELEASE ALONE: a failed Collect leaves it "", which confirms nothing.
	var result Result
	if asksMainThread(run, BoxReadLoss, ThreadTraitsOf(runner)) {
		if collected, err := runner.Collect(ctx, box, run.Handle(), c.answerBudget); err == nil {
			result.HarnessVersion = collected.HarnessVersion
		}
	}
	at := readCapture(BoxReadLoss, result, over)
	asked := c.asksSummary(runner)
	reading := c.readMainThread(ctx, run, box, runner, BoxReadLoss, at, result, asked)
	if err := c.settleReceipts(ctx, run, reading.Receipts, BoxReadLoss); err != nil {
		return nil, nil, err
	}
	if !c.capturing() {
		return nil, nil, nil
	}
	// A MINTING JOB'S SESSION IS BOUND BEFORE ITS LOSS TAKES IT (minted.go): a
	// bind neither made nor refused hands the read back, to be made again.
	if _, err := c.bindMinted(ctx, run, box, runner); undecidedBind(err) {
		return nil, nil, storeSilent(ctx, fmt.Errorf("sandbox: binding lost run %s's session: %w", run.TurnID, err))
	}
	at.summary = c.foundSummary(ctx, run, runner, asked, reading.Summary)
	spooled, kept := c.takeCapture(ctx, run, box, runner, at)
	return spooled, kept, nil
}

// capturesLoss reports whether a lost run's ending reads its box: for its
// session, in a company that keeps sessions ([Coordinator.capturing]), or for
// its served notes' receipts ([PendingRun.asksReceipts]), whatever the company
// keeps; of a run whose job was in a box, by a loss that leaves the box behind
// ([LossCause.leavesBox]). A job already taken whole ([keptWhole]) is read
// again only for its receipts: beside a take still being kept, the read stands
// by for it.
func (c *Coordinator) capturesLoss(run PendingRun) bool {
	receipts := run.asksReceipts()
	if (!c.capturing() && !receipts) || run.SandboxID == "" || !run.lost() || (keptWhole(run) && !receipts) {
		return false
	}
	return run.Lost == nil || run.Lost.Cause.leavesBox()
}

// keepsSessions reports whether this coordinator records on an executor log: an
// engine's, never a test's built without one. It is the WIRING, which every
// turn's end needs whatever the company says; whether a session is worth
// preparing to keep is [Coordinator.capturing].
func (c *Coordinator) keepsSessions() bool { return c.sessions != nil && c.keep != nil }

// capturing reports whether a capture is worth PREPARING: a coordinator that
// records on an executor log, of a company that keeps sessions as this node's
// revision says ([SessionKeep.Keeps]). With the switch off, a launch seals
// nothing for a scrub no capture would make, and a loss asks no other node's
// host to read a box for a take that would keep nothing — holding the seat's
// mail for the minutes a silent host costs. A stop's read is the ending's own
// retry's whatever the switch says, since it ends the job it reads. The take
// itself keeps nothing until the fleet's revision says so
// ([SessionKeep.Policy]).
func (c *Coordinator) capturing() bool { return c.keepsSessions() && c.keep.Keeps() }

// keptWhole reports whether run's row already holds a capture of its current
// job taken once the job had ended that has SETTLED, whatever it came to —
// landed, refused, or its record sent and never answered: nothing since has
// changed the session it holds. One still BEGUN is another read's, still being
// kept, which a read of the job stands by for rather than passes over
// ([Coordinator.takeCapture]): passed over, the loss's read reclaimed the box
// unread beside a keep that could still fail, and nothing kept the session.
func keptWhole(run PendingRun) bool {
	last := run.Capture
	return last != nil && last.Launch == run.LaunchID && last.LaunchSeq == run.LaunchSeq && last.Final &&
		last.State != CaptureBegun
}

// nextCaptureSeq is the next capture's place among the current launch's, from
// what the row's newest capture says.
func nextCaptureSeq(run PendingRun) int64 {
	if last := run.Capture; last != nil && last.LaunchSeq == run.LaunchSeq {
		return last.CaptureSeq + 1
	}
	return 1
}

// MaxScrubBytes bounds what one capture scrubs out by value, in all: what its
// box was handed (by its launch and the poll's renewals, bounded together,
// [MaxHandedBytes]) and its seeded logins read again, in the box and on the
// host ([Coordinator.scrubValues]).
//
// FOUR TIMES WHAT A LAUNCH MAY HAND: a launch at its bound and every login it
// was seeded with read again twice still fits with room to spare, and the
// scrub, which holds every value whole in one replacer, costs the host a few
// mebibytes at most. The box's own code can write its login file, and a file
// of millions of short strings built a replacer of a gibibyte on the engine
// host before a byte of the session was read. A capture past it is refused,
// so it bounds the longest value the executor's scrub holds too.
const MaxScrubBytes = 4 * MaxHandedBytes

// scrubValues is every value a capture of run's box scrubs out by value: what
// its launch handed the box and every token the poll renewed in it since,
// recorded before the poll wrote it ([PendingStore.Handed],
// [PendingStore.AddHanded]), and its seeded logins as they are NOW, in the box
// and on the host, since a refresh mints values the launch never saw.
// [ErrHandedUnknown] where they cannot be known — see handed.go — a set marked
// as holding less than its box among them ([ErrHandedIncomplete]), which
// refuses the capture rather than scrub less; and refused too past its bounds
// — a login read again larger than a whole launch may hand
// ([MaxHandedBytes]), or values past [MaxScrubBytes] in all — since the box's
// code can write its login and the scrub holds every value whole.
func (c *Coordinator) scrubValues(ctx context.Context, run PendingRun, box Sandbox) ([]string, error) {
	sealed, err := c.pending.Handed(ctx, run)
	switch {
	case errors.Is(err, ErrHandedIncomplete):
		return nil, fmt.Errorf("%w: %w", ErrHandedUnknown, err)
	case err != nil:
		return nil, fmt.Errorf("%w: what its box was handed could not be read: %w", ErrHandedUnknown, err)
	}
	if sealed == "" {
		return nil, fmt.Errorf("%w: the run's record keeps none for its job, so its session cannot be "+
			"scrubbed of them", ErrHandedUnknown)
	}
	raw, err := c.keep.OpenHanded(run.TurnID, sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandedUnknown, err)
	}
	handed, err := decodeHanded(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandedUnknown, err)
	}
	values := slices.Clone(handed.Values)
	for _, rel := range slices.Sorted(maps.Keys(handed.Logins)) {
		inBox, err := box.ReadFile(ctx, boxLoginPath(box, rel))
		if err != nil {
			return nil, fmt.Errorf("%w: the login the box was seeded with at %s could not be read "+
				"again in the box: %w", ErrHandedUnknown, rel, err)
		}
		if len(inBox) > MaxHandedBytes {
			return nil, fmt.Errorf("the login the box was seeded with at %s is %d bytes in the box now, "+
				"past the %d a whole launch may hand its box; a login document is a few kibibytes, so "+
				"the box's own code wrote it, and the session is not scrubbed of what it holds",
				rel, len(inBox), MaxHandedBytes)
		}
		values = append(values, loginValuesOf(inBox)...)
		onHost, err := os.ReadFile(handed.Logins[rel]) //nolint:gosec // the entry's own login, at the path its harness keeps it
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("%w: the login the box was seeded from could not be read again on "+
				"this host: %w", ErrHandedUnknown, err)
		default:
			values = append(values, loginValuesOf(onHost)...)
		}
	}
	values = dedupe(values)
	total := 0
	for _, v := range values {
		total += len(v)
	}
	if total > MaxScrubBytes {
		return nil, fmt.Errorf("the values the session would be scrubbed of are %d bytes in all, past "+
			"the %d a capture holds for its scrub, so it is not kept", total, MaxScrubBytes)
	}
	return values, nil
}

// loginValuesOf is [loginValues] of a login read again, nothing for an empty
// one — a box that was not seeded with it, a file since removed.
func loginValuesOf(raw []byte) []string {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return loginValues(raw)
}

// refuseUnplaced stamps a capture refused before it held a place (the session
// could not be read, or the policy or what the box was handed not known), so
// the turn's end and whatever reads the row's newest capture know the launch's
// latest attempt failed rather than reading an older capture as its latest.
//
// AT THE PLACE AFTER THE NEWEST THE TAKE READ, which another read of the same
// job may have reserved since and is keeping: there the stamp is not taken
// ([stampOnto]) and that read's capture stands. UNDER A CONTEXT OF ITS OWN, by
// by at the latest, asked again while the store does not answer
// ([Coordinator.stampSettled]): the take's budget may be what ended it. calls
// is the run's log where the refusal had read it, else nil, and the stamp then
// states no save point, "not known" ([SessionCapture.Through]).
func (c *Coordinator) refuseUnplaced(ctx context.Context, by time.Time, run, current PendingRun,
	at capturePoint, calls *BridgeLog, summary *SummaryStamp, loss executorkeep.Loss, why string,
	attrs ...any) *SessionKept {

	stamp := SessionCapture{LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current),
		Launch: run.LaunchID, State: CaptureRefused, Reason: stampReason(why), Loss: loss, Final: at.final,
		Interrupted: at.cutOff, NoteCursor: notesHeld(current, at.cutOff), At: c.now().UTC(), Summary: summary}
	if calls != nil {
		stamp.placeAt(*calls, current.Launch)
	}
	sayNotKept(ctx, slog.LevelWarn, loss, run.TurnID, append([]any{"point", at.name,
		"capture_seq", stamp.CaptureSeq, "detail", stamp.Reason}, attrs...)...)
	c.stampSettled(ctx, by, run.TurnID, stamp)
	return stamp.kept()
}

// finishCapture stamps how a capture came out and reports it.
//
// UNDER A CONTEXT OF ITS OWN, asked again while the store does not answer
// within a record's budget, and never past by where a take is still stamping it
// ([Coordinator.settleContext]; zero off the read): the capture's budget may be
// what ended it, and the stamp is what says so.
// loss is why it did not land, for any state but landed, which names none.
func (c *Coordinator) finishCapture(ctx context.Context, by time.Time, run PendingRun, stamp SessionCapture,
	state CaptureState, loss executorkeep.Loss, why string) *SessionKept {

	stamp.State, stamp.Reason, stamp.Loss = state, stampReason(why), loss
	if state == CaptureLanded {
		stamp.Loss = ""
	}
	c.stampSettled(ctx, by, run.TurnID, stamp)
	switch state {
	case CaptureLanded:
		carried := 0
		for _, m := range stamp.Media {
			if m.Carried {
				carried++
			}
		}
		log.InfoContext(ctx, "executor_session_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "object", stamp.Object,
			"bytes", stamp.ObjectSize, "media", stamp.mediaBytes(), "media_fresh", len(stamp.Media)-carried,
			"media_carried", carried, "session", stamp.SessionID, "trimmed", stamp.Trimmed)
		c.meterTrim(stamp)
	default:
		sayNotKept(ctx, slog.LevelWarn, loss, run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "state", string(state),
			"detail", stamp.Reason)
	}
	return stamp.kept()
}

// reserveCapture reserves a capture's place on its run's row (its stamp, begun,
// [PendingStore.StampCapture]), asked again while the coordination store does
// not answer ([Coordinator.askStamp]), never past by, as
// [Coordinator.stampSettled] asks an outcome: false, with no error, where
// another read of the same job took the place first.
//
// ASKED AGAIN SO A READ LEAVES A STAMP OF WHAT IT TOOK wherever the store lets
// it: a turn's newest capture is known only by its stamp, and its end tells the
// log it where it was not kept ([unkeptOf]). AND AN EARLIER ASK THE STORE
// APPLIED, its answer lost, is this take's reservation: the store answers a
// stamp the row already holds as taken, so the take goes on under its own
// place rather than walking away from it, begun.
func (c *Coordinator) reserveCapture(ctx context.Context, by time.Time, turnID string,
	stamp SessionCapture) (bool, error) {

	took, _, err := c.askStamp(ctx, by, turnID, stamp, c.pending.StampCapture)
	return took, err
}

// abandonPlace settles the place a take asked to reserve and was never answered
// within its own time ([Coordinator.reserveCapture]). A store that applied the
// reservation and answered too late, or applies it once its asker has given
// up, leaves the place begun, read as a keep still under way
// ([captureInFlight]): the turn's end waits on it with nobody left to settle it.
//
// ONE STAMP, DECIDED ON THE ROW AS IT STANDS when the store writes it: the
// place refused, as THIS take stamped it, at its instant ([stampOnto]). Never
// a read and a decision off it: a reservation landing between the two was left
// begun. A place another read reserved is left as it stands, and the take goes
// on BESIDE that read, answered true ([Coordinator.besideAnother]), since it
// holds the session whole.
func (c *Coordinator) abandonPlace(ctx context.Context, by time.Time, run PendingRun, stamp SessionCapture,
	at capturePoint, cause error) (*SessionKept, bool) {

	refused := stamp
	refused.State, refused.Loss = CaptureRefused, executorkeep.LossKeepFailed
	refused.Reason = stampReason(fmt.Sprintf("the take's time ran out before the store answered the "+
		"reservation of its place on the run's row, so nothing was taken under it: %v", cause))
	took, _, err := c.askStamp(ctx, by, run.TurnID, refused, c.pending.StampCapture)
	switch {
	case took:
		sayNotKept(ctx, slog.LevelWarn, refused.Loss, run.TurnID, "agent", run.AgentHandle,
			"launch_seq", refused.LaunchSeq, "capture_seq", refused.CaptureSeq,
			"state", string(CaptureRefused), "detail", refused.Reason)
		return refused.kept(), false
	case err == nil:
		return nil, true
	}
	sayNotKept(ctx, slog.LevelInfo, refused.Loss, run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "error", errors.Join(cause, err).Error(),
		"detail", "the capture's place could not be settled on the run's row within the read's time; a "+
			"reservation the store took stands begun until it ages out")
	return nil, false
}

// stampSettled stamps a capture's outcome on its run's row, asked again while
// the coordination store does not answer ([Coordinator.askStamp]), under a
// context of its own, which outlives the caller's: the capture's budget may be
// what ended it. Never past by, where a take is still stamping it
// ([Coordinator.settleContext]).
//
// ASKED AGAIN BECAUSE A STAMP LEFT BEGUN IS READ AS A KEEP STILL UNDER WAY for
// as long as a capture can take ([SessionCaptureBudget]): the turn's end waits
// on it for its whole life ([captureLife]). Once the record's budget is spent
// the store's outage is its own alarm's, and the stamp ages out as never
// finished.
func (c *Coordinator) stampSettled(ctx context.Context, by time.Time, turnID string, stamp SessionCapture) {
	c.settleStamp(ctx, by, turnID, stamp, c.pending.StampCapture)
}

// settleStamp is [Coordinator.stampSettled] through write, answering whether
// the row took the stamp: false where the store refused it, or never answered
// within the record's budget, which is said.
func (c *Coordinator) settleStamp(ctx context.Context, by time.Time, turnID string, stamp SessionCapture,
	write stampWrite) bool {

	took, attempts, err := c.askStamp(ctx, by, turnID, stamp, write)
	if err != nil {
		log.WarnContext(ctx, "executor_session_stamp_failed", "turn_id", turnID,
			"capture_seq", stamp.CaptureSeq, "state", string(stamp.State), "attempts", attempts,
			"error", err.Error())
	}
	return took
}

// stampWrite is one write of a capture's stamp on its run's row: the row's own
// rule ([PendingStore.StampCapture]), or a settle's, made only over the begun
// stamp it was decided from ([PendingStore.SettleBegun]).
type stampWrite func(ctx context.Context, turnID string, stamp SessionCapture) (bool, error)

// askStamp stamps a capture on its run's row through write, asked again while
// the coordination store does not answer, spaced by a doubling wait, within one
// record's budget ([sessionRecordBudget]) and the time the row takes to cross
// to the store ([RowBudget]), under a context of its own, never past by
// ([Coordinator.settleContext]): whether the row took it, how many times it was
// asked, and the store's last error where it never answered — the one way of
// asking every stamp of a capture takes.
//
// THE CROSSING TOO, because every stamp sends the row whole: held to the
// record's thirty seconds alone, a row of megabytes across a slow link was cut
// while its bytes were still crossing, and asked again from its first byte
// until the budget ended, never taken (crewlet/crewlet#1076).
func (c *Coordinator) askStamp(ctx context.Context, by time.Time, turnID string,
	stamp SessionCapture, write stampWrite) (bool, int, error) {

	stampCtx, cancel := c.settleContext(ctx, by)
	defer cancel()
	for attempt := 1; ; attempt++ {
		took, err := write(stampCtx, turnID, stamp)
		if err == nil {
			return took, attempt, nil
		}
		wait := time.NewTimer(backoff.Doubling(attempt, c.sessionRetry, sessionRetryCeiling))
		select {
		case <-stampCtx.Done():
			wait.Stop()
			return false, attempt, err
		case <-wait.C:
		}
	}
}

// settleContext is the context a stamp asked again is made under: one of its
// own, which outlives the caller's, within one record's budget and the time the
// run's row takes to cross to the store ([RowBudget]) — and never past by,
// where the read a take is part of needs the rest of its time for the box's
// pause, as [stampContext] bounds a take's single record. by zero, a stamp made
// off the read, is bounded by the record's budget and the crossing alone.
func (c *Coordinator) settleContext(ctx context.Context, by time.Time) (context.Context, context.CancelFunc) {
	free, release := detach(ctx)
	settle, cancel := c.rowContext(free, c.recordBudget)
	return before(settle, func() {
		cancel()
		release()
	}, by)
}

// uploadRefusal is why an upload of a session was refused, in the words its
// stamp keeps and the loss it is.
func uploadRefusal(err error) (executorkeep.Loss, string) {
	switch {
	case errors.Is(err, objstore.ErrFull):
		return executorkeep.LossStoreRefused, "the object store's byte reservation is spent, so the session " +
			"was not uploaded: raise store.objects' reservation or move the objects to a bucket"
	case errors.Is(err, executorkeep.ErrSessionTooLarge):
		return executorkeep.LossTooLarge, tooLargeWhy(err)
	}
	return executorkeep.LossStoreRefused, fmt.Sprintf("the session could not be uploaded: %v", err)
}

// tooLargeWhy is a session refused past a bound, err saying which bound in
// use and by how much: a take's archive past its take's ([takeBound]), its text
// past executorkeep.SessionMaxBytes sealed, or an object past the store's.
func tooLargeWhy(err error) string {
	return fmt.Sprintf("the session is past what one is kept at, %d MiB of text and %d MiB in all at most: %v",
		executorkeep.SessionMaxBytes>>20, executorkeep.SessionKeptMaxBytes>>20, err)
}

// recordCapture records a stamped, uploaded capture of run's turn on the
// executor log under the operation its place derives ([SessionWrites.Capture]),
// asked again within [sessionRecordBudget]: landed, refused (gone: a carry the
// log found gone) or unresolved. EVERY CAPTURE CARRIES THE TURN'S FIRST READ
// ([PendingRun.Base]). A KEEP'S FENCE IS ASKED BEFORE EVERY ATTEMPT
// ([takerLease.fence]; nil for an ask again): fenced, nothing more is sent,
// refused where no attempt had been sent, else unresolved, which a wait asks
// again rather than settle refused under a record that may have landed. EVERY
// ATTEMPT IS CUT AT THE END OF THE WINDOW IT WAS SENT IN: held to the record's
// budget alone, one could land after a peer settled the capture refused.
func (c *Coordinator) recordCapture(ctx context.Context, run PendingRun, stamp SessionCapture,
	fence func() (time.Time, string)) (outcome CaptureState, why string, fenced, gone bool) {

	handle, turn := run.AgentHandle, run.TurnID
	key, err := objstore.ParseKey(stamp.Object)
	if err != nil {
		return CaptureRefused, fmt.Sprintf("the capture names no object a record can: %v", err), false, false
	}
	record := CaptureRecord{
		Handle: handle, Turn: turn, ConversationKey: run.ConversationKey, WorkKey: run.WorkKey,
		Object: objstore.Object{Key: key, Hash: objstore.Hash(stamp.ObjectHash), Size: stamp.ObjectSize},
		Stamp:  stamp,
	}
	if read := run.Base; read != nil && read.ReadThrough > 0 {
		record.ReadThrough, record.Absorbs = read.ReadThrough, read.Absorbs
	}
	sent := false
	c.askRecord(ctx, "capture", turn, c.recordBudget, func(ctx context.Context) (bool, error) {
		attempt := ctx
		if fence != nil {
			through, halt := fence()
			if halt != "" {
				outcome, why, fenced = CaptureRefused, halt, true
				if sent {
					outcome = CaptureUnresolved
				}
				return true, nil
			}
			if !through.IsZero() {
				var cancel context.CancelFunc
				attempt, cancel = context.WithDeadline(ctx, through)
				defer cancel()
			}
		}
		sent = true
		answer, err := c.sessions.Capture(attempt, record)
		switch answer {
		case LogLanded:
			outcome = CaptureLanded
			return true, nil
		case LogPassedOver:
			outcome, why = CaptureRefused, "the log applied the capture and the turn's row names "+
				"another: an end, a newer capture, the seat's removal or the seat's budget got there first"
			return true, nil
		case LogRefused:
			outcome, why, gone = CaptureRefused, fmt.Sprint(err), errors.Is(err, ErrCarryGone)
			return true, nil
		}
		outcome, why = CaptureUnresolved, fmt.Sprint(err)
		return false, err
	})
	// A MINTED SESSION'S CLAIM GOES ONCE THE LOG NAMES IT (minted.go): the
	// log's seek guards the id from here, so the claim is no longer needed.
	if id := stamp.SessionID; outcome == CaptureLanded && id != "" && id == run.Launch.Minted {
		if err := c.pending.ReleaseSessionClaim(context.WithoutCancel(ctx), turn, id); err != nil {
			log.InfoContext(ctx, "executor_session_claim_kept", "turn_id", turn, "session", id,
				"error", err.Error(), "detail", "the claim goes with the run's record instead")
		}
	}
	return outcome, why, fenced, gone
}

// readersBelow is why a capture naming media, or one taken at the kept bound,
// is not recorded, "" where it may be or is neither: who reads it asked again
// before its record, in a room of its own ([MediaReadersRoom], counted in
// [keepBudgetAt]), since a node below the version that joined after the take
// would retain it, ask it again short, or read a keep at the kept bound as
// abandoned. ONLY A DEFINITE ANSWER REFUSES, naming the node
// ([SessionPolicy.MediaBelow], [SessionPolicy.KeptBelow]). One it cannot get
// (the readers or the policy unread) is no evidence of such a joiner, and a
// policy keeping nothing now asks no reader: each goes on, on the take's
// answer, said apart, rather than lose what is already uploaded.
func (c *Coordinator) readersBelow(ctx context.Context, run PendingRun, stamp SessionCapture) string {
	if len(stamp.Media) == 0 && !stamp.KeptBound {
		return ""
	}
	read, cancel := context.WithTimeout(ctx, MediaReadersRoom)
	policy, err := c.keep.Policy(read)
	cancel()
	var kind, why string
	switch {
	case err != nil:
		kind, why = "unread", fmt.Sprintf("the company's policy could not be read: %v", err)
	case !policy.Keep:
		kind, why = "keep_off", "the company keeps no session now, so who reads the capture was not asked "+
			"again, and it is uploaded already"
	case policy.MediaUnread != "":
		kind, why = "unread", policy.MediaUnread
	case len(stamp.Media) > 0 && !policy.MediaApart:
		below := cmp.Or(policy.MediaBelow, "a node that applies the executor log or settles its runs says a "+
			"version below the one a capture naming media needs")
		return below + ", so it no longer reads a capture naming media, and the capture, its media kept " +
			"apart, was not recorded; the next take keeps them inline"
	case stamp.KeptBound && !policy.kept():
		below := cmp.Or(policy.KeptBelow, policy.MediaBelow, "a node that settles its runs says a version "+
			"below the one a capture at the kept bound needs")
		return below + fmt.Sprintf(", so it no longer reads a capture kept past %d MiB, and the capture was "+
			"not recorded; the next take keeps the %d MiB bound", executorkeep.SessionMaxBytes>>20,
			executorkeep.SessionMaxBytes>>20)
	default:
		return ""
	}
	log.InfoContext(ctx, "executor_media_readers_unread", "turn_id", run.TurnID, "kind", kind, "why", why,
		"detail", "the capture is recorded on its take's answer that every node reads it")
	return ""
}

// beginRecordBudget bounds the record of a turn's begin: every attempt at it,
// under its one operation, together.
//
// FIFTEEN SECONDS, on the frame that launches: that frame holds the seat and a
// slot of the node's admission, so it does not wait for the log the way an
// end's own retry does ([sessionRecordBudget]). Three of the publisher's
// five-second waits (statelog.DefaultResolveBudget): a write the log takes
// answers in milliseconds, and three waits are room for a store having a slow
// moment; a relaunch's asks are held to the same ([relaunchAskWait]). A begin
// not held by then is one a slow log or a stopped node is holding: the launch
// continues no base, and a begin that lands later holds no longer than the
// turn's first capture or its end.
const beginRecordBudget = 15 * time.Second

// recordBegin records run's turn's first read of its conversation on the
// executor log (the read its first launch froze, [PendingRun.Base]) as the
// turn's begin ([SessionWrites.Begin]), under the one operation the turn
// derives, asked again within [beginRecordBudget]. Logged, never failed on: a
// begin the log does not hold leaves the launches continuing no base
// ([BaseContinuityOf]), as the turn's own row says.
//
// It answers whether the log read the turn's row back holding the base
// ([BeginAnswer.Held]), read on the serving data node once the begin applied,
// which is what a read of the row would say now (nothing but the turn's first
// capture or its end lifts a hold, and neither precedes this launch's run), so
// the launch takes the base up on it ([Coordinator.continueBase]).
func (c *Coordinator) recordBegin(ctx context.Context, req LaunchRequest, opened PendingRun) bool {
	read := opened.Base
	if !c.keepsSessions() || read == nil || read.ReadThrough <= 0 || req.Turn.ConversationKey == "" {
		return false
	}
	minted, err := c.keep.MintedAt(ctx, req.Turn.TurnID, req.Turn.WorkKey, req.Turn.WorkSince)
	if err != nil {
		log.WarnContext(ctx, "executor_begin_not_recorded", "turn_id", req.Turn.TurnID,
			"agent", req.Turn.AgentHandle, "error", err.Error(),
			"detail", "the turn's operations could not be minted, so its first read is not held on the "+
				"executor log; its launches continue no session its conversation's last turn left")
		return false
	}
	record := BeginRecord{Handle: req.Turn.AgentHandle, Turn: req.Turn.TurnID,
		ConversationKey: req.Turn.ConversationKey, WorkKey: req.Turn.WorkKey, Minted: minted,
		ReadThrough: read.ReadThrough, Absorbs: read.Absorbs}
	if base := read.Session; base != nil {
		key, keyErr := objstore.ParseKey(base.Object)
		if keyErr != nil {
			log.WarnContext(ctx, "executor_begin_not_recorded", "turn_id", req.Turn.TurnID,
				"agent", req.Turn.AgentHandle, "error", keyErr.Error(),
				"detail", "the base the turn's first read froze names no object a record can")
			return false
		}
		record.BaseTurn = base.Turn
		record.BaseObject = objstore.Object{Key: key, Hash: objstore.Hash(base.ObjectHash), Size: base.ObjectSize}
	}
	var (
		held    bool
		why     string
		settled bool
	)
	c.askRecord(ctx, "begin", req.Turn.TurnID, c.beginBudget, func(ctx context.Context) (bool, error) {
		res, err := c.sessions.Begin(ctx, record)
		switch res.Log {
		case LogLanded:
			// APPLIED, or nothing to publish: the turn's first read was
			// recorded already, by an earlier attempt at this begin or a
			// capture that landed first. Either way the row, read back,
			// says what it holds.
			held, why, settled = res.Held, res.Why, true
			return true, nil
		case LogRefused:
			why, settled = fmt.Sprint(err), true
			return true, nil
		}
		why = fmt.Sprint(err)
		return false, err
	})
	switch {
	case !settled:
		log.InfoContext(ctx, "executor_begin_unanswered", "turn_id", req.Turn.TurnID,
			"agent", req.Turn.AgentHandle, "detail", redact.Secrets(why),
			"outcome", "the launch continues no session its conversation's last turn left; the begin may "+
				"still land, and holds no longer than the turn's first capture or its end")
	case record.BaseTurn != "" && !held:
		log.InfoContext(ctx, "executor_begin_holds_no_base", "turn_id", req.Turn.TurnID,
			"agent", req.Turn.AgentHandle, "base_turn", record.BaseTurn, "detail", redact.Secrets(why),
			"outcome", "the turn's launches continue no session its conversation's last turn left")
	}
	return settled && held
}

// askRecord asks one record of the log until it is settled — try answers true —
// or budget runs out, spaced by a doubling wait.
func (c *Coordinator) askRecord(ctx context.Context, what, turn string, budget time.Duration,
	try func(context.Context) (bool, error)) {

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		settled, err := try(ctx)
		if settled {
			return
		}
		wait := backoff.Doubling(attempt, c.sessionRetry, sessionRetryCeiling)
		log.InfoContext(ctx, "executor_record_unanswered", "turn_id", turn, "record", what,
			"attempt", attempt, "error", fmt.Sprint(err),
			"detail", "the executor log did not settle the record; it is asked again under the same "+
				"operation, which lands it once however often it is asked")
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// settleCapture asks again, under its own operation, a capture whose record was
// sent and not answered, and answers what the log answers: landed, refused, or
// still unresolved. The ask is held to ctx; the stamp of a settling answer is
// this node's own write in its own room ([Coordinator.settleContext]), so an
// answer the log gave is not lost. AN ASK THAT LEAVES THE RECORD UNANSWERED
// STAMPS NOTHING: the row holds it unresolved already ([stampOnto]). ASKED
// WHATEVER ITS KEY'S AGE READS ON THIS NODE'S CLOCK: the key was minted on the
// spooling node's clock, and the log refuses an object past
// objstore.RecordWithin by its own (internal/executordomain's ErrUploadStale),
// held here only within twice objstore.AssumedClockSkew; refused unasked, a
// record the log would have taken never landed. The log's refusal is definite,
// so never asked again; the collector's grace outlasts the window.
func (c *Coordinator) settleCapture(ctx context.Context, run PendingRun,
	last SessionCapture) SessionCapture {

	// A STAMP A NEWER BUILD WROTE KEYS INTO this one cannot rebuild its
	// record from: asked again short, the log would name less than the take
	// kept (a media list, once); refused unasked, a record that landed would
	// read lost. So it is settled off the log ([Coordinator.settleOffLog]).
	if len(last.Extra) > 0 {
		return c.settleOffLog(ctx, run, last)
	}
	last.State, last.Reason, _, _ = c.recordCapture(ctx, run, last, nil)
	last.Reason, last.Loss = stampReason(last.Reason), executorkeep.LossLogRefused
	if last.State == CaptureLanded {
		last.Loss = ""
	}
	if last.State != CaptureUnresolved {
		c.stampSettled(ctx, time.Time{}, run.TurnID, last)
	}
	return last
}

// settleOffLog settles an unanswered capture this build cannot ask again whole
// (its stamp holds keys a newer build wrote, [SessionCapture.Extra]) off ONE
// read of the turn's row on the executor log: LANDED where the row names its
// place, written from the row ([landedFrom]); REFUSED where it does not and
// its object is past the window the log records one within
// ([pastRecordWithin]), so no ask of it could land; else left unresolved,
// stamping nothing, for the build that wrote it to ask again. Held to ctx; its
// stamp to its own room ([Coordinator.stampSettled]).
func (c *Coordinator) settleOffLog(ctx context.Context, run PendingRun, last SessionCapture) SessionCapture {
	if c.restore == nil {
		return last
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, run.TurnID)
	switch {
	case err == nil && row.LaunchSeq == last.LaunchSeq && row.CaptureSeq == last.CaptureSeq:
		last = landedFrom(last, row)
	case (err == nil || errors.Is(err, ErrTurnRowNotFound)) && pastRecordWithin(last.Object, c.now()):
		last.State, last.Loss = CaptureRefused, executorkeep.LossLogRefused
		last.Reason = stampReason(fmt.Sprintf("the capture was stamped by a newer build, with %s, which "+
			"this build cannot ask again; the turn's row on the executor log does not name it, and its "+
			"object is past the window the log records one within, so it never lands",
			words.And(slices.Sorted(maps.Keys(last.Extra)))))
	default:
		return last
	}
	c.stampSettled(ctx, time.Time{}, run.TurnID, last)
	log.InfoContext(ctx, "executor_session_settled", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"launch_seq", last.LaunchSeq, "capture_seq", last.CaptureSeq, "state", string(last.State),
		"detail", "a newer build stamped the capture with keys this build cannot ask its record again with, "+
			"so what came of it was read off the turn's row on the executor log")
	return last
}

// landedFrom is settled stamped LANDED off row, the turn's row on the executor
// log that names its place: the object, media and context the row holds, as
// the record that landed wrote them.
func landedFrom(settled SessionCapture, row executorkeep.TurnRow) SessionCapture {
	settled.State, settled.Reason, settled.Loss = CaptureLanded, "", ""
	if row.HasObject() {
		settled.Object, settled.ObjectHash = row.Object.Key.String(), string(row.Object.Hash)
		settled.ObjectSize, settled.ContentKey = row.Object.Size, row.ContentKey
		settled.Media, settled.ArchiveBytes = rowMedia(row.Media, settled.Media), row.ArchiveBytes
	}
	settled.ContextTokens = row.ContextTokens
	return settled
}

// pastRecordWithin reports whether a capture's object, named by object, is past
// objstore.RecordWithin and twice objstore.AssumedClockSkew as of now: past the
// window by any node's clock that the engine holds within two hours of now's, so
// the log, which decides the window on its own clock, refuses a record naming
// it, and no reader waits on that record's answer ([captureAwaitsAnswer]).
// Nothing refuses an ask by it: the ask is the log's to decide
// ([Coordinator.settleCapture]). An object whose key does not parse is not.
func pastRecordWithin(object string, now time.Time) bool {
	key, err := objstore.ParseKey(object)
	return err == nil && now.Sub(key.Minted()) > objstore.RecordWithin+2*objstore.AssumedClockSkew
}

// settleUnresolved asks again, each under its own operation
// ([Coordinator.settleCapture]), every stamp of run's turn in sight whose
// record was sent and never answered and that asks admits: its newest capture
// and its save under way ([PendingRun.savingTake]), which beside a refused
// final take is the job's newest session anybody kept. Each ask is held to
// ctx, its stamp to its own room. Asked by the turn's end before it records
// ([Coordinator.publishEnd]) and by a relaunch before it decides
// ([Coordinator.ContinuedCapture]). It answers the row as it stands after,
// what the asks settled stamped on run by the row's own rule ([stampOnto])
// where the row cannot be read back, so an end never names one just landed as
// unanswered.
func (c *Coordinator) settleUnresolved(ctx context.Context, run PendingRun,
	asks func(SessionCapture) bool) PendingRun {

	var settled []SessionCapture
	for _, stamp := range []*SessionCapture{run.Capture, run.savingTake()} {
		if stamp != nil && stamp.State == CaptureUnresolved && asks(*stamp) {
			settled = append(settled, c.settleCapture(ctx, run, *stamp))
		}
	}
	if len(settled) == 0 {
		return run
	}
	if current, found, err := c.pending.Get(ctx, run.TurnID); err == nil && found {
		return current
	}
	for _, stamp := range settled {
		stampOnto(&run, stamp)
	}
	return run
}

// askEvery admits every stamp to an ask ([Coordinator.settleUnresolved]) or a
// settle ([Coordinator.settleGone]): the turn's end asks and settles every
// stamp of its turn before it records, and a relaunch's held read every one.
func askEvery(SessionCapture) bool { return true }

// takerStoppedWhy is why a capture whose taker stopped before its record landed
// is refused, as its stamp and the turn's end say it.
func takerStoppedWhy(taker string) string {
	return fmt.Sprintf("its taker %s stopped before it was kept", nodeOf(taker))
}

// settleGone settles every begun stamp of run's turn in sight (the newest
// capture and a save under way) that admits and whose taker is gone
// ([Coordinator.takerGone]), and, where readHeld, LANDED only, every such stamp
// whose taker is not read gone, from ONE linearizable read of the turn's row on
// the executor log: LANDED where the row names its place, written from the
// row; a gone taker's REFUSED where it does not. Each only over the begun stamp
// it was decided from ([PendingStore.SettleBegun]). It answers the row after,
// false where gone, and the begun stamps the row named, as read, whether or not
// the store took their settle. Reads are held to ctx; each settle write is
// given its own room ([Coordinator.settleContext]). The package doc, "A
// capture whose taker stopped is settled from the log, not waited out", has
// the argument.
func (c *Coordinator) settleGone(ctx context.Context, run PendingRun, admits func(SessionCapture) bool,
	readHeld bool) (PendingRun, bool, []SessionCapture) {

	if c.restore == nil {
		return run, true, nil
	}
	type tenure struct {
		taker string
		epoch int64
	}
	type settling struct {
		begun SessionCapture
		gone  bool
	}
	// gone is what one read of each taker lease said: gone, or not, a lease
	// held and one the store could not say of alike.
	gone := map[tenure]bool{}
	var settle []settling
	for _, stamp := range []*SessionCapture{run.Capture, run.savingTake()} {
		if stamp == nil || stamp.State != CaptureBegun || !admits(*stamp) {
			continue
		}
		key := tenure{stamp.Taker, stamp.TakerEpoch}
		isGone, read := gone[key]
		if !read {
			said, err := c.takerGone(ctx, *stamp)
			isGone = err == nil && said
			gone[key] = isGone
		}
		switch {
		case isGone:
			settle = append(settle, settling{begun: *stamp, gone: true})
		case readHeld && stamp.Taker != "":
			settle = append(settle, settling{begun: *stamp})
		}
	}
	if len(settle) == 0 {
		return run, true, nil
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, run.TurnID)
	if err != nil && !errors.Is(err, ErrTurnRowNotFound) {
		return run, true, nil
	}
	var took, named []SessionCapture
	for _, s := range settle {
		settled := s.begun
		detail := "the lease the capture's taker kept it under has lapsed or moved on, so what came of it " +
			"was read off the turn's row on the executor log rather than waited for"
		names := err == nil && row.LaunchSeq == settled.LaunchSeq && row.CaptureSeq == settled.CaptureSeq
		switch {
		case names:
			named = append(named, s.begun)
			settled = landedFrom(settled, row)
			if !s.gone {
				detail = "the turn's row on the executor log names the capture, so its record landed and it " +
					"was settled from that row while the lease its taker kept it under was not read gone"
			}
		case s.gone:
			settled.State, settled.Reason = CaptureRefused, stampReason(takerStoppedWhy(s.begun.Taker))
			settled.Loss = executorkeep.LossKeepFailed
		default:
			// A LIVE TAKER'S KEEP THE LOG HAS NOT TAKEN: it may still send
			// its record, so it is waited for as before.
			continue
		}
		if !c.settleStamp(ctx, time.Time{}, run.TurnID, settled, c.pending.SettleBegun) {
			continue
		}
		took = append(took, settled)
		log.InfoContext(ctx, "executor_session_settled", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", settled.LaunchSeq, "capture_seq", settled.CaptureSeq, "taker", nodeOf(s.begun.Taker),
			"taker_epoch", s.begun.TakerEpoch, "state", string(settled.State), "detail", detail)
	}
	current, found, getErr := c.pending.Get(ctx, run.TurnID)
	switch {
	case getErr != nil:
		// WHAT THIS SETTLED, where the row cannot be read back to show it, as
		// [Coordinator.settleUnresolved] keeps what its asks settled, and only
		// a settle the store took: one it did not may have found the row moved
		// on, and the wait's next pass reads the row again.
		for _, settled := range took {
			stampOnto(&run, settled)
		}
		return run, true, named
	case !found:
		return PendingRun{}, false, named
	}
	return current, true, named
}

// standDownGone withdraws the read standing by for run's newest capture where
// its taker is gone ([Coordinator.takerGone]) — its lease lapsed, or held at
// another epoch — as [Coordinator.settleGone] settles a begun capture
// whose taker is: a process that stopped keeps nothing it stood by to keep, so
// a wait holds for it no longer than its lease's TTL and one read. It answers
// the row as it then stands, and whether it is still there; a standby whose
// taker holds, or cannot be asked about, is left as it stands. Its reads are
// held to ctx, the wait's; its withdrawal, this node's own write, to its own
// room past it ([Coordinator.settleContext]).
func (c *Coordinator) standDownGone(ctx context.Context, run PendingRun) (PendingRun, bool) {
	newest := run.Capture
	if newest == nil || !newest.standing(c.now()) {
		return run, true
	}
	standby := newest.standbyOf()
	if gone, err := c.takerGone(ctx, standby); err != nil || !gone {
		return run, true
	}
	took := c.settleStamp(ctx, time.Time{}, run.TurnID, standby, c.pending.StandDown)
	if took {
		log.InfoContext(ctx, "executor_session_stood_down", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", standby.LaunchSeq, "capture_seq", standby.CaptureSeq, "taker", nodeOf(standby.Taker),
			"taker_epoch", standby.TakerEpoch, "detail", "the lease the read standing by for the capture was "+
				"taken under has lapsed or moved on, so it was withdrawn rather than waited for")
	}
	current, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		// WHAT THIS WITHDREW, where the row cannot be read back to show it,
		// as [Coordinator.settleGone] keeps what it settled: past the
		// wait's end, where its write may land, the read back is cut, and a
		// copy still naming the standby held the wait for a read that keeps
		// nothing.
		if took {
			standDownOnto(&run, standby)
		}
		return run, true
	case !found:
		return PendingRun{}, false
	}
	return current, true
}

// captureInFlight reports whether run's turn has a capture still in flight as
// of now, which the turn's end waits for ([Coordinator.awaitCapture]): see
// [stampInFlight].
func captureInFlight(run PendingRun, now time.Time) bool {
	return stampInFlight(run, now, anyStamp) != nil
}

// anyStamp admits every stamp of a turn: the turn's end waits for each
// ([captureInFlight]).
func anyStamp(SessionCapture) bool { return true }

// stampInFlight is the stamp of run's turn still in flight as of now that
// admits takes, nil for none: its newest capture, being kept
// ([SessionCapture.keeping]) or stood by for ([SessionCapture.standing]), since
// a read standing by keeps the session where the capture did not land; else a
// save under way being kept ([PendingRun.saving]), which a final take overtook
// and which is the job's newest session where that take is not kept. A save a
// kept capture superseded, and one whose record went unanswered, are not in
// flight: the first can change nothing, the second is asked, not waited for.
// The turn's end admits every stamp ([anyStamp]); a relaunch only those its
// decision depends on ([relaunchWaitsOn]).
func stampInFlight(run PendingRun, now time.Time, admits func(SessionCapture) bool) *SessionCapture {
	if in := stampsInFlight(run, now, admits); len(in) > 0 {
		return in[0]
	}
	return nil
}

// stampsInFlight is every stamp [stampInFlight] may answer, in its order: a
// relaunch waits as long as the longest of them needs ([Coordinator.relaunchWaitOf]).
func stampsInFlight(run PendingRun, now time.Time, admits func(SessionCapture) bool) []*SessionCapture {
	var in []*SessionCapture
	if newest := run.Capture; newest != nil && (newest.keeping(now) || newest.standing(now)) && admits(*newest) {
		in = append(in, newest)
	}
	if save := run.saving(now); save != nil && save.keeping(now) && admits(*save) {
		in = append(in, save)
	}
	return in
}

// awaitCapture is run's row once no capture of its turn is in flight by the
// caller's rule, inFlight, read again every [captureWaitTick] for at most
// within, and whether it is still there; or why the wait could not say (ctx
// ended, or the row unread when the wait ran out). Every pass settles a gone
// taker's capture from the log and withdraws a gone standby, and, paced by
// [heldRowPace], settles landed a held taker's capture the log took
// ([Coordinator.settleGone]). within bounds the waiting and every read; each
// settle write has its own room past it. A read that fails is no answer, and
// the wait goes on; an end is never recorded from a row it could not read.
// Past within on a row it read, the row is answered as it stands. The package
// doc, "A capture whose taker stopped is settled from the log", has the rest.
func (c *Coordinator) awaitCapture(ctx context.Context, run PendingRun, within time.Duration,
	inFlight func(PendingRun, time.Time) bool) (PendingRun, bool, error) {

	// NEVER LONGER THAN within, whatever the stamp's clock says: a stamp whose
	// node's clock runs ahead is waited out on this one's. Its reads and the
	// lease's are held to it; a settle's write is not (see above).
	wait, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	heldRead := time.Now()
	var unread error
	for {
		if unread == nil {
			readHeld := beganUnderTaker(run) && time.Since(heldRead) >= heldRowPace
			if readHeld {
				heldRead = time.Now()
			}
			var found bool
			if run, found, _ = c.settleGone(wait, run, askEvery, readHeld); !found {
				return PendingRun{}, false, nil
			}
			if run, found = c.standDownGone(wait, run); !found {
				return PendingRun{}, false, nil
			}
		}
		waiting := inFlight(run, c.now())
		switch {
		case !waiting && unread == nil:
			return run, true, nil
		case !waiting:
			return run, true, fmt.Errorf("sandbox: run %s could not be read to see what a capture of "+
				"its turn came to: %w", run.TurnID, unread)
		}
		tick := time.NewTimer(c.captureTick)
		select {
		case <-wait.Done():
			tick.Stop()
			switch {
			case ctx.Err() != nil:
				return run, true, fmt.Errorf("sandbox: waiting for a capture of run %s to settle: %w",
					run.TurnID, ctx.Err())
			case unread != nil:
				return run, true, fmt.Errorf("sandbox: run %s could not be read while a capture of its "+
					"turn was being kept: %w", run.TurnID, unread)
			}
			return run, true, nil
		case <-tick.C:
		}
		current, found, err := c.pending.Get(wait, run.TurnID)
		switch {
		case err != nil:
			unread = err
			continue
		case !found:
			return PendingRun{}, false, nil
		}
		run, unread = current, nil
	}
}

// awaitEndCaptures is run's row once its turn's end may be recorded over it, as
// [Coordinator.awaitCapture] answers it: each begun capture the end sees waited
// for at most [captureLife], each read standing by at most [standbyWindow],
// EACH FROM THE MOMENT THE END FIRST SAW IT on this node's own clock
// ([endSightings]), never from the end's start (a standby can first appear
// after it began) nor a stamp's instant ([SessionCaptureBudget],
// [standbyBudget] still bound what counts as in flight across retries). One
// pass at least, which settles a begun capture past its age whose taker is
// gone. WHY AN END WAITS: it tombstones its turn, so a capture recorded after
// it is refused. At most a life and a read ([captureReadBudget]), 6,780.08 +
// 270 = 7,050.08 s (1 h 57.5 min), or for a standby 11,732.21 + 270 + 7,050.08
// = 19,052.28 s (5 h 17.5 min): its window, a read, its takeover's life, a read.
func (c *Coordinator) awaitEndCaptures(ctx context.Context, run PendingRun) (PendingRun, bool, error) {
	seen := endSightings{}
	within := captureLife
	if until := seen.until(run, c.now()); !until.IsZero() {
		within = time.Until(until)
	}
	for {
		waited, found, err := c.awaitCapture(ctx, run, within, captureInFlight)
		if err != nil || !found {
			return waited, found, err
		}
		run = waited
		if !captureInFlight(run, c.now()) {
			return run, true, nil
		}
		if within = time.Until(seen.until(run, c.now())); within > 0 {
			continue
		}
		// PAST EVERY BOUND, the row is read once more and decided on: a round's
		// last read is issued up to a tick before its bound, so a takeover landing
		// in the window's last instants was never seen, and the end would
		// tombstone the turn and have the takeover refused. What this read shows
		// in flight is a sighting like any other. A failed read is no answer: the
		// end is not recorded off the rounds' copy. A read's room, never a
		// change's ([captureReadBudget]).
		read, cancel := c.rowReadContext(ctx, c.recordBudget)
		current, found, err := c.pending.Get(read, run.TurnID)
		cancel()
		switch {
		case err != nil:
			return run, true, fmt.Errorf("sandbox: run %s could not be read once more as the turn's end "+
				"stopped waiting for a capture of its turn: %w", run.TurnID, err)
		case !found:
			return PendingRun{}, false, nil
		}
		run = current
		if within = time.Until(seen.until(run, c.now())); within <= 0 {
			return run, true, nil
		}
	}
}

// endSighting is one thing a turn's end waits on ([endSightings]): a begun
// capture by its place, its launch, its instant and the taker lease it names,
// or a read standing by for one by the same place and launch, the instant the
// read stood by and its own taker lease. Each instant as Unix nanoseconds: a
// time read off the wire and one held in memory differ as map keys.
type endSighting struct {
	launchSeq, captureSeq int64
	launch                string
	at                    int64
	taker                 string
	takerEpoch            int64
	standby               bool
}

// endSightings is when a turn's end first saw each thing it waits on, on this
// node's monotonic clock ([Coordinator.awaitEndCaptures]).
type endSightings map[endSighting]time.Time

// until notes what of run is in flight as of now ([captureInFlight]) that s has
// not seen yet, as first seen at this moment, and answers the latest moment the
// end still holds for any of what is in flight now: a begun capture its whole
// life ([captureLife]), a save under way being kept ([PendingRun.saving]) as
// much as the newest, and a read standing by its window ([standbyWindow]),
// each from when it was first seen. Zero where nothing is in flight. One first
// seen during a round is noted at the round's end, later than it appeared,
// which only holds the end the longer.
func (s endSightings) until(run PendingRun, now time.Time) time.Time {
	var until time.Time
	hold := func(sighted endSighting, bound time.Duration) {
		first, seen := s[sighted]
		if !seen {
			first = time.Now()
			s[sighted] = first
		}
		if end := first.Add(bound); end.After(until) {
			until = end
		}
	}
	if save := run.saving(now); save != nil && save.keeping(now) {
		hold(endSighting{launchSeq: save.LaunchSeq, captureSeq: save.CaptureSeq, launch: save.Launch,
			at: save.At.UnixNano(), taker: save.Taker, takerEpoch: save.TakerEpoch}, captureLife)
	}
	c := run.Capture
	if c == nil {
		return until
	}
	if c.keeping(now) {
		hold(endSighting{launchSeq: c.LaunchSeq, captureSeq: c.CaptureSeq, launch: c.Launch,
			at: c.At.UnixNano(), taker: c.Taker, takerEpoch: c.TakerEpoch}, captureLife)
	}
	if c.standing(now) {
		hold(endSighting{launchSeq: c.LaunchSeq, captureSeq: c.CaptureSeq, launch: c.Launch,
			at: c.StandbyAt.UnixNano(), taker: c.StandbyTaker, takerEpoch: c.StandbyTakerEpoch, standby: true},
			standbyWindow)
	}
	return until
}

// unkeptOf is the turn's newest capture where it is not one the turn's row on
// the executor log names ([executorkeep.Unkept]): refused, never answered,
// or begun and never finished — and newer than the one that landed, if any.
// Nil where the newest landed, or the turn took none.
func unkeptOf(run PendingRun) *executorkeep.Unkept {
	newest := run.Capture
	if newest == nil || newest.State == CaptureLanded {
		return nil
	}
	if kept := run.Kept; kept != nil && !newest.newerThan(*kept) {
		return nil
	}
	why, loss := newest.Reason, newest.Loss
	switch newest.State {
	case CaptureUnresolved:
		why, loss = "its record was sent to the executor log and never answered", executorkeep.LossLogRefused
	case CaptureBegun:
		why, loss = "it was begun and never finished: the node taking it stopped, or its budget ran out",
			executorkeep.LossKeepFailed
	}
	return &executorkeep.Unkept{LaunchSeq: newest.LaunchSeq, CaptureSeq: newest.CaptureSeq, Loss: loss,
		Reason: redact.Secrets(why)}
}

// wentOnPast reports whether the turn went on past the session its row on the
// executor log names ([executorkeep.Ending.WentOn]): the turn took a capture,
// and run's record does not hold its LAST WORD kept ([lastWord]).
//
// ON WHAT THE RECORD DID NOT KEEP, NOT ONLY ON WHAT IT KEPT: a capture the
// record holds refused can be the log's all the same (its taker handed it on
// before [Coordinator.settleGone] read the row), so the row may name a session
// older than the newest capture the end names unkept. So EVERY UNKEPT CAPTURE
// AN END NAMES RIDES AN END THAT SAYS THE TURN WENT ON ([unkeptOf]), and the
// log's apply never takes the row's session off for it. The end says what the
// record knew; the log, which alone knows what it took, settles which session
// the turn went on past ([lastWordOf]).
func wentOnPast(run PendingRun) bool {
	newest, kept := run.Capture, run.Kept
	return newest != nil && (kept == nil || newest.newerThan(*kept) || !lastWord(run, *kept))
}

// lastWordOf is the place of run's newest capture where that capture is its
// turn's LAST WORD ([lastWord]) and the end says the turn went on past its
// row's session ([wentOnPast]) — so where the record did not keep it: refused,
// never answered, or begun and never finished ([executorkeep.Ending.LastWord]).
// Nil anywhere else: no capture, a last word kept, or a newest capture that is
// not the last word.
//
// WHAT A READER HOLDS THE ROW'S PLACE TO (internal/executordomain's
// Session.WentOn): a row whose place is the last word's holds all the turn did.
// NAMED ONLY BESIDE WENT_ON: a last word the record kept leaves an end
// without went_on, so the field stamps no end at a version went_on had not
// stamped it at.
func lastWordOf(run PendingRun) *executorkeep.CapturePlace {
	newest := run.Capture
	if newest == nil || !wentOnPast(run) || !lastWord(run, *newest) {
		return nil
	}
	return &executorkeep.CapturePlace{LaunchSeq: newest.LaunchSeq, CaptureSeq: newest.CaptureSeq}
}

// lastWord reports whether capture is its turn's LAST WORD: the final capture
// of the last launch of the turn whose job ran — the row's own launch, or,
// where that launch's job never started (a relaunch whose harness would not
// start, one a stop ended while it launched), the launch before it. Nothing of
// a job is taken once a take found it ended ([Coordinator.takeCapture]), and a
// job that never started worked no session past it.
//
// THE LAUNCH BEFORE, AND NO EARLIER: a launch whose job never started is its
// turn's last — [Coordinator.Launch] abandons a run whose job would not start,
// and the seat's recovery reaps a launching row its last holder left — so one
// is never followed by another. Were it ever, the turn would say it went on
// past a session it may not have: the direction that costs least.
func lastWord(run PendingRun, capture SessionCapture) bool {
	switch {
	case !capture.Final:
		return false
	case capture.Launch == run.LaunchID:
		return true
	}
	return !run.Launch.Started && capture.LaunchSeq == run.LaunchSeq-1
}

// TurnEnd is how one turn that launched an executor ended, for the executor
// log's `end` ([Coordinator.EndTurn]).
type TurnEnd struct {
	Handle          string
	TurnID          string
	ConversationKey string
	WorkKey         string

	// WorkSince is when the turn's unit of work began, which its operations
	// are minted from ([SessionKeep.MintedAt]).
	WorkSince time.Time

	// Ask is what the turn was asked, as a person reads it.
	Ask string

	Ending executorkeep.Ending

	// Served is every other conversation the turn served, each one whose wake
	// a step of its run read ([CoordinatorOptions.Served]): each then
	// continues the session the turn kept. None for a turn whose record is
	// gone, which took its wakes with it.
	Served []string
}

// turnEndOf is the end of the turn run's row belongs to, ending as ending.
func (c *Coordinator) turnEndOf(ctx context.Context, run PendingRun, ending executorkeep.Ending) TurnEnd {
	end := TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID, ConversationKey: run.ConversationKey,
		WorkKey: run.WorkKey, WorkSince: run.WorkSince, Ask: run.TaskDescription, Ending: ending}
	if c.served != nil {
		end.Served = c.served(ctx, run)
	}
	return end
}

// OweEnd records on the turn's run row how the turn ended
// ([PendingStore.OweEnd]), for the run's ending to record on the executor log
// before it deletes the row ([Coordinator.finishEnding]). Answers whether the
// row now owes it or has recorded it already ([PendingRun.EndRecorded]); false
// where the turn has no row, which the caller records ([Coordinator.EndTurn]).
// DECIDED ONCE: a turn that owes an end keeps it and this one is dropped.
// UNDER A CONTEXT OF ITS OWN ([Coordinator.detachedRecord]): a frame that ends a
// turn is often one a drain is cancelling, and an end it could not owe was
// recorded before the captures settled. FITTED TO THE LOG'S BOUNDS
// ([executorkeep.Ending.Fit]): its prose is a model's and a guard's, and the
// row's budget measures an owed end at its bounds (rowbytes.go,
// TestTheWholeRowFitsItsBudget).
func (c *Coordinator) OweEnd(ctx context.Context, turnID string, ending executorkeep.Ending) (bool, error) {
	raw, err := owedEnd(turnID, ending)
	if err != nil {
		return false, err
	}
	ctx, cancel := c.detachedRecord(ctx)
	defer cancel()
	owing, _, err := c.pending.OweEnd(ctx, turnID, raw)
	if err != nil {
		return false, fmt.Errorf("sandbox: recording the end turn %s owes: %w", turnID, err)
	}
	return len(owing.OwedEnd) > 0 || owing.EndRecorded, nil
}

// owedEnd is ending as the run's row owes it ([PendingRun.OwedEnd]): fitted to
// the executor log's bounds, and refused where even fitted it is outside them
// ([executorkeep.Ending.Within]) — an engine word past its bound, which the
// log refuses rather than cuts, an outcome it does not know, an unkept capture
// placed nowhere — so the row never carries an end the log cannot record, nor
// one past what its budget measured.
func owedEnd(turnID string, ending executorkeep.Ending) (json.RawMessage, error) {
	fitted := ending.Fit()
	if !fitted.Within() {
		return nil, fmt.Errorf("sandbox: the end of turn %s is not one the executor log records: %w", turnID,
			fitted.Validate())
	}
	raw, err := json.Marshal(fitted)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encoding the end of turn %s: %w", turnID, err)
	}
	return raw, nil
}

// oweEnd is [Coordinator.OweEnd] for an ending's own step: the row as it stands
// after the write, owing the end it decided or one decided before it, or
// having recorded one already.
func (c *Coordinator) oweEnd(ctx context.Context, run PendingRun, ending executorkeep.Ending) (PendingRun, error) {
	raw, err := owedEnd(run.TurnID, ending)
	if err != nil {
		return PendingRun{}, err
	}
	writeCtx, writeDone := c.endingCall(ctx)
	owing, _, err := c.pending.OweEnd(writeCtx, run.TurnID, raw)
	writeDone()
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: recording the end turn %s owes: %w", run.TurnID, err)
	}
	if len(owing.OwedEnd) == 0 && !owing.EndRecorded {
		// GONE under the step: finished by another attempt at the same
		// ending, which recorded whatever it owed first.
		return run, nil
	}
	return owing, nil
}

// endKeys are the keys an [executorkeep.Ending] this build writes may hold.
var endKeys = wireKeys(reflect.TypeFor[executorkeep.Ending]())

// publishEnd records the end run's row owes on the executor log
// ([Coordinator.recordEnd]), asking again a capture still unresolved, and marks
// it recorded. Returns the row as it stands after, or why the end is still owed.
//
// THE END'S CAPTURE FIELDS ([unkeptOf], [wentOnPast], [lastWordOf]) ARE THE
// ROW'S TO SAY, never the frame's that owed the end: decided here, after the
// captures settled. An end holding a key or an outcome this build does not know
// (a newer build owed it; a rolling upgrade can move the seat here) is
// DEFERRED, not published: re-encoded it would drop the keys, or its unknown
// outcome would be refused as malformed and cleared, the end never recorded. It
// stays owed for a node of the build that wrote it. Never re-encode with less:
// what a build cannot read is deferred, never dropped.
func (c *Coordinator) publishEnd(ctx context.Context, run PendingRun) (PendingRun, error) {
	var ending executorkeep.Ending
	unknown, err := unmarshalCarrying(run.OwedEnd, &ending, endKeys)
	switch {
	case err != nil:
		// AN END NO BUILD COULD HAVE WRITTEN: not one this build can say
		// anything true with, and kept, it would hold the row for ever.
		log.ErrorContext(ctx, "executor_turn_end_unreadable", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the end the run's record owes does not decode, so it is dropped; the "+
				"retention duty ends a turn that captured a session as lost")
	case len(unknown) > 0 || !ending.Outcome.Valid():
		why := fmt.Sprintf("its outcome %q is not one this build knows", ending.Outcome)
		if len(unknown) > 0 {
			why = fmt.Sprintf("it holds %s, which this build does not know",
				words.And(slices.Sorted(maps.Keys(unknown))))
		}
		log.WarnContext(ctx, "executor_turn_end_deferred", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"detail", "the end the run's record owes was written by a newer build — "+why+" — so it is "+
				"left owed for a node of that build to record, and the run's ending is kept until one does")
		return run, fmt.Errorf("sandbox: the end turn %s owes was written by a newer build: %s", run.TurnID, why)
	default:
		run = c.settleUnresolved(ctx, run, askEvery)
		ending.Unkept, ending.WentOn, ending.LastWord = unkeptOf(run), wentOnPast(run), lastWordOf(run)
		ending.SpentSessions, ending.NotContinued = slices.Clone(run.SpentSessions), run.turnVerdicts()
		ending.Summary = c.summaryOf(ctx, run)
		if recordErr := c.recordEnd(ctx, c.turnEndOf(ctx, run, ending)); recordErr != nil {
			return run, recordErr
		}
		// AND ITS CONVERSATION IS TOLD what its runs' sessions lost, once the
		// log has the end, which is decided once: within [conversationTellBudget],
		// since the end is cleared from the row only after.
		if told := run.ConversationNotice(); told != nil && c.conversations != nil {
			tellCtx, tellDone := context.WithTimeout(ctx, conversationTellBudget)
			c.conversations.NotContinued(tellCtx, run, *told)
			tellDone()
		}
	}
	clearCtx, clearDone := c.endingCall(ctx)
	_, clearErr := c.pending.ClearEnd(clearCtx, run.TurnID, run.OwedEnd)
	clearDone()
	if clearErr != nil {
		return run, fmt.Errorf("sandbox: clearing the end turn %s owed, which the executor log has: %w",
			run.TurnID, clearErr)
	}
	run.OwedEnd, run.EndRecorded = nil, true
	return run, nil
}

// EndTurn records how a turn ended for a FRAME that found no run row to owe it
// on ([Coordinator.OweEnd] answered false, or could not be written): a launch
// the turn's fence refused once its row existed, whose ending deleted the row,
// or a row the seat's next holder finished. WHERE THE ROW IS THERE AFTER ALL
// (an owe that failed on a store that answers again, a row read gone that was
// not) the end is owed on it and recorded once the captures settle
// ([Coordinator.publishEnd]). A gone row's end, its capture stamps gone with it,
// is recorded now as it stands, best effort, and an end the log has is the
// answer. ITS CONVERSATION IS NOT TOLD what its runs lost ([ConversationTeller]):
// the row held that, and each launch's record says it on the turn's page. With
// no executor log (a test's) it writes nothing.
func (c *Coordinator) EndTurn(ctx context.Context, end TurnEnd) error {
	if !c.keepsSessions() {
		return nil
	}
	if _, found, getErr := c.pending.Get(ctx, end.TurnID); getErr == nil && found {
		if owed, oweErr := c.OweEnd(ctx, end.TurnID, end.Ending); oweErr == nil && owed {
			return nil
		}
	}
	return c.recordEnd(ctx, end)
}

// recordEnd records on the executor log how a turn ended — the account the
// turn's row of the log keeps beside the session it kept — under the one
// operation every path that ends the turn derives ([SessionWrites.End]),
// asked again under it while no answer settles it, within
// [sessionRecordBudget]. Nil once the log has the turn's end — taken now, by an
// earlier attempt, or refused as one it will never take, which is logged — and
// why not otherwise, for the caller to keep it and ask again.
//
// THE RUN'S ENDING CALLS IT for every turn that has a row
// ([Coordinator.publishEnd]), after the turn's captures settle; a frame whose
// turn has no row left reaches it through [Coordinator.EndTurn]. Its failure
// is fit as the log keeps it ([executorkeep.Excerpt]).
func (c *Coordinator) recordEnd(ctx context.Context, end TurnEnd) error {
	if !c.keepsSessions() {
		return nil
	}
	end.Ending.Failure = executorkeep.Excerpt(end.Ending.Failure, executorkeep.FailureMaxBytes)
	minted, err := c.keep.MintedAt(ctx, end.TurnID, end.WorkKey, end.WorkSince)
	if err != nil {
		return fmt.Errorf("sandbox: the operations of turn %s could not be minted to record its end: %w",
			end.TurnID, err)
	}
	record := EndRecord{Handle: end.Handle, Turn: end.TurnID, ConversationKey: end.ConversationKey,
		WorkKey: end.WorkKey, Minted: minted, Ask: end.Ask, Ending: end.Ending, Served: end.Served}
	var lastErr error
	ended := false
	c.askRecord(ctx, "end", end.TurnID, c.recordBudget, func(ctx context.Context) (bool, error) {
		answer, err := c.sessions.End(ctx, record)
		switch answer {
		case LogLanded:
			// Taken now, or ANOTHER PATH ENDED IT FIRST — two frames that
			// both end a turn derive the same operation, and the log takes
			// the first.
			ended = true
			return true, nil
		case LogRefused:
			log.ErrorContext(ctx, "executor_turn_end_refused", "turn_id", end.TurnID, "error", fmt.Sprint(err),
				"detail", "the executor log refused the turn's end as malformed, which no retry changes")
			ended = true
			return true, nil
		}
		lastErr = err
		return false, err
	})
	if !ended {
		return fmt.Errorf("sandbox: the executor log did not take the end of turn %s within %s: %w",
			end.TurnID, c.recordBudget, lastErr)
	}
	return nil
}
