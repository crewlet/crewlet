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
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
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
//     (one its turn's cap ended while it still ran, one that failed before it
//     packed, one lost with its engine whose box is still there), one packed
//     now;
//   - a stop's read of what the run had spent, packed now;
//   - the ending of a run LOST with its box still there (a launch whose engine
//     restarted before its turn could park), packed now, before the ending
//     reclaims the box — on this node's host, or asked of the host the box is
//     on ([BoxReadLoss]).
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
// ([PendingStore.StampCapture]), with the bridged call the job had answered
// through and the note it had read through when it was packed: what a launch
// that continues the session would need to know of what came after it. How it
// came out is stamped beside: LANDED, REFUSED with why (no box to read it out
// of, a pack that failed, a session past its bounds, the seat's budget, an
// upload the store had no room for, a record the log will never take), or
// UNRESOLVED — uploaded, and whether its record landed unknown — which the
// turn's end asks again under the same operation before it ends the turn.
//
// # And every turn's end, owed on the row until the log takes it
//
// How the turn ended — completed or failed, the reviewer's decision, whether
// its reply reached the party waiting, who stopped it and why, what lost it —
// is the account a transcript cannot hold ([executordomain.Ending]), recorded
// on the turn's row of the log beside the session it kept. Whatever ends the
// turn writes it on the run's row ([PendingStore.OweEnd]): the engine's resumed
// turn, and this package's endings that no frame of the turn reaches — a loss,
// a stop ([CoordinatorOptions.TurnLost], [CoordinatorOptions.CloseStopped]).
// The run's ending records it on the executor log ([Coordinator.publishEnd])
// before it deletes the row, which the delete refuses until then
// ([ErrEndOwed]): after every capture of the turn has settled, so the end's
// tombstone never refuses the turn's own last session, and with the turn's
// NEWEST capture where it was not kept ([executordomain.Unkept]), so the log's
// row never goes on naming an older session as the turn's last. The seat's mail
// waits on it as on every ending kept for its retry. A turn with no capture
// still ends: its row names no object, and holds the account.
//
// # The switch
//
// `turn_engine.conversation_session.enabled: false` keeps nothing — no capture
// is taken or stamped ([SessionPolicy.Keep], the fleet's word) — and prepares
// nothing for one: no launch seals what it handed its box, and no loss asks
// another node's host to read a box, holding the seat's mail for a take that
// would keep nothing ([Coordinator.capturing], this node's word). It still
// records every end, which is the account and not the session.

// SessionWrites is the executor log's writes a coordinator records through: the
// estate router's (internal/estate's ExecutorWrites), so a node without `data`
// records through a data node, and a node with it through its own copy — never
// a data node's local writer, which a seats-only node does not have.
type SessionWrites interface {
	Capture(ctx context.Context, c executordomain.Call, n executordomain.NewCapture) (executordomain.CaptureResult, error)
	End(ctx context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error)
}

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
	// ([PendingRun.Handed]); OpenHanded opens what it sealed, or says why it
	// cannot.
	SealHanded(turnID string, handed []byte) (string, error)
	OpenHanded(turnID, sealed string) ([]byte, error)

	// MintedAt is the instant a turn's derived operations are minted at: its
	// work's start, or the instant the turn was rebased onto where that start
	// is past the operation ledger's horizon (statelog.MintAt), as every
	// operation id a turn derives is.
	MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error)

	// Seal streams archive sealed under the fleet keyring into dst, under an
	// object key minted now (executordomain.SessionStore.Seal), and Upload
	// puts what dst held into that object (executordomain.SessionStore.Upload).
	Seal(handle, turn string, archive io.Reader, dst io.Writer) (executordomain.SealedSpool, error)
	Upload(ctx context.Context, spool executordomain.SealedSpool, sealed io.Reader) (executordomain.SealedObject, error)
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
	// or died with its host — so nothing after it changes the session in
	// its box, and a later read of the same job takes nothing new. A job
	// still working when it was taken (a stop's, one its turn's cap ended)
	// is not final.
	Final bool `json:"final,omitempty"`

	// BridgeSeq is the last bridged call ([BridgeCall.Seq]) the row held,
	// and NoteCursor the last note read ([RunSteer.Reads]), when the archive
	// was packed: what the session it holds had been answered and told, and
	// so where what came after it begins for a launch that continues it.
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
// executordomain.SealedMaxBytes: thirty-three mebibytes begun, at thirty
// seconds each) and its record ([sessionRecordBudget]).
var SessionKeepBudget = objstore.PaceFor(executordomain.SealedMaxBytes) + sessionRecordBudget

