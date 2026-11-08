package executordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE EXECUTOR WRITE PATH: one record per begin, capture, end or delete,
// published to the subject of the conversation (or the seat) it contends for,
// decided inside one snapshot of this node's own rows.
//
// # Every write is the engine's
//
// Nobody asks for a session to be captured or a turn to be ended: the engine
// records what its runs did. So a [Call] names the engine's own actor, and the
// operation id it carries is DERIVED PER EXECUTION, never from the work key: a
// begin's from the turn ([BeginOpID]), a capture's from the turn, the launch
// and its ordinal ([CaptureOpID]), an end's from the turn ([EndOpID]), a
// retention delete's from the turn and the
// reaper's slot (the trash reaper's), a seat's removal from the seat and the
// instant the retirement removing it was marked ([SeatDeleteOpID]). Derived
// from the work key, a re-run's capture would be answered from the ledger as
// the first run's and never applied.
//
// EVERY INSTANT AN ID IS MINTED AT IS ONE THE LEDGER CAN STILL VOUCH FOR: a
// turn's, through statelog.MintAt like every derived id of a turn, and a
// retirement's and a reaper's, because each is the instant of the attempt
// that writes under it. An id minted further back than statelog.MintHorizon
// is answered `unknown` on every node for good and never published.
//
// # What a write answers
//
// Every write answers its whole [statelog.Result] and never a bare position,
// because a position cannot tell `applied` and `pending` from `unknown`; and a
// capture the rows refused at the apply (an end, a newer capture, another
// conversation's capture that took the budget, or another turn's begin that
// came to hold a base the budget would have released, between the decide and
// the apply) answers the refusal its own row says, read back, as a begin says
// whether its row holds the base it named.

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
// turn, the launch it was taken from and its ordinal within that launch. One
// capture, one operation, however often it is retried; the next capture of
// the same launch, another.
//
// minted is the instant the turn's derived operations are minted at: its
// work's start, or the instant a turn rebased onto once that start was past
// statelog.MintHorizon of the attempt's own clock (statelog.MintAt, recorded in
// coord.Rebases), as every derived id a turn writes is. NEVER THE LAUNCH'S
// START TAKEN AS IT IS: a launch parked on a person's answer can resume and
// capture a month after it started, and an id minted that far back is one no
// node's ledger vouches for, answered `unknown` and never published.
func CaptureOpID(minted time.Time, handle, turn, launch string, captureSeq int64) string {
	return statelog.DeriveOpID(minted, "executor-capture", "crewlet.executor",
		handle, turn, launch, fmt.Sprint(captureSeq))
}

// BeginOpID is the operation a turn's begin is published under: the seat and
// the turn, at the instant the turn's derived operations are minted at — the
// same minted [CaptureOpID] takes. A turn begins once, however often its first
// launch asks.
func BeginOpID(minted time.Time, handle, turn string) string {
	return statelog.DeriveOpID(minted, "executor-begin", "crewlet.executor", handle, turn)
}

// EndOpID is the operation a turn's end is published under: the seat and the
// turn, at the instant the turn's derived operations are minted at — the same
// minted [CaptureOpID] takes, past statelog.MintAt's rebase. A turn ends once,
// however many paths try to say so.
func EndOpID(minted time.Time, handle, turn string) string {
	return statelog.DeriveOpID(minted, "executor-end", "crewlet.executor", handle, turn)
}

