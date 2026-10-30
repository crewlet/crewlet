package assistantdomain

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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ASSISTANT WRITE PATH: one record per step of a turn, published to the
// subject of the thing it contends for, decided inside one snapshot of this
// node's own rows.
//
// # Every write is the owner's
//
// A [Call] names the conversation's owner, and every decide refuses a
// conversation, turn or proposal that is not theirs as not found, in the same
// snapshot the record is formed in. So a turn of one person can never record
// into another person's conversation, whichever id it was handed.
//
// # The writer seals
//
// Every value a person said or was shown is sealed here, before the record is
// formed, under associated data naming the conversation and the record
// ([AAD]). The applier writes the envelope through.
//
// # What a write answers
//
// Every write answers its whole [statelog.Result] and never a bare position,
// because a position cannot tell `applied` and `pending` from `unknown`; and a
// write the rows refused at the apply (a conversation purged between the
// decide and the apply) answers the refusal its own rows say, read back.

// Call is one write's caller: whose conversation it is, who is acting and
// under which operation.
type Call struct {
	// Owner is the person the conversation belongs to, by their
	// identity-estate id.
	Owner uuid.UUID

	// By is the author as iam.ActorFor records the principal acting: the
	// person themselves for everything a turn records, the engine for the
	// retention reaper and an erasure.
	By iam.Actor

	// OpID is the operation the record is published under, held to the
	// state log's grammar (statelog.CheckCallerOpID): derived from the turn
	// or the proposal it belongs to (statelog.StepOpID), so a retry is the
	// same operation and a duplicate publish writes nothing twice.
	OpID string
}

// validate refuses a call the records could not name.
func (c Call) validate(owned bool) error {
	switch {
	case owned && c.Owner == uuid.Nil:
		return invalid("the call names no owner: every conversation is somebody's")
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

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Sealer seals every value before it is published. Required.
	Sealer *Sealer

	// Now is the clock a record's authored instant is read off; nil is the
	// wall clock in UTC. No row stores it.
	Now func() time.Time
}

// Writer is the assistant write path.
type Writer struct {
	publisher *statelog.Publisher
	db        store.ReplicatedReader
	sealer    *Sealer
	now       func() time.Time
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("assistantdomain: a write authority is required: every " +
			"write is a record on the assistant log")
	case deps.DB.IsZero():
		return nil, errors.New("assistantdomain: the replicated estate is required: " +
			"a write decides from this node's own applied rows")
	case deps.Sealer == nil:
		return nil, errors.New("assistantdomain: a sealer is required: every value a " +
			"person said or was shown is sealed before it is published")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, sealer: deps.Sealer, now: deps.Now}
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
		return statelog.Decision{}, fmt.Errorf("assistantdomain: encode the %s payload "+
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

// publish appends one write through the writer's publisher.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	return publish(ctx, w.publisher, req)
}

// publish appends one write, translating the framework's answers into this
// package's: exhausted rounds are a conflict, and a record a deletion marker
// drops is a write to a purged conversation.
func publish(ctx context.Context, publisher *statelog.Publisher,
	req statelog.Request) (statelog.Result, error) {

	res, err := publisher.Publish(ctx, req)
	if errors.Is(err, statelog.ErrConflict) {
		return res, fmt.Errorf("%w: %s: %w", ErrConflict, req.Subject, err)
	}
	var u *statelog.Unavailable
	if errors.As(err, &u) && u.Reason == statelog.ReasonDeleted {
		return res, fmt.Errorf("%w: %s: %w", ErrPurged, req.Subject, err)
	}
	return res, err
}

// request is one write's framework request on subject, its scope resolved.
func request(subject Subject, scope ScopeSet, opID string, pattern statelog.Pattern,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	return statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: pattern, Decide: decide,
	}
}

// owned is a conversation's row read in tx, refused as not found when it is
// another person's or absent, and as purged when its marker holds it.
func owned(ctx context.Context, tx *sql.Tx, owner uuid.UUID, conversation string) (conversationRow, error) {
	row, found, err := readConversation(ctx, tx, conversation)
	if err != nil {
		return conversationRow{}, err
	}
	if found && row.owner == owner.String() {
		return row, nil
	}
	if !found {
		if _, marked, err := deletionMarker(ctx, tx, conversation); err != nil {
			return conversationRow{}, err
		} else if marked {
			return conversationRow{}, fmt.Errorf("%w: %s", ErrPurged, conversation)
		}
	}
	return conversationRow{}, fmt.Errorf("%w: conversation %s", ErrNotFound, conversation)
}

