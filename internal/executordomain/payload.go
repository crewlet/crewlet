package executordomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The typed payloads, one per (kind, op). A record carries its complete new
// values, all decided by the writer, never a rule for computing them, so a
// replay from zero reproduces the rows and the applier stays a pure function
// of the record and the rows. IDs travel as text: an id a decoder refused
// would stall the log rather than be dropped deterministically by the applier.

// Capture names a new object for a live turn's session, every field read by
// the writer off the run it captured, so the next launch can restore the
// session, check its archive layout and map its paths onto its own box.
type Capture struct {
	V int `json:"v"`

	// Handle is the seat's handle and Turn the turn's (run's) id: the row's
	// key. ConversationKey is the conversation's key, empty for a turn that
	// had none, and WorkKey the unit of work the turn was dispatched for.
	Handle          string `json:"handle"`
	Turn            string `json:"turn"`
	ConversationKey string `json:"conversation_key,omitempty"`
	WorkKey         string `json:"work_key,omitempty"`

	// Harness is the harness that wrote the session (`claude-code`,
	// `opencode`), HarnessVersion the version it reported, and SessionID its
	// own id for the session.
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version,omitempty"`
	SessionID      string `json:"session_id"`

	// Entry is the providers.llm entry the session ran under, as an identity
	// naming no credential (internal/providers/llm/harness's
	// Provider.EntryID): a launch under another entry continues it only on a
	// harness measured to send a resumed session's calls on its launch's
	// route (internal/sandbox's EntryContinues). "" where the capture's
	// launch recorded none. Record version 6 ([entryVersion]).
	Entry string `json:"entry,omitempty"`

	// Object, ObjectHash and ObjectSize name the sealed object the session
	// was uploaded into before this record was decided; ContentKey is the
	// keyring key it is sealed under (what a rotation counts); ArchiveFormat
	// is the archive's layout version, checked before anything is unpacked.
	Object        string `json:"object"`
	ObjectHash    string `json:"object_hash"`
	ObjectSize    int64  `json:"object_size"`
	ContentKey    string `json:"content_key"`
	ArchiveFormat int    `json:"archive_format"`

	// Media is every media object the session's frames name, the turn's
	// whole set, which replaces the one its row named: none for a session
	// kept whole, which keeps the record readable by an older build. Record
	// version 9 ([mediaVersion]).
	Media []CaptureMedia `json:"media,omitempty"`

	// LaunchSeq and CaptureSeq are the capture's ordinals (its launch, and
	// its place among that launch's captures). A pair not newer than the
	// row's writes nothing, so a late capture never rolls a session back.
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

	// Home and Workspace are the writer box's absolute paths, which the next
	// launch's brief maps onto its own.
	Home      string `json:"home,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	// ContextTokens is the whole input, cache included, of the last request
	// the session's harness sent its model as its transcript recorded it
	// (zero where none): what a continuing launch resends first, so its
	// budget floor. Interrupted says the harness was cut off, so the
	// transcript may end mid-call: a continuing launch restores it over any
	// copy its box holds and says so in its brief.
	//
	// They outlive the run record, which goes with the turn. Both are
	// omitted at their zero, which keeps a capture recording neither
	// readable by an older build ([versionedFields]).
	ContextTokens int64 `json:"context_tokens,omitempty"`
	Interrupted   bool  `json:"interrupted,omitempty"`

	// Trimmed says the session's pack cut it, past the bound, to what its
	// harness's resume loads: the history before its newest compaction is not
	// in the object, which every reader of the row says. Stamped
	// [trimmedVersion].
	Trimmed bool `json:"trimmed,omitempty"`

	// ReadThrough is the packed log position the turn's first launch read
	// its conversation's head at (zero where none), and Absorbs the turns
	// that read handed out ([Head.Absorbs]): exactly what the turn's `end`
	// absorbs. A turn the read left out was never shown to this one, so it
	// stays for the next. Both are kept from the first capture carrying a
	// read, because later launches read the turn's own row, not the head.
	ReadThrough int64    `json:"read_through,omitempty"`
	Absorbs     []string `json:"absorbs,omitempty"`

	// SeenRemoval is the position of the seat's last removal this capture's
	// decide saw ([SeatDelete]), zero where none. A capture landing after a
	// removal it did not see writes nothing: a turn with no row at the
	// removal has no tombstone to refuse it.
	SeenRemoval int64 `json:"seen_removal,omitempty"`

	// SeatBudget is the most object bytes the seat may hold once this
	// capture lands: the company's `max_bytes_per_seat`, carried here so
	// every node applies the one number the writer read.
	SeatBudget int64 `json:"seat_budget"`
}

// CaptureMedia is one media object a capture names: the digest its session's
// frames name it by, and the sealed object holding the member. A FRESH one
// was uploaded for this capture; a CARRIED one is an object the turn's own
// row, or the base that row holds, already names, which the apply finds there
// live or refuses the capture (ADR-0027's amendment "a session's media,
// carried").
type CaptureMedia struct {
	Digest     string `json:"digest"`
	Object     string `json:"object"`
	ObjectHash string `json:"object_hash"`
	ObjectSize int64  `json:"object_size"`
	ContentKey string `json:"content_key"`
	Carried    bool   `json:"carried,omitempty"`
}

// Begin records a turn's first read of its conversation and the base it froze:
// the session the turn's launches continue until it keeps its own. The first
// launch publishes it once, under [BeginOpID], as soon as the run record that
// froze the read exists (internal/sandbox's Coordinator.recordBegin).
//
// The base is held by the turn's own row, never a record about the base: only
// a row is weighed by every release in log order. While the row names the
// base, is live and has no capture, no release takes it ([held]).
type Begin struct {
	V int `json:"v"`

	// Handle, Turn, ConversationKey and WorkKey are the turn's. A begin
	// always has a conversation: a turn with none read no head.
	Handle          string `json:"handle"`
	Turn            string `json:"turn"`
	ConversationKey string `json:"conversation_key"`
	WorkKey         string `json:"work_key,omitempty"`

	// ReadThrough is the packed position the head was read at ([Head.Position]),
	// and Absorbs the turns that read handed out that the turn was told of:
	// what its end absorbs once it has landed a capture ([Capture.ReadThrough]).
	ReadThrough int64    `json:"read_through"`
	Absorbs     []string `json:"absorbs,omitempty"`

	// BaseTurn is the head the read named (empty where none), and BaseObject
	// and BaseHash the session its row named then. The apply holds the base
	// only while that row still names exactly that session; otherwise the
	// row is written holding nothing.
	BaseTurn   string `json:"base_turn,omitempty"`
	BaseObject string `json:"base_object,omitempty"`
	BaseHash   string `json:"base_hash,omitempty"`

	// SeenRemoval is [Capture.SeenRemoval] for the begin: one decided before a
	// removal of its seat it did not see writes nothing once it lands after.
	SeenRemoval int64 `json:"seen_removal,omitempty"`
}

// End records how a turn ended, and is terminal.
type End struct {
	V int `json:"v"`

	Handle          string `json:"handle"`
	Turn            string `json:"turn"`
	ConversationKey string `json:"conversation_key,omitempty"`
	WorkKey         string `json:"work_key,omitempty"`

	// AskExcerpt is what the turn was asked, redacted and then cut to
	// [AskExcerptMaxBytes] (executorkeep.Excerpt).
	AskExcerpt string `json:"ask_excerpt,omitempty"`

	Ending executorkeep.Ending `json:"ending"`

	// Served is every OTHER conversation the turn served, by key: each whose
	// wake its run read (ADR-0167), at most [MaxServed], none twice, never the
	// turn's own. Each then reads the turn's row as a turn of its own that
	// read it as this end applied (package doc, "A turn serves every
	// conversation its run read"). Record version 5 ([servedVersion]).
	Served []string `json:"served,omitempty"`

	// SeenRemoval is [Capture.SeenRemoval] for the end, except that the end
	// still applies to a row the removal did not take (one a capture of the
	// seat's next tenure wrote since).
	SeenRemoval int64 `json:"seen_removal,omitempty"`
}

// Delete removes one turn's row and tombstone past the retention, on the
// conversation's subject. It applies only if the row's last instant (end, else
// newest capture, else begin, else tombstone) is at or before Cutoff, so a turn
// captured since the duty nominated it stays.
//
// On a seat's subject (no turn, no conversation) it retires the seat's removal
// record ([SeatDelete]) once past the cutoff, when no record decided before
// the removal can still land.
type Delete struct {
	V int `json:"v"`

	Handle          string    `json:"handle"`
	Turn            string    `json:"turn"`
	ConversationKey string    `json:"conversation_key,omitempty"`
	Cutoff          time.Time `json:"cutoff"`
}

// SeatDelete removes every row a seat holds here, after the company removed
// the seat, tombstones the turns that had not ended, and records the position
// it applied at, which every later capture or end of the seat is held to
// ([Capture.SeenRemoval]).
type SeatDelete struct {
	V      int    `json:"v"`
	Handle string `json:"handle"`
}

// Eviction is a node's eviction from this log, or its readmission. Pinned at
// [GateRecordVersion] for ever as a gate, so its shape only grows by addition.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the inverse commit rather than a delete, so a
	// node evicted, readmitted and evicted again reads correctly.
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

// DecodeMutation reads the typed payload for one record. It dispatches on
// (kind, op) alone, so an unknown pair is an error naming both, not a nil
// payload the applier would read as an empty change.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindConversation && op == OpBegin:
		return decodePayload[Begin](rec)
	case k == KindConversation && op == OpCapture:
		return decodePayload[Capture](rec)
	case k == KindConversation && op == OpEnd:
		return decodePayload[End](rec)
	case k == KindConversation && op == OpDelete:
		return decodePayload[Delete](rec)
	case k == KindSeat && op == OpSeatDelete:
		return decodePayload[SeatDelete](rec)
	case k == KindSeat && op == OpDelete:
		return decodePayload[Delete](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// The two payload-free records: nil is their value, not an
		// absence; the applier's case for each writes nothing here.
		return nil, nil
	}
	return nil, fmt.Errorf("executordomain: no payload shape for (%s, %s): a "+
		"record's kind and op together name its payload, and a pair this build "+
		"does not know is a newer peer's record rather than an empty one", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("executordomain: the record on %s carries no "+
			"payload, and (%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("executordomain: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append (the framework
// owns when, the domain owns the record's shape). An op id is refused: it
// would become the broker's message id, and a repeat in the duplicate window
// is answered from the dedupe cache with no quorum round trip, the one claim
// a barrier must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("executordomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("executordomain: a barrier carries no op id and " +
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
// ADR-0080) as a record this domain's applier decodes (the framework owns when,
// what it removes and its floor; the domain owns the record's shape).
//
// The writer travels, unlike a barrier's: the record removes rows, so the
// eviction gate must be able to drop an evicted node's. No op id: it would
// become a message id and a ledger row nothing resolves. The floor is checked
// here at the write, not in [Subject.Validate], which the read path runs too:
// a refusal there would stop the applier on every node.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("executordomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("executordomain: a trimmed record carries no op id and " +
			"this one has one: an op id becomes a message id and a ledger row nothing " +
			"resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("executordomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// Pinned at the base version for ever: an older node retains
			// a record it cannot read, and every anchor it names.
			V:       baseRecordVersion,
			Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