// SeatDeleteOpID is the operation a removed seat's rows are deleted under: the
// seat, at the instant the retirement removing it was marked.
//
// THE RETIREMENT'S OWN INSTANT, NEVER THE ABSENCE'S. A retirement runs late
// whenever it has to (a node still holding the seat, runs that cannot be ended
// yet, no duty holder), and an id minted at the instant the absence was first
// seen would be past statelog.MintHorizon for one held back a month: answered
// `unknown` on every node and never published, so the retirement would fail
// on every later tick. Each attempt is marked anew, so a retried retirement
// derives a new operation and publishes the removal again, which applies
// idempotently ([SeatDelete]); the attempt's own retries share one.
func SeatDeleteOpID(retiring time.Time, handle string) string {
	return statelog.DeriveOpID(retiring, "executor-seat-delete", "crewlet.executor",
		handle, retiring.UTC().Format(time.RFC3339Nano))
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

	Harness        string
	HarnessVersion string
	SessionID      string

	// Object is the sealed object, ContentKey the keyring key that sealed
	// it, and ArchiveFormat the layout of the archive inside.
	Object        objstore.Object
	ContentKey    string
	ArchiveFormat int

	LaunchSeq  int64
	CaptureSeq int64

	Home      string
	Workspace string

	// ContextTokens is the whole input of the session's last request, as its
	// transcript recorded it, and Interrupted that its harness was cut off
	// ([Capture.ContextTokens], [Capture.Interrupted]): the run's stamp of
	// the capture, which a launch continuing the session after its turn
	// ended reads off the row.
	ContextTokens int64
	Interrupted   bool

	// ReadThrough is the position the turn's first launch read the
	// conversation's head at, zero where it read none, and Absorbs the
	// turns that read handed out in full ([Head.Absorbs]): both from the one
	// [Head] answer the launch began from.
	ReadThrough statelog.Position
	Absorbs     []string

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
		{"the harness", n.Harness, executorkeep.HarnessMaxBytes},
		{"the harness's version", n.HarnessVersion, executorkeep.HarnessVersionMaxBytes},
		{"the session id", n.SessionID, executorkeep.SessionIDMaxBytes},
		{"the content key", n.ContentKey, ContentKeyMaxBytes},
		{"the home path", n.Home, executorkeep.PathMaxBytes},
		{"the workspace path", n.Workspace, executorkeep.PathMaxBytes},
	} {
		if err := checkText(f.field, f.value, f.max); err != nil {
			return err
		}
	}
	switch {
	case strings.TrimSpace(n.Harness) == "":
		return invalid("the capture names no harness")
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
	case n.ContextTokens < 0:
		return invalid("the capture's context is %d tokens, and a request carries none or more",
			n.ContextTokens)
	case n.ReadThrough.Seq > 0 && n.ReadThrough.Stream != Domain{}.Stream().Name:
		return invalid("the head was read on %q, which is not this log", n.ReadThrough.Stream)
	}
	var readThrough int64
	if n.ReadThrough.Seq > 0 {
		readThrough = n.ReadThrough.Packed()
	}
	if _, ok := absorbsOf(n.Absorbs, n.Turn, readThrough); !ok {
		return invalid("the capture's absorbed turns %s are not what a head read hands "+
			"out: at most %d canonical turn ids, none twice and never the turn's own, "+
			"beside the position they were read at", words.AndMore(n.Absorbs, MaxAbsorbs), MaxAbsorbs)
	}
	if err := n.Object.Validate(); err != nil {
		return fmt.Errorf("%w: the capture's object: %w", ErrInvalid, err)
	}
	if n.Object.Size > executorkeep.SealedMaxBytes {
		return invalid("the capture's object is %d bytes and a sealed session is at "+
			"most %d", n.Object.Size, executorkeep.SealedMaxBytes)
	}
	return nil
}

// ContentKeyMaxBytes is the longest content key a capture names: a keyring
// key id a sealed stream's header can carry. Exported for the readers that
// carry a row's content key on in a record of their own and state that
// record's bound from the log's (internal/sandbox's ConversationBaseMaxBytes).
const ContentKeyMaxBytes = 255

