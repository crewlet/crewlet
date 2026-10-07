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
// A RUN THAT ENDS BEFORE ANY TURN TOOK THE ANSWER lets it go the same way,
// whoever ends it and on whichever route the answer arrived: the store will not
// delete a row that still holds a reply no turn took ([ErrAnswerOwed]), so the
// ending takes the answer off the row and records its copies as owed in one
// write, and hands them back before it deletes the row ([Coordinator.letGoAnswer],
// [Coordinator.endRecord]). That covers a retry or an inline attempt whose run
// turns out to be over, a resume that broke before its turn began, and the seat's
// next holder reaping a claim whose node stopped before its turn TOOK the answer,
// once the answer's revivals are spent ([RecordedAnswer.TakenAt],
// [Coordinator.reapTail]) — the one fact that tells a reply nobody acted on from
// one a turn has used.
//
// # Spent when it leaves
//
// A reply that leaves its run — taken by a turn, on the inline attempt as on a
// retry, or let go of — has the delivery that carried it recorded as worked in
// the completion ledger at that moment ([CoordinatorOptions.Spent]): at the
// take, and before the let-go's write. A node that recorded the answer, or took
// it inline, and stopped before acknowledging the delivery leaves it to come
// round again, and by then nothing on the run recognises it as the answer it
// was.
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
// PROVEN, never presumed: a row with no anchor, or a delivery with no instant,
// cannot show that the reply came after the question, and does not qualify —
// the delivery goes its ordinary way. Every park records its anchor
// ([PendingStore.MarkAwaiting] refuses one without), and every event carries
// its instant.
func (r Reply) qualifies(run PendingRun) bool {
	if posted := r.Posted(); run.AskedAt.IsZero() || posted.IsZero() || posted.Before(run.AskedAt) {
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
		Text: redact.Secrets(r.Text), Via: types.AnswerViaChat, By: answererOf(r.trigger()),
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
	// for the reply the record still owes the seat: once the answer is let
	// go of and the hand-back is out, the series ends the run on the same
	// terms — license, let-go and announcement. Nil for every other series.
	// See [Coordinator.oweEnding].
	ending *ending

	// revive is a claim the series' next attempt revives before anything
	// else, and which kind it is: a dead claim's revival that did not land,
	// retried under the lease the seat was recovered under
	// ([Coordinator.oweRevival]), or the series' own claim, which reported a
	// failure and may have landed unheld, given back under this node's lease
	// ([Coordinator.suspectClaim]). Nil for every other series.
	revive *revival

	// stop cancels the pending attempt, reporting whether it had not yet
	// started.
	stop func() bool
}

// revival is a claim an owed answer's series revives, and on what terms.
//
// TWO KINDS, AND ONLY ONE IS A LOSS. A claim whose node stopped before its turn
// took the answer is a lost claim, revived by the seat's next holder under the
// two bounds kept on the row ([Coordinator.revivable]), and counted. A claim
// this node made itself and could not confirm is nobody's loss: no node stopped,
// and reading it as one — counting it, and ending the run past the bounds as an
// abandoned tail — turned a coordination store's errors on a healthy node into a
// run announced as lost with a node that never stopped. That one is given back
// uncounted ([Coordinator.giveBackClaim]), and the series that made it is bounded
// by its own attempts instead.
type revival struct {
	// fence is the lease the revival is made under: the seat's recovery for a
	// lost claim, the claim's own for the series' own.
	fence Fence

	// lost is whether the claim died with its node ([Revival.Lost]).
	lost bool
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

// recordAnswer records a chat delivery as the answer to the run it qualifies
// for — see [Coordinator.record].
func (c *Coordinator) recordAnswer(ctx context.Context, run PendingRun, reply Reply) (PendingRun, bool, error) {
	recorded, won, err := c.record(ctx, run, reply.answerOf(c.now()))
	if err != nil || !won {
		return PendingRun{}, won, err
	}
	log.InfoContext(ctx, "sandbox_clarification_answered",
		"turn_id", recorded.TurnID, "via", string(types.AnswerViaChat),
		"conversation", reply.Conv.Identity, "partition", reply.Conv.Partition,
		"run_conversation", recorded.ConversationKey, "run_partition", recorded.PartitionKey,
		"asked_at", recorded.AskedAt, "posted_at", recorded.Answer.PostedAt)
	return recorded, true, nil
}

// record records an answer on the run it answers, on either route, and makes
// the run owe the resume it drives ([StatusAnswered]).
//
// The resume's first attempt runs INLINE, on the caller's delivery, which is
// what keeps the ordinary case as immediate as it ever was; whatever it
// concludes, the delivery is spent — the answer is on the row, and from here
// on the row resumes it or hands it back, never the delivery.
//
// UNDER THE SEAT'S LEASE, and never on a seat this node does not hold
// ([ErrSeatNotHeld], before anything is written): the answer is the seat
// holder's to record, because only the holder drives what is recorded. A
// node that lost the seat and recorded anyway left the answer on a run its
// successor had already recovered as awaiting — recorded, and driven by
// nobody until the seat moved again. The store refuses a lease a newer one has
// overtaken the same way, for a node that has not noticed yet.
func (c *Coordinator) record(ctx context.Context, run PendingRun, answer RecordedAnswer) (PendingRun, bool, error) {
	fence, held := c.leaseOf(run.AgentHandle)
	if !held {
		return PendingRun{}, false, notHeld(ctx, run.AgentHandle, run.TurnID, "recording an answer")
	}
	recorded, won, err := c.pending.RecordAnswer(ctx, run.TurnID, run.LaunchID, answer, fence)
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
	return recorded, true, nil
}

// resumeOwed takes the seat's inbox hold for an owed answer and makes one
// attempt at its resume — the inline one, on the delivery that recorded it.
//
// THE DELIVERY IS SPENT WHATEVER THE ATTEMPT CONCLUDES: the answer is the
// run's, and a run that ends under this attempt before any turn took the reply
// hands it back to the seat through its row, with the hold kept until the copy
// is out ([Coordinator.resumeAndSettle]).
func (c *Coordinator) resumeOwed(ctx context.Context, run PendingRun) {
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.attemptOwed(ctx, run)
}

// attemptOwed makes one attempt at an owed answer's resume, and settles what
// the attempt leaves: done (the hold lifted), failed (another attempt
// scheduled, or the answer declined once the bounds are spent), or the run
// gone — and then a reply no turn took went back to the seat through the row,
// recorded as owed BEFORE the run's record was deleted
// ([PendingStore.OweHandBack]), so a crash between the ending and the publish
// cannot lose it. See [Coordinator.resumeAndSettle].
func (c *Coordinator) attemptOwed(ctx context.Context, run PendingRun) {
	answer := run.Answer
	if answer == nil {
		// An answered row always carries its answer — the store refuses
		// every write that would leave one without it — so one that does
		// not was written by something this build does not understand,
		// and there is nothing here to resume it with.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	fence, held := c.leaseOf(run.AgentHandle)
	if !held {
		// THE SEAT IS HELD ELSEWHERE, and with it the answer: the holder's
		// recovery pass found the run answered — or will, when it fences
		// the row — and drives it from there. This node claims nothing.
		_ = notHeld(ctx, run.AgentHandle, run.TurnID, "claiming an answer for its resume")
		c.dropOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID, RecordedAnswerTail(run.LaunchID), fence)
	if err != nil {
		// THE CLAIM MAY HAVE LANDED, and one that did is a claim nothing on
		// this node drives: no resume holds it, and no recovery pass comes
		// until the seat moves. So the series' next attempt checks before
		// anything else, and revives it under this node's lease if it did
		// ([Coordinator.suspectClaim]), as the seat's next holder revives a
		// claim whose node stopped. It used to be left for that next
		// holder: the run held in a claim, the seat busy behind it and the
		// person's answer unanswered, until the seat happened to move.
		c.suspectClaim(run, fence)
		c.owedFailed(ctx, run, fmt.Errorf("sandbox: claiming %s for its recorded answer: %w",
			run.TurnID, err))
		return
	}
	if !won {
		// Resumed already (an attempt on another node across a handoff),
		// declined, replaced by the next job, or owned by a newer lease:
		// not this attempt's.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	c.moveRun(claimed.AgentHandle, StatusAnswered, StatusResumed)
	disposition, err := c.resumeAndSettle(ctx, claimed, answerText(claimed, answer.Text, answer.attribution()),
		true, answer.trigger(), runOutcome{
			InputTokens: claimed.ParkedInputTokens, OutputTokens: claimed.ParkedOutputTokens,
		})
	if disposition == AnswerDeferred {
		// The claim went back: the run is [StatusAnswered] again with
		// the same answer on it, owed the same resume.
		c.owedFailed(ctx, claimed, err)
		return
	}
	c.settleOwed(ctx, claimed.AgentHandle, claimed.TurnID)
	if errors.Is(err, errRunMovedOn) {
		// NOT THIS NODE'S TO SAY: the run moved on to another holder
		// before this attempt could end it, and that holder announces
		// what becomes of the answer — resumed, as a revived claim's is.
		// Said here, it would be told as gone first.
		return
	}
	outcome := answeredAs(disposition)
	if outcome == types.AnswerGone && answer.Via == types.AnswerViaOperator && len(answer.Events) > 0 {
		// ITS COPY SAYS SO. The run ended before any turn took the answer,
		// so its ending handed the answer by turn back to the seat's inbox,
		// where it is spent as reaching a run that is gone and announced
		// there ([Coordinator.AnswerByTurn]) — once, through the outbox that
		// survives a crash, rather than here and again there.
		return
	}
	c.announceAnswered(ctx, claimed, answer.Via, outcome, answer.By, answer.BySeat)
}

// owedFailed counts one failed resume of an owed answer, and schedules the next
// attempt or — once [MaxAnswerAttempts] or [answerWindow] is spent — declines
// the answer.
//
// # A claim the failure may have left is revived before the decline
//
// A failed attempt whose CLAIM reported the failure may have left that claim
// on the row, landed and held by nothing ([Coordinator.suspectClaim]). A decline
// made now would read the row as the answered run it no longer is: the store
// refuses to let go of an answer from a claim, the series settles on that
// refusal, and the run sits in the claim — the person's answer untaken on it,
// the delivery already spent, nothing handed back, and the seat held behind it
// on the next recount — until the seat changes hands. So a series that suspects
// its claim is never declined here, on its last attempt or past its window any
// more than before them: its next attempt runs at once, gives the claim back to
// the answer if it landed ([Coordinator.retryRevival]), and then declines the
// answered run and hands the reply back exactly once, through the same spent
// bound every other decline takes.
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
	suspect := r.revive != nil
	c.mu.Unlock()

	if !live && suspect {
		log.WarnContext(ctx, "sandbox_answer_resume_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "failures", failures,
			"retry_in_s", 0, "error", detail,
			"detail", "the answer's attempts are spent, and its last claim may have landed with "+
				"nothing driving it; the claim is given back to the answer first, and the answer "+
				"is then let go of, once")
		c.scheduleOwed(run.TurnID, 0)
		return
	}
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
	var kept *ending
	var revive *revival
	if r != nil {
		r.stop, r.waiting = nil, ""
		handle, launch = r.handle, r.launch
		spent = r.spent(c.now())
		kept, revive = r.ending, r.revive
	}
	c.mu.Unlock()
	if r == nil {
		return
	}
	if _, held := c.leaseOf(handle); !held {
		// THE SEAT MOVED, and whatever this series still owed the run —
		// its resume, a hand-back, an ending it kept, a claim it suspects —
		// is read off the row by the holder's recovery pass, which this
		// node's writes would only race.
		_ = notHeld(ctx, handle, turnID, "retrying an owed answer")
		c.dropOwed(ctx, handle, turnID)
		return
	}
	run, found, err := c.pending.Get(ctx, turnID)
	if err != nil {
		if kept != nil || revive != nil {
			// A READ IS NOT A RESUME, and neither an ending nor a revival
			// owes an attempt: the row is read again on the hand-back's
			// own spacing.
			c.scheduleOwed(turnID, answerRetryCeiling)
			return
		}
		c.owedFailed(ctx, PendingRun{TurnID: turnID, AgentHandle: handle, LaunchID: launch},
			fmt.Errorf("sandbox: reading run %s for its recorded answer: %w", turnID, err))
		return
	}
	if !found {
		// GONE, and with it whatever the series still owed the row: an
		// ending somebody finished, a claim somebody ended.
		c.endingDone(turnID)
		c.revivalDone(turnID)
		c.settleOwed(ctx, handle, turnID)
		return
	}
	if kept != nil {
		// THE RUN WAS ALREADY ENDED, and kept its record only because the
		// ending could not be finished: the reply it owed the seat, or the
		// delete the store did not take. The record goes now, on the terms
		// the ending was decided with — its license, its let-go, the box it
		// still has to reclaim and the announcement it has not made yet —
		// and the ending itself publishes whatever the row still owes
		// before it deletes it, so it knows to say the reply went back. One
		// that cannot be finished even now is kept again, by
		// [Coordinator.endRecord] itself.
		c.endingDone(turnID)
		decided, err := c.endRecord(ctx, run, *kept)
		ended := decided != nil
		if err != nil {
			log.WarnContext(ctx, "sandbox_finish_kept", "turn_id", turnID, "error", err.Error(),
				"detail", "the run's ending is kept again; this node tries it on the hand-back's "+
					"spacing, and the seat's next recovery pass reaps the run if the seat moves "+
					"first")
		}
		// RECOUNTED: the kept record was still a live row to every recount
		// made while it waited, and the seat's counts are the store's answer.
		c.syncSeat(ctx, handle)
		if ended {
			// THE RUN IS OVER, a claim the series suspected with it.
			c.revivalDone(turnID)
		}
		moved := run.Status != StatusAnswered || run.LaunchID != launch
		if err != nil || ended || (moved && revive == nil) {
			c.settleOwed(ctx, handle, turnID)
			return
		}
		// NOT THE ENDING'S TO END after all: the claim it was decided on had
		// been handed back — the hand-back reported a failure and landed —
		// and the run is answered again, owed the very resume this series
		// is for. It goes on as one. (A run that moved elsewhere goes on to
		// the claim the series suspects, which only the series looks at.)
		if !moved {
			log.InfoContext(ctx, "sandbox_kept_ending_declined",
				"turn_id", turnID, "agent", handle,
				"detail", "the run's claim had been handed back after all, so it is resumed with "+
					"its answer rather than ended")
		}
	}
	if revive != nil {
		var owed bool
		if run, owed = c.retryRevival(ctx, run, launch, *revive); !owed {
			return
		}
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
	// A RUN THAT ENDS UNDER THE ATTEMPT before any turn took the reply —
	// settled and announced as lost — owes the seat the reply, because the
	// delivery that carried it was spent when the answer was recorded and
	// nothing else would ever bring it back. The ending lets the answer go
	// through the row, recording its copies as owed, and hands them back
	// before it deletes the row (see [Coordinator.letGoAnswer]). It used to
	// be published here, AFTER the ending had deleted the only record of
	// it, so a crash between the two lost it.
	c.attemptOwed(ctx, run)
}

// oweEnding keeps an ending this node decided and could not finish
// ([Coordinator.endRecord]): the row was not deleted because the answer it lets
// go of could not be recorded as owed, the copies it carries could not be
// published, or the store did not take the delete. The seat's inbox is held
// behind it, as it is behind a decline's, and a retry on the hand-back's
// spacing finishes the ending ON THE TERMS IT WAS DECIDED ON — its box
// included, where the ending reclaims that after the delete — or, if the seat
// moves first, its next holder's recovery pass reaps the row and hands back
// whatever it still owes.
//
// A DELETE THE STORE DID NOT TAKE IS KEPT TOO. It used to be left for the
// seat's next recovery pass, which comes only when the seat moves — and the
// record it left was a live row of its seat: a claim, which holds the seat's
// inbox on every recount, so the seat took no mail at all until it changed
// hands.
func (c *Coordinator) oweEnding(ctx context.Context, run PendingRun, e ending) {
	if run.AgentHandle == "" {
		return
	}
	e.whileIn = slices.Clone(e.whileIn)
	c.mu.Lock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.ending = &e
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, answerRetryCeiling)
}

// oweRevival keeps a dead claim's revival that did not land
// ([Coordinator.reapTail]) for this node's retry, under the lease the seat was
// recovered under. The seat's inbox is held behind it, as it is behind any
// answer the seat owes a run, and a retry on the hand-back's spacing reaps the
// claim afresh ([Coordinator.retryRevival]); if the seat moves first, its next
// holder's recovery pass does.
//
// NOT A REASON TO END THE RUN. The store did not confirm the revival — the
// write failed, or the claim moved under it and could not be read again — and
// that says nothing about whether the run can still be resumed with its answer.
// It used to fall straight through to the reap: the run ended and the answer
// handed to the seat as an ordinary message, over a store blip.
func (c *Coordinator) oweRevival(ctx context.Context, run PendingRun, fence Fence, cause error) {
	log.WarnContext(ctx, "sandbox_answer_revive_owed",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "error", cause.Error(),
		"detail", "the dead claim's answer could not be given back to its run yet; the claim "+
			"stays fenced to this node, which tries again on the hand-back's spacing, and the "+
			"seat's mail waits behind it")
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
	r.revive = &revival{fence: fence, lost: true}
	c.mu.Unlock()
	c.owe(ctx, run.AgentHandle, run.TurnID)
	c.scheduleOwed(run.TurnID, answerRetryCeiling)
}

// suspectClaim marks an owed answer's series whose claim reported a failure:
// the claim may have landed, unheld by any resume, so the series' next attempt
// gives it back under the lease it was taken under if it did
// ([Coordinator.giveBackClaim]) and goes on as the answered run it then is —
// and goes on as it would have if it did not. UNCOUNTED: no node stopped, so it
// is no lost claim ([revival]).
//
// A CLAIM ONLY THE SERIES' OWN FAILURE MARKS, never any claim the retry happens
// to find: the row reads [StatusResumed] for the length of every resume, and a
// retry that revived whatever claim it found would revive one an inline attempt
// on this node is driving that very moment. The store would keep the person
// answered once — the revival and the take are exclusive — but the resume
// in flight would fail for nothing.
//
// fence is the lease the claim was made under, which a give-back must carry.
func (c *Coordinator) suspectClaim(run PendingRun, fence Fence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.revive = &revival{fence: fence}
}

// retryRevival is a scheduled attempt at a revival [Coordinator.oweRevival]
// kept, or one [Coordinator.suspectClaim] suspects is owed, against the row as
// the store holds it now. A LOST claim still on the row is reaped afresh under
// the lease the series holds ([Coordinator.reapTail]) — revived, or ended where
// the run can no longer be resumed; the series' OWN claim is given back to its
// answer uncounted ([Coordinator.giveBackClaim]), never through the reap's
// bounds or its ending, because no node stopped. Anything else is what the run
// has become since. Reports the row to go on with as an owed answer, or false
// where the series has nothing more to do here: settled, or kept again.
func (c *Coordinator) retryRevival(ctx context.Context, run PendingRun, launch string, rv revival,
) (PendingRun, bool) {
	handle, fence := run.AgentHandle, rv.fence
	c.mu.Lock()
	if r := c.retries[run.TurnID]; r != nil {
		r.revive = nil
	}
	c.mu.Unlock()
	switch {
	case run.Ending != nil || run.Status != StatusResumed:
	case !rv.lost:
		given, owed := c.giveBackClaim(ctx, run, launch, fence)
		if !owed {
			return PendingRun{}, false
		}
		run = given
	default:
		moved, did := c.reapTail(ctx, run, fence.Owner, fence.Epoch)
		// RECOUNTED: the claim was nothing this node counted, and what it
		// became — an answered run, or no run — is the store's answer.
		c.syncSeat(ctx, handle)
		switch did {
		case reapOwed:
			return PendingRun{}, false
		case reapRevived, reapMoved:
			run = *moved
		case reapLeft, reapEnded:
			c.settleOwed(ctx, handle, run.TurnID)
			return PendingRun{}, false
		}
	}
	if run.Ending != nil {
		// AN ENDING DECIDED while the revival waited — by the run's own
		// series, or a node that lost the seat — is this holder's to
		// finish, on the terms recorded, as a recovery pass finishes one.
		if _, err := c.endRecord(ctx, run, ending{fence: fence, whileIn: Active,
			launch: EveryLaunch}); err != nil {
			log.WarnContext(ctx, "sandbox_ending_kept", "turn_id", run.TurnID, "error", err.Error(),
				"detail", "an ending decided while the run's revival waited could not be finished "+
					"yet; this node finishes it, and the seat's mail waits behind it")
		}
		c.syncSeat(ctx, handle)
		c.settleOwed(ctx, handle, run.TurnID)
		return PendingRun{}, false
	}
	// Given back, revived, or moved on: what it is now decides what is owed.
	return run, true
}

// giveBackClaim gives a claim the series made itself, and could not confirm,
// back to the answer it was taken for ([PendingStore.ReviveAnswer], uncounted):
// the claim's write reported a failure and landed, and no resume holds it. It
// reports the row to go on with as the owed answer — given back, or whatever
// the run has become since — or false where the series has nothing more to do
// here.
//
// ONLY THE SERIES' OWN CLAIM — its job, its answer, its lease — and never
// through [Coordinator.reapTail]: that is the seat's next holder reaping a claim
// whose node stopped, and its two bounds and its ending ([types.SandboxFailureAbandoned],
// "the node that owned this seat stopped mid-run") are about a node that stopped.
// Here none did, so nothing is counted on the row and nothing ends the run: the
// series goes on — resuming, or declining once its own attempts are spent.
//
// A GIVE-BACK THE STORE DOES NOT CONFIRM is suspected again and retried on the
// hand-back's spacing, as a revival that did not land is ([Coordinator.oweRevival]):
// the seat's mail waits behind it either way. One the store refuses because a
// TURN TOOK THE ANSWER is left to that turn: the take requires the claim's own
// lease, which only a resume of this node's could have held — and that resume
// settles the run itself.
func (c *Coordinator) giveBackClaim(ctx context.Context, run PendingRun, launch string, fence Fence,
) (PendingRun, bool) {
	handle := run.AgentHandle
	if run.LaunchID != launch || run.Answer == nil {
		// NOT THE CLAIM THE SERIES MADE: the run moved on — relaunched, or
		// claimed for another job's completion — and that claim is its own
		// claimant's.
		c.settleOwed(ctx, handle, run.TurnID)
		return PendingRun{}, false
	}
	given, won, err := c.pending.ReviveAnswer(ctx, run.TurnID, Revival{
		Launch: launch, Answer: run.Answer.EventIDs, Fence: fence,
	})
	// RECOUNTED: the claim was nothing this node counted, and what it became
	// is the store's answer.
	c.syncSeat(ctx, handle)
	switch {
	case errors.Is(err, ErrAnswerTaken):
		log.ErrorContext(ctx, "sandbox_answer_claim_taken",
			"turn_id", run.TurnID, "agent", handle,
			"detail", "a claim this node made and could not confirm had its answer taken by a "+
				"turn; the claim is that turn's to settle, and this answer's series ends here")
		c.settleOwed(ctx, handle, run.TurnID)
		return PendingRun{}, false
	case err != nil:
		log.WarnContext(ctx, "sandbox_answer_claim_give_back_failed",
			"turn_id", run.TurnID, "agent", handle, "error", err.Error(),
			"detail", "the claim this node made and could not confirm could not be given back "+
				"to its answer yet; it is tried again, and the seat's mail waits behind it")
		c.suspect(run.TurnID, fence, answerRetryCeiling)
		return PendingRun{}, false
	case won:
		log.InfoContext(ctx, "sandbox_answer_claim_given_back",
			"turn_id", run.TurnID, "agent", handle,
			"detail", "the claim this node made reported a failure and had landed, so no resume "+
				"held it; it is given back to the answer, uncounted, and the answer is resumed")
		return given, true
	}
	// MOVED UNDER THE GIVE-BACK — given back already, ended, or claimed by a
	// newer lease — and read again, so the series goes on with what it is now.
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	switch {
	case err != nil:
		c.suspect(run.TurnID, fence, answerRetryCeiling)
		return PendingRun{}, false
	case !found:
		c.settleOwed(ctx, handle, run.TurnID)
		return PendingRun{}, false
	}
	return latest, true
}

// suspect marks the series' own claim as suspected again and arms its next
// attempt — see [Coordinator.suspectClaim].
func (c *Coordinator) suspect(turnID string, fence Fence, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.retries[turnID]
	if r == nil {
		return
	}
	r.revive = &revival{fence: fence}
	c.scheduleOwedLocked(turnID, delay)
}

// revivalDone forgets a claim the series suspected or a revival it owed, for a
// series whose run is over.
func (c *Coordinator) revivalDone(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.retries[turnID]; r != nil {
		r.revive = nil
	}
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
	// SPENT BEFORE IT IS LET GO, for the reason [Coordinator.letGoAnswer]
	// gives: the copy is the reply from here on, and the original's delivery
	// coming round after it — a node that recorded the answer and stopped
	// before acknowledging it — must not be a second message.
	c.spend(ctx, run.AgentHandle, *answer)
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
//
// NOT WHILE THE SERIES STILL OWES THE ROW SOMETHING: an ending it kept, or a
// claim it suspects ([answerRetry.revive]). Either is a run whose only driver is
// this series, and a settle that dropped it — on a refusal that only says the
// row is not what the settling path expected — left a claim with the person's
// answer on it that nothing drove. The series stays, with an attempt armed if
// none is, and the seat's hold with it.
func (c *Coordinator) settleOwed(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	if r := c.retries[turnID]; r != nil && (r.ending != nil || r.revive != nil) {
		// AN ENDING STILL OWES THE SEAT ITS REPLY — kept by the ending this
		// very attempt made — or A CLAIM IS STILL SUSPECTED, which only this
		// series will ever look at: the series, and the seat's hold, stay.
		if r.stop == nil {
			c.scheduleOwedLocked(turnID, answerRetryCeiling)
		}
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

// dropOwed forgets one owed answer's series on a seat this node does not hold,
// whatever it still owed the row: an ending it kept, a claim it suspects, a
// revival it was retrying. Each is on the row, and the holder's recovery pass
// reads the row and finishes, revives or reaps what it finds — where this
// node's own writes, under a lease the holder has fenced or is about to, would
// only race it. Unlike [Coordinator.settleOwed], which keeps such a series for
// the row it alone drives.
func (c *Coordinator) dropOwed(ctx context.Context, handle, turnID string) {
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

// errDetail is an error's text, or empty.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
