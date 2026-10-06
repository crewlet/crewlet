package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/redact"
)

// An answer that is RECORDED and still OWED a resume.
//
// # Why the answer is recorded before anything is done with it
//
// A person's reply used to be held by its inbox delivery until a resume
// succeeded: a resume that failed handed the MESSAGE back to the queue with a
// Nak, and on the only broker this engine ships a Nak'd message comes back
// after its redelivery backoff, BEHIND the conversation's newer mail. So:
//
//  1. a run parks with a question in a thread;
//  2. the reply R1 arrives, the resume fails on something transient, and R1
//     is handed back;
//  3. the person's next message R2 arrives during R1's backoff, reaches the
//     still-waiting run first, and is spliced in as its answer;
//  4. R1 comes round afterwards and is either spliced into the run as the
//     answer to whatever it asked NEXT, or run as a turn with no context.
//
// Nothing compared a reply with the question it was supposed to answer — the
// match was positional, "the next inbound on the conversation" — and the
// in-memory twin replayed a Nak at the head, so every test certified the order
// production never gave.
//
// So receiving an answer and resuming with it are now two steps with two
// owners. The FIRST QUALIFYING reply — posted at or after the question
// ([PendingRun.AskedAt]) — is recorded on the row by a compare-and-set
// ([PendingStore.RecordAnswer]), and its delivery is acknowledged: the answer
// is durable from that moment and nothing about it depends on the inbox again.
// Resuming is then the coordinator's: one attempt inline, and every retry on
// this package's own backoff ([answerRetryDelay]) under the same two bounds a
// handed-back delivery had ([MaxAnswerAttempts] and [answerWindow]). A later
// reply finds the question answered and is the ordinary message it looks like.
//
// # Why the seat's inbox is held while an answer is owed
//
// The resume no longer runs inside the delivery that carried the answer, so
// nothing about the inbox's own serial dispatch keeps the seat's later mail
// behind it. The coordinator takes an inbox hold ([AnswerHold]) for as long as
// any of the seat's answers is owed, and lifts it once the resume has RETURNED
// — so R2 is worked after R1's resume, with the run's own replies already in
// the thread it reads, rather than racing it. A hold rather than a park,
// because the held mail waits on the broker with its place and its delivery
// count intact, where a park would republish it in a loop for the whole
// backoff.
//
// # What ends an answer that cannot be resumed
//
// The attempt bounds. A run nothing on this node can resume (no runner, a
// conversation this build cannot decode, a seat the epoch does not hold) gives
// its answer up after them ([Coordinator.declineAnswer]): the run goes back to
// waiting on its question, the reply is handed to the seat's ordinary route as
// the message it looks like, and that reply is declined for this question so
// it is never recorded against it again. That is exactly where the
// handed-back delivery used to end up, with the difference that it now gets
// there once, in order, rather than circling the inbox. The run itself stays
// resumable: the seat's next holder, or this node after a restart, is offered
// the next qualifying reply.

// answerRetrySeed and answerRetryCeiling space the retries of a recorded
// answer's resume: the first a second after the failure, each after that twice
// as long, never more than thirty seconds apart.
//
// THE QUEUE'S OWN SCHEDULE (internal/queue/jetstream's nakBackoff, at the
// shipped values), deliberately: [MaxAnswerAttempts]' ten attempts were sized
// against it — about two and a half minutes end to end, which spans a seat
// lease TTL (45 s, internal/seat) and several config reconcile intervals (15 s,
// internal/configplane), the transients a failed resume actually waits out.
// Moving the retry from the inbox to here changed who spaces the attempts, not
// how far apart they need to be.
const (
	answerRetrySeed    = time.Second
	answerRetryCeiling = 30 * time.Second
)

// answerRetryDelay is how long the nth failed resume of a recorded answer waits
// before the next attempt; failures counts from one.
func answerRetryDelay(failures int) time.Duration {
	return backoff.Doubling(failures, answerRetrySeed, answerRetryCeiling)
}

// maxDeclinedAnswers bounds [PendingRun.DeclinedAnswers].
//
// SIXTEEN, which is eight declined deliveries with their eight copies. Every
// decline costs a whole series of [MaxAnswerAttempts] failed resumes — about
// two and a half minutes — so a question collecting this many has been failing
// to resume for twenty minutes, and the replies that fell off the front of the
// list were delivered as ordinary messages long before.
const maxDeclinedAnswers = 16

