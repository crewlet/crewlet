package types

import "slices"

// Which question one row of a turn's answer answers.
//
// # The rule had to move out of the browser
//
// A turn's answer is every row its run published (`EventLog.Turn`, which is
// `WHERE turn_id = ?`), and most of them are not news: a phase start beside the
// phase card it opens, both halves of the turn's own record beside the header
// that draws them. The Turn screen sorted the rest into the questions a reader
// brings — what went wrong, what it was given, what else it did, what it left
// behind — and it did so in `lib/turnstory.ts`, over sets declared in the
// dashboard and held against this catalogue by a gate reading TypeScript.
//
// A member's account of the same turn — the turn's STORY, which tells a
// teammate what an agent did on their work without a prompt, a tool name or a
// token in it — has to be built from the same rows on the server, because the
// server is the only place that can withhold the rest. Two implementations of
// one rule over one answer is the shape ADR-0008 forbids: the day they disagree
// a story and its trace describe two different turns. So the rule is here,
// beside the failure taxonomy it outranks, and the `turn` answer tags every row
// with the band this function gives it rather than leaving a client to work it
// out.
//
// # A band entry is a query predicate, not a wish
//
// Every row a band sorts came from `WHERE turn_id = ?`, and that column is
// filled from the event's own top-level `turn_id` field. A type whose payload
// declares no `turn_id` key never matches the predicate and cannot be in any
// turn's answer however it is banded — so naming one here is a promise the wire
// cannot keep, and it fails EMPTY, which is the one way a panel cannot say it
// is broken. Eight types were once banded that way; four were repaired in their
// producers and are back, and four describe no single turn and never will be.
// `TestTurnBandsNameOnlyTurnScopedEvents` holds every set here against the
// catalogue in both directions, and holds every stored, turn-scoped type to a
// place in one of them.

// TurnBand is which question a turn's row answers.
//
// A named string with a closed set, so an unknown value off the wire — a band a
// newer node names — is a value rather than a panic; [TurnBand.Valid] is how a
// reader asks whether this build knows it.
type TurnBand string

const (
	// BandWentWrong is the engine talking about something going wrong — a
	// turn it stopped, a chain that fell through, a guard that fired, a
	// refusal — and a turn a PERSON stopped, which is the same question asked
	// from outside: why did it not finish?
	BandWentWrong TurnBand = "went_wrong"

	// BandGiven is what the turn was given to work from: the work it opened
	// on, what its prompt was assembled from before the first phase, and a
	// person's note it was handed mid-turn.
	BandGiven TurnBand = "given"

	// BandDid is work the turn did outside its phases' tool loop: a coding
	// run, a delegation, a skill, a page read, and asking a colleague.
	BandDid TurnBand = "did"

	// BandLeftBehind is what the seat learned from the turn after its last
	// phase: the reflection pass's diary, profiles and skills.
	BandLeftBehind TurnBand = "left_behind"

	// BandAbsorbed is a row already on the screen in a better form — a phase
	// card, the turn's header and record — and therefore not a row. It is
	// IN the taxonomy rather than beside it: a reader asking where a row went
	// and a reader asking which band it is in must get one answer, and when
	// the two were separate rules they disagreed on every absorbed type.
	BandAbsorbed TurnBand = "absorbed"

	// BandRest is everything this build has no opinion about. The registry is
	// additive-only, so a type a NEWER node publishes must still render; this
	// build's own types are placed in a band in the change that adds them.
	BandRest TurnBand = "rest"
)

// TurnBands is every band, in the order the Turn screen draws them.
var TurnBands = []TurnBand{
	BandWentWrong, BandGiven, BandDid, BandLeftBehind, BandAbsorbed, BandRest,
}

// Valid reports whether this build knows b.
func (b TurnBand) Valid() bool { return slices.Contains(TurnBands, b) }

// absorbedRows are the types already on the Turn screen in a better form, each
// with where it went — the words the screen's "Already on this page" inventory
// prints, so the claim is checkable and nothing is silently dropped.
//
// UNEXPORTED, for the reason [failureEventTypes] is: an exported map is
// writable by every importer, and one `delete` would vanish a whole kind of row
// off every turn. [AbsorbedInto] answers for one type.
var absorbedRows = map[string]string{
	// Folded onto its own phase card as that phase's start instant.
	"agent_phase_started": "the phase card it opens",
	// The phase cards themselves.
	"agent_phase_completed": "its own phase card",
	// Stream-only; never persisted, so it cannot be in a query answer
	// anyway — see notPersisted in the gate.
	"agent_turn_progress": "the live phase card",
	// The stat strip, the header and the turn record.
	"agent_turn_completed": "the turn's header and record",
	"turn_completed":       "the turn's header and record",
	// What the auxiliary model spent: the in-turn records as the Tokens
	// fact's "+N auxiliary", the reflection's as the Reflection lane's
	// workers.
	"auxiliary_spend": "the Tokens fact and the Reflection lane",
}

// wentWrongRows are the types that report something going wrong, whatever
// their payload says.
//
// `agent_turn_stopped` names who paused the seat with the stop. It is
// deliberately not in [turnStopRows]: its turn's completion carries `stopped`,
// never `failed`, so there is no failure for it to double.
//
// `skill_telemetry_write_failed` is here because its producer stopped dropping
// the turn id rather than because this set lowered its bar: the counter write
// that failed happened inside a turn's reflection, so the turn that used the
// skill is exactly where an operator goes looking for it.
var wentWrongRows = memberSet(
	"turn.guard_breach",
	"llm_unavailable",
	"budget_exhausted",
	"provider_fallback",
	"phase.tool_skill_blocked",
	"sandbox_run_failed",
	"skill_telemetry_write_failed",
	"agent_turn_stopped",
)