// checkText refuses a text field outside its bounds.
func checkText(field, value string, maxBytes int) error {
	switch {
	case !utf8.ValidString(value):
		return invalid("%s is not valid UTF-8", field)
	case len(value) > maxBytes:
		return invalid("%s is %d bytes and the maximum is %d", field, len(value), maxBytes)
	}
	return nil
}

// checkID refuses a nil id.
func checkID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return invalid("%s is required", field)
	}
	return nil
}

// ---- conversations ------------------------------------------------------ //

// CreateConversation makes conversation id for the call's owner, titled title
// (which may be empty until the first turn names it). Create-only: the same id
// created twice is the same conversation, and a conversation purged once is
// never recreated.
func (w *Writer) CreateConversation(ctx context.Context, c Call, id uuid.UUID,
	title string) (statelog.Result, error) {

	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	if err := checkID("the conversation id", id); err != nil {
		return statelog.Result{}, err
	}
	if err := checkText("the title", title, TitleMaxBytes); err != nil {
		return statelog.Result{}, err
	}
	sealed, err := w.sealer.sealString(title, AAD(id, RecordConversation, id, ColumnTitle))
	if err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := ConversationSubject(id), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			erased, err := erasedOwner(ctx, tx, c.Owner.String())
			if err != nil {
				return statelog.Decision{}, err
			}
			if erased {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrErased, c.Owner)
			}
			return decision(stamp, c.By, subject, OpConversationCreated, scope, c.OpID,
				ConversationCreated{V: baseRecordVersion, Owner: c.Owner.String(),
					TitleSealed: sealed}, at)
		}))
}

// TitleConversation sets a conversation's title.
func (w *Writer) TitleConversation(ctx context.Context, c Call, id uuid.UUID,
	title string) (statelog.Result, error) {

	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	if err := checkText("the title", title, TitleMaxBytes); err != nil {
		return statelog.Result{}, err
	}
	sealed, err := w.sealer.sealString(title, AAD(id, RecordConversation, id, ColumnTitle))
	if err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := ConversationSubject(id), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, id.String()); err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, OpConversationTitled, scope, c.OpID,
				ConversationTitled{V: baseRecordVersion, TitleSealed: sealed}, at)
		}))
}

// ClearConversation purges one of the owner's conversations at once, the Clear
// gesture: everything in it goes, and its deletion marker keeps any later
// record about it from writing a row. Revoking the turn's grants and declining
// its proposals is the orchestrator's, in the same gesture.
func (w *Writer) ClearConversation(ctx context.Context, c Call, id uuid.UUID) (statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := ConversationSubject(id), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, id.String()); err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, OpConversationPurged, scope, c.OpID,
				ConversationPurged{V: GateRecordVersion, Reason: PurgeCleared}, at)
		}))
}

// PurgeInactive purges a conversation untouched since before cutoff, as the
// retention reaper. It is decided again in the conversation's snapshot and
// once more at the apply ([ConversationPurged.InactiveBefore]), so a
// conversation used since it was listed is refused [ErrNotDue] or left in
// place.
func (w *Writer) PurgeInactive(ctx context.Context, by iam.Actor, opID string,
	id uuid.UUID, cutoff time.Time) (statelog.Result, error) {

	c := Call{By: by, OpID: opID}
	if err := c.validate(false); err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := ConversationSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			row, found, err := readConversation(ctx, tx, id.String())
			if err != nil {
				return statelog.Decision{}, err
			}
			if !found || row.lastActive >= store.EncodeTime(cutoff) {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrNotDue, id)
			}
			return decision(stamp, by, subject, OpConversationPurged, scope, opID,
				ConversationPurged{V: GateRecordVersion, Reason: PurgeRetention,
					InactiveBefore: cutoff.UTC()}, at)
		}))
	if err != nil {
		return res, err
	}
	// THE APPLY MAY HAVE REFUSED IT: a message recorded between the decide
	// and the apply keeps the conversation, and only its marker says the
	// purge landed.
	if res.Outcome == statelog.OutcomeApplied {
		var marked bool
		if err := w.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			_, marked, err = deletionMarker(ctx, tx, id.String())
			return err
		}); err != nil {
			return res, err
		}
		if !marked {
			return res, fmt.Errorf("%w: %s was used after it was listed", ErrNotDue, id)
		}
	}
	return res, nil
}

