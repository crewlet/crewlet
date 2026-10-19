package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/caveat"
)

// ErrInvalid is wrapped by every refusal of a value this package validates.
var ErrInvalid = errors.New("wire: invalid")

// Frame is one event on a turn's live stream: a head every frame carries, and
// exactly one payload, the one its [EventKind] names.
type Frame struct {
	// V is [ProtocolVersion].
	V int `json:"v"`
	// ConversationID and TurnID name what the frame belongs to.
	ConversationID uuid.UUID `json:"conversation_id"`
	TurnID         uuid.UUID `json:"turn_id"`
	// Seq starts at 1 and grows by one per frame within the turn, across
	// its segments: the frames after an approval continue the numbering
	// of the frames before it.
	Seq uint64 `json:"seq"`
	// At is when the running node wrote the frame, in UTC.
	At   time.Time `json:"at"`
	Kind EventKind `json:"kind"`

	Conversation *ConversationEvent `json:"conversation,omitempty"`
	PageContext  *PageContextEvent  `json:"page_context,omitempty"`
	Status       *StatusEvent       `json:"status,omitempty"`
	Delta        *DeltaEvent        `json:"delta,omitempty"`
	Step         *StepEvent         `json:"step,omitempty"`
	Card         *CardEvent         `json:"card,omitempty"`
	Action       *ActionEvent       `json:"action,omitempty"`
	Done         *DoneEvent         `json:"done,omitempty"`
	Error        *ErrorEvent        `json:"error,omitempty"`
	Snapshot     *SnapshotEvent     `json:"snapshot,omitempty"`
}

// ConversationEvent opens every stream.
type ConversationEvent struct {
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	// Resumed is true on a stream a client re-attached to with
	// Last-Event-ID.
	Resumed bool `json:"resumed"`
}

// PageContextEvent says what the turn was told about the screen. Used is
// false when the person removed the page chip (the turn was sent with a null
// page context); Trimmed is true when the server dropped keys or resources
// past their bounds.
type PageContextEvent struct {
	Used    bool `json:"used"`
	Trimmed bool `json:"trimmed"`
	// Page is the chip the message carried, absent when Used is false.
	Page *PageChip `json:"page,omitempty"`
}

// PageChip is how a message shows the page it was sent with: the label the
// frame drew ("Worklet › Tasks") and the screen it names.
type PageChip struct {
	Title  string `json:"title"`
	Screen string `json:"screen"`
}

// StatusEvent moves the run chip.
type StatusEvent struct {
	State StatusState `json:"state"`
	// StepsDone counts the steps that have finished in this turn.
	StepsDone int `json:"steps_done"`
}

// DeltaEvent appends text to one assistant message. Restart is a provider
// failover mid-message: the client discards what this message drew so far,
// and the text that follows replaces it.
type DeltaEvent struct {
	MessageID uuid.UUID `json:"message_id"`
	Text      string    `json:"text"`
	Restart   bool      `json:"restart,omitempty"`
}

// StepEvent is one tool step, sent when it starts and again when it ends,
// under one StepID.
type StepEvent struct {
	StepID uuid.UUID `json:"step_id"`
	// TurnID names the turn the step belongs to. It is set on every step a
	// Transcript and a SnapshotEvent carry, because a stored step is read
	// apart from the frame that brought it and the client groups a reloaded
	// conversation's steps under their turns by it; a live step frame may
	// leave it out, its head already naming the turn. Where it is set it is
	// the frame's own turn.
	TurnID uuid.UUID `json:"turn_id,omitzero"`
	// Index is the step's place in the turn, from 1.
	Index int `json:"index"`
	// Tool is the catalogue tool's model-facing name.
	Tool string `json:"tool"`
	// Label is the engine's sentence for the step, rendered from the
	// call's safe fields ("Read this week's spend").
	Label string `json:"label"`
	// Request is the ordinary request the step made, shown in mono so the
	// person can see it was one; absent for a step that sent nothing (a
	// write that became a proposal, a refusal before sending).
	Request   *StepRequest `json:"request,omitempty"`
	State     StepState    `json:"state"`
	StartedAt time.Time    `json:"started_at"`
	// DurationMS is the step's own clock, set when it ends.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Summary is the outcome in a few words ("6 rows", "Not permitted").
	Summary string   `json:"summary,omitempty"`
	Refusal *Refusal `json:"refusal,omitempty"`
	// --- deep work (im-deep) ---
	// Parent is the index of the deep-work step this step was made from,
	// absent for a step of the turn's own loop.
	Parent int `json:"parent,omitempty"`
	// Deep is the run a deep-work step is, re-sent as it progresses.
	Deep *DeepProgress `json:"deep,omitempty"`
}

