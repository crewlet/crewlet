package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/redact"
)

// An answer that is recorded and still owed a resume. The first qualifying
// reply is recorded on the row ([PendingStore.RecordAnswer]) and its delivery
// acknowledged; resuming is this package's, on its own backoff
// ([answerRetryDelay], [MaxAnswerAttempts], [answerWindow]), with the seat's
// inbox held behind it ([SeatHold]). An answer that cannot be resumed is
// declined ([Coordinator.declineAnswer]) or let go ([Coordinator.letGoAnswer]),
// exactly once; a reply too large is recorded as its refusal (ADR-0114); a
// retry the seat's conditions refuse waits for their signal
// ([Coordinator.Readmit]). See the package doc, "Owed answers".

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

// [SeatFull] and [ConversationBusy] (seatcap.go) are what an owed answer's
// resume waits on while its seat cannot take another run.

// SeatLost is a control-topic delivery refused because this node no longer
// holds the seat it is for: [ErrSeatNotHeld], with the node's own lease agreeing
// ([Coordinator.OnEvent]). The delivery belongs to the seat's holder, and it
// does not clear on this node, which is letting the seat go, so it is handed
// on to the holder rather than retried here.
//
// NOT EVERY [ErrSeatNotHeld]. The run store answers it too for a row a newer
// lease has stamped, and while this node's lease still stands the two disagree
// for at most a heartbeat, which settles which is right; handing a delivery on
// for a seat this node does hold would leave it deaf to it.
const SeatLost Condition = "seat_lost"

// Draining is a resume refused because this node is draining and starts no turn
// ([TurnGate]). Nothing signals its end: it ends when the drain hands the seat
// back ([Coordinator.ReleaseSeat]) or the node stops. So a completion's
// delivery refused on it is handed on to the seat's next holder, never
// acknowledged (internal/engine's controlOutcome), as [SeatLost] is, and owes
// nothing on the job's record ([Coordinator.oweRefused]); this package names it
// because that decision is its own. An answer waiting on it waits for the
// release.
const Draining Condition = "drain"

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
	// when only an event can lift it. An instant at or before the refusal is
	// one the clock has already passed without lifting it, so it is read as
	// zero ([Coordinator.waitOwed]).
	Until time.Time

	// Window is the budget window an executor launch was refused below its
	// floor in, the one Until is the end of, where the refusal is that
	// launch's: a resumed segment's relaunch the seat's budget held. The
	// release that hands the claim back counts it onto the job's record
	// ([Release.RelaunchRefusedIn]), a held completion's and a turned-back
	// answer's alike, so the turn's next relaunch, in a later window, waits
	// for its brief's room no longer (internal/engine's launchfloor.go). Zero
	// for every other refusal: a gate's, which no launch met, and a round's.
	Window BudgetWindow
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
// for the signal that clears them before it re-checks on its own: a backstop
// for a signal never wired, not a schedule, since every condition changes on an
// observed event ([Coordinator.Readmit]) or its [Refusal.Until]. Five minutes
// is one admission check per refused answer per five minutes, inside the
// 30-minute default `pause_ttl_seconds`. A kept completion's backstop too
// ([Coordinator.keepLocked]), matching the waiter's ceiling
// ([reannounceCeilingPolls]), save ones held until an instant
// ([Coordinator.keptRefireIn]). See the package doc, "Kept completions".
const answerWaitBackstop = 5 * time.Minute

