/**
 * The event types a turn's non-phase rows are sorted by, as the engine's wire
 * allows them — the sets `lib/turnstory.ts` files rows into, declared here
 * because which types can reach a turn's answer at all is a fact the ENGINE
 * owns.
 *
 * # A BAND ENTRY IS A QUERY PREDICATE, NOT A WISH
 *
 * Every row these sets sort came from ONE read: `EventLog.Turn` in
 * `internal/store/eventlog.go`, which is `WHERE turn_id = ?`. That column is
 * filled from the event's own top-level `turn_id` FIELD — `store.ExtractTags`
 * pulls it out of the marshalled envelope and `EventLog.Append` copies it
 * across — so a payload that declares no `turn_id` key writes an empty column,
 * never matches the predicate, and cannot reach the Turn screen however it is
 * banded. Naming one here is not a rule about where it goes: it is a promise
 * the wire cannot keep, and it fails EMPTY, which is the one way a panel
 * cannot say it is broken.
 *
 * Eight types were named here that way. The three A2A audit records were the
 * loudest, because the "What else it did" band advertised "colleagues" on
 * their behalf and an A2A ask had never once appeared under it. The rest
 * carried the same defect quietly: `task_assigned` is the SCHEDULER's cron
 * fire, published before the turn it wakes exists; `turn_trigger_skipped` and
 * `notification_skipped` are both records that NO turn ran; `skill_promoted`
 * is the curator duty promoting a unit's skill across many turns; and
 * `skill_telemetry_write_failed` was a reflection worker that held the turn id
 * and did not stamp it.
 *
 * FOUR OF THE EIGHT WERE FIXED IN THE ENGINE rather than written off here, and
 * they are back: `internal/a2a/service.go` now stamps the publishing turn on
 * all three A2A records, and `internal/learning/skilluse.go` stamps the turn
 * whose reflection attempted the counter write. The four still absent are the
 * ones that describe no single turn, which no change to a payload can alter.
 *
 * `internal/events/types/turnbands_client_test.go` holds these sets against the
 * frozen wire contract, in both directions — so a band entry that can never
 * fill fails the build, and so does a kept-out type that GAINS a `turn_id` and
 * is therefore ready to come back. The second direction is what made this
 * repair land in one change instead of being noticed a release later.
 *
 * AND A THIRD: every type this build stores with a `turn_id` is placed here,
 * in a band or in ABSORBED. The residual band is for a type a NEWER build
 * publishes, which must still render; this build's own types get a decision in
 * the change that adds them, or they are drawn under "everything else" on every
 * turn — the flat list these sets were introduced to end.
 */

/**
 * Types that are ALREADY on the screen in a better form, and are therefore not
 * rows.
 *
 * Each names where it went instead — the point is that nothing is silently
 * dropped, and the next reader can check the claim.
 */
export const ABSORBED: Readonly<Record<string, string>> = {
  // Folded onto its own phase card as that phase's start instant.
  agent_phase_started: "the phase card it opens",
  // The phase cards themselves.
  agent_phase_completed: "its own phase card",
  // Stream-only; never persisted, so it cannot be in a query answer anyway.
  agent_turn_progress: "the live phase card",
  // The turn's opening record, published before its context is gathered.
  // The header names the seat and what woke the turn off it until a phase
  // lands, and off the phases after — the same seat and the same wake.
  agent_turn_started: "the turn's header: its seat and what woke it",
  // The stat strip, the header and the turn record.
  agent_turn_completed: "the turn's header and record",
  turn_completed: "the turn's header and record",
};

/**
 * The engine talking about something going wrong — a turn the engine stopped,
 * a chain that fell through, a guard that fired, a refusal — and a turn a
 * PERSON stopped, which is the same question asked of the turn from outside:
 * why did it not finish? `agent_turn_stopped` names who paused the seat with
 * the stop. It is deliberately not in TURN_STOP below: its turn's completion
 * carries `stopped`, never `failed`, so there is no failure for it to double.
 *
 * These are the rows the old panel's subtitle promised ("fallbacks, guard
 * breaches") and — until the events behind them were given a producer and a
 * turn id — could never actually contain. Seven of the nine that claim carry:
 * every one below publishes `turn_id`, which is what puts it in the answer
 * `lib/turnstory.ts` sorts. `skill_telemetry_write_failed` is the seventh and the most
 * recent, and it is here rather than on the roster because the engine stopped
 * dropping the id rather than because the panel lowered its bar — the counter
 * write that failed happened inside a turn's reflection, so the turn that used
 * the skill is exactly where an operator goes looking for it. The two still
 * absent are on the kept-out roster in
 * `internal/events/types/turnbands_client_test.go`: a skipped trigger and a
 * skipped notification are both records that NO turn ran.
 */
