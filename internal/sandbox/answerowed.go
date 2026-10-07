package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// behind it. The coordinator holds the seat's inbox ([SeatHold], the hold
// seathold.go keeps for a busy seat too) for as long as any of the seat's
// answers is owed, and lifts it once the resume has RETURNED
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
// there EXACTLY ONCE, in order, rather than circling the inbox: the decision
// and the copies it owes are one write, and the copies are published off the
// row afterwards under ids derived from the originals, so a crash at any step
// is finished by the next reader of the row without losing the reply or
// delivering it twice (see [Coordinator.declineAnswer]). The run itself stays
// resumable: the seat's next holder, or this node after a restart, is offered
// the next qualifying reply.
//
// A RETRY WHOSE RUN TURNS OUT TO BE GONE lets the answer go the same way: its
// copies are recorded on the claimed row as owed before the run is ended
// ([Coordinator.oweOnEnding]), and the ending hands them back before it deletes
// the row ([Coordinator.endRecord]).
//
// # A retry the seat's own conditions refuse
//
// WAITS FOR WHAT REFUSED IT, rather than re-checking on a timer: every
// condition a retry is admitted under changes on an event the engine observes
// or at an instant the refusal names, so the refusal says which
// ([Refusal]), and the engine re-checks the attempts waiting on it when it
// happens ([Coordinator.Readmit]). A backstop ([answerWaitBackstop]) re-checks
// what a missed signal would strand. See [Coordinator.waitOwed].

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
// list were delivered as ordinary messages long before. The newest decline is
// kept whole past it, because its copies are the ones still owed or just
// published (see [boundedDeclined]).
const maxDeclinedAnswers = 16

// Condition names what a refused retry is waiting for, so that the signal
// that can clear it wakes it and nothing else does.
//
// DEFINED BY THE ENGINE, whose conditions they are: this package only keeps
// the name a refusal gave and compares it with the one a signal names (see
// [Coordinator.Readmit]).
type Condition string

// Refusal is why a seat may not run a retried resume now, and what would
// change that.
type Refusal struct {
	// Condition is what the refusal waits on; a [Coordinator.Readmit] naming
	// it re-checks the retry at once.
	Condition Condition

	// Reason is the refusal, for a log line.
	Reason string

	// Until is the instant the refusal lifts on the clock alone — the end of
	// the budget window that is refusing, the next lease renew — and zero
	// when only an event can lift it.
	Until time.Time
}

// Admission says whether a seat may run a resume NOW, and if not, why and
// until when.
//
// The engine's answer, because the conditions are the ones its inbox
// screening applies to every delivery — the lease held fresh, no person's
// pause, a turn engine, a posture that admits work, a budget that is not
// refusing — and an inline resume used to inherit them by running inside a
// delivery. A retry runs outside any delivery, so it asks.
type Admission func(ctx context.Context, handle string) (refusal Refusal, refused bool)

// answerWaitBackstop is the longest a retry the seat's conditions refused waits
// for the signal that clears them before it re-checks on its own.
//
// A BACKSTOP AND NOT A SCHEDULE. Every condition a refusal can name changes on
// an event the engine already observes and passes on ([Coordinator.Readmit]) —
// a person resuming the seat, an apply bringing a model or a ceiling, the
// posture admitting work again, the seat established or its ownership proven
// again — or on a clock the refusal itself names
// ([Refusal.Until]). What this bounds is a signal that never came: a path that
// moves one of those conditions and was never wired to say so. Five minutes is
// one admission check (this node's own reads and, at most, one read of the
// token counters) per refused answer per five minutes while nothing has
// changed, and it keeps a missed signal's cost — the person's answer resumed
// late — well inside the 30-minute default `pause_ttl_seconds`, so a missed
// signal does not also cost the run its paused box. The old answer was to
// re-check every 30 seconds, which is a poll of conditions that change only on
// events.
const answerWaitBackstop = 5 * time.Minute

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

