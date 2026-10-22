// Package execstate is the wire format for a suspended Execute loop.
//
// An Execute phase that launches a detached sandbox run stops mid-loop with
// the run_sandbox call UNANSWERED, and is re-entered later — after a restart,
// possibly on another node, possibly days later once a person answers a
// question. The conversation cannot be a parked goroutine: the run outlives
// the process. So it is serialized into the pending-run row and re-entered
// from there.
//
// THIS IS A WIRE FORMAT, not an implementation detail, because it crosses two
// boundaries:
//
//   - Between subsystems. The agent layer writes it; the sandbox coordinator
//     carries it back. (The coordinator never DECODES it — it holds an opaque
//     blob — which is what keeps the sandbox layer free of agent imports.)
//   - Between BUILDS. A rolling upgrade means the node that resumes a run is
//     routinely not the build that suspended it.
//
// So it carries an explicit Version, and decoding an unknown one is a LOUD
// REFUSAL that leaves the row untouched rather than a best-effort read: a
// resumed turn acting on a half-understood conversation is worse than a run
// that waits for a node which understands it.
//
// Evolution is additive WITHIN a version — new fields get defaults, nothing is
// removed or repurposed — and a shape that cannot be read that way takes a new
// version. A version a RELEASED build wrote then needs a reader in the build
// that replaces it, for as long as rows of it can be parked: nothing rewrites
// a parked row, and the layer that holds the blob cannot decode it, so the
// only thing that can read an old row is a build that still knows how.
//
// A version no release wrote needs none, and gets none. Nobody can be running
// a build that parked a row in it, so a reader for one would be code that
// exists only for a build that never shipped. Versions 1 and 2 are such: this
// build reads 3 alone and refuses the others as unknown.
//
// # What version 3 is
//
// The phase a CLI drove. A host-mode executor's tools are called from its
// coding CLI's own loop, over MCP, and the tool loop folds what the run did
// back into the record it always kept: an assistant message per model request
// with the calls it asked for, a tool message per call with what it was
// answered. A detached call runs alone, AFTER every other call its response
// made, so a run that parks on one leaves the conversation ending with that
// response and its siblings' answers — the parked call the one call nothing
// answered. Version 2 was a conversation that ended on the parked response
// itself, whose one call had to be the parked one — a round that called
// anything beside it could not park at all — and the record now carries the
// CLI runs beside the rounds.
package execstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/words"
)

// Version is the format this build writes, and the one it reads.
//
// Bumped only for a change a previous build could MISREAD. An added field a
// reader can default is not a bump; a changed meaning for an existing field
// is — and what a parked conversation may hold after its parked call is one
// (see the package doc).
//
// It is 3 rather than 1 because 1 and 2 named earlier shapes and a number is
// never reused: a blob carrying either is refused as a version this build does
// not read, never decoded as this one.
const Version = 3

