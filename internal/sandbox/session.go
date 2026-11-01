package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EACH LAUNCH'S HARNESS SESSION, KEPT, and each turn's END, recorded
// (ADR-0073).
//
// # Captured wherever its box can still be read
//
// A run's harness keeps its conversation in its box, and a box does not outlive
// its turn. So the session is taken out at every point the box can still be
// read, ON THE NODE WHOSE HOST THE BOX IS ON (ADR-0065), before the box is
// paused or reclaimed ([Coordinator.keepSession]):
//
//   - a completion's collection, the archive the run's script packed when its
//     harness exited — or, for a job that never got to (one its turn's cap
//     ended while it still ran, one that failed before it packed, one lost
//     with its engine whose box is still there), one packed now;
//   - a stop's read of what the run had spent, packed now;
//   - the ending of a run LOST with its box still on this node's host (a
//     launch whose engine restarted before its turn could park), packed now,
//     before the ending reclaims the box.
//
// A box that is GONE — lost with its host, or with a node that left the fleet —
// has nothing left to read, and keeps nothing.
//
// # One pipeline, a piece at a time
//
// The box's archive → the scrub of every secret the engine put in the box and
// the redaction every text leaving a box passes, file by file
// ([SessionTaker]) → the streaming seal under the fleet keyring, bound to the
// seat, the turn and the object → the object store's streaming put → the
// `capture` record on the executor log, through the estate router's writes, so a
// seats-only node records one through a data node ([SessionWrites]). Nothing on
// the way holds more than a piece of the archive.
//
// # Every capture is stamped on the run's row
//
// Its place — the launch's ordinal ([PendingRun.LaunchSeq]) and its own among
// that launch's captures — is reserved on the row before a byte moves
// ([PendingStore.StampCapture]), with the bridged call the job had answered
// through and the note it had read through when it was packed, so a later
// launch that continues it knows what came after it. How it came out is
// stamped beside: LANDED, REFUSED with why (an upload the store had no room
// for, a session past its bound, a record the log will never take), or
// UNRESOLVED — uploaded, and whether its record landed unknown — which the
// turn's end asks again under the same operation before it ends the turn
// ([Coordinator.EndTurn]). A later launch never continues an older capture
// without knowing a newer one was refused.
//
// # And every turn's end
//
// How the turn ended — completed or failed, the reviewer's decision, whether
// its reply reached the party waiting, who stopped it and why, what lost it —
// is the account a transcript cannot hold, and the next turn on the
// conversation is told it ([executordomain.Ending]). It is published at every
// way a turn ends, before the seat settles: by the engine's resumed turn, and by
// this package's endings that no frame of the turn reaches — a loss, a stop
// ([CoordinatorOptions.TurnLost], [CoordinatorOptions.CloseStopped]). A turn
// with no capture still ends: its row names no object, and holds the account.
//
// # The switch
//
// `turn_engine.conversation_session.enabled: false` keeps nothing — no capture
// is taken or stamped — and still records every end, which is the account and
// not the session ([SessionPolicy.Keep]).

// SessionWrites is the executor log's writes a coordinator records through: the
// estate router's (internal/estate's ExecutorWrites), so a node without `data`
// records through a data node, and a node with it through its own copy — never
// a data node's local writer, which a seats-only node does not have.
type SessionWrites interface {
	Capture(ctx context.Context, c executordomain.Call, n executordomain.NewCapture) (executordomain.CaptureResult, error)
	End(ctx context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error)
}

