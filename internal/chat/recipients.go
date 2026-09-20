package chat

import (
	"slices"
	"strings"
)

// WHO A MESSAGE CONCERNS, and why the answer is THREE functions rather than
// one.
//
// [Resolve] is the WRITE side: it turns the rows a room actually has —
// its membership, who has spoken in the thread, who follows it, who leads the
// unit — into the recipient set that rides the record. It is the only half
// that can see those rows, because it runs inside the decide's own snapshot.
//
// [Candidates] is PURE OVER THE RECORD: no clock, no store, no roster. That is
// what lets the wake parser run it on another node minutes later and reach the
// same answer the writer did, from the snapshot the record carried instead of
// from rows it may not have applied yet.
//
// [Route] is the post-filter both of them end with, and it needs two facts no
// record can carry: who the company employs RIGHT NOW, and who made the
// change. Splitting it out is what lets the write path route against the
// roster it has and the parser route again against the roster it has, through
// ONE arithmetic — so the two can never disagree about who a message was for.
//
// # The one rule the whole file is
//
// A HANDLE APPEARS ONCE, UNDER THE FIRST REASON THAT NAMES IT. Somebody who
// is mentioned in a thread they follow in a room they follow in full is told
// they were MENTIONED, which is the strongest fact and the one they will act
// on. The order is [Reasons] — the vocabulary's, not this file's — and nothing
// here computes a precedence.

// Candidate is one handle a message concerns, and why.
//
// TWO FIELDS, AND THERE IS DELIBERATELY NO `Addressed` AMONG THEM. Whether a
// wake obliges an answer is a property of the REASON ([Reason.Addressed]), so
// a field here would be a second copy of it that a caller could set to the
// other answer — and the delivery gate, the prompt and the working indicator
// all read that one value through
// [github.com/crewlet/crewlet/internal/notify.AddressRule].
type Candidate struct {
	Handle string
	Reason Reason
}

// Addressed reports whether this candidate is being asked something, rather
// than told something.
func (c Candidate) Addressed() bool { return c.Reason.Addressed() }

// Routing is the raw material one room's rows answer with, before precedence,
// deduplication and the caps are applied to it.
//
// IT IS GATHERED INSIDE THE DECIDE'S OWN TRANSACTION, which is the whole of
// why it is a value rather than a set of lookups: every field is read in the
// snapshot the record's expectation is formed in, so the routing a record
// carries is the routing the rows supported at the instant the decision was
// made. Gathered afterwards, a membership that changed between the read and
// the publish would produce a record naming people the room no longer has.
type Routing struct {
	// ChannelKind decides two arms at once: a direct conversation wakes
	// its participants under [ReasonDM], and a private room is the one
	// where a mention of a non-member wakes nobody.
	ChannelKind Kind

	// AuthorKind is read by the fallback arm alone — see the arm for why
	// an agent's own post never raises one.
	AuthorKind AuthorKind

	// Members is the room's membership, and the only collection here that
	// carries a per-member flag: [Member.FollowAll] is the whole of the
	// fan-out policy.
	Members []Member

	// Mentions are the resolved handles the body named, IN THE ORDER IT
	// named them — which is the order they are woken in when the cap bites.
	Mentions []string

	// Collective says the message addressed the whole room (`@channel`).
	Collective bool

	// ThreadRoot is the message this one replies to, empty on a room post.
	// Three arms are silent without it: a reply to the thread's own author,
	// the thread's other speakers, and its followers.
	ThreadRoot string

	// RootAuthor is who started that thread, and ThreadParticipants and
	// Followers are who has spoken in it and who subscribed to it.
	RootAuthor         string
	ThreadParticipants []string
	Followers          []string

	// Lead is the seat that answers for this room's unit, RESOLVED AT
	// WRITE TIME: who leads a unit is a fact about the EPOCH, so a parser
	// resolving it later would route a message posted under one org chart
	// to whoever leads under the next one.
	Lead string
}

