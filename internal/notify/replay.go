package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE INBOUND GROUP'S UNHANDLED WAKES, replayed at a node's start and while it
// runs.
//
// A delivery whose record a newer build wrote is PASSED by a node that cannot
// read it ([Deferral], [queue.Pass]), and nothing steers the pass to a node that
// can: the broker hands each return to whichever member of the group asks next,
// so every one of its 25 deliveries can go to older nodes — early in a roll,
// one node of the newer build among many older ones. One that no node handled
// in the ten minutes those deliveries span is dead-lettered with its copy on
// the group's UNHANDLED subject ([queue.RetiredTo]), and so is one whose last
// delivery a node handed back — it shed, or stopped — after the passes before
// it: a hand-back is no failure either. That dead letter, and this replay, are
// what deliver it; the group's deliveries only usually do.
//
// # At the start, and whenever the subject moves
//
// What lets a node read a newer record is a newer build, which arrives only
// with a restart — the change feed's reason for replaying there (internal/
// changefeed's Feed.replay). But a node of that build ALREADY RUNNING reads
// every copy kept after its start as well, and a roll paused with one newer
// node among older ones keeps one every ten minutes or so: replayed only at a
// start, each waited for a restart that a paused roll never brings, and expired
// with its wake. So [Service.Start] begins, after it subscribes, a goroutine of
// its own that [Service.Stop] ends, which replays the group's unhandled subject
// once at once and then again whenever the subject moves — its count of
// entries, asked of the broker every [replayPoll], one request however many it
// holds ([queue.EventQueue.DeadLetterMarks]) — and at least every
// [replayRetry] while it holds a copy kept for a reason that may since have
// cleared, or while some other keep moved the dead letters' last sequence. Each
// pass handles each copy through [Service.Handle], the parse and the delivery a
// live delivery runs:
//
//   - a copy this node handles — its wakes delivered, or parsed to nobody, or
//     settled as any delivery is — is DROPPED;
//   - one this node passes too (its build is older still), one that reads as
//     no event, one whose handling panicked and one it fails as itself — a
//     wake too large to publish — are KEPT for a node that can, and this
//     process never handles them again: its build is what would change that,
//     and it changes only with a restart;
//   - one whose wake was not delivered for a reason of the node's own, one
//     handed back while this node sheds, one whose copy could not be read,
//     and one whose drop failed are KEPT and tried again by a later pass; and
//     one already gone was settled by whoever took it.
//
// So is one whose last delivery reached a node that could not publish its wake
// — its broker did not answer, or its stop ended the handler — since that
// failure is the node's ([queue.NakNode]). The group's poison — a delivery
// one of whose deliveries FAILED AS ITSELF — is kept on its other dead-letter
// subject, which this never reads.
//
// # Each copy contained as a delivery is
//
// A live delivery runs [Service.Handle] inside the queue's recover, where a
// panic is a Nak and a line, and the queue goes on with the rest. The replay
// is no queue handler, so it contains each copy itself, the queue's way
// ([panics.Recovered], [panics.ScopeQueue]): a copy whose handling panicked
// is KEPT, under a line naming its sequence, and the replay goes on with the
// next. Uncontained, a panic on one copy ended the process at every start of
// a node of the build that reads it — the only build that gets that far —
// since the copy, never dropped, was handled again at the next.
//
// # A copy delivered twice is one wake
//
// Two nodes' replays can read one copy before either drops it, and a copy
// whose drop failed is replayed again by a later pass — a vendor's as much
// as a first-party feed's, since a hand-back can spend any delivery's budget.
// That is safe because EVERY wake's id is derived ([Routed.WakeID]): a parser
// that defers a newer record derives its wakes' from the record's change and
// the recipient (changefeed.WakeID), and a wake whose parser derived none —
// every vendor's — takes one derived from the delivery's own id and the
// recipient ([DeliveryWakeID]), which the copy carries. So a second copy of a
// wake carries the first's id, and its seat collapses it as it collapses any
// redelivered wake: within one drain by the inbox's same-id dedupe
// (internal/agent/inbox's Screen), and across drains by the fleet's completion
// ledger (internal/engine's Dispatcher, which drops a worked trigger). A held
// wake is recorded under an id derived from that one ([HeldID]), and collapses
// alike.
//
// # What waits
//
// A copy no running node reads waits for a node that does, kept for the
// dead-letter stream's retention (`stream.event_retention_hours`): within a
// poll of its keep where a node of the build that reads it runs, and otherwise
// at the start of the next one. While one waits, the engine's alarm reading
// names it (`inbound_dead_lettered`), and one the retention lets go first is
// counted as the wake it lost (internal/engine's feeddeadletters.go).

// UnhandledSubject is where the inbound group's unhandled deliveries are kept
// — its passed wakes, those a hand-back retired, and those only a node's own
// failure did — and where the replay replays them from: [queue.RetiredTo] of
// [topics.NotificationsInbound] and [InboundGroup] for a message that never
// failed as itself.
func UnhandledSubject() string {
	return queue.RetiredTo(topics.NotificationsInbound, InboundGroup, false)
}

