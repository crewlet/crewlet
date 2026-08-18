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
  async snapshot() {
    try {
      const response = await fetch(BASE + "/stream/snapshot");
      if (!response.ok) return { _error: response.status };
      return await response.json();
    } catch {
      return { _error: 0 };
    }
  },
};
