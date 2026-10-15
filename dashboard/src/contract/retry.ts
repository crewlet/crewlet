/**
 * The two waits the dashboard's retries are bounded by that the ENGINE owns:
 * what it says when it has nothing better, and the longest hint it fixes.
 *
 * Each is a COPY of an engine value, held to it by `internal/api`'s
 * `TestTheDashboardRetriesOnTheEnginesOwnHints`. What is done with them — the
 * arithmetic over a hint — is behaviour, and lives in `protocol/retry.ts`.
 */

/**
 * How soon an `unavailable` answer that carries NO hint is asked again, in
 * milliseconds.
 *
 * ONLY THE FALLBACK. Every `unavailable` frame the engine sends carries
 * `retry_after`, so an answer without one is one nobody decided a hint for.
 * FIVE SECONDS because that is what the engine itself says when it has nothing
 * better: its shared health tick (`stream.HealthInterval`), the soonest a
 * node's posture can change and the hint every `unavailable` it cannot say
 * more about carries. Waiting it out is asking as the engine would have asked;
 * sooner asks before anything could have changed, and later leaves a recovered
 * node looking broken.
 */
export const UNAVAILABLE_RETRY_MS = 5_000;

/**
 * The longest a hint is waited out before asking again anyway, in
 * milliseconds.
 *
 * A BOUND ON THE ONE HINT THAT HAS NONE. Every hint the engine fixes — the
 * health tick (5 s), a quorum election (4 s), the identity estate's and an
 * undecidable authority's two seconds, a surface another writer holds (3 s),
 * the reconcile poll that brings a company and the floor's heartbeat (15 s), a
 * drain (30 s) — is at or under thirty seconds and is waited out exactly. The
 * one this cuts is DERIVED: a node's backlog divided by the rate it has been
 * draining at, which is minutes on a node that has just joined or restarted
 * behind a busy log, and an estimate made from a rate that only rises as the
 * node warms up. Past thirty seconds a screen waiting on it says "this fills
 * in on its own" for longer than a person believes it, and the cost of asking
 * sooner is one read the node refuses again. THIRTY because it is already this
 * dashboard's ceiling on waiting for an engine to come back — the socket's
 * reconnect backoff and the builder's check backoff stop there for the same
 * reason — and the gate fails the day a hint the engine fixes grows past it.
 */
export const RETRY_AFTER_MAX_MS = 30_000;