// Resolve turns a room's rows into the recipient set, reporting whether a cap
// cut it short.
//
// THE SECOND VALUE IS [Notify.WakesTruncated], and it is a value rather than
// something a caller infers because the count that would reveal it is the size
// of the set BEFORE the cap — which the record deliberately does not carry. A
// room that cannot see this reads a partial broadcast as a complete one.
func Resolve(r Routing) ([]Candidate, bool) {
	c := &collector{}
	for _, reason := range routedReasons() {
		c.add(reason, limitFor(reason), r.arm(reason))
	}
	// THE FALLBACK IS OFFERED LAST, which is the one place this walk
	// departs from [Reasons] — and the vocabulary itself says why: a lead
	// fallback "ARISES ONLY IN THE ABSENCE OF EVERY OTHER REASON". Its
	// place among the addressed reasons is a statement about whether it
	// obliges an answer, not about which reason claims a handle.
	//
	// OFFERED WHERE ITS PRECEDENCE SITS, a lead who had also spoken in the
	// thread would be claimed as the FALLBACK — and [Route] drops a
	// fallback the moment anybody else is reached, so the one person the
	// room was addressed to would be the only one not woken.
	c.add(ReasonLeadFallback, 0, r.arm(ReasonLeadFallback))
	return c.out, c.truncated
}

// Candidates is every handle a record says its message concerned, in
// precedence order.
//
// PURE OVER THE RECORD, and it returns nothing for a nil notification — which
// is the single rule that keeps the two surfaces in step: a quiet record (an
// import, a system line, a reaction) carries no [Notify], so the parser routes
// nobody for exactly the records the writer meant to wake nobody with.
//
// # Why it re-applies the caps and the precedence to a set that already has them
//
// Because the record may have been written by a NEWER BUILD. The write path's
// own set is already deduplicated and capped, so every branch below is a no-op
// for anything this build wrote — and for a record a newer peer wrote, a wake
// set is a TURN BUDGET rather than a row: applying what the fleet accepted is
// the rule for rows, and spending this node's whole concurrency budget on a
// record that named two hundred seats is not something an older build has to
// agree to.
func Candidates(n *Notify) []Candidate {
	if n == nil {
		return nil
	}
	byReason := make(map[Reason][]string, len(Reasons))
	var unknown []Candidate
	for _, r := range n.Recipients {
		if !r.Reason.Valid() {
			// A REASON THIS BUILD DOES NOT KNOW IS STILL A WAKE. A
			// newer build routed this handle deliberately, and
			// dropping it would make a rolling upgrade a period in
			// which some people are simply not told. It is carried
			// unaddressed — [Reason.Addressed] is false for an
			// unknown reason — which is the conservative half: a
			// seat may absorb it rather than having to answer a
			// question this build cannot read.
			unknown = append(unknown, Candidate{Handle: r.Handle, Reason: r.Reason})
			continue
		}
		byReason[r.Reason] = append(byReason[r.Reason], r.Handle)
	}

	c := &collector{}
	for _, reason := range routedReasons() {
		c.add(reason, limitFor(reason), byReason[reason])
	}
	for _, cand := range unknown {
		c.add(cand.Reason, 0, []string{cand.Handle})
	}
	// THE FALLBACK ARM IS THE RECORD'S OWN PLUS THE LEAD IT NAMED, and the
	// second half is what makes the parser's re-route worth running: a
	// record whose ordinary recipients have all left the company since it
	// was written still reaches the lead, because the lead rides the
	// record and this offers it again under today's roster.
	c.add(ReasonLeadFallback, 0, append(slices.Clone(byReason[ReasonLeadFallback]),
		leadOf(n.ChannelKind, n.AuthorKind, n.Lead)...))
	return c.out
}