// EraseOwner removes every conversation the call's owner holds, for good, in
// one record, and keeps any conversation from being created for them again. It
// is the assistant's half of a person's removal from the identity estate,
// published by the engine when the identity applier reports the removal.
//
// IDEMPOTENT: an owner already erased answers the erasure that landed, with
// nothing published.
func (w *Writer) EraseOwner(ctx context.Context, c Call) (statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject := OwnerSubject(c.Owner)
	// THE WHOLE DOMAIN, the widest scope a record may name: an erasure's
	// rows are every conversation the owner holds, which no bounded list of
	// terms could name, so a node that defers it has every read here
	// reported stale until it can apply it.
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			erased, err := erasedOwner(ctx, tx, c.Owner.String())
			if err != nil || erased {
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, OpOwnerErased, scope, c.OpID,
				OwnerErased{V: GateRecordVersion, Owner: c.Owner.String()}, at)
		}))
}

// AuditReceipt is the proof that a compliance read was recorded: the one
// thing [Reader.AuditTranscript] opens a conversation for. Only
// [Writer.RecordAuditRead] mints one, so no path reads somebody else's
// conversation without leaving the record its owner sees.
type AuditReceipt struct {
	conversation uuid.UUID
	opID         string
	result       statelog.Result
}

// Result is what the audit record's publish answered.
func (r AuditReceipt) Result() statelog.Result { return r.result }

// RecordAuditRead records an administrator's compliance read of a
// conversation (decision D06): who read it, from the call's actor, and the
// reason they gave, sealed. Additive: two reads never contend. The receipt it
// answers is what [Reader.AuditTranscript] reads the conversation with.
//
// The verb's authority (admin, a recent proof, a person present, never through
// the assistant) is the authority table's, decided at the route before this is
// called; the audit event naming the reader is the route's as well.
func (w *Writer) RecordAuditRead(ctx context.Context, c Call, conversation uuid.UUID,
	reason string) (AuditReceipt, error) {

	if err := c.validate(false); err != nil {
		return AuditReceipt{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return AuditReceipt{}, invalid("a compliance read needs a reason, which its " +
			"owner is shown beside the reader's name")
	}
	if err := checkText("the reason", reason, AuditReasonMaxBytes); err != nil {
		return AuditReceipt{}, err
	}
	record := auditRecordID(c.OpID)
	sealed, err := w.sealer.sealString(reason, AAD(conversation, RecordAuditRead, record, ColumnReason))
	if err != nil {
		return AuditReceipt{}, err
	}
	at := w.now()
	subject, scope := ConversationSubject(conversation), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternAdditive,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, found, readErr := readConversation(ctx, tx, conversation.String()); readErr != nil {
				return statelog.Decision{}, readErr
			} else if !found {
				return statelog.Decision{}, fmt.Errorf("%w: conversation %s",
					ErrNotFound, conversation)
			}
			return decision(stamp, c.By, subject, OpConversationAuditRead, scope, c.OpID,
				AuditReadRecorded{V: baseRecordVersion, ReasonSealed: sealed}, at)
		}))
	if err != nil {
		return AuditReceipt{}, err
	}
	return AuditReceipt{conversation: conversation, opID: c.OpID, result: res}, nil
}

// AuditNamespace is the namespace a compliance read's record id is derived in
// from its operation. FIXED for the life of the format.
var AuditNamespace = uuid.MustParse("4b6c0f3a-9d21-5e8f-a7c4-1e2d3f4a5b6c")

// auditRecordID is the id a compliance read's sealed reason is bound to.
func auditRecordID(opID string) uuid.UUID {
	return uuid.NewSHA1(AuditNamespace, []byte(opID))
}
