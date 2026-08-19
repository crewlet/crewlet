// The one HTTP read the dashboard still makes.
//
// Everything else goes over the WebSocket — state arrives as pushes and
// anything on-demand is a query on the same socket (see socket.js). This
// remains for exactly one case: a browser that cannot upgrade to a
// WebSocket at all, usually a corporate proxy. While the socket is down
// the client polls this snapshot so the page keeps telling the truth,
// and it stops the moment the socket is back.
//
// The REST API itself is much larger than this — it is a public read
// surface documented in docs/reference/api-endpoints.md. The dashboard
// simply no longer uses it.

const BASE = location.origin;

export const api = {
  /**
   * The degraded-mode snapshot, or `null` if it could not be read.
   *
   * `null` and not an `{_error}` object. This used to answer
   * `{_error: response.status}` on a bad status and `{_error: 0}` on a
   * thrown fetch, and the caller guarded with `!snap._error` — which is
   * TRUE for zero. So the one case where the network is completely gone
   * (the case this whole fallback exists for) applied the error object
   * as if it were a snapshot: agents, events, sandboxes, org and tools
   * all replaced with empties. The page went blank at the exact moment
   * the last state it received was the only thing it had.
   */
  async snapshot() {
    try {
      const response = await fetch(BASE + "/stream/snapshot");
      if (!response.ok) return null;
      return await response.json();
    } catch {
      // A refused connection, a DNS failure, or a proxy answering 200
      // with an HTML error page (which fails to parse as JSON).
      return null;
    }
  },
};
