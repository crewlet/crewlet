/**
 * How often the durable coding-run record is asked again.
 *
 * ONE VALUE for every reader of `sandbox_runs` — the runs board, a run's own
 * page and rail, and the attention queue Home and the Inbox draw. It was two:
 * the board polled every 20 s and the queue every 30 s, so a run that parked
 * on a question appeared on the board and then, up to ten seconds later, as
 * the decision the Inbox asks a person to make — two screens disagreeing about
 * whether anybody was being waited on.
 *
 * TWENTY SECONDS, because nothing pushes this record: a run's status moves on
 * its own row in the coordination store and the live projection carries only
 * what a running-runs panel draws. A run lives for minutes and a parked one
 * waits for a person, who is not answering inside twenty seconds either; the
 * live projection reconciles its own panel against the same record every
 * 30 s, so a shorter poll here would be reading a record that has not moved.
 */
export const RUNS_POLL_MS = 20_000;
