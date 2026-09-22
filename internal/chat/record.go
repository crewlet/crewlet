package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// RecordVersion is the record shape THIS BUILD can decode.
//
// A record above it is RETAINED rather than skipped — see the deferral
// contract in [github.com/crewlet/crewlet/internal/statelog] — which is what
// makes a rolling upgrade a period of reduced coverage rather than an outage.
const RecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER.
//
// An eviction whose version this build could not read would be deferred, and a
// deferred gate leaves this node's own gate table empty while it goes on
// applying every record the evicted node appends — with no inverse that
// repairs it. So it is pinned at 1 and never evolves: a field it needs that it
// cannot have is a field that belongs somewhere else.
const GateRecordVersion = 1

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys — `v`, `op_id`, `subject`, `op`, `created_at`, `gen`,
// `writer`, `scope` — are RESERVED at the top level of the format for its
// life: a later version may add fields beside them and may never repurpose
// one. It is the same split the event envelope makes between a known header
// and an opaque body, for the same reason — a rolling upgrade puts a record
// this build cannot read on the wire, and dropping it would make every upgrade
// an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown,
	// never for this struct.
	V int `json:"v"`

	// OpID is a uuid7: the idempotency key, the Nats-Msg-Id, and the ops
	// table's key. EMPTY on a barrier, deliberately — an op id is what
	// invites a message id, and a duplicate ack is served out of the
	// dedupe window with no quorum round trip at all, which is the one
	// thing a read barrier must never be.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record is published on. For channel
	// state it is what the broker arbitrates; for a message it is the
	// ROOM, and it arbitrates nothing — see [KindMessage].
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. It rides
	// the record so an operator reading the log can see when a write was
	// decided, and it is NEVER ORDERED ON and never written to a row:
	// every instant this domain stores is the broker's, which is what
	// makes one node's copy of a conversation byte-identical to another's.
	// Empty on a barrier, because nothing renders one.
	//
	// A MESSAGE'S DISPLAYED TIME IS THE BROKER'S TOO, and that is worth
	// stating because it is the field somebody will reach for: two people
	// posting a second apart on nodes whose clocks disagree by a minute
	// would otherwise render out of order against the order the log
	// actually has, and every node would render it differently.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence —
	// so the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate —
	// which runs before any kind rule, on a node that may not be able to
	// decode the payload at all. Empty on a barrier, which writes no rows
	// for a gate to drop.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, because it must be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
