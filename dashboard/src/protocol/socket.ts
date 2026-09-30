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
import { retryAfterMs, UNAVAILABLE_RETRY_MS } from "./retry.ts";
import { needSession } from "./session.ts";
import type { Store } from "./store.ts";
import type {
  Frame,
  LogRefusal,
  QueryErrorCode,
  QueryMap,
  QueryName,
  QueryRefusal,
} from "./types.ts";

/**
 * A rejected question, carrying — when the engine refused it on AUTHORITY —
 * the reason and the grants its error frame named, or — when it answered
 * `unavailable` — the state log's refusal behind that and whether asking again
 * can change it ({@link LogRefusal}).
 *
 * `message` IS STILL THE CODE, which every existing reader tests with
 * {@link queryErrorCode}; the refusal rides beside it rather than replacing it,
 * so a screen that only branches on the code is unchanged and one that can say
 * what would admit the reader has it to say.
 */
export class QueryRefusedError extends Error {
  constructor(
    code: string,
    readonly refusal: QueryRefusal | LogRefusal | null,
  ) {
    super(code);
    this.name = "QueryRefusedError";
  }
}

/** What a failed `query` said: its code, and the refusal behind it, if any. */
export interface QueryFailure {
  error: string;
  refusal: QueryRefusal | LogRefusal | null;
}

/**
 * A failed `query` as the pair `QueryState` renders from.
 *
 * ONE READING of a rejection, for every surface that asks outside `useQuery` —
 * a page of older rows, the shared health read. Read inline at each, the
 * refusal was the half a surface forgot: its screen said a read was refused
 * and not which grant would have admitted the reader, although the answer had
 * named it.
 */
export function queryFailure(err: unknown): QueryFailure {
  return {
    error: err instanceof Error ? err.message : "query_failed",
    refusal: err instanceof QueryRefusedError ? err.refusal : null,
  };
}

/**
 * Whether a refusal is the state log's, carried by an `unavailable` answer,
 * rather than one on authority. The two ride the same field of an answer
 * because a screen hands both to `QueryState` the same way.
 */
export function isLogRefusal(refusal: QueryRefusal | LogRefusal): refusal is LogRefusal {
  return "retryAfter" in refusal;
}

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

/**
 * Degraded-mode poll — only ever runs while the socket is down. A `503` the
 * engine wrote replaces its next tick with the answer's own `Retry-After`
 * (see `fallbackFetch`).
 */
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
 * query (see `useQuery`), the shared health read, or a watch — in
 * milliseconds, or `null` for "not on a timer".
 *
 * `unavailable` is the engine saying it cannot answer HERE, and its frame says
 * when that may change: `retry_after`, read through {@link retryAfterMs} —
 * waited out, bounded, and ZERO meaning waiting will not change it, so nothing
 * re-asks. An answer carrying no hint waits {@link UNAVAILABLE_RETRY_MS}, what
 * the engine says when it has nothing better; so does a refusal that is not
 * the state log's, which no `unavailable` answer carries.
 *
 * ONE READING for all three, because the engine's answer is one: a query, the
 * health read and a watch refused `unavailable` by the same node are waiting
 * on the same thing. The fixed five seconds every one of them re-asked at
 * whatever the frame said is what this replaced.
 */
export function unavailableRetryMs(refusal: QueryRefusal | LogRefusal | null): number | null {
  if (refusal === null || !isLogRefusal(refusal)) return UNAVAILABLE_RETRY_MS;
  return retryAfterMs(refusal.retryAfter);
}

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
 * The refusal an error frame carries, or null — for a frame that is neither a
 * refusal on authority nor an `unavailable` answer, or a node too old to say
 * why.
 */