export const WENT_WRONG: ReadonlySet<string> = new Set([
  "turn.guard_breach",
  "llm_unavailable",
  "budget_exhausted",
  "provider_fallback",
  "phase.tool_skill_blocked",
  "sandbox_run_failed",
  "skill_telemetry_write_failed",
  "agent_turn_stopped",
]);

/**
 * The three records `publishFailure` writes for a turn the engine STOPPED.
 *
 * A subset of WENT_WRONG, and the distinction is the whole of it: these are
 * the rows that describe the same stop `agent_turn_completed.failed` already
 * reports, so a reader counting problems must not count both. Everything else
 * in WENT_WRONG — a recovered provider fallback, a refused tool skill, a
 * failed sandbox run — is an INDEPENDENT problem that happens to share a turn
 * with the stop. `sandbox_run_failed` is deliberately not here although it is
 * a failure: `internal/engine/telemetry.go` does not publish it, so it never
 * describes the turn's own stop.
 */
export const TURN_STOP: ReadonlySet<string> = new Set([
  "turn.guard_breach",
  "budget_exhausted",
  "llm_unavailable",
]);

/**
 * What the turn was given to work from: what its prompt was assembled from
 * before the first phase ran, and a person's note it was handed mid-turn —
 * `agent_turn_steered`, which says whether the turn read the note or ended
 * before its next round.
 */
export const GIVEN: ReadonlySet<string> = new Set([
  "prefetch_summary",
  "prompt.size",
  "agent_turn_steered",
]);

/**
 * What the turn changed about the company, after its last phase.
 *
 * `skill_promoted` is NOT here, although it is the reflection estate's own
 * event and reads like one of these: the curator duty promotes a unit's skill
 * off a cluster of many seats' turns, so it names no turn and there is no
 * single right one to name. That is the same reason `skill_synthesized`
 * carries an agent id and an empty turn id on its clustered path.
 *
 * `skill_revived` IS here, and it has the same two-producer shape as
 * `skill_synthesized` rather than `skill_promoted`'s: the reflection worker
 * revives a skill this turn was offered and stamps the turn, while the curator
 * revives one an operator restored by hand and stamps nothing. Only the first
 * can be in a turn's answer, which is the correct half — a skill coming back
 * into use because this seat used it is something this turn changed about the
 * company.
 */
export const LEFT_BEHIND: ReadonlySet<string> = new Set([
  "episode_written",
  "persist_decider_completed",
  "counterparty_profile_updated",
  "reflection_completed",
  "skill_synthesized",
  "skill_refined",
  "skill_revived",
]);

/**
 * Work the turn did that is not a phase: a coding run, a delegation, a tool,
 * and asking a colleague.
 *
 * THE COLLEAGUE ROWS ARE BACK, and what changed is the wire rather than this
 * set. The three A2A audit records were named here for as long as this file
 * has existed while carrying no `turn_id`, so an ask was never once drawn
 * under this heading; they now carry the turn that published them
 * (`internal/a2a/service.go`), which is what puts them in `EventLog.Turn`'s
 * answer. One ask draws three rows — the channel, the brief, and the close —
 * and that is the exchange rather than noise: the brief is the question this
 * turn asked, and the close is the answer having arrived.
 *
 * A CLOSE WITH NO TURN BEHIND IT IS NOT ONE OF THESE. `a2a_channel_closed` has
 * a second producer — the maintenance sweep reaping a channel nobody answered
 * — which carries an empty turn id and therefore cannot be in any turn's
 * answer. That is the band working rather than a hole in it: a swept close is
 * precisely the statement that no turn finished.
 *
 * `sandbox_run_answered` is here beside the question it answers: a parked
 * run's answer is work that happened to this turn between its two segments,
 * whichever route it came by.
 *
 * `task_assigned` was named here too and is a different mistake: it is the
 * SCHEDULER's cron fire — the wake that starts a turn — rather than work a
 * turn did. The trigger is already on the Turn screen, as the brief.
 *
 * `knowledge_read` is here for every one of its ways in, the two the ENGINE
 * chose (`prefetch`, `skill_injected`) as well as the three the model did. The
 * GIVEN band draws only the prefetch blocks and the prompt weights, so a row
 * filed there would go unrendered; here its summary names the pages and the
 * query, which is what a reader scanning a turn for "what did it look at" is
 * after.
 */
export const DID: ReadonlySet<string> = new Set([
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
]);
