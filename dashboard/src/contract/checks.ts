/**
 * The code host's checks and merge queues, in the engine's words: where an
 * attempt of a check stands, what it was checking, where a queue entry stands
 * and how a gated branch's queue lands a change. The Queue and Checks sections,
 * a change's and a repository's checks tabs and the Inbox card key on these.
 *
 * Each is a COPY of a closed set `internal/code` owns, HELD BOTH WAYS, in
 * order, by `internal/code`'s dashboard tests: a value the engine grew that a
 * list here lacks is a row drawn under no word of its own (and so, by this
 * dashboard's rule, never mistaken for a known one: the screens name an
 * unknown value as the engine sent it), and one kept here that the engine
 * dropped is a filter that matches nothing.
 */

/**
 * Where one attempt of a check stands (`code.CheckStates`), in the order a row
 * moves through it: waiting for a runner, claimed, then one of five endings.
 * `passed` and `failed` MEASURED the commit; `errored` and `cancelled` are the
 * infrastructure's or a person's stop and measured nothing; `skipped` never
 * ran, its change touching none of its paths. Held by
 * `internal/code.TestTheDashboardKnowsEveryStateOfACheck`.
 */
export const CHECK_STATES = [
  "queued",
  "running",
  "passed",
  "failed",
  "errored",
  "cancelled",
  "skipped",
] as const;

/**
 * What an attempt of a check was checking (`code.CheckPurposes`): a change's
 * head revision, a merge queue's candidate, a protected branch's tip, or a
 * landing between a green tip and a red one. Held with [CHECK_STATES].
 */
export const CHECK_PURPOSES = ["head", "candidate", "tip", "bisect"] as const;

/**
 * Where a merge queue's entry stands (`code.QueueStates`), the live ones
 * first: waiting for a candidate, being tested by one — and then landed,
 * ejected by the queue, or taken out by somebody. Held by
 * `internal/code.TestTheDashboardKnowsEveryStateOfAQueue`.
 */
export const QUEUE_STATES = ["waiting", "testing", "landed", "ejected", "dequeued"] as const;
