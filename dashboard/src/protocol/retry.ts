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
 * by the socket, so it imports nothing but the contract module that declares
 * the engine's two waits — itself data and nothing else, which the contract's
 * own suite holds: the builder's model takes nothing from this directory at
 * runtime but this file, because the rest of it is the socket and `fetch`
 * (see `routes/org/builder/model/boundary.test.ts`).
 *
 * What it waits is bounded by the engine's own two values — the fallback
 * `UNAVAILABLE_RETRY_MS` and the ceiling {@link RETRY_AFTER_MAX_MS} —
 * declared in `contract/retry.ts`.
 */

// THE ENGINE'S TWO WAITS ARE DECLARED IN THE CONTRACT, the one home of every
// value an engine gate holds (`internal/api`'s retry gate reads them there).
// RELATIVE, because this directory is also built alone as `protocol.js`,
// where the `~` alias does not exist.
import { RETRY_AFTER_MAX_MS } from "../contract/retry.ts";

/**
 * The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
 * ask again on a timer".
 *
 * ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
 * change it, so nothing re-asks — the screen shows the refusal and what it
 * names, and it is asked again only when something a person does could have
 * changed it (a reconnect, a write, a reload). A caller whose answer carried no
 * hint at all does not come here: what an absent hint means is the caller's —
 * the socket's is `UNAVAILABLE_RETRY_MS` (`contract/retry.ts`), and a request
 * that never reached the engine backs off ({@link unansweredRetryMs}).
 *
 * The hint is whole seconds and never negative — both parsers that read one
 * admit nothing else — so anything at or under zero is the zero.
 */
export function retryAfterMs(seconds: number): number | null {
  if (!(seconds > 0)) return null;
  return Math.min(seconds * 1_000, RETRY_AFTER_MAX_MS);
}

/**
 * The wait before the first retry of a request NOBODY ANSWERED, in
 * milliseconds: one that never came back (its deadline passed, the connection
 * dropped), or one something in front of the engine answered instead.
 *
 * THERE IS NO HINT TO WAIT OUT, because the engine said nothing — and that is
 * not the engine saying waiting will not change it, which is the zero above.
 * So the wait is the client's own, and it BACKS OFF: asking at once would
 * hammer an engine that is restarting, or a network that is down, with
 * requests that each wait out the transport's deadline. One second is long
 * enough not to spin against a refused connection and short enough to notice
 * a restarted engine the moment it accepts one.
 */
export const UNANSWERED_RETRY_BASE_MS = 1_000;

/**
 * The longest wait between retries of a request nobody answered, in
 * milliseconds: `REQUEST_TIMEOUT_MS` in `rest.ts`, the longest one attempt may
 * itself take, so an engine that recovers is never noticed later than one more
 * attempt would have taken to fail. `retry.test.ts` holds the two equal; this
 * file imports nothing from this directory, so it cannot name the other.
 */
export const UNANSWERED_RETRY_MAX_MS = 30_000;

/**
 * The wait before retry `failures` of a request nobody answered — 1 for the
 * first — doubling from {@link UNANSWERED_RETRY_BASE_MS} up to
 * {@link UNANSWERED_RETRY_MAX_MS}.
 *
 * ONE BACKOFF for every such request: the org builder's check and a screen's
 * REST read are the same question put to the same engine, and two copies of
 * the arithmetic would be two answers to how hard this page leans on a node
 * that is not answering.
 */
export function unansweredRetryMs(failures: number): number {
  const exponent = Math.max(0, failures - 1);
  return Math.min(UNANSWERED_RETRY_MAX_MS, UNANSWERED_RETRY_BASE_MS * 2 ** Math.min(exponent, 30));
}
