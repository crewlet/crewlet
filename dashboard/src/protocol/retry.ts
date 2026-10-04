/**
 * When to ask the engine again after it said it cannot answer yet.
 *
 * THE ENGINE SAYS WHEN, and every retry path in this dashboard reads that one
 * way, here. A socket query's or a watch's `unavailable` frame carries
 * `retry_after`, and a REST `503` the engine wrote carries a `Retry-After`
 * header; both are whole seconds decided by the engine — the refusal's derived
 * hint where it has one, the node's health tick where it has none — and ZERO
 * where waiting will not change the answer: a log at its byte ceiling, a
 * record this node cannot decode, a barrier its broker refused.
 *
 * PURE ARITHMETIC OVER A NUMBER, bounded by the engine's own ceiling
 * {@link RETRY_AFTER_MAX_MS}, declared in `contract/retry.ts` beside the
 * fallback a caller waits when an answer carried no hint at all.
 */

// RELATIVE, because this directory is also built alone as `protocol.js`, where
// the `~` alias does not exist.
import { RETRY_AFTER_MAX_MS } from "../contract/retry.ts";

/**
 * The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
 * ask again on a timer".
 *
 * ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
 * change it, so nothing re-asks — the screen shows the refusal and what it
 * names, and it is asked again only when something a person does could have
 * changed it (a reconnect, a write, a reload). A caller whose answer carried no
 * hint at all does not come here: what an absent hint means is the caller's.
 *
 * BOUNDED, because the one hint the engine does not fix — a backlog divided by
 * a drain rate — runs to minutes on a node that has just joined, and a screen
 * that says "this fills in on its own" for that long is not believed.
 *
 * The hint is whole seconds and never negative — both parsers that read one
 * admit nothing else — so anything at or under zero is the zero.
 */
export function retryAfterMs(seconds: number): number | null {
  if (!(seconds > 0)) return null;
  return Math.min(seconds * 1_000, RETRY_AFTER_MAX_MS);
}