// Route is the post-filter: who is actually WOKEN.
//
// Four steps, in this order, and each one is a different question:
//
//  1. Drop handles the roster no longer knows. A seat that was removed from
//     the org chart is a wake nothing can run.
//  2. Drop PEOPLE. A human seat is ADDRESSABLE and never woken: a wake runs a
//     TURN, and routing one to a person would spend a model call answering on
//     their behalf. This is stated HERE rather than left to the notification
//     spine to refuse downstream, because the record carries the routed set —
//     so a person left in it is a wake this package asked for and a later
//     layer happens to decline.
//  3. Drop the actor. Telling somebody what they just said is a round a model
//     spends on nothing, and there is no reason here that wakes its own author
//     — a chat message has no equivalent of the tracker's unblocked notice,
//     which is about somebody else's task.
//  4. If no ordinary candidate survived, keep the fallback and only the
//     fallback. The other two conditions on it — a unit room, and a person or
//     an operator speaking — are facts about the RECORD, so they are applied
//     where the candidate is offered rather than here.
//
// A NIL PREDICATE ROUTES NOBODY. It cannot tell a seat this company still has
// from one it does not, nor a person from an agent, and both answers are
// required — so the honest response to having neither is the empty set rather
// than waking everybody the record named.
func Route(candidates []Candidate, known func(handle string) (exists, human bool),
	actor string) []Candidate {

	if known == nil {
		return nil
	}
	actor = strings.TrimSpace(actor)
	var ordinary, fallback []Candidate
	for _, c := range candidates {
		if c.Handle == "" || c.Handle == actor {
			continue
		}
		exists, human := known(c.Handle)
		if !exists || human {
			continue
		}
		if c.Reason == ReasonLeadFallback {
			fallback = append(fallback, c)
			continue
		}
		ordinary = append(ordinary, c)
	}
	if len(ordinary) > 0 {
		return ordinary
	}
	if len(fallback) == 0 {
		return nil
	}
	// THE FIRST SURVIVING FALLBACK AND NOTHING ELSE. There is one lead per
	// room, so the slice holds more than one entry only when a record
	// named a lead and carried another, and waking both would make the
	// absence of every other reason reach more people than a mention does.
	return fallback[:1]
}

// RecipientsOf renders a routed candidate set as the record carries it.
func RecipientsOf(candidates []Candidate) []Recipient {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]Recipient, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, Recipient{Handle: c.Handle, Reason: c.Reason})
	}
	return out
}

// arm is the handles one reason names, read off the room's own rows.
//
// THE SWITCH IS THE ROUTING POLICY, all of it, in the vocabulary's own order.
// A reason with no arm here names nobody, which is why the switch has no
// default: a reason added to [Reasons] without a source is a wake nothing
// produces, and that is a hole the compiler cannot see but a reader can.
func (r Routing) arm(reason Reason) []string {
	switch reason {
	case ReasonDM:
		// EVERY PARTICIPANT, because a direct conversation exists only
		// between these parties: there is nothing in it that is not
		// addressed to all of them.
		if !r.ChannelKind.Direct() {
			return nil
		}
		return memberHandles(r.Members)

	case ReasonMention:
		return r.mentioned()

	case ReasonReplyToOwnRoot:
		if r.ThreadRoot == "" {
			return nil
		}
		return []string{r.RootAuthor}

	case ReasonReply:
		if r.ThreadRoot == "" {
			return nil
		}
		return r.ThreadParticipants

	case ReasonCollective:
		if !r.Collective {
			return nil
		}
		return memberHandles(r.Members)

	case ReasonFollow:
		if r.ThreadRoot == "" {
			return nil
		}
		return r.Followers

	case ReasonFollowAll:
		out := make([]string, 0, len(r.Members))
		for _, m := range r.Members {
			if m.FollowAll {
				out = append(out, m.Handle)
			}
		}
		return out

	case ReasonLeadFallback:
		return leadOf(r.ChannelKind, r.AuthorKind, r.Lead)
	}
	return nil
}