// checkTurn refuses the identity every record about a turn carries: a seat's
// handle, a turn id in its one canonical spelling ([canonicalTurn]), and keys
// within their bounds.
func checkTurn(handle, turn, conversationKey, workKey string) error {
	switch {
	case !iam.ValidSeatHandle(handle):
		return invalid("%q is not a seat's handle", handle)
	case len(turn) > executorkeep.TurnIDMaxBytes:
		return invalid("the turn id is %d bytes and the maximum is %d", len(turn), executorkeep.TurnIDMaxBytes)
	case !canonicalTurn(turn):
		return invalid("the turn id %q is not a run id in its canonical spelling: "+
			"a UUID, lowercase and dashed, as the engine mints one", turn)
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
	// end, a newer capture, a removal of the seat, another conversation's
	// capture that took the budget, or another turn's begin that came to
	// hold a base the budget would have released got there first), and on a
	// pending or unknown one that this node has not seen it apply yet.
	Recorded bool
}

// captureReadBackStatement is the object one turn's row names.
const captureReadBackStatement = `
	SELECT object FROM executor_sessions INDEXED BY ` + sessionKey + ` WHERE agent_handle = ? AND turn_id = ?`

// Capture records a new object for a live turn's session.
//
// Refused at the decide: an object minted longer than objstore.RecordWithin
// ago by this node's clock ([ErrUploadStale]), a turn that has ended or been
// removed ([ErrTurnEnded]), ordinals not newer than the row's
// ([ErrStaleCapture]), another conversation or unit of work than the turn's
// ([ErrInvalid]), and a capture the seat's budget cannot hold with every ended
// session no other live turn holds as its base released ([ErrOverBudget]). The
// apply repeats every guard but the age, and an applied capture is read back
// ([CaptureResult.Recorded]).
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
		Harness: n.Harness, HarnessVersion: n.HarnessVersion, SessionID: n.SessionID,
		Object: n.Object.Key.String(), ObjectHash: string(n.Object.Hash),
		ObjectSize: n.Object.Size, ContentKey: n.ContentKey, ArchiveFormat: n.ArchiveFormat,
		LaunchSeq: n.LaunchSeq, CaptureSeq: n.CaptureSeq,
		Home: n.Home, Workspace: n.Workspace, SeatBudget: n.SeatBudget,
		ContextTokens: n.ContextTokens, Interrupted: n.Interrupted,
		Absorbs: n.Absorbs,
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
			removal, decideErr := decideCapture(ctx, tx, n, at)
			if decideErr != nil {
				return statelog.Decision{}, decideErr
			}
			// THE REMOVAL THIS SNAPSHOT SAW, carried so a capture decided
			// before one it did not see writes nothing landing after it.
			decided := p
			decided.SeenRemoval = removal
			return decision(stamp, c.By, subject, OpCapture, scope, c.OpID, decided, at)
		}))
	out := CaptureResult{Result: res}
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		return out, err
	}
	// READ BACK: the apply decides the guards and the budget again in log
	// order, and only the row says whether this capture is the one it names.
	err = w.db.Read(ctx, func(tx *sql.Tx) error {
		var object sql.NullString
		scanErr := tx.QueryRowContext(ctx, captureReadBackStatement, n.Handle, n.Turn).Scan(&object)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		out.Recorded = scanErr == nil && object.Valid && object.String == p.Object
		return scanErr
	})
	return out, err
}

// turnEnded is [ErrTurnEnded] for a turn that has ended or whose seat was
// removed, and nil for one that has not: what every decide about a turn
// refuses a closed one by.
//
// THE TOMBSTONE ALONE DECIDES IT, never the row's own end. An end writes the
// turn's tombstone in the very apply that ends its row ([applyEnd]), and only
// the retention removes a tombstone, together with its row ([removeTurn]), so
// no ended row is without one; and a turn whose row a later turn's end
// absorbed, or its seat's removal deleted, has the tombstone alone — which a
// test of the row would let through as a turn with no row yet.
func turnEnded(ctx context.Context, tx *sql.Tx, handle, turn string) error {
	_, tombed, err := readTomb(ctx, tx, handle, turn)
	switch {
	case err != nil:
		return err
	case tombed:
		return fmt.Errorf("%w: %s's turn %s", ErrTurnEnded, handle, turn)
	}
	return nil
}