// answerRetry is one owed answer's series of attempts on this node: at its
// resume, or — once that is let go — at handing its reply back to the seat.
type answerRetry struct {
	handle string
	launch string

	// failures is how many resumes of this answer have failed here, and
	// first when the first of them did — what [answerWindow] runs from.
	failures int
	first    time.Time
	window   time.Duration

	// waiting is what an attempt the seat's conditions refused is waiting
	// on, for as long as it waits — and empty while the series is
	// attempting, failing or handing back. See [Coordinator.Readmit].
	waiting Condition

	// ending is an ending of the run that was decided and kept its record
	// for the reply the record still owes the seat: once the hand-back is
	// out, the series ends the run under the same license. Nil for every
	// other series. See [Coordinator.oweEnding].
	ending *owedEnding

	// stop cancels the pending attempt, reporting whether it had not yet
	// started.
	stop func() bool
}

// owedEnding is the license an ending was decided under — see
// [Coordinator.endRecord].
type owedEnding struct {
	fence   Fence
	whileIn []string
}

// live reports whether the series may make another attempt.
func (r *answerRetry) live(now time.Time) bool {
	return answerBudget{failures: r.failures, first: r.first}.live(now, r.window)
}

// spent reports whether the series has failed and run out of attempts — what
// sends a retry to the decline rather than to another resume.
func (r *answerRetry) spent(now time.Time) bool {
	return r.failures > 0 && !r.live(now)
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
	// A NEW ANSWER IS A NEW SERIES. The run's series can still be alive when
	// this lands — kept to publish what an earlier answer's decline owes the
	// seat — and its count is that earlier answer's: carried over, it spent
	// this answer's attempts before this answer had one, and let it go after
	// a single failed resume.
	c.mu.Lock()
	if r := c.retries[recorded.TurnID]; r != nil {
		r.launch, r.failures, r.first = recorded.LaunchID, 0, c.now()
		r.window = answerWindow(recorded)
	}
	c.mu.Unlock()
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
	// THE DELIVERY IS IN THE CALLER'S HAND, so a run that turns out to be
	// gone owes the seat nothing: the caller works the delivery as the
	// ordinary message it is.
	return c.attemptOwed(ctx, run, false)
}

