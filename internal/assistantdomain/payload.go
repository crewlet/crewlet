package assistantdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// A record carries its COMPLETE new values, never a rule for computing them: a
// turn's number, a proposal's expiry and every sealed value are decided by the
// writer. That is what makes a replay from zero produce the rows the original
// apply did, and it is what lets the applier stay a pure function of the
// record and the rows.
//
// IDS TRAVEL AS TEXT, not as uuid.UUID: a payload is decoded on every node,
// and an id refused by a decoder would be a record that stalls the log rather
// than one the applier drops deterministically. The applier parses them.
//
// EVERY `*Sealed` FIELD IS A KEYRING ENVELOPE the writer sealed ([Sealer]), or
// the empty string for an empty value. The applier writes it through and never
// opens it.

// PurgeReason is why a conversation was purged.
type PurgeReason string

const (
	// PurgeCleared is the person's own Clear gesture.
	PurgeCleared PurgeReason = "cleared"

	// PurgeRetention is the retention reaper: the conversation was untouched
	// for its whole retention.
	PurgeRetention PurgeReason = "retention"

	// PurgeErased is the person's erasure, which takes every conversation
	// they own in one record ([OpOwnerErased]).
	PurgeErased PurgeReason = "erased"
)

// PurgeReasons are the three.
var PurgeReasons = []PurgeReason{PurgeCleared, PurgeRetention, PurgeErased}

// Valid reports whether a reason off the wire is one this build knows.
func (r PurgeReason) Valid() bool { return slices.Contains(PurgeReasons, r) }

// ConversationCreated makes a conversation.
type ConversationCreated struct {
	V           int    `json:"v"`
	Owner       string `json:"owner"`
	TitleSealed string `json:"title_sealed,omitempty"`
}

// ConversationTitled sets a conversation's title.
type ConversationTitled struct {
	V           int    `json:"v"`
	TitleSealed string `json:"title_sealed"`
}

// ConversationPurged removes a conversation. A retention purge names the
// instant the conversation had to be untouched since, and the apply refuses it
// when activity landed after the writer decided: the activity is on the turn's
// own subject, so the conversation's arbitration does not see it.
type ConversationPurged struct {
	V      int         `json:"v"`
	Reason PurgeReason `json:"reason"`

	// InactiveBefore is set on a retention purge only.
	InactiveBefore time.Time `json:"inactive_before,omitzero"`
}

// AuditReadRecorded is one administrator's compliance read. Who read is the
// record's own actor.
type AuditReadRecorded struct {
	V            int    `json:"v"`
	ReasonSealed string `json:"reason_sealed"`
}

// FileRecorded is one file kept for the conversation whose subject it is on:
// its name sealed, the type sniffed from its bytes, and the object its SEALED
// content was uploaded into before this record was published.
type FileRecorded struct {
	V int `json:"v"`

	File string `json:"file"`
	// Turn is empty for an attachment uploaded before its question.
	Turn   string          `json:"turn,omitempty"`
	Owner  string          `json:"owner"`
	Origin wire.FileOrigin `json:"origin"`

	NameSealed string `json:"name_sealed"`
	MediaType  string `json:"media_type"`
	// Size is the file's own size; ObjectSize and Hash are the sealed
	// bytes' the object holds, which every read is checked against.
	Size       int64  `json:"size"`
	Object     string `json:"object"`
	Hash       string `json:"hash"`
	ObjectSize int64  `json:"object_size"`
	// ContentKey is the keyring key the content is sealed under, read off
	// its envelope by the writer: what a rotation counts.
	ContentKey string `json:"content_key"`
}