// SessionCaptureBudget is the most a capture takes from its take to its record:
// a capture stamped begun longer ago than this is one whose node stopped, or
// whose budget ran out, before it could stamp what came of it.
var SessionCaptureBudget = SessionTakeBudget + SessionKeepBudget

// sessionKeepDrain is how long a stopping coordinator lets keeps already under
// way finish before it cancels them ([Coordinator.Stop]): the teardown grace
// every box call is given ([discardGrace]). A keep cut there is stamped refused
// saying so, and the turn's next launch starts fresh.
const sessionKeepDrain = discardGrace

// captureWaitTick spaces the reads of a run's row an ending makes while a
// capture of its turn is still being kept ([Coordinator.awaitCapture]).
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
}

// readCapture is the capture a box read takes: the archive the run's script
// packed where its job ended and packed one, and one packed now otherwise — a
// job still working (one its turn's cap ended, one a person stopped), which is
// not final, or one that ended without packing (a crash, a loss with its box
// left), which is.
func readCapture(read BoxRead, result Result, running bool) capturePoint {
	return capturePoint{name: string(read), pack: running || result.Captured == nil, final: !running}
}

// lossCapture is a lost run's box, read before its ending reclaims it: its job
// went with its host, so what its box holds is final.
var lossCapture = capturePoint{name: "loss", pack: true, final: true}

// spooledCapture is a capture taken out of its box and spooled, sealed, on this
// node's disk: its upload and its record are what is left ([Coordinator.keepCapture]).
type spooledCapture struct {
	run   PendingRun
	stamp SessionCapture
	spool executordomain.SealedSpool
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
// logged and the read goes on.
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
		return nil, c.refuseUnplaced(ctx, by, run, current, at, fmt.Sprintf("the company's session "+
			"policy could not be read off the revision the fleet runs: %v", err))
	}
	if !policy.Keep {
		return nil, nil
	}
	scrub, err := c.scrubValues(ctx, current, box)
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, err.Error())
	}
	taken, err := taker.TakeSession(ctx, box, SessionTake{Pack: at.pack, Scrub: scrub,
		MaxBytes: executordomain.SessionMaxBytes})
	if err != nil {
		return nil, c.refuseUnplaced(ctx, by, run, current, at, takeRefusal(ctx, err))
	}
	if taken.Body == nil {
		return nil, nil
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
	stampCtx, stampCancel := stampContext(ctx, by)
	took, stampErr := c.pending.StampCapture(stampCtx, run.TurnID, stamp)
	stampCancel()
	if stampErr != nil || !took {
		// ANOTHER READ OF THE SAME RUN TOOK THIS PLACE FIRST — a stop's and a
		// collection's, at once — and keeps the session itself; or the row
		// could not be written, which leaves nowhere to say what came of it.
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
			"capture_seq", stamp.CaptureSeq, "error", fmt.Sprint(stampErr),
			"detail", "the capture's place could not be reserved on the run's row")
		return nil, nil
	}
	spooled, err := c.spool(ctx, run, stamp, taken)
	if err != nil {
		return nil, c.finishCapture(ctx, by, run, stamp, CaptureRefused, takeRefusal(ctx, err))
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
// room — the stamp of what came of it, the digest it leaves in the box
// ([stampContext]) — and after them its pause and its answer theirs
// ([discardGrace] each). So nothing a take does, cut short or not, costs the
// box its pause, which is what stops a job its turn's cap ended. by is zero
// where the read has no deadline.
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

// stampContext is the context one of a take's own records is made under —
// a stamp of the capture, the digest of its archive left in the box: one of
// its own ([detached]), since the take's budget may be what ended the take and
// the stamp is what says so, and never past by, where the read the take is
// part of needs the rest of its time for the box's pause
// ([Coordinator.takeContext]). by zero bounds it by its own grace alone.
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
	case errors.Is(err, executordomain.ErrSessionTooLarge):
		return fmt.Sprintf("the session is past the %d bytes a session is kept at, sealed",
			executordomain.SessionMaxBytes)
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
	outcome, why := c.recordCapture(ctx, run.AgentHandle, run.TurnID, run.ConversationKey, run.WorkKey, stamp)
	return c.finishCapture(ctx, time.Time{}, run, stamp, outcome, why)
}