// attemptOwed makes one attempt at an owed answer's resume, and settles what
// the attempt leaves: done (the hold lifted), failed (another attempt
// scheduled, or the answer declined once the bounds are spent), or the run
// terminally gone, which it reports.
//
// handBack is whether a run that turns out to be gone owes the seat its reply:
// true on a retry, whose delivery was acknowledged when the answer was
// recorded, so the reply is handed back as the ordinary message it is — and
// recorded on the row as owed BEFORE the run is ended ([PendingStore.OweHandBack]),
// so a crash between the ending and the publish cannot lose it.
func (c *Coordinator) attemptOwed(ctx context.Context, run PendingRun, handBack bool) (gone bool) {
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
	var owed []HandedBack
	if handBack {
		owed = handBackOf(*answer)
	}
	disposition, err := c.resumeAndSettle(ctx, claimed, answerText(claimed, answer.Text, ""), true,
		answer.trigger(), runOutcome{
			InputTokens: claimed.ParkedInputTokens, OutputTokens: claimed.ParkedOutputTokens,
		}, owed)
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

// scheduleOwed arms the next attempt at an owed answer.
func (c *Coordinator) scheduleOwed(turnID string, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scheduleOwedLocked(turnID, delay)
}

// scheduleOwedLocked is [Coordinator.scheduleOwed] for a caller holding c.mu —
// which a refusal and a readmission both have to be, so that a signal landing
// between "this attempt is waiting on X" and the timer that waits for it can
// never be overwritten by that timer.
func (c *Coordinator) scheduleOwedLocked(turnID string, delay time.Duration) {
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

// retryOwed is one scheduled attempt, made against the row as the store holds
// it now: a hand-back the row still owes is published first, an answer whose
// attempts are spent is declined, and anything else is a resume — admitted by
// the seat's conditions first.
func (c *Coordinator) retryOwed(turnID string) {
	ctx := c.life
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	r := c.retries[turnID]
	var handle, launch string
	var spent bool
	var ending *owedEnding
	if r != nil {
		r.stop, r.waiting = nil, ""
		handle, launch = r.handle, r.launch
		spent = r.spent(c.now())
		ending = r.ending
	}
	c.mu.Unlock()
	if r == nil {
		return
	}
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		if ending != nil {
			// A READ IS NOT A RESUME, and an ending owes no attempt: the
			// row is read again on the hand-back's own spacing.
			c.scheduleOwed(turnID, answerRetryCeiling)
			return
		}
		c.owedFailed(ctx, PendingRun{TurnID: turnID, AgentHandle: handle, LaunchID: launch},
			fmt.Errorf("sandbox: reading run %s for its recorded answer: %w", turnID, err))
		return
	}
	if !found {
		c.endingDone(turnID)
		c.settleOwed(ctx, handle, turnID)
		return
	}
	// WHAT A DECLINE ALREADY DECIDED goes out before anything else is
	// decided about the run: the write that let the answer go is on the row,
	// and its copies are what it owes. Whatever the run has done since — a
	// new answer, a new question, a relaunch — they are still owed.
	if len(run.HandBack) > 0 {
		if err := c.deliverHandBack(ctx, run); err != nil {
			log.ErrorContext(ctx, "sandbox_answer_handback_failed",
				"turn_id", turnID, "agent", handle, "error", err.Error(),
				"detail", "the reply this run let go of is recorded on it as owed to the seat; "+
					"the hand-back is tried again, and the seat's mail waits behind it")
			c.scheduleOwed(turnID, answerRetryCeiling)
			return
		}
	}
	if ending != nil {
		// THE RUN WAS ALREADY ENDED, and kept its record only for the reply
		// it owed the seat, which is out now: the record goes, under the
		// license the ending was decided with. One that owes the seat
		// something again is owed again, by [Coordinator.endRecord] itself.
		c.endingDone(turnID)
		if _, _, err := c.endRecord(ctx, run, ending.fence, ending.whileIn); err != nil {
			log.WarnContext(ctx, "sandbox_finish_failed", "turn_id", turnID, "error", err.Error(),
				"detail", "the run's reply is handed back but its record was not deleted; the "+
					"seat's next recovery pass reaps it")
		}
		// RECOUNTED: the kept record was still a live row to every recount
		// made while it waited, and the seat's counts are the store's answer.
		c.syncSeat(ctx, handle)
		c.settleOwed(ctx, handle, turnID)
		return
	}
	if run.Status != StatusAnswered || run.LaunchID != launch {
		// Over, resumed, declined or relaunched since: nothing is owed.
		c.settleOwed(ctx, handle, turnID)
		return
	}
	if spent {
		// THE BOUND IS SPENT and the decline that followed it did not
		// land: decided again, never resumed again — the attempts it was
		// owed have all been made.
		c.declineAnswer(ctx, run, errors.New("sandbox: the answer's attempts are spent and "+
			"letting it go did not land"))
		return
	}
	if c.admit != nil {
		// What has been signalled so far, read BEFORE the check: a signal
		// that lands while the check is deciding is one the wait below must
		// not sleep through. See [Coordinator.waitOwed].
		c.mu.Lock()
		seen := maps.Clone(c.signals)
		c.mu.Unlock()
		if refusal, refused := c.admit(ctx, handle); refused {
			c.waitOwed(ctx, turnID, refusal, seen)
			return
		}
	}
	// A RUN THAT ENDS UNDER THE ATTEMPT — settled and announced as lost —
	// owes the seat the reply, because the delivery that carried it was
	// acknowledged long ago and nothing else would ever bring it back. It
	// is recorded on the row as owed before the run is ended, and the ending
	// hands it back before it deletes the row (see [Coordinator.oweOnEnding]):
	// the inline attempt's caller hands on the delivery it still holds
	// instead. It used to be published here, AFTER the ending had deleted
	// the only record of it, so a crash between the two lost it.
	c.attemptOwed(ctx, run, true)
}

// oweOnEnding records on a claimed run, before the run is ended, the copies of
// a reply the ending lets go of ([PendingStore.OweHandBack]), so the ending
// hands them back before it deletes the row ([Coordinator.endRecord]) and a
// crash anywhere between the two leaves them owed on a row whoever reads it
// next publishes. Nothing to record is nothing to do.
//
// A ROW THAT CANNOT CARRY THEM — the write refused, or the run no longer the
// claim this node took — gets them published here, before the ending, which
// is the order that cannot lose them to anything but the process stopping in
// the same instant the store is failing: published twice is the same message
// under the same derived ids, which the inbox's same-id dedupe and the
// completion ledger collapse.
func (c *Coordinator) oweOnEnding(ctx context.Context, run PendingRun, owed []HandedBack) {
	if len(owed) == 0 {
		return
	}
	recorded, err := c.pending.OweHandBack(ctx, run.TurnID, run.LaunchID, owed, fenceOf(run))
	if err == nil && recorded {
		return
	}
	log.WarnContext(ctx, "sandbox_answer_handback_unrecorded",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "error", errDetail(err),
		"detail", "the run is ending without the reply it was answered with, and the reply "+
			"could not be recorded on it as owed to the seat, so it is handed back now")
	if _, err := c.publishCopies(ctx, run.AgentHandle, owed); err != nil {
		log.ErrorContext(ctx, "sandbox_answer_handback_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error(),
			"detail", "the run this reply answered is ending and the reply could neither be "+
				"recorded as owed to the seat nor handed to it")
	}
}

// oweEnding keeps an ending this node decided and could not finish, for the
// reply its row still owes the seat ([Coordinator.endRecord]): the row was not
// deleted because the copies it carries could not be published. The seat's
// inbox is held behind them, as it is behind a decline's, and a retry on the
// hand-back's spacing publishes them and then ends the run under the same
// license — or, if the seat moves first, its next holder's recovery pass does
// both.
func (c *Coordinator) oweEnding(ctx context.Context, run PendingRun, fence Fence, whileIn []string) {
	if run.AgentHandle == "" {
		return
	}
	c.mu.Lock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.ending = &owedEnding{fence: fence, whileIn: slices.Clone(whileIn)}
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, answerRetryCeiling)
}

