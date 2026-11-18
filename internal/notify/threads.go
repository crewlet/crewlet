package notify

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// The thread-follow model, written once for every chat backend.
//
//   - A TOP-LEVEL channel message always reaches the seat.
//   - A THREAD REPLY reaches it only if the seat FOLLOWS that thread.
//   - A seat follows when mentioned, when somebody uses a collective address,
//     or when it posts in the thread. A top-level message naming it or the
//     room starts the thread its own id becomes FOR THAT REASON, which
//     decides whether replies oblige an answer ([FollowReason.Addresses]).
//
// Backends differ only in thread id shape and mention GRAMMAR (Slack's markup
// `<@U123>`, Mattermost's literal `@agent-swe`), behind [MentionGrammar]: a
// new backend supplies a grammar, not a rule.

// FollowReason is why a seat follows a thread.
//
// Kept rather than reduced to a bool because it is the difference between
// "somebody named this seat" and "this seat happened to be in the room when
// somebody shouted" — the first question an operator asks when a seat
// answers something surprising.
type FollowReason string

const (
	// FollowMention means the seat was named personally.
	FollowMention FollowReason = "mention"
	// FollowCollective means an @channel / @here / @all style address.
	FollowCollective FollowReason = "collective"
	// FollowParticipated means the seat posted in the thread, which is what
	// every chat client does when a person replies.
	FollowParticipated FollowReason = "participated"
)

// followReasons is the vocabulary, strongest signal first. One list, so
// [FollowReason.Valid] and [AddressingFollows] cannot know different sets.
//
// THERE IS NO "explicit": a top-level message's follow of its own thread was
// recorded as a subscription whatever triggered it, so every seat in the room
// owed an answer to every reply under every @channel. That follow records the
// reason that triggered it.
var followReasons = []FollowReason{
	FollowMention, FollowCollective, FollowParticipated,
}

// keep is the reason a follow holds once a seat is asserted into its thread
// for asserted: the STRONGER of that and what it held.
//
// NEVER A DOWNGRADE: the reason decides whether a reply obliges an answer
// ([FollowReason.Addresses]), and a seat named and then caught by an @here
// would otherwise follow as a broadcast and be free to ignore the next reply.
// A shout followed by a mention still upgrades.
//
// A held reason this build does not know is KEPT: a newer build wrote it,
// nothing here can rank it, and overwriting could be the downgrade. The write
// still refreshes it.
func keep(held FollowReason, following bool, asserted FollowReason) FollowReason {
	switch {
	case !following || held == "":
		return asserted
	case !held.Valid():
		return held
	case slices.Index(followReasons, held) <= slices.Index(followReasons, asserted):
		return held
	}
	return asserted
}

// Valid reports whether r is a reason the engine knows.
//
// A reason read back out of a follow store or off a notification's metadata
// is a VALUE rather than a panic: the row may have been written by a build
// that knew a reason this one does not, which a rolling upgrade guarantees.
func (r FollowReason) Valid() bool { return slices.Contains(followReasons, r) }

// Addresses reports whether a seat in a conversation FOR THIS REASON owes
// its next message a reply.
//
// MENTION ONLY. A collective address, or having spoken once, puts the seat in
// the room without making it the addressee of all that follows; otherwise
// agents answering each other's threads never stop.
//
// MENTION IS A PROXY, deliberately: the obligation wants "this conversation
// is the seat's own", but a vendor parser holds one delivery, not the root's
// author, so being named is the closest evidence. It errs toward answering,
// the half a person notices: silence reads as a lost message.
func (r FollowReason) Addresses() bool { return r == FollowMention }

// MentionGrammar is how one backend writes a mention.
//
// Backend() rides on the grammar rather than being a separate constructor
// argument, because the two must agree: a tracker built with one backend's
// grammar and another's namespace key reads and writes the wrong rows, and
// nothing about that fails — it just makes a seat deaf in one backend and
// randomly attentive in the other.
type MentionGrammar interface {
	// Backend is the namespace this grammar's follows are stored under.
	Backend() string

	// Detect reports the follow reason text triggers for a seat known by
	// selfIdentity — a user id on a markup backend, a username on a
	// literal one.
	//
	// An EMPTY selfIdentity must fall through to the collective check
	// rather than matching everything: the identity is resolved against
	// the third-party app at connect, so every message arriving before that would
	// otherwise read as a personal mention of every seat.
	Detect(text, selfIdentity string) (FollowReason, bool)
}