// TurnOpened opens a turn in the conversation whose subject it is on.
type TurnOpened struct {
	V int `json:"v"`

	Turn  string `json:"turn"`
	Owner string `json:"owner"`

	// Seq is the conversation's turn count plus one, decided in the
	// conversation's snapshot, so the broker's arbitration makes it unique.
	Seq int `json:"seq"`

	Node            string `json:"node,omitempty"`
	PromptVersion   string `json:"prompt_version,omitempty"`
	Model           string `json:"model,omitempty"`
	ProviderKey     string `json:"provider_key,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
	PageTitleSealed string `json:"page_title_sealed,omitempty"`
	PageScreen      string `json:"page_screen,omitempty"`
}

// TurnParked parks a running turn, its loop's conversation sealed beside it.
type TurnParked struct {
	V              int    `json:"v"`
	MessagesSealed string `json:"messages_sealed"`
	// Steps is how many steps the turn has taken so far: what its `done` and
	// `status` frames say whenever the turn is told from its record rather
	// than its frame log. A record without it (an older writer's) parks the
	// turn at zero, as every turn was parked before it was counted.
	Steps int `json:"steps,omitempty"`
}

// TurnResumed hands a parked turn to the runner on Node.
type TurnResumed struct {
	V    int    `json:"v"`
	Node string `json:"node"`
}

// TurnClosed ends a turn.
type TurnClosed struct {
	V            int            `json:"v"`
	State        wire.TurnState `json:"state"`
	Steps        int            `json:"steps,omitempty"`
	InputTokens  int64          `json:"input_tokens,omitempty"`
	OutputTokens int64          `json:"output_tokens,omitempty"`
	CacheRead    int64          `json:"cache_read,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
}

// MessageRecorded is one message the person sent or was shown.
type MessageRecorded struct {
	V               int              `json:"v"`
	Message         string           `json:"message"`
	Role            wire.MessageRole `json:"role"`
	TextSealed      string           `json:"text_sealed,omitempty"`
	PageTitleSealed string           `json:"page_title_sealed,omitempty"`
	PageScreen      string           `json:"page_screen,omitempty"`
	// Apps are the apps the turn had read through when the message was
	// written, as a card's are.
	Apps []string `json:"apps,omitempty"`
	// AttachmentsSealed is the files a user message was sent with: a JSON
	// array of wire FileRefs, sealed.
	AttachmentsSealed string `json:"attachments_sealed,omitempty"`
}

// StepRecorded is one tool step at its latest state: its place, its tool, its
// state and its refusal's code in the clear, and the whole wire StepEvent
// sealed.
type StepRecorded struct {
	V           int            `json:"v"`
	Step        string         `json:"step"`
	Index       int            `json:"index"`
	Tool        string         `json:"tool"`
	State       wire.StepState `json:"state"`
	RefusalCode string         `json:"refusal_code,omitempty"`
	BodySealed  string         `json:"body_sealed"`
}

// CardRecorded is one card, its body the whole wire Card sealed.
type CardRecorded struct {
	V          int           `json:"v"`
	Card       string        `json:"card"`
	Message    string        `json:"message"`
	Type       wire.CardKind `json:"type"`
	Apps       []string      `json:"apps,omitempty"`
	BodySealed string        `json:"body_sealed"`
}