function refusalOf(msg: Frame): QueryRefusal | LogRefusal | null {
  // AN `unavailable` ANSWER'S REFUSAL AND HINT, where the engine sent them:
  // the state log's code and words, and whether — and when — asking this node
  // again can change the answer. A frame with no `retry_after` is a node too
  // old to say, which reads as no hint at all (see `unavailableRetryMs`); so
  // does a negative one, which is not a number of seconds the engine writes,
  // exactly as a `Retry-After` that is not whole seconds is none to
  // `rest.ts`.
  if (msg.error === "unavailable") {
    if (typeof msg.retry_after !== "number" || !(msg.retry_after >= 0)) return null;
    return {
      code: typeof msg.refusal === "string" ? msg.refusal : null,
      detail: typeof msg.detail === "string" ? msg.detail : null,
      retryAfter: msg.retry_after,
    };
  }
  if (msg.error !== "unauthorized" || typeof msg.reason !== "string") return null;
  return {
    reason: msg.reason,
    grants: Array.isArray(msg.grants) ? msg.grants.filter((g) => typeof g === "string") : [],
  };
}

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
  private fallbackTimer: ReturnType<typeof setTimeout> | 0 = 0;
  /**
   * The degraded-mode poll now running, or 0 for none. A NUMBER PER RUN rather
   * than a flag, because a read can be in flight when its run is stopped, and
   * one that landed after a new run began would schedule a second chain of
   * reads beside the new one's.
   */
  private fallbackRun = 0;
  private fallbackRuns = 0;
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

  constructor(store: Store) {
    this.store = store;
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
    // A RE-DIAL IS A NEW ATTEMPT, usually with a credential the browser did
    // not hold at the last one — a sign-in's cookie, a step-up's replacement
    // — so the last refusal no longer describes it. Left standing, the page a
    // sign-in lands on would open under a banner saying this browser was
    // refused, until the handshake it had just started answered.
    this.store.setAuthRejected(false);
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
    // NO CREDENTIAL IN THE FRAME. The engine asks every question as the
    // principal the handshake resolved and reads no per-frame token, so one
    // here was the reader's bearer copied into every frame for nothing.
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
   * or the directory a login resolves through, so it is asked again when the
   * frame's `retry_after` says that may have changed ({@link
   * unavailableRetryMs}) — which this used to claim and did not do: it re-asked
   * at a fixed five seconds whatever the frame said. A ZERO is a read no wait
   * clears, and ANY OTHER CODE IS A DECISION; either way asking again on a
   * timer would only be answered the same: the screen's poll carries on as it
   * did before there was a push at all, and the next socket asks once more in
   * case the answer moved.
   */
  private watchAnswered(msg: Frame): void {
    // ANY ANSWER SUPERSEDES A RETRY an earlier one scheduled: a refusal that
    // lands while it is pending is a decision, and the retry would only be
    // refused the same.
    clearTimeout(this.watchRetry);
    this.watchRetry = 0;
    if (msg.error !== "unavailable" || this.watched === "") return;
    const wait = unavailableRetryMs(refusalOf(msg));
    if (wait === null) return;
    this.watchRetry = setTimeout(() => {
      this.watchRetry = 0;
      this.sendWatch();
    }, wait);
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
    // THE HANDSHAKE CARRIES THE SESSION COOKIE, which the browser attaches to
    // a same-origin upgrade as it does to any request, and nothing else. There
    // is deliberately no query-string credential: a URL is written into every
    // proxy's access log, and the engine reads none there.
    let sock: WebSocket;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}`);
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
   * This client believed otherwise once, and the whole repair path hung off a
   * code that never arrived: a refused credential produced a dashboard that
   * reconnected for ever, said "retrying", and offered no way to correct the
   * one thing that was wrong.
   *
   * So the status is fetched where a browser will hand it over. A plain GET of
   * the same path runs the same guard, with the same cookie, and stops one line
   * short of the upgrade: 401 is nobody signed in, 426 (Upgrade Required)
   * means the session was accepted and only the missing header stopped it. A
   * throw is the network, which is not an auth problem and must not send
   * anybody to sign in.
   */
  private async probeRefusal(): Promise<void> {
    if (this.isClosed) return;
    try {
      const res = await fetch(PATH, { credentials: "same-origin", cache: "no-store" });
      // Not `!res.ok`: 426 is the healthy answer here, and every other failure
      // is the network or a proxy, neither of which the reader fixes by
      // signing in.
      if (res.status === 401) this.authRejected();
      else if (res.status === 403) {
        const body = (await res.json().catch(() => null)) as {
          error?: string;
          detail?: string;
        } | null;
        // A SESSION THAT MAY ONLY ENROL A SECOND FACTOR is refused this
        // surface until it has, and its repair is the person's own — the
        // enrolment screen — rather than an administrator's. So it stops
        // the loop as a refusal does, and asks for the enrolment rather
        // than saying access was withdrawn.
        if (body?.error === "second_factor_enrolment_required") this.enrolmentRequired();
        else this.accessRefused(body?.detail ?? "");
      }
    } catch {
      // Offline, or a proxy that refuses the request outright. The reconnect
      // loop already covers it.
    }
  }

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
    this.stopDialling();
    this.store.setAccessRefused(reason);
  }

  /**
   * The engine accepts this browser's session for nothing but enrolling the
   * second factor the deployment requires. Every dial would be refused the
   * same way until it has, so the loop stops; the enrolment ends by calling
   * `reconnect()` with the whole session it opened.
   */
  private enrolmentRequired(): void {
    this.stopDialling();
    needSession("second_factor");
  }

  /** Stops the reconnect loop and the REST fallback, until `reconnect()`. */
  private stopDialling(): void {
    this.refused = true;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.stopFallback();
  }

  /**
   * The engine resolved nobody from this browser's cookie.
   *
   * Two things happen, and both are needed. Asking for a sign-in is the repair
   * — the dashboard is served unauthenticated by design (the page that signs a
   * person in cannot itself require them to be), so the browser has no other
   * moment to learn it needs one. The store flag is what the chrome reads while
   * the loop goes on dialling: a sign-in in another tab gives this one the
   * cookie too, and the next dial is what notices.
   *
   * The socket does not own the screen. It cannot: the sign-in is a route, and
   * a transport that reaches into the router is a transport that cannot be
   * tested without one — so it raises the session need the app follows.
   */
  private authRejected(): void {
    this.store.setAuthRejected(true);
    needSession("sign_in");
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
          this.watchAnswered(msg);
          break;
        }
        // An error frame always carries a code. One that does not is still
        // a failure nobody explained, which is what `query_failed` means.
        //
        // AND A REFUSAL ON AUTHORITY SAYS WHY — the rule and the grants that
        // would have admitted the reader — which the engine sends beside the
        // code under the REST envelope's own keys.
        this.settle(msg.id, msg.error || "query_failed", null, refusalOf(msg));
        break;
      case "pong":
        break;
    }
  }

  private settle(
    id: number | undefined,
    error: string | null,
    data: unknown,
    refusal: QueryRefusal | LogRefusal | null = null,
  ): void {
    if (id === undefined) return;
    const entry = this.inflight.get(id);
    if (!entry) return;
    this.inflight.delete(id);
    clearTimeout(entry.timer);
    if (error) entry.reject(new QueryRefusedError(error, refusal));
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
    if (this.isClosed || this.refused || this.fallbackRun !== 0) return;
    this.fallbackRun = ++this.fallbackRuns;
    void this.fallbackFetch(this.fallbackRun);
  }

  private stopFallback(): void {
    clearTimeout(this.fallbackTimer);
    this.fallbackTimer = 0;
    this.fallbackRun = 0;
  }

  private async fallbackFetch(run: number): Promise<void> {
    this.fallbackTimer = 0;
    const read = await api.snapshot();
    // A fetch started while the socket was down can land after it came back, by
    // which time the handshake snapshot and any pushes since are fresher than
    // this one. Degraded mode must not overwrite live state with a reading it
    // took before the connection recovered — and the open that stopped this
    // run is what says so, as is a `stop()`.
    if (this.fallbackRun !== run || this.connected) return;
    if (read.state === "read") this.store.applySnapshot(read.snapshot);
    // THE NEXT READ WAITS WHAT THE ENGINE SAID, as every other re-ask does: a
    // 503 it wrote replaces the next tick with its `Retry-After`, and one with
    // none is its statement that waiting will not change the answer, so this
    // run stops there — the reconnect loop goes on, and a socket that opens
    // and later drops starts a fresh one. Anything else it could not read —
    // the network, a proxy — is the ordinary tick.
    const next =
      read.state === "unread" && read.retryAfter !== null
        ? retryAfterMs(read.retryAfter)
        : FALLBACK_MS;
    if (next === null) return;
    this.fallbackTimer = setTimeout(() => void this.fallbackFetch(run), next);
  }
}