// AnswerHold takes and lifts the hold on a seat's inbox that keeps its later
// mail behind an owed answer's resume.
//
// DECLARED HERE AND IMPLEMENTED BY THE ENGINE, which owns the seat's inbox and
// the name the hold is taken under. The coordinator calls Hold when a seat
// first owes an answer and Release when it owes none, once each — the holds are
// keyed by reason, so a second Hold would be the same hold and a Release the
// last of them.
type AnswerHold interface {
	Hold(ctx context.Context, handle string) error
	Release(ctx context.Context, handle string) error
}

// Admission says whether a seat may run a resume NOW, and why not.
//
// The engine's answer, because the conditions are the ones its inbox
// screening applies to every delivery — the lease held fresh, no person's
// pause, a turn engine, a posture that admits work, a budget that is not
// refusing — and an inline resume used to inherit them by running inside a
// delivery. A retry runs outside any delivery, so it asks.
type Admission func(ctx context.Context, handle string) (ok bool, reason string)

// Reply is a delivery offered to a parked run as the answer to its question.
type Reply struct {
	// Conv is where the delivery came from — see [ConversationRef].
	Conv ConversationRef

	// Text is the delivery as a turn would be asked it: the reply the
	// resumed run is handed.
	Text string

	// Events are the delivery's events, oldest first.
	Events []*events.Event
}

// Posted is when the delivery's NEWEST event was posted — the vendor's own
// instant where the producer stamped it on the envelope, the moment the engine
// received it otherwise. Zero for a delivery that carries no event.
//
// THE NEWEST, because a delivery is one conversation's batch and it answers
// the question if anything in it was written after the question was asked:
// that is the reply the batch carries, and the earlier lines in it are the
// context it was sent with.
func (r Reply) Posted() time.Time {
	var newest time.Time
	for _, ev := range r.Events {
		if ev != nil && ev.Timestamp.After(newest) {
			newest = ev.Timestamp
		}
	}
	return newest
}

// trigger is the delivery's newest event: what the resume is raised off and
// who the answer is recorded as coming from.
func (r Reply) trigger() *events.Event {
	var newest *events.Event
	for _, ev := range r.Events {
		if ev != nil && (newest == nil || !ev.Timestamp.Before(newest.Timestamp)) {
			newest = ev
		}
	}
	return newest
}

// ids are the delivery's event ids.
func (r Reply) ids() []string {
	out := make([]string, 0, len(r.Events))
	for _, ev := range r.Events {
		if ev != nil {
			out = append(out, ev.ID.String())
		}
	}
	return out
}

// qualifies reports whether this delivery may answer the question a run is
// parked on: it was posted at or after the question was asked, and none of it
// is a reply that question already let go of.
//
// AT OR AFTER, with no allowance for clock skew, and the margin is human: a
// question is announced after [PendingRun.AskedAt] is taken, so any reply to it
// was written after somebody read it, which takes far longer than the skew
// between two NTP-disciplined clocks. An allowance would admit only messages
// written before the question existed.
//
// A ROW WITH NO ANCHOR — parked by a build that did not record one — and a
// delivery with no instant both keep the positional match that was all there
// was before, because refusing them would strand every question parked across
// an upgrade.
func (r Reply) qualifies(run PendingRun) bool {
	if posted := r.Posted(); !run.AskedAt.IsZero() && !posted.IsZero() && posted.Before(run.AskedAt) {
		return false
	}
	for _, id := range r.ids() {
		if slices.Contains(run.DeclinedAnswers, id) {
			return false
		}
	}
	return true
}

// Best is the parked run this delivery answers, out of a seat's runs: the
// conversation decides which questions it may answer and which of them it
// does ([ConversationRef.Best]), among the ones still waiting that it
// QUALIFIES for.
func (r Reply) Best(runs []PendingRun) (PendingRun, bool) {
	var parked []PendingRun
	for _, run := range runs {
		if slices.Contains(Awaiting, run.Status) && r.qualifies(run) {
			parked = append(parked, run)
		}
	}
	return r.Conv.Best(parked)
}

// recordedOn is the run this delivery is ALREADY the recorded answer of.
//
// A copy of an answer reaching the seat again — a redelivery whose
// acknowledgement was lost, the node that recorded it stopping before it
// acked — is that answer, and must be spent as one rather than matched again
// (the question it answered is no longer waiting) or run as a turn.
func (r Reply) recordedOn(runs []PendingRun) (PendingRun, bool) {
	ids := r.ids()
	for _, run := range runs {
		if run.Answer == nil {
			continue
		}
		for _, id := range ids {
			if slices.Contains(run.Answer.EventIDs, id) {
				return run, true
			}
		}
	}
	return PendingRun{}, false
}