// SessionKeep is what keeping a session needs of the engine beside the log's
// writes, which this package cannot read for itself.
type SessionKeep interface {
	// Policy is whether sessions are kept, and the per-seat budget a capture
	// carries, as the revision the FLEET runs states them, or why this node
	// cannot know — every node applies the budget a capture carries, so no
	// node's own revision may decide it.
	Policy(ctx context.Context) (SessionPolicy, error)

	// Secrets is every value the engine put in run's box by value that is a
	// secret — its login's tokens, its environment's credentials — for the
	// scrub a capture streams through ([SessionTake.Scrub]).
	Secrets(ctx context.Context, run PendingRun) []string

	// MintedAt is the instant a turn's derived operations are minted at: its
	// work's start, or the instant the turn was rebased onto where that start
	// is past the operation ledger's horizon (statelog.MintAt), as every
	// operation id a turn derives is.
	MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error)

	// Put seals archive under the fleet keyring as it streams and uploads it
	// into an object the store mints (executordomain.SessionStore.Put). It
	// closes archive.
	Put(ctx context.Context, handle, turn string, archive io.ReadCloser) (executordomain.SealedObject, error)
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

// SessionKept is what a box read's capture of the job's session came to, for
// the launch's own record (`executor_collected`), on whichever node collects:
// it rides the read's result back from the box's host.
type SessionKept struct {
	// CaptureSeq is which of the launch's captures it was.
	CaptureSeq int64

	// Object and Bytes are the object the executor log names now, and its
	// size, where the capture landed; empty where it did not.
	Object string
	Bytes  int64

	// NotKept is why the capture is not one a later launch can continue: it
	// was refused, or its record's outcome is not known yet. Empty where it
	// landed.
	NotKept string
}

// CaptureState is how one capture of a run's session stands on the run's row.
type CaptureState string

