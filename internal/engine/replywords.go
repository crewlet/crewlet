package engine

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// replyWords is what a delivery offered to a parked run as its answer SAYS
// ([sandbox.Reply.Text]): each event's words as the turn that would otherwise
// have worked it records what it was asked ([eventAsk]), a chat message
// exactly as it was typed, oldest first, a paragraph each. Where the delivery
// holds more than one writer's words, each event's are put under the name its
// writer posted as, the way a coalesced burst's salient text already names
// each of its senders, so no writer's words are recorded as the newest
// writer's alone, which is who the answer is recorded as coming from.
//
// NEVER THE TRIGGER ([DescribeTrigger]). That is the prompt the delivery would
// wake the seat with: the surface's triage guidance, how to reply, the ids to
// act on, written for the seat's own turn. The answer is sealed onto the run's
// row, announced as `sandbox_run_answered`'s `answer`, read back to the person
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
		words := strings.TrimSpace(eventAsk(ev))
		if words == "" {
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