// FollowStore persists follow state across restarts.
//
// Durable, because no chat backend exposes per-bot thread subscription state:
// this IS that state, and in memory every restart would make every seat deaf
// to its threads. The seat is named by its immutable handle (ADR-0013).
//
// Follow writes the reason choose answers, run against the record the write
// lands on, so no choice is made over a reason another node wrote a moment
// before; [ThreadTracker] thus keeps the STRONGER of two reasons parsed at
// once, and the store ranks nothing. Every call writes, an unchanged reason
// included: the write is the follow's activity stamp.
type FollowStore interface {
	Follow(ctx context.Context, backend, seat, channel, thread string, at time.Time,
		choose func(held string, following bool) string) (string, error)
	Following(ctx context.Context, backend, seat, channel, thread string) (string, bool, error)
}

// ChatMessage is one inbound chat message, in backend-neutral terms.
type ChatMessage struct {
	Channel string

	// Thread is the conversation this message belongs to, EMPTY for a
	// top-level channel message. That emptiness is the whole distinction
	// the follow model turns on, so a backend that reports a top-level
	// message's own id here would deliver every reply in every thread.
	Thread string

	// ID is this message's own id — the thread every reply to it will
	// carry, which is what a top-level message that names the seat makes
	// it follow. Read only where Thread is empty.
	ID string

	Text string

	// Targeting is what the BACKEND knows about this message that its
	// text cannot show. See [Targeting].
	Targeting Targeting
}

// Targeting is a backend's own answer to "was this seat a target?".
//
// A server resolving a collective address against real membership knows
// things no pattern over the text could, but it is half an answer: one that
// rewrites the field per connection reports a broadcast and a personal
// mention identically.
//
// The two signals answer DIFFERENT questions: the backend says WHETHER, the
// text says WHY. The list for "why" turns every broadcast into a personal
// address, which the working-status modes exist to prevent; the text for
// "whether" misses every mention a pattern cannot see.
type Targeting int

const (
	// TargetingUnknown means the backend has no answer, so the text decides
	// alone. The ordinary case, and the one a backfilled message re-read
	// over a REST API always falls into — those carry no mention list.
	TargetingUnknown Targeting = iota

	// TargetingPersonal means this seat is the addressee whatever the text
	// says. A direct message, where there is nobody else it could be for
	// — so even an "@channel" typed into one is addressed to this seat.
	TargetingPersonal

	// TargetingIncluded means this seat was among the targets, but the backend
	// cannot say whether personally. The text decides why; being included
	// at all is enough to follow the thread.
	TargetingIncluded

	// TargetingExcluded means this seat was NOT a target, and this VETOES the
	// text. The backend resolved the addressees and this seat is not among
	// them, which outranks anything a pattern thinks it found.
	TargetingExcluded
)

// ThreadTracker decides whether a chat message reaches a seat.
type ThreadTracker struct {
	grammar MentionGrammar
	store   FollowStore
}

// NewThreadTracker binds the follow model to one backend's grammar.
func NewThreadTracker(grammar MentionGrammar, store FollowStore) (*ThreadTracker, error) {
	if grammar == nil {
		return nil, fmt.Errorf("notify: a thread tracker needs a mention grammar")
	}
	if grammar.Backend() == "" {
		return nil, fmt.Errorf("notify: a mention grammar must name its backend")
	}
	if store == nil {
		return nil, fmt.Errorf("notify: a thread tracker needs a follow store")
	}
	return &ThreadTracker{grammar: grammar, store: store}, nil
}

// Backend is the namespace this tracker reads and writes.
func (t *ThreadTracker) Backend() string { return t.grammar.Backend() }

// Delivery is what a tracker decided about one message.
type Delivery struct {
	// Deliver is whether the message reaches the seat.
	Deliver bool

	// Reason is why THIS MESSAGE reached the seat: what it triggered, or,
	// for a reply that triggered nothing, why the seat follows the thread.
	// Empty for a top-level channel message that triggered nothing, which
	// needs no reason — the bot is in the channel.
	Reason FollowReason

	// Following is why the seat is in this message's THREAD once this
	// message is counted — the reason the follow holds afterwards, which
	// for a trigger is the stronger of what it held and what it asserted,
	// so an @here in a thread the seat was named in reaches it as a
	// broadcast while it stays there because it was named. Empty where no
	// follow is involved, and where one could not be recorded.
	Following FollowReason

	// Followed is whether THIS message established or refreshed a follow,
	// as opposed to riding one already held.
	Followed bool
}