// decideCapture is the capture's decide over one snapshot of the turn and its
// seat: every refusal [Writer.Capture] names. It answers the seat's removal the
// snapshot holds, which the record carries ([Capture.SeenRemoval]).
//
// NOTHING IS REFUSED FOR BEING MINTED IN THE FUTURE. A key that a fast clock
// minted ahead of this one protects itself: the collector deletes nothing
// whose minting instant is inside its grace.
func decideCapture(ctx context.Context, tx *sql.Tx, n NewCapture, at time.Time) (int64, error) {
	if age := at.Sub(n.Object.Key.Minted()); age > objstore.RecordWithin {
		return 0, fmt.Errorf("%w: object %s was minted %s ago and a capture names an "+
			"upload at most %s after it", ErrUploadStale, n.Object.Key,
			age.Truncate(time.Second), objstore.RecordWithin)
	}
	removal, err := readRemoval(ctx, tx, n.Handle)
	if err != nil {
		return 0, err
	}
	if err = turnEnded(ctx, tx, n.Handle, n.Turn); err != nil {
		return 0, err
	}
	row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
	if err != nil {
		return 0, err
	}
	if found {
		switch {
		case row.conversationKey != n.ConversationKey:
			return 0, invalid("turn %s belongs to another conversation than the capture names",
				n.Turn)
		case !sameWork(row.workKey, n.WorkKey):
			return 0, invalid("turn %s was dispatched for work %q and the capture names %q",
				n.Turn, row.workKey, n.WorkKey)
		case !newer(n.LaunchSeq, n.CaptureSeq, row.launchSeq, row.captureSeq):
			return 0, fmt.Errorf("%w: turn %s holds (%d, %d) and this capture is (%d, %d)",
				ErrStaleCapture, n.Turn, row.launchSeq, row.captureSeq, n.LaunchSeq, n.CaptureSeq)
		}
	}
	_, fits, err := seatPlan(ctx, tx, n.Handle, n.Turn, n.Object.Size, n.SeatBudget)
	if err != nil {
		return 0, err
	}
	if !fits {
		return 0, fmt.Errorf("%w: %s's live turns, and the bases their first reads hold, take more "+
			"than %d bytes beside this %d-byte capture", ErrOverBudget, n.Handle, n.SeatBudget,
			n.Object.Size)
	}
	return removal, nil
}

// ---- begin -------------------------------------------------------------- //

// NewBegin is a turn's first read of its conversation, and the base that read
// froze: what the turn's first launch publishes once its run is recorded.
type NewBegin struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// ReadThrough is the position the head was read at ([Head.Position]) and
	// Absorbs the turns the read handed out that the turn was told of: what
	// the turn's end absorbs once it has landed a capture.
	ReadThrough statelog.Position
	Absorbs     []string

	// BaseTurn is the head the read named, and BaseObject the session its row
	// named then: what the turn's launches continue while it keeps no
	// session of its own. Both zero where the read named no head.
	BaseTurn   string
	BaseObject objstore.Object
}

