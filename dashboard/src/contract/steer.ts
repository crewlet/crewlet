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
