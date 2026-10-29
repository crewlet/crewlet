/**
 * The Dashboard Assistant's transport: its routes, and the turn stream.
 *
 * EVERY REQUEST GOES THROUGH `rest.ts`, the stream included (`rest.stream`):
 * the session cookie and nothing else, no browser cache in front of a guarded
 * answer, a refusal read as the engine's JSON, a `401` sending the reader to
 * sign in and a `403 step_up_required` confirmed and replayed. Nothing here
 * holds a credential, and the assistant's own grants never reach the browser:
 * the engine derives them per request from this session (ADR-0050).
 *
 * # Routes are typed by one table
 *
 * `AssistantRoutes` (`assistantWire.ts`) names each route with the body it
 * takes and the answer it gives, and [call] is the one function that sends
 * one, so a route and its shapes are named together once. Every body carries
 * the protocol version, and a question or a decision carries its operation
 * key in `Idempotency-Key`, minted per gesture; a decision sends its key again
 * unchanged on a retry.
 *
 * # The stream
 *
 * A turn's frames are server-sent events read over `fetch` rather than an
 * `EventSource`, because an `EventSource` sends no custom header and cannot
 * be told which frame it last saw on the first connection; here a resume
 * sends `Last-Event-ID` (`<turn_id>.<seq>`) and the engine replays only what
 * was missed. [openTurnStream] keeps one stream open until its turn ends:
 *
 *  - a quiet stream is STALLED after two missed keepalives
 *    (`ASSISTANT_STALL_MS`) and dialled again from the last frame it read;
 *  - a dropped connection or a 5xx is dialled again with a backoff, and the
 *    caller is told it is reconnecting, so the drawer can say so. The backoff
 *    and the outage it measures end only when a connection makes PROGRESS (a
 *    frame that moves the turn, or a keepalive from a turn still running), not
 *    when one is merely answered: a connection answered and closed with
 *    nothing new is the same outage, and redialling it at the first step of
 *    the backoff every time is a loop on the engine;
 *  - a frame of another protocol version stops the stream for good with
 *    `protocol_mismatch`, which the drawer answers by asking for a reload,
 *    and a refusal (a 4xx) stops it with the refusal;
 *  - the stream ends when the engine closes it after the turn's `done`, or
 *    answers `204 No Content`: the segment has ended and the caller already
 *    holds all of it (a turn parked on an approval, resumed at its last
 *    frame), so nothing will follow until a decision opens the next one.
 *
 * The frames' order and their duplicates are the reducer's to judge
 * (`app/assistant/chunk/turn.ts` drops a frame at or below the seq it has).
 */

// RELATIVE, not through `~`: protocol/ is also built alone as protocol.js,
// where the alias does not exist.
import { ASSISTANT_PROTOCOL_VERSION } from "../contract/assistant.ts";
import { isAbort, pathSegment, rest, RestError } from "./rest.ts";
import {
  ASSISTANT_STALL_MS,
  assistantEventId,
  createAssistantStreamParser,
  type AssistantNarrowFrame,
  type AssistantRoutes,
  type AssistantStreamFault,
} from "./assistantWire.ts";

/** Where the engine's own assistant routes live; nothing else is followed. */
const ASSISTANT_PREFIX = "/assistant/";

/** A route of the assistant's, as `AssistantRoutes` names it. */
export type AssistantRoute = keyof AssistantRoutes;

/** What a route's request is sent with beyond its body. */
export interface AssistantCallOptions {
  /** The `{id}` in the route's path. */
  id?: string;
  /** Query parameters (a listing's cursor and limit). */
  query?: Record<string, string | number | undefined>;
  /** The gesture's operation key, for the routes that take one. */
  opKey?: string;
  signal?: AbortSignal;
}

/**
 * Send one of the assistant's routes and read its answer, typed by the route.
 * A refusal rejects with the engine's [RestError]; the caller's own abort
 * rejects with its reason.
 */
export async function call<R extends AssistantRoute>(
  route: R,
  body: AssistantRoutes[R]["body"],
  options: AssistantCallOptions = {},
): Promise<AssistantRoutes[R]["answer"]> {
  const space = route.indexOf(" ");
  const method = route.slice(0, space);
  const template = route.slice(space + 1);
  if (template.includes("{id}") && !options.id) {
    throw new TypeError(`assistant: ${route} needs an id`);
  }
  // EVERY ROUTE IN THE TABLE IS UNDER THE PREFIX, and the request names it
  // as a literal so the dev server's proxy gate can read where it goes.
  const tail = template
    .slice(ASSISTANT_PREFIX.length)
    .replace("{id}", () => pathSegment(options.id ?? ""));
  const { body: answer } = await rest.request(method, `/assistant/${tail}`, {
    // A FILE IS SENT AS ITS OWN BYTES, typed as the browser typed it; the
    // engine sniffs the bytes and refuses a type it does not take.
    ...(body === null
      ? {}
      : body instanceof Blob
        ? { body, contentType: body.type || "application/octet-stream" }
        : { body, contentType: "application/json" }),
    ...(options.query ? { query: options.query } : {}),
    ...(options.opKey ? { headers: { "Idempotency-Key": options.opKey } } : {}),
    ...(options.signal ? { signal: options.signal } : {}),
  });
  return answer as AssistantRoutes[R]["answer"];
}

