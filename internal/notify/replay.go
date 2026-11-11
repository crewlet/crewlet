package notify

import (
	"context"
	"encoding/json"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE INBOUND GROUP'S PASSED WAKES, replayed at a node's start.
//
// A delivery whose record a newer build wrote is PASSED by a node that cannot
// read it ([Deferral], [queue.Pass]), and nothing steers the pass to a node that
// can: the broker hands each return to whichever member of the group asks next,
// so every one of its 25 deliveries can go to older nodes — early in a roll,
// one node of the newer build among many older ones. One that no node handled
// in the ten minutes those deliveries span is dead-lettered with its copy, under
// the header naming its pass and its reason ([queue.PassedLetter]). That dead
// letter, and this replay, are what deliver it; the group's deliveries only
// usually do.
//
// # Why at the start
//
// What lets a node read a newer record is a newer build, which arrives only
// with a restart — the change feed's reason for replaying there too
// (internal/changefeed's Feed.replay). So [Service.Start] lists the group's
// dead letters once, after it subscribes, and handles each copy a pass for a
// reason this build names retired ([DeferReason]) through [Service.Handle], the
// parse and the delivery a live delivery runs:
//
//   - a copy this node handles — its wakes delivered, or parsed to nobody, or
//     settled as any delivery is — is DROPPED;
//   - one this node passes too (its build is older still), one whose wake was
//     not delivered, and one handed back while this node sheds are KEPT for a
//     later start;
//   - one whose copy could not be read, or reads as no event, is KEPT, never
//     dropped by a node that could not judge it; and one already gone was
//     settled by whoever took it.
//
// Every other entry the group's subject holds — a failure's poison, which no
// header marks, or a pass for a reason a newer build names — is left alone.
//
// # Two nodes starting together may both deliver one copy, and that is safe
//
// Every parser that defers a newer record derives its wakes' ids from the
// record's change and the recipient ([Routed.WakeID], changefeed.WakeID), so a
// second copy of a wake carries the first's id, and its seat collapses it as it
// collapses any redelivered wake: within one drain by the inbox's same-id
// dedupe (internal/agent/inbox's Screen), and across drains by the fleet's
// completion ledger (internal/engine's Dispatcher, which drops a worked
// trigger). A held wake is recorded under an id derived from that one
// ([HeldID]), and collapses alike.
//
// # What waits
//
// A copy kept after the last start of a node that reads it waits for the next
// such start, kept for the dead-letter stream's retention
// (`stream.event_retention_hours`): a roll paused with one node of the newer
// build among older ones delivers its kept copies as the roll goes on, at each
// next node's start.

// DeadLetterSubject is where the inbound group's dead letters are kept, its
// passed wakes among them, and where a node's start replays them from:
// [topics.DeadLetter] of [topics.NotificationsInbound] and [InboundGroup].
func DeadLetterSubject() string {
	return topics.DeadLetter(topics.NotificationsInbound, InboundGroup)
}

// letterFate is what one replay made of one passed copy.
type letterFate int

const (
	// letterKept is a copy the replay left for a later start.
	letterKept letterFate = iota
	// letterDropped is a copy the replay handled and dropped.
	letterDropped
	// letterGone is a copy gone before the replay read it.
	letterGone
)

// replay handles every passed copy the inbound group's dead letters hold, once,
// at the start: see this file's head for what becomes of each.
func (s *Service) replay(ctx context.Context) {
	subject := DeadLetterSubject()
	kept, err := s.queue.DeadLettersOn(ctx, subject)
	if err != nil {
		log.WarnContext(ctx, "inbound_dead_letters_unread", "subject", subject,
			"error", err.Error(),
			"detail", "the inbound group's dead letters could not be listed, so no passed "+
				"wake was replayed now; they are listed again at the next start, and kept "+
				"until a node replays them or their retention lets them go")
		return
	}
	fates := map[letterFate]int{}
	passed := 0
	for _, entry := range kept {
		reason, ok := queue.PassedReason(entry.Header)
		if !ok || !DeferReason(reason).Valid() {
			continue
		}
		passed++
		fates[s.replayOne(ctx, subject, entry, DeferReason(reason))]++
	}
	if passed > 0 {
		log.InfoContext(ctx, "inbound_dead_letters_replayed", "subject", subject,
			"passed", passed, "replayed", fates[letterDropped], "kept", fates[letterKept],
			"gone", fates[letterGone])
	}
}

// replayOne handles one passed copy, kept on subject as entry, as this node
// handles a delivery, and answers what became of it.
func (s *Service) replayOne(ctx context.Context, subject string, entry queue.Kept,
	reason DeferReason) letterFate {

	frame, held, err := s.queue.DeadLetterFrame(ctx, subject, entry.Seq)
	switch {
	case err != nil:
		log.WarnContext(ctx, "inbound_dead_letter_frame_unread", "subject", subject,
			"seq", entry.Seq, "passed", string(reason), "error", err.Error(),
			"detail", "kept, and replayed at the next start: its copy could not be read")
		return letterKept
	case !held:
		// GONE SINCE THE LISTING — a peer's replay dropped it, or the
		// retention let it go: whoever took it settled it.
		log.DebugContext(ctx, "inbound_dead_letter_gone", "subject", subject, "seq", entry.Seq)
		return letterGone
	}
	var ev events.Event
	if err := json.Unmarshal(frame, &ev); err != nil {
		log.WarnContext(ctx, "inbound_dead_letter_unreadable", "subject", subject,
			"seq", entry.Seq, "passed", string(reason), "error", err.Error(),
			"detail", "kept for a build that reads it: its copy reads as no event here")
		return letterKept
	}
	res := s.Handle(ctx, &ev)
	switch res.Outcome {
	case queue.OutcomeAck:
		if err := s.queue.DropDeadLetter(ctx, subject, entry.Seq); err != nil {
			log.WarnContext(ctx, "inbound_dead_letter_drop_failed", "subject", subject,
				"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "error", err.Error(),
				"detail", "handled and kept: the next start handles it again, and its "+
					"wakes' derived ids collapse the second copy")
			return letterKept
		}
		log.InfoContext(ctx, "inbound_dead_letter_replayed", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "passed", string(reason))
		return letterDropped
	case queue.OutcomePass:
		log.InfoContext(ctx, "inbound_dead_letter_still_deferred", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "passed", res.Reason,
			"detail", "this node cannot read it either; kept for a node that can")
	default:
		var met string
		if res.Err != nil {
			met = res.Err.Error()
		}
		log.WarnContext(ctx, "inbound_dead_letter_not_replayed", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "passed", string(reason),
			"outcome", res.Outcome.String(), "reason", res.Reason, "error", met,
			"detail", "kept, and replayed at the next start")
	}
	return letterKept
}