// validate refuses a begin the record could not carry, naming the field.
func (n NewBegin) validate() error {
	if err := checkTurn(n.Handle, n.Turn, n.ConversationKey, n.WorkKey); err != nil {
		return err
	}
	switch {
	case n.ConversationKey == "":
		return invalid("the begin names no conversation: a turn with none read no head, " +
			"and continues no session")
	case n.ReadThrough.Seq == 0:
		return invalid("the begin names no read: it records where the turn's first launch " +
			"read its conversation")
	case n.ReadThrough.Stream != Domain{}.Stream().Name:
		return invalid("the head was read on %q, which is not this log", n.ReadThrough.Stream)
	}
	if _, ok := absorbsOf(n.Absorbs, n.Turn, n.ReadThrough.Packed()); !ok {
		return invalid("the begin's absorbed turns %s are not what a head read hands out: at "+
			"most %d canonical turn ids, none twice and never the turn's own",
			words.AndMore(n.Absorbs, MaxAbsorbs), MaxAbsorbs)
	}
	if n.BaseTurn == "" {
		if !n.BaseObject.Key.IsZero() || n.BaseObject.Hash != "" {
			return invalid("the begin names a base session and no base turn")
		}
		return nil
	}
	// THE BASE IS ONE OF THE TURNS THE READ HANDED OUT, which absorbsOf has
	// just held to canonical turn ids other than this turn's own: so a base
	// among them is a turn in its one spelling and never the turn itself,
	// and neither needs a test of its own.
	if !slices.Contains(n.Absorbs, n.BaseTurn) {
		return invalid("the base turn %q is not among the turns the read handed out, and a "+
			"read's head always is", n.BaseTurn)
	}
	if err := n.BaseObject.Validate(); err != nil {
		return fmt.Errorf("%w: the base's object: %w", ErrInvalid, err)
	}
	return nil
}

// BeginResult is what a begin answers: the publish's result, and whether the
// turn's row holds the base the begin named now.
type BeginResult struct {
	statelog.Result

	// Held is true when the turn's row was read back holding the base: live,
	// no capture of its own landed, naming the base. Why says, where it is
	// false, what kept it from holding — no base named, a base that moved
	// before the begin landed, a turn already ended or captured, or an
	// outcome this node has not seen applied yet.
	Held bool
	Why  string
}

// Begin records a turn's first read of its conversation and the base that read
// froze, which the turn's row then holds against every release but a
// retirement while it is live and has landed no capture ([Begin]).
//
// Refused at the decide: a turn that has ended or been removed
// ([ErrTurnEnded]), and one whose row names another conversation or unit of
// work ([ErrInvalid]). A turn whose row already holds its first read publishes
// nothing — that read stands, whichever record carried it — and answers what
// the row holds. The apply repeats every guard, decides in log order whether
// the base still stands, and the row is read back ([BeginResult.Held]).
func (w *Writer) Begin(ctx context.Context, c Call, n NewBegin) (BeginResult, error) {
	if err := c.validate(); err != nil {
		return BeginResult{}, err
	}
	if err := n.validate(); err != nil {
		return BeginResult{}, err
	}
	p := Begin{
		V: baseRecordVersion, Handle: n.Handle, Turn: n.Turn,
		ConversationKey: n.ConversationKey, WorkKey: n.WorkKey,
		ReadThrough: n.ReadThrough.Packed(), Absorbs: n.Absorbs, BaseTurn: n.BaseTurn,
	}
	if n.BaseTurn != "" {
		p.BaseObject, p.BaseHash = n.BaseObject.Key.String(), string(n.BaseObject.Hash)
	}
	subject := ConversationSubject(n.Handle, n.ConversationKey, n.Turn)
	// THE CONVERSATION, NOT THE SEAT: a begin writes its own turn's row and
	// reads only rows of its own conversation. A capture of another
	// conversation is not arbitrated against it, and need not be: its apply
	// asks about the hold in log order, and its seat-wide scope contains this
	// one, so a node that defers either defers the other.
	scope := ScopeSet{Subject: true}
	res, err := publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			removal, begun, decideErr := decideBegin(ctx, tx, n)
			switch {
			case decideErr != nil:
				return statelog.Decision{}, decideErr
			case begun:
				// THE TURN'S FIRST READ STANDS: nothing to publish, and the
				// row, read back below, says what it holds.
				return statelog.Decision{}, nil
			}
			decided := p
			decided.SeenRemoval = removal
			return decision(stamp, c.By, subject, OpBegin, scope, c.OpID, decided, w.now())
		}))
	out := BeginResult{Result: res}
	if err != nil {
		return out, err
	}
	if res.Outcome != statelog.OutcomeApplied {
		out.Why = fmt.Sprintf("the log answered %s, and this node has not seen the begin apply",
			res.Outcome)
		return out, nil
	}
	// READ BACK: the apply decides the guards and the base again in log
	// order, and only the row says whether the base is held.
	err = w.db.Read(ctx, func(tx *sql.Tx) error {
		var readErr error
		out.Held, out.Why, readErr = beginHolds(ctx, tx, n, p)
		return readErr
	})
	return out, err
}