// answerOf is what recording this delivery writes.
func (r Reply) answerOf(now time.Time) RecordedAnswer {
	answer := RecordedAnswer{
		// Redacted HERE, at the one place the reply becomes a record a
		// second store keeps: it is spliced into a phase record verbatim.
		Text: redact.Secrets(r.Text), By: answererOf(r.trigger()),
		EventIDs: r.ids(), PostedAt: r.Posted(), RecordedAt: now,
	}
	for _, ev := range r.Events {
		if ev == nil {
			continue
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			// Kept without it: the answer is the text, and an event
			// that cannot be carried costs the resume its indicator and
			// a decline its copy, never the answer itself.
			log.Warn("sandbox_answer_event_unencodable", "event_id", ev.ID.String(),
				"error", err.Error())
			continue
		}
		answer.Events = append(answer.Events, raw)
	}
	return answer
}

// decodedEvents are a recorded answer's deliveries, as events again.
func (a RecordedAnswer) decodedEvents() []*events.Event {
	out := make([]*events.Event, 0, len(a.Events))
	for _, raw := range a.Events {
		var ev events.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Warn("sandbox_answer_event_undecodable", "error", err.Error())
			continue
		}
		out = append(out, &ev)
	}
	return out
}

// declinedTo is the status a declined answer puts its run back to: the one the
// record took it out of — awaiting, or reseed when the pause reaper had
// already reclaimed the box. A record this build did not write names no
// status a question can wait in, and awaiting is the one every reader takes a
// parked run in; the box record beside it still says whether there is a
// checkout to continue in.
func (a RecordedAnswer) declinedTo() string {
	if slices.Contains(Awaiting, a.From) {
		return a.From
	}
	return StatusAwaiting
}

// trigger is the newest of a recorded answer's deliveries, which the resume is
// raised off — nil where none could be carried.
func (a RecordedAnswer) trigger() *events.Event {
	return Reply{Events: a.decodedEvents()}.trigger()
}

// declinedCopyNamespace derives the ids of the copies a declined answer hands
// back.
var declinedCopyNamespace = uuid.MustParse("6f1d2c3a-6a1e-5b7e-9c0d-1e2f3a4b5c6d")

// declinedCopyID is the id a declined delivery's copy is handed back under.
//
// A NEW ID, because the original is spent: the dispatcher recorded it in the
// completion ledger when the answer was recorded, so a copy under the same id
// would be dropped as already worked. DERIVED rather than random, so handing
// the same answer back twice — a decline whose record did not land, retried —
// is one message to the inbox and the completion ledger, not two.
func declinedCopyID(id uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(declinedCopyNamespace, []byte(id.String()))
}

// answerRetry is one owed answer's series of failed resumes on this node.
type answerRetry struct {
	handle string
	launch string

	// failures is how many resumes of this answer have failed here, and
	// first when the first of them did — what [answerWindow] runs from.
	failures int
	first    time.Time
	window   time.Duration

	// stop cancels the pending attempt, reporting whether it had not yet
	// started.
	stop func() bool
}

// live reports whether the series may make another attempt.
func (r *answerRetry) live(now time.Time) bool {
	return answerBudget{failures: r.failures, first: r.first}.live(now, r.window)
}

// recordAnswer records a delivery as the answer to the run it qualifies for,
// and starts the resume it owes.
//
// The resume's first attempt runs INLINE, on the caller's delivery, which is
// what keeps the ordinary case as immediate as it ever was; whatever it
// concludes, the delivery is spent — the answer is on the row.
func (c *Coordinator) recordAnswer(ctx context.Context, run PendingRun, reply Reply) (PendingRun, bool, error) {
	recorded, won, err := c.pending.RecordAnswer(ctx, run.TurnID, run.LaunchID, reply.answerOf(c.now()))
	if err != nil || !won {
		return PendingRun{}, won, err
	}
	// Out of the question set: nobody is waited on, and the screening
	// offers this seat's mail to no question that already has its answer.
	c.moveRun(recorded.AgentHandle, run.Status, StatusAnswered)
	log.InfoContext(ctx, "sandbox_clarification_answered",
		"turn_id", recorded.TurnID, "via", string(types.AnswerViaChat),
		"conversation", reply.Conv.Identity, "partition", reply.Conv.Partition,
		"run_conversation", recorded.ConversationKey, "run_partition", recorded.PartitionKey,
		"asked_at", recorded.AskedAt, "posted_at", recorded.Answer.PostedAt)
	return recorded, true, nil
}

