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
 * The socket is the channel for the projection and for every question the
 * query registry answers — not for everything. Writes and the guarded reads no
 * query answers (`/secrets`, `/setup`, `/config`) go over REST through
 * `rest.ts`, and this file makes two HTTP requests of its own: the degraded
 * snapshot, which keeps the page honest while the socket is down (a proxy that
 * refuses to upgrade, a restarting engine) and stops the moment the socket is
 * back, and the refusal probe after a handshake that never opened.
 *
 * # What a close means
 *
 * The handshake carries the session cookie and nothing else, and the engine
 * decides it once. After that it CLOSES the socket when the identity estate
 * moves under it — and the code says what the tab should do:
 *
 *  - 4401 (`CLOSE_UNAUTHENTICATED`): the session the socket was opened with
 *    ended — a sign-out here or everywhere, a suspension, a removal, a
 *    revocation, a fleet-wide invalidation, or a STEP-UP, which replaces the
 *    session it was made from. Not a refusal on its own: the browser may hold
 *    a newer cookie, and after a step-up it does, once the answer that set it
 *    has landed. So the tab waits for its own requests to settle and dials
 *    again; only a re-handshake the probe then reads as 401 sends anybody to
 *    sign in.
 *  - 4403 (`CLOSE_FORBIDDEN`): the engine knows who this is and will not serve
 *    them this surface. Reconnecting reaches the same person with the same
 *    access, so the socket STOPS and the page says why.
 *  - anything else, the standard's 1013 "try again later" included: an
 *    ordinary reconnect on the backoff.
 */

// RELATIVE, like every contract import in this directory: it is also built
// alone as `protocol.js`, where the `~` alias does not exist.
import { CLOSE_FORBIDDEN, CLOSE_UNAUTHENTICATED } from "../contract/closecodes.ts";
import type { QueryErrorCode } from "../contract/errors.ts";
import { UNAVAILABLE_RETRY_MS } from "../contract/retry.ts";
import { api } from "./api.ts";
import { retryHintOf, whenRequestsSettle } from "./rest.ts";
import { retryAfterMs } from "./retry.ts";
import { needSession } from "./signin.ts";
import type { Store } from "./store.ts";
import type { Frame, LogRefusal, QueryMap, QueryName, QueryRefusal } from "./types.ts";

const PATH = "/ws/stream";

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

/**
 * A query the engine refused, with what its error frame said beyond the code.
 *
 * AN ERROR WHOSE MESSAGE IS THE CODE, so every caller that reads a refusal by
 * [queryErrorCode] of its message keeps working, and a TYPE beside it for what
 * the text of an error is no place to carry: the refusal — on AUTHORITY, the
 * rule and the grants that would have admitted the reader; on `unavailable`,
 * the state log's refusal and whether, and when, asking again can change it.
 */
export class QueryError extends Error {
  /**
   * The refusal behind the code, or null — for a frame that is neither a
   * refusal on authority nor an `unavailable` answer, and for the socket's own
   * `timeout` and `closed`, which no engine said anything about.
   */
  readonly refusal: QueryRefusal | LogRefusal | null;
  /**
   * The refusal's own sentence on a `bad_params` refusal — the parameter to
   * change and what it accepts — and null everywhere else. The engine writes
   * that one refusal FOR the caller and keeps every other failure's text in
   * its log, so this is never a path or a driver's message. An `unavailable`
   * answer's words are its {@link LogRefusal}'s.
   */
  readonly detail: string | null;

  constructor(
    code: string,
    refusal: QueryRefusal | LogRefusal | null = null,
    detail: string | null = null,
  ) {
    super(code);
    this.name = "QueryError";
    this.refusal = refusal;
    this.detail = detail;
  }
}

/**
 * Whether a refusal is the state log's, carried by an `unavailable` answer,
 * rather than one on authority. The two ride the same field of an answer
 * because a screen hands both to `QueryState` the same way.
 */
export function isLogRefusal(refusal: QueryRefusal | LogRefusal): refusal is LogRefusal {
  return "retryAfter" in refusal;
}