// StepRequest is a step's request: the method and the path it was sent to,
// wildcards filled in (`/query/knowledge`, `/work/ENG-42`), never the query
// values or the body.
type StepRequest struct {
	Method string `json:"method"`
	Route  string `json:"route"`
}

// Refusal is a refusal in the engine's own words: the API's code, its
// sentence, what to do about it where the route said, and, where the
// authority table refused, the rule that decided and the grants that would
// have admitted the caller.
//
// TWO READERS, TWO CHANNELS. Message and Hint are what a person reads, and
// Detail is the route's own account of this one refusal (the envelope's
// `detail`), written for whoever corrects the request: the model. Detail
// names tools, fields and config keys a person never sees, so it is never
// serialised: it rides in memory from the answer to the model's next message
// ([Refusal.ForModel]) and goes no further, neither to the drawer nor to the
// stored transcript.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Hint is the next step, shown on its own line.
	Hint string `json:"hint,omitempty"`
	// Reason is the deciding rule's own word (the envelope's `reason`, an
	// authz.Reason) where the authority table refused, and empty for any
	// other refusal. THE GRANTS ALONE CANNOT SAY WHETHER ACCESS IS THE WAY
	// PAST: a stale proof names the grants that admitted the caller. So the
	// sentence a person reads follows the rule rather than the grants, and
	// the rule travels with them: they are a way past it only where it says
	// so.
	Reason string   `json:"reason,omitempty"`
	Grants []string `json:"grants,omitempty"`
	// Detail is the route's own account of the refusal, for the model only.
	Detail string `json:"-"`
}

// ForModel is the refusal as the model reads it: the route's own account where
// it gave one, which names the field and the value to correct, else the
// sentence.
func (r Refusal) ForModel() string {
	if r.Detail != "" {
		return r.Detail
	}
	return r.Message
}

// CardEvent carries one card, attached to the assistant message it belongs
// to.
type CardEvent struct {
	CardID    uuid.UUID `json:"card_id"`
	MessageID uuid.UUID `json:"message_id"`
	Card      Card      `json:"card"`
}