// endingDone forgets an owed ending, for a series about to settle it.
func (c *Coordinator) endingDone(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.retries[turnID]; r != nil {
		r.ending = nil
	}
}

// waitOwed parks an attempt the seat's conditions refused until what refused
// it can have changed.
//
// # Why a wait and not a poll
//
// NOT A FAILURE, so nothing is charged: a paused seat, a refusing budget, a
// company with no model, a node whose posture refuses work and a lease being
// renewed are the seat's condition, and the attempts exist for a resume that
// fails. And not a poll either, which is what this was — a re-check every 30
// seconds for as long as the condition held, which for a person's pause or a
// spent daily budget is hours of re-checks that can only say no. Every one of
// those conditions changes on something DISCRETE, and the engine observes
// each: the pause watch hears a resume, an apply brings a model, a ceiling or
// a posture, the budget window ends at an instant the refusal names, and a
// seat starts admitting turns when its acquisition is established or a renew
// proves its lease again after a blip — the seat host says so at either edge
// — with one heartbeat on the clock behind them for a renew that was merely
// late. So
// the refusal names what it waits on ([Refusal]): the clock it waits for is
// armed, the event it waits for re-checks it through [Coordinator.Readmit] the
// moment it happens, and nothing runs in between.
//
// # Why there is still a timer
//
// A signal can be missed — a path that moves one of those conditions and was
// never wired to say so — and an answered run whose re-check never comes is a
// person's answer stranded on a seat that could have taken it. So no wait is
// longer than [answerWaitBackstop], and that re-check finds the condition
// cleared or names it again.
//
// # A signal that raced the check is not missed
//
// The check and the wait are two steps, and [Coordinator.Readmit] only wakes an
// attempt already marked waiting. A signal landing between them — the seat
// established while its inherited answer was being refused for establishing —
// found nothing waiting, and the wait that followed slept through it, to the
// clock or the backstop. So the caller reads what had been signalled before it
// checked (seen), and a wait on a condition signalled since re-checks at once.
func (c *Coordinator) waitOwed(ctx context.Context, turnID string, refusal Refusal,
	seen map[signal]uint64,
) {
	wait := answerWaitBackstop
	if !refusal.Until.IsZero() {
		wait = min(wait, max(refusal.Until.Sub(c.now()), 0))
	}
	c.mu.Lock()
	r := c.retries[turnID]
	var handle string
	if r != nil {
		handle = r.handle
		if c.signalledSinceLocked(refusal.Condition, handle, seen) {
			wait = 0
		} else {
			r.waiting = refusal.Condition
		}
		c.scheduleOwedLocked(turnID, wait)
	}
	c.mu.Unlock()
	if r == nil {
		return
	}
	log.DebugContext(ctx, "sandbox_answer_resume_waiting",
		"turn_id", turnID, "agent", handle, "condition", string(refusal.Condition),
		"reason", refusal.Reason, "recheck_in_s", wait.Seconds())
}