// overBudget is why a capture of size bytes cannot fit the seat's budget beside
// the sessions its other LIVE turns hold, or "" where it may: asked before the
// upload, so a seat whose live turns fill its budget does not upload a session
// on every launch that the log then refuses, holding the store's room for a day
// each time. Read off the seat's runs in the coordination store, whose kept
// captures are what its live turns hold; the log decides again at the record
// ([executordomain.ErrOverBudget]), which also counts a turn whose end was lost.
// A listing that cannot be read decides nothing here.
func (c *Coordinator) overBudget(ctx context.Context, run PendingRun, size, budget int64) string {
	runs, err := c.pending.ListActiveForSeat(ctx, run.AgentHandle)
	if err != nil {
		return ""
	}
	var held int64
	for _, other := range runs {
		if other.TurnID != run.TurnID && other.Kept != nil {
			held += other.Kept.ObjectSize
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
// the last read of it anything makes. On this node's host it is attached and
// taken here; on another node's it is asked of that host ([BoxReadLoss]), which
// takes it there. A box that is gone, and a job of this launch already taken
// whole, are passed over.
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
			log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", lossCapture.name,
				"node", run.LaunchNode, "error", err.Error(),
				"detail", "the lost run's box is on another node's host, which did not read it before "+
					"its ending reclaims it")
			return
		}
		if kept := result.Kept; kept != nil && kept.NotKept != "" {
			log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", lossCapture.name,
				"node", run.LaunchNode, "detail", kept.NotKept)
		}
		return
	}
	spooled, _ := c.readLost(ctx, run)
	c.keepLater(spooled)
}

// readLost is the take of a lost run's box on this node's host — for this node's
// own ending, or asked by the node ending it ([BoxReadLoss]).
func (c *Coordinator) readLost(ctx context.Context, run PendingRun) (*spooledCapture, *SessionKept) {
	manager := c.mgr()
	if manager == nil {
		return nil, nil
	}
	box, runner, err := manager.Attach(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		why := fmt.Sprintf("the lost run's box could not be read before it was reclaimed: %v", err)
		log.InfoContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", lossCapture.name,
			"detail", why)
		return nil, &SessionKept{NotKept: why}
	}
	return c.takeCapture(ctx, run, box, runner, lossCapture)
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

// capturesStop reports whether a stopped run's read takes its session: a
// company that keeps sessions ([Coordinator.capturing]), and a job that started
// in a box.
func (c *Coordinator) capturesStop(run PendingRun) bool {
	return c.capturing() && run.SandboxID != "" && run.CommandID != ""
}

// keepsSessions reports whether this coordinator records on an executor log: an
// engine's, never a test's built without one. It is the WIRING, which every
// turn's end needs whatever the company says; whether a session is worth
// preparing to keep is [Coordinator.capturing].
func (c *Coordinator) keepsSessions() bool { return c.sessions != nil && c.keep != nil }

