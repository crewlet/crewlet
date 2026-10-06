/**
 * A running coding run's live view, as the engine bounds it
 * (`internal/sandbox`, livetail.go and livefeed.go).
 *
 * Each is a COPY of something the engine owns, because this is a separate
 * build that cannot import a Go identifier — held to the engine's by
 * `internal/sandbox`'s `client_gate_test.go`. What is done with them is
 * behaviour, and lives in `lib/useLiveTail.ts` and the live output it draws.
 */

/**
 * The outcomes a `sandbox_tail` answer names — `sandbox.TailOutcomes`.
 *
 * `launching` and `box_paused` are NOT terminal — a job still being set up, a
 * box paused between its collection and its record moving on — and a screen
 * keeps asking through both; only `not_running` ends the asking.
 */
export const SANDBOX_TAIL_OUTCOMES = [
  "tail",
  "launching",
  "not_running",
  "owner_silent",
  "owner_upgrading",
  "box_paused",
] as const;

/**
 * The most of a running job's output the live view holds, in UTF-8 bytes —
 * `sandbox.MaxRunTextBytes`, the most the run's own record will hold of it.
 * The view keeps the END of what it has been sent, trimmed on a line.
 */
export const LIVE_OUTPUT_MAX_BYTES = 262_144;

/**
 * How often an open coding run's live output is asked for, in ms.
 *
 * THREE SECONDS, chained after each answer, so an open view has one request in
 * flight. Above the owner's reuse window (`sandbox.LiveReuse`, two seconds) so
 * a single viewer is read fresh on every poll while any number of viewers
 * share one read of the box, and above the fleet read budget
 * (`sandbox.TailReadBudget`) so a poll never starts before the last one's
 * owner could have answered. Short enough that the agent's current step is on
 * screen while it is still the current step; it costs nothing while nobody
 * looks.
 */
export const SANDBOX_TAIL_POLL_MS = 3_000;