// mentioned is the mention arm, and the one arm a room's PRIVACY narrows.
//
// A MENTION WAKES THE SEAT IT NAMED, which is the whole point of typing one —
// except in a room whose transcript is reachable only through its membership,
// where a wake would send a seat to open a conversation it cannot read. In a
// public or a unit room a mention reaches a seat that has not joined, because
// there is nothing to join: the room is readable by everybody, and a mention
// that woke nobody would make naming a colleague in the company's open rooms a
// gesture with no effect at all.
func (r Routing) mentioned() []string {
	if !privateRoom(r.ChannelKind) {
		return r.Mentions
	}
	members := make(map[string]bool, len(r.Members))
	for _, m := range r.Members {
		members[m.Handle] = true
	}
	out := make([]string, 0, len(r.Mentions))
	for _, h := range r.Mentions {
		if members[h] {
			out = append(out, h)
		}
	}
	return out
}

// leadOf is the lead fallback's own two conditions, in ONE place because both
// halves of the file need them and neither may answer differently.
//
// A UNIT ROOM, AND A PERSON OR AN OPERATOR SPEAKING. The first is what a lead
// even means: a room nobody's org chart names has no lead to fall back to. The
// second is what stops a company talking to itself — an agent's post that
// reached nobody has reached exactly who it should, while a person who posted
// in a unit's room and was answered by silence is somebody talking to an empty
// screen.
func leadOf(kind Kind, author AuthorKind, lead string) []string {
	if lead == "" || kind != KindUnit {
		return nil
	}
	if author != AuthorHuman && author != AuthorOperator {
		return nil
	}
	return []string{lead}
}

// routedReasons is [Reasons] without the fallback, in the vocabulary's order.
//
// DERIVED FROM THE SLICE rather than typed again, so a reason added to the
// vocabulary is walked by both halves of this file without anybody
// remembering to add it — the failure of a forgotten arm is a wake nothing
// produces and nothing reports.
func routedReasons() []Reason {
	out := make([]Reason, 0, len(Reasons))
	for _, r := range Reasons {
		if r != ReasonLeadFallback {
			out = append(out, r)
		}
	}
	return out
}

// limitFor is the cap on one arm, and 0 means the arm is bounded only by
// [MaxRecipients].
//
// THREE ARMS CARRY THEIR OWN, and each bounds a different unbounded thing: the
// handles a body may name, the voices a thread may have collected, and the
// members one `@channel` may reach. The rest — a direct conversation's
// participants, a thread's followers, a room's full followers — are bounded by
// the total, which is exactly what [MaxRecipients] is the sum for.
func limitFor(reason Reason) int {
	switch reason {
	case ReasonMention:
		return MaxMentions
	case ReasonReply:
		return MaxThreadParticipants
	case ReasonCollective:
		return MaxCollectiveRecipients
	}
	return 0
}

// collector applies the two rules every arm passes through: one reason per
// handle, and the caps.
type collector struct {
	seen      map[string]bool
	out       []Candidate
	truncated bool
}

// add offers one arm's handles under one reason.
//
// A CAP THAT BITES SETS [collector.truncated] RATHER THAN CUTTING SILENTLY.
// The record carries that flag, so a room can see that a message reached fewer
// people than it named — which is the difference between a broadcast that was
// bounded and one that quietly was not.
func (c *collector) add(reason Reason, limit int, handles []string) {
	taken := 0
	for _, handle := range handles {
		handle = strings.TrimSpace(handle)
		if handle == "" || c.seen[handle] {
			// ALREADY CLAIMED IS NOT TRUNCATION. A handle a stronger
			// reason took is still being woken, so counting it
			// against this arm's cap — or reporting it as somebody
			// the message failed to reach — would be a lie in both
			// directions.
			continue
		}
		if len(c.out) >= MaxRecipients || (limit > 0 && taken >= limit) {
			c.truncated = true
			return
		}
		if c.seen == nil {
			c.seen = make(map[string]bool, MaxRecipients)
		}
		c.seen[handle] = true
		c.out = append(c.out, Candidate{Handle: handle, Reason: reason})
		taken++
	}
}
