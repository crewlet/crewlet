package executordomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// A record carries its COMPLETE new values, never a rule for computing them:
// the object, its digest and size, the ordinals, the budget, the cutoff are all
// decided by the writer. That is what makes a replay from zero produce the rows
// the original apply did, and it is what lets the applier stay a pure function
// of the record and the rows.
//
// IDS TRAVEL AS TEXT: a payload is decoded on every node, and an id refused by
// a decoder would be a record that stalls the log rather than one the applier
// drops deterministically.

// Capture names a new object for a live turn's session. Every field is the
// writer's, read off the run it captured; the row keeps them so the next
// launch can restore the session, check its archive layout and map the paths
// it was written under onto its own box.
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

	// Object, ObjectHash and ObjectSize name the sealed object the session
	// was uploaded into before this record was decided; ContentKey is the
	// keyring key it is sealed under, what a rotation counts; and
	// ArchiveFormat is the layout version of the archive inside it, which a
	// restorer checks before it unpacks anything.
	Object        string `json:"object"`
	ObjectHash    string `json:"object_hash"`
	ObjectSize    int64  `json:"object_size"`
	ContentKey    string `json:"content_key"`
	ArchiveFormat int    `json:"archive_format"`

	// LaunchSeq and CaptureSeq are the capture's ordinals: the turn's launch
	// it was taken from and its place among that launch's captures. A
	// capture whose pair is not newer than the row's writes nothing, so a
	// late capture never rolls a session back.
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

	// Home and Workspace are the writer box's absolute paths, which the next
	// launch's brief maps onto its own.
	Home      string `json:"home,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	// ReadThrough is the packed log position the turn's first launch read
	// its conversation's head at, zero where it read none, and Absorbs the
	// turns that read handed out in full ([Head.Absorbs]): the head it
	// continued, the endings it was told and the siblings it was offered.
	// They are what the turn's `end` absorbs, and nothing else: a turn the
	// read left out (past [MaxEndings], or ended after it) was never shown
	// to this one, so its ending and its session stay for the next turn to
	// be shown. Both are kept from the first capture that carries a read,
	// because a turn's later launches read its own row, not the head.
	ReadThrough int64    `json:"read_through,omitempty"`
	Absorbs     []string `json:"absorbs,omitempty"`

	// SeenRemoval is the position of the seat's last removal this capture's
	// decide saw ([SeatDelete]), zero where it saw none. A capture decided
	// before a removal it did not see, and landing after it, writes nothing:
	// it is a turn of the seat the company removed, and a turn that had no
	// row when the seat was removed has no tombstone to refuse it.
	SeenRemoval int64 `json:"seen_removal,omitempty"`

	// SeatBudget is the most object bytes the seat may hold once this
	// capture lands: the company's `max_bytes_per_seat`, read by the writer
	// off the fleet's revision and carried here so every node applies the
	// one number.
	SeatBudget int64 `json:"seat_budget"`
}

// End records how a turn ended, and is terminal.
type End struct {
	V int `json:"v"`

	Handle          string `json:"handle"`
	Turn            string `json:"turn"`
	ConversationKey string `json:"conversation_key,omitempty"`
	WorkKey         string `json:"work_key,omitempty"`

	// AskExcerpt is what the turn was asked, cut to [AskExcerptMaxBytes].
	AskExcerpt string `json:"ask_excerpt,omitempty"`

	Ending Ending `json:"ending"`

	// SeenRemoval is [Capture.SeenRemoval] for the end: an end decided
	// before a removal of its seat it did not see writes nothing once it
	// lands after it, unless the turn has a row the removal did not take
	// (one a capture of the seat's next tenure wrote since).
	SeenRemoval int64 `json:"seen_removal,omitempty"`
}

// Delete removes one turn's row and tombstone past the retention, on the
// turn's conversation's subject. Cutoff is the instant the row's own last
// instant (its end's, else its newest capture's, else its tombstone's) must be
// at or before for the delete to apply, so a turn captured again since the
// duty nominated it stays.
//
// ON A SEAT'S SUBJECT, with no turn and no conversation, it is the retention
// of the seat's removal ([SeatDelete]) instead: the record of where the seat's
// rows were last removed goes once that removal is past the cutoff, by which
// time no record decided before it can still land.
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

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a pair this build does
// not know is an error naming both rather than a nil payload the applier would
// read as an empty change.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
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
	case k == KindBarrier && op == OpBarrier:
		// THE ONE PAYLOAD-FREE RECORD, and nil is its value rather than an
		// absence: the applier has a case for it that writes nothing.
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

// EncodeBarrier renders the read index's payload-free append: the framework
// owns when a barrier goes out, the domain owns what a record on its log looks
// like.
//
// AN OP ID IS REFUSED: it would become the broker's message id, and a repeat
// inside the duplicate window is answered from the dedupe cache with no quorum
// round trip, which is the one claim a barrier must never fake.
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
