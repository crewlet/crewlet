package config

import (
	"time"
)

// Chat is which chat surface this company talks on.
//
// THE THIRD BACKEND AXIS, on exactly the terms the tracker and the knowledge
// base run on: single-homed, derived from integration presence when it is
// left empty, and refused when a native backend is declared beside the vendor
// it would duplicate. The reasoning is the same one the other two axes state —
// two surfaces would both route and both answer "where was this decided",
// with nothing keeping them in step — and it bites harder here, because a
// conversation is the one artefact this engine cannot reconcile after the
// fact: a person replied in one place and the agents read the other.
//
// A company may run MATTERMOST AND SLACK TOGETHER (two workspaces with
// different people in them, which is what an org migrating between them
// looks like), so the vendor value here names a POSTURE rather than a
// product. See [ChatBackend].
type Chat struct {
	// Backend is which chat surface this company runs.
	//
	// Empty DERIVES: a company that declares a vendor chat block gets
	// `vendor`, and one that declares none gets `native` — so a company
	// that configures nothing has somewhere for its seats to talk, and a
	// company already on Mattermost or Slack keeps the surface it had
	// without reading this note.
	Backend ChatBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=native|vendor|none" desc:"Which chat surface: native, vendor, or none. Empty derives from integrations.mattermost and the seats' Slack apps."`

	// Native is the engine's own chat's policy — the settings that exist
	// because the company owns the messages rather than renting a place to
	// put them.
	//
	// PRESENT ONLY ON A NATIVE COMPANY, and refused otherwise, for the
	// reason [Tracker.Native] gives: a retention horizon on a company whose
	// messages live in somebody else's product describes nothing, and the
	// failure that produces is silence.
	Native *ChatNativeConfig `yaml:"native,omitempty" json:"native,omitempty"`
}

// ChatBackend is which chat surface a company runs.
type ChatBackend string

// The chat surfaces.
const (
	// ChatNative is the engine's own: channels, messages and threads held
	// as records on the company's own log and applied into every node.
	ChatNative ChatBackend = "native"

	// ChatVendor is somebody else's chat, read and written through the
	// vendor transports.
	//
	// A POSTURE RATHER THAN A PRODUCT, which is where this axis differs
	// from `tracker.backend: jira` and `knowledge.backend: confluence`.
	// Each of those names its vendor because there is exactly one of them
	// and a company on it is on it; chat has two served surfaces, and a
	// company legitimately runs BOTH — Mattermost and Slack are different
	// workspaces with different people in them, and an org migrating from
	// one to the other runs the pair for months. A value naming a product
	// would have no answer for that company, and `mattermost+slack` is not
	// a backend, it is a list.
	ChatVendor ChatBackend = "vendor"

	// ChatNone is a company whose seats do not talk to people in chat at
	// all: the tracker, the code host and schedules are what wake them.
	ChatNone ChatBackend = "none"
)

// ChatBackends is the closed set, read by both the validator and the schema
// generator so an editor's enum cannot drift from what the engine accepts.
var ChatBackends = []ChatBackend{ChatNative, ChatVendor, ChatNone}

// Valid reports whether b is a chat surface this build serves.
func (b ChatBackend) Valid() bool {
	switch b {
	case ChatNative, ChatVendor, ChatNone:
		return true
	}
	return false
}