/** The protocol version every body carries. */
export const versioned = { protocol_version: ASSISTANT_PROTOCOL_VERSION } as const;

/**
 * Whether a path the engine handed back (a stream, an export) is one of the
 * assistant's own routes on this origin: a path, under `/assistant/`, with no
 * scheme, no host and no way out of it. A path that is not is refused before
 * any request, since the browser would send the session cookie with it.
 */
export function isAssistantPath(path: string): boolean {
  if (!path.startsWith(ASSISTANT_PREFIX) || path.startsWith("//")) return false;
  // A BACKSLASH OR A CONTROL CHARACTER, which a browser rewrites before it
  // parses an address, and a `..` segment, which climbs out of the prefix.
  if (/[\\\u0000-\u001f\u007f]/.test(path)) return false;
  return !path
    .split("?")[0]!
    .split("/")
    .some((segment) => segment === ".." || segment === ".");
}

/**
 * A file the engine handed back a path to, as its bytes, read under the same
 * session: a table card's export from the stored snapshot (`exports.csv`,
 * `exports.xlsx`), or a stored file's `download` route (a file a deep run
 * delivered, an image the person attached). The path is checked to be one of
 * the assistant's before any request.
 */
export async function exportFile(path: string, signal?: AbortSignal): Promise<Blob> {
  if (!isAssistantPath(path)) {
    throw new RestError(0, {
      error: "invalid_body",
      detail: "the export path is not the assistant's",
    });
  }
  return rest.blob(`/assistant/${path.slice(ASSISTANT_PREFIX.length)}`, signal);
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * A file of the person's own conversation, as its bytes, read through the
 * owner's route (`GET /assistant/conversations/{conversation}/files/{file}`)
 * under the same session: what a draft's file field fetches once its change
 * is approved, and what the card draws a thumbnail of. Both ids are checked
 * to be ids before any request; the bytes are typed by the caller from the
 * reference, never from the response.
 */
export async function conversationFile(
  ref: { conversation_id: string; file_id: string },
  signal?: AbortSignal,
): Promise<Blob> {
  if (!UUID.test(ref.conversation_id) || !UUID.test(ref.file_id)) {
    throw new RestError(0, { error: "invalid_body", detail: "the file reference is not one" });
  }
  return rest.blob(`/assistant/conversations/${ref.conversation_id}/files/${ref.file_id}`, signal);
}

/** Where a turn stream stands, as the drawer says it. */
export type StreamState = "connecting" | "live" | "reconnecting" | "closed";

/** Why a stream stopped for good rather than being dialled again. */
export type StreamStop =
  | AssistantStreamFault
  | { code: "refused"; error: RestError }
  | { code: "invalid_path"; path: string };

export interface TurnStreamHandlers {
  /** One frame, in arrival order; duplicates are the caller's to drop. */
  frame: (frame: AssistantNarrowFrame) => void;
  /** The stream's state moved. `reconnectingSince` is when this outage began. */
  state: (state: StreamState, reconnectingSince: number | null) => void;
  /** The stream stopped for good, for a reason no reconnect can change. */
  stop: (reason: StreamStop) => void;
}

export interface TurnStreamOptions {
  /** The frame the caller already holds, `<turn_id>.<seq>`; the engine replays what follows it. */
  lastEventId?: string;
  /** How long a quiet stream may stay quiet, in ms; the engine's keepalive, missed twice. */
  stallMs?: number;
  /** The first wait before a reconnect, doubled to [maxBackoffMs]. */
  backoffMs?: number;
  maxBackoffMs?: number;
  /** A clock, for a suite. */
  now?: () => number;
}

/** A stream being read; `close` ends it and dials nothing more. */
export interface TurnStream {
  close: () => void;
}

/**
 * Read a turn's frames from `streamUrl` until the turn ends or [close]; see
 * the file's doc for what each failure does.
 */
export function openTurnStream(
  streamUrl: string,
  handlers: TurnStreamHandlers,
  options: TurnStreamOptions = {},
): TurnStream {
  const {
    stallMs = ASSISTANT_STALL_MS,
    backoffMs = 1_000,
    maxBackoffMs = 15_000,
    now = Date.now,
  } = options;
  const outer = new AbortController();
  let last = options.lastEventId ?? "";
  let ended = false;
  let stopped = false;
  let outageSince: number | null = null;
  let wait = backoffMs;
  // THE WAIT THE ENGINE NAMED for the next dial (a `429 assistant_busy`'s or
  // a `503`'s Retry-After), in ms; 0 where it named none.
  let told = 0;

  const setState = (state: StreamState) => {
    if (outer.signal.aborted && state !== "closed") return;
    handlers.state(state, state === "reconnecting" ? outageSince : null);
  };
  const stop = (reason: StreamStop | null) => {
    if (stopped) return;
    stopped = true;
    if (reason !== null) handlers.stop(reason);
    setState("closed");
  };

  if (!isAssistantPath(streamUrl)) {
    queueMicrotask(() => stop({ code: "invalid_path", path: streamUrl }));
    return { close: () => outer.abort() };
  }

  /** Wait `ms`, or until the stream is closed, holding nothing on the signal after. */
  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      const wake = () => {
        clearTimeout(timer);
        outer.signal.removeEventListener("abort", wake);
        resolve();
      };
      const timer = setTimeout(wake, ms);
      outer.signal.addEventListener("abort", wake, { once: true });
    });

  /** One connection: true when the turn ended, false to dial again. */
  async function attempt(): Promise<boolean> {
    const own = new AbortController();
    const forward = () => own.abort();
    outer.signal.addEventListener("abort", forward, { once: true });
    let stall: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      clearTimeout(stall);
      stall = setTimeout(() => own.abort(), stallMs);
    };
    let fault: AssistantStreamFault | null = null;
    // THE OUTAGE ENDS ON PROGRESS. A snapshot alone is the engine saying
    // what it can while it cannot follow the turn, and asking to be asked
    // again: that is not the turn moving, so it backs off like a drop.
    const progressed = () => {
      outageSince = null;
      wait = backoffMs;
    };
    const parser = createAssistantStreamParser({
      frame: (frame) => {
        last = assistantEventId(frame.turn_id, frame.seq);
        if (frame.kind === "done") ended = true;
        if (frame.kind !== "snapshot") progressed();
        handlers.frame(frame);
      },
      fault: (f) => {
        fault = f;
      },
      keepalive: progressed,
    });
    try {
      const body = await rest.stream(`/assistant/${streamUrl.slice(ASSISTANT_PREFIX.length)}`, {
        headers: last === "" ? {} : { "Last-Event-ID": last },
        signal: own.signal,
      });
      // NOTHING FOLLOWS, AND NOTHING WILL: the engine says the segment has
      // ended and this stream already holds all of it.
      if (body.noContent) return true;
      setState("live");
      arm();
      for (;;) {
        const chunk = await body.next();
        if (chunk === null) return ended;
        arm();
        parser.push(chunk);
        if (fault !== null) {
          stop(fault);
          return true;
        }
      }
    } catch (err) {
      if (outer.signal.aborted) return true;
      // A REFUSAL IS AN ANSWER: a stream the engine will not serve this
      // session (not theirs, the protocol moved on, nobody signed in) is
      // not one a reconnect can change. A 408, a 429 and every 5xx are
      // conditions, and so is a connection that dropped or stalled.
      if (err instanceof RestError && err.status >= 400 && err.status < 500) {
        if (err.status !== 408 && err.status !== 429) {
          stop({ code: "refused", error: err });
          return true;
        }
      } else if (!(err instanceof RestError) && !isAbort(err)) {
        throw err;
      }
      // A BUSY ENGINE SAYS WHEN TO COME BACK: the next dial waits for its
      // Retry-After rather than for the backoff, which would ask again
      // into the same refusal.
      if (err instanceof RestError && err.retryAfterSeconds !== null) {
        told = err.retryAfterSeconds * 1000;
      }
      return false;
    } finally {
      // EVERY WAY OUT ENDS THE CONNECTION: after a fault or a refusal the
      // body may still be open, and a body nobody reads holds the engine's
      // handler on a write until the tab closes.
      clearTimeout(stall);
      outer.signal.removeEventListener("abort", forward);
      own.abort();
    }
  }

  void (async () => {
    setState("connecting");
    for (;;) {
      let done: boolean;
      try {
        done = await attempt();
      } catch (err) {
        stop({ code: "malformed", detail: String(err) });
        return;
      }
      if (done || outer.signal.aborted || stopped) break;
      outageSince ??= now();
      setState("reconnecting");
      await sleep(Math.max(wait, told));
      told = 0;
      wait = Math.min(maxBackoffMs, wait * 2);
      if (outer.signal.aborted) break;
    }
    stop(null);
  })();

  return {
    close: () => {
      outer.abort();
      stop(null);
    },
  };
}
