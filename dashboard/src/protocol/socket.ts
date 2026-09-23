/**
 * The dashboard's single connection to the engine.
 *
 * State arrives as pushes (`snapshot`, then `agents` / `seats` / `sandboxes` /
 * `tokens` / `budget` / `event` / `org` / `tools` / `schedules` / `health`), and
 * anything the dashboard needs on demand — a seat's phase history, one event's
 * payload, a trace, a different spend window, the configuration document — is
 * asked for over the same socket and answered on it.
 *
 * One push is addressed to a SEAT rather than to every tab: `inbox_changed`,
 * sent only to a socket that asked to `watch` that seat and was allowed to. It
 * is how a person learns they have work without waiting for a poll.
 *
 * There are no HTTP fetches in normal operation. The REST snapshot is used for
 * exactly one thing: keeping the page honest while the socket is down (a proxy
 * that refuses to upgrade, a restarting engine), and it stops the moment the
 * socket is back.
 */

import { api } from "./api.ts";
import { apiToken } from "./authToken.ts";
import type { Store } from "./store.ts";
import type { Frame, QueryErrorCode, QueryMap, QueryName } from "./types.ts";

const PATH = "/ws/stream";

/**
 * The credential this socket was opened with names nobody any more — the
 * session ended, expired or was revoked. The engine re-checks an open socket
 * every minute and closes it with this when that happens. NOT a refusal on its
 * own: the browser may hold a newer cookie than the one this socket was opened
 * with, so the ordinary reconnect is the repair, and only a handshake that is
 * then refused (see `probeRefusal`) asks the reader for anything.
 */
const CLOSE_UNAUTHENTICATED = 4401;

/**
 * The credential still names somebody who may not have this surface: their
 * seat is gone from the chart, or the grant the socket needs was withdrawn.
 * Reconnecting reaches the same person with the same access, so the socket
 * STOPS and the page says why.
 */
const CLOSE_FORBIDDEN = 4403;

/**
 * Reconnect backoff ceiling. Long enough that a dashboard left open against a
 * stopped engine is not hammering it, short enough that bringing the engine
 * back feels immediate.
 */
const MAX_BACKOFF_MS = 30_000;

/** Application-level keepalive, comfortably inside the 60 s idle timeout most reverse proxies apply. */
const PING_MS = 25_000;

/** Degraded-mode poll — only ever runs while the socket is down. */
const FALLBACK_MS = 5_000;

/**
 * How long a query waits for its answer ONCE SENT.
 *
 * The clock starts when the frame goes out, not when the query is made, so time
 * spent waiting for a socket is not counted against the server. Ten seconds is
 * far beyond the slowest query's normal latency and still short enough that a
 * screen shows an error rather than an eternal skeleton.
 */
const QUERY_TIMEOUT_MS = 10_000;

/**
 * How soon something the engine answered `unavailable` is asked again — a
 * query (see `useQuery`) or a watch.
 *
 * `unavailable` is the engine saying "ask me in a moment": its projection is
 * catching up, its coordination store did not answer, or its chart view is
 * behind. The banner for it tells a person the screen fills in on its own, and
 * a request with nothing behind it never asked again, so a screen opened during
 * a restart held that banner until somebody reloaded. Five seconds is the
 * engine's own Retry-After when it has no better hint, which is its shared
 * health tick (`stream.HealthInterval`): sooner asks before anything could
 * have changed, later leaves a recovered node looking broken.
 *
 * ONE DECLARATION for both, because the engine's answer is one: a query and a
 * watch refused `unavailable` by the same node are waiting on the same thing.
 */
export const UNAVAILABLE_RETRY_MS = 5_000;

/**
 * What a watch refusal's error frame names in `what` — the engine's
 * `watchWhat`. A watch carries no query id, so this is how an error frame about
 * one is told from a query's.
 */
const WATCH_WHAT = "watch";

/**
 * Every {@link QueryErrorCode}, as a value a rejection's message can be tested
 * against.
 *
 * A Record over the union rather than a list beside it: the compiler refuses a
 * key the union lacks and a member left out, so the two cannot drift. The
 * union itself is pinned to the engine's own codes by a Go test in
 * `internal/api/stream` that reads it.
 */
const QUERY_ERROR_CODES: Record<QueryErrorCode, true> = {
  unknown_query: true,
  unauthorized: true,
  query_failed: true,
  bad_params: true,
  not_found: true,
  unavailable: true,
  timeout: true,
  closed: true,
};

