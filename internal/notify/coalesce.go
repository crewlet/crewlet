package notify

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// Coalescing: N events in one conversation become ONE trigger.
//
// The companion of the broker's batched delivery. A seat's pending inbox is
// partitioned by conversation key, and a partition with more than one event is
// merged here — so five comments on one issue, or a person typing four
// messages in a row, cost one turn instead of five.
//
// What makes it safe is that the merged event is shaped so every existing
// consumer keeps working unchanged. The flat fields mirror the LATEST
// constituent, whose metadata carries the pointers a reply needs; the full
// list is carried alongside for the workers that must observe every sender;
// and the two fields that bound what a turn may do — recon and delegation
// depth — merge conservatively rather than by taking the latest.

// Coalesce merges same-conversation notifications into one trigger.
//
// A single event is returned unchanged, which is the overwhelmingly common
// case and must stay exactly the pre-coalescing path: a partition of one that
// went through the merge would gain a digest header describing one event.
func Coalesce(prompts Prompts, evs []types.ExternalNotification, at []time.Time) (types.ExternalNotification, bool) {
	if len(evs) == 0 {
		return types.ExternalNotification{}, false
	}
	ordered := chronological(evs, at)
	latest := ordered[len(ordered)-1].event
	if len(ordered) == 1 {
		return latest, true
	}
	prompt := prompts.For(latest.NotificationSource)
	superseded := laterDuplicates(prompt, ordered)

	merged := latest
	merged.Body = digest(prompt, ordered, superseded)
	merged.SalientBody = mergedSalient(prompt, ordered, superseded)
	merged.Messages = constituents(ordered)
	// CONSERVATIVE, both of them, and for the same reason: a merge must not
	// be able to launder a constraint. Recon is required when ANY
	// constituent required it — a digest containing one bare pointer is
	// still a trigger the seat has to go and look behind.
	merged.ContextRequiresRecon = slices.ContainsFunc(ordered, func(c constituent) bool {
		return c.event.ContextRequiresRecon
	})
	return merged, true
}

// constituent is one event with the timestamp it is ordered by.
//
// Carried alongside rather than read off the event because the ORDER is the
// caller's fact: the events arrive from a broker partition, and their envelope
// timestamps are what put them in sequence. A merge that sorted by something
// on the payload would reorder a conversation whenever a vendor stamped its
// own clock.
type constituent struct {
	event types.ExternalNotification
	at    time.Time
}

// chronological pairs each event with its timestamp and sorts.
//
// A STABLE sort, so two events sharing a timestamp — which a vendor emitting a
// burst routinely produces — keep the order the broker delivered them in.
// Re-ordering them would rewrite the conversation on nothing but a tie.
func chronological(evs []types.ExternalNotification, at []time.Time) []constituent {
	out := make([]constituent, 0, len(evs))
	for i, ev := range evs {
		c := constituent{event: ev}
		if i < len(at) {
			c.at = at[i]
		}
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b constituent) int { return a.at.Compare(b.at) })
	return out
}