// unhandledUnder is the subtree of the dead letters the inbound group's
// unhandled subject lies under, whose marks a running replay asks
// ([queue.EventQueue.DeadLetterMarks]).
var unhandledUnder = topics.DeadLettersUnder(topics.NotificationsInbound)

// replayPoll is how often a running replay asks whether the group's unhandled
// subject moved: one request of the broker, however many entries it holds.
//
// TEN SECONDS, the beat the engine's alarm reading takes of the same subject
// (statelog.AlarmInterval): a copy kept while a node that reads it runs is
// delivered within a beat of the alarm first naming it, which is the only
// moment an operator would act on it, and the poll costs a node one small
// request of the broker's leader a beat.
const replayPoll = 10 * time.Second

// replayRetry is the most a running replay waits before a pass nothing on its
// subject asked for: one that tries again a copy kept for a reason that may
// have cleared — a wake the broker did not take, a node that shed, a frame it
// could not read, a drop that failed — or that lists the subject because the
// dead letters' last sequence moved, a keep somewhere, which a keep and a drop
// of the subject's own inside one poll leave its count unmoved by.
//
// A MINUTE: each of those copies already waited out its group's ten minutes
// of deliveries, so a minute more is a fraction of its wait, and a pass that
// finds nothing to do costs one listing a minute of a subject that holds
// copies; a poll's ten seconds would list it six times as often for the sake
// of a broker that has rarely come back so soon.
const replayRetry = time.Minute

// letterFate is what one replay made of one unhandled copy.
type letterFate int

const (
	// letterKept is a copy the replay left for a later pass: kept for a
	// reason that may clear.
	letterKept letterFate = iota
	// letterHeld is a copy this process's build cannot handle — one it
	// passed, one that reads as no event, one whose handling panicked —
	// left for a node that can, and never handled again here.
	letterHeld
	// letterDropped is a copy the replay handled and dropped.
	letterDropped
	// letterGone is a copy gone before the replay read it.
	letterGone
)

// replayMemory is what a running replay keeps across its passes.
type replayMemory struct {
	// held is every copy this process will not handle again
	// ([letterHeld]), by its sequence; pruned to what the subject still
	// holds at each pass.
	held map[uint64]struct{}
	// kept is how many copies the last pass kept for a reason that may
	// clear ([letterKept]), which a later pass tries again.
	kept int
}

// newReplayMemory is a memory of no pass.
func newReplayMemory() *replayMemory { return &replayMemory{held: map[uint64]struct{}{}} }

// replay is the running replay: a pass at once, and another whenever the
// group's unhandled subject moves, or every [replayRetry] while it waits on a
// copy that may now be handled — until ctx ends, which [Service.Stop] ends.
// first is closed once the first pass has returned. See this file's head.
func (s *Service) replay(ctx context.Context, first chan<- struct{}) {
	subject, mem := UnhandledSubject(), newReplayMemory()
	marks, _ := s.letters.DeadLetterMarks(ctx, unhandledUnder)
	s.replayPass(ctx, mem)
	close(first)

	poll := time.NewTicker(replayPoll)
	defer poll.Stop()
	var since time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
		since += replayPoll
		now, err := s.letters.DeadLetterMarks(ctx, unhandledUnder)
		if err != nil {
			// QUIET: a broker that did not answer raises its own alarms,
			// and the next poll asks again.
			log.DebugContext(ctx, "inbound_dead_letter_marks_unread", "error", err.Error())
			continue
		}
		held := now.Counts[subject]
		moved := held != marks.Counts[subject]
		due := since >= replayRetry && held > 0 && (mem.kept > 0 || now.Last != marks.Last)
		if !moved && !due {
			continue
		}
		marks, since = now, 0
		s.replayPass(ctx, mem)
	}
}

// replayPass handles every copy the inbound group's unhandled subject holds
// that mem does not hold back, until ctx ends — leaving every copy it did not
// reach kept — and records in mem what it made of each: see this file's head.
func (s *Service) replayPass(ctx context.Context, mem *replayMemory) {
	subject := UnhandledSubject()
	kept, err := s.letters.DeadLettersOn(ctx, subject)
	if err != nil {
		log.WarnContext(ctx, "inbound_dead_letters_unread", "subject", subject,
			"error", err.Error(),
			"detail", "the inbound group's unhandled deliveries could not be listed, so "+
				"none was replayed now; they are listed again by a later pass, and kept "+
				"until a node replays them or their retention lets them go")
		return
	}
	listed := make(map[uint64]bool, len(kept))
	fates := map[letterFate]int{}
	tried := 0
	stopped := false
	for _, entry := range kept {
		listed[entry.Seq] = true
		if _, held := mem.held[entry.Seq]; held {
			continue
		}
		if ctx.Err() != nil {
			stopped = true
			fates[letterKept]++
			continue
		}
		tried++
		fate := s.replayOne(ctx, subject, entry)
		fates[fate]++
		if fate == letterHeld {
			mem.held[entry.Seq] = struct{}{}
		}
	}
	for seq := range mem.held {
		if !listed[seq] {
			delete(mem.held, seq)
		}
	}
	mem.kept = fates[letterKept]
	if tried > 0 {
		log.InfoContext(ctx, "inbound_dead_letters_replayed", "subject", subject,
			"listed", len(kept), "replayed", fates[letterDropped],
			"kept", fates[letterKept]+fates[letterHeld], "gone", fates[letterGone],
			"stopped", stopped)
	}
}

