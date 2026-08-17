// Live-stream WebSocket client with reconnect/backoff, heartbeat, and a
// REST snapshot fallback for when the socket is down or can't upgrade.

import { apiToken, ensureTokenForApi, isAuthError } from "./authToken.js";

const PATH = "/ws/stream";
const MAX_BACKOFF = 30000;
const PING_INTERVAL = 25000;
const FALLBACK_INTERVAL = 5000;

export class LiveStream {
  constructor(store, api) {
    this.store = store;
    this.api = api;
    this.sock = null;
    this.attempt = 0;
    this.reconnectTimer = 0;
    this.pingTimer = 0;
    this.fallbackTimer = 0;
    this.closed = false;
    this.opened = false;
  }

  start() {
    // Paint immediately from REST so the UI isn't blank during the
    // handshake; the snapshot dedups against the WS handshake snapshot.
    this._fallbackFetch();
    this._connect();
  }

  _connect() {
    if (
      this.sock &&
      (this.sock.readyState === WebSocket.OPEN ||
        this.sock.readyState === WebSocket.CONNECTING)
    ) {
      return;
    }
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // The WebSocket constructor cannot send an Authorization header, so
    // the token rides the query string. The server accepts either form;
    // a rejected handshake closes with 1008 and the reconnect backoff
    // takes over (the views show the token gate on the parallel 401s).
    const token = apiToken();
    const qs = token ? `?token=${encodeURIComponent(token)}` : "";
    let sock;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}${qs}`);
    } catch {
      this._scheduleReconnect();
      return;
    }
    this.sock = sock;

    sock.onopen = () => {
      // A reconnect missed every event during the outage; the snapshot
      // re-hydrates agent state (incl. per-agent token totals), but the
      // /tokens/breakdown rollup is fetched separately and folded
      // forward — re-establish its baseline so it doesn't drift below
      // the per-agent rows it sits above.
      const reconnected = this.opened;
      this.opened = true;
      this.attempt = 0;
      clearTimeout(this.reconnectTimer);
      this._stopFallback();
      this._startPing();
      if (reconnected) this._refreshTokens();
    };
    sock.onmessage = (e) => this._onMessage(e.data);
    sock.onclose = () => {
      this._stopPing();
      this.sock = null;
      this.store.setConnected(false);
      this._scheduleReconnect();
      this._startFallback();
    };
    sock.onerror = () => {
      // close handler runs next; just surface the disconnected state.
      this.store.setConnected(false);
    };
  }

  _onMessage(raw) {
    let msg;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    switch (msg.kind) {
      case "snapshot":
        this.store.applySnapshot(msg.data);
        break;
      case "event":
        this.store.applyEvent(msg.data);
        break;
      case "health":
        this.store.applyHealth(msg.data);
        break;
      case "pong":
        break;
    }
  }

  _scheduleReconnect() {
    if (this.closed) return;
    clearTimeout(this.reconnectTimer);
    const delay = Math.min(1000 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF);
    this.attempt++;
    this.reconnectTimer = setTimeout(() => this._connect(), delay);
  }

  _startPing() {
    this._stopPing();
    this.pingTimer = setInterval(() => {
      if (this.sock && this.sock.readyState === WebSocket.OPEN) {
        try {
          this.sock.send(JSON.stringify({ kind: "ping" }));
        } catch {
          /* ignore */
        }
      }
    }, PING_INTERVAL);
  }
  _stopPing() {
    clearInterval(this.pingTimer);
    this.pingTimer = 0;
  }

  _startFallback() {
    if (this.fallbackTimer) return;
    this.fallbackTimer = setInterval(() => this._fallbackFetch(), FALLBACK_INTERVAL);
  }
  _stopFallback() {
    clearInterval(this.fallbackTimer);
    this.fallbackTimer = 0;
  }
  async _fallbackFetch() {
    const snap = await this.api.snapshot();
    if (isAuthError(snap)) {
      // Every view is fed from this snapshot, so a 401 here means the
      // dashboard has nothing to show. Ask for a token rather than
      // rendering an empty shell that looks like a broken engine.
      ensureTokenForApi();
      return;
    }
    if (snap && !snap._error) this.store.applySnapshot(snap);
  }

  // Re-fetch the token breakdown baseline for the window the views last
  // requested. Only runs once a baseline exists — the dashboard / tokens
  // views own the initial fetch on demand.
  async _refreshTokens() {
    const t = this.store.state.tokens;
    if (!t) return;
    const win = t.window || 7;
    const d = await this.api.tokens({ sinceDays: win });
    if (d && !d._error) this.store.setTokens(win, d);
  }

  stop() {
    this.closed = true;
    clearTimeout(this.reconnectTimer);
    this._stopPing();
    this._stopFallback();
    if (this.sock) this.sock.close();
  }
}