/**
 * How soon something the engine answered `unavailable` is asked again — a
 * query (see `useQuery`) or a watch — in milliseconds, or `null` for "not on a
 * timer".
 *
 * `unavailable` is the engine saying it cannot answer HERE, and its frame says
 * when that may change: `retry_after`, read through {@link retryAfterMs} —
 * waited out, bounded, and ZERO meaning waiting will not change it, so nothing
 * re-asks. An answer carrying no hint waits {@link UNAVAILABLE_RETRY_MS}, what
 * the engine says when it has nothing better.
 */
export function unavailableRetryMs(refusal: QueryRefusal | LogRefusal | null): number | null {
  if (refusal === null || !isLogRefusal(refusal)) return UNAVAILABLE_RETRY_MS;
  return retryAfterMs(refusal.retryAfter);
}

/**
 * A `bad_params` frame's sentence, or null for any other frame. The engine
 * writes that one refusal for the caller; an `unavailable` frame's words are
 * its refusal's ({@link refusalOf}), and every other frame carries none.
 */
function badParamsDetail(msg: Frame): string | null {
  return msg.error === "bad_params" && typeof msg.detail === "string" && msg.detail !== ""
    ? msg.detail
    : null;
}

/**
 * The refusal an error frame carries, or null — for a frame that is neither a
 * refusal on authority nor an `unavailable` answer.
 */
