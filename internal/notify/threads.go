package notify

import (
	"context"
	"fmt"
	"time"
)

// The thread-follow model, written once for every chat backend.
//
// The rule is the same everywhere Crewlet speaks chat:
//
//   - A TOP-LEVEL channel message always reaches the seat. Its bot is in the
//     channel, so it should see what is said in the channel.
//   - A THREAD REPLY reaches it only if the seat FOLLOWS that thread.
//   - A seat starts following when it is mentioned, when somebody uses a
//     collective address, when it posts in the thread itself, or when
//     something subscribes it explicitly.
//
// What differs per backend is only the shape of a thread identifier and the
// GRAMMAR a mention is written in — Slack rewrites every mention into markup
// (`<@U123>`) before delivery, Mattermost leaves it literal (`@agent-swe`).
// Both are abstracted behind [MentionGrammar], so the model above exists in
// one place and a third backend supplies a grammar rather than a rule.

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
	// FollowExplicit means something subscribed the seat directly.
	FollowExplicit FollowReason = "explicit"
)

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
	// the vendor at connect, so every message arriving before that would
	// otherwise read as a personal mention of every seat.
	Detect(text, selfIdentity string) (FollowReason, bool)
}

// FollowStore persists follow state across restarts.
//
// It has to be durable. No chat backend exposes per-bot thread subscription
// state, so this IS that state — and holding it in memory means every
// restart makes every seat deaf to every thread it was following, with no
// way back but for somebody to mention it again.
type FollowStore interface {
	Follow(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) error
	Following(ctx context.Context, backend, handle, channel, thread string) (string, bool, error)
	Unfollow(ctx context.Context, backend, handle, channel, thread string) (bool, error)
}

// ChatMessage is one inbound chat message, in backend-neutral terms.
type ChatMessage struct {
	Channel string

	// Thread is the conversation this message belongs to, EMPTY for a
	// top-level channel message. That emptiness is the whole distinction
	// the follow model turns on, so a backend that reports a top-level
	// message's own id here would deliver every reply in every thread.
	Thread string

	Text string

	// Targeting is what the BACKEND knows about this message that its
	// text cannot show. See [Targeting].
	Targeting Targeting
}

// Targeting is a backend's own answer to "was this seat a target?".
//
// Some backends compute one, and it is strictly better than any grammar: a
// server resolving a collective address against real membership, or matching
// a group and a notification keyword, knows things no pattern over the text
// could. But it is only half an answer — a server that rewrites the field
// per connection reports a broadcast and a personal mention identically,
// because both resolved to this seat's own id.
//
// So the two signals answer DIFFERENT questions and neither can do the
// other's job: the backend says WHETHER, the text says WHY. Using the list
// for "why" turns every broadcast into a personal address, which is exactly
// what the working-status modes exist to prevent; using the text for
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

	// Reason is why, when a follow is involved. Empty for a top-level
	// channel message, which needs none — the bot is in the channel.
	Reason FollowReason

	// Followed is whether THIS message established or refreshed a follow,
	// as opposed to riding one already held.
	Followed bool
}

// Reaches decides whether a message reaches a seat, recording any follow it
// establishes.
//
// One method rather than a detect / follow / check trio, because those three
// steps in that order ARE the rule — and a rule spread across three calls is
// one each backend re-assembles, in its own order, with its own omissions.
//
// The follow is recorded BEFORE the delivery decision and independently of
// it: a mention in a thread means this seat follows the thread from now on,
// whether or not this particular message is the one that wakes it.
//
// An unreadable store reports NOT following — the read fails closed. A
// missed thread reply is quiet and self-healing, because the next mention
// re-establishes the follow through the ordinary path; delivering instead
// would wake the seat for every reply in every thread of every channel its
// bot sits in, which is a burst of turns nobody asked for and cannot be
// taken back. The error rides along so the caller can log it.
func (t *ThreadTracker) Reaches(ctx context.Context, handle, selfIdentity string, m ChatMessage, at time.Time) (Delivery, error) {
	reason, triggered := t.trigger(m, selfIdentity)

	var err error
	if triggered && m.Thread != "" {
		err = t.store.Follow(ctx, t.Backend(), handle, m.Channel, m.Thread, string(reason), at)
		if err != nil {
			// The follow was not recorded, but this message still
			// named the seat — deliver it and let the next mention
			// re-establish the follow. Dropping it too would mean a
			// store blip eats a message somebody addressed by name.
			log.Warn("thread_follow_not_recorded", "backend", t.Backend(),
				"handle", handle, "thread", m.Thread, "error", err.Error())
			return Delivery{Deliver: true, Reason: reason}, err
		}
	}
	if triggered {
		return Delivery{Deliver: true, Reason: reason, Followed: m.Thread != ""}, nil
	}
	if m.Thread == "" {
		// A top-level channel message. The bot is in the channel.
		return Delivery{Deliver: true}, nil
	}

	held, following, err := t.store.Following(ctx, t.Backend(), handle, m.Channel, m.Thread)
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{Deliver: following, Reason: FollowReason(held)}, nil
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
// The one write that is driven by OUTBOUND traffic, and the reason chat
// backends' own echoes of a bot's messages are worth reading rather than
// discarding at the door: the echo is how a node learns that a seat it may
// not even be running has joined a conversation.
//
// It does not downgrade an existing follow: a seat already following because
// it was named keeps that reason, because participation is the weaker
// signal and an operator reading `participated` on a thread the seat was
// summoned to would be reading a lie.
func (t *ThreadTracker) Participated(ctx context.Context, handle, channel, thread string, at time.Time) error {
	if thread == "" {
		return nil
	}
	if _, following, err := t.store.Following(ctx, t.Backend(), handle, channel, thread); err != nil {
		return err
	} else if following {
		return nil
	}
	return t.store.Follow(ctx, t.Backend(), handle, channel, thread,
		string(FollowParticipated), at)
}

// Follow subscribes a seat to a thread explicitly.
func (t *ThreadTracker) Follow(ctx context.Context, handle, channel, thread string, at time.Time) error {
	return t.store.Follow(ctx, t.Backend(), handle, channel, thread,
		string(FollowExplicit), at)
}

// Unfollow drops a subscription, reporting whether one was there.
func (t *ThreadTracker) Unfollow(ctx context.Context, handle, channel, thread string) (bool, error) {
	return t.store.Unfollow(ctx, t.Backend(), handle, channel, thread)
}