const (
	// CaptureBegun is a capture whose place is reserved and whose archive
	// is on its way out of the box.
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

	// Final says the job had ended when the capture was taken, so nothing
	// after it changes the session in its box: a later read of the same job
	// takes it again for nothing.
	Final bool `json:"final,omitempty"`

	// BridgeSeq is the last bridged call ([BridgeCall.Seq]) the row held,
	// and NoteCursor the last note read ([RunSteer.Reads]), when the archive
	// was packed: what the session it holds had been answered and told. A
	// launch that continues it is told what came after.
	BridgeSeq  int `json:"bridge_seq,omitempty"`
	NoteCursor int `json:"note_cursor,omitempty"`

	// At is when it was taken, on this node's clock.
	At time.Time `json:"at"`

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

// kept is the capture as a box read reports it ([SessionKept]).
func (c SessionCapture) kept() *SessionKept {
	k := &SessionKept{CaptureSeq: c.CaptureSeq}
	switch c.State {
	case CaptureLanded:
		k.Object, k.Bytes = c.Object, c.ObjectSize
	case CaptureUnresolved:
		k.NotKept = "its record was sent and not answered; the turn's end asks again"
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

// sessionAuthor is who every executor record this package publishes names: the
// engine's sandbox runtime, which no person and no model drives.
var sessionAuthor = iam.Actor{Name: "sandbox", Kind: iam.ActorSystem}

// sessionRecordBudget bounds one record of a capture or an end: every attempt
// at it, under its one operation, together.
//
// THIRTY SECONDS. A write the log takes is answered in milliseconds; one its
// serving node has published and not yet applied is answered within the
// publisher's own five-second wait (statelog.DefaultResolveBudget); and a write
// sent to a data node that died under it is asked of the next data node by the
// router itself. Thirty seconds outlasts all of that and a broker's leader
// election besides. A log that has not answered by then is an outage its own
// alarms name, and the turn is not held through it: an unresolved capture is
// asked again by the turn's end, and an end the log never took is ended `lost`
// by the retention duty a day after the turn's last capture
// (executordomain.LostAfter).
const sessionRecordBudget = 30 * time.Second

// endTurnBudget bounds [Coordinator.EndTurn]: a record of a capture still
// unresolved, then the end's own, each within [sessionRecordBudget].
const endTurnBudget = 2 * sessionRecordBudget

// sessionRetryBase and sessionRetryCeiling space the attempts at one record: a
// second, doubling to eight — six attempts inside [sessionRecordBudget].
const (
	sessionRetryBase    = time.Second
	sessionRetryCeiling = 8 * time.Second
)

// SessionCaptureBudget is the most a capture adds to the read of a box it is
// taken in: the pack ([SessionPackWait]), the upload of a sealed session at its
// bound at the object store's slowest pace (objstore.PaceFor of
// executordomain.SealedMaxBytes: thirty-three mebibytes begun, at thirty
// seconds each), and the record ([sessionRecordBudget]).
//
// SIZED ON THE BOUND, NOT ON THE ARCHIVE, because a node that asks another to
// read a box ([BoxReadBudget]) cannot know how large the session in it is
// before the read: a session of an ordinary day's work is a mebibyte or two and
// takes seconds, and the bound is what a host that is still working is given
// before the asker takes it for one that is not.
var SessionCaptureBudget = SessionPackWait + objstore.PaceFor(executordomain.SealedMaxBytes) +
	sessionRecordBudget

// capturePoint is where in a run's life a capture is taken.
type capturePoint struct {
	// name says which, for the log.
	name string
	// pack says the session is packed now rather than taken as the run's
	// script left it ([SessionTake.Pack]).
	pack bool
	// final says the job had ended ([SessionCapture.Final]).
	final bool
}

// collectionCapture is the capture a completion's collection takes: the
// archive the run's script packed where it packed one and its job ended, and
// one packed now otherwise — a job its turn's cap ended while it ran, which is
// not final, or one that ended without packing (a crash, or a loss with its box
// left), which is.
func collectionCapture(result Result, ended bool) capturePoint {
	return capturePoint{name: "collection", pack: ended || result.Captured == nil, final: !ended}
}

var (
	// stopCapture is a stop's read: the job was working when a person
	// stopped it, and its box is reclaimed next.
	stopCapture = capturePoint{name: "stop", pack: true, final: true}

	// lossCapture is a lost run's box, read before its ending reclaims it.
	lossCapture = capturePoint{name: "loss", pack: true, final: true}
)

// keepSession takes the session of run's job out of box, keeps it, and stamps
// how it went on the run's row — see the file comment — reporting what came of
// it, nil where nothing was taken: no executor log wired to this coordinator, a
// runner whose harness carries no session, the switch off, a box holding no
// session, or a job of this launch already kept whole.
//
// BEST EFFORT AND BOUNDED ([SessionCaptureBudget]): what the read it is part of
// does is never the capture's to decide, so every failure is stamped and
// logged and the read goes on.
func (c *Coordinator) keepSession(ctx context.Context, run PendingRun, box Sandbox, runner Runner,
	at capturePoint) *SessionKept {

	if c.sessions == nil || c.keep == nil {
		return nil
	}
	taker, ok := runner.(SessionTaker)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, SessionCaptureBudget)
	defer cancel()
	// THE ROW AS IT STANDS, for the ordinals and what the job had been told:
	// the reader's copy may be the claim's, from before the job's last calls.
	current, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"error", err.Error(), "detail", "the run's row could not be read to place the capture")
		return nil
	case !found:
		return nil
	case current.LaunchSeq <= 0:
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"detail", "the job was launched by a build that numbered no launch, so no capture of it "+
				"can be ordered; the turn's next launch is numbered and kept")
		return nil
	}
	if last := current.Capture; last != nil && last.LaunchSeq == current.LaunchSeq &&
		last.Launch == run.LaunchID && last.Final {
		// THE JOB WAS KEPT WHOLE ALREADY — its collection retried after a
		// resume that failed, or read again for a stop — and nothing has
		// changed its session since: kept again, it would be another upload
		// of the same bytes, holding the store's room for a day.
		switch last.State {
		case CaptureLanded:
			return last.kept()
		case CaptureUnresolved:
			return c.settleCapture(ctx, current, *last).kept()
		}
	}
	policy, err := c.keep.Policy(ctx)
	if err != nil {
		return c.refuseUnplaced(ctx, run, current, at, fmt.Sprintf("the company's session policy could "+
			"not be read off the revision the fleet runs: %v", err))
	}
	if !policy.Keep {
		return nil
	}
	taken, err := taker.TakeSession(ctx, box, SessionTake{Pack: at.pack, Scrub: c.keep.Secrets(ctx, run)})
	if err != nil {
		return c.refuseUnplaced(ctx, run, current, at, fmt.Sprintf("the box's session could not be "+
			"read: %v", err))
	}
	if taken.Body == nil {
		return nil
	}
	defer func() { _ = taken.Body.Close() }()
	stamp := SessionCapture{
		LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current), Launch: run.LaunchID,
		State: CaptureBegun, Final: at.final,
		BridgeSeq: current.LastBridgeSeq(), NoteCursor: current.Steer.Reads, At: c.now().UTC(),
		Harness: taken.Archive.Harness, HarnessVersion: taken.Archive.Version,
		SessionID: taken.Archive.SessionID, ArchiveFormat: taken.Archive.Format,
		Home: box.Home(), Workspace: path.Join(box.Home(), WorkspaceSubdir),
		SeatBudget: policy.SeatBudget,
	}
	if took, stampErr := c.pending.StampCapture(ctx, run.TurnID, stamp); stampErr != nil || !took {
		// ANOTHER READ OF THE SAME RUN TOOK THIS PLACE FIRST — a stop's and a
		// collection's, at once — and keeps the session itself; or the row
		// could not be written, which leaves nowhere to say what came of it.
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"capture_seq", stamp.CaptureSeq, "error", fmt.Sprint(stampErr),
			"detail", "the capture's place could not be reserved on the run's row")
		return nil
	}
	if stamp.Minted, err = c.keep.MintedAt(ctx, run.TurnID, run.WorkKey, run.WorkSince); err != nil {
		return c.finishCapture(ctx, run, stamp, CaptureRefused,
			fmt.Sprintf("the turn's operations could not be minted: %v", err))
	}
	if taken.Archive.Size > executordomain.SessionMaxBytes {
		return c.finishCapture(ctx, run, stamp, CaptureRefused, fmt.Sprintf("the session's archive is "+
			"%d bytes, past the %d a session is kept at", taken.Archive.Size, executordomain.SessionMaxBytes))
	}
	sealed, err := c.keep.Put(ctx, run.AgentHandle, run.TurnID, taken.Body)
	if err != nil {
		return c.finishCapture(ctx, run, stamp, CaptureRefused, uploadRefusal(err))
	}
	stamp.Object, stamp.ObjectHash = sealed.Object.Key.String(), string(sealed.Object.Hash)
	stamp.ObjectSize, stamp.ContentKey = sealed.Object.Size, sealed.ContentKey
	if keepErr := taken.Keep(ctx); keepErr != nil {
		// NOT A REFUSAL: the session is kept. What is lost is the box's
		// knowledge that it holds this archive, so a later launch that carries
		// it back into this very box restores it over the copy the box held —
		// which went no further than this capture unless a later one of the
		// box failed, which is what the digest protects against.
		log.WarnContext(ctx, "executor_session_digest_unkept", "turn_id", run.TurnID,
			"session", stamp.SessionID, "error", keepErr.Error(),
			"detail", "the kept archive's digest could not be recorded in its box")
	}
	outcome, why := c.recordCapture(ctx, run.AgentHandle, run.TurnID, run.ConversationKey, run.WorkKey, stamp)
	return c.finishCapture(ctx, run, stamp, outcome, why)
}