// turnStopRows are the three records `publishFailure` writes for a turn the
// engine STOPPED — a subset of [wentWrongRows], and the distinction is the
// whole of it: these describe the same stop `agent_turn_completed.failed`
// already reports, so a count of what went wrong must not count both.
// Everything else in the band — a recovered provider fallback, a refused tool
// skill, a failed coding run — is an INDEPENDENT problem that happens to share
// a turn with the stop. `sandbox_run_failed` is deliberately not here although
// it is a failure: the engine's turn-failure path does not publish it, so it
// never describes the turn's own stop.
var turnStopRows = memberSet(
	"turn.guard_breach",
	"budget_exhausted",
	"llm_unavailable",
)

// givenRows are what the turn was given to work from: `agent_turn_started`,
// published before its context is gathered and carrying the trigger that woke
// it and the item it is charged to; what its prompt was assembled from before
// the first phase ran; and a person's note it was handed mid-turn, which says
// whether the turn read it or ended before its next round.
var givenRows = memberSet(
	"agent_turn_started",
	"prefetch_summary",
	"prompt.size",
	"agent_turn_steered",
)

// leftBehindRows are what the seat learned from the turn, after its last
// phase — the reflection pass.
//
// `skill_promoted` is NOT here although it reads like one of these: the
// curator duty promotes a unit's skill off a cluster of many seats' turns, so
// it names no turn and there is no single right one to name. `skill_revived`
// IS here, with two producers: the reflection worker revives a skill this turn
// was offered and stamps the turn, while the curator revives one an operator
// restored by hand and stamps nothing. Only the first can be in a turn's
// answer, which is the correct half.
var leftBehindRows = memberSet(
	"episode_written",
	"persist_decider_completed",
	"counterparty_profile_updated",
	"reflection_completed",
	"skill_synthesized",
	"skill_refined",
	"skill_revived",
)

// didRows are work the turn did that is not a phase.
//
// THE COLLEAGUE ROWS carry the turn that published them (internal/a2a), which
// is what puts an ask in the answer at all. One ask is three rows — the
// channel, the brief and the close — and that is the exchange rather than
// noise. A close with no turn behind it — the maintenance sweep reaping a
// channel nobody answered — carries an empty turn id and is in no turn's
// answer, which is the band working: a swept close is the statement that no
// turn finished.
//
// `sandbox_run_answered` is here beside the question it answers: a parked
// run's answer is work that happened to this turn between its two segments.
//
// `knowledge_read` is here for every one of its ways in, the engine's choices
// (`prefetch`, `skill_injected`) as well as the model's: GIVEN draws only the
// prefetch blocks and the prompt weights, so a read filed there would go
// unrendered, while here its summary names the pages, which is what a reader
// scanning a turn for "what did it look at" is after.
//
// `task_assigned` is NOT here: it is the scheduler's cron fire, the wake that
// starts a turn rather than work a turn did, and it precedes every turn id
// there could be. The trigger is already on the screen, as the brief.
var didRows = memberSet(
	"sandbox_run_started",
	"sandbox_clarification_requested",
	"sandbox_run_answered",
	"sandbox_run_completed",
	"subagent_batched",
	"skill_used",
	"knowledge_read",
	"a2a_channel_opened",
	"a2a_message_sent",
	"a2a_channel_closed",
)

// BandOf is the band a turn's row belongs in: its type, and whether the work it
// reports failed ([Failed]'s answer for it — the store stamps that on every row
// it reads).
//
// ABSORPTION OUTRANKS THE FAILURE, which is the one precedence worth stating
// because it looks like an exception and is not: the destination draws the
// failure itself. A failed `agent_phase_completed` is a phase card carrying the
// engine's own error kind and text, so banding it `went_wrong` as well would put
// a red row above the card that already says so, and add one to a count of
// problems that gained no problem.
//
// THEN A FAILED ROW IS WHAT WENT WRONG whatever its set says: a coding run that
// came back failed is something that went wrong on this turn even though a
// completed run is ordinary work, and a type this build has never seen is still
// loud when it fails.
func BandOf(eventType string, failed bool) TurnBand {
	switch _, absorbed := absorbedRows[eventType]; {
	case absorbed:
		return BandAbsorbed
	case failed || wentWrongRows[eventType]:
		return BandWentWrong
	case givenRows[eventType]:
		return BandGiven
	case leftBehindRows[eventType]:
		return BandLeftBehind
	case didRows[eventType]:
		return BandDid
	}
	return BandRest
}

// AbsorbedInto is where the Turn screen already draws a row of eventType, in
// the words its inventory prints, or "" for a type that is a row.
func AbsorbedInto(eventType string) string { return absorbedRows[eventType] }

// StopsTheTurn reports whether a row of eventType describes the engine stopping
// the turn — the same stop the turn's own completion reports as failed — so a
// count of what went wrong counts the stop once. See [turnStopRows].
func StopsTheTurn(eventType string) bool { return turnStopRows[eventType] }

// memberSet is a membership map over names, for the declarations above.
func memberSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}