// ActionEvent is an instruction for the page. A page action carries the
// proposal it applies, the page action's own name, the revision it was
// prepared against and its payload; the reserved [ActionContextInvalidate]
// carries Invalidate instead.
type ActionEvent struct {
	ActionID uuid.UUID `json:"action_id"`
	Name     string    `json:"name"`
	// ProposalID is the approval this action applies; for a page action
	// whose screen declares it `confirm: none`, the proposal recorded for
	// it all the same, so every page change has one record.
	ProposalID     *uuid.UUID      `json:"proposal_id,omitempty"`
	SourceRevision string          `json:"source_revision,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Confirm        ActionConfirm   `json:"confirm,omitempty"`
	Invalidate     *Invalidate     `json:"invalidate,omitempty"`
}

// Invalidate tells open screens a write landed: at Position on the log of the
// state-log domain named Domain (`tracker`, never its stream's name), so the
// next read they make includes it. Both are empty for a write whose answer
// named no position; the screens read again all the same.
type Invalidate struct {
	Domain   string `json:"domain"`
	Position string `json:"position"`
}

// DoneEvent ends a turn segment.
type DoneEvent struct {
	State      TurnState `json:"state"`
	DurationMS int64     `json:"duration_ms"`
	Steps      int       `json:"steps"`
	// MessageID is the answer's message, absent for a segment that wrote
	// none.
	MessageID *uuid.UUID `json:"message_id,omitempty"`
	// TraceID is the turn's trace, for support to open; never a token
	// count.
	TraceID string `json:"trace_id,omitempty"`
}

// ErrorEvent is a turn-level failure the person reads.
type ErrorEvent struct {
	Code              ErrorCode `json:"code"`
	Message           string    `json:"message"`
	Hint              string    `json:"hint,omitempty"`
	Retryable         bool      `json:"retryable"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	// Remedy names the setting a person changes to clear an error a
	// configurable limit caused (a token budget spent), absent for every
	// other (ADR-0060).
	Remedy *caveat.Remedy `json:"remedy,omitempty"`
}

// SnapshotEvent replaces what a resuming client drew of this turn: its
// messages, steps and cards as the transcript holds them.
type SnapshotEvent struct {
	Messages []Message   `json:"messages"`
	Steps    []StepEvent `json:"steps"`
	Cards    []CardEvent `json:"cards"`
}

// payloads names each kind's payload as set or not, in [EventKinds] order.
func (f Frame) payloads() map[EventKind]bool {
	return map[EventKind]bool{
		EventConversation: f.Conversation != nil,
		EventPageContext:  f.PageContext != nil,
		EventStatus:       f.Status != nil,
		EventDelta:        f.Delta != nil,
		EventStep:         f.Step != nil,
		EventCard:         f.Card != nil,
		EventAction:       f.Action != nil,
		EventDone:         f.Done != nil,
		EventError:        f.Error != nil,
		EventSnapshot:     f.Snapshot != nil,
	}
}

// Validate reports what is wrong with a frame, naming the field: the head,
// exactly one payload and that it is the kind's, and every closed set inside
// it.
func (f Frame) Validate() error {
	switch {
	case f.V != ProtocolVersion:
		return fmt.Errorf("%w: frame v %d, this build speaks %d", ErrInvalid, f.V, ProtocolVersion)
	case f.ConversationID == uuid.Nil:
		return fmt.Errorf("%w: frame names no conversation_id", ErrInvalid)
	case f.TurnID == uuid.Nil:
		return fmt.Errorf("%w: frame names no turn_id", ErrInvalid)
	case f.Seq == 0:
		return fmt.Errorf("%w: frame seq is 0; a turn's frames count from 1", ErrInvalid)
	case f.At.IsZero():
		return fmt.Errorf("%w: frame at is the zero instant", ErrInvalid)
	case !f.Kind.Valid():
		return fmt.Errorf("%w: frame kind %q is not one of %v", ErrInvalid, f.Kind, EventKinds)
	}
	for kind, set := range f.payloads() {
		if set != (kind == f.Kind) {
			if set {
				return fmt.Errorf("%w: a %s frame carries a %s payload", ErrInvalid, f.Kind, kind)
			}
			return fmt.Errorf("%w: a %s frame carries no %s payload", ErrInvalid, f.Kind, kind)
		}
	}
	return f.validatePayload()
}

