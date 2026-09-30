/**
 * The builder core's interfaces, bound to the browser.
 *
 * THE ONE PLACE THE MODEL MEETS A GLOBAL. `model/` takes time, storage,
 * randomness and the network as arguments so every rule in it runs under a
 * test with a fake clock and a scripted engine; `Builder.tsx` owns their
 * lifetimes, and this module is what it hands over.
 *
 * - The TRANSPORT resolves with every answer, refusals included, as the model
 *   requires, and rejects only when the caller aborted. `protocol/rest.ts`
 *   throws on a refusal; a thrown refusal is turned back into the status and
 *   body the engine sent, and anything else that never reached the engine is
 *   status 0.
 * - The CLOCK is monotonic (`performance.now`), because the check's debounce
 *   and backoff are durations and a wall clock moved by NTP or a suspended
 *   laptop would fire them early or never.
 * - Randomness is `crypto.getRandomValues`, which every browsing context has.
 *   `crypto.randomUUID` does not: it exists only in a secure context, and the
 *   dashboard of a node reached at `http://10.0.0.4:8000` is not one.
 * - Storage is `sessionStorage`, reached through a guarded read because the
 *   accessor itself can throw (a sandboxed frame, blocked site data).
 */

import { layoutOpID } from "~/protocol/gate.ts";
import { isAbort, rest, RestError, type RestResponse } from "~/protocol/index.ts";
import type { DraftStorage } from "./model/persistence.ts";
import type { KeySource } from "./model/keys.ts";
import type { Clock, EngineTransport, HttpAnswer } from "./model/transport.ts";

/** What a request that never reached the engine answers: status 0 with its reason. */
function unreachable(err: unknown): HttpAnswer {
  return {
    status: 0,
    body: {
      error: "unreachable",
      detail: err instanceof Error ? err.message : "The engine could not be reached.",
    },
    etag: null,
  };
}

/**
 * Runs one REST call and resolves with the answer, refusal or not. An abort
 * rejects, because a superseded request is not an engine that answered.
 */
export async function answerOf(call: Promise<RestResponse>): Promise<HttpAnswer> {
  try {
    const { status, body, etag } = await call;
    return { status, body, etag };
  } catch (err) {
    if (isAbort(err)) throw err;
    // THE ENGINE'S RETRY HINT RIDES THE ANSWER ([HttpAnswer.retryAfter]),
    // read by the rule [RestError.retryHint] keeps, so the model holds no
    // second copy of which 503 is the engine's.
    if (err instanceof RestError) {
      return { status: err.status, body: err.body, etag: null, retryAfter: err.retryHint };
    }
    return unreachable(err);
  }
}

/** The settings and the org chart, over the dashboard's one REST path. */
export const restTransport: EngineTransport = {
  send: (request, signal) =>
    answerOf(
      rest.request(request.method, request.path, {
        query: request.query,
        contentType: request.contentType,
        headers: request.headers,
        body: request.body,
        signal,
      }),
    ),
  settings: (signal) => answerOf(rest.request("GET", "/config", { signal })),
  revision: (id, signal) =>
    answerOf(rest.request("GET", `/config/revisions/${encodeURIComponent(id)}`, { signal })),
  chart: (signal) =>
    answerOf(rest.request("GET", "/chart", { query: { runtime: "true" }, signal })),
};

/** A monotonic clock and one-shot timers. */
export const browserClock: Clock = {
  now: () => performance.now(),
  setTimer: (callback, ms) => {
    const timer = setTimeout(callback, ms);
    return () => clearTimeout(timer);
  },
};

/**
 * How many random bytes a minted token carries. Sixteen bytes is 128 bits,
 * the entropy of a random UUID: enough that two keys minted in one draft, or
 * two write ids from two tabs, never collide. Hex-encoded that is 32
 * characters, inside the 64 a minted key and a write id may hold.
 */
const TOKEN_BYTES = 16;

/** Random tokens for minted node keys and write ids. */
export const randomKeys: KeySource = {
  next: () => {
    const bytes = crypto.getRandomValues(new Uint8Array(TOKEN_BYTES));
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  },
};

/**
 * A fresh write id (`model/writes.ts`'s `isWriteId`): a bare operation id in the
 * engine's grammar, minted in the browser like a gate's (`newGateOpID`) and on
 * the browser's clock for its reason. Call it in the event handler that saves.
 */
export function newWriteId(
  now: number = Date.now(),
  random: (bytes: Uint8Array<ArrayBuffer>) => Uint8Array = (bytes) => crypto.getRandomValues(bytes),
): string {
  return layoutOpID(now, random(new Uint8Array(10)), "");
}

/** The tab's session storage, or `null` when the browser refuses it. */
export function sessionDraftStorage(): DraftStorage | null {
  try {
    return globalThis.sessionStorage ?? null;
  } catch {
    return null;
  }
}