// Reply is a delivery offered to a parked run as the answer to its question.
type Reply struct {
	// Conv is where the delivery came from — see [ConversationRef].
	Conv ConversationRef

	// Text is what the delivery says, in its writers' own words: each
	// message as it was written, under its writer's name where the delivery
	// holds more than one writer's. It is the reply the resumed run is
	// handed, and what the person who gave it reads back as their answer
	// ([AnswerWords.Text]), so never the prompt the delivery would wake the
	// seat with, which the engine writes for the seat's own turn.
	Text string

	// Label is what the fence around the delivery's message says of Text,
	// for the seat it was delivered to: where it was read, who wrote it, and
	// how far the run may rely on it, as the trigger's own fence labelled it.
	// The run the reply carries on reads it inside a fence of its turn's
	// token under it (ADR-0040). Nil where the delivery holds words no
	// surface labelled, which the run reads defused instead.
	Label *provenance.Label

	// Events are the delivery's events, oldest first.
	Events []*events.Event

	// Writer is the seat the delivery's writer holds, resolved by the engine
	// through its party registry and never from the delivery's words: the
	// newest event's writer, which after the partition function is the
	// delivery's only one (one turn works for one writer, ADR-0109). Empty
	// for a writer who holds no seat (a stranger, a credential acting as no
	// seat, the engine itself), whose reply answers no parked question
	// ([Reply.putTo]).
	Writer string

	// Says is whether the delivery carries words its writer addressed to the
	// conversation (a message, a comment, a review), judged by the engine from
	// the event's surface. Only such a delivery answers a parked question.
	Says bool

	// WorkingFor is whom the delivery's writer wrote it for: the carrier an
	// agent's write states (ADR-0109), and the one the engine's record about
	// an agent's earlier act carries, read off the newest event by the rule a
	// wake is read by; nil for a person's or a credential's writer, for a
	// vendor's delivery and for an agent's write made for nobody else. A reply
	// answers a run only when it was written for whom the run works for
	// ([Reply.putTo]), and the engine's answers none, its writer holding no
	// seat (Writer).
	WorkingFor *types.WorkingFor
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
// AT OR AFTER, with no allowance for clock skew: a question is announced after
// [PendingRun.AskedAt] is taken, so a reply was written after somebody read it,
// far longer than NTP skew; an allowance would admit only messages written
// before the question existed.
//
// PROVEN, never presumed: a row with no anchor or a delivery with no instant
// cannot show the reply came after, and does not qualify (every park records
// its anchor, [PendingStore.MarkAwaiting] refuses one without).
func (r Reply) qualifies(run PendingRun) bool {
	if !r.Says {
		return false
	}
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
// QUALIFIES for and whose question its writer was PUT TO ([Reply.putTo]).
//
// STILL WAITING BY THE STORE'S OWN RULE ([PendingRun.WaitsOn]), never by the
// status alone: a run whose ending is decided keeps its status until the
// ending deletes its row, and the store records no answer on it. Matched on
// its status, a reply to it lost the record, was matched again to the same
// run on every read, and came back deferred as a seat whose runs kept
// changing, until the ending finished — a reply too large for the run as well,
// once the store refused that only to a run it would have recorded it on.
func (r Reply) Best(runs []PendingRun) (PendingRun, bool) {
	var parked []PendingRun
	for _, run := range runs {
		if run.WaitsOn(run.LaunchID) && r.qualifies(run) && r.putTo(run) {
			parked = append(parked, run)
		}
	}
	return r.Conv.Best(parked)
}

// putTo reports whether run's question was put to this delivery's writer: a
// seat of its audience ([PendingRun.AudienceHandles]) or its requester
// ([PendingRun.Requester]).
//
// ONE TURN WORKS FOR ONE WRITER (ADR-0109): a reply from anyone else runs as
// the ordinary message it is and the run stays parked. A row naming neither
// admits nobody from the conversation; the dashboard's route still answers it.
// Whether the holder may act NOW is asked once a run would take the reply
// ([AudienceResolver.MayAnswer]). AND ONE TURN WORKS FOR ONE WHOM: the reply
// must be written for whom the run works for ([Reply.writtenForTheRun]). A wait
// the engine answers ([PendingRun.EngineAnswers]) is put to nobody: mail in its
// conversation waits aside behind it (ADR-0176).
func (r Reply) putTo(run PendingRun) bool {
	return r.Writer != "" && !run.EngineAnswers() &&
		(run.Requester == r.Writer || slices.Contains(run.AudienceHandles, r.Writer)) && r.writtenForTheRun(run)
}

// writtenForTheRun reports whether the reply was written for whom run works
// for. NIL ALWAYS MATCHES: the writer spoke for itself, safe because a record
// skips its carrier only where its decide proves no agent but the writer may
// start a turn (ADR-0109). OTHERWISE both sides are read as the ceiling reads
// them ([runPrincipal], [carriedPrincipal]) and compared by kind and the
// seat's handle or credential's login. Whether a node can place a login is left
// out, since it differs by node; such a reply runs as its own turn (the safe
// direction). A credential's apps are never compared: the resumed run narrows
// them on every call, and comparing would refuse a genuine reply.
func (r Reply) writtenForTheRun(run PendingRun) bool {
	if r.WorkingFor == nil {
		return true
	}
	return carriedPrincipal(r.WorkingFor, run.AgentHandle) == runPrincipal(run)
}

// principal is whom a run works for, or whom a reply was written for, as the
// answer rule compares them: a kind, and the seat's handle or the credential's
// login. The run's own work and a stranger are kinds of their own.
type principal struct {
	kind string
	id   string
}

const (
	principalOwn        = "own"
	principalStranger   = "stranger"
	principalSeat       = "seat"
	principalCredential = "credential"
)

// runPrincipal is whom run works for, read in the order the ceiling composes
// it: its WorkingFor where set, else its Requester where set and not its own
// seat, else its RequesterLogin with RequesterApps, else its own work.
func runPrincipal(run PendingRun) principal {
	switch {
	case run.WorkingFor != nil:
		return carriedPrincipal(run.WorkingFor, run.AgentHandle)
	case run.Requester != "" && run.Requester != run.AgentHandle:
		return principal{kind: principalSeat, id: run.Requester}
	case run.RequesterLogin != "":
		return credentialPrincipal(run.RequesterLogin, len(run.RequesterApps))
	}
	return principal{kind: principalOwn}
}

// carriedPrincipal is a carrier as the ceiling reads it before it asks the
// directory: a seat naming the run's own seat is its own work, a seat with an
// empty handle and a kind this build does not know are a stranger, and a
// credential is read by [credentialPrincipal].
func carriedPrincipal(w *types.WorkingFor, own string) principal {
	switch w.Kind {
	case types.WorkingForSeat:
		switch handle := strings.TrimSpace(w.Seat); handle {
		case "":
			return principal{kind: principalStranger}
		case own:
			return principal{kind: principalOwn}
		default:
			return principal{kind: principalSeat, id: handle}
		}
	case types.WorkingForCredential:
		return credentialPrincipal(w.Login, len(w.Apps))
	}
	return principal{kind: principalStranger}
}

// credentialPrincipal is a credential as the ceiling reads it before it asks
// the directory: one carrying no apps is a stranger on every node, the first
// thing the ceiling decides of it, and so is one with no login, which no
// directory places.
func credentialPrincipal(login string, apps int) principal {
	login = strings.TrimSpace(login)
	if login == "" || apps == 0 {
		return principal{kind: principalStranger}
	}
	return principal{kind: principalCredential, id: login}
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

// answerOf is what recording this delivery writes: the words the seal carries
// and the record they are sealed onto ([Coordinator.record]). The words are
// read only as far as an answer's share ([MaxAnswerBytes]): words whose
// redaction passes it are sealed as its first MaxAnswerBytes+1 bytes
// ([redact.Prefix]), which alone weigh the record past the share, so the store
// refuses it as it refuses every answer past it ([answerFits]).
func (r Reply) answerOf(now time.Time) (AnswerWords, RecordedAnswer) {
	who := chatAnswerer(r.trigger())
	text, _ := redact.Prefix(r.Text, MaxAnswerBytes+1)
	words := AnswerWords{
		// Redacted HERE, at the one place the reply becomes a record a
		// second store keeps: it is sealed onto the run's row, and handed
		// as it is to the run's harness — the asking session's next
		// message, or a fresh run's brief — which the box and the harness's
		// own session keep.
		Text: text, Via: types.AnswerViaChat,
		By: who.by, ByKind: who.kind, OperatorID: who.operatorID,
		Label: r.Label,
	}
	answer := RecordedAnswer{EventIDs: r.ids(), PostedAt: r.Posted(), RecordedAt: now}
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
	return words, answer
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

	// release is a claim's hand-back the coordination store did not answer
	// (a collection's, a park's or a resume's, an answer's own included),
	// which the series' next attempt makes again before anything else,
	// unless a claim of its job has taken it over or is on its way: the
	// claim is this node's until the store answers it, and no claim of its
	// job is made here meanwhile ([Coordinator.oweRelease],
	// [Coordinator.claimCompletion], claimrelease.go). Nil for every other
	// series.
	release *claimRelease

	// unread is a series whose release gave its claim back to running and
	// has landed, and which has not read the row since ([Coordinator.retryOwed]).
	// Such a run owes the series no answer, only what a read of it says (a
	// decline's copies it still owes the seat, say), so a read the store does
	// not answer is no failed attempt at an answer, and is made again on the
	// hand-back's spacing until one is answered. Counted, a store that took
	// the release and answered no read after it ran the series of a
	// completion's claim through an answer's attempts, each logged as an
	// answer's failed resume, and gave up as unreadable an answer the run
	// never had. A release that gave an answer's claim back leaves it false:
	// that series is the answer's, and from its second read after the release
	// a read the store does not answer counts as an answer series' reads do.
	unread bool

	// silent is when this node first asked the box's host of the stopped run
	// this series ends, under that ending, and heard nothing — by this node's
	// own clock; zero until then, and cleared where the host answers busy (it
	// is there) ([Coordinator.hostSilence]). Not carried to the seat's next
	// holder, which counts from its own first unanswered ask, as the fleet's
	// waiter counts a launch node's absence from its own first sight.
	silent silence

	// stop cancels the pending attempt, reporting whether it had not yet
	// started.
	stop func() bool
}

// silence is a box's host heard nothing from since an instant of this node's
// clock, under one ending of the run ([answerRetry.silent]).
type silence struct {
	ending string
	since  time.Time
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
	words, answer := reply.answerOf(c.now())
	recorded, won, err := c.record(ctx, run, words, answer)
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

// recordRefusal records a chat delivery the store refused as too large to be
// the answer to the run it qualifies for ([ErrAnswerTooLarge]) as the REFUSAL
// that is that question's outcome ([PendingStore.RecordRefusal], ADR-0114):
// the deliveries it was made of, when the newest was posted and who gave it,
// sealed with the figures it was refused on — and none of its words, which
// stay in the conversation and in the delivery the seat's inbox keeps until
// the refusal leaves the run ([AnswerRefused]). See [Coordinator.record].
//
// FROM THE FIGURES THE STORE REFUSED IT ON, never measured again here: the
// answer the store weighed is the one a run would have taken (past the share,
// its words' opening: [Reply.answerOf]), and the figures the run, the record
// and the dashboard state are what it weighed there.
func (c *Coordinator) recordRefusal(ctx context.Context, run PendingRun, reply Reply,
	tooLarge *AnswerTooLargeError,
) (PendingRun, bool, error) {
	who := chatAnswerer(reply.trigger())
	refusal := AnswerRefusal{EventIDs: reply.ids(), Bytes: tooLarge.Weighs, MaxBytes: tooLarge.May,
		Address: addressOf(ctx, reply.trigger())}
	words := AnswerWords{Via: types.AnswerViaChat, By: who.by, ByKind: who.kind,
		OperatorID: who.operatorID, Refusal: &refusal}
	answer := RecordedAnswer{EventIDs: reply.ids(), PostedAt: reply.Posted(), RecordedAt: c.now()}
	recorded, won, err := c.record(ctx, run, words, answer)
	if err != nil || !won {
		return PendingRun{}, won, err
	}
	log.InfoContext(ctx, "sandbox_answer_refused",
		"turn_id", recorded.TurnID, "launch_id", recorded.LaunchID, "via", string(types.AnswerViaChat),
		"conversation", reply.Conv.Identity, "partition", reply.Conv.Partition,
		"messages", refusal.Messages(), "bytes", refusal.Bytes, "max_bytes", refusal.MaxBytes,
		"asked_at", recorded.AskedAt, "posted_at", recorded.Answer.PostedAt, "error", tooLarge.Error(),
		"detail", "a reply past what a run's row carries as its answer is recorded as that "+
			"question's refusal, with none of its words, and the run is resumed told that the "+
			"reply did not reach it; the reply's delivery is kept on the seat's inbox until the "+
			"refusal leaves the run")
	return recorded, true, nil
}

// addressOf is where trigger was posted, as a refusal keeps it
// ([AnswerRefusal.Address]): its notification's transport metadata, nil where
// it carries none or more than [MaxRefusalAddressBytes].
func addressOf(ctx context.Context, trigger *events.Event) map[string]string {
	n, ok := events.DataAs[*types.ExternalNotification](trigger)
	if !ok || n == nil || len(n.Metadata) == 0 {
		return nil
	}
	if weighs := addressBytes(n.Metadata); weighs > MaxRefusalAddressBytes {
		log.WarnContext(ctx, "sandbox_refusal_address_dropped", "event_id", trigger.ID.String(),
			"bytes", weighs, "max_bytes", MaxRefusalAddressBytes,
			"detail", "the refused reply's address is past what a refusal keeps, so the resume it "+
				"drives raises no working indicator in the conversation")
		return nil
	}
	return maps.Clone(n.Metadata)
}

// trigger is the event the resume a refusal drives is raised off
// ([ResumeRequest.Trigger]): a notification carrying the reply's address
// ([AnswerRefusal.Address]) and NONE OF ITS WORDS — no body, no subject — under
// the id of its newest delivery and the instant it was posted; nil where the
// refusal keeps no address.
//
// THE ADDRESS IS ALL A RESUMED TURN READS OF ITS TRIGGER: the conversation its
// working indicator is raised in, for the person who replied. An answer's
// resume reads it off the deliveries the answer carries; a refusal carries none
// of them, so it is rebuilt from what the refusal kept, on every attempt — the
// inline one, a retry and a revival alike.
func (r AnswerRefusal) trigger(posted time.Time) *events.Event {
	if len(r.Address) == 0 {
		return nil
	}
	ev := events.New(types.ExternalNotification{Metadata: maps.Clone(r.Address)}, events.TraceContext{})
	if len(r.EventIDs) > 0 {
		// The newest delivery: a delivery's events come oldest first.
		if id, err := uuid.Parse(r.EventIDs[len(r.EventIDs)-1]); err == nil {
			ev.ID = id
		}
	}
	if !posted.IsZero() {
		ev.Timestamp = posted.UTC()
	}
	return ev
}

// announceRefusal publishes the record of the refusal run holds: its
// `executor_answered` with the outcome too_large ([types.AnswerTooLarge]), by
// the reply's sender, with what it weighed and what an answer may and none of
// its words.
//
// ONE RECORD PER RUN, QUESTION AND DELIVERY, however often it is published
// ([tooLargeRecord]): as the refusal is recorded, and again by every copy of
// the kept delivery that finds it on the run, so a node that recorded the
// refusal and stopped before it published the record leaves it to the copy
// the broker hands the seat's next reader. The resume publishes nothing of it,
// which would say the run carried on with an answer (`resumed`); a let-go of
// it publishes its own record ([Coordinator.announceRefusalLetGo]).
func (c *Coordinator) announceRefusal(ctx context.Context, run PendingRun, trigger *events.Event) {
	refusal, refused := run.Refused()
	if !refused {
		return
	}
	by := chatAnswerer(trigger)
	by.answerBytes, by.answerMaxBytes = refusal.Bytes, refusal.MaxBytes
	c.announceAnsweredAs(ctx, run, types.AnswerViaChat, types.AnswerTooLarge, by, tooLargeRecord(run, refusal))
}

// announceRefusalLetGo publishes what became of a refusal let go of before any
// turn told the run of it (declined after its attempts, or its seal not
// opening): `executor_answered` with outcome declined ([types.AnswerDeclined])
// on the chat route, with the refusal's figures, none of the reply's words, and
// by whom the opened seal names (zero where it did not open). The run waits on
// its question again and the reply goes back to the seat as the message it
// came in.
//
// ONE RECORD PER RUN, QUESTION AND DELIVERY ([refusalLetGoRecord]), stamped
// with the reply's instant, so a let-go published twice is one row. Said
// because the left-standing `too_large` tells a person their next short reply
// has nowhere to go.
func (c *Coordinator) announceRefusalLetGo(ctx context.Context, run PendingRun, refusal AnswerRefusal,
	by answerer,
) {
	by.answerBytes, by.answerMaxBytes = refusal.Bytes, refusal.MaxBytes
	c.announceAnsweredAs(ctx, run, types.AnswerViaChat, types.AnswerDeclined, by,
		refusalLetGoRecord(run, refusal))
}

// tooLargeRecordNamespace derives the ids [tooLargeRecord] names, and
// refusalLetGoNamespace the ids [refusalLetGoRecord] does.
var (
	tooLargeRecordNamespace = uuid.MustParse("d4d834de-f40c-46e4-8cbb-95766fae0dfe")
	refusalLetGoNamespace   = uuid.MustParse("4b0e6a52-7f3d-4c51-9a2e-0c6d1f8e93b7")
)

// recordIdentity is the identity a record is published under where every
// publish of the same fact must be ONE row: its id and its instant. Zero is a
// fresh record, its id and instant [events.New]'s.
//
// BOTH, because the event store keys a row on the pair, never on the id alone
// (internal/store's `ON CONFLICT (event_time, event_id) DO NOTHING`, the key
// every listing and the dashboard's feed read by): a derived id stamped with a
// fresh instant at each publish is a second row.
type recordIdentity struct {
	id uuid.UUID
	at time.Time
}

// tooLargeRecord is the identity the record of the refusal run holds is
// published under ([Coordinator.announceRefusal]): its id derived from the
// run, the question the reply was refused for ([PendingRun.LaunchID]) and the
// deliveries the reply was made of, and its instant the reply's own
// ([RecordedAnswer.PostedAt], the newest of its messages) — all of them off the
// row, so the publish at the record and every publish after it agree, whatever
// node makes it and whatever part of the delivery reached it.
func tooLargeRecord(run PendingRun, refusal AnswerRefusal) recordIdentity {
	return refusalRecord(tooLargeRecordNamespace, run, refusal)
}

// refusalLetGoRecord is the identity a refusal's let-go is published under
// ([Coordinator.announceRefusalLetGo]): derived as [tooLargeRecord] is, under a
// namespace of its own.
func refusalLetGoRecord(run PendingRun, refusal AnswerRefusal) recordIdentity {
	return refusalRecord(refusalLetGoNamespace, run, refusal)
}

// refusalRecord is an identity derived in namespace from the run, the question
// the reply was refused for and the deliveries it was made of, at the reply's
// own instant.
func refusalRecord(namespace uuid.UUID, run PendingRun, refusal AnswerRefusal) recordIdentity {
	identity := recordIdentity{id: uuid.NewSHA1(namespace,
		[]byte(run.TurnID+"\x00"+run.LaunchID+"\x00"+strings.Join(refusal.EventIDs, "\x00")))}
	if run.Answer != nil {
		identity.at = run.Answer.PostedAt
	}
	return identity
}

// refusedReply is what becomes of the delivery a refusal on run was recorded
// from, once this node has attempted the refusal's resume: kept reports whether
// a turn of that attempt kept the refusal ([Coordinator.attemptOwed]), and the
// rest is DECIDED FROM THE ROW AS THE ATTEMPT LEFT IT, read again here: the
// attempt may have let the refusal go while run, the snapshot the reply was
// matched against, still shows it held, and deciding off it deferred a reply
// already back to being the ordinary message it is, stopping the seat's inbox.
// Where the row cannot be read the reply is KEPT ([AnswerRefused]): the
// delivery is its only copy, decided again when it comes round.
func (c *Coordinator) refusedReply(ctx context.Context, run PendingRun, reply Reply, kept bool,
) AnswerDisposition {
	// SPENT ([AnswerConsumed]) once a turn keeps the refusal: the run was told.
	// Certain where the turn was this attempt's (kept), whatever the row says:
	// the relaunch cleared the refusal, an ending deleted the row and the
	// completion ledger fails open, so deciding off those would hand the seat a
	// reply its run was told never reached it. Otherwise: worked in that ledger
	// ([Coordinator.spendRefused]), held by a turn in flight
	// ([RecordedAnswer.TakenAt]), or carried on from on the row below.
	if kept || c.spentAll(ctx, run.AgentHandle, reply) {
		return AnswerConsumed
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil {
		log.WarnContext(ctx, "sandbox_refused_reply_unread", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"error", err.Error(),
			"detail", "the run whose refusal this reply was recorded as could not be read after the "+
				"attempt at its resume, so the reply is kept and decided again when it comes round")
		return AnswerRefused
	}
	if !found {
		return AnswerNotMine
	}
	if refusal, refused := latest.Refused(); refused && reply.amongst(refusal.EventIDs) {
		if latest.Answer.Taken() {
			return AnswerConsumed
		}
		// Still the run's, untaken: handed back at the head of the inbox,
		// unspent. The row carries none of the reply
		// ([PendingStore.RecordRefusal]), so the delivery is its only copy.
		return AnswerRefused
	}
	if latest.carriedOnFromRefusal(run.LaunchID) {
		return AnswerConsumed
	}
	// The refusal left the run unspent (let go, ended, reaped): back to the seat
	// as the ordinary message it is.
	return AnswerNotMine
}

// spentAll reports whether every delivery of reply is recorded as worked in the
// seat's completion ledger ([CoordinatorOptions.WasSpent]) — false with no
// ledger to ask, which reads every delivery as unspent.
func (c *Coordinator) spentAll(ctx context.Context, handle string, reply Reply) bool {
	if c.wasSpent == nil {
		return false
	}
	for _, ev := range reply.Events {
		if ev != nil && !c.wasSpent(ctx, handle, ev) {
			return false
		}
	}
	return true
}

// amongst reports whether any of this delivery's events is one of ids.
func (r Reply) amongst(ids []string) bool {
	return slices.ContainsFunc(r.ids(), func(id string) bool { return slices.Contains(ids, id) })
}

// record records the outcome of the question a run is parked on, an answer on
// either route or the refusal of a chat reply too large to be one (words carry
// it: [AnswerWords.Refusal], [PendingStore.RecordRefusal]), and makes the run
// owe the resume it drives ([StatusAnswered]).
//
// SEALED HERE ([AnswerSeal]), bound to the turn, launch and deliveries under
// this node's active key; the record carries the words only inside that seal.
// The first resume attempt runs INLINE: an answer's delivery is then spent, a
// refusal's KEPT until it leaves the run ([AnswerRefused]). UNDER THE SEAT'S
// LEASE ([ErrSeatNotHeld] before any write): an answer recorded by a node that
// lost the seat would sit undriven; the store refuses an overtaken lease too.
func (c *Coordinator) record(ctx context.Context, run PendingRun, words AnswerWords, answer RecordedAnswer,
) (PendingRun, bool, error) {
	fence, held := c.leaseOf(run.AgentHandle)
	if !held {
		return PendingRun{}, false, notHeld(ctx, run.AgentHandle, run.TurnID, "recording an answer")
	}
	answer, err := c.answers.Seal(run, words, answer)
	if err != nil {
		return PendingRun{}, false, err
	}
	// OUT OF THE QUESTION SET, once, as the record's answer is taken: nobody is
	// waited on, and the screening offers this seat's mail to no question that
	// already has its answer. Recorded before the write, so a recount meanwhile
	// counts the run as the question it is still counted as (countwrite.go).
	write := c.writingCount(run, run.Status, StatusAnswered)
	defer c.settleWrite(write)
	var recorded PendingRun
	var won bool
	if words.Refusal != nil {
		recorded, won, err = c.pending.RecordRefusal(ctx, run.TurnID, run.LaunchID, answer, *words.Refusal, fence)
	} else {
		recorded, won, err = c.pending.RecordAnswer(ctx, run.TurnID, run.LaunchID, answer, fence)
	}
	landed := err == nil && won
	c.mu.Lock()
	moved := c.wroteCountLocked(write, landed)
	// A NEW ANSWER IS A NEW SERIES. The run's series can still be alive when
	// this lands — kept to publish what an earlier answer's decline owes the
	// seat — and its count is that earlier answer's: carried over, it spent
	// this answer's attempts before this answer had one, and let it go after
	// a single failed resume.
	if r := c.retries[recorded.TurnID]; landed && r != nil {
		r.launch, r.failures, r.first = recorded.LaunchID, 0, c.now()
		r.window = answerWindow(recorded)
	}
	c.mu.Unlock()
	moved()
	if !landed {
		return PendingRun{}, won, err
	}
	return recorded, true, nil
}

// resumeOwed takes the seat's inbox hold for an owed answer and makes one
// attempt at its resume — the inline one, on the delivery that recorded it.
//
// AN ANSWER'S DELIVERY IS SPENT WHATEVER THE ATTEMPT CONCLUDES: the answer is
// the run's, and a run that ends under this attempt before any turn took the
// reply hands it back to the seat through its row, with the hold kept until the
// copy is out ([Coordinator.resumeAndSettle]). A REFUSAL'S IS KEPT until the
// refusal leaves the run, and is what goes back where it is let go of
// ([AnswerRefused]) — or SPENT AT ONCE where a turn of this very attempt kept
// it, which is what this reports ([Coordinator.attemptOwed]).
func (c *Coordinator) resumeOwed(ctx context.Context, run PendingRun) (kept bool) {
	c.owe(ctx, run.AgentHandle, run.TurnID)
	return c.attemptOwed(ctx, run)
}

// attemptOwed makes one attempt at an owed answer's resume and settles what it
// leaves: done (hold lifted), failed (retry scheduled, or declined once the
// bounds are spent), or the run gone, when a reply no turn took goes back to the
// seat through the row, owed BEFORE the record was deleted
// ([PendingStore.OweHandBack]) so a crash cannot lose it.
//
// REPORTS WHETHER A TURN OF THIS ATTEMPT KEPT what the run held: took it, then
// ran or broke after acting ([Coordinator.spendRefused]). The inline caller
// decides its delivery by that ([Coordinator.refusedReply]): nothing else this
// node can read says so for certain.
func (c *Coordinator) attemptOwed(ctx context.Context, run PendingRun) (kept bool) {
	answer := run.Answer
	if answer == nil {
		// An answered row always carries its answer — the store refuses
		// every write that would leave one without it — so one that does
		// not was written by something this build does not understand,
		// and there is nothing here to resume it with.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return false
	}
	fence, held := c.leaseOf(run.AgentHandle)
	if !held {
		// THE SEAT IS HELD ELSEWHERE, and with it the answer: the holder's
		// recovery pass found the run answered — or will, when it fences
		// the row — and drives it from there. This node claims nothing.
		_ = notHeld(ctx, run.AgentHandle, run.TurnID, "claiming an answer for its resume")
		c.dropOwed(ctx, run.AgentHandle, run.TurnID)
		return false
	}
	// NO CLAIM WHILE A RELEASE OF THE JOB IS OUT on this node, a park's whose
	// question the store recorded though it did not answer the write included:
	// that release names the very claim this would take, and made again over
	// it, gave it back to running under the turn it resumed. The series that
	// owes the release makes it first and comes back here (claimrelease.go).
	if c.leaveToRelease(ctx, run) {
		return false
	}
	// OPENED BEFORE THE CLAIM, and only what the seal carries goes on: the
	// words the turn is handed and the answerer it and the announcement
	// name. An answer that does not open is claimed for nothing — it waits
	// for a key this node does not hold, or is let go of — so no turn is
	// ever resumed with it. See answerseal.go.
	words, err := c.answers.Open(run)
	switch {
	case errors.Is(err, ErrAnswerKeyUnknown):
		c.awaitAnswerKey(ctx, run, err)
		return false
	case err != nil:
		c.dropUnopened(ctx, run, err)
		return false
	}
	// ADMITTED BEFORE THE CLAIM, and held through the resume: see
	// [TurnGate]. The inline attempt asks under the delivery that carried
	// the answer, whose own admission answers for it; a retry asks afresh.
	// What was signalled is read first, for the reason [Coordinator.waitOwed]
	// gives. AFTER THE SEAL OPENS, because opening is a check on this node
	// alone that starts no turn: an answer that will not open is never
	// resumed, so it takes no slot and does not wait out a drain to be told
	// so.
	seen := c.signalsSeen()
	adm, err := c.admitTurn(ctx, run.AgentHandle, run.ConversationKey, ResumeAnswer)
	if err != nil {
		c.owedRefused(ctx, run, refusalOf(err), seen)
		return false
	}
	defer adm.release()
	return c.resumeOwedAdmitted(adm, run, answer, words, fence, seen)
}

// resumeOwedAdmitted is an owed answer's claim and resume, under the admission
// [Coordinator.attemptOwed] took for it, and the settling of what they leave.
//
// IT TAKES THE ADMISSION, NOT A CONTEXT, and runs under the admission's own,
// as [Coordinator.resumeAndSettle] does: the claim belongs to the admitted
// turn, so the drain's cut ends it too, and a bare context could be the
// caller's, which the admission never covered.
//
// A resume REFUSED after its claim, reverted with a [*TurnRefused] naming a
// condition, waits for it as a gate's refusal does and is charged nothing; seen
// is what had been signalled before the gate was asked ([Coordinator.waitOwed]).
// Reports whether the turn KEPT what the run held, as [Coordinator.attemptOwed].
func (c *Coordinator) resumeOwedAdmitted(adm admitted, run PendingRun, answer *RecordedAnswer,
	words AnswerWords, fence Fence, seen map[signal]uint64,
) (kept bool) {
	ctx := adm.ctx
	// THROUGH THE SEAT'S RECOVERY GATE FIRST ([Coordinator.claimGateLocked]):
	// while a recovery pass of the seat runs on this node no claim of its runs
	// is taken, and the answer, still recorded on its run and owed here, is
	// handed to the pass, which arms its series at once when it is done. Taken
	// meanwhile, the pass found its own node's live claim resumed and revived
	// or reaped it under the turn this attempt was resuming. Let through, the
	// claim is this node's live attempt until this frame returns.
	c.mu.Lock()
	done, gated := c.claimGateLocked(run.AgentHandle, run.TurnID)
	c.mu.Unlock()
	if gated != nil {
		log.InfoContext(ctx, "sandbox_answer_recovery_refused",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
			"detail", "the seat's recovery pass is under way on this node, which claims none of its runs "+
				"until the pass is done; the answer stays owed, and its next attempt is armed when the pass ends")
		c.carryPastRecovery(ctx, run.AgentHandle, passCarry{answer: &run})
		return false
	}
	defer done()
	// INTO THE HOLDING SET, once, as the claim's answer is taken; recorded
	// before the write, so a recount meanwhile counts the run as the answer it
	// is still counted as (countwrite.go). THE SEAT'S SLOT IS RESERVED IN THE
	// SAME STEP, or the resume waits for one (seatcap.go): the check before
	// the admission ran outside the lock, beside a launch that may take it.
	write, err := c.reservingCount(run, StatusAnswered, StatusResumed)
	if err != nil {
		c.waitOwed(ctx, run.TurnID, refusalOf(err), seen)
		return false
	}
	defer c.settleWrite(write)
	claimed, won, err := c.pending.ClaimForResume(ctx, run.TurnID, RecordedAnswerTail(run.LaunchID), fence)
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && won)
	c.mu.Unlock()
	moved()
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
		return false
	}
	if !won {
		// Resumed already (an attempt on another node across a handoff),
		// declined, replaced by the next job, or owned by a newer lease:
		// not this attempt's.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return false
	}
	// RAISED WHERE THE REPLY WAS POSTED: an answer's trigger is the newest of
	// the deliveries it carries, and a refusal's is rebuilt from the address
	// it kept, with none of the reply's words ([AnswerRefusal.trigger]).
	trigger := answer.trigger()
	if words.Refusal != nil {
		trigger = words.Refusal.trigger(answer.PostedAt)
	}
	// NO REPORT, and the question and its answer instead: the run is not
	// over, and the turn carries it on with them ([ResumeRequest.Asked]).
	disposition, err := c.resumeAndSettle(adm, claimed, "", true, trigger, runOutcome{
		InputTokens: claimed.ParkedInputTokens, OutputTokens: claimed.ParkedOutputTokens,
		CacheReadTokens:  claimed.ParkedCacheReadTokens,
		CacheWriteTokens: claimed.ParkedCacheWriteTokens,
		Engine:           parkedEngineSpend(claimed),
		Asked:            c.askedWith(claimed, words),
	}, seen)
	if disposition == AnswerDeferred {
		// The claim went back: the run is [StatusAnswered] again with
		// the same answer on it, owed the same resume.
		if refusal, refused := waitingRefusal(err); refused {
			c.owedRefused(ctx, claimed, refusal, seen)
			return false
		}
		c.owedFailed(ctx, claimed, err)
		return false
	}
	// KEPT, FOR A CLAIM TAKEN FOR WHAT THE ROW RECORDED, IS CONSUMED: the turn
	// took it and then ran, or broke after acting — the two ways a resume of
	// one returns [AnswerConsumed], and the two after which
	// [Coordinator.resumeAndSettle] spends a refusal's delivery
	// ([Coordinator.spendRefused]). Every other way back gave it up: the claim
	// went back above, or the run ended under the attempt before any turn took
	// it ([AnswerNotMine]), which lets it go.
	kept = disposition == AnswerConsumed
	c.settleOwed(ctx, claimed.AgentHandle, claimed.TurnID)
	if errors.Is(err, errRunMovedOn) {
		// NOT THIS NODE'S TO SAY: the run moved on to another holder
		// before this attempt could end it, and that holder announces
		// what becomes of the answer — resumed, as a revived claim's is.
		// Said here, it would be told as gone first.
		return kept
	}
	if words.Refusal != nil {
		// ITS ONE RECORD IS THE REFUSAL'S, published when it was recorded
		// (`too_large`, [Coordinator.announceRefusal]): `resumed` would read
		// the refusal as an answer the run carried on with — a person's
		// answered decision, which it is not — and `gone` as a reply that
		// reached a run which ended, where it goes back to the seat as the
		// delivery it came in, which the run's ending says
		// ([PendingRun.EndingReturnsRefusal]).
		return kept
	}
	outcome := answeredAs(disposition)
	if outcome == types.AnswerGone && answer.Via == types.AnswerViaOperator && len(answer.Events) > 0 {
		// ITS COPY SAYS SO. The run ended before any turn took the answer,
		// so its ending handed the answer by turn back to the seat's control
		// topic, where it is spent as reaching a run that is gone and
		// announced there ([Coordinator.AnswerByTurn]) — once, through the
		// outbox that survives a crash, rather than here and again there.
		return kept
	}
	c.announceAnswered(ctx, claimed, words.Via, outcome, words.answerer())
	return kept
}

// awaitAnswerKey keeps an owed answer sealed under a key this node does not
// hold waiting for it ([ConditionAnswerKey]): recorded, owed, uncounted and
// never let go of on that account, with the seat's inbox held behind it — the
// key is a fact about this node, and the answer is the person's whichever node
// can read it. Re-checked on [answerWaitBackstop], which says again which keys
// this node holds.
func (c *Coordinator) awaitAnswerKey(ctx context.Context, run PendingRun, cause error) {
	log.WarnContext(ctx, "sandbox_answer_key_unknown",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"node_holds", c.answers.KeyIDs(), "error", cause.Error(),
		"detail", "the answer recorded on this run is sealed under a key this node does not "+
			"hold; it stays recorded and owed, the seat's mail waits behind it, and it is "+
			"resumed once a node holding the key takes the seat or this node is restarted "+
			"with the key in secrets.keys")
	c.mu.Lock()
	if _, ok := c.retries[run.TurnID]; !ok {
		c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
			first: c.now(), window: answerWindow(run)}
	}
	c.mu.Unlock()
	c.waitOwed(ctx, run.TurnID, Refusal{Condition: ConditionAnswerKey,
		Reason: "the answer is sealed under a key this node does not hold"}, nil)
}

// dropUnopened lets go of an answer whose seal does not open as the answer to
// this run's question: logged at error, handed to no turn and attributed to
// nobody — no announcement names an answerer, and nothing of it is handed back
// to the seat or recorded as worked, because nothing on the row about it can be
// attributed either. The run goes back to waiting on its question
// ([PendingStore.DeclineAnswer], with nothing owed), where the next qualifying
// reply answers it. A refusal's delivery, which the seat's inbox kept and the
// row never held ([AnswerRefused]), comes round as the ordinary message it is,
// as it does for every refusal let go of, and the let-go is announced, naming
// nobody ([Coordinator.announceRefusalLetGo]).
func (c *Coordinator) dropUnopened(ctx context.Context, run PendingRun, cause error) {
	answer := run.Answer
	log.ErrorContext(ctx, "sandbox_answer_unverified",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "launch_id", run.LaunchID,
		"node_holds", c.answers.KeyIDs(), "error", cause.Error(),
		"detail", "the answer recorded on this run does not open as an answer to its question "+
			"under the fleet keyring; it is not handed to the run or attributed to anybody, and "+
			"the run waits on its question again")
	if len(answer.EventIDs) == 0 {
		// NOTHING TO LET GO OF BY: the store refuses an answer that names no
		// delivery, so this row was written by something this build does not
		// understand. Left recorded, and resumed with nothing.
		c.settleOwed(ctx, run.AgentHandle, run.TurnID)
		return
	}
	refusal, refused := run.Refused()
	// BACK ON ITS QUESTION, once, as the decline's answer is taken; recorded
	// before the write, so a recount meanwhile counts the run as the answer it
	// is still counted as (countwrite.go).
	write := c.writingCount(run, StatusAnswered, answer.declinedTo())
	defer c.settleWrite(write)
	_, let, err := c.pending.DeclineAnswer(ctx, run.TurnID, run.LaunchID, answer.EventIDs, nil, fenceOf(run))
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && let)
	c.mu.Unlock()
	moved()
	if err != nil {
		// THE WRITE MAY HAVE LANDED: the retry reads the row again, and finds
		// the run waiting on its question or this answer still on it.
		log.ErrorContext(ctx, "sandbox_answer_decline_failed",
			"turn_id", run.TurnID, "agent", run.AgentHandle, "error", err.Error())
		c.mu.Lock()
		if _, ok := c.retries[run.TurnID]; !ok {
			c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
				first: c.now(), window: answerWindow(run)}
		}
		c.mu.Unlock()
		c.scheduleOwed(run.TurnID, answerRetryCeiling)
		return
	}
	if let && refused {
		// ITS TOO_LARGE RECORD SAID THE RUN WOULD BE TOLD, and it is waiting
		// on its question again: said, with nobody named, since nothing on
		// the row about the reply can be attributed.
		c.announceRefusalLetGo(ctx, run, refusal, answerer{})
	}
	c.settleOwed(ctx, run.AgentHandle, run.TurnID)
}

