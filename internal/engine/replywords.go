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
// answer to its question: what it says ([replyWords]), the label the run reads
// it fenced under ([replyLabel]), its writer, and whom it was written for
// ([replyWorkingFor]).
func replyOf(prompts notify.Prompts, parties notify.Parties, self string,
	conv sandbox.ConversationRef, evs []*events.Event) sandbox.Reply {

	return sandbox.Reply{Conv: conv, Text: replyWords(evs),
		Label: replyLabel(prompts, parties, self, evs), Events: evs,
		Writer: replyWriter(parties, evs), Says: replySays(prompts, parties, self, evs),
		WorkingFor: replyWorkingFor(evs)}
}

// newestEvent is the newest of a delivery's events, which [replyWriter] and
// [replyWorkingFor] read: after the partition function every event of a
// delivery has the same writer and carrier.
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
// ([sandbox.Reply.WorkingFor]): the newest event's carrier, read by the wake's
// own rule ([writtenForIn]) so the two cannot drift. The engine's delivery
// answers no parked question whatever it carries, its writer holding no seat
// ([sandbox.Reply.Writer]). No party registry is needed: whom a write was made
// for is stated on the write, never looked up.
func replyWorkingFor(evs []*events.Event) *types.WorkingFor {
	n, ok := events.DataAs[*types.ExternalNotification](newestEvent(evs))
	if !ok || n == nil {
		return nil
	}
	return writtenForIn(n)
}

// replySays is whether a delivery carries words its writer addressed to the
// conversation ([sandbox.Reply.Says]): a notification from a surface a writer
// speaks on, or one no prompt classifies; never a field change, an alarm or a
// pickup.
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

// replyWriter is the seat the newest event's writer holds, through the party
// registry ([sandbox.Reply.Writer]), or "": the coordinator records the answer
// as coming from it.
//
// A first-party notice's writer by its record (a seat's or person's handle the
// registry still holds); anything else by the actor its parser stamped, else
// its sender, in that source's namespace ([writerSubject], [subjectOf]). NEVER
// THE DELIVERY'S WORDS, and never a credential acting as no seat or the engine.
// A registry withholds a suspended or removed holder, so their vendor chat
// resolves to nobody; a seat named here is asked again when a run would take
// the reply ([audienceResolver.MayAnswer]).
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
		// AN ENGINE-AUTHORED NOTICE stamps no actor; its sender stands in, as
		// [subjectOf] reads the turn's requester, so reply and turn agree.
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
// ([sandbox.Reply.Text]): each event's words as [eventAsk] records them
// unfenced, a chat message as typed, oldest first, a paragraph each. With more
// than one writer, each event's words go under its writer's posted name (as a
// coalesced burst's already do), since the answer is recorded as the newest
// writer's.
//
// NEVER THE TRIGGER ([DescribeTrigger]), the seat's wake prompt with its
// triage guidance and ids: the answer is sealed on the run's row, announced as
// `executor_answered`'s `answer`, shown to the person among their answered
// decisions (ADR-0068) and handed to the run, so the trigger would show a
// seat's instructions as the person's own reply.
func replyWords(evs []*events.Event) string {
	type said struct {
		writer, words string
		// named is a coalesced burst, whose words already name each sender.
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

// replyLabel is who wrote what a reply says, for the seat self: each body's
// label in its trigger ([notify.SalientLabel]), merged by [provenance.Merge]
// (every writer once, at the least trust any earns).
//
// THE TRIGGER'S OWN LABEL, so the run reads a reply as it would have read the
// same message waking its turn: a colleague's words as a colleague's, a
// stranger's as data. NIL where the delivery holds words no notification
// carries, which no surface labelled and the run then reads defused.
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
