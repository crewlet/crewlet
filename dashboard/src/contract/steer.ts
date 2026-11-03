/**
 * A person's note to a running turn, as the engine bounds it
 * (`internal/agent/steer`).
 *
 * The Steer dialog holds a note at this length before it is sent, so a reader
 * is told the bound where they are typing rather than by a refusal after the
 * press. HELD AGAINST `steer.MaxNoteRunes` by
 * `internal/agent/steer.TestTheDashboardBoundsANoteAtTheEnginesCap` — a
 * figure copied here and changed there would hold a note the engine takes, or
 * send one it refuses.
 */

/** The longest note a turn takes, in characters (Unicode code points). */
export const STEER_NOTE_MAX_RUNES = 2000;

/**
 * When the turn reads a note the engine took (`steer.Reads`), as `steer_turn`
 * answers it beside `pending`: at the turn's next step — a round of the
 * engine's own loop, or a step of its executor in its box — or, for an
 * executor parked on a question, once the question is answered. The dialog's answer
 * says which. HELD AGAINST `steer.ReadsAll` by
 * `internal/agent/steer.TestTheDashboardKnowsWhenANoteIsRead`: a value the
 * engine answers that is missing here would be told as the generic sentence.
 */
export const STEER_READS = ["next_step", "after_answer"] as const;

/**
 * The kinds of step a note is read at (`types.SteerBoundary`), as the turn's
 * `agent_turn_steered` names it: a round of the engine's own loop, or — inside
 * the executor's launch — its brief, a batch of its tool calls, its finish,
 * and its next step. The trace says each in words, and places every read
 * inside a launch on the launch's bar. HELD AGAINST `types.SteerBoundaries` by
 * `internal/events/types.TestTheDashboardNamesEverySteerBoundary`.
 */
export const STEER_BOUNDARIES = ["round", "brief", "tool_batch", "finish", "step"] as const;