// owedFailed counts one failed resume of an owed answer, and schedules the next
// attempt or — once [MaxAnswerAttempts] or [answerWindow] is spent — declines
// the answer.
//
// A CLAIM THE FAILURE MAY HAVE LEFT IS REVIVED BEFORE THE DECLINE: a failed
// claim may have landed on the row held by nothing ([Coordinator.suspectClaim]).
// Declining then reads the row as the answered run it no longer is; the store
// refuses to let go of an answer from a claim, and the run sits in it, answer
// untaken, delivery spent, seat held, until the seat changes hands. So a series
// that suspects its claim is never declined here, even past its bounds: its
// next attempt runs at once, gives the claim back to the answer
// ([Coordinator.retryRevival]), then declines once through the usual bound.
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
//
// A SERIES THAT OWES A CLAIM'S RELEASE IS ARMED NO LATER THAN THE HAND-BACK'S
// SPACING ([answerRetryCeiling]), whatever its caller asked for: the release
// is made again before anything else the series does ([Coordinator.retryOwed]),
// and a longer wait, an answer's for its seat's conditions, held the claim on
// the store's row, and the seat's mail behind it, past the store's answer. A
// shorter one stands: it asks the store again sooner, as an answer's backoff
// after its failed resume does.
func (c *Coordinator) scheduleOwedLocked(turnID string, delay time.Duration) {
	r := c.retries[turnID]
	if r == nil || c.closed {
		return
	}
	if r.release != nil {
		delay = min(delay, answerRetryCeiling)
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
	var release *claimRelease
	var unread bool
	if r != nil {
		r.stop, r.waiting = nil, ""
		handle, launch = r.handle, r.launch
		spent = r.spent(c.now())
		kept, revive, release, unread = r.ending, r.revive, r.release, r.unread
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
	if release != nil {
		// A claim's release the store did not answer is made again first, with
		// no read of the row: the release decides on the row as it stands, and
		// the attempt goes on against what it left (claimrelease.go); settled
		// at once, the answer would be left with nothing here to resume it. But
		// not once a claim of its job has taken it over, nor while one is on
		// its way: it could land after that claim and give it back. The claim
		// decides first, and a claim that starts from here is refused while the
		// release stays recorded ([Coordinator.claimCompletion]).
		c.mu.Lock()
		out := c.releases[turnID] == release
		onItsWay := c.claiming[turnID] > 0
		if series := c.retries[turnID]; !out && series != nil && series.release == release {
			series.release = nil
		}
		c.mu.Unlock()
		switch {
		case !out:
			c.settleOwed(ctx, handle, turnID)
			return
		case onItsWay:
			c.scheduleOwed(turnID, answerRetryCeiling)
			return
		}
		_ = c.unclaimOwing(ctx, release)
		if c.releasing(turnID, release.run.LaunchID) {
			return
		}
		unread = release.release.To != StatusAnswered
	}
	run, found, err := c.pending.Get(ctx, turnID)
	c.mu.Lock()
	if series := c.retries[turnID]; series != nil {
		series.unread = unread && err != nil
	}
	c.mu.Unlock()
	if err != nil {
		if kept != nil || revive != nil || release != nil || unread {
			// A READ IS NOT A RESUME, and neither an ending, a revival nor
			// a release just settled owes an attempt, nor does a run a
			// release put back in running ([answerRetry.unread]), however
			// many reads after it the store leaves unanswered: the row is
			// read again on the hand-back's own spacing.
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
		decided, err := c.endRun(ctx, run, *kept, true)
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
		// THE BOUND IS SPENT, and the decline that followed it did not
		// land — or waited for the claim the last attempt left to be
		// given back first ([Coordinator.owedFailed]): decided now, never
		// resumed again — the attempts it was owed have all been made.
		c.declineAnswer(ctx, run, errors.New("sandbox: the answer's attempts are spent and "+
			"letting it go did not land"))
		return
	}
	// What has been signalled so far, read BEFORE the checks: a signal that
	// lands while they are deciding is one the wait below must not sleep
	// through. See [Coordinator.waitOwed].
	seen := c.signalsSeen()
	if refusal, refused := c.seatRefusal(run.TurnID, handle, run.ConversationKey); refused {
		c.waitOwed(ctx, turnID, refusal, seen)
		return
	}
	if c.admit != nil {
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
// ([Coordinator.endRecord]): the answer could not be recorded as owed, its
// copies could not be published, or the store did not take the delete. The
// seat's inbox is held behind it as behind a decline's, and a retry finishes it
// ON THE TERMS IT WAS DECIDED ON (its box included), or the seat's next
// holder's recovery pass reaps the row and hands back what it owes. A delete
// the store did not take is kept too: left for that pass, the live row is a
// claim holding the seat's inbox on every recount until the seat moves.
func (c *Coordinator) oweEnding(ctx context.Context, run PendingRun, e ending) {
	c.oweEndingAfter(ctx, run, e, answerRetryCeiling)
}

// oweEndingAfter is [Coordinator.oweEnding] with the first retry after delay:
// the hand-back's spacing for an ending a step of which did not land, and none
// for one decided on a frame that may not finish it ([errAskAway]).
//
// KEPT, OWED AND ARMED IN ONE STEP ([Coordinator.keepSeriesLocked]).
func (c *Coordinator) oweEndingAfter(ctx context.Context, run PendingRun, e ending, delay time.Duration) {
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
	c.keepSeriesLocked(run, delay)
	c.mu.Unlock()
	c.reconcileHold(ctx, run.AgentHandle)
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
	// KEPT, OWED AND ARMED IN ONE STEP ([Coordinator.keepSeriesLocked]).
	c.mu.Lock()
	r := c.retries[run.TurnID]
	if r == nil {
		r = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID, first: c.now(),
			window: answerWindow(run)}
		c.retries[run.TurnID] = r
	}
	r.revive = &revival{fence: fence, lost: true}
	c.keepSeriesLocked(run, answerRetryCeiling)
	c.mu.Unlock()
	c.reconcileHold(ctx, run.AgentHandle)
}

// suspectClaim marks an owed answer's series whose claim reported a failure:
// the claim may have landed unheld by any resume, so the next attempt gives it
// back ([Coordinator.giveBackClaim]) if it did. UNCOUNTED: no node stopped, so
// it is no lost claim ([revival]). fence is the lease the claim was made under.
//
// ONLY THE SERIES' OWN FAILURE MARKS A CLAIM, never any claim a retry finds:
// the row reads [StatusResumed] for every resume, and reviving whatever it
// found would revive one an inline attempt is driving, failing that resume for
// nothing (the store still answers the person once).
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
		case reapLeft, reapEnded, reapLive:
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
// the claim's write reported a failure and landed, and no resume holds it.
// Reports the row to go on with as the owed answer, or false where the series
// has nothing more to do.
//
// ONLY THE SERIES' OWN CLAIM, never through [Coordinator.reapTail]: that reaps a
// claim whose node stopped, with bounds and an ending
// ([types.ExecutorFailureAbandoned]) about a node that stopped. None did here,
// so nothing is counted and nothing ends the run. A give-back the store does not
// confirm is suspected again and retried ([Coordinator.oweRevival]); one it
// refuses because a TURN TOOK THE ANSWER is left to that turn, which settles it.
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
// A wait, not a failure (nothing is charged) and not a poll: each condition
// changes on something DISCRETE, so the refusal names what it waits on
// ([Refusal]): its clock is armed, its event re-checks via [Coordinator.Readmit].
// No wait outlasts [answerWaitBackstop], since a missed signal would strand an
// answer. Readmit only wakes an attempt already marked waiting, so the caller
// reads what was signalled before checking (seen), and a condition signalled
// since re-checks at once. An instant already past is no instant: the clock did
// not lift the condition, and re-checking at it refuses thousands of times a
// second.
func (c *Coordinator) waitOwed(ctx context.Context, turnID string, refusal Refusal,
	seen map[signal]uint64,
) {
	wait := answerWaitBackstop
	if until := refusal.Until.Sub(c.now()); !refusal.Until.IsZero() && until > 0 {
		wait = min(wait, until)
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

// signalsSeen is what has been signalled so far, read before a check whose
// refusal [Coordinator.waitOwed] then waits on.
func (c *Coordinator) signalsSeen() map[signal]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.signals)
}

// owedRefused parks an owed answer whose resume the node's gate refused
// ([TurnGate]) until what refused it can have changed, as [Coordinator.waitOwed]
// parks one the seat's conditions refused: NOT A FAILURE, so none of the
// answer's attempts is spent on a node that is merely draining or renewing.
//
// THE SERIES IS OPENED HERE when the refusal is the first thing it meets, which
// is the inline attempt's case: a wait needs a series to arm, and without one
// the answer would wait on nothing until the seat moved.
func (c *Coordinator) owedRefused(ctx context.Context, run PendingRun, refusal Refusal,
	seen map[signal]uint64,
) {
	c.mu.Lock()
	if c.retries[run.TurnID] == nil {
		c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
			first: c.now(), window: answerWindow(run)}
	}
	c.mu.Unlock()
	log.InfoContext(ctx, "sandbox_answer_resume_not_admitted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "reason", refusal.Reason,
		"detail", "this node may not start the turn the answer resumes; the answer stays "+
			"recorded on its run and the resume waits, here or on the seat's next holder")
	c.waitOwed(ctx, run.TurnID, refusal, seen)
}

// Readmit re-checks at once every attempt waiting on cond, for the named seats
// or, naming none, every seat; an attempt waiting on anything else is left
// waiting. The attempts are an owed answer's retry, a completion this node
// refused on cond ([Coordinator.keepRefused]), and a resume held on cond on its
// row ([PendingRun.HeldFor]), whose hold is lifted and job re-fired
// ([Coordinator.liftHoldsLocked], [Coordinator.liftAndRefire]). Safe from any
// goroutine, and cheap enough for every occurrence of the event. See the
// package doc, "Kept completions".
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
	c.liftHoldsLocked(cond, handles)
	c.refireRefusedLocked(cond, handles)
}

// liftHoldsLocked lifts, in the background, every hold on cond this node's
// seats' runs are under ([PendingRun.HeldFor]), for the seats named or every
// seat, and re-fires each lifted job this node does not carry already
// ([Coordinator.refireLifted]). The rows are the record, so a new holder lifts
// its predecessor's holds; only seats this node holds, as a lift is a write
// under the lease; in the background, as Readmit must stay cheap. Not for
// [SeatFull] or [ConversationBusy], which no hold names and whose signals come
// at every freed slot.
// The caller holds c.mu. See the package doc, "Kept completions".
func (c *Coordinator) liftHoldsLocked(cond Condition, handles []string) {
	if c.closed || cond == SeatFull || cond == ConversationBusy {
		return
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		ctx := c.life
		runs, err := c.pending.ListActive(ctx)
		if err != nil {
			log.WarnContext(ctx, "sandbox_hold_lift_unread", "condition", string(cond),
				"error", err.Error(), "detail", "the holds lift on the clock instead")
			return
		}
		for _, run := range runs {
			if run.HeldFor != cond ||
				(len(handles) > 0 && !slices.Contains(handles, run.AgentHandle)) {
				continue
			}
			if _, held := c.leaseOf(run.AgentHandle); !held {
				continue
			}
			lifted, err := c.pending.LiftHold(ctx, run.TurnID, cond, run.Holds)
			switch {
			case err != nil:
				log.WarnContext(ctx, "sandbox_hold_lift_failed", "turn_id", run.TurnID,
					"condition", string(cond), "error", err.Error())
			case lifted:
				log.InfoContext(ctx, "sandbox_resume_hold_lifted", "turn_id", run.TurnID,
					"agent", run.AgentHandle, "condition", string(cond))
				c.refireLifted(run, cond)
			}
		}
	}()
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

// declineAnswer gives up on an answer this node could not resume with, once its
// attempts are spent: the reply goes to the seat as an ordinary message and the
// run goes back to its question. EXACTLY ONCE across a crash, by this order:
// (1) THE WRITE is the decision ([PendingStore.DeclineAnswer]): the answer is
// cleared and the copies (ids derived from the originals, [declinedCopyID])
// are recorded as owed ([PendingRun.HandBack]). (2) THE PUBLISH of the stored
// copies; stopped before it, the next reader finds them owed. (3) THE CLEAR
// ([PendingStore.ClearHandBack]); stopped before it, they are republished
// under the same ids, one message to the dedupe. Publishing first could
// deliver twice. THE RUN IS NOT ENDED ([MaxAnswerAttempts]): a failure here is
// about THIS node, and another holder may still resume the turn.
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
	// before acknowledging it — must not be a second message. A REFUSAL HAS
	// NO COPY, so nothing is spent or handed back for it: the delivery the
	// seat's inbox kept is the reply, and it comes round as the ordinary
	// message it is once the hold lifts ([AnswerRefused]).
	c.spend(ctx, run.AgentHandle, *answer)
	// BACK ON ITS QUESTION, once, as the decline's answer is taken; recorded
	// before the write, so a recount meanwhile counts the run as the answer it
	// is still counted as (countwrite.go).
	write := c.writingCount(run, StatusAnswered, answer.declinedTo())
	defer c.settleWrite(write)
	declined, let, err := c.pending.DeclineAnswer(ctx, run.TurnID, run.LaunchID,
		answer.EventIDs, handBackOf(*answer), fenceOf(run))
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && let)
	c.mu.Unlock()
	moved()
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
	if refusal, refused := run.Refused(); refused {
		// ITS TOO_LARGE RECORD SAID THE RUN WOULD BE TOLD, and no turn told
		// it: the let-go is said, by whom the seal names where it opens
		// here ([Coordinator.announceRefusalLetGo]).
		by := answerer{}
		if words, err := c.answers.Open(run); err == nil {
			by = words.answerer()
		}
		c.announceRefusalLetGo(ctx, run, refusal, by)
	}
	log.ErrorContext(ctx, "sandbox_answer_requeue_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "attempts", MaxAnswerAttempts,
		"window_s", answerWindow(run).Seconds(), "error", errDetail(cause),
		"detail", "this node could not resume the executor that asked with the answer "+
			"recorded for it, in every one of its spaced attempts, so the reply is handed "+
			"to the seat as the ordinary message it looks like; the executor waits on its "+
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
	published, err := c.publishCopies(ctx, run, run.HandBack)
	if len(published) > 0 {
		clearCtx, clearDone := c.endingCall(ctx)
		_, clearErr := c.pending.ClearHandBack(clearCtx, run.TurnID, published)
		clearDone()
		if clearErr != nil {
			log.WarnContext(ctx, "sandbox_answer_handback_uncleared",
				"turn_id", run.TurnID, "agent", run.AgentHandle, "error", clearErr.Error(),
				"detail", "the copies are published; whoever reads the run next publishes "+
					"them again under the same ids, which the seat's inbox collapses")
		}
	}
	return err
}

// publishCopies publishes copies back to the seat that holds run, in order,
// and reports the ids it published (counting one that can never be
// published, which is dropped loudly rather than owed for ever) and the error
// that stopped it.
//
// THE ROW'S SEAT ID NAMES THE SUBJECT ([handBackSubject]), as it does for an
// answer by turn ([AnswerDesk.Deliver]): a seat's topics are named by its id
// (ADR-0013), and the handle the row also carries is only a label for the log.
// Each copy goes back the way its original came: an answer by turn to the
// control topic, a chat reply to the inbox.
func (c *Coordinator) publishCopies(ctx context.Context, run PendingRun, copies []HandedBack) ([]string, error) {
	handle := run.AgentHandle
	if inboxSubject(run.AgentID) == "" {
		return nil, fmt.Errorf("sandbox: seat %q names no seat id (%q) to hand its reply back to",
			handle, run.AgentID)
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
		if err := c.queue.Publish(ctx, handBackSubject(run.AgentID, &ev), &ev); err != nil {
			return published, fmt.Errorf("sandbox: handing %s back to %s: %w", copied.Original, handle, err)
		}
		published = append(published, copied.ID)
	}
	return published, nil
}

// queueOwed holds the seat's inbox behind what a run is owed and arms its first
// attempt at once, through the admission every retry passes
// ([Coordinator.retryOwed]) rather than inline: neither caller runs inside a
// delivery the seat's screening admitted. The seat's preparation takes over
// what a previous holder left owed, and an answer by turn arrives on the
// control topic at any moment of the seat's day ([Coordinator.AnswerByTurn]).
// Kept, owed and armed in one step ([Coordinator.keepSeriesLocked]).
func (c *Coordinator) queueOwed(ctx context.Context, run PendingRun) {
	c.mu.Lock()
	if _, ok := c.retries[run.TurnID]; !ok {
		c.retries[run.TurnID] = &answerRetry{handle: run.AgentHandle, launch: run.LaunchID,
			first: c.now(), window: answerWindow(run)}
	}
	c.keepSeriesLocked(run, 0)
	c.mu.Unlock()
	c.reconcileHold(ctx, run.AgentHandle)
}

// keepSeriesLocked owes the seat's run ([Coordinator.oweLocked]) and arms the
// next attempt of run's series after delay ([Coordinator.scheduleOwedLocked]),
// in the step of the lock that recorded the series; the caller brings the
// seat's inbox hold into line ([Coordinator.reconcileHold]) once it lets go of
// c.mu. In the one step, because a seat's release ([Coordinator.releaseOwed])
// drops series, attempts and owed runs together, and a run owed after a release
// would hold the seat's mail with nothing to drive it. See the package doc,
// "Owed answers".
func (c *Coordinator) keepSeriesLocked(run PendingRun, delay time.Duration) {
	c.oweLocked(run.AgentHandle, run.TurnID)
	c.scheduleOwedLocked(run.TurnID, delay)
}

// owe records that a seat owes one of its runs an answer's resume, and takes
// the seat's inbox hold if it is the first thing holding it.
func (c *Coordinator) owe(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	c.oweLocked(handle, turnID)
	c.mu.Unlock()
	c.reconcileHold(ctx, handle)
}

// oweLocked is [Coordinator.owe]'s record, for a caller holding c.mu, which
// brings the seat's inbox hold into line ([Coordinator.reconcileHold]) once it
// has let go of the lock.
func (c *Coordinator) oweLocked(handle, turnID string) {
	owed := c.owed[handle]
	if owed == nil {
		owed = map[string]struct{}{}
		c.owed[handle] = owed
	}
	owed[turnID] = struct{}{}
}

// settleOwed forgets an owed answer — resumed, declined or moved on — and
// lifts the seat's inbox hold if nothing else needs it.
//
// NOT WHILE THE SERIES STILL OWES THE ROW SOMETHING: an ending it kept, a
// claim it suspects ([answerRetry.revive]), or a claim's release the store
// did not answer ([answerRetry.release]). Each is a run whose only driver is
// this series, and a settle that dropped it — on a refusal that only says the
// row is not what the settling path expected — left a claim, with the
// person's answer or the job's result on it, that nothing drove. The series
// stays, with an attempt armed if none is, and the seat's hold with it.
func (c *Coordinator) settleOwed(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	if r := c.retries[turnID]; r != nil && (r.ending != nil || r.revive != nil || r.release != nil) {
		// AN ENDING STILL OWES THE SEAT ITS REPLY — kept by the ending this
		// very attempt made — or A CLAIM IS STILL SUSPECTED, or ITS RELEASE
		// IS STILL OWED, which only this series will ever look at: the
		// series, and the seat's hold, stay.
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
	// AND THE RELEASES OF ITS CLAIMS THIS NODE WAS GIVING BACK OR OWED: the
	// successor reaps those claims, and a record of them kept here would
	// refuse this node every later claim of their jobs. Each one's write in
	// its run's entry goes with the seat's other writes ([Coordinator.ReleaseSeat]).
	for turnID, cr := range c.releases {
		if cr.run.AgentHandle == handle {
			delete(c.releases, turnID)
		}
	}
	delete(c.owed, handle)
}

// dropOwed forgets one owed answer's series on a seat this node does not hold,
// whatever it still owed the row: an ending it kept, a claim it suspects, a
// revival it was retrying, a claim's release it owed. Each is on the row,
// and the holder's recovery pass reads the row and finishes, revives or reaps
// what it finds — where this node's own writes, under a lease the holder has
// fenced or is about to, would only race it. Unlike [Coordinator.settleOwed],
// which keeps such a series for the row it alone drives.
func (c *Coordinator) dropOwed(ctx context.Context, handle, turnID string) {
	c.mu.Lock()
	if r := c.retries[turnID]; r != nil {
		if r.stop != nil && r.stop() {
			c.inflight.Done()
		}
		delete(c.retries, turnID)
	}
	if cr := c.releases[turnID]; cr != nil {
		// ITS WRITE IN THE RUN'S ENTRY GOES WITH IT, answered as one that did
		// not land: no answer of it is taken here again (countwrite.go). The
		// seat's inbox hold follows below.
		delete(c.releases, turnID)
		_ = c.wroteCountLocked(cr.write, false)
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