// keepLostSession keeps the session of a run whose ending is a LOSS, where its
// box is still on this node's host — a launch whose engine restarted before its
// turn could park, reaped as the seat is recovered — before the ending reclaims
// the box: the last read of it anything makes. A box that is gone, one on
// another node's host (which its host reclaims, and nothing here reads), and a
// job of this launch already kept whole are passed over without a read.
//
// ATTACHED, NEVER CONNECTED: a box about to be reclaimed is not woken to be
// read, and one its backend cannot read paused is passed over.
//
// INSIDE THE ENDING'S OWN FRAME, and bounded by it: an ending is never held for
// a capture past what the frame that decided it allows, so a session too large
// to upload in that is refused, and stamped so.
func (c *Coordinator) keepLostSession(ctx context.Context, run PendingRun) {
	if c.sessions == nil || c.keep == nil || run.SandboxID == "" || !c.hosts(run) || keptWhole(run) {
		return
	}
	manager := c.mgr()
	if manager == nil {
		return
	}
	box, runner, err := manager.Attach(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", lossCapture.name,
			"error", err.Error(), "detail", "the lost run's box could not be read before it was reclaimed")
		return
	}
	c.keepSession(ctx, run, box, runner, lossCapture)
}

// keptWhole reports whether run's row already holds a capture of its current
// job taken once the job had ended, which landed or is waiting on its record's
// answer: nothing since has changed the session it holds.
func keptWhole(run PendingRun) bool {
	last := run.Capture
	return last != nil && last.Launch == run.LaunchID && last.LaunchSeq == run.LaunchSeq && last.Final &&
		(last.State == CaptureLanded || last.State == CaptureUnresolved)
}