// ProposalCreated makes a proposal in the conversation its subject leads with.
type ProposalCreated struct {
	V int `json:"v"`

	Turn  string `json:"turn"`
	Owner string `json:"owner"`

	Tool   string             `json:"tool"`
	Act    string             `json:"act,omitempty"`
	Scope  wire.ProposalScope `json:"scope"`
	Method string             `json:"method"`
	Path   string             `json:"path"`

	QuerySealed string `json:"query_sealed,omitempty"`
	BodySealed  string `json:"body_sealed,omitempty"`

	RequestDigest  string    `json:"request_digest"`
	Digest         string    `json:"digest"`
	OpKey          string    `json:"op_key"`
	SourceRevision string    `json:"source_revision,omitempty"`
	StepUp         bool      `json:"step_up,omitempty"`
	CardSealed     string    `json:"card_sealed"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// ProposalMoved is every proposal move: the state it was decided from, the
// state it moves to and, for a settlement, what the executed request answered.
type ProposalMoved struct {
	V    int                `json:"v"`
	From wire.ProposalState `json:"from"`
	To   wire.ProposalState `json:"to"`

	Outcome       string `json:"outcome,omitempty"`
	Position      string `json:"position,omitempty"`
	ReceiptSealed string `json:"receipt_sealed,omitempty"`
	RefusalCode   string `json:"refusal_code,omitempty"`
}

// OwnerErased removes everything one person owns here. PINNED AT
// [GateRecordVersion] FOR EVER and a gate, so its shape may only grow by
// addition.
type OwnerErased struct {
	V     int    `json:"v"`
	Owner string `json:"owner"`
}

// Eviction is a node's eviction from this log, or its readmission. PINNED AT
// [GateRecordVersion] FOR EVER and a gate, so its shape may only grow by
// addition.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the INVERSE COMMIT rather than a delete, so an
	// evicted, readmitted and evicted again node reads correctly.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record: this log was recreated, and every
// position below this generation is comparable and safely stale.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// proposalMoveOps are the ops a [ProposalMoved] payload rides.
var proposalMoveOps = []OpKind{
	OpProposalApproved, OpProposalExecuting, OpProposalDeclined, OpProposalExpired,
	OpProposalStale, OpProposalApplying, OpProposalSettled, OpProposalUnknown,
	OpProposalRetried,
}

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a pair this build does
// not know is an error naming both rather than a nil payload the applier would
// read as an empty change.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindConversation && op == OpConversationCreated:
		return decodePayload[ConversationCreated](rec)
	case k == KindConversation && op == OpConversationTitled:
		return decodePayload[ConversationTitled](rec)
	case k == KindConversation && op == OpConversationPurged:
		return decodePayload[ConversationPurged](rec)
	case k == KindConversation && op == OpConversationAuditRead:
		return decodePayload[AuditReadRecorded](rec)
	case k == KindConversation && op == OpFileStored:
		return decodePayload[FileRecorded](rec)
	case k == KindConversation && op == OpTurnOpened:
		return decodePayload[TurnOpened](rec)
	case k == KindTurn && op == OpTurnParked:
		return decodePayload[TurnParked](rec)
	case k == KindTurn && op == OpTurnResumed:
		return decodePayload[TurnResumed](rec)
	case k == KindTurn && op == OpTurnClosed:
		return decodePayload[TurnClosed](rec)
	case k == KindTurn && op == OpMessageRecorded:
		return decodePayload[MessageRecorded](rec)
	case k == KindTurn && op == OpStepRecorded:
		return decodePayload[StepRecorded](rec)
	case k == KindTurn && op == OpCardRecorded:
		return decodePayload[CardRecorded](rec)
	case k == KindProposal && op == OpProposalCreated:
		return decodePayload[ProposalCreated](rec)
	case k == KindProposal && slices.Contains(proposalMoveOps, op):
		return decodePayload[ProposalMoved](rec)
	case k == KindOwner && op == OpOwnerErased:
		return decodePayload[OwnerErased](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// THE TWO PAYLOAD-FREE RECORDS, and nil is their value rather than
		// an absence: the applier has a case for each that writes nothing
		// of this domain's.
		return nil, nil
	}
	return nil, fmt.Errorf("assistantdomain: no payload shape for (%s, %s): a "+
		"record's kind and op together name its payload, and a pair this build "+
		"does not know is a newer peer's record rather than an empty one", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("assistantdomain: the record on %s carries no "+
			"payload, and (%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("assistantdomain: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append: the framework
// owns when a barrier goes out, the domain owns what a record on its log looks
// like.
//
// AN OP ID IS REFUSED: it would become the broker's message id, and a repeat
// inside the duplicate window is answered from the dedupe cache with no quorum
// round trip, which is the one claim a barrier must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("assistantdomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("assistantdomain: a barrier carries no op id and " +
			"this one has one: an op id becomes a message id, and a duplicate " +
			"acknowledgement is served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record ([statelog.TrimmedEnvelope],
// ADR-0073) as a record this domain's applier decodes: the framework owns when
// one is appended, what it removes and the floor its subject names, and the
// domain owns what a record on its log looks like.
//
// THE WRITER TRAVELS, unlike a barrier's: a trimmed record removes rows, so the
// eviction gate must be able to drop an evicted node's. NO OP ID, asserted: one
// becomes a message id and a ledger row nothing resolves. And the floor is
// CHECKED here, at the write, rather than in [Subject.Validate], which the read
// path runs too: a refusal there would turn one malformed record into an
// applier that stops on every node, where the framework's apply faults instead.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("assistantdomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("assistantdomain: a trimmed record carries no op id and " +
			"this one has one: an op id becomes a message id and a ledger row nothing " +
			"resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED at the base version for ever: an older node retains
			// a record it cannot read, and with it every anchor the
			// record names.
			V:       baseRecordVersion,
			Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
