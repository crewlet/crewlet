package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// EACH LAUNCH'S HARNESS SESSION, KEPT, and each turn's END, recorded
// (ADR-0073).
//
// # Taken wherever its box can still be read
//
// A run's harness keeps its conversation in its box, and a box does not outlive
// its turn. So the session is taken out at every point the box can still be
// read, ON THE NODE WHOSE HOST THE BOX IS ON (ADR-0065), before the box is
// paused or reclaimed:
//
//   - a completion's collection ([Coordinator.readBox]): the archive the run's
//     script packed when its harness exited — or, for a job that never got to
//     (one that failed before it packed, one lost with its engine whose box is
//     still there), one packed now. A job its turn's cap passed while it still
//     ran is ENDED first, as a stop's is, since the cap is an end;
//   - a stop's read of what the run had spent, which first ENDS THE JOB
//     ([Coordinator.endJob]): its script, asked, stops its harness and packs
//     the session as its own exit does, and the archive it left is taken —
//     one packed now where it left none, the job killed past the wait;
//   - the ending of a run LOST with its box still there (a launch whose engine
//     restarted before its turn could park), whose job is ended the same way
//     before the ending reclaims the box — on this node's host, or asked of the
//     host the box is on ([BoxReadLoss]).
//
// So a capture is FINAL wherever its job ended — by itself, asked, or killed —
// and taken as it stands, not final, only where the job still works: one a box
// would not end. No pack is made beside a harness still working but that.
//
// A box that is GONE — lost with its host, or with a node that left the fleet —
// has nothing left to read, and keeps nothing.
//
// # Two halves: taken before the box goes, kept off every frame
//
// What has to happen before the box is paused or reclaimed is only what reads
// it: the TAKE ([Coordinator.takeCapture]) — the box's archive, packed where it
// must be, streamed through the scrub of every secret the engine handed the box
// and the redaction every text leaving a box passes ([SessionTaker],
// [HandedSecrets]), sealed under the fleet keyring as it streams and spooled
// to this node's disk sealed ([SessionKeep.Seal]). It is bounded
// ([SessionTakeBudget]), and never past where the read it is part of must stop
// to leave its pause and its answer their room.
//
// What can take minutes — the upload of a session at its bound at the object
// store's slowest pace, and the record — is the KEEP ([Coordinator.keepCapture]),
// made after the read has answered, off its frame, under a budget of its own
// ([SessionKeepBudget]): no read a node is waiting on, and no ending, holds
// for an upload. The spool is the node's own and goes once the keep is done.
//
// # Every capture is stamped on the run's row
//
// Its place — the launch's ordinal ([PendingRun.LaunchSeq]) and its own among
// that launch's captures — is reserved on the row before a byte is spooled
// ([PendingStore.StampCapture]), with its SAVE POINT — how many of its round's
// calls the run's log had taken when it was read before the pack
// ([SessionCapture.Through]; every call the log takes is numbered as it is
// appended, [BridgeCall.Appended]) — and the note the job had read through: what
// a launch that continues the session would need to know of what came after
// it. How it came out is stamped beside: LANDED, REFUSED with why (no box to
// read it out of, a pack that failed, a session past its bounds, the seat's
// budget, an upload the store had no room for, a record the log will never
// take), or UNRESOLVED — uploaded, and whether its record landed unknown —
// which the turn's end, and a relaunch before it decides, asks again under the
// same operation.
//
// # A capture whose taker stopped is settled from the log, not waited out
//
// A take dies with its process, leaving its place stamped begun and its spool
// on that process's disk, so read by its age alone ([captureInFlight]) such a
// capture held the turn's end for as long as a capture can take
// ([SessionCaptureBudget], about twenty-one minutes), and a relaunch that
// followed it started a session of its own beside a record the log may well
// have taken.
//
// So every process keeps the captures it takes under a TAKER LEASE of its own
// in the coordination store ([coord.ClassTaker], taker.go): claimed when its
// coordinator is built, renewed [coord.KeptRenewals] times a TTL, held until
// its last keep has ended — through a drain, which gives presence up while the
// keeps still upload — and released then; a process that dies leaves it to
// lapse on its TTL. Every stamp names that lease: its holder, the process
// incarnation ([SessionCapture.Taker]), and its fencing token
// ([SessionCapture.TakerEpoch]), read again right before the take reserves its
// place, so a lease claimed afresh while the session was packed is the one
// named.
//
// A TAKER IS GONE where that lease has lapsed or is held at another epoch, and
// nothing else: one coordination read, three-valued, so a store that cannot
// answer is never a taker gone ([Coordinator.takerGone]). Every wait for a
// capture ([Coordinator.awaitCapture]) — the end's and the relaunch's — asks
// it of a begun capture, and settles one whose taker is gone from the turn's
// row on the executor log, read at the level a relaunch reads it, which has
// seen every record the log took before it ([Coordinator.settleGoneTaker]):
// LANDED where the row names its place, written from the row; REFUSED where it
// does not, since a taker gone sends nothing more. Written only over the begun
// stamp it was decided from ([PendingStore.SettleBegun]), so a stamp the taker
// wrote in the meantime stands — an unresolved one among them, which says a
// record was sent and may yet land, and which a wait then asks again under its
// own operation rather than settle refused.
//
// THAT IS WHAT THE FENCE BUYS. A keep checks its own lease before its upload
// and again before every attempt at its record, from its own renewals rather
// than a store read: held at the epoch its stamp names, inside the exclusive
// window of its last confirmed renewal (coord.ExclusiveThrough, one renewal
// interval its margin) — and EVERY ATTEMPT AT THE RECORD IS CUT AT THAT
// WINDOW'S END, so none outlasts it. Fenced, it uploads and records nothing,
// stamps what it knows — refused where no record was sent, unresolved where
// one was and went unanswered, which a wait asks again under its own operation
// rather than settle — and says so (`executor_session_fenced`, beside the
// `executor_session_not_kept` every unkept capture logs). So no attempt the
// taker makes outlives the window, which closes a renewal interval before any
// peer can read the lease lapsed.
//
// WHAT THE FENCE CANNOT REACH is a request the taker had already handed on
// before its window closed — to a data node the router asked, or to the
// broker connection's own buffer while it reconnects — and that the log takes
// only after a peer's read of the turn's row: the capture's record then lands
// beside a stamp that says refused. On the turn's end the log orders the two
// itself — a record it took before the end is the row's place and is kept,
// one after it is refused by the end's tombstone — and never contradicts
// itself. THAT IS DECIDED, AND NO REFUSAL GOES TO THE LOG: ordered there as the
// end's tombstone is, the settle's refusal would have the applier refuse the
// late capture and lose a session, and no session history is given up for a
// stamp's sake. So a late capture the log takes before the turn's end is kept
// and read as any other — the turn's row names it, a session read and the
// conversation's next turn take it — and one decision alone goes by the stamp
// once the log names the capture: the relaunch that follows the job whose
// capture was settled, which continues only what the run's record keeps, and
// the record never keeps a place it holds refused — so every attempt at that
// relaunch starts without the capture, the one whose wait settled it and one
// asked again after the record landed alike; and where the decision is told
// with the turn's row in hand — to a reader of the seat's sessions, and in the
// relaunch's own reason, which reads the row for a capture its record refused
// — it says the log keeps the capture, never that it was not kept, and the
// relaunch's run, starting without it, is named the turn's own as a session it
// may read ([namedThoughRefused], [ContinuedSession.KeptTurn]).
// Nothing else treats the stamp as authoritative once the log names the
// capture: the end names it unkept at the row's own place, which releases
// nothing ([executorkeep.Ending.Unkept]), and an account of how the turn ended
// tells a capture as not kept only where it is newer than the row's own place
// (internal/executordomain's Session.Unkept).
//
// THAT RELAUNCH STARTS WITHOUT IT AS ONE DOES THAT WILL NOT WAIT OUT A SLOW
// KEEP ([relaunchCaptureWait], [Coordinator.continuedWithout]): in the base the
// turn's first launch froze, where the turn has kept nothing and its row still
// held that base when the relaunch read it — the late record landing only
// after — and otherwise in a session of its own, or the one its job's box
// holds where it carries the run on in that box.
//
// NOT PRESENCE, which answers another question. A draining node gives its
// presence up at once while its keeps still upload, so a taker read as gone
// without it would have its capture refused under it, the turn's end would
// tombstone the turn before the draining node's record landed, and the turn's
// last session would be lost — with the older one the end's unkept capture
// lets go. Presence says whether a node may take new work; only the taker's
// own lease says whether a process still keeps what it began.
//
// A stamp an older build wrote names no taker, and is waited for by its age
// alone.
//
// # And every turn's end, owed on the row until the log takes it
//
// How the turn ended — completed or failed, the reviewer's decision, whether
// its reply reached the party waiting, who stopped it and why, what lost it —
// is the account a transcript cannot hold ([executorkeep.Ending]), recorded
// on the turn's row of the log beside the session it kept. Whatever ends the
// turn writes it on the run's row ([PendingStore.OweEnd]): the engine's resumed
// turn, and this package's endings that no frame of the turn reaches — a loss,
// a stop ([CoordinatorOptions.TurnLost], [CoordinatorOptions.CloseStopped]).
// The run's ending records it on the executor log ([Coordinator.publishEnd])
// before it deletes the row, which the delete refuses until then
// ([ErrEndOwed]): after every capture of the turn has settled, so the end's
// tombstone never refuses the turn's own last session, with the turn's NEWEST
// capture where it was not kept ([executorkeep.Unkept]), and saying whether the
// turn WENT ON PAST the session its row names ([wentOnPast]) — kept from a
// capture taken while its job still worked, or from an earlier launch's — so
// the log's row never names an older session as the turn's last without
// saying so: the log keeps that session, and every reader of it is told the
// turn went on past it. On every frame
// somebody waits on — a completion's resume, a stop, a seat's recovery — the
// ending is decided there and the end recorded by the ending's own retry,
// armed at once ([errAskAway]): a capture still being kept, and the log's
// write after it, are not that frame's to wait on. A seat's retirement, which
// holds the seat's lease and a budget of its own, both decides its endings and
// records their ends on its own frame. So the row outlives the frame that
// decided its ending by the settling of the turn's captures — bounded by a
// capture's own budget ([SessionCaptureBudget], about twenty-one minutes at
// the bounds), and for a capture whose taker is gone by its lease's TTL and
// one read of the log (above) — and the end's write, and the seat's mail
// waits on it as on every ending kept for its retry. A turn with no capture
// still ends: its row names no object, and holds the account.
//
// # And every turn's first read of its conversation, held on the log
//
// A turn's first launch freezes the read its dispatch made of its conversation
// on the turn's run record ([PendingRun.Base]) and records it on the log as the
// turn's BEGIN ([Coordinator.recordBegin]): the turn's row from then on names
// where the read was made, the turns it handed out that the turn was told of —
// which its end absorbs once it has landed a capture — and the head it froze,
// which the row HOLDS against every release but a retirement while the turn is
// live and has landed no capture. Every capture carries the same read
// ([Coordinator.recordCapture]), which the log takes where no begin landed
// first: a turn's first landed record keeps the read.
//
// # The switch
//
// `turn_engine.conversation_session.enabled: false` keeps nothing — no capture
// is taken or stamped ([SessionPolicy.Keep], the fleet's word) — and prepares
// nothing for one: no launch seals what it handed its box, and no loss asks
// another node's host to read a box, holding the seat's mail for a take that
// would keep nothing ([Coordinator.capturing], this node's word). It still
// records every end, which is the account and not the session; and a stop
// still ends its job before its box goes, which is how a stop ends a job.

