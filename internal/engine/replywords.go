package engine

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// replyOf is a delivery offered to a parked run of the seat self as the
// answer to its question: where it came from, what it says ([replyWords]),
// who wrote that and how far the run may rely on it ([replyLabel]), which the
// run reads it fenced under, and whom its writer wrote it for
// ([replyWorkingFor]).
func replyOf(prompts notify.Prompts, parties notify.Parties, self string,
	conv sandbox.ConversationRef, evs []*events.Event) sandbox.Reply {

	return sandbox.Reply{Conv: conv, Text: replyWords(evs),
		Label: replyLabel(prompts, parties, self, evs), Events: evs,
		Writer: replyWriter(parties, evs), Says: replySays(prompts, parties, self, evs),
		WorkingFor: replyWorkingFor(evs)}
}

// newestEvent is the newest of a delivery's events, the one its writer and
// whom it was written for are read off ([replyWriter], [replyWorkingFor]):
// after the partition function every event of a delivery has the same writer
// and the same carrier.
func newestEvent(evs []*events.Event) *events.Event {
	var newest *events.Event
	for _, ev := range evs {
		if ev != nil && (newest == nil || !ev.Timestamp.Before(newest.Timestamp)) {
			newest = ev
		}
	}
	return newest
}

// replyWorkingFor is whom a delivery offered to a parked run was written for
// ([sandbox.Reply.WorkingFor]): the newest event's carrier, read by the rule a
// wake is read by ([writtenForIn]), so the two cannot drift — an agent's
// carrier, off the first constituent where the notification coalesced, and the
// engine's on its record about an agent's earlier act; nil for a person's or a
// credential's writer and for a vendor's notification. The engine's delivery
// answers no parked question whatever it carries, since it says nothing and
// its writer holds no seat ([sandbox.Reply.Writer]). Read whether or not there
// is a party registry: whom a write was made for is stated on the write, never
// looked up.
func replyWorkingFor(evs []*events.Event) *types.WorkingFor {
	n, ok := events.DataAs[*types.ExternalNotification](newestEvent(evs))
	if !ok || n == nil {
		return nil
	}
	return writtenForIn(n)
}

// replySays is whether a delivery carries words its writer addressed to the
// conversation ([sandbox.Reply.Says]): one of its notifications read from a
// surface a writer speaks on, or one no prompt classifies; never a field change,
// an alarm or a pickup.
func replySays(prompts notify.Prompts, parties notify.Parties, self string, evs []*events.Event) bool {
	for _, ev := range evs {
		n, ok := events.DataAs[*types.ExternalNotification](ev)
		if !ok || n == nil {
			continue
		}
		labels := notify.SalientLabels(prompts, *n, parties, self)
		if len(labels) > 0 && !slices.ContainsFunc(labels, func(l provenance.Label) bool {
			return !l.Source.Said() && l.Source != provenance.SourceNotification
		}) {
			return true
		}
	}
	return false
}

// replyWriter is the seat the writer of a delivery offered to a parked run
// holds, through the party registry ([sandbox.Reply.Writer]), or "": the
// newest event's writer, as the coordinator records the answer as coming
// from it, which after the partition function is the delivery's only one.
//
// A first-party notice's writer by the kind its record names (a seat's or a
// person's write by the handle the registry still holds), and anything else by
// the actor its parser stamped, else its sender, in that source's own
// namespace, as the interactions resolve a sender ([writerSubject],
// [subjectOf]). NEVER THE
// DELIVERY'S WORDS, and never a credential acting as no seat or the engine
// itself, which hold no seat. A registry withholds a suspended or removed
// holder's identities, so a vendor's chat from them resolves to nobody here;
// whoever holds a seat this does name is asked again when a run would take the
// reply ([audienceResolver.MayAnswer]).
func replyWriter(parties notify.Parties, evs []*events.Event) string {
	n, ok := events.DataAs[*types.ExternalNotification](newestEvent(evs))
	if !ok || n == nil || parties == nil {
		return ""
	}
	if w := n.Writer; w != nil {
		if w.Kind != iam.ActorAgent && w.Kind != iam.ActorHuman {
			return ""
		}
		if party, ok := parties.ByHandle(strings.TrimSpace(w.Seat)); ok {
			return party.Handle
		}
		return ""
	}
	actor := strings.TrimSpace(n.Metadata[notify.ActorField])
	if actor == "" {
		// AN ENGINE-AUTHORED NOTICE stamps no actor, and its sender is then
		// all there is, as [subjectOf] reads it for the turn's requester:
		// the reply and the turn are written by the same somebody.
		actor = strings.TrimSpace(n.Sender)
	}
	if actor == "" {
		return ""
	}
	if party, ok := parties.ByExternalID(n.NotificationSource, actor); ok {
		return party.Handle
	}
	return ""
}

