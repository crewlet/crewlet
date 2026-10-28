// Package execstate is the wire format for a turn parked on its executor's
// run.
//
// Every seat's executor is its coding CLI's own agentic run, detached in a box
// and minutes to hours long. The turn that launched it does not wait for it:
// it parks, and is re-entered when the run is collected — after a restart,
// possibly on another node, possibly days later once a person answers a
// question the run asked. The turn cannot be a parked goroutine: the run
// outlives the process. So what the rest of the turn needs is serialized into
// the pending-run row and read back from there.
//
// There is NO CONVERSATION here. The run's own session holds its transcript,
// in its box; the engine never had one to keep. What a resume needs is the
// turn's bookkeeping — the surface the run was offered, the rounds closed
// before it, what the turn was asked and by whom, what it wrote and owes —
// and the context the turn fetched once, at its start, which every later
// launch of the turn is built from.
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
// resumed turn acting on a half-understood state is worse than a run that
// waits for a node which understands it.
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
// exists only for a build that never shipped. Versions 1 to 3 are such: this
// build reads 4 alone and refuses the others as unknown.
//
// AND A VERSION NO RELEASE AND NO IMAGE OF MAIN WROTE IS NOT YET A FORMAT: the
// rule that nothing is removed within a version protects rows some build
// shipped, and nothing shipped 4 yet, so 4 lost its notes field (`steered`)
// without a bump. The notes a turn has read are on its run's record now, read
// there by every segment that resumes it (internal/engine, runsteer.go), and a
// second copy here would be a second answer to the same question.
package execstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Version is the format this build writes, and the one it reads.
//
// Bumped only for a change a previous build could MISREAD. An added field a
// reader can default is not a bump; a changed meaning for an existing field
// is.
//
// It is 4 because 2 and 3 described a parked engine conversation — a tool
// loop stopped on an unanswered call, 3 the one a CLI drove over MCP — with a
// flag marking the states that were not one. Every state is a parked run now
// and the conversation is gone, so a build that read a new row as 3 would
// take it for a conversation with no messages; a number is never reused, so a
// blob carrying 3, 2 or 1 is refused as a version this build does not read,
// never decoded as this one. Images of main wrote rows of 3, which is why that
// number is retired rather than taken over.
const Version = 4

// State is a turn parked on its executor's run.
//
// JSON tags are the on-the-wire names and are part of the contract: renaming a
// Go field is free, renaming a tag is a format change.
type State struct {
	Version int `json:"version"`

	// ActiveTools is the tool surface the run was offered, including
	// anything activated before it launched. Replayed rather than
	// re-derived: the run's bridged calls are collected against the
	// surface it was given, and one rebuilt from the plan would lose every
	// activation the turn made.
	ActiveTools []string `json:"active_tool_names"`

	// LoadedSkills is the skill-guard state, replayed for the same reason.
	LoadedSkills []string `json:"loaded_skill_keys"`

	// Round is the turn iteration the run was launched in, so the resumed
	// turn continues numbering rather than restarting at 1.
	//
	// TWO READERS, and they have to agree: the runner stamps the collected
	// phase's own record with it, and the turn loop starts its round
	// counter there. The loop did not read it once, which is how one turn
	// came to publish phase records numbered from the parked round while
	// its budget was being spent from one again.
	Round int `json:"iteration"`

	// LaunchMS is how long the executor phase's LAUNCHING segment took, in
	// milliseconds: from the phase opening — its started record — to the
	// run being under way, the prompt, the member pick, the box and its
	// setup included.
	//
	// HERE because the executor is one phase in two segments and ONE record:
	// the launch publishes the phase's start and nothing else, and the
	// collection publishes its completion, whose duration covers both
	// segments and not the run between them (types.AgentPhaseCompleted's
	// DurationMS). The launch's half is known only to the segment that
	// launched, and the collection is often another process. Without it the
	// record measured the collection alone — a millisecond — and every
	// reader drew an executor that took no time.
	LaunchMS int `json:"launch_ms,omitempty"`

	// LaunchedAt is when that launching segment opened the phase — its
	// started record's instant — on the launching node's clock, in UTC.
	//
	// THE START OF THE RUN'S ONE ROUND. The collected record states a
	// single round, from the engine's request (this instant) to the run's
	// answer (the collection), and places the run's bridged calls inside
	// it; LaunchMS is a span with no anchor, and the collection is often
	// another process that never saw the launch. Every launch records it;
	// a state that carries none leaves that round untimed rather than
	// placing it at the collection, which would draw a run that took no
	// time.
	LaunchedAt time.Time `json:"launched_at,omitzero"`

	// Deadline is the instant the turn's wall-clock cap ends, in UTC — a
	// scheduled fire's cap from the moment the turn began — and zero for a
	// turn with none.
	//
	// HERE because the cap bounds the TURN, and the turn is several
	// segments, each a later call that is often another process: measured
	// from each segment's own start, the cap bounded nothing — the
	// launching segment ends at its first round, and every resumed one
	// started its clock afresh. Carried, the segment that collects the run
	// refuses to start another round past it, and relaunches nothing.
	Deadline time.Time `json:"deadline,omitzero"`

	// Written is the work items the turn's writes had committed to when it
	// parked, in first-write order, and WrittenMany whether there were
	// more than it lists: the turn's own write set (turnctx.Written), carried.
	//
	// HERE because a turn nothing at dispatch named an item for is charged
	// at its END to the one item it wrote, and a parked turn ends in
	// another segment, often in another process. Without this the resumed
	// segment would judge "exactly one" over only what it wrote itself: a
	// turn that filed its task before launching its run would end
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
	// and a turn that filed its task, launched its run and resumed
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

	// Iterations is the closed-round ledger of the parked TURN. The resume
	// is the same turn, so without this it would forget every round that
	// closed before the park.
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

	// Interactions is who spoke to the seat and what they said — one per
	// constituent of the trigger, as the dispatch read them — parked for the
	// reason Ask is: the segment that settles the turn re-reads no trigger.
	//
	// TWO READERS, both in that segment. Its completed-turn event carries
	// them, and that event is the one every post-turn worker that waits for
	// a settled turn reads — the persist decider attributes each fact to the
	// interaction that stated it, and every executor's turn parks, so
	// without these every turn settled with nobody having said anything.
	// And who ASKED is read off them as a dispatch reads it
	// (turnctx.Turn.Senders): a refresh_memory call re-runs the turn-start
	// memory filter, whose per-subject rule is judged against exactly that
	// list. Empty for a turn no interaction woke.
	Interactions []types.InboundInteraction `json:"interactions,omitempty"`

	// Context is what the turn fetched and rendered ONCE, at its start, that
	// every launch's prompt is built from: the prefetched blocks and the
	// prior-turns block.
	//
	// HERE because the turn's later launches are made by the segment that
	// collected this run, often in another process, and the freeze is the
	// point of these strings: re-fetched there, the seat's memory, its
	// company's pages and its thread would answer differently, so a turn
	// sent back for another iteration would launch on a system prompt that
	// moved underneath work already in hand — and pay a provider's whole
	// prompt again, since a cache keys on an exact prefix. The zero value is
	// a turn that fetched nothing, whose prompts carry none of these blocks.
	Context Frozen `json:"frozen_context"`
}