// State is a suspended Execute loop, whole.
//
// JSON tags are the on-the-wire names and are part of the contract: renaming a
// Go field is free, renaming a tag is a format change.
type State struct {
	Version int `json:"version"`

	// Messages is the conversation as it stood: every model request of the
	// phase as an assistant message with the calls it asked for, each call
	// answered by a tool message — but the one that parked it. The last
	// assistant message is the response that made the parked call, followed
	// only by its other calls' answers. Its JSON is [llm.Message]'s own, which
	// is pinned for exactly this reader.
	Messages []llm.Message `json:"messages"`

	// PendingCallID and PendingCallName identify the dangling call the
	// resume answers.
	PendingCallID   string `json:"pending_tool_call_id"`
	PendingCallName string `json:"pending_tool_name"`

	// ActiveTools is the tool surface the suspended phase had, including
	// anything it activated mid-loop. Replayed rather than re-derived: a
	// resumed phase that rebuilt its surface from the plan would lose every
	// activation the pre-suspend rounds made.
	ActiveTools []string `json:"active_tool_names"`

	// LoadedSkills is the skill-guard state, replayed for the same reason.
	LoadedSkills []string `json:"loaded_skill_keys"`

	// Round is the turn iteration the suspend happened in, so the resumed
	// phase continues numbering rather than restarting at 1.
	//
	// TWO READERS, and they have to agree: the runner stamps the resumed
	// phase's own record with it, and the turn loop starts its round
	// counter there. The loop did not read it once, which is how one turn
	// came to publish phase records numbered from the parked round while
	// its budget was being spent from one again.
	Round int `json:"iteration"`

	// InputTokens and OutputTokens are what the pre-suspend rounds spent.
	// Carried so the resumed phase's record is the turn's total rather than
	// only its second half.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// ToolExecutions is what the pre-suspend rounds actually ran. The
	// resumed phase's ledger entry is built from these plus its own, which
	// is what stops a resumed turn re-firing a delivery that already went.
	//
	// In the phase record's own wire shape, `call_id` included: the id each
	// call carries in Messages, which is what pairs a row with the call the
	// conversation made and tells two calls of one response apart.
	ToolExecutions []types.ToolExecution `json:"tool_executions"`

	// RoundsUsed is how many tool-loop rounds ran before the suspend, and
	// RoundNarration what they thought and said — keyed on the same round
	// numbers ToolExecutions carries.
	//
	// Both are here because NOTHING ELSE HOLDS THEM. A suspended phase
	// publishes no `agent_phase_completed` — it returns before the record is
	// written — and `agent_turn_progress` is stream-only, refused by the
	// event store. So the resumed phase's record is the only durable account
	// this phase will ever have, and without these it began at round 1 with
	// the pre-suspend half, the `run_sandbox` call included, gone for good.
	// Zero and empty when nothing ran or was narrated before the suspend.
	RoundsUsed     int                    `json:"rounds_used,omitempty"`
	RoundNarration []types.RoundNarration `json:"round_narration,omitempty"`

	// Rounds is the pre-suspend provider calls, timed — and CacheRead and
	// CacheWrite the prompt cache's share of InputTokens across them — for
	// the reason RoundsUsed is here: the resumed phase's record is the only
	// durable account of this phase, and a record whose timeline began at
	// the resume would report the rounds before it as having taken no time
	// and the cache as having served nothing.
	Rounds           []types.PhaseRound `json:"rounds,omitempty"`
	CacheReadTokens  int                `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int                `json:"cache_write_tokens,omitempty"`

	// Invocations is the pre-suspend CLI runs — each one process, from
	// launch to exit, with what it spent and how it ended — for the reason
	// Rounds is here: a round is a model request a CLI's record showed, and
	// a run is what the engine started and stopped itself, on every record
	// whatever its CLI reports. Without them the resumed record would count
	// the phase's runs from the resume.
	Invocations []types.PhaseInvocation `json:"invocations,omitempty"`

	// Written is the work items the turn's writes had committed to when it
	// suspended, in first-write order, and WrittenMany whether there were
	// more than it lists: the turn's own write set (turnctx.Written), carried.
	//
	// HERE because a turn nothing at dispatch named an item for is charged
	// at its END to the one item it wrote, and a suspended turn ends in
	// another segment, often in another process. Without this the resumed
	// segment would judge "exactly one" over only what it wrote itself: a
	// turn that filed its task before launching a coding run would end
	// having written nothing, and be charged to nothing. Empty when the
	// first half wrote nothing.
	Written     []types.WorkItem `json:"written,omitempty"`
	WrittenMany bool             `json:"written_many,omitempty"`

	// Uncharged is what the turn's segments cost that no work item has been
	// charged for yet — the half before a park, when nothing at dispatch
	// named an item and the segment that parked could not conclude one.
	//
	// HERE for the reason Written is: a turn charged at its END by a sole
	// write is charged by a segment that is not the one that spent most of
	// it. Without this the resumed segment would pay only its own share,
	// and a turn that filed its task, launched a coding run and resumed
	// would be charged for the collection and not for the work — and never
	// counted as a turn at all, since only the first segment counts one.
	// When the finishing segment is charged, it pays this as well; when it
	// is not, nothing ever is, which is what an unattributed turn is.
	//
	// Nil when nothing is owed: the half before the park was charged to the
	// item its dispatch named.
	Uncharged *Uncharged `json:"uncharged,omitempty"`

	// SoFar is what the turn's segments before this one MEASURED — their
	// spans and their reviews — for the record that ENDS the turn to state
	// the whole of it ([types.TurnCompleted.Turn]).
	//
	// HERE for the reason Written is: the segment that ends a turn is often
	// in another process, and the turn's earlier records are in the event
	// log of whichever node the earlier segments ran on — or of whichever
	// data node kept their custody batch. Summed from those records, every
	// node counted the turn from the part it held. Carried on every park,
	// charged or not, since it is a measurement rather than a charge — a
	// zero one as `{}`, never omitted.
	//
	// NIL ON A RESUMED TURN MEANS ITS PAST IS UNMEASURED: the park was made
	// by a build before this field, so what the segments before it ran is
	// unknown rather than zero. The resumed turn then hands none on at a
	// park of its own and its ending record states no tally
	// ([types.TurnCompleted.Turn]), which the usage reader sums from the
	// turn's records instead.
	SoFar *TurnSoFar `json:"so_far,omitempty"`

	// ElapsedMS is how long this phase had already been running when it
	// suspended, so the resumed half reports the WHOLE phase rather than the
	// re-entry.
	//
	// Every other aggregate on this state folds across the suspend —
	// RoundsUsed, the tokens, the tool executions — and a duration that did
	// not would be the one number on `agent_phase_completed` that means
	// something different for a sandboxed phase than for every other. It
	// would also be wrong in the direction that hides the cost: a detached
	// coding run is minutes of the most expensive work a seat does, and the
	// record would report only the seconds spent collecting its answer.
	//
	// Milliseconds because that is the resolution the event carries, and it
	// is a wire field: a time.Duration would serialize as a nanosecond count
	// nothing else here speaks.
	//
	// Zero (and omitted) when the half before the suspend took under a
	// millisecond; the resume adds it to the resumed half's clock.
	ElapsedMS int `json:"elapsed_ms,omitempty"`

	// Iterations is the closed-round ledger of the suspended TURN. The
	// resume is the same turn, so without this it would forget every round
	// that closed before the suspend.
	Iterations []ledger.Iteration `json:"iteration_history"`

	// Task is the turn's task description, carried so the resumed turn
	// re-enters with the same brief rather than one rebuilt from a trigger
	// that may no longer be readable.
	Task string `json:"task_description"`

	// Ask is what the turn was ASKED — the trigger's own words, without
	// the wrapping Task carries — for the segment that finishes the turn to
	// record ([types.TurnCompleted.Ask]): it is woken by the run's
	// completion or a person's reply, not by the trigger, so without this
	// the episode of every coding turn knew it only by its collection's
	// label. Empty for a turn that was told nothing, whose episode is then
	// embedded as its label and what it did.
	Ask string `json:"ask,omitempty"`

	// AskProvenance is who wrote Ask ([types.TurnCompleted.AskProvenance]),
	// parked beside it for the same reason: the segment that records the
	// ask reads no trigger, so the label the first segment read it under is
	// one nothing else could rebuild. Nil where Ask is the company's or the
	// seat's own words, or empty.
	AskProvenance *provenance.Label `json:"ask_provenance,omitempty"`

	// Senders is who ASKED — every distinct identifiable sender of the
	// turn's interactions, in the order they first spoke — parked beside
	// the ask for the reason the ask is: the resumed segment re-reads no
	// trigger, so it has no interactions to read them off, and a
	// refresh_memory call it makes re-runs the turn-start memory filter,
	// whose per-subject rule ("a note about a person applies to a turn that
	// person is party to") is judged against exactly this list. Without it
	// every resumed segment told the filter nobody was asking, and a note
	// about one person could be applied to a turn somebody else started.
	// Empty for a turn no identifiable sender woke.
	Senders []types.CanonicalIdentity `json:"senders,omitempty"`

	// AgentRun marks a suspension whose executor IS the detached run: a
	// coding CLI in box mode drove its own loop in a sandbox box, its calls
	// over the bridge rather than through a loop of the engine's, so there
	// is no engine conversation to re-enter and no parked call to answer.
	//
	// It changes what the resume DOES, which is why the flag is on the
	// state rather than re-derived from the config at resume time: the
	// company's providers may have been applied again while the run was
	// parked, and a run launched in a box must be collected as one whatever
	// the config says days later. The alternative — reading the seat's
	// provider at resume — resumes a box run into the host loop with no
	// messages, which rescues a turn that in fact delivered.
	//
	// False is a host-mode suspension, which carries a conversation to
	// re-enter.
	AgentRun bool `json:"agent_run,omitempty"`

	// Steered is every person's note the turn had read when it suspended,
	// rendered exactly as the model read it, in order; Steers the rounds of
	// THIS phase that read them.
	//
	// HERE because a note binds the rest of the turn, and the rest of the
	// turn is often in another process. The suspended conversation carries
	// the notes this phase read — the resumed executor re-enters it — but
	// the reviewer and any later executor iteration open fresh
	// conversations, and without this they would be handed the task the
	// person had corrected. See internal/agent/steer. Empty when no note
	// was read before the suspend.
	Steered []string           `json:"steered,omitempty"`
	Steers  []types.PhaseSteer `json:"steers,omitempty"`
}

// Uncharged is spend carried across a park, in the counters a work item's
// charge is made of (see worklet.TurnSpend, which it mirrors field for field:
// this package is the conversation's wire format and does not import a
// domain).
type Uncharged struct {
	Turns      int `json:"turns,omitempty"`
	Rounds     int `json:"rounds,omitempty"`
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	WallMs     int `json:"wall_ms,omitempty"`
	Workers    int `json:"workers,omitempty"`
	SentBack   int `json:"sent_back,omitempty"`
}

// TurnSoFar is a turn's own measurement carried across a park, in the terms
// the record that ends it states (types.TurnTally, which it mirrors but for
// whether the turn failed — a parked turn has not ended either way).
type TurnSoFar struct {
	// DurationMS is the earlier segments' spans summed, each from its start
	// to its end.
	DurationMS int `json:"duration_ms,omitempty"`
	// Reviews, SentBack and Unaccepted are the review phases the earlier
	// segments completed, those that sent the work back, and those that
	// decided anything but done.
	Reviews    int `json:"reviews,omitempty"`
	SentBack   int `json:"sent_back,omitempty"`
	Unaccepted int `json:"unaccepted,omitempty"`
}

// ErrUnknownVersion reports a state this build cannot read.
var ErrUnknownVersion = errors.New("execstate: unknown version")

// ErrNoPendingCall reports a state with no dangling call to answer.
var ErrNoPendingCall = errors.New("execstate: no pending tool call")

// ErrDanglingCalls reports a conversation that left more than one call
// unanswered.
var ErrDanglingCalls = errors.New("execstate: more than one unanswered tool call")

// ErrDuplicateCallID reports a conversation in which two calls share an id, so
// which of them an answer answers cannot be told.
var ErrDuplicateCallID = errors.New("execstate: two tool calls share an id")

// ErrPastTheParkedCall reports a conversation in which the model said more
// after the response that made its parked call.
var ErrPastTheParkedCall = errors.New("execstate: the conversation goes on past its parked call")

// Validate checks the suspended-conversation invariants.
//
// Run on BOTH serialize and resume, because a state that violates one is
// corrupt whichever side produced it — and the two sides are different builds,
// so a writer's check is not a reader's guarantee.
//
// EXACTLY ONE CALL UNANSWERED, AND EVERY OTHER ANSWERED. Zero means there is
// nothing to resume into; two means the model will answer one and strand the
// other, and the stranded one is a sandbox box nothing will ever collect. The
// parked call's siblings — the calls its response made beside it, which ran
// before it — are answered like any other, which is what lets a round that
// called something beside its detached run park at all.
//
// AND THE PARKED CALL IS THE CONVERSATION'S LAST WORD: its response is the
// last assistant message, so the answer the resume appends follows the call
// it answers, never a turn the model took after it. Every call's id is its
// own, or "answered" means nothing.
func (s State) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("%w: %d (this build writes %d)", ErrUnknownVersion, s.Version, Version)
	}
	if s.AgentRun {
		// AN AGENT RUN HAS NO CONVERSATION, so every invariant below is
		// about a shape it does not have. What it must NOT have is
		// messages: a state carrying both would be ambiguous about which
		// resume it wants, and the ambiguity resolves differently in the
		// two builds of a rolling upgrade.
		if len(s.Messages) > 0 {
			return fmt.Errorf("%w: an agent run carries no engine conversation, "+
				"but this state has %d messages", ErrDanglingCalls, len(s.Messages))
		}
		return nil
	}
	if s.PendingCallID == "" {
		return ErrNoPendingCall
	}
	seen := map[string]bool{}
	for _, msg := range s.Messages {
		for _, call := range msg.ToolCalls {
			if seen[call.ID] {
				return fmt.Errorf("%w: %q", ErrDuplicateCallID, call.ID)
			}
			seen[call.ID] = true
		}
	}
	dangling := s.dangling()
	switch len(dangling) {
	case 0:
		return fmt.Errorf("%w: the conversation answers every call it made", ErrNoPendingCall)
	case 1:
		parked := dangling[0]
		if parked.call.ID != s.PendingCallID || parked.call.Name != s.PendingCallName {
			return fmt.Errorf("%w: the unanswered call is %q (%s) but the state names %q (%s)",
				ErrNoPendingCall, parked.call.ID, parked.call.Name, s.PendingCallID, s.PendingCallName)
		}
		if parked.message != s.lastAssistant() {
			return fmt.Errorf("%w: %q", ErrPastTheParkedCall, s.PendingCallID)
		}
	default:
		ids := make([]string, 0, len(dangling))
		for _, open := range dangling {
			ids = append(ids, open.call.ID)
		}
		return fmt.Errorf("%w: %s", ErrDanglingCalls, words.And(ids))
	}
	return nil
}

// openCall is a call the conversation never answered, and the message that
// made it.
type openCall struct {
	call    llm.ToolCall
	message int
}

// dangling is every call the conversation never answered.
func (s State) dangling() []openCall {
	answered := map[string]bool{}
	for _, msg := range s.Messages {
		if msg.ToolCallID != "" {
			answered[msg.ToolCallID] = true
		}
	}
	var open []openCall
	for i, msg := range s.Messages {
		for _, call := range msg.ToolCalls {
			if !answered[call.ID] {
				open = append(open, openCall{call: call, message: i})
			}
		}
	}
	return open
}

// lastAssistant is the index of the conversation's last assistant message, or
// -1.
func (s State) lastAssistant() int {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == llm.RoleAssistant {
			return i
		}
	}
	return -1
}

// Encode serializes the state for the pending-run row, validating first.
//
// A refusal here is better than a row that cannot be resumed: the suspending
// turn still holds the box and can fail loudly, where a resume finding a
// corrupt row has already lost the conversation.
//
// The result is the row's bytes and nothing else. It used to be decoded back
// into a map[string]any for the row to hold, and that decode read every number
// in the conversation as a float64 — so a nineteen-digit id a model passed as
// a tool argument came back from a suspension rounded to the nearest
// representable double, and the resumed loop replayed (and could act on) an
// argument the model never wrote. Bytes have no such step to get wrong.
func Encode(s State) (json.RawMessage, error) {
	s.Version = Version
	if err := s.Validate(); err != nil {
		return nil, err
	}
	blob, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("execstate: encode: %w", err)
	}
	return blob, nil
}

// Decode reads a state back out of a pending-run row.
//
// An empty blob — no bytes, a JSON null or an object with no keys — is
// (State{}, false, nil): a run with no suspended conversation is an ordinary
// condition — a crash between launching the job and persisting the suspend —
// and the caller settles it rather than treating it as a broken store.
func Decode(blob json.RawMessage) (State, bool, error) {
	if len(blob) == 0 {
		return State{}, false, nil
	}
	// The version FIRST, off the raw blob, because which shape to decode
	// into is exactly what it answers. Decoding into the current State and
	// checking afterwards would silently zero every field another format
	// spells differently before the version was ever looked at.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		return State{}, false, fmt.Errorf("execstate: decode: %w", err)
	}
	if len(fields) == 0 {
		return State{}, false, nil
	}
	var version float64
	if raw, ok := fields["version"]; ok {
		// A version that is not a number leaves zero, which no build has
		// ever written and which therefore takes the refusal below.
		_ = json.Unmarshal(raw, &version)
	}
	switch int(version) {
	case Version:
		var s State
		if err := unmarshalExact(blob, &s); err != nil {
			return State{}, false, fmt.Errorf("execstate: decode: %w", err)
		}
		if err := s.Validate(); err != nil {
			return State{}, false, err
		}
		return s, true, nil
	default:
		// LOUD, and the row is left alone. A resume that guessed at a
		// format it does not know would act on a half-understood
		// conversation; refusing leaves the run for a node that
		// understands it, which in a rolling upgrade is a node that
		// exists.
		return State{}, false, fmt.Errorf("%w: %d (this build reads %d)",
			ErrUnknownVersion, int(version), Version)
	}
}

// unmarshalExact decodes with every number kept as the digits it was written
// in. The conversation's tool arguments are map[string]any, and a plain
// Unmarshal reads a number there as a float64 — exact only to 2^53, so an id
// with more digits than that changes value across the suspension. The
// provider that decoded the call kept it as a json.Number, and this is the
// same choice on the way back in.
func unmarshalExact(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(into)
}

// Answer appends the tool result that answers the pending call, returning the
// conversation a resumed loop starts from.
//
// The result goes in as a tool message against the pending call's id, which is
// what makes the resumed loop see a completed exchange rather than a dangling
// request the provider would reject.
func (s State) Answer(content string) []llm.Message {
	out := make([]llm.Message, 0, len(s.Messages)+1)
	out = append(out, s.Messages...)
	return append(out, llm.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: s.PendingCallID,
		Name:       s.PendingCallName,
	})
}