// decideBegin is the begin's decide over one snapshot of the turn: a turn
// already ended or removed ([turnEnded]), and one whose row names another
// conversation or unit of work, are refused; a row that already holds the
// turn's first read is begun, with nothing left to publish. It answers the
// seat's removal the snapshot holds.
func decideBegin(ctx context.Context, tx *sql.Tx, n NewBegin) (removal int64, begun bool, err error) {
	if removal, err = readRemoval(ctx, tx, n.Handle); err != nil {
		return 0, false, err
	}
	if err = turnEnded(ctx, tx, n.Handle, n.Turn); err != nil {
		return 0, false, err
	}
	row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
	switch {
	case err != nil:
		return 0, false, err
	case !found:
		return removal, false, nil
	case row.conversationKey != n.ConversationKey:
		return 0, false, invalid("turn %s belongs to another conversation than the begin names", n.Turn)
	case !sameWork(row.workKey, n.WorkKey):
		return 0, false, invalid("turn %s was dispatched for work %q and the begin names %q",
			n.Turn, row.workKey, n.WorkKey)
	}
	return removal, row.readThrough > 0, nil
}

// beginHolds reads back whether the turn's row holds the base the begin named
// and, where it does not, why.
func beginHolds(ctx context.Context, tx *sql.Tx, n NewBegin, p Begin) (bool, string, error) {
	if n.BaseTurn == "" {
		return false, "the read named no base", nil
	}
	row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
	switch {
	case err != nil:
		return false, "", err
	case !found:
		return false, "the log holds no row of the turn: its seat was removed before the begin " +
			"landed", nil
	case row.ended:
		return false, "the turn ended before its begin landed", nil
	case row.launchSeq > 0:
		return false, "the turn landed a capture of its own first, which its launches continue " +
			"instead", nil
	case row.baseTurn == n.BaseTurn:
		return true, "", nil
	case row.readThrough != p.ReadThrough:
		return false, "the turn's first read was recorded at another position, and it stands", nil
	}
	why, err := baseStands(ctx, tx, n.Handle, n.ConversationKey, p.ReadThrough, n.BaseTurn,
		p.BaseObject, p.BaseHash)
	if err != nil {
		return false, "", err
	}
	if why == "" {
		why = "the base did not stand when the begin applied"
	}
	return false, why, nil
}

// ---- end ---------------------------------------------------------------- //

// NewEnd is how one turn ended.
type NewEnd struct {
	Handle          string
	Turn            string
	ConversationKey string
	WorkKey         string

	// Ask is what the turn was asked; the record keeps its first
	// [AskExcerptMaxBytes] as it keeps somebody's words, redacted before it is
	// cut on a rune boundary (executorkeep.Excerpt).
	Ask string

	Ending executorkeep.Ending
}

