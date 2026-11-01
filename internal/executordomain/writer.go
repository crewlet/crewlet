package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// THE EXECUTOR WRITE PATH: one record per capture, end or delete, published
// to the subject of the conversation (or the seat) it contends for, decided
// inside one snapshot of this node's own rows.
//
// # Every write is the engine's
//
// Nobody asks for a session to be captured or a turn to be ended: the engine
// records what its runs did. So a [Call] names the engine's own actor, and the
// operation id it carries is DERIVED PER EXECUTION, never from the work key: a
// capture's from the turn, the launch and its ordinal ([CaptureOpID]), an end's
// from the turn ([EndOpID]), a retention delete's from the turn and its cutoff
// (the trash reaper's), a seat's removal from the seat and the instant its
// absence was first observed ([SeatDeleteOpID]). Derived from the work key, a
// re-run's capture would be answered from the ledger as the first run's and
// never applied.
//
// # What a write answers
//
// Every write answers its whole [statelog.Result] and never a bare position,
// because a position cannot tell `applied` and `pending` from `unknown`; and a
// capture the rows refused at the apply (an end, a newer capture or another
// conversation's capture that took the budget, between the decide and the
// apply) answers the refusal its own row says, read back.

// Call is one write's caller: who is writing and under which operation.
type Call struct {
	// By is the author as iam.ActorFor records a principal: the engine, as
	// `system`, for every record here.
	By iam.Actor

	// OpID is the operation the record is published under, held to the
	// state log's grammar (statelog.CheckCallerOpID).
	OpID string
}

// validate refuses a call the records could not name.
func (c Call) validate() error {
	switch {
	case strings.TrimSpace(c.By.Name) == "":
		return invalid("the call names no actor: every record names who wrote it")
	case !c.By.Kind.Valid():
		return invalid("actor kind %q is not one of %s", c.By.Kind, words.Or(iam.ActorKinds))
	}
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return fmt.Errorf("%w: the operation id %w", ErrInvalid, err)
	}
	return nil
}

// CaptureOpID is the operation a capture is published under: the seat, the
// turn, the launch it was taken from and its ordinal within that launch, at
// the instant the launch started (the unit of work a retry of the capture
// reproduces). One capture, one operation, however often it is retried; the
// next capture of the same launch, another.
func CaptureOpID(launchStarted time.Time, handle, turn, launch string, captureSeq int64) string {
	return statelog.DeriveOpID(launchStarted, "executor-capture", "crewlet.executor",
		handle, turn, launch, fmt.Sprint(captureSeq))
}

// EndOpID is the operation a turn's end is published under: the seat and the
// turn, at the instant the turn's derived operations are minted at (its work's
// start, which the engine rebases past statelog.MintHorizon). A turn ends
// once, however many paths try to say so.
func EndOpID(minted time.Time, handle, turn string) string {
	return statelog.DeriveOpID(minted, "executor-end", "crewlet.executor", handle, turn)
}

// SeatDeleteOpID is the operation a removed seat's rows are deleted under: the
// seat, at the instant its absence was first observed, so every retirement of
// one absence derives one operation and a seat removed again after it came
// back derives another.
func SeatDeleteOpID(absentSince time.Time, handle string) string {
	return statelog.DeriveOpID(absentSince, "executor-seat-delete", "crewlet.executor",
		handle, absentSince.UTC().Format(time.RFC3339Nano))
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Now is the clock a capture's object age is judged on and a record's
	// authored instant is read off; nil is the wall clock in UTC. No row
	// stores it.
	Now func() time.Time
}

// Writer is the executor write path.
type Writer struct {
	publisher *statelog.Publisher
	db        store.ReplicatedReader
	now       func() time.Time
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("executordomain: a write authority is required: every " +
			"write is a record on the executor log")
	case deps.DB.IsZero():
		return nil, errors.New("executordomain: the replicated estate is required: " +
			"a write decides from this node's own applied rows")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, now: deps.Now}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// decision encodes one record inside the snapshot's own transaction, stamped