/**
 * The query error code `value` is, or null for anything else: no failure at
 * all, or prose a screen wrote itself.
 *
 * Branching on the narrowed value is what keeps a screen's handling inside the
 * vocabulary. A comparison against a code the union lacks, such as the
 * `no_event_store` the engine never sent, is then a type error rather than a
 * branch that can never run. `Object.hasOwn`, because `in` would also accept
 * `toString` and every other name an object inherits.
 */
export function queryErrorCode(value: string | null | undefined): QueryErrorCode | null {
  return value && Object.hasOwn(QUERY_ERROR_CODES, value) ? (value as QueryErrorCode) : null;
}

interface Inflight {
  id: number;
  what: string;
  params: Record<string, unknown>;
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
  timer: ReturnType<typeof setTimeout> | 0;
}

export class LiveSocket {
  private store: Store;
  private sock: WebSocket | null = null;
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | 0 = 0;
  private pingTimer: ReturnType<typeof setInterval> | 0 = 0;
  private fallbackTimer: ReturnType<typeof setInterval> | 0 = 0;
  private isClosed = false;
  /**
   * Whether the engine refused this browser the surface (see `accessRefused`).
   * A latch rather than a cancelled timer, because the refusal can arrive from
   * an async probe while a reconnect is already in flight, whose own close
   * would otherwise schedule the next one.
   */
  private refused = false;
  private nextQueryId = 1;
  private inflight = new Map<number, Inflight>();
  /**
   * The seat this tab watches for `inbox_changed` frames, or "". Held HERE
   * rather than only sent, because a watch lives in the engine's routing index
   * for ONE socket: every new socket starts watching nothing, so the tab has to
   * say it again on every open.
   */
  private watched = "";
  private watchRetry: ReturnType<typeof setTimeout> | 0 = 0;
  private token = "";
  /** Whether the shell has already been asked to collect a token. */
  private askedForToken = false;
  private authRejectedHandler: (() => void) | null = null;

  constructor(store: Store) {
    this.store = store;
  }

  /** Operator bearer token, sent on the handshake and with every query frame. */
  setToken(token: string): void {
    this.token = token || "";
    // A supplied credential clears the ask-once latch. The latch exists so a
    // 30-second reconnect backoff cannot reopen the dialog forever — not to
    // make a SECOND refusal silent. Without this, a reader who answered with a
    // token the engine also rejects is never asked again and sits on a page
    // that never says why.
    if (this.token) this.askedForToken = false;
  }

  start(): void {
    this.connect();
  }

  /**
   * Drop this socket and re-dial immediately.
   *
   * A dropped envelope is gone: the server's per-client queue discards the
   * OLDEST frame under backpressure, so a lost `agents` overlay is never
   * re-sent and the only true repair is a fresh handshake snapshot.
   */
  reconnect(): void {
    this.refused = false;
    if (this.sock) this.sock.close();
    else this.connect();
  }

