package config

import (
	"time"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// Chat is which chat surface this company talks on.
//
// THE THIRD BACKEND AXIS, on exactly the terms the tracker and the knowledge
// base run on: single-homed, derived from integration presence when it is
// left empty, and refused when Chatlet is declared beside the vendor
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
	// `vendor`, and one that declares none gets `chatlet` — so a company
	// that configures nothing has somewhere for its seats to talk, and a
	// company already on Mattermost or Slack keeps the surface it had
	// without reading this note.
	Backend ChatBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=chatlet|vendor|none" desc:"Which chat surface: chatlet, vendor, or none. Empty derives from integrations.mattermost, integrations.slack and the seats' Slack apps."`

	// Chatlet is Chatlet's policy — the settings that exist
	// because the company owns the messages rather than renting a place to
	// put them.
	//
	// PRESENT ONLY ON A CHATLET COMPANY, and refused otherwise, for the
	// reason [Tracker.Worklet] gives: a retention horizon on a company whose
	// messages live in somebody else's product describes nothing, and the
	// failure that produces is silence.
	Chatlet *ChatletConfig `yaml:"chatlet,omitempty" json:"chatlet,omitempty"`
}

// ChatBackend is which chat surface a company runs.
type ChatBackend string

// The chat surfaces.
const (
	// ChatChatlet is the engine's own: channels, messages and threads held
	// as records on the company's own log and applied on every data node.
	ChatChatlet ChatBackend = "chatlet"

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
var ChatBackends = []ChatBackend{ChatChatlet, ChatVendor, ChatNone}

// chatVendor is one integration surface a [ChatVendor] company can talk to
// people on.
type chatVendor struct {
	// name is the surface's own name, as [Company.DeclaresIntegration] takes
	// it and as a client is told it ([Company.ChatVendors]).
	name string
	// declaredAt is the path a company declares this surface at, or "" for a
	// company that does not.
	declaredAt func(c *Company) string
	// howToDeclare is how an author declares it, for the refusal of a vendor
	// backend with no vendor to talk to.
	howToDeclare string
	// title is how a sentence names the surface to a person.
	title string
	// transport is the contact identity a person is reached at on this
	// surface ([org.HumanContact.Identities]), and contactKey the key it is
	// written under in a seat's `contact` — for the advisory that names a
	// person nobody can mention here ([Company.unreachableWarnings]).
	transport  org.Transport
	contactKey string
}

// chatVendors is EVERY vendor chat surface, in the order a screen names them
// — the two [ChatVendor] is a posture over.
//
// STATED ONCE, and read by everything that asks the question: which backend a
// company derives ([Company.ChatBackendFor]), the refusals that name what to
// delete ([Company.validateChat]), whether a company declares the surface at
// all ([Company.DeclaresIntegration]), the list a client is told
// ([Company.ChatVendors]) and the advisory naming a person nobody can
// mention there ([Company.unreachableWarnings]). It was a list of names
// beside a function that tested the same two blocks by hand, with nothing
// tying them: a vendor added to one would have had the viewer answer
// `vendor` naming none, or name one the backend decision never counted.
var chatVendors = []chatVendor{
	{
		name: "mattermost",
		declaredAt: func(c *Company) string {
			if c.Integrations.Mattermost != nil {
				return "integrations.mattermost"
			}
			return ""
		},
		howToDeclare: "integrations.mattermost",
		title:        "Mattermost",
		transport:    org.TransportMattermost,
		contactKey:   "mattermost_user_id",
	},
	{
		name: "slack",
		// THE BLOCK, OR THE FIRST SEAT CARRYING AN APP. The org-level
		// `integrations.slack` block is optional — it holds
		// working-indicator settings and nothing else — so a company with
		// seven working Slack apps may never have written one, and a refusal
		// naming the block would send its author to a line that is not
		// there. Only the first seat: the refusal asks an author to choose a
		// chat surface, not to read a list of every seat that has an app.
		declaredAt: func(c *Company) string {
			if c.Integrations.Slack != nil {
				return "integrations.slack"
			}
			for role, path := range c.EachRole() {
				if role.Integrations.Slack != nil {
					return at(path, "integrations.slack").String()
				}
			}
			return ""
		},
		howToDeclare: "integrations.slack, or a seat carrying its own Slack app",
		title:        "Slack",
		transport:    org.TransportSlack,
		contactKey:   "slack_user_id",
	},
}

// chatVendorNamed is the vendor chat surface called name, if it is one.
func chatVendorNamed(name string) (chatVendor, bool) {
	for _, vendor := range chatVendors {
		if vendor.name == name {
			return vendor, true
		}
	}
	return chatVendor{}, false
}

// ChatVendors is the name of every vendor chat surface this company declares,
// in the order a screen names them — empty, never nil, for a company that
// declares none.
func (c *Company) ChatVendors() []string {
	out := []string{}
	for _, vendor := range chatVendors {
		if vendor.declaredAt(c) != "" {
			out = append(out, vendor.name)
		}
	}
	return out
}

// Valid reports whether b is a chat surface this build serves.
func (b ChatBackend) Valid() bool {
	switch b {
	case ChatChatlet, ChatVendor, ChatNone:
		return true
	}
	return false
}

// ChatletConfig is the founder's policy over the engine's own chat.
//
// # Why these three and nothing else
//
// Everything else a chat surface could be told is either a fact about the
// OPERATOR (how big the log may grow, how long its replay window is kept —
// Tier A, under `stream.`) or a decision the engine makes once for everybody
// (every cap the record layer enforces). What is left is genuinely a
// company's own: how long it keeps what people said, whether a room is
// public by default, and when a seat raises its working indicator.
//
// How much of a thread a woken seat is shown is NOT here, and that is the
// engine's decision rather than an omission: the turn-start prefetch reads the
// thread for every chat surface alike and bounds it at one byte ceiling, a
// third of the conversation ledger's, keeping the root and the newest whole
// messages and condensing the ones between them — and that bound is
// deliberately not configuration (see internal/agent/prefetch).
type ChatletConfig struct {
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
	// NOTE THE CONTRAST WITH `tracker.worklet.inbox_retention_days`, whose 0
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

// MessageRetention is how long a message lives, with the default applied,
// and whether there is a horizon at all.
//
// TWO RETURNS, because a zero [time.Duration] is the one value that cannot
// say "for ever": every caller would have to remember that 0 means the
// opposite here of what it means everywhere else a duration is read, and the
// one caller that forgot would prune a company's entire history on its first
// sweep. A false second return is "no horizon", and the duration is then
// meaningless rather than zero.
func (n *ChatletConfig) MessageRetention() (time.Duration, bool) {
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
func (n *ChatletConfig) Status() WorkingStatus {
	if n == nil || n.TypingStatus == "" {
		return StatusAlways
	}
	return n.TypingStatus
}

// DefaultPrivate is whether a room created without a stated visibility is
// private, with the nil block answering the documented default.
//
// AN ACCESSOR RATHER THAN THE FIELD: `chat.chatlet` is optional, so every
// reader would otherwise carry the nil check, and a reader that forgot it
// would panic on the commonest company there is — one that wrote no chat
// block at all.
func (n *ChatletConfig) DefaultPrivate() bool {
	if n == nil {
		return false
	}
	return n.DefaultChannelPrivate
}

func (n *ChatletConfig) validate(path Path) error {
	var p problems
	if n == nil {
		return nil
	}
	p.wrap(n.TypingStatus.validate(at(path, "typing_status")))
	if d := n.MessageRetentionDays; d != nil && *d != 0 &&
		(*d < MinMessageRetentionDays || *d > MaxMessageRetentionDays) {
		p.add(at(path, "message_retention_days"), ErrOutOfRange,
			"%d is outside %d..%d. Under a month, someone who was away could "+
				"miss a decision before they read it. To keep messages longer, "+
				"write 0, which keeps them for ever",
			*d, MinMessageRetentionDays, MaxMessageRetentionDays)
	}
	return p.err()
}

// ChatBackendFor resolves the chat surface, deriving an empty one.
func (c *Company) ChatBackendFor() ChatBackend {
	if c.Chat.Backend != "" {
		return c.Chat.Backend
	}
	if len(c.chatVendorPaths()) > 0 {
		return ChatVendor
	}
	return ChatChatlet
}

// chatVendorPaths is every vendor chat surface this company declares, named
// by the path it was declared at, in [chatVendors] order.
//
// THE PATHS RATHER THAN THE NAMES, because the refusals built on this tell an
// author what to delete, and a Slack company may have declared Slack only on
// a seat ([chatVendors]).
func (c *Company) chatVendorPaths() []string {
	var out []string
	for _, vendor := range chatVendors {
		if path := vendor.declaredAt(c); path != "" {
			out = append(out, path)
		}
	}
	return out
}

// validateUnitRooms refuses two units that DECLARE one channel on a company
// whose chat is the engine's own.
//
// # A Chatlet unit room belongs to ONE unit
//
// The engine opens a room for each channel a unit declares, and the room
// records the unit it was opened for once and for good: its members are that
// unit's subtree and its lead, and that lead is who answers a person nobody
// else did. So a second unit naming the same channel is something the room
// cannot hold — the engine opens it for whichever unit its walk reaches first
// and reports every other one as not reconciled at every boot and every apply,
// while that team's prompt calls the room its own and none of its seats are in
// it. On a vendor surface the same shape is merely worth knowing, and
// [Company.channelAdvisories] says so; here it is a room the engine cannot
// build.
//
// AN ADMISSION RULE rather than a runnable one, for the reason every admission
// rule is one ([Company.ValidateRunnable]): nothing about RUNNING depends on
// it — a revision carrying the shape still runs, one unit with its room and
// the engine reporting the other — and during a rolling upgrade this build
// applies what a newer peer admitted, so refusing such a revision at apply
// would split the fleet's epoch over one team's room. A document somebody
// submits with it is refused.
//
// COMPARED AS WRITTEN, which is the comparison the room's address makes: the
// channel grammar ([unitChannel]) admits only names the Chatlet address
// normalisation leaves unchanged, and a test holds the two together. And the
// DECLARED channel only — a child that writes none inherits its parent's room,
// which is the remedy this rule points at rather than a collision.
func (c *Company) validateUnitRooms() error {
	if c.ChatBackendFor() != ChatChatlet {
		return nil
	}
	type declarer struct {
		name string
		path Path
	}
	holders := map[string][]declarer{}
	var order []string
	var walk func(units []Unit, path Path)
	walk = func(units []Unit, path Path) {
		for i := range units {
			u := &units[i]
			here := idx(path, i)
			if u.Channel != "" {
				if _, seen := holders[u.Channel]; !seen {
					order = append(order, u.Channel)
				}
				holders[u.Channel] = append(holders[u.Channel], declarer{u.Name, here})
			}
			walk(u.Children, at(here, "children"))
		}
	}
	walk(c.Units, field("units"))

	// BESIDE EACH OF THEM, the placement a duplicated unit name gets:
	// fixing it means changing all but one, and an author shown the
	// refusal on only one of them cannot tell which.
	var p problems
	for _, channel := range order {
		units := holders[channel]
		if len(units) < 2 {
			continue
		}
		for i, u := range units {
			others := make([]string, 0, len(units)-1)
			for j, other := range units {
				if j != i {
					others = append(others, other.name)
				}
			}
			p.add(at(u.path, "channel"), ErrConflict,
				"unit %q declares the channel %q, and so does %s. In Chatlet "+
					"a unit's room belongs to one unit, so the others would "+
					"be left out of it. Give each its own channel, or leave a "+
					"child unit's empty so it inherits its parent's room",
				u.name, channel, words.And(others))
		}
	}
	return p.err()
}

// validateChat holds the rules that keep the chat axis single-homed, and it
// is [Company.validateKnowledgeBackend]'s four rules over a third surface:
// an unknown value, Chatlet beside the vendor it would duplicate, a
// vendor backend with no vendor to talk to, and a Chatlet policy block on a company that does not run Chatlet.
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

	vendors := c.chatVendorPaths()

	// A BACKEND AND ITS VENDOR TOGETHER IS THE MIRROR THE DOCTRINE FORBIDS,
	// and a conversation is the artefact it cannot be undone for: both
	// surfaces would route, both would wake seats, and a person who replied
	// in one of them has answered a thread the agents are reading the other
	// half of. Refused at the authored path so the message names what to
	// delete.
	if c.Chat.Backend == ChatChatlet && len(vendors) > 0 {
		p.add(field("chat.backend"), ErrConflict,
			"Chatlet and %s can't both run, because conversations would "+
				"be split between them. Remove one", names(vendors))
	}
	if c.Chat.Backend == ChatVendor && len(vendors) == 0 {
		ways := make([]words.Said, len(chatVendors))
		for i, vendor := range chatVendors {
			ways[i] = words.Said(vendor.howToDeclare)
		}
		p.add(field("chat.backend"), ErrConflict,
			"chat.backend: vendor needs %s", words.Or(ways))
	}

	// A CHATLET BLOCK ON A COMPANY THAT DOES NOT RUN CHATLET describes nothing, and
	// the failure that produces is silence: an operator writes a retention
	// horizon that no message this company holds will ever be measured
	// against. Against the DERIVED backend rather than the literal field,
	// because an empty `backend` with no vendor chat IS Chatlet, and
	// refusing the default configuration would be the opposite of the
	// intent.
	if c.Chat.Chatlet != nil && c.ChatBackendFor() != ChatChatlet {
		p.add(field("chat.chatlet"), ErrConflict,
			"this company's chat is %q, so nothing reads `chat.chatlet`. "+
				"Remove the block, or use Chatlet", c.ChatBackendFor())
	}
	return p.err()
}