// ChatNativeConfig is the founder's policy over the engine's own chat.
//
// # Why these four and nothing else
//
// Everything else a chat surface could be told is either a fact about the
// OPERATOR (how big the log may grow, how long its replay window is kept —
// Tier A, under `stream.`) or a decision the engine makes once for everybody
// (every cap the record layer enforces). What is left is genuinely a
// company's own: how long it keeps what people said, whether a room is
// public by default, when a seat raises its working indicator, and how much
// of a thread rides into the prompt of the seat it wakes.
type ChatNativeConfig struct {
	// MessageRetentionDays is how long a message lives before the prune
	// covers it.
	//
	// A POINTER, because this field has THREE settings and a plain int can
	// carry two: absent takes the 365-day default, 0 means keep messages
	// FOR EVER, and 30..3650 is a horizon. Collapsing absent and 0 is not a
	// theoretical hazard in this tree — `providers.sandbox.default_pause_ttl_seconds`
	// was a plain float64 whose 0 means "never pause", every operator who
	// wrote it was silently mapped onto the 1800-second default, and their
	// paused boxes were kept and billed for exactly as long as the setting
	// existed to refuse. The same collapse here deletes a company's whole
	// chat history a year after somebody asked for it to be kept for ever.
	//
	// NOTE THE CONTRAST WITH `tracker.native.inbox_retention_days`, whose 0
	// means the opposite: there 0 is "take the default", because an inbox
	// row is DERIVED from a record that answers for ever, so "keep the
	// mailbox for ever" is not a setting anybody needs and a plain int
	// loses nothing. A message is not derived from anything — it is the
	// only copy of what somebody said — so "for ever" is exactly the
	// setting a company with a legal-hold policy comes here to write.
	MessageRetentionDays *int `yaml:"message_retention_days,omitempty" json:"message_retention_days,omitempty" js:"min=0;max=3650" desc:"How long a message lives (default 365, 30..3650). 0 keeps messages for ever."`

	// DefaultChannelPrivate makes a channel created without an explicit
	// visibility private.
	//
	// A PLAIN BOOL, and the contrast with the field above is the whole of
	// the zero-value rule: false is not "unset" here, it is the SETTING —
	// channels are public unless somebody says otherwise, which is what a
	// company that put its conversations in one place wanted. There is no
	// third state to carry, so there is nothing for a pointer to hold.
	DefaultChannelPrivate bool `yaml:"default_channel_private,omitempty" json:"default_channel_private,omitempty" desc:"Create channels private when the caller names no visibility. Default false: a company's rooms are readable by its seats."`

	// TypingStatus is when a seat raises its working indicator in a
	// channel.
	//
	// THE VENDOR BLOCKS' OWN TYPE, verbatim, rather than a second enum
	// spelling the same two words: the question ("is somebody plausibly
	// waiting on this seat") is a property of the MESSAGE and not of the
	// product carrying it, and two closed sets that agree today are two
	// that drift on the next value. See [WorkingStatus].
	TypingStatus WorkingStatus `yaml:"typing_status,omitempty" json:"typing_status,omitempty" js:"enum=always|addressed" desc:"When to show the working indicator (default always)."`

	// ThreadContextMessages is how many earlier messages of a thread ride
	// into the prompt of the seat a reply wakes.
	//
	// 0 TAKES THE DEFAULT rather than meaning "no context", which is the
	// tracker's shape for the same reason it is the tracker's: there is no
	// "none" setting to collide with it. A chat turn's whole input is the
	// conversation — a seat handed a reply with none of the thread it
	// replies to cannot answer it, and would answer anyway — so this engine
	// does not offer zero, and a company that wants to spend fewer tokens
	// writes a smaller number.
	//
	// Ten because it is one screen of a thread and about two rounds of a
	// conversation, and because the whole block is re-sent on every round
	// of the tool loop: the count is really a multiplier on the dominant
	// repeated content of a chat turn, the same arithmetic
	// [Company.NotificationCoalesceMaxBatch] is capped by. Fifty is the
	// ceiling for that reason and not because a longer thread is rare.
	ThreadContextMessages int `yaml:"thread_context_messages,omitempty" json:"thread_context_messages,omitempty" js:"min=0;max=50" desc:"Earlier thread messages carried into the woken seat's prompt (default 10, up to 50). 0 takes the default."`
}

// The message horizon's default and bounds.
const (
	// DefaultMessageRetentionDays is a year: long enough that "what did we
	// decide last quarter" is answered out of chat, short enough that a
	// company that never thought about it is not keeping every message for
	// the life of the deployment by accident.
	DefaultMessageRetentionDays = 365

	// MinMessageRetentionDays is a month, below which the archive stops
	// being one: a decision taken while somebody was on leave would be gone
	// before they read it.
	MinMessageRetentionDays = 30

	// MaxMessageRetentionDays is ten years. Past it the horizon is not a
	// policy but a rounding error against "for ever", which is what 0 is
	// for and says plainly.
	MaxMessageRetentionDays = 3650
)

// DefaultThreadContextMessages is how much of a thread a woken seat reads
// when the company names no number. See the field for why ten.
const DefaultThreadContextMessages = 10

// MaxThreadContextMessages bounds it, on the field's own arithmetic: the
// block is re-sent on every round of the tool loop, so the count multiplies
// the dominant repeated content of the turn.
const MaxThreadContextMessages = 50

// MessageRetention is how long a message lives, with the default applied,
// and whether there is a horizon at all.
//
// TWO RETURNS, because a zero [time.Duration] is the one value that cannot
// say "for ever": every caller would have to remember that 0 means the
// opposite here of what it means everywhere else a duration is read, and the
// one caller that forgot would prune a company's entire history on its first
// sweep. A false second return is "no horizon", and the duration is then
// meaningless rather than zero.
func (n *ChatNativeConfig) MessageRetention() (time.Duration, bool) {
	days := DefaultMessageRetentionDays
	if n != nil && n.MessageRetentionDays != nil {
		days = *n.MessageRetentionDays
	}
	if days == 0 {
		return 0, false
	}
	return time.Duration(days) * 24 * time.Hour, true
}

// Status is the working-indicator mode, applying the always default — the
// same accessor, with the same default, that [Slack.Status] and
// [Mattermost.Status] answer with.
func (n *ChatNativeConfig) Status() WorkingStatus {
	if n == nil || n.TypingStatus == "" {
		return StatusAlways
	}
	return n.TypingStatus
}

// ThreadContext is how many earlier thread messages a woken seat reads,
// applying the default. A nil block is a native company that wrote no policy,
// which takes every default here.
func (n *ChatNativeConfig) ThreadContext() int {
	if n == nil || n.ThreadContextMessages == 0 {
		return DefaultThreadContextMessages
	}
	return n.ThreadContextMessages
}

