// The dashboard's single connection to the engine.
//
// State arrives as pushes (`snapshot`, then `agents` / `sandboxes` /
// `tokens` / `events` / `org` / `tools` / `schedules` / `health`), and
// anything the dashboard needs on demand — an agent's LLM history, one
// event's payload, a trace, a different spend window, the configuration
// document — is asked for over the same socket and answered on it.
//
// There are no HTTP fetches in normal operation. The REST snapshot is
// used for exactly one thing: keeping the page honest while the socket
// is down (a proxy that refuses to upgrade, a restarting engine), and it
// stops the moment the socket is back.

import { api } from "./api.js";

const PATH = "/ws/stream";
// Reconnect backoff ceiling. Long enough that a dashboard left open
// against a stopped engine is not hammering it, short enough that
// bringing the engine back feels immediate.
const MAX_BACKOFF_MS = 30_000;
// Application-level keepalive. Comfortably inside the 60s idle timeout
// that most reverse proxies apply to an idle WebSocket.
const PING_MS = 25_000;
// Degraded-mode poll — only ever runs while the socket is down.
const FALLBACK_MS = 5_000;
// How long a query waits before giving up. The slowest query is an
// agent's LLM history, a bounded scan over a 7-day window; ten seconds
// is far beyond its normal latency and still short enough that a view
// shows an error rather than an eternal skeleton.
const QUERY_TIMEOUT_MS = 10_000;

export class LiveSocket {
  constructor(store) {
    this.store = store;
    this.sock = null;
    this.attempt = 0;
    this.reconnectTimer = 0;
    this.pingTimer = 0;
    this.fallbackTimer = 0;
    this.closed = false;
    this.opened = false;
    this.nextQueryId = 1;
    // id → {resolve, reject, timer} for queries awaiting a reply.
    this.inflight = new Map();
    this.token = "";
  }

  /** Operator bearer token, sent with config-family queries. */
  setToken(token) {
    this.token = token || "";
  }

  start() {
    this._connect();
  }

  stop() {
    this.closed = true;
    clearTimeout(this.reconnectTimer);
    this._stopPing();
    this._stopFallback();
    this._failInflight("closed");
    if (this.sock) this.sock.close();
  }

  get connected() {
    return !!this.sock && this.sock.readyState === WebSocket.OPEN;
  }

  /**
   * Ask the server for something and resolve with its reply.
   * Rejects with an Error carrying the server's machine-readable code
   * (`not_found`, `unauthorized`, `no_event_store`, …).
   */
  query(what, params = {}) {
    if (!this.connected) {
      return Promise.reject(new Error("offline"));
    }
    const id = this.nextQueryId++;
    const frame = { kind: "query", id, what, params };
    if (this.token) frame.token = this.token;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.inflight.delete(id);
        reject(new Error("timeout"));
      }, QUERY_TIMEOUT_MS);
      this.inflight.set(id, { resolve, reject, timer });
      try {
        this.sock.send(JSON.stringify(frame));
      } catch {
        clearTimeout(timer);
        this.inflight.delete(id);
        reject(new Error("offline"));
      }
    });
  }

  // ---- connection ----

  _connect() {
    if (
      this.sock &&
      (this.sock.readyState === WebSocket.OPEN ||
        this.sock.readyState === WebSocket.CONNECTING)
    ) {
      return;
    }
    const proto = location.protocol === "https:" ? "wss" : "ws";
    let sock;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}`);
    } catch {
      this._scheduleReconnect();
      return;
    }
    this.sock = sock;

    sock.onopen = () => {
      this.opened = true;
      this.attempt = 0;
      clearTimeout(this.reconnectTimer);
      this._stopFallback();
      this._startPing();
      // The handshake snapshot re-hydrates everything, so a reconnect
      // needs no catch-up fetch of its own.
    };
    sock.onmessage = (e) => this._onMessage(e.data);
    sock.onclose = () => {
      this._stopPing();
      this.sock = null;
      this._failInflight("offline");
      this.store.setConnected(false);
      this._scheduleReconnect();
      this._startFallback();
    };
    sock.onerror = () => {
      // `onclose` runs next and owns the recovery; just surface the
      // disconnected state so the header stops claiming to be live.
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
      case "agents":
        this.store.applyAgents(msg.data);
        break;
      case "sandboxes":
        this.store.applySandboxes(msg.data);
        break;
      case "tokens":
        this.store.applyTokens(msg.data);
        break;
      case "schedules":
        this.store.applySchedules(msg.data);
        break;
      case "org":
        this.store.applyOrg(msg.data);
        break;
      case "tools":
        this.store.applyTools(msg.data);
        break;
      case "health":
        this.store.applyHealth(msg.data);
        break;
      case "result":
        this._settle(msg.id, null, msg.data);
        break;
      case "error":
        this._settle(msg.id, msg.error || "error", null);
        break;
      case "pong":
        break;
    }
  }

  _settle(id, error, data) {
    const entry = this.inflight.get(id);
    if (!entry) return;
    this.inflight.delete(id);
    clearTimeout(entry.timer);
    if (error) entry.reject(new Error(error));
    else entry.resolve(data);
  }

  _failInflight(reason) {
    for (const [, entry] of this.inflight) {
      clearTimeout(entry.timer);
      entry.reject(new Error(reason));
    }
    this.inflight.clear();
  }

  _scheduleReconnect() {
    if (this.closed) return;
    clearTimeout(this.reconnectTimer);
    const delay = Math.min(1000 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
    this.attempt++;
    this.reconnectTimer = setTimeout(() => this._connect(), delay);
  }

  _startPing() {
    this._stopPing();
    this.pingTimer = setInterval(() => {
      if (this.connected) {
        try {
          this.sock.send(JSON.stringify({ kind: "ping" }));
        } catch {
          /* the close handler owns recovery */
        }
      }
    }, PING_MS);
  }

  _stopPing() {
    clearInterval(this.pingTimer);
    this.pingTimer = 0;
  }

  // ---- degraded mode ----

  _startFallback() {
    if (this.fallbackTimer) return;
    this._fallbackFetch();
    this.fallbackTimer = setInterval(() => this._fallbackFetch(), FALLBACK_MS);
  }

  _stopFallback() {
    clearInterval(this.fallbackTimer);
    this.fallbackTimer = 0;
  }

  async _fallbackFetch() {
    const snap = await api.snapshot();
    if (snap && !snap._error) this.store.applySnapshot(snap);
  }
}
