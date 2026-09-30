/**
 * When to ask the engine again after it said it cannot answer yet.
 *
 * THE ENGINE SAYS WHEN, and every retry path in this dashboard reads that one
 * way, here. A socket query's or a watch's `unavailable` frame carries
 * `retry_after`, and a REST `503` the engine wrote carries a `Retry-After`
 * header; both are whole seconds decided by the state log's own rule — the
 * refusal's derived hint where it has one, the node's health tick where it has
 * none — and ZERO where waiting will not change the answer: a log at its byte
 * ceiling, a record this node cannot decode, a barrier its broker refused. The
 * query layer used to ignore all of it and re-ask at a fixed five seconds, so a
 * node draining a long backlog was asked a dozen times before it could have
 * answered once, and a refusal only an operator could lift was asked for as
 * long as the tab stayed open.
 *
 * PURE ARITHMETIC OVER A NUMBER, imported by the org builder's core as well as
 * by the socket, so it imports nothing: the builder's model takes nothing from
 * this directory at runtime but this file, because the rest of it is the socket
 * and `fetch` (see `routes/org/builder/model/boundary.test.ts`).
 */

/**
 * How soon an `unavailable` answer that carries NO hint is asked again, in
 * milliseconds.
 *
 * ONLY THE FALLBACK. Every `unavailable` frame the engine sends carries
 * `retry_after`, so an answer without one is one nobody decided a hint for —
 * during a rolling upgrade the node behind this page's address may be on a
 * build older than the field. FIVE SECONDS because that is what the engine
 * itself says when it has nothing better: its shared health tick
 * (`stream.HealthInterval`), the soonest a node's posture can change and the
 * hint every `unavailable` it cannot say more about carries. Waiting it out
 * is asking as the engine would have asked; sooner asks before anything could
 * have changed, and later leaves a recovered node looking broken.
 * `internal/api`'s `TestTheDashboardRetriesOnTheEnginesOwnHints` holds it to
 * the engine's value.
 */
export const UNAVAILABLE_RETRY_MS = 5_000;

/**
 * The longest a hint is waited out before asking again anyway, in
 * milliseconds.
 *
 * A BOUND ON THE ONE HINT THAT HAS NONE. Every hint the engine fixes — the
 * health tick (5 s), a quorum election (4 s), the identity estate's two
 * seconds, the reconcile poll that brings a company (15 s), a drain (30 s) —
 * is at or under thirty seconds and is waited out exactly. The one this cuts is
 * DERIVED: a node's backlog divided by the rate it has been draining at, which
 * is minutes on a node that has just joined or restarted behind a busy log,
 * and an estimate made from a rate that only rises as the node warms up. Past
 * thirty seconds a screen waiting on it says "this fills in on its own" for
 * longer than a person believes it, and the cost of asking sooner is one read
 * the node refuses again. THIRTY because it is already this dashboard's
 * ceiling on waiting for an engine to come back — the socket's reconnect
 * backoff and the builder's check backoff stop there for the same reason — and
 * `internal/api`'s `TestTheDashboardRetriesOnTheEnginesOwnHints` fails the day
 * a hint the engine fixes grows past it.
 */
export const RETRY_AFTER_MAX_MS = 30_000;

/**
 * The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
 * ask again on a timer".
 *
 * ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
 * change it, so nothing re-asks — the screen shows the refusal and what it
 * names, and it is asked again only when something a person does could have
 * changed it (a reconnect, a write, a reload). A caller whose answer carried no
 * hint at all does not come here: what an absent hint means is the caller's —
 * the socket's is {@link UNAVAILABLE_RETRY_MS}, a request that never reached
 * the engine backs off.
 *
 * The hint is whole seconds and never negative — both parsers that read one
 * admit nothing else — so anything at or under zero is the zero.
 */
export function retryAfterMs(seconds: number): number | null {
  if (!(seconds > 0)) return null;
  return Math.min(seconds * 1_000, RETRY_AFTER_MAX_MS);
}
