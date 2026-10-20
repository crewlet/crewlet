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
 * The handshake carries the session cookie, and the tab's reader in its URL
 * (a browser sets no header on a WebSocket), and the engine decides it once.
 * After that it CLOSES the socket when the identity estate moves under it —
 * and the code says what the tab should do:
 *
 *  - 4401 (`CLOSE_UNAUTHENTICATED`): the session the socket was opened with
 *    ended — a sign-out here or everywhere, a suspension, a removal, a
 *    revocation, a fleet-wide invalidation, or a STEP-UP, which replaces the
 *    session it was made from. Not a refusal on its own: the browser may hold
 *    a newer cookie, and after a step-up it does, once the answer that set it
 *    has landed. So the tab waits for its own requests to settle and asks
 *    `GET /auth/session` once: a `401` is nobody and sends the reader to sign
 *    in with nothing dialled, a session somebody else's sign-in left in the
 *    browser hands the tab to them (`LiveSocketOptions.takeSession`), and any
 *    other answer dials again.
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
import { MAX_ARTIFACT_ROOMS, QUERY_TIMEOUT_MS } from "../contract/wire.ts";
import { api } from "./api.ts";
import { auth } from "./auth.ts";
import { RestError, retryHintOf, whenRequestsSettle } from "./rest.ts";
import { retryAfterMs } from "./retry.ts";
import {
  needSession,
  READER_CHANGED,
  readerHeaders,
  readerMoved,
  readerQuery,
  readerUnlearned,
} from "./signin.ts";
import type { Store } from "./store.ts";
import type {
  ArtifactChangedPush,
  ArtifactDataPush,
  ArtifactJoined,
  ArtifactPresencePush,
  Frame,
  LogRefusal,
  PresenceActivity,
  PresenceFocus,
  PresenceRoomKind,
  QueryMap,
  QueryName,
  QueryRefusal,
} from "./types.ts";

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
 * What a watch refusal's error frame names in `what` — the engine's
 * `watchWhat`. A watch carries no query id, so this is how an error frame about
 * one is told from a query's.
 */
const WATCH_WHAT = "watch";

/**
 * What an artifact room's join, its refusal and its withdrawal name in `what`:
 * the engine's `artifactJoinWhat`. A join's answer carries its id; a
 * withdrawal, which the engine sends when the reader's access narrows while
 * they are in the room, carries none and names the artifact instead.
 */
const ARTIFACT_JOIN_WHAT = "artifact_join";

/**
 * What a screen in an artifact's room hears.
 *
 * `joined` is called with EVERY join's answer: the first, and the one each
 * reconnect makes, whose `resync` says whether the pushes this tab missed can
 * be caught up from the `through` it held or the datastore must be read whole.
 * `refused` is a join the engine refused or a room it withdrew, with the code
 * (`not_found` for both, in the words an artifact nobody holds is answered
 * with), after which no push arrives until the listener joins again.
 */
export interface RoomListener {
  joined?: (answer: ArtifactJoined) => void;
  refused?: (error: QueryError) => void;
  changed?: (push: ArtifactChangedPush) => void;
  data?: (push: ArtifactDataPush) => void;
  presence?: (push: ArtifactPresencePush) => void;
}

/** One artifact's room as this tab holds it. */
interface Room {
  listeners: Set<RoomListener>;
  /** The id of the join frame out now, or 0. */
  joining: number;
  /** Whether the engine put this socket in the room, and has not withdrawn it. */
  joined: boolean;
  /**
   * The datastore's version the tab last held: the latest join's or push's
   * `through`, which the next join names as `since` so a reconnect catches up
   * rather than reading the collections whole.
   */
  since: number;
  /** A join the engine could not decide, asked again on its `retry_after`. */
  retry: ReturnType<typeof setTimeout> | 0;
}

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

/** What the app hands the socket that the protocol alone cannot know. */
export interface LiveSocketOptions {
  /**
   * Asked with the person a `4401`'s `GET /auth/session` answered for, before
   * the re-dial: whether this tab carries on as them. The app answers through
   * `lib/session.ts`'s `takeSession`, which hands the tab over — a reload with
   * nothing kept — where the session is somebody other than the person the tab
   * was read by, and answers false so nothing is dialled for them in it. A
   * socket handed nothing carries on with every session: the protocol knows
   * no reader, and neither does the e2e replay that drives it.
   */
  takeSession?: (person: string) => boolean;
}

