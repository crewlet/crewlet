// The dashboard's single connection to the engine.
//
// State arrives as pushes (`snapshot`, then `agents` / `sandboxes` /
// `tokens` / `budget` / `events` / `org` / `tools` / `schedules` /
// `health`), and
// anything the dashboard needs on demand — an agent's LLM history, one
// event's payload, a trace, a different spend window, the configuration
// document — is asked for over the same socket and answered on it.
//
// There are no HTTP fetches in normal operation. The REST snapshot is
// used for exactly one thing: keeping the page honest while the socket
// is down (a proxy that refuses to upgrade, a restarting engine), and it
// stops the moment the socket is back.

import { api } from "./api.js";
import { apiToken } from "./authToken.js";

const PATH = "/ws/stream";
// `policy violation` — a refusal delivered as a close FRAME, which is
// only reachable once a connection has opened. The engine does not
// currently refuse anyone that late (it answers 401 to the handshake
// instead, see `_probeRefusal`), so nothing here fires today; it is
// honoured because a close code that means "your credential stopped
// being good" is the one a long-lived socket would use.
const CLOSE_UNAUTHORIZED = 1008;
// Reconnect backoff ceiling. Long enough that a dashboard left open
// against a stopped engine is not hammering it, short enough that
// bringing the engine back feels immediate.
const MAX_BACKOFF_MS = 30_000;
// Application-level keepalive. Comfortably inside the 60s idle timeout
// that most reverse proxies apply to an idle WebSocket.
const PING_MS = 25_000;
// Degraded-mode poll — only ever runs while the socket is down.
const FALLBACK_MS = 5_000;
// How long a query waits for its answer ONCE SENT. The slowest query is
// an agent's LLM history, a bounded scan over a 7-day window; ten
// seconds is far beyond its normal latency and still short enough that a
// view shows an error rather than an eternal skeleton. The clock starts
// when the frame goes out, not when the query is made, so time spent
// waiting for a socket is not counted against the server.
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
    // Whether the shell has already been asked to collect a token.
    this._askedForToken = false;
    this._onAuthRejected = null;
  }

  /** Operator bearer token, sent with config-family queries. */
  setToken(token) {
    this.token = token || "";
    // A supplied credential clears the ask-once latch. The latch is
    // there so a 30-second reconnect backoff cannot reopen the dialog
    // forever — not to make a SECOND refusal silent. Without this, a
    // reader who answered with a token the engine also rejects is never
    // asked again and sits on a page that never says why.
    if (this.token) this._askedForToken = false;
  }

  start() {
    this._connect();
  }

  /** Drop this socket and re-dial immediately.
   *
   * A dropped envelope is gone: `_fan_out` discards the OLDEST queued
   * frame, so a lost `agents` overlay is never re-sent and the only
   * true repair is a fresh handshake snapshot.
   */
  reconnect() {
    if (this.sock) this.sock.close();
    else this._connect();
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
   *
   * A query made before the socket is open, or while it is reconnecting,
   * WAITS for the connection rather than failing. That matters more than
   * it sounds: a view issues its first query from `mount`, which happens
   * as the page boots, so rejecting when not-yet-connected meant every
   * deep link and every reload of an agent or event page rendered "could
   * not load" and stayed there. Queries are pure reads, so one that was
   * in flight when the socket dropped is simply re-sent on reconnect.
   *
   * Rejects with an Error carrying the server's machine-readable code
   * (`not_found`, `unauthorized`, `no_event_store`, …), `timeout` if a
   * sent query goes unanswered, or `closed` if the client shuts down.
   */
  query(what, params = {}) {
    const id = this.nextQueryId++;
    return new Promise((resolve, reject) => {
      const entry = { id, what, params, resolve, reject, timer: 0 };
      this.inflight.set(id, entry);
      this._sendQuery(entry);
    });
  }

  _sendQuery(entry) {
    if (!this.connected) return; // `onopen` flushes it
    const frame = { kind: "query", id: entry.id, what: entry.what, params: entry.params };
    if (this.token) frame.token = this.token;
    try {
      this.sock.send(JSON.stringify(frame));
    } catch {
      return; // the close handler will re-send it
    }
    clearTimeout(entry.timer);
    entry.timer = setTimeout(() => {
      this.inflight.delete(entry.id);
      entry.reject(new Error("timeout"));
    }, QUERY_TIMEOUT_MS);
  }

  _flushQueries() {
    for (const entry of this.inflight.values()) this._sendQuery(entry);
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
    // A `WebSocket` constructor cannot carry an `Authorization` header,
    // so the token rides the query string — the one place this dashboard
    // sends it that way, and the server accepts either form. It is sent
    // on EVERY dial, not only after a rejection: an engine that guards
    // reads refuses the handshake outright, and a client that waits to be
    // told would spend a full backoff cycle disconnected on every load.
    const token = this.token || apiToken();
    const qs = token ? `?token=${encodeURIComponent(token)}` : "";
    let sock;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}${qs}`);
    } catch {
      this._scheduleReconnect();
      return;
    }
    this.sock = sock;
    // Per-dial, unlike `this.opened`, which latches for the life of the
    // page. The question on a close is whether THIS handshake completed:
    // a socket that opened and later dropped is an outage, and one that
    // never opened may be a refusal.
    let handshakeCompleted = false;

    sock.onopen = () => {
      this.opened = true;
      handshakeCompleted = true;
      this.attempt = 0;
      this.store.setAuthRejected(false);
      this.store.setConnected(true);
      clearTimeout(this.reconnectTimer);
      this._stopFallback();
      this._startPing();
      // The handshake snapshot re-hydrates everything, so a reconnect
      // needs no catch-up fetch of its own — but any query that was
      // waiting for this socket, or lost with the last one, does need
      // sending now.
      this._flushQueries();
    };
    sock.onmessage = (e) => this._onMessage(e.data);
    sock.onclose = (e) => {
      this._stopPing();
      this.sock = null;
      // Queries are NOT failed here: they are reads, and the reconnect
      // re-sends them. Their answer-timeout is stopped so the wait for a
      // new socket is not counted against the server.
      for (const entry of this.inflight.values()) {
        clearTimeout(entry.timer);
        entry.timer = 0;
      }
      this.store.setConnected(false);
      this._scheduleReconnect();
      this._startFallback();
      // Last, and deliberately: the shell's response to this is to ask
      // for a token and re-dial on an answer. Running it before the
      // teardown above would have that re-dial race the cleanup still
      // finishing around it.
      if (e && e.code === CLOSE_UNAUTHORIZED) this._authRejected();
      else if (!handshakeCompleted) this._probeRefusal();
    };
    sock.onerror = () => {
      // `onclose` runs next and owns the recovery; just surface the
      // disconnected state so the header stops claiming to be live.
      this.store.setConnected(false);
    };
  }

  /**
   * Ask, over plain HTTP, whether that dial was refused or merely failed.
   *
   * A handshake the engine answers 401 NEVER reaches this page as
   * close(1008). A close code travels in a close frame, and a connection
   * that never opened has no frames — so the browser reports 1006, the
   * same code it gives for an engine that is simply down, and withholds
   * the status deliberately (a page that could read it could use a
   * socket to scan ports it cannot otherwise reach).
   *
   * This client believed otherwise, and the whole repair path below —
   * the banner, the dialog, "forget this token" — hung off a code that
   * never arrived. A wrong token in `localStorage` therefore produced a
   * dashboard that reconnected for ever, said "retrying", and offered no
   * way to correct the one thing that was wrong.
   *
   * So the status is fetched where a browser will hand it over. A plain
   * GET of the same path runs the same guard and stops one line short of
   * the upgrade: 401 is a refused credential, 426 (Upgrade Required)
   * means it was accepted and only the missing header stopped it. A
   * throw is the network, which is not an auth problem and must not
   * raise a dialog.
   *
   * The credential goes in the HEADER here, not the query string the
   * handshake is forced to use: a fetch can set one, and a token in a
   * URL is a token in every proxy's access log.
   */
  async _probeRefusal() {
    if (this.closed) return;
    const token = this.token || apiToken();
    try {
      const res = await fetch(PATH, {
        headers: token ? { Authorization: "Bearer " + token } : {},
        cache: "no-store",
      });
      // Not `!res.ok`: 426 is the healthy answer here, and every other
      // failure is the network or a proxy, neither of which the reader
      // fixes by typing a token.
      if (res.status === 401) this._authRejected();
    } catch {
      // Offline, or a proxy that refuses the request outright. The
      // reconnect loop already covers it.
    }
  }

  /**
   * The engine refused this browser's credential.
   *
   * Two things happen, and both are needed. Asking for a token is the
   * repair — the dashboard is served unauthenticated by design (the
   * page that asks for a token cannot itself require one), so the
   * browser has no other moment to learn it needs one. The store flag is
   * what happens when the reader dismisses that request: it fires once
   * and only once, deliberately, so that a 30-second reconnect backoff
   * does not reopen a dialog forever — which leaves the page looking
   * like an outage unless the chrome can say otherwise.
   *
   * The socket does not own the asking. It cannot: the dialog belongs to
   * the shell, and a transport that reaches up into the DOM to draw one
   * is a transport that cannot be tested without a browser.
   */
  _authRejected() {
    this.store.setAuthRejected(true);
    if (this._askedForToken || !this._onAuthRejected) return;
    this._askedForToken = true;
    this._onAuthRejected();
  }

  /**
   * Register what to do the first time the engine refuses a credential.
   *
   * Called once by the shell at boot.
   */
  onAuthRejected(fn) {
    this._onAuthRejected = fn;
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
      case "seats":
        this.store.applySeats(msg.data);
        break;
      case "sandboxes":
        this.store.applySandboxes(msg.data);
        break;
      case "tokens":
        this.store.applyTokens(msg.data);
        break;
      case "budget":
        this.store.applyBudget(msg.data);
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
    for (const entry of this.inflight.values()) {
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
    // The `closed` guard is not decoration. `stop()` clears the timers
    // and THEN closes the socket, whose own close handler comes back
    // here — so without it a stopped client left a 5-second fetch loop
    // hammering the engine for the life of the tab, with no socket and
    // nothing left to render it into. `_scheduleReconnect` has always
    // had the same guard; this one was simply missing.
    if (this.closed || this.fallbackTimer) return;
    this._fallbackFetch();
    this.fallbackTimer = setInterval(() => this._fallbackFetch(), FALLBACK_MS);
  }

  _stopFallback() {
    clearInterval(this.fallbackTimer);
    this.fallbackTimer = 0;
  }

  async _fallbackFetch() {
    const snap = await api.snapshot();
    // A fetch started while the socket was down can land after it came
    // back, by which time the handshake snapshot and any pushes since
    // are fresher than this one. Degraded mode must not overwrite live
    // state with a reading it took before the connection recovered.
    if (this.connected) return;
    if (snap) this.store.applySnapshot(snap);
  }
}