// End records how a turn ended, and tombstones it: no capture lands for it
// after this, and a second end is [ErrTurnEnded]. With no row it writes an
// ended row naming no object; with one it absorbs and dedupes the
// conversation's ended rows (see the package doc). An end naming another
// conversation or another unit of work than the turn's row is [ErrInvalid]:
// the end is authoritative for the turn's work key where its captures carried
// none, and contradicts nothing they carried.
func (w *Writer) End(ctx context.Context, c Call, n NewEnd) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := checkTurn(n.Handle, n.Turn, n.ConversationKey, n.WorkKey); err != nil {
		return statelog.Result{}, err
	}
	if err := n.Ending.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !utf8.ValidString(n.Ask) {
		return statelog.Result{}, invalid("the ask is not valid UTF-8")
	}
	p := End{
		V: baseRecordVersion, Handle: n.Handle, Turn: n.Turn,
		ConversationKey: n.ConversationKey, WorkKey: n.WorkKey,
		AskExcerpt: executorkeep.Excerpt(n.Ask, AskExcerptMaxBytes), Ending: n.Ending.Fit(),
	}
	subject := ConversationSubject(n.Handle, n.ConversationKey, n.Turn)
	scope := ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			removal, decideErr := decideEnd(ctx, tx, n)
			if decideErr != nil {
				return statelog.Decision{}, decideErr
			}
			decided := p
			decided.SeenRemoval = removal
			return decision(stamp, c.By, subject, OpEnd, scope, c.OpID, decided, w.now())
		}))
}

// decideEnd is the end's decide over one snapshot of the turn: a turn already
// ended or removed ([turnEnded]), and one whose row names another conversation
// or unit of work, are refused. It answers the seat's removal the snapshot
// holds.
func decideEnd(ctx context.Context, tx *sql.Tx, n NewEnd) (int64, error) {
	removal, err := readRemoval(ctx, tx, n.Handle)
	if err != nil {
		return 0, err
	}
	if err = turnEnded(ctx, tx, n.Handle, n.Turn); err != nil {
		return 0, err
	}
	row, found, err := readSession(ctx, tx, n.Handle, n.Turn)
	if err != nil {
		return 0, err
	}
	switch {
	case !found:
	case row.conversationKey != n.ConversationKey:
		return 0, invalid("turn %s belongs to another conversation than the end names", n.Turn)
	case !sameWork(row.workKey, n.WorkKey):
		return 0, invalid("turn %s was dispatched for work %q and the end names %q",
			n.Turn, row.workKey, n.WorkKey)
	}
	return removal, nil
}

// lostFailure is the account an end the retention duty records for a turn
// whose own end was lost carries ([Writer.EndLost]). It is kept in the
// replicated ending and told to the conversation's next turn, so it names the
// turn's LAST RECORD rather than its last capture: a turn whose begin wrote
// its row may have captured nothing at all.
const lostFailure = "no end was recorded for this turn, and no run of it was live a day " +
	"after its last record: its end was lost"

// EndLost records a turn whose end was lost as ended `lost`, as the retention
// duty, under the operation the duty derived: a turn that never ended, whose
// last record — its newest capture, or the begin of a turn that captured
// nothing — was at or before before, and which the duty found no live run of
// ([LostAfter]). Its session is kept as any ended turn's is — the next turn on
// the conversation continues it — and what the end releases is the seat's
// budget, which never releases a live turn's session, and the base the turn
// held, if it captured nothing. A turn that ended, or was captured, since the
// duty listed it is refused [ErrTurnEnded] or [ErrNotDue].
func (w *Writer) EndLost(ctx context.Context, c Call, handle, conversationKey, turn string,
	before time.Time) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	n := NewEnd{Handle: handle, Turn: turn, ConversationKey: conversationKey,
		Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeLost, Failure: lostFailure}}
	if err := checkTurn(handle, turn, conversationKey, ""); err != nil {
		return statelog.Result{}, err
	}
	p := End{V: baseRecordVersion, Handle: handle, Turn: turn,
		ConversationKey: conversationKey, Ending: n.Ending.Fit()}
	subject := ConversationSubject(handle, conversationKey, turn)
	scope := ScopeSet{Subject: true}
	limit := store.EncodeTime(before)
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			removal, decideErr := decideEnd(ctx, tx, n)
			if decideErr != nil {
				return statelog.Decision{}, decideErr
			}
			row, found, readErr := readSession(ctx, tx, handle, turn)
			switch {
			case readErr != nil:
				return statelog.Decision{}, readErr
			case !found:
				return statelog.Decision{}, fmt.Errorf("%w: %s's turn %s", ErrNotFound,
					handle, turn)
			case row.lastAt > limit:
				return statelog.Decision{}, fmt.Errorf("%w: %s's turn %s was captured "+
					"after it was listed", ErrNotDue, handle, turn)
			}
			decided := p
			decided.SeenRemoval = removal
			return decision(stamp, c.By, subject, OpEnd, scope, c.OpID, decided, w.now())
		}))
}