// resumeOwed takes the seat's inbox hold for an owed answer and makes one
// attempt at its resume, reporting whether the run turned out to be
// terminally GONE — settled by the attempt itself, so nothing is owed the
// reply any more and it is the ordinary message it looks like.
func (c *Coordinator) resumeOwed(ctx context.Context, run PendingRun) (gone bool) {
	c.owe(ctx, run.AgentHandle, run.TurnID)
	return c.attemptOwed(ctx, run)
}

// attemptOwed makes one attempt at an owed answer's resume, and settles what
// the attempt leaves: done (the hold lifted), failed (another attempt
// scheduled, or the answer declined once the bounds are spent), or the run
// terminally gone, which it reports.
func (c *Coordinator) attemptOwed(ctx context.Context, run PendingRun) (gone bool) {
	answer := run.Answer
	if answer == nil {
		// An answered row always carries its answer; one that does not
		// was written by something this build does not understand, and
		// there is nothing here to resume it with.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return false
	}
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID, RecordedAnswerTail(run.LaunchID))
	if err != nil {
		// THE CLAIM MAY HAVE LANDED. A retry finds out: a row it can no
		// longer claim has moved on, and the seat's next recovery pass
		// reaps a claim nobody is resuming.
		c.owedFailed(ctx, run, fmt.Errorf("sandbox: claiming %s for its recorded answer: %w",
			run.TurnID, err))
		return false
	}
	if !won {
		// Resumed already (an attempt on another node across a handoff),
		// declined, or replaced by the next job: not this attempt's.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return false
	}
	c.moveRun(claimed.AgentHandle, StatusAnswered, StatusResumed)
	disposition, err := c.resumeAndSettle(ctx, claimed, answerText(claimed, answer.Text, ""), true,
		answer.trigger(), runOutcome{
			InputTokens: claimed.ParkedInputTokens, OutputTokens: claimed.ParkedOutputTokens,
		})
	if disposition == AnswerDeferred {
		// The claim went back: the run is [StatusAnswered] again with
		// the same answer on it, owed the same resume.
		c.owedFailed(ctx, claimed, err)
		return false
	}
	c.settleOwed(ctx, claimed.AgentHandle, claimed.TurnID)
	c.announceAnswered(ctx, claimed, types.AnswerViaChat, answeredAs(disposition), answer.By, "")
	return disposition == AnswerNotMine
}

// owedFailed counts one failed resume of an owed answer, and schedules the next
// attempt or — once [MaxAnswerAttempts] or [answerWindow] is spent — declines
// the answer.
func (c *Coordinator) owedFailed(ctx context.Context, run PendingRun, cause error) {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	c.mu.Lock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.failures++
	live := r.live(c.now())
	failures := r.failures
	c.mu.Unlock()

	if !live {
		c.declineAnswer(ctx, run, cause)
		return
	}
	delay := answerRetryDelay(failures)
	log.WarnContext(ctx, "sandbox_answer_resume_failed",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "failures", failures,
		"retry_in_s", delay.Seconds(), "error", detail,
		"detail", "the answer is recorded on the run, so it is the coordinator that "+
			"retries the resume; the seat's later mail waits behind it")
	c.scheduleOwed(run.TurnID, delay)
}

// scheduleOwed arms the next attempt at an owed answer's resume.
func (c *Coordinator) scheduleOwed(turnID string, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[turnID]
	if r == nil || c.closed {
		return
	}
	if r.stop != nil {
		if r.stop() {
			c.inflight.Done()
		}
	}
	c.inflight.Add(1)
	r.stop = c.after(delay, func() {
		defer c.inflight.Done()
		c.retryOwed(turnID)
	})
}