// nextCaptureSeq is the next capture's place among the current launch's, from
// what the row's newest capture says.
func nextCaptureSeq(run PendingRun) int64 {
	if last := run.Capture; last != nil && last.LaunchSeq == run.LaunchSeq {
		return last.CaptureSeq + 1
	}
	return 1
}

// refuseUnplaced stamps a capture refused before it held a place — the session
// could not be read, or the policy not known — so a later launch knows the
// launch's latest attempt failed rather than reading an older capture as its
// latest.
func (c *Coordinator) refuseUnplaced(ctx context.Context, run, current PendingRun,
	at capturePoint, why string) *SessionKept {

	stamp := SessionCapture{LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current),
		Launch: run.LaunchID, State: CaptureRefused, Reason: why, Final: at.final,
		BridgeSeq: current.LastBridgeSeq(), NoteCursor: current.Steer.Reads, At: c.now().UTC()}
	log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "detail", why)
	if _, err := c.pending.StampCapture(ctx, run.TurnID, stamp); err != nil {
		log.WarnContext(ctx, "executor_session_stamp_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	return stamp.kept()
}

// finishCapture stamps how a capture came out and reports it.
//
// UNDER A CONTEXT OF ITS OWN: the capture's budget may be what ended it, and
// the stamp is what tells a later launch so.
func (c *Coordinator) finishCapture(ctx context.Context, run PendingRun, stamp SessionCapture,
	state CaptureState, why string) *SessionKept {

	stamp.State, stamp.Reason = state, why
	stampCtx, cancel := detached(ctx)
	defer cancel()
	if _, err := c.pending.StampCapture(stampCtx, run.TurnID, stamp); err != nil {
		log.WarnContext(ctx, "executor_session_stamp_failed", "turn_id", run.TurnID,
			"capture_seq", stamp.CaptureSeq, "state", string(state), "error", err.Error())
	}
	switch state {
	case CaptureLanded:
		log.InfoContext(ctx, "executor_session_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "object", stamp.Object,
			"bytes", stamp.ObjectSize, "session", stamp.SessionID)
	default:
		log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"launch_seq", stamp.LaunchSeq, "capture_seq", stamp.CaptureSeq, "state", string(state),
			"detail", why)
	}
	return stamp.kept()
}

// uploadRefusal is why an upload of a session was refused, in the words its
// stamp keeps.
func uploadRefusal(err error) string {
	switch {
	case errors.Is(err, objstore.ErrFull):
		return "the object store's byte reservation is spent, so the session was not uploaded: " +
			"raise store.objects' reservation or move the objects to a bucket"
	case errors.Is(err, executordomain.ErrSessionTooLarge):
		return fmt.Sprintf("the session's archive is past the %d bytes a session is kept at",
			executordomain.SessionMaxBytes)
	}
	return fmt.Sprintf("the session could not be uploaded: %v", err)
}