// ---- delete ------------------------------------------------------------- //

// DeleteExpired removes one turn's row and tombstone whose last instant is at
// or before cutoff, as the retention duty, under the operation the duty
// derived. It is decided again in the conversation's snapshot and once more at
// the apply, so a turn captured since it was listed, or held since as a live
// turn's base ([held]), is refused [ErrNotDue] or left in place.
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
			due, dueErr := expiredIn(ctx, tx, handle, conversationKey, turn,
				store.EncodeTime(cutoff))
			if dueErr != nil {
				return statelog.Decision{}, dueErr
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
	err = w.db.Read(ctx, func(tx *sql.Tx) error {
		_, found, readErr := readSession(ctx, tx, handle, turn)
		if readErr != nil || found {
			left = found
			return readErr
		}
		_, left, readErr = readTomb(ctx, tx, handle, turn)
		return readErr
	})
	if err != nil {
		return res, err
	}
	if left {
		return res, fmt.Errorf("%w: %s's turn %s was captured, or held as a live turn's base, "+
			"after it was listed", ErrNotDue, handle, turn)
	}
	return res, nil
}

// ExpireRemoval forgets a removed seat's record of its removal once that
// removal is at or before cutoff, as the retention duty, under the operation
// the duty derived: by then no capture or end decided before the removal can
// still land, which is all the record is kept to refuse. A removal made since
// the duty listed it is refused [ErrNotDue].
func (w *Writer) ExpireRemoval(ctx context.Context, c Call, handle string,
	cutoff time.Time) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if !iam.ValidSeatHandle(handle) {
		return statelog.Result{}, invalid("%q is not a seat's handle", handle)
	}
	p := Delete{V: baseRecordVersion, Handle: handle, Cutoff: cutoff.UTC()}
	subject := SeatSubject(handle)
	scope := ScopeSet{Subject: true}
	limit := store.EncodeTime(cutoff)
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			var at int64
			scanErr := tx.QueryRowContext(ctx, removalAtStatement, handle).Scan(&at)
			switch {
			case errors.Is(scanErr, sql.ErrNoRows):
				return statelog.Decision{}, fmt.Errorf("%w: seat %s holds no removal", ErrNotDue,
					handle)
			case scanErr != nil:
				return statelog.Decision{}, fmt.Errorf("executordomain: read seat %s's "+
					"removal: %w", handle, scanErr)
			case at > limit:
				return statelog.Decision{}, fmt.Errorf("%w: seat %s was removed again since "+
					"it was listed", ErrNotDue, handle)
			}
			return decision(stamp, c.By, subject, OpDelete, scope, c.OpID, p, w.now())
		}))
}

// removalAtStatement is the broker instant a seat's rows were last removed at.
const removalAtStatement = `
	SELECT at FROM executor_seat_removals WHERE agent_handle = ?`

// DeleteSeat removes every row a seat the company removed holds, as the
// mailbox retirement, under [SeatDeleteOpID], and records where it did.
//
// ALWAYS PUBLISHED, whatever this node's copy holds: the record removes, in log
// order on every node, every row any record before it wrote — including rows
// this node has not applied yet — and its position is what refuses a capture or
// an end decided before it and landing after ([Capture.SeenRemoval]). Decided
// from this node's rows, a copy behind the log would answer "nothing to remove"
// for a seat that holds sessions, and a retirement would end believing they
// were gone. Its apply is idempotent, so a second removal costs one record.
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
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpSeatDelete, scope, c.OpID,
				SeatDelete{V: baseRecordVersion, Handle: handle}, w.now())
		}))
}