// digest renders the merged body: the earlier constituents in order, then the
// latest one in full.
//
// The latest LAST and complete, because a vendor's prompt scaffolding — its
// triage rules, its "how to get full context" block — is built into that body.
// Rendering it once, from the most recent state, is what keeps a five-event
// digest from repeating the same boilerplate five times with progressively
// staler pointers in it.
func digest(prompt Prompt, ordered []constituent, superseded map[int]bool) string {
	var b strings.Builder
	b.WriteString("## Coalesced updates (" + strconv.Itoa(len(ordered)) +
		" events in this conversation)\n")
	b.WriteString("Multiple events for this conversation arrived in quick succession " +
		"(or while you were busy). They are listed chronologically below; the MOST " +
		"RECENT event is rendered in full afterwards. Handle them as ONE piece of " +
		"work — read everything before acting, and reply at most once.\n\n")
	for i, c := range ordered[:len(ordered)-1] {
		b.WriteString(digestLine(prompt, c, superseded[i]) + "\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString(ordered[len(ordered)-1].event.Body)
	return b.String()
}

// digestLine renders one earlier constituent as a bullet.
//
// NO LENGTH CAP on the body. The seat reads the digest as its trigger, so
// truncating it hides content it may have to act on — and the one line that
// mattered is as likely to be at the end of a long message as the start.
// Continuation lines are indented so a multi-line body stays inside its
// bullet rather than reading as the next one.
func digestLine(prompt Prompt, c constituent, duplicate bool) string {
	sender := c.event.Sender
	if sender == "" {
		sender = "(unknown sender)"
	}
	when := c.event.Metadata["timestamp"]
	if when == "" && !c.at.IsZero() {
		when = c.at.UTC().Format(time.RFC3339)
	}
	label := "- **" + sender + "** (" + when
	if kind := eventTypeOf(c.event); kind != "" {
		label += ", " + kind
	}
	label += ")"

	if duplicate {
		return label + ": (no new content — identical to a later update)"
	}
	body := strings.TrimSpace(effectiveBody(prompt, c.event))
	if body == "" {
		return label + ": (no message body — superseded by later state)"
	}
	return label + ": " + strings.ReplaceAll(body, "\n", "\n  ")
}

// effectiveBody is the body a constituent contributes, per the source's
// supersede rule.
//
// ONE resolver for the digest and the merged salient text, so the trigger the
// planner reads and the text the learning workers embed can never say
// different things about what was said.
func effectiveBody(prompt Prompt, ev types.ExternalNotification) string {
	return prompt.DigestBody(eventTypeOf(ev), rawBody(ev))
}

// eventTypeOf prefers the vendor's own metadata over the envelope field.
//
// A parser that carries a finer-grained type in metadata knows more than the
// envelope's coarse one, and the supersede rules are written against the finer
// name.
func eventTypeOf(ev types.ExternalNotification) string {
	if kind := ev.Metadata["event_type"]; kind != "" {
		return kind
	}
	return ev.SourceEventType
}

// rawBody honours the salient-body contract.
//
// nil means the producer emitted no distinct raw message — an extension
// without a prompt builder, for which the body IS the message — so it falls
// back. An EMPTY STRING is a genuinely contentless message and must not fall
// back, or the digest fills with the scaffolding the salient body exists to
// strip.
func rawBody(ev types.ExternalNotification) string {
	if ev.SalientBody != nil {
		return *ev.SalientBody
	}
	return ev.Body
}

// laterDuplicates finds constituents a later one from the same sender repeats.
//
// Webhook sources re-emit unchanged state on every event — a code host sends
// the whole pull-request description each time a label moves — and rendering N
// identical copies buries the one actionable line and pollutes the merged text
// the learning filters embed.
//
// SAME SENDER and byte-identical, both required. The later copy still renders,
// in the digest or in full as the latest, so nothing is lost; and two
// different people each saying "+1" are two facts, not one repeated.
func laterDuplicates(prompt Prompt, ordered []constituent) map[int]bool {
	bodies := make([]string, len(ordered))
	for i, c := range ordered {
		bodies[i] = strings.TrimSpace(effectiveBody(prompt, c.event))
	}
	duplicates := map[int]bool{}
	for i := range ordered {
		if bodies[i] == "" {
			continue
		}
		for j := i + 1; j < len(ordered); j++ {
			if bodies[j] == bodies[i] && ordered[j].event.Sender == ordered[i].event.Sender {
				duplicates[i] = true
				break
			}
		}
	}
	return duplicates
}

// mergedSalient is the sender-attributed raw text of the whole conversation.
//
// What the learning workers embed, so it carries the same supersede rule the
// digest does — a body the source superseded contributes nothing here either.
// It is a POINTER because the field distinguishes nil from empty: a merge
// always produces a salient body, even an empty one, and letting it read as
// nil would send a worker back to the scaffolding-laden digest.
func mergedSalient(prompt Prompt, ordered []constituent, superseded map[int]bool) *string {
	var lines []string
	for i, c := range ordered {
		if superseded[i] {
			continue
		}
		body := strings.TrimSpace(effectiveBody(prompt, c.event))
		if body == "" {
			continue
		}
		sender := c.event.Sender
		if sender == "" {
			sender = "(unknown sender)"
		}
		lines = append(lines, sender+": "+body)
	}
	merged := strings.Join(lines, "\n\n")
	return &merged
}

// constituents is every event in the conversation, at full fidelity.
//
// The flat fields mirror only the latest, which is right for replying but
// wrong for learning: a worker that observed only the last sender would never
// see the four people who spoke before them.
func constituents(ordered []constituent) []types.CoalescedMessage {
	out := make([]types.CoalescedMessage, 0, len(ordered))
	for _, c := range ordered {
		out = append(out, types.CoalescedMessage{
			Sender:               c.event.Sender,
			Body:                 rawBody(c.event),
			Timestamp:            c.at,
			SourceEventType:      c.event.SourceEventType,
			Metadata:             c.event.Metadata,
			ContextRequiresRecon: c.event.ContextRequiresRecon,
		})
	}
	return out
}