export class LiveSocket {
  private store: Store;
  private takeSession: (person: string) => boolean;
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
  /**
   * Whether the latch above is set because NOBODY IS SIGNED IN (see
   * `authRejected`) — the one refusal the tab coming back may lift, since a
   * sign-in in another tab gives this one the cookie too.
   */
  private signedOut = false;
  /**
   * Whether a sign-out holds the socket (see `hold`). Not `refused`, because
   * a sign-out that fails gives the socket back as it found it — still
   * refused, if it was.
   */
  private held = false;
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
  /**
   * The artifact rooms this tab is in, by artifact id. HELD HERE rather than
   * only sent, for the watch's reason: a room lives in the engine's routing
   * index for ONE socket, so every open joins each again.
   */
  private rooms = new Map<string, Room>();
  /** Join frames out now, by their id, to the artifact each names. */
  private joins = new Map<number, string>();
  /**
   * Which room a tab has open, and whether somebody is typing in it.
   *
   * HELD RATHER THAN SENT AND FORGOTTEN, for the reason the watch is: it is
   * state the SERVER keeps per socket, so a reconnect gets a new socket that
   * knows nothing, and a reader sitting in a room through a thirty-second
   * backoff would come back invisible to everybody else in it and see nobody.
   */
  private focused = "";
  private typing = false;
  /** Seats, by AGENT ID, whose live call is being fetched whole, each with
   *  whether a push found the call behind again while the fetch was out — see
   *  `fetchCalls`. */
  private fetchingCalls = new Map<string, boolean>();

  constructor(store: Store, options: LiveSocketOptions = {}) {
    this.store = store;
    this.takeSession = options.takeSession ?? (() => true);
  }

  /**
   * The tab came back to a socket stopped because nobody was signed in: dial
   * ONCE, which is what notices a sign-in made in another tab. A dial that
   * finds nobody still stops again.
   */
  private readonly onVisible = (): void => {
    if (!this.signedOut || this.isClosed || document.visibilityState !== "visible") return;
    this.signedOut = false;
    this.refused = false;
    this.connect();
  };

  /**
   * Dial for the session the frame has just read (`app/Shell.tsx`), unless a
   * dial is already open or under way.
   *
   * IT LIFTS WHAT AN EARLIER SESSION LATCHED: the frame calls this for every
   * session it reads — after a sign-in, or once an admin has put a refusal
   * right — and a refusal or a sign-out recorded under the last one no longer
   * describes the browser.
   */
  start(): void {
    this.lift();
    this.connect();
  }

  /**
   * Drop this socket and re-dial immediately.
   *
   * A dropped envelope is gone: the server's per-client queue discards the
   * OLDEST frame under backpressure, so a lost `agents` overlay is never
   * re-sent and the only true repair is a fresh handshake snapshot. It is
   * also how a restored access is tried: a re-dial is a new attempt, so the
   * last refusal no longer describes it.
   */
  reconnect(): void {
    this.lift();
    if (this.sock) this.sock.close();
    else this.connect();
  }

  /** Forget what the last refusal or sign-out latched. */
  private lift(): void {
    this.refused = false;
    this.signedOut = false;
    this.store.setAuthRejected(false);
    this.store.setAccessRefused(null);
  }

  /**
   * Dial nothing while this tab signs out, until `release()`.
   *
   * The engine ends this session's socket (4401) as it applies the sign-out,
   * and that close re-dialled once this tab's own requests had settled — which
   * is before the sign-out's reload — so every sign-out made one more
   * handshake the engine refused.
   */
  hold(): void {
    this.held = true;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.stopFallback();
  }

  /**
   * Give back what `hold` took: a sign-out nothing answered stays on the page,
   * and the socket dials again — unless a refusal had already stopped it,
   * which the sign-out did not change.
   */
  release(): void {
    this.held = false;
    this.connect();
  }