// SessionWrites is the executor log's writes a coordinator records through, in
// this package's words. The engine answers them over the estate router's
// writes (internal/estate's ExecutorWrites), so a node without `data` records
// through a data node, and a node with it through its own copy — never a data
// node's local writer, which a seats-only node does not have — and it states
// each record as the log's: the author every executor record of this package
// names, and the operation the record's place derives.
//
// IN THIS PACKAGE'S WORDS, NEVER THE LOG'S: the log's records and its answers
// are the state log's types, and the sandbox links no state log (its
// dependency gate), so the engine translates both ways — a record out, and the
// log's answer back as what a coordinator does next ([LogAnswer]). What the two
// sides must agree on byte for byte, the turn's ending and a session's bounds,
// is internal/executorkeep's.
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
}

// SessionPolicy is the company's word on keeping sessions.
type SessionPolicy struct {
	// Keep is `turn_engine.conversation_session.enabled`: false keeps no
	// session at all, and records every turn's end all the same.
	Keep bool

	// SeatBudget is `turn_engine.conversation_session.max_bytes_per_seat`,
	// carried by every capture so every node applies one number.
	SeatBudget int64
}

// SessionKept is what a box read took of the job's session, for the launch's
// own record (`executor_collected`), on whichever node collects: it rides the
// read's result back from the box's host.
//
// WHAT WAS TAKEN, NOT WHAT LANDED: the read answers once the session is out of
// the box and sealed, and its upload and record are made after, off the read
// ([Coordinator.keepCapture]); how they came out is stamped on the run's row
// and logged (`executor_session_kept`, `executor_session_not_kept`).
type SessionKept struct {
	// CaptureSeq is which of the launch's captures it was.
	CaptureSeq int64

	// Bytes is the size of the sealed session taken to be kept; zero where
	// none was.
	Bytes int64

	// NotKept is why the job's session was not taken to be kept — refused,
	// with why. Empty where it was taken.
	NotKept string
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
	// the turn's end, and read as refused once its object is past
	// objstore.RecordWithin, which no record may name.
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
	// did not.
	State  CaptureState `json:"state"`
	Reason string       `json:"reason,omitempty"`

	// Final says the job had ENDED when the capture was taken — it exited,
	// was ended by a stop, a loss or its turn's cap, or died with its host —
	// so nothing after it changes the session in its box, and a later read of
	// the same job takes nothing new. A job still working when it was taken
	// (one a box would not end) is not final.
	Final bool `json:"final,omitempty"`

	// Through is the session's SAVE POINT: how many calls the round's log had
	// taken ([BridgeLog.Appends]) at the one read of it the capture was placed
	// by, made before the archive was packed ([Coordinator.takeCapture]).
	// Every call the log takes after that read is numbered past it
	// ([BridgeCall.Appended]), and is one the session had not been answered
	// when it was read: a launch that continues the session is told each, with
	// its outcome — whatever its place in the order the calls were made, so a
	// call in flight at the read is told however many places the log's bound
	// dropped below it — and how many of them the bound dropped from the
	// middle, exactly (internal/agent/runner's calledAfter). A call answered
	// between the read and the pack is in the session and told all the same:
	// told twice, never not at all.
	//
	// A POINTER, because no call taken is a save point — 0, a session that
	// holds none of the round's calls — and "not stated" is another fact. A
	// stamp an older build wrote states none, and a kept capture that states
	// none is not continued ([followedCapture]), as a session whose place
	// cannot be stated is not kept. On a REFUSED stamp nil is "not known": the
	// refusal came before the log was read, or the log could not be read
	// ([Coordinator.refuseUnplaced]).
	Through *int `json:"bridge_through,omitempty"`

	// BridgeSeq and BridgeOpen are the same read's save point AS AN OLDER BUILD
	// READS IT, written for a fleet mid-upgrade and read by nothing of this
	// build: the last bridged call ([BridgeCall.Seq]) the log held
	// ([BridgeLog.LastSeq]), and the places at or below it the log held no
	// call at ([openPlaces]) — a call the bridge numbers as it arrives is
	// recorded only once its tool answers, so a place below the highest can
	// be a call in flight. That build tells a continuation every call past
	// BridgeSeq or at a place BridgeOpen names; but BridgeOpen keeps only the
	// highest [maxOpenPlaces] of them, which a long round's dropped middle
	// fills, so a call in flight below them reads to it as answered, which no
	// count of places can mend and Through does. FROM THE SAME READ AS
	// THROUGH, so the two state one save point. On a REFUSED stamp a BridgeSeq
	// of zero is "not known", as a nil Through is.
	BridgeSeq  int   `json:"bridge_seq,omitempty"`
	BridgeOpen []int `json:"bridge_open,omitempty"`

	// NoteCursor is the last note read ([RunSteer.Reads]) the session holds
	// ([notesHeld]): every note read when the archive was packed, but for a
	// session its harness was cut off in, none its job took at its last take
	// of them, which may never have reached it. A launch that continues the
	// session hands it every note read after.
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

	// At is when it was taken, on this node's clock.
	At time.Time `json:"at"`

	// Taker and TakerEpoch name the TAKER LEASE the capture was taken under
	// (see the file comment): its holder, the process incarnation that took
	// it — `<node>:<uuid>`, the taking coordinator's own
	// ([CoordinatorOptions.Incarnation]) — and the fencing token it held
	// then, both on every stamp of the take. A wait for the capture settles it
	// from the executor log once that lease has lapsed or is held at another
	// epoch ([Coordinator.takerGone]), and the keep goes no further once its
	// own is ([takerLease.fence]). Empty on a stamp an older build wrote,
	// which is waited out by its age alone, and carried in [SessionCapture.Extra]
	// by a build that does not know them. The holder is held to
	// [maxIncarnationBytes] as the row's owner is, which a stamp past it is
	// refused at ([SessionCapture.Bounded]) and a coordinator built with one
	// past it at its construction; the token is an integer, as the owner's is.
	Taker      string `json:"taker,omitempty"`
	TakerEpoch int64  `json:"taker_epoch,omitempty"`

	// Harness, HarnessVersion and SessionID are the session as the box
	// described it; ArchiveFormat its layout.
	Harness        string `json:"harness,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ArchiveFormat  int    `json:"archive_format,omitempty"`

	// Object, ObjectHash, ObjectSize and ContentKey are the sealed object it
	// was uploaded into, once it was; Home and Workspace the box's paths,
	// which a later launch maps onto its own.
	Object     string `json:"object,omitempty"`
	ObjectHash string `json:"object_hash,omitempty"`
	ObjectSize int64  `json:"object_size,omitempty"`
	ContentKey string `json:"content_key,omitempty"`
	Home       string `json:"home,omitempty"`
	Workspace  string `json:"workspace,omitempty"`

	// SeatBudget and Minted are the record's: the per-seat budget it carries
	// and the instant its operation is derived at, kept so an unresolved
	// capture is asked again as the very operation it was.
	SeatBudget int64     `json:"seat_budget,omitempty"`
	Minted     time.Time `json:"minted,omitzero"`

	// Extra is every key of the capture this build does not know, kept
	// verbatim and written back by every write of the record (wirekeys.go).
	Extra map[string]json.RawMessage `json:"-"`
}

// ErrCaptureUnbounded is a capture stamp carrying a field past the bound the
// row holds it to ([SessionCapture.Bounded]), refused by
// [PendingStore.StampCapture] before anything is written.
var ErrCaptureUnbounded = errors.New("sandbox: a capture stamp carries a field past its bound")

// Bounded reports why c is past what a run's row carries of a capture, nil
// where it is within it: each text a box describes its session by — the
// harness, the release that wrote it, its session id — the paths the box
// names, and why a capture did not land, each within the bound the executor
// log holds the same field to (internal/executorkeep), the open places within
// [maxOpenPlaces], and the process that took it within [maxIncarnationBytes].
//
// WHAT A BOX SAYS OF ITS SESSION IS THE BOX'S OWN DOING: a file its run's own
// agent can write, read up to the size a box's file read takes. Nothing else
// bounds it before the row, and the log's own check runs only after the stamp
// has landed — on a row that, with a release of megabytes on it twice over
// (the place and the kept capture), took no later write that adds a byte: no
// bridged call, no owed end, no stop's ending, and so never its delete.
// Refused here, the row is never asked to carry it; the reader that parses the
// box's record (internal/sandbox/executor) reads such a release as none.
func (c SessionCapture) Bounded() error {
	for _, f := range []struct {
		field, value string
		max          int
	}{
		{"launch_id", c.Launch, executorkeep.TurnIDMaxBytes},
		{"reason", c.Reason, executorkeep.UnkeptReasonMaxBytes},
		{"harness", c.Harness, executorkeep.HarnessMaxBytes},
		{"harness_version", c.HarnessVersion, executorkeep.HarnessVersionMaxBytes},
		{"session_id", c.SessionID, executorkeep.SessionIDMaxBytes},
		{"home", c.Home, executorkeep.PathMaxBytes},
		{"workspace", c.Workspace, executorkeep.PathMaxBytes},
		{"taker", c.Taker, maxIncarnationBytes},
	} {
		if len(f.value) > f.max {
			return fmt.Errorf("sandbox: its %s is %d bytes, past the %d it may be: %w", f.field,
				len(f.value), f.max, ErrCaptureUnbounded)
		}
	}
	if len(c.BridgeOpen) > maxOpenPlaces {
		return fmt.Errorf("sandbox: it names %d open places, past the %d it may: %w", len(c.BridgeOpen),
			maxOpenPlaces, ErrCaptureUnbounded)
	}
	return nil
}

// maxIncarnationBytes bounds a process incarnation as a run's row carries one:
// the process that owns the run's seat ([PendingRun.Owner]) and the one that
// took a capture ([SessionCapture.Taker]).
//
// ONE HUNDRED AND TWENTY-EIGHT BYTES, from the incarnation's own shape,
// `<node>:<uuid>` (internal/config's NewIncarnation): a node id at the
// sixty-four bytes its grammar admits, the separator and a uuid's thirty-six
// are 101, and the rest is room. The engine writes both from its own
// incarnation, never from anything a box says, and the row's budget counts
// each at this bound (TestTheWholeRowFitsItsBudget). The owner, which every
// fence the seat's lease hands the store writes, is held to it by that shape
// alone; the taker besides by name — a stamp naming one past it is refused
// before it reaches the row, and a coordinator built with an incarnation past
// it is refused at its construction ([NewCoordinator]), rather than every
// capture it takes at its stamp.
const maxIncarnationBytes = 128

// placeAt places c's session in its round's calls as read — one read of the
// run's log, made before the take — holds them: its save point
// ([SessionCapture.Through]) and, off the same read, the places an older build
// reads it by ([SessionCapture.BridgeSeq], [SessionCapture.BridgeOpen]).
func (c *SessionCapture) placeAt(read BridgeLog) {
	c.Through = new(read.Appends())
	c.BridgeSeq = read.LastSeq()
	c.BridgeOpen = openPlaces(read, c.BridgeSeq)
}

// SavePoint is the save point c states ([SessionCapture.Through]), and whether
// it states one: a stamp an older build wrote states none, and neither does a
// refusal made before the log was read.
func (c SessionCapture) SavePoint() (int, bool) {
	if c.Through == nil {
		return 0, false
	}
	return *c.Through, true
}

// stampReason is why a capture came out as it did, as its stamp carries it:
// redacted, and held to the bound the executor log holds the same words to
// ([SessionCapture.Bounded]) — an error's text is the engine's and has no
// bound of its own.
func stampReason(why string) string {
	return textcut.Within(redact.Secrets(why), executorkeep.UnkeptReasonMaxBytes)
}

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
// same take's stamp at the newest place, in the same state — what a write the
// store applied and answered too late for its asker to hear leaves there.
func holdsStamp(run PendingRun, capture SessionCapture) bool {
	newest := run.Capture
	return newest != nil && capture.sameTake(*newest) && newest.State == capture.State
}

// stampOnto stamps capture on run as [PendingStore.StampCapture] does, and
// reports whether it changed the row: the row's own rule, which the store
// applies to the row as it stands, and an ending applies to its copy where the
// row cannot be read back after a stamp it made ([Coordinator.publishEnd]).
//
// A CAPTURE PLACED AFTER THE ROW'S NEWEST IS TAKEN, whatever its state, and one
// at the newest's own place only where it is the SAME TAKE'S ([sameTake]) and
// moves that capture on — begun to an outcome, unresolved to an answer — so a
// late write never takes an answer back, and no read settles a place another
// read reserved: a take refused before it held a place, say, which stamps the
// place after the newest it read, finds it reserved since by a read that is
// keeping the session. Anything older is refused. And a capture that LANDED is
// the turn's kept one wherever it is newer than the kept one — an older
// capture's answer that comes after a newer one began included — while a place
// the row holds refused is never kept, whatever a late write says.
func stampOnto(run *PendingRun, capture SessionCapture) bool {
	took := false
	newest := run.Capture
	switch {
	case newest == nil || capture.newerThan(*newest):
		took = true
	case capture.sameTake(*newest):
		took = newest.State.advancesTo(capture.State)
	}
	if took {
		stamped := capture
		run.Capture = &stamped
	}
	landed := capture.State == CaptureLanded && (took || !capture.samePlace(*newest))
	if landed && (run.Kept == nil || capture.newerThan(*run.Kept)) {
		kept := capture
		run.Kept, took = &kept, true
	}
	return took
}

// kept is the capture as a box read reports it ([SessionKept]).
func (c SessionCapture) kept() *SessionKept {
	k := &SessionKept{CaptureSeq: c.CaptureSeq}
	switch c.State {
	case CaptureLanded:
		k.Bytes = c.ObjectSize
	case CaptureBegun, CaptureUnresolved:
		// Taken already, and on its way; how it lands is on the row.
	default:
		k.NotKept = c.Reason
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
// at it, under its one operation, together.
//
// THIRTY SECONDS. A write the log takes is answered in milliseconds; one its
// serving node has published and not yet applied is answered within the
// publisher's own five-second wait (statelog.DefaultResolveBudget); and a write
// sent to a data node that died under it is asked of the next data node by the
// router itself. Thirty seconds outlasts all of that and a broker's leader
// election besides. A log that has not answered by then is an outage its own
// alarms name: an unresolved capture is asked again by the turn's end, and an
// end the log has not taken stays owed on the run's row, its ending kept for
// this node's retry.
const sessionRecordBudget = 30 * time.Second

// sessionRetryBase and sessionRetryCeiling space the attempts at one record: a
// second, doubling to eight — six attempts inside [sessionRecordBudget].
const (
	sessionRetryBase    = time.Second
	sessionRetryCeiling = 8 * time.Second
)

// SessionScrubWait bounds the scrub of a session as it streams out of its box:
// two readings of an archive at its bound — a tar is read twice, to learn each
// file's size as rewritten — each of up to the plaintext the scrub admits
// (internal/sandbox/executor's scrubExpansion times the archive's bound, which
// its boundsFor computes: eight times), redacted at the tens of mebibytes a
// second the redaction runs at, and the archive read out of a box whose agent
// serves it at a mebibyte a second at the least.
//
// TWO MINUTES, and a judgment: what the redaction costs per mebibyte is the
// redaction's own, which grows with its rules. A session that is still being
// scrubbed when it runs out is refused rather than left holding its box.
const SessionScrubWait = 2 * time.Minute

// SessionTakeBudget is the most a take adds to the read of a box it is made in:
// the pack ([SessionPackWait]) and the scrub ([SessionScrubWait]).
//
// SIZED ON THE BOUNDS, NOT ON THE ARCHIVE, because a node that asks another to
// read a box ([BoxReadBudget]) cannot know how large the session in it is
// before the read: a session of an ordinary day's work is a mebibyte or two and
// takes seconds, and the bound is what a host that is still working is given
// before the asker takes it for one that is not.
const SessionTakeBudget = SessionPackWait + SessionScrubWait

// SessionKeepBudget bounds a keep, off the read: the upload of a sealed session
// at its bound at the object store's slowest pace (objstore.PaceFor of
// executorkeep.SealedMaxBytes: thirty-three mebibytes begun, at thirty
// seconds each) and its record ([sessionRecordBudget]).
var SessionKeepBudget = objstore.PaceFor(executorkeep.SealedMaxBytes) + sessionRecordBudget

// SessionCaptureBudget is the most a capture takes from its take to its record:
// a capture stamped begun longer ago than this is one whose node stopped, or
// whose budget ran out, before it could stamp what came of it.
var SessionCaptureBudget = SessionTakeBudget + SessionKeepBudget

// sessionKeepDrain is how long a stopping coordinator lets keeps already under
// way finish before it cancels them ([Coordinator.Stop]): the teardown grace
// every box call is given ([discardGrace]). A keep cut there is stamped refused
// saying so, and the turn's next launch starts fresh.
const sessionKeepDrain = discardGrace

// captureWaitTick spaces the reads of a run's row an ending, or a relaunch,
// makes while a capture of its turn is still being kept
// ([Coordinator.awaitCapture]).
const captureWaitTick = 2 * time.Second

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
		cutOff: !over || result.Captured == nil || result.HarnessCutOff || result.JobEnd != ""}
}

// exhaustedNotKept is why a job that ran out of its model's context left no
// capture: what its launch's record and the log say.
const exhaustedNotKept = "the job ran out of its model's context, so no launch continues its session and it " +
	"is not kept"

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

// sessionsRanOutOfContext are the sessions such a job may have run, in the row's word
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

// withExhausted is held with sessions added, no id twice, held to
// [executorkeep.ExhaustedMaxSessions]. Past it the first stays, the one earlier rows
// of the conversation name, and the next oldest goes.
func withExhausted(held, sessions []string) []string {
	out := slices.Clone(held)
	for _, session := range sessions {
		if !slices.Contains(out, session) {
			out = append(out, session)
		}
	}
	for len(out) > executorkeep.ExhaustedMaxSessions {
		out = slices.Delete(out, 1, 2)
	}
	return out
}

// maxOpenPlaces bounds [SessionCapture.BridgeOpen]: the most places below a
// capture's highest call it names, the highest of them — the save point an
// older build reads, which this one writes for it and does not read.
//
// SIXTY-FOUR, from what can be open at once: a call is in flight from its
// arrival to its tool's answer, and a harness has at most its model's parallel
// calls and its subagents' in flight together — a handful, a few dozen at the
// most. But the rest of what a row lacks is not all older than those: the
// places the row's bound dropped from the middle of a long round
// ([MaxBridgeCalls]) are the HIGHEST it leaves unlisted, so in a round long
// enough they fill every slot, and a call in flight below them is named in
// none — which that build reads as answered. That is why this build reads a
// count of the calls the log took instead ([SessionCapture.Through]); the
// bound stays, since the row's arithmetic and that build's reading are both
// held to it.
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

// notesHeld is the last note read ([RunSteer.Reads]) a session packed from
// run's job — launch — holds ([SessionCapture.NoteCursor]): every one read,
// where its harness exited cleanly; and where it was cut off, every one read
// before the job's LAST TAKE of its notes, whose notes may never have reached
// it. A take is recorded read in the write that answers the harness's ask,
// before the answer is handed back (internal/engine's note hooks), so a
// harness cut off between the two never had them; a take before the last was
// answered, since the harness went on to ask again. The notes the job's brief
// read are the brief's, which the session holds from its first message.
func notesHeld(run PendingRun, launch string, cutOff bool) int {
	held := run.Steer.Reads
	if !cutOff {
		return held
	}
	last := 0
	for _, note := range run.Steer.Notes {
		read := note.Read
		if read != nil && read.Launch == launch && read.Boundary != string(types.SteerBoundaryBrief) &&
			read.Order > last {
			last = read.Order
		}
	}
	if last > 0 {
		held = min(held, last-1)
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
}

// spoolPrefix names this package's spools in the node's spool directory
// ([CoordinatorOptions.SpoolDir]), so a spool a crash left behind is found
// again ([Coordinator.sweepSpools]).
const spoolPrefix = "crewlet-session-"

// takeCapture takes the session of run's job out of box — see the file comment
// — and stamps where it stands on the run's row, reporting what came of it for
// the read's result, and the spool its keep uploads; nil spool where nothing is
// left to keep: no executor log wired to this coordinator, a runner whose
// harness carries no session, the switch off, a box holding no session, a job
// of this launch taken once it had ended already, or a take refused.
//
// BEST EFFORT AND BOUNDED ([Coordinator.takeContext]): what the read it is part
// of does is never the capture's to decide, so every failure is stamped and
// logged and the read goes on — a place the store was asked to reserve and did
// not answer within the take's time included, which is stamped refused on the
// row as it stands when the store writes that, whether the store took the
// reservation before it or would after ([Coordinator.abandonPlace]). Only a row
// the store never lets the read write, past every ask, is left as it stands.
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
	ctx, cancel, by := c.takeContext(ctx)
	defer cancel()
	// THE ROW AS IT STANDS, for the ordinals, what the job was handed and
	// what it had been told: the reader's copy may be the claim's, from
	// before the job's last calls.
	current, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		why := fmt.Sprintf("the run's record could not be read to place the capture: %v", err)
		log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"detail", why)
		return nil, &SessionKept{NotKept: why}
	case !found || current.LaunchID != run.LaunchID:
		// GONE, OR ANOTHER JOB'S NOW: the read was of a job the row no
		// longer names, and its box is that job's.
		return nil, nil
	case current.LaunchSeq <= 0:
		why := "the job was launched by a build that numbered no launch, so no capture of it can be " +
			"ordered; the turn's next launch is numbered and kept"
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name, "detail", why)
		return nil, &SessionKept{NotKept: why}
	}
	if last := current.Capture; last != nil && last.LaunchSeq == current.LaunchSeq &&
		last.Launch == run.LaunchID && last.Final {
		// THE JOB WAS TAKEN ONCE IT HAD ENDED — its collection retried after
		// a resume that failed, or a stop's read made again — and nothing has
		// changed its session since, whatever that take came to: taken
		// again, it would be another upload of the same bytes, holding the
		// store's room for a day, or the same refusal.
		return nil, last.kept()
	}
	policy, err := c.keep.Policy(ctx)
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, nil, fmt.Sprintf("the company's session "+
			"policy could not be read off the revision the fleet runs: %v", err))
	}
	if !policy.Keep {
		return nil, nil
	}
	if at.exhausted {
		// REFUSED UNTAKEN: no launch would continue it, and its upload could release
		// another conversation's session against the seat's budget. Stamped, so the
		// end names it ([unkeptOf]) and the row stops naming anything older.
		return nil, c.refuseUnplaced(ctx, by, run, current, at, nil, exhaustedNotKept)
	}
	// A TAKER LEASE THE CAPTURE CAN BE TAKEN UNDER, asked before the take: a
	// begun stamp is waited on for as long as the lease it names holds, so
	// one this process could not name — its claim or its last renewal
	// unconfirmed, a store it cannot reach — is not begun at all, and the
	// box is spared a pack nothing would keep.
	if _, held := c.heldTaker(); !held {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, nil, takerUnheld)
	}
	// THE SESSION'S PLACE IN THE RUN'S LOG, read BEFORE THE TAKE: how many
	// calls the log had taken is how far the session had been answered
	// ([SessionCapture.Through]). A call is appended once its tool has
	// answered it, so every call this read counts was answered before the
	// pack that follows, and a call answered after it is numbered past it,
	// and told — a call in flight now among them, whatever its place. Kept
	// beside the row rather than on it ([BridgeLog]), so a log the store
	// could not read REFUSES the capture: a session whose place cannot be
	// stated is one a launch that continues it could not tell what came
	// after, and is not kept. Read as the job stands to hold it
	// ([PendingStore.TurnLog]), so a log a relaunch has not yet opened to the
	// row's job places the session where that opening will number from,
	// never at 0.
	calls, err := c.pending.TurnLog(ctx, current)
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, nil,
			fmt.Sprintf("the run's bridged log could not be read: %v", err))
	}
	scrub, err := c.scrubValues(ctx, current, box)
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, &calls, err.Error())
	}
	taken, err := taker.TakeSession(ctx, box, SessionTake{Pack: at.pack, Scrub: scrub,
		MaxBytes: executorkeep.SessionMaxBytes})
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, &calls, takeRefusal(ctx, err))
	}
	if taken.Body == nil {
		return nil, nil
	}
	defer func() { _ = taken.Body.Close() }()
	// AND THE TENURE THE STAMP NAMES, read again once the session is in hand,
	// right before its place is reserved: the pack and the scrub can take
	// minutes ([SessionTakeBudget]), and a lease that lapsed and was claimed
	// again meanwhile is held at a new epoch. Stamped with the tenure read
	// before the pack, the capture named one every peer reads as gone at once
	// and its own keep fences, and a session taken while this process held its
	// lease again was lost.
	hold, held := c.heldTaker()
	if !held {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, &calls, takerUnheld)
	}
	stamp := SessionCapture{
		LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current), Launch: run.LaunchID,
		State: CaptureBegun, Final: at.final, Interrupted: at.cutOff,
		NoteCursor: notesHeld(current, run.LaunchID, at.cutOff), At: c.now().UTC(),
		Taker: hold.holder, TakerEpoch: hold.epoch,
		Harness: taken.Archive.Harness, HarnessVersion: taken.Archive.Version,
		SessionID: taken.Archive.SessionID, ArchiveFormat: taken.Archive.Format,
		Home: box.Home(), Workspace: path.Join(box.Home(), WorkspaceSubdir),
		SeatBudget: policy.SeatBudget,
	}
	stamp.placeAt(calls)
	// THE PLACE IS RESERVED WITHIN THE TAKE'S OWN TIME, never past it: a
	// reservation answered after the take's deadline reserves a place for a
	// take that can no longer spool, and leaves the outcome stamp none of the
	// room it is kept before by ([Coordinator.takeContext]).
	reserveBy, _ := ctx.Deadline()
	took, stampErr := c.reserveCapture(ctx, reserveBy, run.TurnID, stamp)
	switch {
	case stampErr != nil:
		// UNANSWERED WITHIN THE TAKE'S TIME, which a store may have taken all
		// the same — a write applied and answered too late, or applied once
		// its asker had given up — so the place is settled in the room the
		// read leaves the outcome stamp ([Coordinator.abandonPlace]).
		return nil, c.abandonPlace(ctx, by, run, stamp, at, stampErr)
	case !took:
		// ANOTHER READ OF THE SAME RUN TOOK THIS PLACE FIRST — a stop's and a
		// collection's, at once — and keeps the session itself. Never this
		// take's own earlier ask, answered too late: the store answers a
		// stamp the row already holds as taken ([PendingStore.StampCapture]).
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"capture_seq", stamp.CaptureSeq,
			"detail", "another read of the run reserved the capture's place first, and keeps the session")
		return nil, nil
	}
	spooled, err := c.spool(ctx, run, stamp, taken)
	if err != nil {
		return nil, c.finishCapture(ctx, by, run, stamp, CaptureRefused, takeRefusal(ctx, err))
	}
	// WHAT THE SESSION'S LAST REQUEST CARRIED, read off the archive as it was
	// spooled, for the floor of the launch that continues it: stamped with the
	// capture's outcome, which is the stamp that launch reads.
	if taken.Context != nil {
		spooled.stamp.ContextTokens = taken.Context()
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
			"session", stamp.SessionID, "error", keepErr.Error(),
			"detail", "the kept archive's digest could not be recorded in its box")
	}
	return spooled, &SessionKept{CaptureSeq: stamp.CaptureSeq, Bytes: spooled.spool.Size}
}

// takeContext is the context a take runs under, and the instant by which its
// own records must be made: its own budget ([SessionTakeBudget]), and never
// past where the read it is part of must stop to leave those records their
// room — the stamp of what came of it ([Coordinator.settleContext]), the
// digest it leaves in the box ([stampContext]) — and after them its pause and
// its answer theirs ([discardGrace] each). So nothing a take does, cut short or
// not, costs the box its pause, which is what holds a collected box asleep, and
// bounded by the pause reaper, until whatever its turn does next.
// by is zero where the read has no deadline.
func (c *Coordinator) takeContext(ctx context.Context) (context.Context, context.CancelFunc, time.Time) {
	deadline := time.Now().Add(SessionTakeBudget)
	var by time.Time
	if d, ok := ctx.Deadline(); ok {
		by = d.Add(-discardGrace)
		if last := by.Add(-discardGrace); last.Before(deadline) {
			deadline = last
		}
	}
	take, cancel := context.WithDeadline(ctx, deadline)
	return take, cancel, by
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
	if by.IsZero() {
		return stamp, cancel
	}
	bounded, cancelBounded := context.WithDeadline(stamp, by)
	return bounded, func() {
		cancelBounded()
		cancel()
	}
}

// takeRefusal is why a take was refused, in the words its stamp keeps: the
// take's budget where that is what ended it, which says so plainly rather than
// as a deadline error.
func takeRefusal(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, executorkeep.ErrSessionTooLarge):
		return fmt.Sprintf("the session is past the %d bytes a session is kept at, sealed",
			executorkeep.SessionMaxBytes)
	case ctx.Err() != nil:
		return fmt.Sprintf("the session could not be taken out of its box within the read's budget "+
			"(at most %s): %v", SessionTakeBudget, err)
	}
	return fmt.Sprintf("the box's session could not be taken: %v", err)
}

// spool seals taken's body into a file of this node's own, as it streams, under
// an object key minted now.
//
// THE CLOSE IS WHAT BOUNDS IT: a box's stream that stalls blocks a read for as
// long as it stalls, which no check of ctx between reads can interrupt, so the
// body is closed the moment ctx ends.
func (c *Coordinator) spool(ctx context.Context, run PendingRun, stamp SessionCapture,
	taken TakenSession) (*spooledCapture, error) {

	file, err := os.CreateTemp(c.spoolDirectory(), spoolPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("sandbox: a spool for the session on this node's disk: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = taken.Body.Close() })
	sealed, sealErr := c.keep.Seal(run.AgentHandle, run.TurnID, taken.Body, file)
	stop()
	if err = errors.Join(sealErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return nil, err
	}
	return &spooledCapture{run: run, stamp: stamp, spool: sealed, file: file.Name()}, nil
}

// spoolDirectory is where this coordinator's spools wait:
// [CoordinatorOptions.SpoolDir], or the node's temporary directory.
func (c *Coordinator) spoolDirectory() string {
	if c.spoolDir != "" {
		return c.spoolDir
	}
	return os.TempDir()
}

// sweepSpools removes every spool in the node's spool directory older than a
// capture can take — twice [SessionCaptureBudget] — which is what a crash
// between a take and its keep left behind; never a younger one, which may be a
// keep of this very process still under way.
//
// ON EVERY TAKE, not once a process: a node that restarts soon after a crash
// finds that crash's spools young at its first take, and swept once, it left
// them on the node's disk — a few dozen mebibytes each, in a temporary
// directory that may be memory — until its next restart, weeks away. Swept at
// every take, they go at the first take after they age past the cutoff. A
// glob of one directory, beside a take that reads a whole session.
func (c *Coordinator) sweepSpools() {
	matches, err := filepath.Glob(filepath.Join(c.spoolDirectory(), spoolPrefix+"*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-2 * SessionCaptureBudget)
	for _, name := range matches {
		if info, statErr := os.Stat(name); statErr == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(name)
		}
	}
}

// keepLater keeps a spooled capture off the frame that took it, on the
// coordinator's own lifetime ([Coordinator.keepCapture]); nothing for nil. A
// coordinator that is stopping keeps nothing more, and says why on the stamp.
func (c *Coordinator) keepLater(spooled *spooledCapture) {
	if spooled == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.dropSpool(spooled, "the node stopped before the session could be uploaded")
		return
	}
	c.keeping.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.keeping.Done()
		c.keepCapture(c.keepLife, spooled)
	}()
}

// dropSpool refuses a spooled capture that will not be kept, saying why, and
// removes its spool.
func (c *Coordinator) dropSpool(spooled *spooledCapture, why string) {
	defer func() { _ = os.Remove(spooled.file) }()
	c.finishCapture(c.keepLife, time.Time{}, spooled.run, spooled.stamp, CaptureRefused, why)
}

// keepCapture uploads a spooled capture into the object it was sealed for,
// records it on the executor log and stamps what came of it — see the file
// comment — under a budget of its own ([SessionKeepBudget]), and removes the
// spool.
//
// FENCED BY THE TAKER LEASE ITS STAMP NAMES, before the upload and before every
// attempt at the record, each attempt cut at the end of the window it was sent
// in ([takerLease.fence], [Coordinator.recordCapture]): a keep this process's
// lease no longer covers may be one a peer has already settled refused, so it
// uploads and records nothing more ([Coordinator.fenceKeep]).
func (c *Coordinator) keepCapture(ctx context.Context, spooled *spooledCapture) *SessionKept {
	defer func() { _ = os.Remove(spooled.file) }()
	ctx, cancel := context.WithTimeout(ctx, SessionKeepBudget)
	defer cancel()
	run, stamp := spooled.run, spooled.stamp
	minted, err := c.keep.MintedAt(ctx, run.TurnID, run.WorkKey, run.WorkSince)
	if err != nil {
		return c.finishCapture(ctx, time.Time{}, run, stamp, CaptureRefused,
			fmt.Sprintf("the turn's operations could not be minted: %v", err))
	}
	stamp.Minted = minted
	if why := c.overBudget(ctx, run, spooled.spool.Size, stamp.SeatBudget); why != "" {
		return c.finishCapture(ctx, time.Time{}, run, stamp, CaptureRefused, why)
	}
	fence := c.keepFence(stamp)
	if _, why := fence(); why != "" {
		return c.fenceKeep(ctx, run, stamp, "upload", CaptureRefused, why)
	}
	body, err := os.Open(spooled.file)
	if err != nil {
		return c.finishCapture(ctx, time.Time{}, run, stamp, CaptureRefused,
			fmt.Sprintf("the spooled session could not be read back on this node: %v", err))
	}
	sealed, err := c.keep.Upload(ctx, spooled.spool, body)
	_ = body.Close()
	if err != nil {
		return c.finishCapture(ctx, time.Time{}, run, stamp, CaptureRefused, uploadRefusal(err))
	}
	stamp.Object, stamp.ObjectHash = sealed.Object.Key.String(), string(sealed.Object.Hash)
	stamp.ObjectSize, stamp.ContentKey = sealed.Object.Size, sealed.ContentKey
	outcome, why, fenced := c.recordCapture(ctx, run, stamp, fence)
	if fenced {
		return c.fenceKeep(ctx, run, stamp, "record", outcome, why)
	}
	return c.finishCapture(ctx, time.Time{}, run, stamp, outcome, why)
}

// overBudget is why a capture of size bytes cannot fit the seat's budget beside
// the sessions its other LIVE turns hold, or "" where it may: asked before the
// upload, so a seat whose live turns fill its budget does not upload a session
// on every launch that the log then refuses, holding the store's room for a day
// each time. Read off the seat's runs in the coordination store: each live
// turn's kept capture, and — for a live turn that has kept nothing yet — the
// session its conversation's last turn left that it froze ([PendingRun.Base]),
// which its begin holds against the budget's release (each such base once,
// however many turns froze it). The capturing turn's own base is not counted:
// its capture lifts its hold in the same apply. The log decides again at the
// record (a capture it refuses, [LogRefused]), which also counts a turn whose
// end was lost. A listing that cannot be read decides nothing here.
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
			held += other.Kept.ObjectSize
		case other.Base != nil && other.Base.Session != nil && !bases[other.Base.Session.Turn]:
			bases[other.Base.Session.Turn] = true
			held += other.Base.Session.ObjectSize
		}
	}
	if held+size <= budget {
		return ""
	}
	return fmt.Sprintf("the seat's other live turns hold %d bytes of sessions, and this one is %d: past "+
		"the seat's budget of %d (turn_engine.conversation_session.max_bytes_per_seat), so it was not "+
		"uploaded", held, size, budget)
}

// keepLostSession keeps the session of a run whose ending is a LOSS where its box
// may still be read — a launch whose engine restarted before its turn could
// park, reaped as the seat is recovered — before the ending reclaims the box:
// the last read of it anything makes, which ends the job first, as a stop's
// does ([Coordinator.endJob]). On this node's host it is attached and taken
// here; on another node's it is asked of that host ([BoxReadLoss]), which ends
// the job and takes its session there. A box that is gone, and a job of this
// launch already taken whole, are passed over.
//
// ATTACHED, NEVER CONNECTED: a box about to be reclaimed is not woken to be
// read, and one its backend cannot read paused is passed over.
//
// MADE BY THE ENDING'S OWN RETRY, off any frame somebody waits on
// ([Coordinator.finishEnding]): the pack and the take are bounded by their own
// budget, and the keep after them by its own.
func (c *Coordinator) keepLostSession(ctx context.Context, run PendingRun) {
	if !c.capturesLoss(run) {
		return
	}
	if !c.hosts(run) {
		if c.boxes == nil {
			return
		}
		result, _, err := c.boxes.ReadBox(ctx, run, BoxReadLoss)
		if err != nil {
			log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", string(BoxReadLoss),
				"node", run.LaunchNode, "error", err.Error(),
				"detail", "the lost run's box is on another node's host, which did not read it before "+
					"its ending reclaims it")
			return
		}
		if kept := result.Kept; kept != nil && kept.NotKept != "" {
			log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", string(BoxReadLoss),
				"node", run.LaunchNode, "detail", kept.NotKept)
		}
		return
	}
	spooled, _ := c.readLost(ctx, run)
	c.keepLater(spooled)
}

// readLost is the read of a lost run's box on this node's host — for this
// node's own ending, or asked by the node ending it ([BoxReadLoss]): the box
// attached, never woken; its job ended, where it still works
// ([Coordinator.endJob]); and its session taken, final wherever the job
// ended. Nothing else of the box is read: what a lost job spent is no
// question a loss asks.
func (c *Coordinator) readLost(ctx context.Context, run PendingRun) (*spooledCapture, *SessionKept) {
	manager := c.mgr()
	if manager == nil {
		return nil, nil
	}
	box, runner, err := manager.Attach(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		why := fmt.Sprintf("the lost run's box could not be read before it was reclaimed: %v", err)
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", string(BoxReadLoss),
			"detail", why)
		return nil, &SessionKept{NotKept: why}
	}
	over := c.endJob(ctx, run, box, runner, nil)
	return c.takeCapture(ctx, run, box, runner, readCapture(BoxReadLoss, Result{}, over))
}

// capturesLoss reports whether a lost run's ending reads its box for its
// session: a company that keeps sessions ([Coordinator.capturing]), a run whose
// job was in a box, a loss that leaves the box behind ([LossCause.leavesBox] — a
// box lost with its host, or with a node that left, has nothing left to read),
// and no take of this job made once it had ended.
func (c *Coordinator) capturesLoss(run PendingRun) bool {
	if !c.capturing() || run.SandboxID == "" || !run.lost() || keptWhole(run) {
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
// job taken once the job had ended, whatever came of it: nothing since has
// changed the session it holds.
func keptWhole(run PendingRun) bool {
	last := run.Capture
	return last != nil && last.Launch == run.LaunchID && last.LaunchSeq == run.LaunchSeq && last.Final
}

// nextCaptureSeq is the next capture's place among the current launch's, from
// what the row's newest capture says.
func nextCaptureSeq(run PendingRun) int64 {
	if last := run.Capture; last != nil && last.LaunchSeq == run.LaunchSeq {
		return last.CaptureSeq + 1
	}
	return 1
}

// maxScrubBytes bounds what one capture scrubs out by value, in all: what its
// launch handed the box ([MaxHandedBytes]) and its seeded logins read again, in
// the box and on the host ([Coordinator.scrubValues]).
//
// FOUR TIMES WHAT A LAUNCH MAY HAND: a launch at its bound, and every login it
// was seeded with read again twice — a login document is a kibibyte or two —
// still fits with room to spare, and the scrub, which holds every value whole
// in one replacer, costs the host a few mebibytes at the most. The bound is
// what the box's own code cannot move: it can write its login file (its
// harness refreshes the file there), and a file of millions of short strings,
// each a value of the scrub, built a replacer of a gibibyte on the engine
// host before a byte of the session was read. A capture past it is refused.
const maxScrubBytes = 4 * MaxHandedBytes

// scrubValues is every value a capture of run's box scrubs out by value: what
// its launch handed the box ([PendingStore.Handed]), and its seeded logins as
// they are NOW, in the box and on the host, since a refresh mints values the
// launch never saw. [ErrHandedUnknown] where they cannot be known — see
// handed.go — which refuses the capture rather than scrub less; and refused
// too past its bounds — a login read again larger than a whole launch may hand
// ([MaxHandedBytes]), or values past [maxScrubBytes] in all — since the box's
// code can write its login and the scrub holds every value whole.
func (c *Coordinator) scrubValues(ctx context.Context, run PendingRun, box Sandbox) ([]string, error) {
	sealed, err := c.pending.Handed(ctx, run)
	if err != nil {
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
	if total > maxScrubBytes {
		return nil, fmt.Errorf("the values the session would be scrubbed of are %d bytes in all, past "+
			"the %d a capture holds for its scrub, so it is not kept", total, maxScrubBytes)
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

// refuseUnplaced stamps a capture refused before it held a place — the session
// could not be read, or the policy or what the box was handed not known — so
// the turn's end, and whatever reads the row's newest capture, know the
// launch's latest attempt failed rather than reading an older capture as its
// latest.
//
// AT THE PLACE AFTER THE NEWEST THE TAKE READ, which another read of the same
// job may have reserved since — a stop's beside a collection's — and is
// keeping: there the stamp, another take's, is not taken ([stampOnto]), and
// that read's capture stands.
//
// UNDER A CONTEXT OF ITS OWN, by by at the latest, asked again while the store
// does not answer ([Coordinator.stampSettled]): the take's budget may be what
// ended it, and the stamp is what says so. calls is the run's log where the
// refusal had read it, and nil where it had not: its stamp then states no save
// point, "not known" ([SessionCapture.Through]).
func (c *Coordinator) refuseUnplaced(ctx context.Context, by time.Time, run, current PendingRun,
	at capturePoint, calls *BridgeLog, why string) *SessionKept {

	stamp := SessionCapture{LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current),
		Launch: run.LaunchID, State: CaptureRefused, Reason: stampReason(why), Final: at.final,
		Interrupted: at.cutOff, NoteCursor: notesHeld(current, run.LaunchID, at.cutOff), At: c.now().UTC()}
	if calls != nil {
		stamp.placeAt(*calls)
	}
	log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "detail", stamp.Reason)
	c.stampSettled(ctx, by, run.TurnID, stamp)
	return stamp.kept()
}

// finishCapture stamps how a capture came out and reports it.
//
// UNDER A CONTEXT OF ITS OWN, asked again while the store does not answer
// within a record's budget, and never past by where a take is still stamping it
// ([Coordinator.settleContext]; zero off the read): the capture's budget may be
// what ended it, and the stamp is what says so.
func (c *Coordinator) finishCapture(ctx context.Context, by time.Time, run PendingRun, stamp SessionCapture,
	state CaptureState, why string) *SessionKept {

	stamp.State, stamp.Reason = state, stampReason(why)
	c.stampSettled(ctx, by, run.TurnID, stamp)
	switch state {
	case CaptureLanded:
		log.InfoContext(ctx, "executor_session_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "object", stamp.Object,
			"bytes", stamp.ObjectSize, "session", stamp.SessionID)
	default:
		log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "state", string(state),
			"detail", stamp.Reason)
	}
	return stamp.kept()
}

// reserveCapture reserves a capture's place on its run's row — its stamp,
// begun ([PendingStore.StampCapture]) — asked again while the coordination
// store does not answer ([Coordinator.askStamp]), never past by — the take's
// own deadline, where a take reserves it — as [Coordinator.stampSettled] asks
// an outcome: false, with no error, where another read of the same job took the
// place first.
//
// ASKED AGAIN SO A READ LEAVES A STAMP OF WHAT IT TOOK wherever the store lets
// it: a turn's newest capture is known only by its stamp, and its end tells the
// log the turn's newest capture where it was not kept ([unkeptOf]). A place the
// store would not take at all is a capture never made, which the take abandons.
// AND AN EARLIER ASK THE STORE APPLIED, its answer lost, is this take's
// reservation: asked again, the store answers a stamp the row already holds as
// taken, so the take goes on under its own place rather than reading it as
// another read's and walking away from it, begun.
func (c *Coordinator) reserveCapture(ctx context.Context, by time.Time, turnID string,
	stamp SessionCapture) (bool, error) {

	took, _, err := c.askStamp(ctx, by, turnID, stamp, c.pending.StampCapture)
	return took, err
}

// abandonPlace settles the place a take asked to reserve and was never answered
// within its own time ([Coordinator.reserveCapture]). A store that applied the
// reservation and answered too late — or that applies it once its asker has
// given up, its write already on its way — leaves the place begun on the run's
// row, read as a keep still under way for as long as a capture can take
// ([captureInFlight]): the turn's end waits on it, and a relaunch reads it as
// one it cannot continue, with nobody left to settle it.
//
// ONE STAMP, DECIDED ON THE ROW AS IT STANDS when the store writes it — the
// place refused, as THIS take stamped it, at its instant: taken where the row
// holds the take's own begun stamp, which it advances, and where the row holds
// nothing at that place yet, which a reservation still on its way finds
// answered — refused as made against a row that has moved since, or, asked
// again, taking nothing of a place that has its answer ([stampOnto]). Never a
// read of the row and a decision off it: a reservation landing between the two
// was left begun. A place another read reserved, or a newer one, is left as it
// stands, never settled for a take that never held it.
//
// ASKED AGAIN WHILE THE STORE DOES NOT ANSWER, within what the read leaves the
// outcome stamp and never past by ([Coordinator.takeContext]): a reservation
// is answered late by a store having a bad moment — a stream's leader stepping
// down after it took the write — which answers nothing else during it either.
func (c *Coordinator) abandonPlace(ctx context.Context, by time.Time, run PendingRun, stamp SessionCapture,
	at capturePoint, cause error) *SessionKept {

	refused := stamp
	refused.State = CaptureRefused
	refused.Reason = stampReason(fmt.Sprintf("the take's time ran out before the store answered the "+
		"reservation of its place on the run's row, so nothing was taken under it: %v", cause))
	took, _, err := c.askStamp(ctx, by, run.TurnID, refused, c.pending.StampCapture)
	if took {
		log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", refused.LaunchSeq, "capture_seq", refused.CaptureSeq,
			"state", string(CaptureRefused), "detail", refused.Reason)
		return refused.kept()
	}
	detail := "another read of the run holds the capture's place, or a newer one, and keeps the session"
	if err != nil {
		detail = "the capture's place could not be settled on the run's row within the read's time; a " +
			"reservation the store took stands begun until it ages out"
	}
	log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "error", errors.Join(cause, err).Error(), "detail", detail)
	return nil
}

// stampSettled stamps a capture's outcome on its run's row, asked again while
// the coordination store does not answer ([Coordinator.askStamp]), under a
// context of its own, which outlives the caller's: the capture's budget may be
// what ended it. Never past by, where a take is still stamping it
// ([Coordinator.settleContext]).
//
// ASKED AGAIN BECAUSE A STAMP LEFT BEGUN IS READ AS A KEEP STILL UNDER WAY for
// as long as a capture can take ([SessionCaptureBudget], twenty minutes and
// more): the turn's end waits that long on it, and a relaunch reads a capture
// that settled as one it cannot continue. Once the record's budget is spent
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
// record's budget ([sessionRecordBudget]) and under a context of its own, never
// past by ([Coordinator.settleContext]): whether the row took it, how many times
// it was asked, and the store's last error where it never answered — the one
// way of asking every stamp of a capture takes.
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
// own, which outlives the caller's, within one record's budget — and never past
// by, where the read a take is part of needs the rest of its time for the box's
// pause, as [stampContext] bounds a take's single record. by zero, a stamp made
// off the read, is bounded by the record's budget alone.
func (c *Coordinator) settleContext(ctx context.Context, by time.Time) (context.Context, context.CancelFunc) {
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.recordBudget)
	if by.IsZero() {
		return settle, cancel
	}
	bounded, cancelBounded := context.WithDeadline(settle, by)
	return bounded, func() {
		cancelBounded()
		cancel()
	}
}

// uploadRefusal is why an upload of a session was refused, in the words its
// stamp keeps.
func uploadRefusal(err error) string {
	switch {
	case errors.Is(err, objstore.ErrFull):
		return "the object store's byte reservation is spent, so the session was not uploaded: " +
			"raise store.objects' reservation or move the objects to a bucket"
	case errors.Is(err, executorkeep.ErrSessionTooLarge):
		return fmt.Sprintf("the session's archive is past the %d bytes a session is kept at",
			executorkeep.SessionMaxBytes)
	}
	return fmt.Sprintf("the session could not be uploaded: %v", err)
}

// recordCapture records a stamped, uploaded capture of run's turn on the
// executor log under the operation its place derives ([SessionWrites.Capture]),
// asked again under that same operation while no answer settles it, within
// [sessionRecordBudget]: landed, refused (and why), or unresolved.
//
// EVERY CAPTURE CARRIES THE TURN'S FIRST READ as its first launch froze it
// ([PendingRun.Base]), never a read made now: the log keeps the read the turn's
// first landed record carried — its begin, or where none landed, its first
// capture — so a capture made after a sibling of the conversation ended still
// names the read the turn was told of, and its end absorbs exactly that.
//
// A KEEP'S FENCE IS ASKED BEFORE EVERY ATTEMPT ([takerLease.fence]; nil for
// none — an ask again of an unresolved record, which settles what was sent
// rather than keeps): fenced, nothing more is sent, and the answer is fenced —
// refused where no attempt had been sent, so no record can land, and
// unresolved where one had, which a wait asks again under its own operation
// rather than settle refused under a record that may yet have landed.
//
// AND EVERY ATTEMPT IS CUT AT THE END OF THE WINDOW IT WAS SENT IN, the instant
// the fence answered: bounded by the record's budget alone, an attempt sent a
// moment before the window closed went on for up to that budget — thirty
// seconds, well past the window's margin of one renewal interval, and spent
// under the very trouble that lapses a lease — and could land after a peer
// had read the lease lapsed and settled the capture refused. Cut there, the
// attempt is answered unresolved, and the next one finds the window renewed
// or is fenced.
func (c *Coordinator) recordCapture(ctx context.Context, run PendingRun, stamp SessionCapture,
	fence func() (time.Time, string)) (outcome CaptureState, why string, fenced bool) {

	handle, turn := run.AgentHandle, run.TurnID
	key, err := objstore.ParseKey(stamp.Object)
	if err != nil {
		return CaptureRefused, fmt.Sprintf("the capture names no object a record can: %v", err), false
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
			outcome, why = CaptureRefused, fmt.Sprint(err)
			return true, nil
		}
		outcome, why = CaptureUnresolved, fmt.Sprint(err)
		return false, err
	})
	return outcome, why, fenced
}

// beginRecordBudget bounds the record of a turn's begin: every attempt at it,
// under its one operation, together.
//
// FIFTEEN SECONDS, on the frame that launches: that frame holds the seat and a
// slot of the node's admission, so it does not wait for the log the way an
// end's own retry does ([sessionRecordBudget]). Three of the publisher's
// five-second waits (statelog.DefaultResolveBudget), for the reason the
// relaunch's wait for a capture is three ([relaunchCaptureWait]): a write the
// log takes answers in milliseconds, and three waits are room for a store
// having a slow moment. A begin not held by then is one a slow log or a
// stopped node is holding: the launch continues no base, and a begin that
// lands later holds no longer than the turn's first capture or its end.
const beginRecordBudget = 15 * time.Second

// recordBegin records run's turn's first read of its conversation on the
// executor log — the read its first launch froze on the record ([PendingRun.Base])
// — as the turn's begin ([SessionWrites.Begin]), under the one operation the
// turn derives, asked again under it while no answer settles it, within [beginRecordBudget]. A read that froze a base
// names it, whatever the launch then takes up, so a later relaunch can still
// continue it. Logged, never failed on: a begin the log does not hold leaves
// the turn's launches continuing no base ([BaseContinuityOf]), which is what
// the turn's own row then says. A coordinator with no executor log (a test's)
// records nothing.
//
// It answers whether the log read the turn's row back holding the base the read
// froze ([BeginAnswer.Held]): read on the serving data node once
// the begin applied, in log order, which is what a read of the row would say
// now — nothing but the turn's own first capture or its end lifts a hold, and
// neither comes before this launch's run starts — so the launch takes the
// base up on it rather than reading the row again ([Coordinator.continueBase]).
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
// sent and not answered, and stamps what the log answers: landed, refused, or
// still unresolved — the stamp never past by, where a caller's own wait bounds
// it (zero for none: [Coordinator.settleContext]). One whose object is past
// objstore.RecordWithin is refused without asking: no record may name it any
// more, and the collector may already be reclaiming it.
func (c *Coordinator) settleCapture(ctx context.Context, by time.Time, run PendingRun,
	last SessionCapture) SessionCapture {

	key, err := objstore.ParseKey(last.Object)
	if err == nil && c.now().Sub(key.Minted()) > objstore.RecordWithin {
		last.State, last.Reason = CaptureRefused, fmt.Sprintf("its record was never answered within the "+
			"%s an object may be named in", objstore.RecordWithin)
	} else {
		last.State, last.Reason, _ = c.recordCapture(ctx, run, last, nil)
		last.Reason = stampReason(last.Reason)
	}
	c.stampSettled(ctx, by, run.TurnID, last)
	return last
}

// settleUnresolved asks run's newest capture again where its record was sent
// and never answered ([Coordinator.settleCapture]), never past by, and answers
// the row as it stands after; run as it was where its newest capture is not
// unresolved. Asked by the turn's end before it records the end
// ([Coordinator.publishEnd]), and by a relaunch before it decides which
// session it continues ([Coordinator.ContinuedCapture]).
//
// WHAT THE ASK JUST SETTLED, where the row cannot be read back to show it:
// stamped on run by the row's own rule ([stampOnto]). Read off the copy as it
// was, an end named a capture that had just landed as one never answered, and
// a relaunch would not continue it.
func (c *Coordinator) settleUnresolved(ctx context.Context, by time.Time, run PendingRun) PendingRun {
	newest := run.Capture
	if newest == nil || newest.State != CaptureUnresolved {
		return run
	}
	settled := c.settleCapture(ctx, by, run, *newest)
	if current, found, err := c.pending.Get(ctx, run.TurnID); err == nil && found {
		return current
	}
	stampOnto(&run, settled)
	return run
}

// takerStoppedWhy is why a capture whose taker stopped before its record landed
// is refused, as its stamp and the turn's end say it.
func takerStoppedWhy(taker string) string {
	return fmt.Sprintf("its taker %s stopped before it was kept", nodeOf(taker))
}

// settleGoneTaker settles run's newest capture where it is begun and its
// taker is gone ([Coordinator.takerGone]) — see the file comment — and answers
// the row as it stands after, and false where the row is gone. Nothing is
// written where the taker's lease still holds at the epoch the stamp names,
// where the store cannot say whether it does, where this node has no log to
// read, or where the log does not answer: the capture is then waited for as
// before, and asked again at the wait's next pass.
//
// FROM THE TURN'S ROW ON THE EXECUTOR LOG ([SessionRestore.Session]), read at
// the level a relaunch reads it — linearizable, so the read has seen every
// record the log took before it. Where the row names the capture's place, its
// record LANDED, and the stamp is written landed from the row: the object it
// names and what its last request carried, which only the keep that ended
// knew. Where the row names another place, or the log holds no row for the
// turn, the stamp is written refused, saying so: the taker's own fence ends
// every attempt at the record inside the window its lease covered
// ([takerLease.fence]), which closed before the lease could be read lapsed
// (the file comment says what that leaves).
//
// ONLY OVER THE BEGUN STAMP IT WAS DECIDED FROM ([PendingStore.SettleBegun]),
// decided by the store on the row as it stands: a stamp the taker wrote since
// stands — its landing, its refusal, or an unresolved record it sent, which
// may yet land and is asked again under its own operation rather than settled
// refused under it — and so does a newer take's. And once written, the taker's
// own stamp coming after is refused by the row's rule ([stampOnto]): a place
// settled is never taken back.
func (c *Coordinator) settleGoneTaker(ctx context.Context, by time.Time, run PendingRun) (PendingRun, bool) {
	begun := run.Capture
	if begun == nil || c.restore == nil {
		return run, true
	}
	if gone, err := c.takerGone(ctx, *begun); err != nil || !gone {
		return run, true
	}
	row, err := c.restore.Session(ctx, run.AgentHandle, run.TurnID)
	if err != nil && !errors.Is(err, ErrTurnRowNotFound) {
		return run, true
	}
	settled := *begun
	if err == nil && row.LaunchSeq == begun.LaunchSeq && row.CaptureSeq == begun.CaptureSeq {
		settled.State, settled.Reason = CaptureLanded, ""
		if row.HasObject() {
			settled.Object, settled.ObjectHash = row.Object.Key.String(), string(row.Object.Hash)
			settled.ObjectSize, settled.ContentKey = row.Object.Size, row.ContentKey
		}
		settled.ContextTokens = row.ContextTokens
	} else {
		settled.State, settled.Reason = CaptureRefused, stampReason(takerStoppedWhy(begun.Taker))
	}
	took := c.settleStamp(ctx, by, run.TurnID, settled, c.pending.SettleBegun)
	if took {
		log.InfoContext(ctx, "executor_session_settled", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", settled.LaunchSeq, "capture_seq", settled.CaptureSeq, "taker", nodeOf(begun.Taker),
			"taker_epoch", begun.TakerEpoch, "state", string(settled.State),
			"detail", "the lease the capture's taker kept it under has lapsed or moved on, so what came of it "+
				"was read off the turn's row on the executor log rather than waited for")
	}
	current, found, getErr := c.pending.Get(ctx, run.TurnID)
	switch {
	case getErr != nil:
		// WHAT THIS SETTLED, where the row cannot be read back to show it, as
		// [Coordinator.settleUnresolved] keeps what its ask settled — and only
		// a settle the store took: one it did not may have found the row moved
		// on, and the wait's next pass reads the row again.
		if took {
			stampOnto(&run, settled)
		}
		return run, true
	case !found:
		return PendingRun{}, false
	}
	return current, true
}

// captureInFlight reports whether run's turn has a capture still being taken or
// kept, as of now: stamped begun, and younger than a capture can take
// ([SessionCaptureBudget]). An older one's node stopped, or its budget ran out,
// before it could say what came of it. This is its age alone: a wait for it
// asks besides whether its taker is gone ([Coordinator.settleGoneTaker]).
func captureInFlight(run PendingRun, now time.Time) bool {
	c := run.Capture
	return c != nil && c.State == CaptureBegun && now.Sub(c.At) < SessionCaptureBudget
}

// awaitCapture is run's row once no capture of its turn is in flight
// ([captureInFlight]) — read again every [captureWaitTick], for at most within
// — and whether it is still there; or why the wait could not say: ctx ended
// first, or the row could not be read when the wait ran out.
//
// WHY AN END WAITS, for as long as a capture can take ([SessionCaptureBudget]):
// the end tombstones its turn, and a capture whose record lands after it is
// refused, so ended first, the turn's last session would be lost to the next
// turn. A keep is made off the read that took it, on the node whose host the
// box is on, which may be another node; the row is what both read. An end
// waits off every frame somebody waits on (its ending's own retry).
//
// A RELAUNCH WAITS FAR LESS ([relaunchCaptureWait]), on the frame that
// relaunches: see restore.go. Its wait running out on a row it did read leaves
// the capture in flight there, which the relaunch reads as one it does not
// wait out.
//
// A CAPTURE WHOSE TAKER IS GONE IS SETTLED, NOT WAITED OUT: every pass over a
// row it did read asks whether a begun capture's taker lease still holds at
// the epoch its stamp names — one coordination read — and settles it from the
// turn's row on the executor log where it does not
// ([Coordinator.settleGoneTaker]), so an end or a relaunch that follows its
// taker's crash, restart or exit waits at most for that lease's TTL and one
// read of the log rather than the capture's budget. A stamp with no taker, and
// one whose taker's lease holds or cannot be read, are waited for by their age
// as before: a draining taker holds its lease until its last keep ends.
//
// A READ THAT FAILS IS NO ANSWER, and the wait goes on: the capture is still
// begun on the last row this read, and nothing says it has settled. The wait
// used to end on it, with that stale row, and the end it was holding back was
// recorded over a capture still being kept — the end's tombstone refused the
// turn's last session, and the end named it begun and never finished, which it
// was not. An end is never recorded from a row the wait could not read: once
// the capture's own budget is out with the last read failed, or the wait's
// budget is, or ctx ended, the caller keeps its ending and asks again on its
// own spacing; and a relaunch continues no kept session off such a row.
//
// PAST THE BUDGET ON A ROW IT DID READ, for an end's wait the capture's own
// budget is out too, on this node's clock, and the end is recorded naming it
// begun and never finished ([unkeptOf]): its node stopped, or its budget ran
// out.
func (c *Coordinator) awaitCapture(ctx context.Context, run PendingRun,
	within time.Duration) (PendingRun, bool, error) {

	// NEVER LONGER THAN within, whatever the stamp's clock says: a stamp whose
	// node's clock runs ahead is waited out on this one's. And a settle's
	// stamp is made within it too.
	wait, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	by, _ := wait.Deadline()
	var unread error
	for {
		if unread == nil {
			var found bool
			if run, found = c.settleGoneTaker(wait, by, run); !found {
				return PendingRun{}, false, nil
			}
		}
		inFlight := captureInFlight(run, c.now())
		switch {
		case !inFlight && unread == nil:
			return run, true, nil
		case !inFlight:
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
	why := newest.Reason
	switch newest.State {
	case CaptureUnresolved:
		why = "its record was sent to the executor log and never answered"
	case CaptureBegun:
		why = "it was begun and never finished: the node taking it stopped, or its budget ran out"
	}
	return &executorkeep.Unkept{LaunchSeq: newest.LaunchSeq, CaptureSeq: newest.CaptureSeq,
		Reason: redact.Secrets(why)}
}

// wentOnPast reports whether the turn went on past the session its row on the
// executor log names ([executorkeep.Ending.WentOn]): run kept a capture, and it
// is not the final capture of the turn's last launch — one taken while its job
// still worked, or an earlier launch's, the jobs after it keeping nothing newer.
// False where the turn kept none: its row then names no session to say it of.
//
// ITS LAST LAUNCH'S FINAL CAPTURE IS THE TURN'S LAST WORD, and the one: nothing
// of a job is taken once a take found it ended ([Coordinator.takeCapture]), so
// an end that says nothing of the kind names no unkept capture newer than the
// one it kept ([unkeptOf]). Every unkept capture an end names beside a kept one
// rides an end that says the turn went on past it.
func wentOnPast(run PendingRun) bool {
	kept := run.Kept
	return kept != nil && (!kept.Final || kept.Launch != run.LaunchID)
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
}

// turnEndOf is the end of the turn run's row belongs to, ending as ending.
func turnEndOf(run PendingRun, ending executorkeep.Ending) TurnEnd {
	return TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID, ConversationKey: run.ConversationKey,
		WorkKey: run.WorkKey, WorkSince: run.WorkSince, Ask: run.TaskDescription, Ending: ending}
}

// OweEnd records on the turn's run row how the turn ended
// ([PendingStore.OweEnd]), for the run's ending to record on the executor log
// before it deletes the row ([Coordinator.finishEnding]) — the path every end
// takes whose turn still has a row. Answers whether the row now owes it, or
// has recorded the turn's end already ([PendingRun.EndRecorded]): false where
// the turn has no row, which the caller records itself ([Coordinator.EndTurn]).
//
// DECIDED ONCE: a turn that owes an end already keeps it, and this one is
// dropped, which is what lets two paths that end one turn end it once.
//
// UNDER A CONTEXT OF ITS OWN ([detached]): a frame that ends a turn is often
// one a drain is cancelling, and an end that frame could not owe was recorded
// at once instead — before the turn's captures settled, and without what they
// came to.
//
// FITTED TO THE EXECUTOR LOG'S BOUNDS BEFORE IT RIDES THE ROW
// ([executorkeep.Ending.Fit]): its prose is a model's and a guard's — a
// reviewer's completed work, a failure's text — which nothing bounds before
// here, and the row's budget measures an owed end as this writes it, every
// field at its bound escaped at its worst (rowbytes.go,
// TestTheWholeRowFitsItsBudget). The log fits it the same way when it records
// it, so what the row owes is what the log will hold.
func (c *Coordinator) OweEnd(ctx context.Context, turnID string, ending executorkeep.Ending) (bool, error) {
	raw, err := owedEnd(turnID, ending)
	if err != nil {
		return false, err
	}
	ctx, cancel := detached(ctx)
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
	owing, _, err := c.pending.OweEnd(ctx, run.TurnID, raw)
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
// ([Coordinator.recordEnd]) — after asking again a capture of the turn still
// unresolved, with the turn's newest capture where it was not kept
// ([unkeptOf]), and saying whether the turn went on past the session its row
// names ([wentOnPast]) — and clears it from the row, marking it recorded.
// Returns the row as it stands after, or why the end is still owed.
//
// THE END'S CAPTURE FIELDS ARE THE ROW'S TO SAY, never the frame's that owed
// the end: they are decided here, after the turn's captures settled, over any
// the owed end carried — a frame decides how the turn ended, not what of its
// session was kept.
//
// # An end a newer build owed is left for a node that reads it
//
// A rolling upgrade mixes builds, and the seat can move from the node that
// owed the end to one running this build. An end holding a key this build
// does not know, or an outcome it does not know, is DEFERRED rather than
// published: decoded into this build's [executorkeep.Ending] and encoded
// again, the keys it could not read were dropped from the turn's account; and
// an outcome it does not know, refused by this build's log as malformed, was
// taken for an end the log will never take and cleared — the turn's end never
// recorded. Left owed, the ending is kept, the seat's mail waits on it as on any
// owed end, and a node of the build that wrote it records it, whichever node
// holds the seat once the upgrade reaches it. Never re-encoded with less, which
// is the rule every record of the fleet's builds keeps: what a build cannot read
// is deferred, never dropped.
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
		run = c.settleUnresolved(ctx, time.Time{}, run)
		ending.Unkept, ending.WentOn = unkeptOf(run), wentOnPast(run)
		ending.Exhausted = run.ExhaustedSessions
		if recordErr := c.recordEnd(ctx, turnEndOf(run, ending)); recordErr != nil {
			return run, recordErr
		}
	}
	if _, clearErr := c.pending.ClearEnd(ctx, run.TurnID, run.OwedEnd); clearErr != nil {
		return run, fmt.Errorf("sandbox: clearing the end turn %s owed, which the executor log has: %w",
			run.TurnID, clearErr)
	}
	run.OwedEnd, run.EndRecorded = nil, true
	return run, nil
}

// EndTurn records how a turn ended for a FRAME that found no run row to owe it
// on ([Coordinator.OweEnd] answered false, or could not be written): a launch
// the turn's fence refused once its row existed, whose ending deleted the row,
// or a row the seat's next holder finished.
//
// WHERE THE ROW IS THERE AFTER ALL — an owe that failed on a store that answers
// again, a row read gone that was not — the end is owed on it, and the run's
// ending records it once the turn's captures settle and with what they came
// to ([Coordinator.publishEnd]). Only a row that is gone, which took its
// capture stamps with it, has its end recorded now, as it stands: best effort,
// and an end the log has already is the answer.
//
// A coordinator with no executor log (a test's) writes nothing.
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
// turn has no row left reaches it through [Coordinator.EndTurn].
func (c *Coordinator) recordEnd(ctx context.Context, end TurnEnd) error {
	if !c.keepsSessions() {
		return nil
	}
	end.Ending.Failure = redact.Secrets(end.Ending.Failure)
	minted, err := c.keep.MintedAt(ctx, end.TurnID, end.WorkKey, end.WorkSince)
	if err != nil {
		return fmt.Errorf("sandbox: the operations of turn %s could not be minted to record its end: %w",
			end.TurnID, err)
	}
	record := EndRecord{Handle: end.Handle, Turn: end.TurnID, ConversationKey: end.ConversationKey,
		WorkKey: end.WorkKey, Minted: minted, Ask: end.Ask, Ending: end.Ending}
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
