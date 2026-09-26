/**
 * The dashboard's single connection to the engine.
 *
 * State arrives as pushes (`snapshot`, then `agents` / `seats` / `sandboxes` /
 * `tokens` / `budget` / `event` / `org` / `tools` / `schedules` / `health`), and
 * anything the dashboard needs on demand — a seat's phase history, one event's
 * payload, a trace, a different spend window, the configuration document — is
 * asked for over the same socket and answered on it.
 *
 * The socket is the channel for the projection and for every question the
 * query registry answers — not for everything. Writes and the guarded reads no
 * query answers (`/secrets`, `/setup`, `/config`) go over REST through
 * `rest.ts`, and this file makes two HTTP requests of its own: the degraded
 * snapshot, which keeps the page honest while the socket is down (a proxy that
 * refuses to upgrade, a restarting engine) and stops the moment the socket is
 * back, and the refusal probe after a handshake that never opened.
 */

import { api } from "./api.ts";
import { apiToken } from "./authToken.ts";
import type { Store } from "./store.ts";
import type { Frame, QueryMap, QueryName } from "./types.ts";
import type { QueryErrorCode } from "../contract/errors.ts";

const PATH = "/ws/stream";

/**
 * `policy violation` — a refusal delivered as a close FRAME, which is only
 * reachable once a connection has opened. The engine does not currently refuse
 * anyone that late (it answers 401 to the handshake instead, see
 * `probeRefusal`), so nothing here fires today; it is honoured because a close
 * code meaning "your credential stopped being good" is the one a long-lived
 * socket would use.
 */
const CLOSE_UNAUTHORIZED = 1008;

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

/**
 * A query the engine refused, with the wait it asked for.
 *
 * AN ERROR WHOSE MESSAGE IS THE CODE, so every caller that reads a refusal by
 * [queryErrorCode] of its message keeps working, and a TYPE beside it because
 * the wait is a number and the text of an error is no place to carry one: the
 * `unavailable` frame carries `retry_after_seconds`, computed by the same
 * engine helper as the REST 503's `Retry-After`, and a transport that dropped
 * it left each screen to guess a wait of its own.
 */
export class QueryError extends Error {
  /**
   * The engine's wait in seconds, on an `unavailable` refusal that named one,
   * and null everywhere else — the socket's own `timeout` and `closed`
   * included, which no engine said anything about.
   */
  readonly retryAfterSeconds: number | null;

  constructor(code: string, retryAfterSeconds: number | null = null) {
    super(code);
    this.name = "QueryError";
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

/** The engine's wait on a refusal, or null where it named none. */
function retryHint(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : null;
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
  private nextQueryId = 1;
  private inflight = new Map<number, Inflight>();
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
    if (this.sock) this.sock.close();
    else this.connect();
  }

  stop(): void {
    this.isClosed = true;
    clearTimeout(this.reconnectTimer);
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
      entry.reject(new QueryError("timeout"));
    }, QUERY_TIMEOUT_MS);
  }

  private flushQueries(): void {
    for (const entry of this.inflight.values()) this.sendQuery(entry);
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
      this.store.setConnected(true);
      clearTimeout(this.reconnectTimer);
      this.stopFallback();
      this.startPing();
      // The handshake snapshot re-hydrates everything, so a reconnect needs no
      // catch-up fetch of its own — but any query that was waiting for this
      // socket, or lost with the last one, does need sending now.
      this.flushQueries();
    };
    sock.onmessage = (e: MessageEvent) => this.onMessage(String(e.data));
    sock.onclose = (e: CloseEvent) => {
      this.stopPing();
      this.sock = null;
      // Queries are NOT failed here: they are reads, and the reconnect re-sends
      // them. Their answer-timeout is stopped so the wait for a new socket is
      // not counted against the server.
      for (const entry of this.inflight.values()) {
        clearTimeout(entry.timer);
        entry.timer = 0;
      }
      this.store.setConnected(false);
      this.scheduleReconnect();
      this.startFallback();
      // Last, and deliberately: the shell's response to this is to ask for a
      // token and re-dial on an answer. Running it before the teardown above
      // would have that re-dial race the cleanup still finishing around it.
      if (e && e.code === CLOSE_UNAUTHORIZED) this.authRejected();
      else if (!handshakeCompleted) void this.probeRefusal();
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
      case "result":
        this.settle(msg.id, null, msg.data);
        break;
      case "error":
        // An error frame always carries a code. One that does not is still
        // a failure nobody explained, which is what `query_failed` means.
        this.settle(msg.id, msg.error || "query_failed", null, retryHint(msg.retry_after_seconds));
        break;
      case "pong":
        break;
      default:
        // A KIND THIS BUILD DOES NOT KNOW IS A NEWER PEER'S, and it is
        // ignored rather than thrown on: a fleet mid-upgrade has a node
        // pushing what this bundle was built before. It is COUNTED rather
        // than dropped silently, because the same fall-through is also what
        // this build's own engine sending a kind its own client forgot looks
        // like — and the e2e replay asserts that count is zero.
        this.store.noteUnknownPush((msg as { kind?: unknown }).kind);
        break;
    }
  }

  private settle(
    id: number | undefined,
    error: string | null,
    data: unknown,
    retryAfterSeconds: number | null = null,
  ): void {
    if (id === undefined) return;
    const entry = this.inflight.get(id);
    if (!entry) return;
    this.inflight.delete(id);
    clearTimeout(entry.timer);
    if (error) entry.reject(new QueryError(error, retryAfterSeconds));
    else entry.resolve(data);
  }

  private failInflight(reason: string): void {
    for (const entry of this.inflight.values()) {
      clearTimeout(entry.timer);
      entry.reject(new QueryError(reason));
    }
    this.inflight.clear();
  }

  private scheduleReconnect(): void {
    if (this.isClosed) return;
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
    if (this.isClosed || this.fallbackTimer) return;
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