//
// TWO CONDITIONS, NOT ONE. An eviction is a gate by its KIND. An ERASE and a
// PRUNE are gates by their OP, on an ordinary channel subject, and they have
// to be: both DELETE message rows, and a deferred deletion is a node that goes
// on serving what every other node removed — an operator's redaction still
// readable on one member, or a year of a room that retention deleted
// everywhere else. Neither has an inverse that repairs it, which is exactly
// the test the wiki's purge meets.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpErase || e.Op == OpPrune
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the sequence this mutation was decided against.
	// PROVENANCE ONLY — the broker is what enforced it, and nothing reads
	// this to decide anything. Absent on every additive record, which is
	// every message.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	TurnID     string     `json:"turn_id,omitempty"`
	Chain      []string   `json:"chain,omitempty"`

	// Notify is the routing snapshot, and NIL is what "wakes nobody"
	// means.
	//
	// A NIL POINTER RATHER THAN A quiet FLAG, so the wake filter is "has a
	// Notify" — a question about the record's own shape — instead of a
	// boolean a writer can forget to set. A quiet record is a full record,
	// ordered exactly like a loud one, and writes its history row like
	// every other: quiet means it wakes nobody, and nothing else.
	//
	// A SYSTEM NARRATION IS THE CASE THIS EXISTS FOR — the engine saying
	// somebody joined, or a prune taking a year of a room. A flag on the
	// payload would have to be read after a version-gated decode, by which
	// point the record a newer build wrote has already been filed as
	// routable.
	Notify *Notify `json:"notify,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT in the envelope, deliberately: the BROKER'S instant.
//
// The effective instant every duration is measured against derives from the
// broker's own store timestamp, which the replication loop stamps onto the
// record before calling the applier. A writer must not be able to author it,
// and a field a writer could set is a field a writer could lie about — so it
// reaches the applier as an argument rather than as a key in the format.
// Stated here so nobody "completes" the envelope by adding it.

// Reason is WHY somebody was woken by a message.
//
// It is the wire vocabulary and nothing else: the arithmetic that decides
// which reason a recipient gets is ONE function set in recipients.go, called
// by the write path's decide AND by the parser, so the two can never disagree
// about what a record means.
type Reason string

// The eight reasons.
//
// `reply` and `reply_to_own_root` are one event split in two, and the split is
// the whole point of the vocabulary: a reply in a thread you merely take part
// in is news, while a reply to the thread YOU STARTED is a question put back
// to you. Collapsing them would make every busy thread oblige every past
// speaker to answer again.
const (
	// ReasonDM is a message in a conversation that exists only between
	// these parties. Nothing addresses it more directly than that.
	ReasonDM Reason = "dm"

	// ReasonMention is a message that named this handle.
	ReasonMention Reason = "mention"

	// ReasonReplyToOwnRoot is a reply in a thread this recipient started.
	ReasonReplyToOwnRoot Reason = "reply_to_own_root"

	// ReasonLeadFallback is a person or an operator posting in a unit's
	// room where the routing reached nobody else.
	//
	// IT ARISES ONLY IN THE ABSENCE OF EVERY OTHER REASON, which is what
	// puts it last among the addressed ones although it obliges an answer
	// as much as a mention does: it is the answer to "somebody spoke to
	// this unit and nobody was listening", and a room where that resolved
	// to silence is a person talking to an empty screen.
	ReasonLeadFallback Reason = "lead_fallback"

	// ReasonReply is a reply in a thread this recipient takes part in.
	ReasonReply Reason = "reply"

	// ReasonCollective is an `@channel`: the message addressed the room
	// rather than anybody in it.
	ReasonCollective Reason = "collective"

	// ReasonFollow is a thread this recipient follows.
	ReasonFollow Reason = "follow"

	// ReasonFollowAll is a room this recipient follows in full, which the
	// engine sets for a unit's own agent seats in that unit's room.
	ReasonFollowAll Reason = "follow_all"
)

// Reasons are the eight, IN PRECEDENCE ORDER: a recipient appears once, under
// the first of these that applies to them.
//
// # The ordering, and the two rules it is built from
//
// EVERY ADDRESSED REASON OUTRANKS EVERY UNADDRESSED ONE. An obligation to
// answer is a stronger claim than an FYI, so the four [Reason.Addressed] ones
// come first — and that is a property a test can assert over this slice rather
// than a convention somebody maintains.
//
// WITHIN EACH HALF, WHAT THIS MESSAGE DID TO YOU OUTRANKS THE ROLE YOU HOLD.
// That is the tracker's own routing precedence, and it decides the rest: a DM
// exists for you, a mention named you, a reply answered the thread you
// started; then a reply in a thread you are in and an `@channel` are both
// about this message, while following a thread or a room is a standing
// arrangement. A reply outranks a collective address because a reply is about
// a conversation you are already in and `@channel` is about everybody.
//
// THE ORDER IS THE VOCABULARY'S, NOT THE ARITHMETIC'S. Nothing in this package
// computes a precedence; recipients.go walks this slice.
var Reasons = []Reason{
	ReasonDM, ReasonMention, ReasonReplyToOwnRoot, ReasonLeadFallback,
	ReasonReply, ReasonCollective, ReasonFollow, ReasonFollowAll,
}

// Valid reports whether a reason off the wire is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// Addressed reports whether being woken for this reason OBLIGES A REPLY.
//
// FOUR, AND IT IS A CLOSED SET: a direct conversation, a mention, a reply to
// the thread you started, and the lead fallback. Each is somebody speaking TO
// this recipient.
//
// THE OTHER FOUR ARE NOT, and each would be a false obligation: a collective
// address is spoken to the room, a plain reply reaches everybody who ever
// posted in the thread, and both follow reasons are standing arrangements the
// recipient made rather than anything this message did. A seat that treats
// them as addressed answers every remark in every room it follows, which is a
// company that talks to itself instead of working.
//
// This is the one value the delivery gate, the prompt and the working
// indicator all read, through
// [github.com/crewlet/crewlet/internal/notify.AddressRule], so the three can
// never disagree about whether somebody owes an answer.
func (r Reason) Addressed() bool {
	switch r {
	case ReasonDM, ReasonMention, ReasonReplyToOwnRoot, ReasonLeadFallback:
		return true
	}
	return false
}

// Recipient is one seat this message wakes, and why.
//
// THE REASON RIDES THE RECORD beside the handle rather than being recomputed
// downstream, because the parser that fans a wake out may run on a node whose
// applier has not reached the change: recomputing would route from stale rows,
// or block the feed until it had caught up.
type Recipient struct {
	Handle string `json:"h"`
	Reason Reason `json:"r"`
}

// Notify is the routing snapshot a wake is derived from, copied at write time
// so the node that wins a feed message routes without reading anything.
//
// EVERYTHING A NOTIFICATION IS BUILT FROM IS HERE, and that is the contract
// rather than a convenience: the feed relays the RECORD, and the node that
// wins that message may be one whose applier has not reached the post — so a
// parser that read a message row instead would route from a room it has not
// seen, or stall the feed until it had.
type Notify struct {
	// MessageID is what a recipient opens. A wake with no message id
	// reaches nobody at all — there is nothing to open.
	MessageID string `json:"message_id"`

	// ChannelID, ChannelName and ChannelKind are the room, so a card
	// renders and a filter runs without a lookup. The NAME is empty for a
	// direct conversation, which has none.
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	ChannelKind Kind   `json:"channel_kind"`

	// ThreadRoot is the message this one replies to, empty on a root post.
	// It is what tells a thread reply from a room post at the moment of
	// routing rather than after a read.
	ThreadRoot string `json:"thread_root,omitempty"`

	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`

	// Excerpt is at most [MaxExcerpt] bytes of what a card should show,
	// cut rune-safely by [Excerpt].
	Excerpt string `json:"excerpt,omitempty"`

	// Mentions are the handles the body named, carried beside the
	// recipients rather than folded into them: a mention of somebody who
	// is not a member of the room wakes nobody, and a card that showed
	// only the woken would render the message as though it had never
	// named them.
	Mentions []string `json:"mentions,omitempty"`

	// Lead is the seat a [ReasonLeadFallback] resolved to, RESOLVED AT
	// WRITE TIME inside the decide.
	//
	// ON THE RECORD rather than looked up by the parser, because who leads
	// a unit is a fact about the EPOCH: a parser resolving it later would
	// route a message posted under one org chart to whoever leads under
	// the next one.
	Lead string `json:"lead,omitempty"`

	// Recipients is the resolved wake set, each under its strongest
	// reason, computed once at write time so the feed never has to
	// subtract and can never forget to. At most [MaxRecipients].
	Recipients []Recipient `json:"recipients,omitempty"`

	// ThreadContext is the earlier lines of this message's thread, oldest
	// first, empty on a root post and on a thread with nothing before
	// this message.
	//
	// ON THE RECORD, which is the whole reason a native chat turn needs no
	// reconciliation pass where a vendor one does. The node that wins a
	// feed message is rarely the node running the woken seat and is often
	// behind on the rows, so a prompt that fetched the thread at wake time
	// would read a thread shorter than the one it is answering — or none
	// at all, on a node that has not applied the root yet.
	//
	// HOW MANY is the company's own `chat.native.thread_context_messages`,
	// read once at the edge and carried here; [MaxThreadContext] and
	// [MaxThreadContextBytes] are the ceilings that hold whatever it asked
	// for, so no config value can widen a record past what
	// [MaxRecordBytes] was sized for.
	ThreadContext []ThreadLine `json:"thread_context,omitempty"`

	// WakesTruncated says the routing reached fewer seats than the
	// message named, because a collective address exceeded
	// [MaxCollectiveRecipients].
	//
	// ON THE RECORD rather than inferred from a count, because the count
	// that would reveal it is the size of the set BEFORE the cap and the
	// record deliberately does not carry that set. A room that cannot see
	// this reads a partial broadcast as a complete one.
	WakesTruncated bool `json:"wakes_truncated,omitempty"`
}

