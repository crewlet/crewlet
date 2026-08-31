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

import { apiToken } from "./authToken.ts";
import type { Snapshot } from "./types.ts";

export const api = {
  /**
   * The degraded-mode snapshot, or `null` if it could not be read.
   *
   * `null` and not an `{_error}` object. That shape was tried: the caller
   * guarded with `!snap._error`, which is TRUE for zero, so the one case this
   * whole fallback exists for — the network completely gone — applied the
   * error object as if it were a snapshot and replaced agents, events,
   * sandboxes, org and tools with empties. The page went blank at the exact
   * moment the last state it received was the only thing it had.
   */
  async snapshot(): Promise<Snapshot | null> {
    try {
      const stored = apiToken();
      const response = await fetch(
        location.origin + "/stream/snapshot",
        stored ? { headers: { Authorization: "Bearer " + stored } } : undefined,
      );
      if (!response.ok) return null;
      return (await response.json()) as Snapshot;
    } catch {
      // A refused connection, a DNS failure, or a proxy answering 200 with an
      // HTML error page (which fails to parse as JSON).
      return null;
    }
  },
};