  stop(): void {
    this.isClosed = true;
    document.removeEventListener("visibilitychange", this.onVisible);
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.watchRetry);
    for (const room of this.rooms.values()) clearTimeout(room.retry);
    this.rooms.clear();
    this.joins.clear();
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
   * Say which chat room this tab is looking at, and whether it is being typed
   * in: the chat room's `presence` frame, which names the room by its kind and
   * its id like every room's (ADR-0038).
   *
   * THE FRAME ITSELF JOINS, unlike an artifact's, because a tab looks at one
   * chat room at a time: naming a room puts this socket in its presence (once
   * the engine has decided the room is one this reader may see) and takes it
   * out of the room it was in, and an empty room id takes it out of the one it
   * was in. The engine answers with the room's members (`chat_presence`) and
   * every change after; typing is EDITING, which the engine puts back to
   * viewing once nothing has re-asserted it inside its lapse.
   *
   * It is not delivered while the socket is down and is not queued: presence
   * is about now. The reconnect re-sends what this tab is looking at, which is
   * the state above, since the new socket is in no room. Only the chat screen
   * sends it, and the frame mounts that screen only for a reader holding
   * Chatlet, so a tab without it never names a room.
   */
  focus(channelID: string, typing: boolean): void {
    this.focused = channelID;
    this.typing = typing;
    this.sendFocus();
  }