// retryOwed is one scheduled attempt: admitted by the seat's conditions, then
// made against the row as the store holds it now.
func (c *Coordinator) retryOwed(turnID string) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	r := c.retries[turnID]
	var handle, launch string
	if r != nil {
		r.stop = nil
		handle, launch = r.handle, r.launch
	}
	c.mu.Unlock()
	if r == nil {
		return
	}
	if c.admit != nil {
		if ok, reason := c.admit(ctx, handle); !ok {
			// NOT A FAILURE, so nothing is charged: a paused seat, a
			// refusing budget, a stale company or a lease being renewed
			// is the seat's condition, and the resume waits it out at
			// the slowest spacing rather than spending the attempts that
			// exist for a resume that fails.
			log.DebugContext(ctx, "sandbox_answer_resume_waiting",
				"turn_id", turnID, "agent", handle, "reason", reason)
			c.scheduleOwed(turnID, answerRetryCeiling)
			return
		}
	}
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		c.owedFailed(ctx, PendingRun{TurnID: turnID, AgentHandle: handle, LaunchID: launch},
			fmt.Errorf("sandbox: reading run %s for its recorded answer: %w", turnID, err))
		return
	}
	if !found || run.Status != StatusAnswered || run.LaunchID != launch {
		// Over, resumed, declined or relaunched since: nothing is owed.
		c.settleOwed(ctx, handle, turnID)
		return
	}
	if gone := c.attemptOwed(ctx, run); gone {
		// THE RUN ENDED UNDER THE ATTEMPT — settled and announced as
		// lost — and the delivery that carried the reply was acknowledged
		// long ago, so nothing would ever bring the reply to the seat.
		// Handed back as the ordinary message it is, as the inline
		// attempt's caller does with the delivery still in hand.
		if _, err := c.handBack(ctx, run.AgentHandle, *run.Answer); err != nil {
			log.ErrorContext(ctx, "sandbox_answer_handback_failed",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error(),
				"detail", "the run this reply answered has ended and the reply could not "+
					"be handed to the seat as an ordinary message")
		}
	}
}