  stop(): void {
    this.isClosed = true;
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.watchRetry);
    this.stopPing();
    this.stopFallback();
    this.failInflight("closed");
    if (this.sock) this.sock.close();
  }

  get connected(): boolean {
    return !!this.sock && this.sock.readyState === WebSocket.OPEN;
  }

  /**
   * Ask the server for something and resolve with its reply.
   *
   * A query made before the socket is open, or while it is reconnecting,
   * **waits** for the connection rather than failing. That matters more than it
   * sounds: a screen issues its first query as the page boots, so rejecting
   * when not-yet-connected meant every deep link and every reload rendered
   * "could not load" and stayed there. Queries are pure reads, so one that was
   * in flight when the socket dropped is simply re-sent on reconnect.
   *
   * Rejects with an Error carrying the server's machine-readable code
   * (`not_found`, `unauthorized`, `unavailable`, …), `timeout` if a sent
   * query goes unanswered, or `closed` if the client shuts down.
   */
  query<K extends QueryName>(what: K, params?: Record<string, unknown>): Promise<QueryMap[K]> {
    const id = this.nextQueryId++;
    return new Promise<QueryMap[K]>((resolve, reject) => {
      const entry: Inflight = {
        id,
        what,
        params: params ?? {},
        resolve: resolve as (value: unknown) => void,
        reject,
        timer: 0,
      };
      this.inflight.set(id, entry);
      this.sendQuery(entry);
    });
  }

  private sendQuery(entry: Inflight): void {
    if (!this.connected || !this.sock) return; // `onopen` flushes it
    const frame: Record<string, unknown> = {
      kind: "query",
      id: entry.id,
      what: entry.what,
      params: entry.params,
    };
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

  private flushQueries(): void {
    for (const entry of this.inflight.values()) this.sendQuery(entry);
  }

  /**
   * Ask for one seat's `inbox_changed` frames on this socket and every socket
   * after it, or stop with "".
   *
   * ONE SEAT, because the engine keeps one per socket: a watch is a screen
   * saying where it now is, so a second call replaces the first rather than
   * adding to it. The engine decides a watch the way it decides the inbox
   * question about the same seat — its holder, whoever leads it, or the admin
   * grant — and answers a refusal on the socket rather than closing it; see
   * `watchAnswered`.
   */
  watch(seat: string): void {
    const next = seat.trim();
    clearTimeout(this.watchRetry);
    this.watchRetry = 0;
    // A CLEAR IS SENT even to a socket that watched nothing, because the one
    // it is sent on may be watching what this tab asked for before.
    const clearing = next === "" && this.watched !== "";
    this.watched = next;
    if (next !== "" || clearing) this.sendWatch();
  }

  private sendWatch(): void {
    if (!this.connected || !this.sock) return; // `onopen` sends it
    try {
      this.sock.send(JSON.stringify({ kind: "watch", seat: this.watched }));
    } catch {
      // The close handler owns recovery, and the next open re-sends it.
    }
  }

  /**
   * The engine refused a watch — the one it was just sent, or the one it
   * re-decided when it re-checked this socket's credential.
   *
   * `unavailable` means this node could not read the chart that decides it,
   * which clears on its own, so it is asked again after the engine's own
   * retry hint. ANY OTHER CODE IS A DECISION, and asking again would only be
   * refused again: the screen's poll carries on as it did before there was a
   * push at all, and the next socket asks once more in case the answer moved.
   */
  private watchAnswered(code: string | undefined): void {
    if (code !== "unavailable" || this.watched === "") return;
    clearTimeout(this.watchRetry);
    this.watchRetry = setTimeout(() => {
      this.watchRetry = 0;
      this.sendWatch();
    }, UNAVAILABLE_RETRY_MS);
  }

  // ---- connection --------------------------------------------------------

  private connect(): void {
    if (
      this.sock &&
      (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING)
    ) {
      return;
    }
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // A `WebSocket` constructor cannot carry an `Authorization` header, so the
    // token rides the query string — the one place this dashboard sends it that
    // way, and the server accepts either form. It is sent on EVERY dial, not
    // only after a rejection: an engine that guards reads refuses the handshake
    // outright, and a client that waited to be told would spend a full backoff
    // cycle disconnected on every load.
    const token = this.token || apiToken();
    const qs = token ? `?token=${encodeURIComponent(token)}` : "";
    let sock: WebSocket;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}${qs}`);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.sock = sock;
    // Per-dial, unlike a latch for the life of the page. The question on a
    // close is whether THIS handshake completed: a socket that opened and later
    // dropped is an outage, and one that never opened may be a refusal.
    let handshakeCompleted = false;

    sock.onopen = () => {
      handshakeCompleted = true;
      this.attempt = 0;
      this.store.setAuthRejected(false);
      this.store.setAccessRefused(null);
      this.store.setConnected(true);
      clearTimeout(this.reconnectTimer);
      this.stopFallback();
      this.startPing();
      // The handshake snapshot re-hydrates everything, so a reconnect needs no
      // catch-up fetch of its own — but any query that was waiting for this
      // socket, or lost with the last one, does need sending now.
      this.flushQueries();
      // AND THE WATCH, which the engine held for the last socket only.
      if (this.watched !== "") this.sendWatch();
    };
    sock.onmessage = (e: MessageEvent) => this.onMessage(String(e.data));
    sock.onclose = (e: CloseEvent) => {
      this.stopPing();
      this.sock = null;
      // A pending watch retry was for the socket that just went; the next
      // open sends the watch anyway.
      clearTimeout(this.watchRetry);
      this.watchRetry = 0;
      // Queries are NOT failed here: they are reads, and the reconnect re-sends
      // them. Their answer-timeout is stopped so the wait for a new socket is
      // not counted against the server.
      for (const entry of this.inflight.values()) {
        clearTimeout(entry.timer);
        entry.timer = 0;
      }
      this.store.setConnected(false);
      if (e && e.code === CLOSE_FORBIDDEN) {
        // No reconnect and no REST fallback: both would be answered 403 by
        // the same decision, for as long as the tab stayed open.
        this.accessRefused(e.reason);
        return;
      }
      this.scheduleReconnect();
      this.startFallback();
      // A 4401 needs nothing beyond the reconnect above — see
      // CLOSE_UNAUTHENTICATED. A dial that never opened may be a refusal, and
      // only a plain HTTP re-ask can say which (see `probeRefusal`).
      if (!handshakeCompleted && (!e || e.code !== CLOSE_UNAUTHENTICATED)) {
        void this.probeRefusal();
      }
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
   * A handshake the engine answers 401 NEVER reaches this page as close(1008).
   * A close code travels in a close frame, and a connection that never opened
   * has no frames — so the browser reports 1006, the same code it gives for an
   * engine that is simply down, and withholds the status deliberately (a page
   * that could read it could use a socket to scan ports it cannot otherwise
   * reach).
   *
   * This client believed otherwise once, and the whole repair path — the
   * banner, the dialog, "forget this token" — hung off a code that never
   * arrived. A wrong token in `localStorage` therefore produced a dashboard
   * that reconnected for ever, said "retrying", and offered no way to correct
   * the one thing that was wrong.
   *
   * So the status is fetched where a browser will hand it over. A plain GET of
   * the same path runs the same guard and stops one line short of the upgrade:
   * 401 is a refused credential, 426 (Upgrade Required) means it was accepted
   * and only the missing header stopped it. A throw is the network, which is
   * not an auth problem and must not raise a dialog.
   *
   * The credential goes in the HEADER here, not the query string the handshake
   * is forced to use: a fetch can set one, and a token in a URL is a token in
   * every proxy's access log.
   */
  private async probeRefusal(): Promise<void> {
    if (this.isClosed) return;
    const token = this.token || apiToken();
    try {
      const res = await fetch(PATH, {
        headers: token ? { Authorization: "Bearer " + token } : {},
        cache: "no-store",
      });
      // Not `!res.ok`: 426 is the healthy answer here, and every other failure
      // is the network or a proxy, neither of which the reader fixes by typing
      // a token.
      if (res.status === 401) this.authRejected();
      else if (res.status === 403) {
        const body = (await res.json().catch(() => null)) as { detail?: string } | null;
        this.accessRefused(body?.detail ?? "");
      }
    } catch {
      // Offline, or a proxy that refuses the request outright. The reconnect
      // loop already covers it.
    }
  }

  /**
   * The engine refused this browser's credential.
   *
   * Two things happen, and both are needed. Asking for a token is the repair —
   * the dashboard is served unauthenticated by design (the page that asks for a
   * token cannot itself require one), so the browser has no other moment to
   * learn it needs one. The store flag is what happens when the reader
   * dismisses that request: the ask fires once and only once, deliberately, so
   * a 30-second reconnect backoff does not reopen a dialog forever — which
   * leaves the page looking like an outage unless the chrome can say otherwise.
   *
   * The socket does not own the asking. It cannot: the dialog belongs to the
   * shell, and a transport that reaches into the DOM to draw one is a transport
   * that cannot be tested without a browser.
   */
  /**
   * The engine knows who this browser is and will not serve it this surface.
   *
   * The reconnect loop and the REST fallback both STOP: each would be refused
   * by the same decision every thirty seconds for as long as the tab stayed
   * open, and a page that says "reconnecting" to somebody whose access was
   * withdrawn is telling them to wait for something that will not happen.
   * `reconnect()` is the way back, once an administrator has restored it.
   */
  private accessRefused(reason: string): void {
    this.refused = true;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.stopFallback();
    this.store.setAccessRefused(reason);
  }

  private authRejected(): void {
    this.store.setAuthRejected(true);
    if (this.askedForToken || !this.authRejectedHandler) return;
    this.askedForToken = true;
    this.authRejectedHandler();
  }

  /** Register what to do the first time the engine refuses a credential. */
  onAuthRejected(fn: () => void): void {
    this.authRejectedHandler = fn;
  }

  /**
   * The dispatch table.
   *
   * Public because the e2e replay reaches it directly: `internal/e2e` captures
   * the frames a real company's socket produced and pushes them through THIS
   * function, so the gate asks "does the client understand what the server
   * sent" rather than "did the server send something". Re-implementing the
   * switch in the replay would let it agree with a server the dashboard does
   * not.
   */
  onMessage(raw: string): void {
    let msg: Frame;
    try {
      msg = JSON.parse(raw) as Frame;
    } catch {
      return;
    }
    switch (msg.kind) {
      case "snapshot":
        this.store.applySnapshot(msg.data as never);
        break;
      case "event":
        this.store.applyEvent(msg.data as never);
        break;
      case "agents":
        this.store.applyAgents(msg.data);
        break;
      case "seats":
        this.store.applySeats(msg.data);
        break;
      case "sandboxes":
        this.store.applySandboxes(msg.data as never);
        break;
      case "tokens":
        this.store.applyTokens(msg.data as never);
        break;
      case "budget":
        this.store.applyBudget(msg.data as never);
        break;
      case "schedules":
        this.store.applySchedules(msg.data as never);
        break;
      case "org":
        this.store.applyOrg(msg.data as never);
        break;
      case "tools":
        this.store.applyTools(msg.data as never);
        break;
      case "health":
        this.store.applyHealth(msg.data as never);
        break;
      case "inbox_changed":
        this.store.applyInboxChanged(msg.data as never);
        break;
      case "identity":
        this.store.applyIdentity(msg.data as never);
        break;
      case "result":
        this.settle(msg.id, null, msg.data);
        break;
      case "error":
        // A WATCH'S REFUSAL carries no query id, only what it is about.
        if (msg.what === WATCH_WHAT && msg.id === undefined) {
          this.watchAnswered(msg.error);
          break;
        }
        // An error frame always carries a code. One that does not is still
        // a failure nobody explained, which is what `query_failed` means.
        this.settle(msg.id, msg.error || "query_failed", null);
        break;
      case "pong":
        break;
    }
  }

  private settle(id: number | undefined, error: string | null, data: unknown): void {
    if (id === undefined) return;
    const entry = this.inflight.get(id);
    if (!entry) return;
    this.inflight.delete(id);
    clearTimeout(entry.timer);
    if (error) entry.reject(new Error(error));
    else entry.resolve(data);
  }

  private failInflight(reason: string): void {
    for (const entry of this.inflight.values()) {
      clearTimeout(entry.timer);
      entry.reject(new Error(reason));
    }
    this.inflight.clear();
  }

  private scheduleReconnect(): void {
    if (this.isClosed || this.refused) return;
    clearTimeout(this.reconnectTimer);
    const delay = Math.min(1000 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
    this.attempt++;
    this.reconnectTimer = setTimeout(() => this.connect(), delay);
  }

  private startPing(): void {
    this.stopPing();
    this.pingTimer = setInterval(() => {
      if (this.connected && this.sock) {
        try {
          this.sock.send(JSON.stringify({ kind: "ping" }));
        } catch {
          /* the close handler owns recovery */
        }
      }
    }, PING_MS);
  }

  private stopPing(): void {
    clearInterval(this.pingTimer);
    this.pingTimer = 0;
  }

  // ---- degraded mode -----------------------------------------------------

  private startFallback(): void {
    // The `isClosed` guard is not decoration. `stop()` clears the timers and
    // THEN closes the socket, whose own close handler comes back here — so
    // without it a stopped client left a 5-second fetch loop hammering the
    // engine for the life of the tab, with no socket and nothing to render
    // into.
    if (this.isClosed || this.refused || this.fallbackTimer) return;
    void this.fallbackFetch();
    this.fallbackTimer = setInterval(() => void this.fallbackFetch(), FALLBACK_MS);
  }

  private stopFallback(): void {
    clearInterval(this.fallbackTimer);
    this.fallbackTimer = 0;
  }

  private async fallbackFetch(): Promise<void> {
    const snap = await api.snapshot();
    // A fetch started while the socket was down can land after it came back, by
    // which time the handshake snapshot and any pushes since are fresher than
    // this one. Degraded mode must not overwrite live state with a reading it
    // took before the connection recovered.
    if (this.connected) return;
    if (snap) this.store.applySnapshot(snap);
  }
}