// with the framework's writer and generation.
func decision(stamp statelog.Stamp, by iam.Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, at time.Time) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("executordomain: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: at,
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation: body, Actor: by.Name, ActorKind: by.Kind, OperatorID: by.OperatorID,
	})
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded}, nil
}

// publish appends one write, translating the framework's answer to exhausted
// rounds into this package's.
func publish(ctx context.Context, publisher *statelog.Publisher,
	req statelog.Request) (statelog.Result, error) {

	res, err := publisher.Publish(ctx, req)
	if errors.Is(err, statelog.ErrConflict) {
		return res, fmt.Errorf("%w: %s: %w", ErrConflict, req.Subject, err)
	}
	return res, err
}

// request is one write's framework request on subject, its scope resolved.
func request(subject Subject, scope ScopeSet, opID string,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	return statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: statelog.PatternArbitrated, Decide: decide,
	}
}

// ---- capture ------------------------------------------------------------ //

// NewCapture is one capture of a live turn's session: its object already
// uploaded and sealed ([SessionStore.Put]), and everything the next launch
// needs to restore it.
type NewCapture struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	CLI        string
	CLIVersion string
	SessionID  string

	// Object is the sealed object, ContentKey the keyring key that sealed
	// it, and ArchiveFormat the layout of the archive inside.
	Object        objstore.Object
	ContentKey    string
	ArchiveFormat int

	LaunchSeq  int64
	CaptureSeq int64

	Home      string
	Workspace string

	// ReadThrough is the position the turn's first launch read the
	// conversation's head at, zero where it read none.
	ReadThrough statelog.Position

	// SeatBudget is the company's `max_bytes_per_seat` as the fleet's
	// revision states it.
	SeatBudget int64
}

// validate refuses a capture the record could not carry, naming the field.
func (n NewCapture) validate() error {
	if err := checkTurn(n.Handle, n.Turn, n.ConversationKey, n.WorkKey); err != nil {
		return err
	}
	for _, f := range []struct {
		field, value string
		max          int
	}{
		{"the CLI", n.CLI, CLIMaxBytes}, {"the CLI's version", n.CLIVersion, CLIVersionMaxBytes},
		{"the session id", n.SessionID, SessionIDMaxBytes},
		{"the content key", n.ContentKey, maxContentKeyBytes},
		{"the home path", n.Home, PathMaxBytes}, {"the workspace path", n.Workspace, PathMaxBytes},
	} {
		if err := checkText(f.field, f.value, f.max); err != nil {
			return err
		}
	}
	switch {
	case strings.TrimSpace(n.CLI) == "":
		return invalid("the capture names no CLI")
	case strings.TrimSpace(n.SessionID) == "":
		return invalid("the capture names no session id: a session nobody can name " +
			"is one no launch can resume")
	case n.ContentKey == "":
		return invalid("the capture names no content key: a rotation could not count it")
	case n.ArchiveFormat <= 0:
		return invalid("the capture names no archive format: a restorer checks it " +
			"before it unpacks anything")
	case n.LaunchSeq <= 0 || n.CaptureSeq <= 0:
		return invalid("the capture's ordinals are (%d, %d) and both start at 1",
			n.LaunchSeq, n.CaptureSeq)
	case n.SeatBudget <= 0:
		return invalid("the capture carries no seat budget")
	case n.ReadThrough.Seq > 0 && n.ReadThrough.Stream != Domain{}.Stream().Name:
		return invalid("the head was read on %q, which is not this log", n.ReadThrough.Stream)
	}
	if err := n.Object.Validate(); err != nil {
		return fmt.Errorf("%w: the capture's object: %w", ErrInvalid, err)
	}
	if n.Object.Size > SealedMaxBytes {
		return invalid("the capture's object is %d bytes and a sealed session is at "+
			"most %d", n.Object.Size, SealedMaxBytes)
	}
	return nil
}