// Frozen is a turn's start-of-turn context as a park carries it: each
// prefetched block's rendered text, and the prior-turns block.
//
// MIRRORED rather than the prefetch package's own type, for the reason
// [Uncharged] is: this package is the parked turn's wire format, and a type
// embedded whole would put on the row everything that type ever grows — the
// knowledge hits behind a block, the turn's embedding — none of which a
// launch's prompt reads.
//
// BOUNDED where it is rendered, not here: each block by its own source and the
// conversation by its render budget, so the row carries the strings the first
// launch's prompt did and nothing more.
type Frozen struct {
	ThreadContext       string `json:"thread_context,omitempty"`
	PersonalMemory      string `json:"personal_memory,omitempty"`
	RelevantKnowledge   string `json:"relevant_knowledge,omitempty"`
	EpisodeRecall       string `json:"episode_recall,omitempty"`
	CounterpartyProfile string `json:"counterparty_profile,omitempty"`
	SynthesizedSkills   string `json:"synthesized_skills,omitempty"`
	Conversation        string `json:"conversation,omitempty"`
}

// Uncharged is spend carried across a park, in the counters a work item's
// charge is made of (see worklet.TurnSpend, which it mirrors field for field:
// this package is the parked turn's wire format and does not import a
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

// Validate checks what a parked state must be to be resumed: one this build
// writes.
//
// Run on BOTH serialize and resume, because a state that is not is corrupt
// whichever side produced it — and the two sides are different builds, so a
// writer's check is not a reader's guarantee.
func (s State) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("%w: %d (this build writes %d)", ErrUnknownVersion, s.Version, Version)
	}
	return nil
}

// Encode serializes the state for the pending-run row, validating first.
//
// A refusal here is better than a row that cannot be resumed: the parking
// turn still holds the run and can fail loudly, where a resume finding a
// corrupt row has already lost the turn.
//
// The result is the row's bytes and nothing else. It used to be decoded back
// into a map[string]any for the row to hold, and that decode read every number
// as a float64 — so a nineteen-digit id a model passed as a tool argument came
// back from a park rounded to the nearest representable double, and the
// resumed turn acted on an argument the model never wrote. Bytes have no such
// step to get wrong.
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
// (State{}, false, nil): a run with no parked state is an ordinary condition —
// a crash between launching the job and persisting the park — and the caller
// settles it rather than treating it as a broken store.
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
		// format it does not know would act on a half-understood state;
		// refusing leaves the run for a node that understands it, which in
		// a rolling upgrade is a node that exists.
		return State{}, false, fmt.Errorf("%w: %d (this build reads %d)",
			ErrUnknownVersion, int(version), Version)
	}
}

// unmarshalExact decodes with every number kept as the digits it was written
// in. The closed rounds' calls carry their arguments as map[string]any
// ([ledger.Iteration]), and a plain Unmarshal reads a number there as a
// float64 — exact only to 2^53, so an id with more digits than that changes
// value across the park. The provider that decoded the call kept it as a
// json.Number, and this is the same choice on the way back in.
func unmarshalExact(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(into)
}