// replayOne handles one unhandled copy, kept on subject as entry, as this node
// handles a delivery, and answers what became of it.
func (s *Service) replayOne(ctx context.Context, subject string, entry queue.Kept) letterFate {

	frame, held, err := s.letters.DeadLetterFrame(ctx, subject, entry.Seq)
	switch {
	case err != nil:
		log.WarnContext(ctx, "inbound_dead_letter_frame_unread", "subject", subject,
			"seq", entry.Seq, "error", err.Error(),
			"detail", "kept, and tried again by a later pass: its copy could not be read")
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
			"seq", entry.Seq, "error", err.Error(),
			"detail", "kept for a build that reads it: its copy reads as no event here")
		return letterHeld
	}
	res := s.handleContained(ctx, subject, entry.Seq, &ev)
	if res.Outcome == queue.OutcomeNak && errors.Is(res.Err, errValveRefused) {
		log.InfoContext(ctx, "inbound_dead_letter_rate_limited", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "error", res.Err.Error(),
			"detail", "kept, and tried again by a later pass: the notification rate limit "+
				"still refused a wake once its window had reopened")
		return letterKept
	}
	if res.Outcome == queue.OutcomeNak && errors.Is(res.Err, errReplayPanicked) {
		return letterHeld
	}
	switch res.Outcome {
	case queue.OutcomeAck:
		if err := s.letters.DropDeadLetter(ctx, subject, entry.Seq); err != nil {
			log.WarnContext(ctx, "inbound_dead_letter_drop_failed", "subject", subject,
				"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "error", err.Error(),
				"detail", "handled and kept: a later pass handles it again, and its "+
					"wakes' derived ids collapse the second copy")
			return letterKept
		}
		log.InfoContext(ctx, "inbound_dead_letter_replayed", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source)
		return letterDropped
	case queue.OutcomePass:
		log.InfoContext(ctx, "inbound_dead_letter_still_deferred", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source, "passed", res.Reason,
			"detail", "this node cannot read it either; kept for a node that can")
		return letterHeld
	default:
		var met string
		if res.Err != nil {
			met = res.Err.Error()
		}
		// A FAILURE OF THE COPY'S OWN is this build's every time — a wake
		// too large to publish — so this process never tries it again; one
		// of the node's own, or a hand-back, may clear.
		if res.FailsMessage() {
			log.WarnContext(ctx, "inbound_dead_letter_not_replayed", "subject", subject,
				"seq", entry.Seq, "event", ev.ID, "source", ev.Source,
				"outcome", res.Outcome.String(), "reason", res.Reason, "error", met,
				"detail", "kept for a node of another build: this one fails the copy as "+
					"itself, and never tries it again")
			return letterHeld
		}
		log.WarnContext(ctx, "inbound_dead_letter_not_replayed", "subject", subject,
			"seq", entry.Seq, "event", ev.ID, "source", ev.Source,
			"outcome", res.Outcome.String(), "reason", res.Reason, "error", met,
			"detail", "kept, and tried again by a later pass")
	}
	return letterKept
}

// errReplayPanicked is a replayed copy whose handling panicked
// ([Service.handleContained]): kept, and never handled again by this process,
// whose build panics on it every time.
var errReplayPanicked = errors.New("handling the replayed copy panicked")

// handleContained handles ev, the copy kept on subject as the entry seq, as
// [Service.Handle] handles a delivery, and contains a panic the way the queue
// contains a handler's: read through [panics.Recovered] in the queue's scope,
// logged with the copy's sequence and its stack, and answered as a Nak — a
// copy whose wake was not delivered, which the replay keeps for a node of
// another build ([errReplayPanicked]).
func (s *Service) handleContained(ctx context.Context, subject string, seq uint64,
	ev *events.Event) (res queue.Result) {

	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeQueue, r)
			log.ErrorContext(ctx, "inbound_dead_letter_panicked", "subject", subject,
				"seq", seq, "event", ev.ID, "source", ev.Source, "panic", r,
				"stack", contained.Stack,
				"detail", "kept for a node of another build, and never handled again by this "+
					"process; the replay goes on with the copies after it")
			res = queue.Nak(fmt.Errorf("%w: %w", errReplayPanicked, contained.Err()))
		}
	}()
	return s.handle(ctx, ev, true)
}