// Reaches decides whether a message reaches a seat, recording any follow it
// establishes.
//
// One method, not a detect / follow / check trio: those steps in that order
// ARE the rule. The follow is recorded BEFORE the delivery decision, and a
// TOP-LEVEL trigger follows the thread its own id becomes, for the reason
// that triggered it.
//
// An unreadable store reports NOT following (fails closed): a missed reply
// self-heals at the next mention, while delivering would wake the seat for
// every reply in every thread its bot sits in. The error rides along for the
// log. seat is the seat's handle.
func (t *ThreadTracker) Reaches(ctx context.Context, seat, selfIdentity string, m ChatMessage, at time.Time) (Delivery, error) {
	reason, triggered := t.trigger(m, selfIdentity)

	// The thread a trigger follows: the one this message is in, or the one
	// a top-level message starts.
	thread := m.Thread
	if thread == "" {
		thread = m.ID
	}
	if triggered && thread != "" {
		following, err := t.assert(ctx, seat, m.Channel, thread, reason, at)
		if err != nil {
			// The follow was not recorded, but this message still
			// named the seat — deliver it and let the next mention
			// re-establish the follow. Dropping it too would mean a
			// store blip eats a message somebody addressed by name.
			log.WarnContext(ctx, "thread_follow_not_recorded", "backend", t.Backend(),
				"seat", seat, "thread", thread, "error", err.Error())
			return Delivery{Deliver: true, Reason: reason}, err
		}
		return Delivery{Deliver: true, Reason: reason, Following: following, Followed: true}, nil
	}
	if triggered {
		return Delivery{Deliver: true, Reason: reason}, nil
	}
	if m.Thread == "" {
		// A top-level channel message. The bot is in the channel.
		return Delivery{Deliver: true}, nil
	}

	held, following, err := t.store.Following(ctx, t.Backend(), seat, m.Channel, m.Thread)
	if err != nil {
		return Delivery{}, err
	}
	if !following {
		return Delivery{}, nil
	}
	return Delivery{Deliver: true, Reason: FollowReason(held), Following: FollowReason(held)}, nil
}

// assert records that a seat is in a thread for reason, keeping a stronger
// reason the follow already holds, and answers the reason it holds afterwards.
//
// THE ONE WRITE every follow takes — a trigger and the seat's own post alike —
// so the no-downgrade rule ([keep]) and the activity stamp are one path rather
// than one per caller: each of them used to decide for itself, and only one
// remembered not to downgrade.
func (t *ThreadTracker) assert(ctx context.Context, seat, channel, thread string,
	reason FollowReason, at time.Time) (FollowReason, error) {

	held, err := t.store.Follow(ctx, t.Backend(), seat, channel, thread, at,
		func(held string, following bool) string {
			return string(keep(FollowReason(held), following, reason))
		})
	return FollowReason(held), err
}

// trigger combines the backend's targeting with the text's grammar.
//
// The backend has the veto and the text has the reason — see [Targeting].
func (t *ThreadTracker) trigger(m ChatMessage, selfIdentity string) (FollowReason, bool) {
	switch m.Targeting {
	case TargetingExcluded:
		return "", false
	case TargetingPersonal:
		// Whatever the text says. A collective address typed into a
		// direct message is still addressed to the one person in it.
		return FollowMention, true
	}
	reason, triggered := t.grammar.Detect(m.Text, selfIdentity)
	if m.Targeting == TargetingIncluded && !triggered {
		// The backend saw a mention the text cannot show — a group, a
		// notification keyword, a collective resolved against real
		// membership. Being included at all is enough to follow.
		return FollowMention, true
	}
	return reason, triggered
}

// Participated records that a seat posted in a thread, which auto-follows it
// — mirroring what every chat client does when a person replies.
//
// The one write driven by OUTBOUND traffic: a backend's echo of a bot's own
// message is how a node learns a seat it may not be running joined a thread.
//
// It does not downgrade an existing follow (participation is the weaker
// signal, and the reason decides the reply obligation), but DOES refresh it:
// the record is the activity stamp (quiet follows expire after ninety days,
// coord.FollowRetention), and returning early on a held follow let a thread
// the seat kept answering expire, dropping the next unnamed reply.
func (t *ThreadTracker) Participated(ctx context.Context, seat, channel, thread string, at time.Time) error {
	if thread == "" {
		return nil
	}
	_, err := t.assert(ctx, seat, channel, thread, FollowParticipated, at)
	return err
}