func (f Frame) validatePayload() error {
	switch f.Kind {
	case EventStatus:
		if !f.Status.State.Valid() {
			return fmt.Errorf("%w: status state %q is not one of %v", ErrInvalid,
				f.Status.State, StatusStates)
		}
	case EventDelta:
		if f.Delta.MessageID == uuid.Nil {
			return fmt.Errorf("%w: delta names no message_id", ErrInvalid)
		}
	case EventStep:
		if err := f.Step.Validate(); err != nil {
			return err
		}
		return f.Step.inTurn(f.TurnID)
	case EventCard:
		return f.Card.Validate()
	case EventAction:
		return f.Action.Validate()
	case EventDone:
		if !f.Done.State.Ended() {
			return fmt.Errorf("%w: done state %q does not end a segment", ErrInvalid, f.Done.State)
		}
	case EventError:
		if !f.Error.Code.Valid() {
			return fmt.Errorf("%w: error code %q is not one of %v", ErrInvalid,
				f.Error.Code, ErrorCodes)
		}
	case EventSnapshot:
		for _, s := range f.Snapshot.Steps {
			if err := s.Validate(); err != nil {
				return err
			}
			if err := s.inTurn(f.TurnID); err != nil {
				return err
			}
		}
		for _, c := range f.Snapshot.Cards {
			if err := c.Validate(); err != nil {
				return err
			}
		}
		for _, m := range f.Snapshot.Messages {
			if !m.Role.Valid() {
				return fmt.Errorf("%w: message role %q is not one of %v", ErrInvalid,
					m.Role, MessageRoles)
			}
		}
	}
	return nil
}

// Validate reports what is wrong with a step.
func (s StepEvent) Validate() error {
	switch {
	case s.StepID == uuid.Nil:
		return fmt.Errorf("%w: step names no step_id", ErrInvalid)
	case s.Index < 1:
		return fmt.Errorf("%w: step index %d; steps count from 1", ErrInvalid, s.Index)
	case s.Tool == "" || s.Label == "":
		return fmt.Errorf("%w: step %d names no tool or no label", ErrInvalid, s.Index)
	case !s.State.Valid():
		return fmt.Errorf("%w: step state %q is not one of %v", ErrInvalid, s.State, StepStates)
	case s.State == StepRefused && s.Refusal == nil:
		return fmt.Errorf("%w: a refused step carries no refusal", ErrInvalid)
	case s.Parent < 0 || s.Parent >= s.Index && s.Parent != 0:
		return fmt.Errorf("%w: step %d nests under step %d, which is not before it", ErrInvalid,
			s.Index, s.Parent)
	case s.Deep != nil:
		return s.Deep.validate()
	}
	return nil
}

// inTurn reports a step that names a turn other than the frame carrying it.
func (s StepEvent) inTurn(turn uuid.UUID) error {
	if s.TurnID != uuid.Nil && s.TurnID != turn {
		return fmt.Errorf("%w: step %d names turn %s inside a frame of turn %s", ErrInvalid,
			s.Index, s.TurnID, turn)
	}
	return nil
}

// Validate reports what is wrong with a carried card.
func (c CardEvent) Validate() error {
	if c.CardID == uuid.Nil || c.MessageID == uuid.Nil {
		return fmt.Errorf("%w: a card names no card_id or no message_id", ErrInvalid)
	}
	return c.Card.Validate()
}

// Validate reports what is wrong with an action.
func (a ActionEvent) Validate() error {
	if a.ActionID == uuid.Nil || a.Name == "" {
		return fmt.Errorf("%w: an action names no action_id or no name", ErrInvalid)
	}
	if a.Name == ActionContextInvalidate {
		if a.Invalidate == nil || a.ProposalID != nil || a.Payload != nil {
			return fmt.Errorf("%w: %s carries invalidate and nothing a page action carries",
				ErrInvalid, ActionContextInvalidate)
		}
		return nil
	}
	switch {
	case a.Invalidate != nil:
		return fmt.Errorf("%w: page action %s carries invalidate", ErrInvalid, a.Name)
	case a.ProposalID == nil:
		return fmt.Errorf("%w: page action %s names no proposal_id", ErrInvalid, a.Name)
	case !a.Confirm.Valid():
		return fmt.Errorf("%w: page action %s confirm %q is not one of %v", ErrInvalid,
			a.Name, a.Confirm, ActionConfirms)
	}
	return nil
}