// capturing reports whether a capture is worth PREPARING: a coordinator that
// records on an executor log, of a company that keeps sessions as this node's
// revision says ([SessionKeep.Keeps]). With the switch off, a launch seals
// nothing for a scrub no capture would make, a loss asks no other node's host
// to read a box for a take that would keep nothing — holding the seat's mail
// for the minutes a silent host costs — and a stop reads its run's spend on
// the frame that decided it, as it did before sessions were kept. The take
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
// its launch handed the box ([PendingRun.Handed]), and its seeded logins as
// they are NOW, in the box and on the host, since a refresh mints values the
// launch never saw. [ErrHandedUnknown] where they cannot be known — see
// handed.go — which refuses the capture rather than scrub less; and refused
// too past its bounds — a login read again larger than a whole launch may hand
// ([MaxHandedBytes]), or values past [maxScrubBytes] in all — since the box's
// code can write its login and the scrub holds every value whole.
func (c *Coordinator) scrubValues(ctx context.Context, run PendingRun, box Sandbox) ([]string, error) {
	if run.Handed == "" {
		return nil, fmt.Errorf("%w: the run's record keeps none for its job, so its session cannot be "+
			"scrubbed of them", ErrHandedUnknown)
	}
	raw, err := c.keep.OpenHanded(run.TurnID, run.Handed)
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
// UNDER A CONTEXT OF ITS OWN, by by at the latest ([stampContext]): the take's
// budget may be what ended it, and the stamp is what says so.
func (c *Coordinator) refuseUnplaced(ctx context.Context, by time.Time, run, current PendingRun,
	at capturePoint, why string) *SessionKept {

	stamp := SessionCapture{LaunchSeq: current.LaunchSeq, CaptureSeq: nextCaptureSeq(current),
		Launch: run.LaunchID, State: CaptureRefused, Reason: redact.Secrets(why), Final: at.final,
		BridgeSeq: current.LastBridgeSeq(), NoteCursor: current.Steer.Reads, At: c.now().UTC()}
	log.WarnContext(ctx, "executor_session_not_kept", "turn_id", run.TurnID, "point", at.name,
		"capture_seq", stamp.CaptureSeq, "detail", stamp.Reason)
	stampCtx, cancel := stampContext(ctx, by)
	defer cancel()
	if _, err := c.pending.StampCapture(stampCtx, run.TurnID, stamp); err != nil {
		log.WarnContext(ctx, "executor_session_stamp_failed", "turn_id", run.TurnID, "error", err.Error())
	}
	return stamp.kept()
}

// finishCapture stamps how a capture came out and reports it.
//
// UNDER A CONTEXT OF ITS OWN, by by at the latest where a take is still
// stamping it ([stampContext]; zero off the read): the capture's budget may be
// what ended it, and the stamp is what says so.
func (c *Coordinator) finishCapture(ctx context.Context, by time.Time, run PendingRun, stamp SessionCapture,
	state CaptureState, why string) *SessionKept {

	stamp.State, stamp.Reason = state, redact.Secrets(why)
	stampCtx, cancel := stampContext(ctx, by)
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
			"detail", stamp.Reason)
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

// captureInFlight reports whether run's turn has a capture still being taken or
// kept, as of now: stamped begun, and younger than a capture can take
// ([SessionCaptureBudget]). An older one's node stopped, or its budget ran out,
// before it could say what came of it.
func captureInFlight(run PendingRun, now time.Time) bool {
	c := run.Capture
	return c != nil && c.State == CaptureBegun && now.Sub(c.At) < SessionCaptureBudget
}

// awaitCapture is run's row once no capture of its turn is in flight
// ([captureInFlight]) — read again every [captureWaitTick], for as long as the
// capture's budget allows — or as it stands when ctx ends first.
//
// WHY AN END WAITS: the end tombstones its turn, and a capture whose record
// lands after it is refused, so ended first, the turn's last session would be
// lost to the next turn. A keep is made off the read that took it, on the node
// whose host the box is on, which may be another node; the row is what both
// read.
func (c *Coordinator) awaitCapture(ctx context.Context, run PendingRun) PendingRun {
	// NEVER LONGER THAN A CAPTURE CAN TAKE, whatever the stamp's clock says: a
	// stamp whose node's clock runs ahead is waited out on this one's.
	ctx, cancel := context.WithTimeout(ctx, SessionCaptureBudget)
	defer cancel()
	for captureInFlight(run, c.now()) {
		wait := time.NewTimer(c.captureTick)
		select {
		case <-ctx.Done():
			wait.Stop()
			return run
		case <-wait.C:
		}
		current, found, err := c.pending.Get(ctx, run.TurnID)
		if err != nil || !found {
			return run
		}
		run = current
	}
	return run
}

// unkeptOf is the turn's newest capture where it is not one the turn's row on
// the executor log names ([executordomain.Unkept]): refused, never answered,
// or begun and never finished — and newer than the one that landed, if any.
// Nil where the newest landed, or the turn took none.
func unkeptOf(run PendingRun) *executordomain.Unkept {
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
	return &executordomain.Unkept{LaunchSeq: newest.LaunchSeq, CaptureSeq: newest.CaptureSeq,
		Reason: redact.Secrets(why)}
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

// turnEndOf is the end of the turn run's row belongs to, ending as ending.
func turnEndOf(run PendingRun, ending executordomain.Ending) TurnEnd {
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
func (c *Coordinator) OweEnd(ctx context.Context, turnID string, ending executordomain.Ending) (bool, error) {
	raw, err := json.Marshal(ending)
	if err != nil {
		return false, fmt.Errorf("sandbox: encoding the end of turn %s: %w", turnID, err)
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	owing, _, err := c.pending.OweEnd(ctx, turnID, raw)
	if err != nil {
		return false, fmt.Errorf("sandbox: recording the end turn %s owes: %w", turnID, err)
	}
	return len(owing.OwedEnd) > 0 || owing.EndRecorded, nil
}

// oweEnd is [Coordinator.OweEnd] for an ending's own step: the row as it stands
// after the write, owing the end it decided or one decided before it, or
// having recorded one already.
func (c *Coordinator) oweEnd(ctx context.Context, run PendingRun, ending executordomain.Ending) (PendingRun, error) {
	raw, err := json.Marshal(ending)
	if err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: encoding the end of turn %s: %w", run.TurnID, err)
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

// endKeys are the keys an [executordomain.Ending] this build writes may hold.
var endKeys = wireKeys(reflect.TypeFor[executordomain.Ending]())

// publishEnd records the end run's row owes on the executor log
// ([Coordinator.recordEnd]) — after asking again a capture of the turn still
// unresolved, and with the turn's newest capture where it was not kept
// ([unkeptOf]) — and clears it from the row, marking it recorded. Returns the
// row as it stands after, or why the end is still owed.
//
// # An end a newer build owed is left for a node that reads it
//
// A rolling upgrade mixes builds, and the seat can move from the node that
// owed the end to one running this build. An end holding a key this build
// does not know, or an outcome it does not know, is DEFERRED rather than
// published: decoded into this build's [executordomain.Ending] and encoded
// again, the keys it could not read were dropped from the turn's account; and
// an outcome it does not know, refused by this build's log as malformed, was
// taken for an end the log will never take and cleared — the turn's end never
// recorded. Left owed, the ending is kept, the seat's mail waits on it as on any
// owed end, and a node of the build that wrote it records it, whichever node
// holds the seat once the upgrade reaches it. Never re-encoded with less, which
// is the rule every record of the fleet's builds keeps: what a build cannot read
// is deferred, never dropped.
func (c *Coordinator) publishEnd(ctx context.Context, run PendingRun) (PendingRun, error) {
	var ending executordomain.Ending
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
				strings.Join(slices.Sorted(maps.Keys(unknown)), ", "))
		}
		log.WarnContext(ctx, "executor_turn_end_deferred", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"detail", "the end the run's record owes was written by a newer build — "+why+" — so it is "+
				"left owed for a node of that build to record, and the run's ending is kept until one does")
		return run, fmt.Errorf("sandbox: the end turn %s owes was written by a newer build: %s", run.TurnID, why)
	default:
		if newest := run.Capture; newest != nil && newest.State == CaptureUnresolved {
			c.settleCapture(ctx, run, *newest)
			if current, found, getErr := c.pending.Get(ctx, run.TurnID); getErr == nil && found {
				run = current
			}
		}
		ending.Unkept = unkeptOf(run)
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
// operation every path that ends the turn derives ([executordomain.EndOpID]),
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
	call := executordomain.Call{By: sessionAuthor, OpID: executordomain.EndOpID(minted, end.Handle, end.TurnID)}
	n := executordomain.NewEnd{Handle: end.Handle, Turn: end.TurnID, ConversationKey: end.ConversationKey,
		WorkKey: end.WorkKey, Ask: end.Ask, Ending: end.Ending}
	var lastErr error
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
				"detail", "the executor log refused the turn's end as malformed, which no retry changes")
			ended = true
			return true, nil
		}
		lastErr = fmt.Errorf("outcome %s: %w", res.Outcome, err)
		return false, err
	})
	if !ended {
		return fmt.Errorf("sandbox: the executor log did not take the end of turn %s within %s: %w",
			end.TurnID, c.recordBudget, lastErr)
	}
	return nil
}
