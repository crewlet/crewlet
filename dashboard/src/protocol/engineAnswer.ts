/**
 * Whether an answer is the ENGINE'S OWN, the one rule every reader of a
 * failed request shares.
 *
 * An answer is the engine's only when all of these hold:
 *
 *  - something came back: status 0 is a request never fully answered (a
 *    dropped connection, the client's own deadline);
 *  - it carries the engine's mark, [ENGINE_HEADER], which the engine sets on
 *    every answer its API writes and no proxy sets. The error envelope alone
 *    could be forged by anything in front of the engine: a gateway answering
 *    JSON with an `error` field read as the engine refusing;
 *  - it carries an error code, and the code is not `unreadable_body`, which
 *    `rest.ts` gives a body that was not the engine's JSON (a 200 cut off part
 *    way through).
 *
 * WHY IT MATTERS: an answer the engine wrote says what it did, and one it did
 * not write says nothing about it. A 504 from a proxy that gave up waiting is
 * an engine that may still have written, and a gateway's 403 or 413 is no
 * refusal of the engine's; read as the engine's, a screen blamed the engine
 * for an answer it never sent, or told a person their write was refused when
 * nobody knows.
 *
 * PURE, IMPORTING NOTHING BUT THE CONTRACT, so `rest.ts` ([RestError.unanswered])
 * and the configuration classifier (`configAnswer.ts`, which the org
 * builder's pure model reads) share this one copy.
 */

import { ENGINE_HEADER, ENGINE_HEADER_VALUE } from "../contract/engine.ts";

/** Whether a response's headers carry the engine's mark: `get` is `Headers.get`. */
export function markedByEngine(headers: { get(name: string): string | null }): boolean {
  return headers.get(ENGINE_HEADER) === ENGINE_HEADER_VALUE;
}

/**
 * Whether an answer of `status`, carrying the error code `code`, was written
 * by the engine; `marked` is whether it carried the engine's mark
 * ([markedByEngine]).
 */
export function answeredByEngine(status: number, code: string, marked: boolean): boolean {
  return status !== 0 && marked && code !== "" && code !== "unreadable_body";
}