// Readmit re-checks at once every attempt waiting on cond, for the named seats
// or — named none — every seat: the signal that the condition may have
// cleared. An attempt waiting on anything else is left waiting, so a signal
// costs nothing but the attempts it can actually release, and one that turns
// out not to have released them waits again on whatever refuses it now.
//
// Safe from any goroutine, and cheap enough to call on every occurrence of the
// event: with nothing waiting it is one pass over this node's owed answers.
func (c *Coordinator) Readmit(cond Condition, handles ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(handles) == 0 {
		c.signals[signal{cond: cond}]++
	}
	for _, handle := range handles {
		c.signals[signal{cond: cond, handle: handle}]++
	}
	for turnID, r := range c.retries {
		if r.waiting == "" || r.waiting != cond ||
			(len(handles) > 0 && !slices.Contains(handles, r.handle)) {
			continue
		}
		r.waiting = ""
		c.scheduleOwedLocked(turnID, 0)
	}
}

// signal is what a [Coordinator.Readmit] was for: a condition, on one seat or —
// with no handle — on every seat.
type signal struct {
	cond   Condition
	handle string
}

// signalledSinceLocked reports whether cond has been signalled for handle
// since seen was read. The caller holds c.mu.
func (c *Coordinator) signalledSinceLocked(cond Condition, handle string, seen map[signal]uint64) bool {
	for _, key := range []signal{{cond: cond}, {cond: cond, handle: handle}} {
		if c.signals[key] != seen[key] {
			return true
		}
	}
	return false
}