// declineAnswer gives up on an answer this node could not resume with, once
// its attempts are spent: the reply goes on to the seat's ordinary route as
// the message it looks like, and the run goes back to waiting on its question.
//
// THE COPIES ARE PUBLISHED FIRST, under ids derived from the originals
// ([declinedCopyID]), and the record is let go of second. The other order
// loses the reply outright on a failure between the two; this one can at worst
// deliver it twice — once as the answer a later attempt resumed with, once as
// an ordinary message — which is the recoverable way round.
//
// THE RUN IS NOT ENDED, for the reason [MaxAnswerAttempts] gives: every
// failure that reaches here is a statement about THIS node, and the turn is
// still resumable by a node that holds the seat with the runner and the build
// it needs.
func (c *Coordinator) declineAnswer(ctx context.Context, run PendingRun, cause error) {
	answer := run.Answer
	if answer == nil {
		// Only a row this attempt could not READ arrives here without its
		// answer, and nothing can be handed back or let go of without
		// reading it. The seat's mail is let through rather than held
		// behind a store that will not answer; the row stays answered,
		// and the seat's next recovery pass drives it again.
		log.ErrorContext(ctx, "sandbox_answer_unresumable_unread",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", errDetail(cause),
			"detail", "every attempt to resume this run with its recorded answer failed and "+
				"the run could not be read to hand the answer back; it stays recorded for "+
				"the seat's next recovery pass")
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	copies, err := c.handBack(ctx, run.AgentHandle, *answer)
	if err != nil {
		// NOT LET GO OF: without the copy the reply would be lost. The
		// decline is tried again at the slowest spacing.
		log.ErrorContext(ctx, "sandbox_answer_handback_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error())
		c.scheduleOwed(run.TurnID, answerRetryCeiling)
		return
	}
	declined := append(slices.Clone(answer.EventIDs), copies...)
	let, err := c.pending.DeclineAnswer(ctx, run.TurnID, run.LaunchID, declined, fenceOf(run))
	if err != nil {
		log.ErrorContext(ctx, "sandbox_answer_decline_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error())
		c.scheduleOwed(run.TurnID, answerRetryCeiling)
		return
	}
	if let {
		c.moveRun(run.AgentHandle, StatusAnswered, answer.declinedTo())
	}
	log.ErrorContext(ctx, "sandbox_answer_requeue_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "attempts", MaxAnswerAttempts,
		"window_s", answerWindow(run).Seconds(), "error", errDetail(cause),
		"detail", "this node could not resume the coding run that asked with the answer "+
			"recorded for it, in every one of its spaced attempts, so the reply is handed "+
			"to the seat as the ordinary message it looks like; the run waits on its "+
			"question again and its box is bounded by pause_ttl_seconds")
	c.settleOwed(ctx, run.AgentHandle, run.TurnID)
}

// recoverOwed takes over an answer a previous holder of the seat recorded and
// did not resume with: the seat's inbox is held behind it, and the first
// attempt is made at once, off the seat's preparation.
func (c *Coordinator) recoverOwed(ctx context.Context, run PendingRun) {
	c.mu.Lock()
	if _, ok := c.retries[run.TurnID]; !ok {
		c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
			first: c.now(), window: answerWindow(run)}
	}
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, 0)
}

// handBack publishes a recorded answer's deliveries to the seat's inbox as the
// ordinary messages they are, under ids derived from the originals
// ([declinedCopyID]), and reports those ids.
//
// Their own instants are kept: the copy is the message the person sent, and
// whether it could answer a question is measured from when they sent it.
func (c *Coordinator) handBack(ctx context.Context, handle string, answer RecordedAnswer) ([]string, error) {
	inbox := topics.AgentInbox(handle)
	if inbox == "" {
		return nil, fmt.Errorf("sandbox: seat %q has no inbox to hand its reply back to", handle)
	}
	var copies []string
	for _, ev := range answer.decodedEvents() {
		handed := *ev
		handed.ID = declinedCopyID(ev.ID)
		if err := c.queue.Publish(ctx, inbox, &handed); err != nil {
			return nil, fmt.Errorf("sandbox: handing %s back to %s: %w", ev.ID, handle, err)
		}
		copies = append(copies, handed.ID.String())
	}
	return copies, nil
}

// owe records that a seat owes one of its runs an answer's resume, and takes
// the seat's inbox hold if it is the first.
func (c *Coordinator) owe(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	owed := c.owed[handle]
	if owed == nil {
		owed = map[string]struct{}{}
		c.owed[handle] = owed
	}
	owed[turnID] = struct{}{}
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
}

// settleOwed forgets an owed answer — resumed, declined or moved on — and
// lifts the seat's inbox hold if it was the last.
func (c *Coordinator) settleOwed(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	if r := c.retries[turnID]; r != nil {
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		delete(c.retries, turnID)
	}
	if owed := c.owed[handle]; owed != nil {
		delete(owed, turnID)
		if len(owed) == 0 {
			delete(c.owed, handle)
		}
	}
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
}

// releaseOwed drops everything a seat owes on this node, for a seat it no
// longer holds: the successor's recovery pass drives every recorded answer
// again.
func (c *Coordinator) releaseOwed(ctx context.Context, handle string) {
	c.mu.Lock()
	for turnID, r := range c.retries {
		if r.handle != handle {
			continue
		}
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		delete(c.retries, turnID)
	}
	delete(c.owed, handle)
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
}

// reconcileHold brings a seat's inbox hold into line with what it owes.
//
// ONE RECONCILER PER SEAT AT A TIME, and it loops until what it applied is
// still what is wanted. The queue call cannot be made under the lock — the
// in-memory twin's release drains the inbox synchronously into handlers that
// come straight back here — and two callers each applying their own decision
// outside it could land in either order, leaving a seat held with nothing owed
// or owed with nothing held. So a caller that finds a reconcile in progress
// marks the seat dirty and leaves, and the one in progress goes round again.
func (c *Coordinator) reconcileHold(ctx context.Context, handle string) {
	if c.hold == nil || handle == "" {
		return
	}
	// A hold is the seat's, not the request's: one taken or lifted on a
	// context a drain is cancelling would do nothing at all.
	ctx = context.WithoutCancel(ctx)
	for {
		c.mu.Lock()
		if c.holdBusy[handle] {
			c.holdDirty[handle] = true
			c.mu.Unlock()
			return
		}
		want := len(c.owed[handle]) > 0
		if want == c.held[handle] {
			c.mu.Unlock()
			return
		}
		c.holdBusy[handle] = true
		c.mu.Unlock()

		var err error
		if want {
			err = c.hold.Hold(ctx, handle)
		} else {
			err = c.hold.Release(ctx, handle)
		}

		c.mu.Lock()
		delete(c.holdBusy, handle)
		dirty := c.holdDirty[handle]
		delete(c.holdDirty, handle)
		if err == nil {
			if want {
				c.held[handle] = true
			} else {
				delete(c.held, handle)
			}
		}
		c.mu.Unlock()
		if err != nil {
			// Logged and left: a hold that could not be taken lets the
			// seat's later mail run beside the resume, which reorders
			// it and loses nothing; one that could not be lifted is
			// lifted by the seat's next detach, which releases every
			// hold its inbox carries. Retrying a queue call that just
			// refused, from a loop that holds a delivery, is worse
			// than either.
			log.WarnContext(ctx, "sandbox_answer_hold_failed", "agent", handle,
				"hold", want, "error", err.Error())
			if !dirty {
				return
			}
		}
	}
}

// errDetail is an error's text, or empty.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
