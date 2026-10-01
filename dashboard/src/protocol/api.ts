/**
 * The one HTTP read the dashboard still makes.
 *
 * Everything else goes over the WebSocket — state arrives as pushes and
 * anything on demand is a query on the same socket. This remains for exactly
 * one case: a browser that cannot upgrade to a WebSocket at all, usually a
 * corporate proxy. While the socket is down the client polls this snapshot so
 * the page keeps telling the truth, and it stops the moment the socket is back.
 *
 * It had a second entry once, and that one is why the Fleet screen shipped
 * dead: a screen reaching for its own transport takes its client from
 * somewhere, and the somewhere it chose was a context field the shell never
 * populated. There is one transport for reads, and only `socket.ts` imports
 * this file.
 *
 * The REST API itself is much larger than this — it is a public read surface
 * documented in docs/reference/api-endpoints.md. The dashboard simply does not
 * use it.
 */

import { retryHintOf } from "./rest.ts";
import type { Snapshot } from "./types.ts";

/**
 * What one read of the degraded-mode snapshot came to: the snapshot, or no
 * snapshot and when the engine said to ask again.
 *
 * TWO STATES THE CALLER CANNOT MISTAKE FOR EACH OTHER, and never an error
 * object beside a snapshot's fields. That shape was tried: the caller guarded
 * with `!snap._error`, which is TRUE for zero, so the one case this whole
 * fallback exists for — the network completely gone — applied the error object
 * as if it were a snapshot and replaced agents, events, sandboxes, org and
 * tools with empties. The page went blank at the exact moment the last state it
 * received was the only thing it had.
 */
export type SnapshotRead =
  | { readonly state: "read"; readonly snapshot: Snapshot }
  | {
      readonly state: "unread";
      /**
       * [RestError.retryHint]: a `503` the engine wrote says when to ask
       * again, zero where waiting will not change it; null for everything
       * else — the network, a proxy, any other refusal.
       */
      readonly retryAfter: number | null;
    };

export const api = {
  /** The degraded-mode snapshot, or why not and when to ask again. */
  async snapshot(): Promise<SnapshotRead> {
    try {
      // The session cookie is the credential, as it is on every request this
      // dashboard makes (see rest.ts).
      const response = await fetch(location.origin + "/stream/snapshot", {
        credentials: "same-origin",
      });
      if (!response.ok) {
        // WHOSE REFUSAL, read by the rule every other read takes: only a
        // 503 carrying the engine's own error code carries its hint.
        return { state: "unread", retryAfter: await retryHintOf(response) };
      }
      return { state: "read", snapshot: (await response.json()) as Snapshot };
    } catch {
      // A refused connection, a DNS failure, or a proxy answering 200 with an
      // HTML error page (which fails to parse as JSON).
      return { state: "unread", retryAfter: null };
    }
  },
};