// replyWords is what a delivery offered to a parked run as its answer SAYS
// ([sandbox.Reply.Text]): each event's words as the turn that would otherwise
// have worked it records what it was asked ([eventAsk], with no fence), a
// chat message exactly as it was typed, oldest first, a paragraph each.
// Where the delivery holds more than one writer's words, each event's are
// put under the name its writer posted as, the way a coalesced burst's
// salient text already names each of its senders, so no writer's words are
// recorded as the newest writer's alone, which is who the answer is recorded
// as coming from.
//
// NEVER THE TRIGGER ([DescribeTrigger]). That is the prompt the delivery would
// wake the seat with: the surface's triage guidance, how to reply, the ids to
// act on, written for the seat's own turn. The answer is sealed onto the run's
// row, announced as `executor_answered`'s `answer`, read back to the person
// who gave it among their answered decisions (ADR-0068), and handed to the run
// as the answer to its question. Recorded as the trigger, a person was shown a
// seat's instructions as their own reply, and the run was handed them as the
// person's answer.
func replyWords(evs []*events.Event) string {
	type said struct {
		writer, words string
		// named is a coalesced burst, whose words already name each
		// sender.
		named bool
	}
	var (
		lines   []said
		writers []string
	)
	wrote := func(writer string) {
		if !slices.Contains(writers, writer) {
			writers = append(writers, writer)
		}
	}
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		part, ok := eventAsk(notify.Prompts{}, nil, "", ev)
		words := strings.TrimSpace(prefetch.Ask{part}.Text())
		if !ok || words == "" {
			continue
		}
		line := said{writer: ev.Actor(), words: words}
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil && len(n.Messages) > 0 {
			line.named = true
			for _, m := range n.Messages {
				wrote(m.Sender)
			}
		} else {
			wrote(line.writer)
		}
		lines = append(lines, line)
	}
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(writers) > 1 && !line.named {
			line.words = line.writer + ": " + line.words
		}
		parts = append(parts, line.words)
	}
	return strings.Join(parts, "\n\n")
}

// replyLabel is who wrote what a reply says, for the seat self: the label the
// fence around each of the delivery's bodies carries in the trigger it was
// delivered in ([notify.SalientLabel]), merged as one reply by the one rule
// for several labels over one text ([provenance.Merge]): every writer once, at
// the least trust any of them earns, under the surface they share, or an
// integration's notification where they differ.
//
// THE TRIGGER'S OWN LABEL, so the run reads a reply exactly as it would have
// read the same message waking its turn: a colleague's words as a colleague's,
// a stranger's as data. NIL where the delivery holds words that are no
// notification's, which no surface labelled, and which the run then reads
// defused.
func replyLabel(prompts notify.Prompts, parties notify.Parties, self string,
	evs []*events.Event) *provenance.Label {

	var labels []provenance.Label
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		part, asks := eventAsk(prompts, parties, self, ev)
		if !asks || strings.TrimSpace(prefetch.Ask{part}.Text()) == "" {
			continue
		}
		n, ok := events.DataAs[*types.ExternalNotification](ev)
		if !ok || n == nil {
			return nil
		}
		labels = append(labels, notify.SalientLabel(prompts, *n, parties, self))
	}
	if len(labels) == 0 {
		return nil
	}
	merged := provenance.Merge(labels...)
	return &merged
}

// parties is [Dispatcher.Parties]' registry, or none.
func (d *Dispatcher) parties() notify.Parties {
	if d.Parties == nil {
		return nil
	}
	return d.Parties()
}