// ThreadLine is one earlier message of a thread, as a woken seat reads it.
//
// AN EXCERPT RATHER THAN A BODY, and the same [Excerpt] every card uses: this
// is context for answering the newest message, not a transcript to reproduce,
// and a thread of full bodies is what [MaxThreadContextBytes] exists to
// refuse.
//
// NO MESSAGE ID. A line here is something to read, and every id it could
// carry would invite a model to act on a message it was only shown — reply to
// it, react to it, quote it back. The message being answered is the one on
// the record.
type ThreadLine struct {
	Author     string     `json:"author"`
	AuthorKind AuthorKind `json:"author_kind"`
	Excerpt    string     `json:"excerpt"`
}

// Excerpt is the ONE spelling of how a body becomes a card's preview.
//
// [textcut.Within] rather than [textcut.Ellipsis], because [MaxExcerpt] is a
// CEILING [Notify.Validate] enforces: a marker appended outside the budget
// would turn every long message into a refused write rather than a marked one.
func Excerpt(body string) string { return textcut.Within(body, MaxExcerpt) }

// Validate refuses a notification that would render wrong or cost the fleet
// more than it was meant to.
//
// # Why the caps are checked at the publish boundary
//
// Because the record is what the cost lives on. A snapshot is copied onto the
// log, replicated to every node, held for the stream's whole retention window
// and read back by every applier — so a collection that grew without a bound
// is not one screen rendering badly, it is bytes every member of the fleet
// stores for a year. And it is a REFUSAL rather than a trim: a trim would
// publish a notification that silently told fewer people than the writer
// named, which is the failure this whole family exists to prevent, arriving at
// the one place nobody is watching.
func (n *Notify) Validate() error {
	if n == nil {
		return nil
	}
	if n.MessageID == "" {
		return invalid("notify.message_id", "a wake names no message, so there "+
			"is nothing for a recipient to open")
	}
	if n.ChannelID == "" {
		return invalid("notify.channel_id", "a wake names no room, so nothing "+
			"can render where it was said")
	}
	if !n.ChannelKind.Valid() {
		return invalid("notify.channel_kind", "%q is not a channel kind this "+
			"build serves — the kind decides how a card renders and whether a "+
			"reason is addressed, so an unknown one is a wake nothing can "+
			"present", n.ChannelKind)
	}
	if !n.AuthorKind.Valid() {
		return invalid("notify.author_kind", "%q is not an author kind this "+
			"build serves", n.AuthorKind)
	}
	if len(n.Excerpt) > MaxExcerpt {
		return invalid("notify.excerpt", "a notification excerpt is %d bytes "+
			"against a %d cap — build it with chat.Excerpt, which cuts inside "+
			"the budget on a rune boundary", len(n.Excerpt), MaxExcerpt)
	}
	if len(n.Mentions) > MaxMentions {
		return invalid("notify.mentions", "a notification names %d mentions "+
			"and the maximum is %d", len(n.Mentions), MaxMentions)
	}
	if len(n.ThreadContext) > MaxThreadContext {
		return invalid("notify.thread_context", "a notification carries %d "+
			"earlier lines of its thread and the maximum is %d — the count is "+
			"the company's `chat.native.thread_context_messages`, which is "+
			"itself bounded there, so a record past this cap is one built "+
			"from something other than that setting",
			len(n.ThreadContext), MaxThreadContext)
	}
	if size := threadContextBytes(n.ThreadContext); size > MaxThreadContextBytes {
		return invalid("notify.thread_context", "a notification's thread "+
			"context is %d bytes against a %d cap — build it with "+
			"chat.ThreadContextOf, which takes lines newest first and stops "+
			"when the budget is spent", size, MaxThreadContextBytes)
	}
	for _, line := range n.ThreadContext {
		if !line.AuthorKind.Valid() {
			return invalid("notify.thread_context", "%q is not an author kind "+
				"this build serves", line.AuthorKind)
		}
	}
	if len(n.Recipients) > MaxRecipients {
		return invalid("notify.recipients", "a notification wakes %d seats and "+
			"the maximum is %d — the arms are disjoint reasons over the same "+
			"people, so a set this size is a routing that stopped deduplicating",
			len(n.Recipients), MaxRecipients)
	}
	for _, r := range n.Recipients {
		if r.Handle == "" {
			return invalid("notify.recipients", "a recipient names no handle, "+
				"so the wake reaches nobody while counting against the cap")
		}
		if !r.Reason.Valid() {
			return invalid("notify.recipients", "%q is not a wake reason this "+
				"build knows — the reason decides whether %s owes an answer, "+
				"and an unknown one has no answer to that question",
				r.Reason, r.Handle)
		}
	}
	return nil
}