  private sendFocus(): void {
    if (!this.connected || !this.sock) return; // `onopen` sends it
    const activity: PresenceActivity = this.focused !== "" && this.typing ? "editing" : "viewing";
    const room: PresenceRoomKind = "chat";
    try {
      this.sock.send(
        JSON.stringify({ kind: "presence", room, channel_id: this.focused, activity }),
      );
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

  // ---- artifact rooms ----------------------------------------------------

  /**
   * Be in an artifact's room for as long as `listener` is: hear what moves on
   * the artifact (`artifact_changed`), what its datastore commits
   * (`artifact_data`) and who else is there (`artifact_presence`). Returns the
   * way out.
   *
   * ONE JOIN PER ARTIFACT however many screens listen: the first listener
   * joins, the last to leave leaves, and every open of the socket joins each
   * room again with the `through` the tab last held, so the engine answers
   * whether a catch-up is enough (`resync: false`) or the datastore must be
   * read whole. The engine decides a join as it decides reading the artifact,
   * and refuses one in the words an artifact nobody holds is answered with; a
   * refusal, and a room the engine withdraws when the reader's access narrows,
   * reach every listener's `refused`, and no push follows.
   */
  room(artifact: string, listener: RoomListener): () => void {
    const id = artifact.trim();
    let room = this.rooms.get(id);
    if (!room) {
      // PAST THE ENGINE'S BOUND, said at once rather than after the round
      // trip the engine's own `bad_params` would take.
      if (this.rooms.size >= MAX_ARTIFACT_ROOMS) {
        listener.refused?.(
          new QueryError(
            "bad_params",
            null,
            `a tab is in at most ${MAX_ARTIFACT_ROOMS} artifact rooms at once`,
          ),
        );
        return () => {};
      }
      room = { listeners: new Set(), joining: 0, joined: false, since: 0, retry: 0 };
      this.rooms.set(id, room);
      room.listeners.add(listener);
      this.sendJoin(id);
    } else {
      room.listeners.add(listener);
    }
    return () => this.unlisten(id, listener);
  }

  /**
   * Say what this tab is doing in a room it is in: what it is looking at and
   * whether it is editing. Who is doing it is never the frame's to say: the
   * engine shows the seat the socket's credential resolved to. A frame for a
   * room the engine has not put this socket in is not sent, since it would be
   * dropped there; the next one after the join lands says it again.
   */
  presence(artifact: string, activity: PresenceActivity, focus: PresenceFocus): void {
    const room = this.rooms.get(artifact);
    if (!room?.joined || !this.connected || !this.sock) return;
    try {
      const room: PresenceRoomKind = "artifact";
      this.sock.send(JSON.stringify({ kind: "presence", room, artifact, activity, focus }));
    } catch {
      // The close handler owns recovery, and the next join says it again.
    }
  }

  private unlisten(artifact: string, listener: RoomListener): void {
    const room = this.rooms.get(artifact);
    if (!room || !room.listeners.delete(listener) || room.listeners.size > 0) return;
    clearTimeout(room.retry);
    this.rooms.delete(artifact);
    if (room.joining) this.joins.delete(room.joining);
    if (!this.connected || !this.sock) return;
    try {
      this.sock.send(JSON.stringify({ kind: "artifact_leave", artifact }));
    } catch {
      // A socket that is closing leaves every room by itself.
    }
  }

  private sendJoin(artifact: string): void {
    const room = this.rooms.get(artifact);
    if (!room || !this.connected || !this.sock) return; // `onopen` joins it
    clearTimeout(room.retry);
    room.retry = 0;
    if (room.joining) this.joins.delete(room.joining);
    const id = this.nextQueryId++;
    room.joining = id;
    this.joins.set(id, artifact);
    const frame = {
      kind: "artifact_join",
      id,
      artifact,
      ...(room.since > 0 ? { since: room.since } : {}),
    };
    try {
      this.sock.send(JSON.stringify(frame));
    } catch {
      // The close handler owns recovery, and the next open joins again.
    }
  }

  /** A join's answer: the room as it presents now, and where the datastore stands. */
  private joinAnswered(id: number, answer: ArtifactJoined): void {
    const artifact = this.joins.get(id);
    if (artifact === undefined) return;
    this.joins.delete(id);
    const room = this.rooms.get(artifact);
    if (!room || room.joining !== id) return;
    room.joining = 0;
    room.joined = true;
    if (typeof answer.through === "number" && answer.through > 0) room.since = answer.through;
    for (const listener of [...room.listeners]) listener.joined?.(answer);
  }

  /**
   * A join the engine refused, or a room it withdrew (no id). `unavailable` is
   * a node that could not decide, asked again on the frame's `retry_after`
   * (`unavailableRetryMs`); every other code is a decision, said to the
   * listeners, which stay registered so a later open of the socket (after a
   * share is put right) asks again.
   */
  private joinRefused(msg: Frame): void {
    let artifact = msg.artifact ?? "";
    if (msg.id !== undefined) {
      const joined = this.joins.get(msg.id);
      if (joined === undefined) return;
      this.joins.delete(msg.id);
      artifact = joined;
    }
    const room = this.rooms.get(artifact);
    if (!room || (msg.id !== undefined && room.joining !== msg.id)) return;
    room.joining = 0;
    room.joined = false;
    const code = msg.error || "query_failed";
    const refusal = refusalOf(msg);
    if (code === "unavailable") {
      const wait = unavailableRetryMs(refusal);
      if (wait !== null) {
        room.retry = setTimeout(() => {
          room.retry = 0;
          this.sendJoin(artifact);
        }, wait);
      }
    }
    const error = new QueryError(code, refusal, badParamsDetail(msg));
    for (const listener of [...room.listeners]) listener.refused?.(error);
  }

  /** Hands a room's push to its listeners, reading the room off the payload's own `artifact`. */
  private roomPush(kind: "changed" | "data" | "presence", data: unknown): void {
    if (!data || typeof data !== "object") return;
    const artifact = (data as { artifact?: unknown }).artifact;
    if (typeof artifact !== "string") return;
    const room = this.rooms.get(artifact);
    if (!room?.joined) return;
    if (kind === "data") {
      const through = (data as ArtifactDataPush).through;
      if (typeof through === "number" && through > room.since) room.since = through;
    }
    for (const listener of [...room.listeners]) {
      if (kind === "changed") listener.changed?.(data as ArtifactChangedPush);
      else if (kind === "data") listener.data?.(data as ArtifactDataPush);
      else listener.presence?.(data as ArtifactPresencePush);
    }
  }

  // ---- connection --------------------------------------------------------

  /**
   * Dial, for the tab's reader.
   *
   * `learned` is a caller that has just read the session and taken it
   * ([learnReader], [redialIfSignedIn]): the reader is whatever that left,
   * and asking again could not learn more — a browser refusing storage
   * records nobody however often it is asked.
   */
  private connect(learned = false): void {
    if (
      this.isClosed ||
      this.refused ||
      this.held ||
      this.learning ||
      (this.sock &&
        (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING))
    ) {
      return;
    }
    // A TAB THAT HAS RECORDED NOBODY LEARNS WHO BEFORE IT DIALS. The frame
    // dials once its `GET /auth/session` has answered, and that read records
    // the reader — but it dials when the read FAILED too, and a reconnect or
    // a signed-out tab coming back into view dials on its own. A dial naming
    // nobody is served as whoever holds the cookie, so a sign-in in another
    // tab between that dial and the tab's next session read was ADOPTED as
    // this tab's reader by a tab still drawing the company it had been
    // served for the last one.
    if (!learned && readerUnlearned()) {
      void this.learnReader();
      return;
    }
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // THE HANDSHAKE CARRIES THE SESSION COOKIE, which the browser attaches to
    // a same-origin upgrade as it does to any request, and no credential
    // beside it: a URL is written into every proxy's access log, and the
    // engine reads none there.
    //
    // WHAT IT DOES CARRY IN ITS URL IS THE TAB'S READER (`readerQuery`), the
    // one thing every other request says in a header and a handshake cannot:
    // a browser sets no header on a WebSocket. A dial made under a cookie a
    // sign-in in another tab moved is then refused before any frame of the
    // new person's company is drawn — as 1006, which the refusal probe reads
    // as `reader_changed` and hands the tab over on — rather than served
    // until the tab's next request was refused. The id is not a secret; it is
    // in the access log of the socket's path.
    let sock: WebSocket;
    try {
      sock = new WebSocket(`${proto}://${location.host}${PATH}${readerQuery()}`);
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
      // AND EVERY ARTIFACT ROOM, for the same reason, each naming the
      // `through` it last held so the engine can say whether a catch-up is
      // enough.
      for (const artifact of this.rooms.keys()) this.sendJoin(artifact);
      // AND WHERE THIS TAB IS LOOKING, which the new socket does not know
      // either: a reader who sat in a room through a reconnect would otherwise
      // be invisible to everybody else in it until they clicked something.
      if (this.focused !== "") this.sendFocus();
    };
    sock.onmessage = (e: MessageEvent) => this.onMessage(String(e.data));
    sock.onclose = (e: CloseEvent) => {
      this.stopPing();
      this.sock = null;
      // A pending watch retry was for the socket that just went; the next
      // open sends the watch anyway.
      clearTimeout(this.watchRetry);
      this.watchRetry = 0;
      // THE ROOMS WENT WITH THE SOCKET: the engine took this socket out of
      // every room as it closed, and the next open joins each again. A join
      // out now, or one waiting on a retry, was for this socket.
      this.joins.clear();
      for (const room of this.rooms.values()) {
        clearTimeout(room.retry);
        room.retry = 0;
        room.joining = 0;
        room.joined = false;
      }
      // Queries are NOT failed here: they are reads, and the reconnect re-sends
      // them. Their answer-timeout is stopped so the wait for a new socket is
      // not counted against the server.
      for (const entry of this.inflight.values()) {
        clearTimeout(entry.timer);
        entry.timer = 0;
      }
      this.store.setConnected(false);
      // A SIGN-OUT IS UNDER WAY: neither a re-dial nor the fallback's read,
      // both of which would carry the cookie it is ending.
      if (this.held) return;
      const code = e ? e.code : 0;
      if (code === CLOSE_FORBIDDEN) {
        // No reconnect and no REST fallback: both would be answered 403 by
        // the same decision, for as long as the tab stayed open.
        this.accessRefused(e.reason);
        return;
      }
      if (code === CLOSE_UNAUTHENTICATED) {
        // THE SESSION ENDED, and the browser may already hold the one that
        // replaced it — or be about to: a step-up's answer sets it. Ask once
        // this tab's own requests have settled (`redialIfSignedIn`).
        // Neither the backoff nor the fallback: the fallback's read, sent
        // now, would carry the ended cookie and send a person who just
        // proved who they are to the sign-in form.
        void whenRequestsSettle().then(() => this.redialIfSignedIn());
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
   * After a `4401`: dial again only if the browser still holds a session.
   *
   * `GET /auth/session` FIRST, rather than a dial whose handshake decides:
   * a session that ended elsewhere — a password change, a sign-out
   * everywhere, an administrator — was a refused handshake, its refusal
   * probe and a refused degraded-mode snapshot before the sign-in, three
   * `401`s where one says it. A `401` here is that one (`rest.ts` raises the
   * sign-in need); any other failure says nothing about the session, and
   * the dial's own handshake decides.
   *
   * AND ONLY FOR THE PERSON THIS TAB WAS READ BY (`takeSession`): a sign-in
   * as somebody else in another tab ends this tab's session and moves the
   * cookie, and a re-dial on any answer reached the company as them in a tab
   * still holding the last reader's.
   */
  private async redialIfSignedIn(): Promise<void> {
    try {
      const session = await auth.session();
      if (!this.takeSession(session.person)) return;
    } catch (err) {
      if (err instanceof RestError && err.status === 401) {
        this.authRejected();
        return;
      }
      // ANY OTHER FAILURE says nothing about the session, and the dial's
      // own handshake decides — for the reader this tab already has; a tab
      // that recorded nobody learns who first ([learnReader]).
      this.connect();
      return;
    }
    this.connect(true);
  }

  /** Whether [learnReader]'s session read is out, which a dial waits for. */
  private learning = false;

  /**
   * Read who this browser is signed in as, take it, and only then dial.
   *
   * THE READ FIRST, so the dial names the reader the tab has just recorded
   * and the engine refuses it if the cookie moves before it lands. A `401`
   * is nobody, the sign-in need every transport raises. ANY OTHER FAILURE
   * DIALS NOTHING and asks again on the backoff: the read says nothing about
   * who this is, and a dial now would be served as whoever holds the cookie
   * to a tab that could not tell them from its next reader. A session the
   * app hands the tab over for dials nothing: the tab is being reloaded.
   */
  private async learnReader(): Promise<void> {
    this.learning = true;
    let person: string;
    try {
      person = (await auth.session()).person;
    } catch (err) {
      this.learning = false;
      if (err instanceof RestError && err.status === 401) this.authRejected();
      else this.scheduleReconnect();
      return;
    }
    this.learning = false;
    if (this.isClosed || this.held || !this.takeSession(person)) return;
    this.connect(true);
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
      // NAMING THE TAB'S READER in the header a plain GET can carry. A dial
      // refused while the browser is somebody else's — for the reader its
      // URL named, or for that person's own session — is then told so here,
      // and the tab handed to them, rather than re-asked as them.
      const res = await fetch(PATH, {
        credentials: "same-origin",
        cache: "no-store",
        headers: readerHeaders(),
      });
      // Not `!res.ok`: 426 is the healthy answer here, and every other failure
      // is the network or a proxy, neither of which the reader fixes by
      // signing in.
      if (res.status === 401) this.authRejected();
      else if (res.status === 409) {
        const body = (await res.json().catch(() => null)) as { error?: unknown } | null;
        if (body?.error === READER_CHANGED) readerMoved();
      } else if (res.status === 503) await this.redialWhenSaid(res);
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
   * `reconnect()` is the way back, once an admin has put it right — or
   * `start()`, which the frame calls once it reads the session again.
   */
  private accessRefused(reason: string): void {
    this.stopDialling();
    this.store.setAccessRefused(reason);
  }

  /**
   * The engine accepts this browser's session for nothing but enrolling the
   * second factor the deployment requires. Every dial would be refused the
   * same way until it has, so the loop stops; the enrolment ends in the
   * frame, which dials again (`start()`) for the whole session it opened.
   */
  private enrolmentRequired(): void {
    this.stopDialling();
    needSession("second_factor");
  }

  /** Stops the reconnect loop and the REST fallback, until `start()` or `reconnect()`. */
  private stopDialling(): void {
    this.refused = true;
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.stopFallback();
  }

  /**
   * The engine resolved nobody from this browser's cookie.
   *
   * Asking for a sign-in is the repair — the dashboard is served
   * unauthenticated by design (the page that signs a person in cannot itself
   * require them to be), so the browser has no other moment to learn it needs
   * one — and the store flag is what the chrome reads meanwhile.
   *
   * AND THE LOOP STOPS, as it does for a refusal: every dial until somebody
   * signs in is the same 401, and a signed-out tab — the sign-in page, an
   * invitation's or a reset link's — dialled one on its backoff for as long
   * as it stayed open, each a failed handshake and a 401 in the console. What
   * ends it is a sign-in: in this tab, which lands in the frame and dials
   * (`start()`); or in another, which gives this one the cookie too and is
   * noticed by ONE dial when the tab comes back (`onVisible`).
   *
   * The socket does not own the screen: the sign-in is a route, and a
   * transport that reaches into the router is a transport that cannot be
   * tested without one — so it raises the session need the app follows.
   */
  private authRejected(): void {
    this.store.setAuthRejected(true);
    needSession("sign_in");
    this.stopDialling();
    this.signedOut = true;
    // ONE LISTENER however often this runs: the same function is added once.
    document.addEventListener("visibilitychange", this.onVisible);
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
        this.fetchCalls(this.store.applyAgents(msg.data));
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
      case "artifact_changed":
        this.roomPush("changed", msg.data);
        break;
      case "artifact_data":
        this.roomPush("data", msg.data);
        break;
      case "artifact_presence":
        this.roomPush("presence", msg.data);
        break;
      // THE CHAT ARM. Six kinds, ONE store method: the frame carries the
      // record's own `op` (`post`, `edit`, `react`, …) and the kind is a
      // routing label the server derived from it, so passing the kind as well
      // would be a second vocabulary for one fact — and the two would
      // disagree on exactly the op somebody adds next.
      //
      // Every one of these was already filtered by the engine against whether
      // this viewer may read the room, failing closed, so nothing below this
      // line decides who may see what. The one frame sent past that rule is
      // the membership change that took this viewer OUT of a room: it names
      // the room and the record and no member, and the rail's re-read on it
      // is how the room leaves the rail.
      case "chat_posted":
      case "chat_edited":
      case "chat_deleted":
      case "chat_reacted":
      case "chat_room_changed":
      case "chat_membership_changed":
        this.store.applyChatChange(msg.data as never);
        break;
      case "chat_presence":
        this.store.applyChatPresence(msg.data as never);
        break;
      // NOT ABOUT A ROOM AT ALL: where the seat this tab reads as has read
      // to, sent to that seat's own sockets and nobody else's, so a badge
      // cleared in one window clears in the other.
      case "chat_cursor_moved":
        this.store.applyChatRead(msg.data as never);
        break;
      case "result":
        if (msg.id !== undefined && this.joins.has(msg.id)) {
          this.joinAnswered(msg.id, msg.data as ArtifactJoined);
          break;
        }
        this.settle(msg.id, null, msg.data);
        break;
      case "error":
        // A WATCH'S REFUSAL carries no query id, only what it is about.
        if (msg.what === WATCH_WHAT && msg.id === undefined) {
          this.watchAnswered(msg);
          break;
        }
        // A JOIN'S REFUSAL carries its id, and a ROOM WITHDRAWN carries none
        // and names the artifact.
        if (msg.what === ARTIFACT_JOIN_WHAT) {
          this.joinRefused(msg);
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

  /**
   * Fetch, whole, each seat's live call this tab holds behind what a push
   * described — asked by the seat's AGENT ID, the key the push named it by — the push that moved one of its heavy fields was dropped by the
   * server's queue (`mergeLiveCall`).
   *
   * ONE ASK PER SEAT IN FLIGHT, but a push that finds the call behind while it
   * is out is NOT the same thing said twice: the answer may have been read
   * before that push's change, and the store then drops it or takes only the
   * fields it has newer (`Store.applyLiveCall`). So such a push is remembered,
   * and once the answer lands BEHIND the pushes applied since — or the ask
   * fails — the call is asked for again, once, rather than left behind until
   * the seat's next push, which a frozen failed call or a long round never
   * sends. An answer at or past them holds everything they named, and ends it.
   */
  private fetchCalls(ids: string[]): void {
    for (const id of ids) {
      if (this.fetchingCalls.has(id)) {
        this.fetchingCalls.set(id, true);
        continue;
      }
      this.fetchingCalls.set(id, false);
      this.query("live_call", { agent_id: id })
        .then(
          (answer) => this.store.applyLiveCall(answer),
          () => false,
        )
        .then((current) => {
          const again = this.fetchingCalls.get(id) ?? false;
          this.fetchingCalls.delete(id);
          if (again && !current) this.fetchCalls([id]);
        });
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
    // NOBODY SIGNED IN ENDS THIS RUN: every later read is the same 401, and a
    // signed-out tab — the sign-in page, an invitation's — polled one every
    // five seconds for as long as it stayed open. The dial's own probe has
    // stopped the loop too (`authRejected`): a sign-in, or the tab coming
    // back, is what dials again.
    if (read.state === "nobody") return;
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