// recordCapture records a stamped, uploaded capture on the executor log under
// the operation its place derives ([executordomain.CaptureOpID]), asked again
// under that same operation while no answer settles it, within
// [sessionRecordBudget]: landed, refused (and why), or unresolved.
func (c *Coordinator) recordCapture(ctx context.Context, handle, turn, conversationKey, workKey string,
	stamp SessionCapture) (CaptureState, string) {

	key, err := objstore.ParseKey(stamp.Object)
	if err != nil {
		return CaptureRefused, fmt.Sprintf("the capture names no object a record can: %v", err)
	}
	n := executordomain.NewCapture{
		Handle: handle, Turn: turn, ConversationKey: conversationKey, WorkKey: workKey,
		Harness: stamp.Harness, HarnessVersion: stamp.HarnessVersion, SessionID: stamp.SessionID,
		Object:     objstore.Object{Key: key, Hash: objstore.Hash(stamp.ObjectHash), Size: stamp.ObjectSize},
		ContentKey: stamp.ContentKey, ArchiveFormat: stamp.ArchiveFormat,
		LaunchSeq: stamp.LaunchSeq, CaptureSeq: stamp.CaptureSeq,
		Home: stamp.Home, Workspace: stamp.Workspace, SeatBudget: stamp.SeatBudget,
	}
	call := executordomain.Call{By: sessionAuthor,
		OpID: executordomain.CaptureOpID(stamp.Minted, handle, turn, stamp.Launch, stamp.CaptureSeq)}
	var outcome CaptureState
	var why string
	c.askRecord(ctx, "capture", turn, func(ctx context.Context) (bool, error) {
		res, err := c.sessions.Capture(ctx, call, n)
		switch {
		case err == nil && res.Outcome == statelog.OutcomeApplied && res.Recorded:
			outcome = CaptureLanded
			return true, nil
		case err == nil && res.Outcome == statelog.OutcomeApplied:
			outcome, why = CaptureRefused, "the log applied the capture and the turn's row names "+
				"another: an end, a newer capture, the seat's removal or the seat's budget got there first"
			return true, nil
		case refusedCapture(err):
			outcome, why = CaptureRefused, err.Error()
			return true, nil
		}
		outcome, why = CaptureUnresolved, fmt.Sprintf("outcome %s: %v", res.Outcome, err)
		return false, err
	})
	return outcome, why
}