// ErrFutureVersion reports a record a newer build wrote.
//
// It carries the SUBJECT because the caller is the applier's deferral arm,
// which has to file the record under something — and a version error with no
// subject is a record that can only be dropped.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("chat: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the kind
// and op a gate reads, and the version that decides whether there is a second
// pass at all. A record that failed to decode AT ALL yields no position, no
// kind, no subject and no scope, so the only thing that could ever be done
// with it is to drop it.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("chat: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("chat: a record carries version "+
			"%d — every record states its version, and one that does not "+
			"cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it is the one that may refuse on version.
//
// It returns the envelope alongside the error whenever the envelope itself
// decoded, because that is precisely the case the applier retains: the caller
// needs the subject, the scope and the kind of a record it cannot read.
func Decode(payload []byte) (MutationRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, err
	}
	if env.V > RecordVersion {
		return MutationRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec MutationRecord
	extra, err := decodeInto(payload, &rec, recordFields)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("chat: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote.
//
// LOSSLESS IN BOTH DIRECTIONS, which is what makes a rolling upgrade safe: a
// node that read a record it only half understood and republished it — the
// reanchor path does exactly that — must not strip the half it did not.
func Encode(rec MutationRecord) ([]byte, error) {
	data, err := encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("chat: encode the record on %s: %w",
			rec.Subject, err)
	}
	return data, nil
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again: the omitempty names have to
// be listed because a zero value does not marshal them, and a name missing
// here is decoded into the struct AND carried as unknown — so the next encode
// writes the stale carried copy back over what the caller set.
var recordFields = fieldSet(MutationRecord{}, "op_id", "created_at", "gen",
	"writer", "expect", "mutation", "actor", "actor_kind", "operator_id",
	"turn_id", "chain", "notify")

// ---- encoding ---------------------------------------------------------- //
//
// THE PACKAGE'S ONE ENCODER. Every shape here — the record and each typed
// payload — goes through these three functions rather than a merge of its own:
// a carried field LOSES to a known one, and two implementations of that rule
// are one place where a stale carried copy undoes the write that set it.

// encode marshals a value and folds unknown fields back in.
func encode(value any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("chat: encode: %w", err)
	}
	if len(extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("chat: encode: %w", err)
	}
	for name, value := range extra {
		if _, known := merged[name]; !known {
			merged[name] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("chat: encode: %w", err)
	}
	return out, nil
}

// decodeInto unmarshals into out and returns the fields out has no home for.
func decodeInto(data []byte, out any, known map[string]bool) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for name, value := range all {
		if known[name] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[name] = value
	}
	return extra, nil
}

// fieldSet is the JSON names a struct defines: the ones a zero value
// marshals, plus the omitempty names given explicitly.
func fieldSet(v any, omitted ...string) map[string]bool {
	data, err := json.Marshal(v)
	if err != nil {
		panic("chat: a record type does not marshal: " + err.Error())
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		panic("chat: a record type does not marshal to an object: " + err.Error())
	}
	out := make(map[string]bool, len(named)+len(omitted))
	for name := range named {
		out[name] = true
	}
	for _, name := range omitted {
		out[name] = true
	}
	return out
}

// threadContextBytes is what a thread costs on the record: the excerpts and
// the authors, which is every field a line carries that is not a fixed-width
// enum.
//
// THE AUTHORS COUNT. A thread of fifty one-word replies from handles that are
// each forty bytes is two thirds handle, and a budget that measured only the
// text would let exactly that shape through.
func threadContextBytes(lines []ThreadLine) int {
	total := 0
	for _, line := range lines {
		total += len(line.Excerpt) + len(line.Author)
	}
	return total
}

// ThreadContextOf takes at most n lines of a thread for a routing snapshot,
// within [MaxThreadContextBytes].
//
// THE INPUT IS OLDEST FIRST and so is the answer, because that is the order a
// person reads a conversation in and the order a prompt renders. What the
// budget is spent in is the OTHER direction: lines are admitted newest first
// and the answer is re-ordered, so a long thread keeps the exchange nearest
// the message being answered rather than its opening.
//
// A LINE THAT DOES NOT FIT ENDS THE WALK rather than being skipped over for a
// shorter one behind it. A thread with a hole in it reads as a complete
// conversation that did not happen, which is worse than a short one: the
// model answers a sequence nobody had.
func ThreadContextOf(lines []ThreadLine, n int) []ThreadLine {
	if n <= 0 || len(lines) == 0 {
		return nil
	}
	if n > MaxThreadContext {
		n = MaxThreadContext
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	budget := MaxThreadContextBytes
	first := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		cost := len(lines[i].Excerpt) + len(lines[i].Author)
		if cost > budget {
			break
		}
		budget -= cost
		first = i
	}
	if first == len(lines) {
		return nil
	}
	return slices.Clone(lines[first:])
}