// declineAnswer gives up on an answer this node could not resume with, once
// its attempts are spent: the reply goes on to the seat's ordinary route as
// the message it looks like, and the run goes back to waiting on its question.
//
// # Exactly once, across a crash at any step
//
// Letting go is a write to the row and a publish to the broker, and it is
// ordered so that a process stopping between any two steps neither loses the
// reply nor delivers it twice:
//
//  1. THE WRITE, which is the decision ([PendingStore.DeclineAnswer]): the
//     answer is cleared, the reply and its copies are declined for the
//     question, and the copies — encoded, under ids derived from the
//     originals ([declinedCopyID]) — are recorded on the row as owed to the
//     seat ([PendingRun.HandBack]). Stopped before it lands, the answer is
//     still recorded and owed its resume, nothing has been published, and the
//     seat's next holder (or this node's next attempt) drives it again from
//     the row.
//  2. THE PUBLISH of those copies, read back off what the write stored.
//     Stopped before it, whoever reads the row next — this node's retry, or
//     the seat's next holder's recovery pass — finds the copies still owed
//     and publishes them.
//  3. THE CLEAR ([PendingStore.ClearHandBack]). Stopped before it, the copies
//     are published AND still recorded, so the next reader publishes them
//     again — under the same ids, which is one message to the inbox's same-id
//     dedupe and to the completion ledger, never a second.
//
// It used to publish first and write second, which could not lose the reply
// but could deliver it twice: a crash between the two left the answer
// recorded and its copy on the inbox, and the run was then resumed with the
// reply that had also been worked as an ordinary message.
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
	declined, let, err := c.pending.DeclineAnswer(ctx, run.TurnID, run.LaunchID,
		answer.EventIDs, handBackOf(*answer), fenceOf(run))
	if err != nil {
		// THE WRITE MAY HAVE LANDED, and either way nothing has been
		// published: the retry reads the row and finds the copies owed, or
		// the answer still recorded and declines it again.
		log.ErrorContext(ctx, "sandbox_answer_decline_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error())
		c.scheduleOwed(run.TurnID, answerRetryCeiling)
		return
	}
	if !let {
		// Resumed, declined or relaunched since this attempt read the row:
		// whatever that was is its writer's, a hand-back included.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	c.moveRun(run.AgentHandle, StatusAnswered, answer.declinedTo())
	log.ErrorContext(ctx, "sandbox_answer_requeue_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "attempts", MaxAnswerAttempts,
		"window_s", answerWindow(run).Seconds(), "error", errDetail(cause),
		"detail", "this node could not resume the coding run that asked with the answer "+
			"recorded for it, in every one of its spaced attempts, so the reply is handed "+
			"to the seat as the ordinary message it looks like; the run waits on its "+
			"question again and its box is bounded by pause_ttl_seconds")
	if err := c.deliverHandBack(ctx, declined); err != nil {
		// RECORDED AS OWED, so not lost: the retry publishes it. The seat
		// stays held until it does, so the copy reaches the inbox before
		// the mail waiting behind it is let go and the two are drained —
		// and ordered by when they were posted — together.
		log.ErrorContext(ctx, "sandbox_answer_handback_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error(),
			"detail", "the reply is recorded on the run as owed to the seat and is handed "+
				"back on the next attempt")
		c.scheduleOwed(run.TurnID, answerRetryCeiling)
		return
	}
	c.settleOwed(ctx, run.AgentHandle, run.TurnID)
}

// handBackOf is what letting go of a recorded answer owes the seat's inbox:
// each of its deliveries as the ordinary message it is, under an id derived
// from the original's ([declinedCopyID]).
//
// Their own instants are kept: the copy is the message the person sent, and
// whether it could answer a question is measured from when they sent it.
func handBackOf(answer RecordedAnswer) []HandedBack {
	var out []HandedBack
	for _, ev := range answer.decodedEvents() {
		handed := *ev
		handed.ID = declinedCopyID(ev.ID)
		raw, err := json.Marshal(&handed)
		if err != nil {
			log.Warn("sandbox_answer_copy_unencodable", "event_id", ev.ID.String(),
				"error", err.Error())
			continue
		}
		out = append(out, HandedBack{ID: handed.ID.String(), Original: ev.ID.String(), Event: raw})
	}
	return out
}

// deliverHandBack publishes the copies a run's row owes the seat's inbox and
// then clears them from the row. See [Coordinator.declineAnswer] for why in
// that order; a clear that fails is only logged, because the copies are out
// and the next reader's repeat of them is the same messages again.
func (c *Coordinator) deliverHandBack(ctx context.Context, run PendingRun) error {
	published, err := c.publishCopies(ctx, run.AgentHandle, run.HandBack)
	if len(published) > 0 {
		if _, clearErr := c.pending.ClearHandBack(ctx, run.TurnID, published); clearErr != nil {
			log.WarnContext(ctx, "sandbox_answer_handback_uncleared",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "error", clearErr.Error(),
				"detail", "the copies are published; whoever reads the run next publishes "+
					"them again under the same ids, which the seat's inbox collapses")
		}
	}
	return err
}

// publishCopies publishes copies to a seat's inbox, in order, and reports the
// ids it published — counting one that can never be published, which is
// dropped loudly rather than owed for ever — and the error that stopped it.
func (c *Coordinator) publishCopies(ctx context.Context, handle string, copies []HandedBack) ([]string, error) {
	inbox := topics.AgentInbox(handle)
	if inbox == "" {
		return nil, fmt.Errorf("sandbox: seat %q has no inbox to hand its reply back to", handle)
	}
	var published []string
	for _, copied := range copies {
		var ev events.Event
		if err := json.Unmarshal(copied.Event, &ev); err != nil {
			log.ErrorContext(ctx, "sandbox_answer_copy_undecodable", "agent", handle,
				"copy", copied.ID, "original", copied.Original, "error", err.Error(),
				"detail", "a reply recorded as owed to the seat cannot be decoded and is "+
					"dropped from what the run owes")
			published = append(published, copied.ID)
			continue
		}
		if err := c.queue.Publish(ctx, inbox, &ev); err != nil {
			return published, fmt.Errorf("sandbox: handing %s back to %s: %w", copied.Original, handle, err)
		}
		published = append(published, copied.ID)
	}
	return published, nil
}

// recoverOwed takes over what a previous holder of the seat left owed on a run
// — an answer it recorded and did not resume with, or copies a decline it
// wrote did not publish: the seat's inbox is held behind it, and the first
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

// owe records that a seat owes one of its runs an answer's resume, and takes
// the seat's inbox hold if it is the first thing holding it.
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
// lifts the seat's inbox hold if nothing else needs it.
func (c *Coordinator) settleOwed(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	if r := c.retries[turnID]; r != nil && r.ending != nil {
		// AN ENDING STILL OWES THE SEAT ITS REPLY — kept by the ending this
		// very attempt made — so the series, and the seat's hold, stay.
		c.mu.Unlock()
		return
	}
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
// and every owed hand-back again. The inbox hold is not lifted here — the
// release detached it — see [Coordinator.forgetHold].
func (c *Coordinator) releaseOwed(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
}

// errDetail is an error's text, or empty.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