// maxContentKeyBytes is the longest content key a capture names: a keyring
// key id a sealed stream's header can carry.
const maxContentKeyBytes = 255

// checkTurn refuses the identity every record about a turn carries.
func checkTurn(handle, turn, conversationKey, workKey string) error {
	switch {
	case !iam.ValidSeatHandle(handle):
		return invalid("%q is not a seat's handle", handle)
	case len(turn) > TurnIDMaxBytes:
		return invalid("the turn id is %d bytes and the maximum is %d", len(turn), TurnIDMaxBytes)
	}
	if _, err := uuid.Parse(turn); err != nil {
		return invalid("the turn id %q is not a run id: %v", turn, err)
	}
	if err := checkText("the conversation key", conversationKey, ConversationKeyMaxBytes); err != nil {
		return err
	}
	return checkText("the work key", workKey, WorkKeyMaxBytes)
}

// CaptureResult is what a capture answers: the publish's result, and whether
// the turn's row names this capture's object now.
type CaptureResult struct {
	statelog.Result

	// Recorded is true when the row was read back naming this capture's
	// object. False on an applied result means the apply refused it (an
	// end, a newer capture or the seat's budget got there first), and on a
	// pending or unknown one that this node has not seen it apply yet.
	Recorded bool
}

// Capture records a new object for a live turn's session.
//
// Refused at the decide: an object minted longer than objstore.RecordWithin
// ago by this node's clock ([ErrUploadStale]), a turn that has ended or been
// removed ([ErrTurnEnded]), ordinals not newer than the row's
// ([ErrStaleCapture]), and a capture the seat's budget cannot hold with every
// ended session released ([ErrOverBudget]). The apply repeats every guard but
// the age, and an applied capture is read back ([CaptureResult.Recorded]).
func (w *Writer) Capture(ctx context.Context, c Call, n NewCapture) (CaptureResult, error) {
	if err := c.validate(); err != nil {
		return CaptureResult{}, err
	}
	if err := n.validate(); err != nil {
		return CaptureResult{}, err
	}
	p := Capture{
		V: baseRecordVersion, Handle: n.Handle, Turn: n.Turn,
		ConversationKey: n.ConversationKey, WorkKey: n.WorkKey,
		CLI: n.CLI, CLIVersion: n.CLIVersion, SessionID: n.SessionID,
		Object: n.Object.Key.String(), ObjectHash: string(n.Object.Hash),
		ObjectSize: n.Object.Size, ContentKey: n.ContentKey, ArchiveFormat: n.ArchiveFormat,
		LaunchSeq: n.LaunchSeq, CaptureSeq: n.CaptureSeq,
		Home: n.Home, Workspace: n.Workspace, SeatBudget: n.SeatBudget,
	}
	if n.ReadThrough.Seq > 0 {
		p.ReadThrough = n.ReadThrough.Packed()
	}
	subject := ConversationSubject(n.Handle, n.ConversationKey, n.Turn)
	// THE SEAT, NOT THE CONVERSATION: a capture past the budget releases the
	// seat's oldest ended sessions, which may be any conversation's.
	scope := seatScope(n.Handle)
	res, err := publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE DECIDE'S OWN CLOCK, read here rather than before the
			// publish: a capture retried after its object aged past the
			// bound is judged at the instant it is decided again.
			at := w.now()
			if err := decideCapture(ctx, tx, n, at); err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, OpCapture, scope, c.OpID, p, at)
		}))
	if err != nil {
		return CaptureResult{Result: res}, err
	}
	out := CaptureResult{Result: res}
	if res.Outcome != statelog.OutcomeApplied {
		return out, nil
	}
	// READ BACK: the apply decides the guards and the budget again in log
	// order, and only the row says whether this capture is the one it names.
	err = w.db.Read(ctx, func(tx *sql.Tx) error {
		var object sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT object FROM executor_sessions WHERE agent_handle = ? AND turn_id = ?`,
			n.Handle, n.Turn).Scan(&object)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		out.Recorded = err == nil && object.Valid && object.String == p.Object
		return err
	})
	return out, err
}

// decideCapture is the capture's decide over one snapshot of the turn and its
// seat: every refusal [Writer.Capture] names.
//
// NOTHING IS REFUSED FOR BEING MINTED IN THE FUTURE. A key that a fast clock
// minted ahead of this one protects itself: the collector deletes nothing
// whose minting instant is inside its grace.
func decideCapture(ctx context.Context, tx *sql.Tx, n NewCapture, at time.Time) error {
	if age := at.Sub(n.Object.Key.Minted()); age > objstore.RecordWithin {
		return fmt.Errorf("%w: object %s was minted %s ago and a capture names an "+
			"upload at most %s after it", ErrUploadStale, n.Object.Key,
			age.Truncate(time.Second), objstore.RecordWithin)
	}
	if _, tombed, err := readTomb(ctx, tx, n.Handle, n.Turn); err != nil {
		return err
	} else if tombed {
		return fmt.Errorf("%w: %s's turn %s", ErrTurnEnded, n.Handle, n.Turn)
	}
	row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
	if err != nil {
		return err
	}
	if found {
		switch {
		case row.ended:
			return fmt.Errorf("%w: %s's turn %s", ErrTurnEnded, n.Handle, n.Turn)
		case row.conversationKey != n.ConversationKey:
			return invalid("turn %s belongs to another conversation than the capture names",
				n.Turn)
		case !newer(n.LaunchSeq, n.CaptureSeq, row.launchSeq, row.captureSeq):
			return fmt.Errorf("%w: turn %s holds (%d, %d) and this capture is (%d, %d)",
				ErrStaleCapture, n.Turn, row.launchSeq, row.captureSeq, n.LaunchSeq, n.CaptureSeq)
		}
	}
	if _, fits, err := seatPlan(ctx, tx, n.Handle, n.Turn, n.Object.Size, n.SeatBudget); err != nil {
		return err
	} else if !fits {
		return fmt.Errorf("%w: %s's live turns hold more than %d bytes beside this %d-byte "+
			"capture", ErrOverBudget, n.Handle, n.SeatBudget, n.Object.Size)
	}
	return nil
}

// ---- end ---------------------------------------------------------------- //

// NewEnd is how one turn ended.
type NewEnd struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// Ask is what the turn was asked; the record keeps its first
	// [AskExcerptMaxBytes], cut on a rune boundary.
	Ask string

	Ending Ending
}

// End records how a turn ended, and tombstones it: no capture lands for it
// after this, and a second end is [ErrTurnEnded]. With no row it writes an
// ended row naming no object; with one it absorbs and dedupes the
// conversation's ended rows (see the package doc).
func (w *Writer) End(ctx context.Context, c Call, n NewEnd) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := checkTurn(n.Handle, n.Turn, n.ConversationKey, n.WorkKey); err != nil {
		return statelog.Result{}, err
	}
	if err := n.Ending.validate(); err != nil {
		return statelog.Result{}, err
	}
	if !utf8.ValidString(n.Ask) {
		return statelog.Result{}, invalid("the ask is not valid UTF-8")
	}
	p := End{
		V: baseRecordVersion, Handle: n.Handle, Turn: n.Turn,
		ConversationKey: n.ConversationKey, WorkKey: n.WorkKey,
		AskExcerpt: textcut.Within(n.Ask, AskExcerptMaxBytes), Ending: n.Ending.fit(),
	}
	subject := ConversationSubject(n.Handle, n.ConversationKey, n.Turn)
	scope := ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, tombed, err := readTomb(ctx, tx, n.Handle, n.Turn); err != nil {
				return statelog.Decision{}, err
			} else if tombed {
				return statelog.Decision{}, fmt.Errorf("%w: %s's turn %s", ErrTurnEnded,
					n.Handle, n.Turn)
			}
			row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
			if err != nil {
				return statelog.Decision{}, err
			}
			if found && row.conversationKey != n.ConversationKey {
				return statelog.Decision{}, invalid("turn %s belongs to another "+
					"conversation than the end names", n.Turn)
			}
			return decision(stamp, c.By, subject, OpEnd, scope, c.OpID, p, w.now())
		}))
}

// ---- delete ------------------------------------------------------------- //

// DeleteExpired removes one turn's row and tombstone whose last instant is at
// or before cutoff, as the retention duty, under the operation the duty
// derived. It is decided again in the conversation's snapshot and once more at
// the apply, so a turn captured since it was listed is refused [ErrNotDue] or
// left in place.
func (w *Writer) DeleteExpired(ctx context.Context, c Call, handle, conversationKey,
	turn string, cutoff time.Time) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := checkTurn(handle, turn, conversationKey, ""); err != nil {
		return statelog.Result{}, err
	}
	p := Delete{V: baseRecordVersion, Handle: handle, Turn: turn,
		ConversationKey: conversationKey, Cutoff: cutoff.UTC()}
	subject := ConversationSubject(handle, conversationKey, turn)
	scope := ScopeSet{Subject: true}
	res, err := publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			due, err := expiredIn(ctx, tx, handle, conversationKey, turn, cutoff)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !due {
				return statelog.Decision{}, fmt.Errorf("%w: %s's turn %s", ErrNotDue, handle, turn)
			}
			return decision(stamp, c.By, subject, OpDelete, scope, c.OpID, p, w.now())
		}))
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		return res, err
	}
	// THE APPLY MAY HAVE REFUSED IT: a capture landed between the decide and
	// the apply keeps the turn, and only the rows say the delete landed.
	var left bool
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		_, found, err := readSession(ctx, tx, handle, turn)
		if err != nil || found {
			left = found
			return err
		}
		_, left, err = readTomb(ctx, tx, handle, turn)
		return err
	}); err != nil {
		return res, err
	}
	if left {
		return res, fmt.Errorf("%w: %s's turn %s was captured after it was listed",
			ErrNotDue, handle, turn)
	}
	return res, nil
}

// expiredIn reports whether a turn has a row or a tombstone in conversation
// whose last instant is at or before cutoff: what a retention delete removes.
func expiredIn(ctx context.Context, tx *sql.Tx, handle, conversationKey, turn string,
	cutoff time.Time) (bool, error) {

	limit := store.EncodeTime(cutoff)
	row, found, err := readSession(ctx, tx, handle, turn)
	if err != nil {
		return false, err
	}
	if found {
		return row.conversationKey == conversationKey && row.lastAt <= limit, nil
	}
	marker, tombed, err := readTomb(ctx, tx, handle, turn)
	if err != nil || !tombed {
		return false, err
	}
	return marker.conversationKey == conversationKey && marker.at <= limit, nil
}

// DeleteSeat removes every row a seat the company removed holds here, as the
// mailbox retirement, under [SeatDeleteOpID].
//
// IDEMPOTENT: a seat with nothing here answers with nothing published.
func (w *Writer) DeleteSeat(ctx context.Context, c Call, handle string) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if !iam.ValidSeatHandle(handle) {
		return statelog.Result{}, invalid("%q is not a seat's handle", handle)
	}
	subject := SeatSubject(handle)
	scope := ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			var held int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM executor_sessions WHERE agent_handle = ?`,
				handle).Scan(&held); err != nil {
				return statelog.Decision{}, fmt.Errorf("executordomain: read seat %s's "+
					"sessions: %w", handle, err)
			}
			if held == 0 {
				return statelog.Decision{}, nil
			}
			return decision(stamp, c.By, subject, OpSeatDelete, scope, c.OpID,
				SeatDelete{V: baseRecordVersion, Handle: handle}, w.now())
		}))
}