// refusedCapture reports whether err is the log's definite refusal of a capture:
// the turn ended or removed, a newer capture recorded, an object too old to
// name, a seat budget the live turns fill, or a record it refuses outright.
// Anything else — a node that did not answer, none that serves, a conversation
// that kept changing — may land if asked again under the same operation.
func refusedCapture(err error) bool {
	for _, refusal := range []error{executordomain.ErrTurnEnded, executordomain.ErrStaleCapture,
		executordomain.ErrUploadStale, executordomain.ErrOverBudget, executordomain.ErrInvalid,
		executordomain.ErrSessionTooLarge, executordomain.ErrNotFound} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// askRecord asks one record of the log until it is settled — try answers true —
// or [sessionRecordBudget] runs out, spaced by a doubling wait.
func (c *Coordinator) askRecord(ctx context.Context, what, turn string,
	try func(context.Context) (bool, error)) {

	ctx, cancel := context.WithTimeout(ctx, c.recordBudget)
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
// still unresolved. One whose object is past objstore.RecordWithin is refused
// without asking: no record may name it any more, and the collector may already
// be reclaiming it.
func (c *Coordinator) settleCapture(ctx context.Context, run PendingRun, last SessionCapture) SessionCapture {
	key, err := objstore.ParseKey(last.Object)
	if err == nil && c.now().Sub(key.Minted()) > objstore.RecordWithin {
		last.State, last.Reason = CaptureRefused, fmt.Sprintf("its record was never answered within the "+
			"%s an object may be named in", objstore.RecordWithin)
	} else {
		last.State, last.Reason = c.recordCapture(ctx, run.AgentHandle, run.TurnID, run.ConversationKey,
			run.WorkKey, last)
	}
	stampCtx, cancel := detached(ctx)
	defer cancel()
	if _, err := c.pending.StampCapture(stampCtx, run.TurnID, last); err != nil {
		log.WarnContext(ctx, "executor_session_stamp_failed", "turn_id", run.TurnID,
			"capture_seq", last.CaptureSeq, "error", err.Error())
	}
	return last
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

	Ending executordomain.Ending
}

// EndTurn records on the executor log how a turn ended — the account the next
// turn on its conversation is told beside the session it continues — under the
// one operation every path that ends the turn derives
// ([executordomain.EndOpID]), asked again under it while no answer settles it.
//
// A CAPTURE STILL UNRESOLVED IS ASKED FIRST: an end tombstones its turn, and a
// capture whose record lands after it is refused, so a session uploaded and not
// yet named would be lost to the next turn.
//
// BEST EFFORT, AND SAID: the turn is over whatever the log answers, and a log
// that does not take the end inside its budget is an outage, which holds no
// seat. Its turn is ended `lost` by the retention duty a day after its last
// capture, and a turn that captured nothing leaves no row to end. Nil writes
// nothing — a coordinator with no executor log (a test's).
func (c *Coordinator) EndTurn(ctx context.Context, end TurnEnd) {
	if c.sessions == nil || c.keep == nil {
		return
	}
	// A CONTEXT OF ITS OWN, as a teardown's is: the frame ending the turn
	// may be one a drain is cancelling, and the end is still owed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTurnBudget)
	defer cancel()
	if run, found, err := c.pending.Get(ctx, end.TurnID); err == nil && found &&
		run.Capture != nil && run.Capture.State == CaptureUnresolved {
		c.settleCapture(ctx, run, *run.Capture)
	}
	minted, err := c.keep.MintedAt(ctx, end.TurnID, end.WorkKey, end.WorkSince)
	if err != nil {
		log.WarnContext(ctx, "executor_turn_end_left", "turn_id", end.TurnID, "agent", end.Handle,
			"error", err.Error(), "detail", "the turn's operations could not be minted, so its end "+
				"was not recorded; the retention duty ends a turn that captured a session as lost")
		return
	}
	call := executordomain.Call{By: sessionAuthor, OpID: executordomain.EndOpID(minted, end.Handle, end.TurnID)}
	n := executordomain.NewEnd{Handle: end.Handle, Turn: end.TurnID, ConversationKey: end.ConversationKey,
		WorkKey: end.WorkKey, Ask: end.Ask, Ending: end.Ending}
	ended := false
	c.askRecord(ctx, "end", end.TurnID, func(ctx context.Context) (bool, error) {
		res, err := c.sessions.End(ctx, call, n)
		switch {
		case err == nil && res.Outcome == statelog.OutcomeApplied:
			ended = true
			return true, nil
		case errors.Is(err, executordomain.ErrTurnEnded):
			// ANOTHER PATH ENDED IT FIRST — two frames that both end a
			// turn derive the same operation, and the second is refused.
			ended = true
			return true, nil
		case errors.Is(err, executordomain.ErrInvalid):
			log.ErrorContext(ctx, "executor_turn_end_refused", "turn_id", end.TurnID, "error", err.Error(),
				"detail", "the executor log refused the turn's end as malformed")
			return true, nil
		}
		return false, err
	})
	if !ended {
		log.WarnContext(ctx, "executor_turn_end_left", "turn_id", end.TurnID, "agent", end.Handle,
			"outcome", string(end.Ending.Outcome), "detail", "the executor log did not take the turn's "+
				"end within its budget; the retention duty ends a turn that captured a session as lost "+
				"a day after its last capture")
	}
}