func (n *ChatNativeConfig) validate(path Path) error {
	var p problems
	if n == nil {
		return nil
	}
	p.wrap(n.TypingStatus.validate(at(path, "typing_status")))
	if d := n.MessageRetentionDays; d != nil && *d != 0 &&
		(*d < MinMessageRetentionDays || *d > MaxMessageRetentionDays) {
		p.add(at(path, "message_retention_days"), ErrOutOfRange,
			"%d is outside %d..%d — below a month a decision taken while "+
				"somebody was away would be gone before they read it, and past "+
				"ten years the horizon is a rounding error against never "+
				"pruning. Write 0 for that, which keeps messages for ever",
			*d, MinMessageRetentionDays, MaxMessageRetentionDays)
	}
	if m := n.ThreadContextMessages; m < 0 || m > MaxThreadContextMessages {
		p.add(at(path, "thread_context_messages"), ErrOutOfRange,
			"%d is outside 0..%d, where 0 takes the default of %d: the block "+
				"is re-sent on every round of the tool loop, so this count "+
				"multiplies the repeated content of a whole turn",
			m, MaxThreadContextMessages, DefaultThreadContextMessages)
	}
	return p.err()
}

// ChatBackendFor resolves the chat surface, deriving an empty one.
func (c *Company) ChatBackendFor() ChatBackend {
	if c.Chat.Backend != "" {
		return c.Chat.Backend
	}
	if len(c.chatVendorSurfaces()) > 0 {
		return ChatVendor
	}
	return ChatNative
}

// chatVendorSurfaces is every vendor chat surface this company declares,
// named by the path it was declared at, in document order.
//
// THE PATHS RATHER THAN THE NAMES, because the refusals built on this tell an
// author what to delete and Slack is not where a reader would look for it:
// the org-level `integrations.slack` block is optional (it holds
// working-indicator settings and nothing else), and a company with seven
// working Slack apps may never have written one. So a Slack company is
// recognised exactly as [Company.DeclaresIntegration] recognises it — the
// block or any seat's own app — and reported at whichever of the two it was
// actually found at.
func (c *Company) chatVendorSurfaces() []string {
	var out []string
	if c.Integrations.Mattermost != nil {
		out = append(out, "integrations.mattermost")
	}
	if c.Integrations.Slack != nil {
		return append(out, "integrations.slack")
	}
	// THE FIRST SEAT CARRYING ONE, and only the first: the refusal asks an
	// author to choose a chat surface, not to read a list of every seat
	// that has an app.
	for role, path := range c.EachRole() {
		if role.Integrations.Slack != nil {
			return append(out, at(path, "integrations.slack").String())
		}
	}
	return out
}

// validateChat holds the rules that keep the chat axis single-homed, and it
// is [Company.validateKnowledgeBackend]'s four rules over a third surface:
// an unknown value, a native backend beside the vendor it would duplicate, a
// vendor backend with no vendor to talk to, and a native policy block on a
// company that is not native.
//
// Selection keys on integration-block PRESENCE, for the reason the other two
// axes give: every setting inside a chat block has a default, so no value in
// one can be the signal.
func (c *Company) validateChat() error {
	var p problems

	if b := c.Chat.Backend; b != "" && !b.Valid() {
		p.add(field("chat.backend"), ErrShape,
			"chat.backend must be one of: %s", names(ChatBackends))
	}

	vendors := c.chatVendorSurfaces()

	// A BACKEND AND ITS VENDOR TOGETHER IS THE MIRROR THE DOCTRINE FORBIDS,
	// and a conversation is the artefact it cannot be undone for: both
	// surfaces would route, both would wake seats, and a person who replied
	// in one of them has answered a thread the agents are reading the other
	// half of. Refused at the authored path so the message names what to
	// delete.
	if c.Chat.Backend == ChatNative && len(vendors) > 0 {
		p.add(field("chat.backend"), ErrConflict,
			"native chat and %s cannot both run: conversations would live in "+
				"two places and nothing would keep them in step. Remove one",
			names(vendors))
	}
	if c.Chat.Backend == ChatVendor && len(vendors) == 0 {
		p.add(field("chat.backend"), ErrConflict,
			"chat.backend: vendor needs integrations.mattermost, "+
				"integrations.slack or a seat carrying its own Slack app — "+
				"there is no vendor surface here to talk on")
	}

	// A NATIVE BLOCK ON A COMPANY THAT IS NOT NATIVE describes nothing, and
	// the failure that produces is silence: an operator writes a retention
	// horizon that no message this company holds will ever be measured
	// against. Against the DERIVED backend rather than the literal field,
	// because an empty `backend` with no vendor chat IS native, and
	// refusing the default configuration would be the opposite of the
	// intent.
	if c.Chat.Native != nil && c.ChatBackendFor() != ChatNative {
		p.add(field("chat.native"), ErrConflict,
			"this company's chat is %q, and `chat.native` is the engine's own "+
				"chat's policy — nothing would read it. Remove the block, or run "+
				"native chat", c.ChatBackendFor())
	}
	return p.err()
}
