/**
 * Whether an answer is the ENGINE'S OWN, the one rule every reader of a
 * failed request shares.
 *
 * Every refusal the engine writes is JSON with an `error` code, its auth
 * guard's included and its router's own `no_route` and `method_not_allowed`.
 * So an answer is the engine's only when something came back (status 0 is a
 * request never fully answered: a dropped connection, the client's own
 * deadline), it carries a code, and the code is not `unreadable_body`, which
 * `rest.ts` gives a body that was not the engine's JSON (a gateway's HTML
 * page, a 200 cut off part way through).
 *
 * WHY IT MATTERS: an answer the engine wrote says what it did, and one it did
 * not write says nothing about it. A 504 from a proxy that gave up waiting is
 * an engine that may still have written; read as "the engine failed", a
 * screen blamed the engine for an answer it never sent.
 *
 * PURE AND IMPORTING NOTHING, so `rest.ts` ([RestError.unanswered]) and the
 * configuration classifier (`configAnswer.ts`, which the org builder's pure
 * model reads) share this one copy.
 */

/** Whether an answer of `status` carrying the error code `code` was written by the engine. */
export function answeredByEngine(status: number, code: string): boolean {
  return status !== 0 && code !== "" && code !== "unreadable_body";
}