function refusalOf(msg: Frame): QueryRefusal | LogRefusal | null {
  // AN `unavailable` ANSWER'S REFUSAL AND HINT: the state log's code and
  // words, and whether — and when — asking this node again can change the
  // answer. The engine always sends `retry_after` there; a frame without one,
  // or with a negative one, reads as no hint at all (see `unavailableRetryMs`).
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
   * re-sent and the only true repair is a fresh handshake snapshot. It is
   * also how a sign-in or a restored access is picked up: a re-dial is a new
   * attempt, usually with a credential the browser did not hold at the last
   * one, so the last refusal no longer describes it.
   */
  reconnect(): void {
    this.refused = false;
    this.store.setAuthRejected(false);
    this.store.setAccessRefused(null);
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
   * Rejects with a [QueryError] carrying the server's machine-readable code
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
    // NO CREDENTIAL IN THE FRAME. The engine asks every question as the
    // principal the handshake resolved.
    const frame = { kind: "query", id: entry.id, what: entry.what, params: entry.params };
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
   * The engine refused a watch.
   *
   * `unavailable` means this node could not read the chart that decides it,
   * or the directory a login resolves through, so it is asked again when the
   * frame's `retry_after` says that may have changed ({@link
   * unavailableRetryMs}). A ZERO is a read no wait clears, and ANY OTHER CODE
   * IS A DECISION; either way asking again on a timer would only be answered
   * the same: the screens' polls carry on as they did before there was a push
   * at all, and the next socket asks once more in case the answer moved.
   */
  private watchAnswered(msg: Frame): void {
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
      this.isClosed ||
      this.refused ||
      (this.sock &&
        (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING))
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
      const code = e ? e.code : 0;
      if (code === CLOSE_FORBIDDEN) {
        // No reconnect and no REST fallback: both would be answered 403 by
        // the same decision, for as long as the tab stayed open.
        this.accessRefused(e.reason);
        return;
      }
      if (code === CLOSE_UNAUTHENTICATED) {
        // THE SESSION ENDED, and the browser may already hold the one that
        // replaced it — or be about to: a step-up's answer sets it. Dial
        // once this tab's own requests have settled, and let that dial's
        // handshake say whether anybody is signed in. Neither the backoff
        // nor the fallback: the fallback's read, sent now, would carry the
        // ended cookie and send a person who just proved who they are to
        // the sign-in form.
        void whenRequestsSettle().then(() => this.connect());
        return;
      }
      this.scheduleReconnect();
      this.startFallback();
      // A dial that never opened may be a refusal, and only a plain HTTP
      // re-ask can say which (see `probeRefusal`).
      if (!handshakeCompleted) void this.probeRefusal();
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
   * A handshake the engine refuses NEVER reaches this page as a close code. A
   * close code travels in a close frame, and a connection that never opened
   * has no frames — so the browser reports 1006, the same code it gives for an
   * engine that is simply down, and withholds the status deliberately (a page
   * that could read it could use a socket to scan ports it cannot otherwise
   * reach).
   *
   * So the status is fetched where a browser will hand it over. A plain GET of
   * the same path runs the same guard, with the same cookie, and stops one line
   * short of the upgrade: 401 is nobody signed in, 426 (Upgrade Required)
   * means the session was accepted and only the missing header stopped it, a
   * 403 is somebody refused this surface, and a `503` the engine wrote is a
   * node that could not decide the handshake yet, which says in its
   * `Retry-After` when to dial again (see `redialWhenSaid`). A throw is the
   * network, which is not an auth problem and must not send anybody to sign
   * in.
   */
  private async probeRefusal(): Promise<void> {
    if (this.isClosed) return;
    try {
      const res = await fetch(PATH, { credentials: "same-origin", cache: "no-store" });
      // Not `!res.ok`: 426 is the healthy answer here, and every other failure
      // is the network or a proxy, neither of which the reader fixes by
      // signing in.
      if (res.status === 401) this.authRejected();
      else if (res.status === 503) await this.redialWhenSaid(res);
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
        else this.accessRefused(body?.detail ?? body?.error ?? "");
      }
    } catch {
      // Offline, or a proxy that refuses the request outright. The reconnect
      // loop already covers it.
    }
  }

  /**
   * A refused handshake's `503`: dial again when the engine said, in place of
   * the backoff — exactly, and bounded at `RETRY_AFTER_MAX_MS`.
   *
   * ONLY THE ENGINE'S `503` (`retryHintOf`): a proxy in front of a node that
   * is down writes one too, and that is the backoff's case. A ZERO keeps the
   * backoff rather than stopping the loop, because a dial is not a re-ask of
   * this one node — behind a balancer the next one may reach another — and
   * the loop is this tab's only way back to any engine.
   */
  private async redialWhenSaid(res: Response): Promise<void> {
    const hint = await retryHintOf(res);
    const wait = hint === null ? null : retryAfterMs(hint);
    if (wait !== null) this.scheduleReconnect(wait);
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
   * The socket does not own the screen: the sign-in is a route, and a
   * transport that reaches into the router is a transport that cannot be
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
        this.store.applyAgents(msg.data as never);
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
        this.settle(
          msg.id,
          msg.error || "query_failed",
          null,
          refusalOf(msg),
          badParamsDetail(msg),
        );
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
    refusal: QueryRefusal | LogRefusal | null = null,
    detail: string | null = null,
  ): void {
    if (id === undefined) return;
    const entry = this.inflight.get(id);
    if (!entry) return;
    this.inflight.delete(id);
    clearTimeout(entry.timer);
    if (error) entry.reject(new QueryError(error, refusal, detail));
    else entry.resolve(data);
  }

  private failInflight(reason: string): void {
    for (const entry of this.inflight.values()) {
      clearTimeout(entry.timer);
      entry.reject(new QueryError(reason));
    }
    this.inflight.clear();
  }

  /**
   * Dial again after the backoff — or after `after` ms, where the engine said
   * when (`redialWhenSaid`), which replaces the dial already scheduled and
   * leaves the backoff's count where it was.
   *
   * FULL JITTER: the wait is drawn uniformly between zero and the backoff's
   * ceiling for this attempt. A node that closes every socket at once — a
   * 1013 to everybody on a restart — would otherwise bring every tab back in
   * lockstep, one wave per doubling, each wave as large as the first.
   */
  private scheduleReconnect(after?: number): void {
    if (this.isClosed || this.refused) return;
    clearTimeout(this.reconnectTimer);
    let delay = after;
    if (delay === undefined) {
      delay = Math.random() * Math.min(1000 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
      this.attempt++;
    }
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
    // THE NEXT READ WAITS WHAT THE ENGINE SAID: a 503 it wrote replaces the
    // next tick with its `Retry-After`, and one with none is its statement
    // that waiting will not change the answer, so this run stops there — the
    // reconnect loop goes on, and a socket that opens and later drops starts
    // a fresh one. Anything else it could not read is the ordinary tick.
    const next =
      read.state === "unread" && read.retryAfter !== null
        ? retryAfterMs(read.retryAfter)
        : FALLBACK_MS;
    if (next === null) return;
    this.fallbackTimer = setTimeout(() => void this.fallbackFetch(run), next);
  }
}
