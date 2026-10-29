/**
 * The dashboard's one REST transport: every write, and the guarded reads the
 * socket has no question for.
 *
 * The socket remains the data channel for the projection and for every
 * question in the query registry. This is not a second one: it carries the
 * requests that are not questions about state at all, and the reads that no
 * query answers. A screen does not call it for a read directly — it reads
 * through `lib/useRest.ts`, the one loader, so that a superseded answer, an
 * unmounted screen and a server's `Retry-After` are each handled once. Writes
 * never go over the socket, deliberately: a write is judged on its `Origin` by
 * the engine's cross-site check before its handler runs, answers the three
 * write outcomes as statuses with an op id to retry by, and may be refused for
 * a step-up this module confirms and replays — none of which a frame on an
 * open socket carries. And a handful of reads exist only as REST,
 * `GET /secrets` above all, because no query in the registry answers them.
 *
 * ONE MODULE, for the reason `api.ts` states about itself: a screen reaching
 * for its own transport takes its client from somewhere, and the somewhere the
 * Fleet screen chose was a context field the shell never populated, so it
 * shipped dead. Everything here is a plain function over `fetch` against
 * `location.origin`, which is where the dashboard is served from and the only
 * origin the engine answers on (it writes no CORS header at all).
 *
 * THE CREDENTIAL IS THE SESSION COOKIE, which the browser attaches to every
 * same-origin request and this module never sees: it is `HttpOnly`, so no
 * script on the page can read it, and there is no token in storage for one to
 * take instead. A call that must present something else — the API token
 * exchange at `POST /auth/token` — sets its own `Authorization` header for
 * that one request, and nothing keeps it.
 */

import { confirmStepUp, needSession } from "./signin.ts";
import type { LogRefusal, QueryRefusal } from "./types.ts";
import { answeredByEngine, markedByEngine } from "./engineAnswer.ts";
import { remedyOf, type Remedy } from "./remedy.ts";

/**
 * What the engine said when it refused.
 *
 * `status` alone is not enough to act on: the setup surface distinguishes a
 * revision that moved under the caller from a config slot holding a literal
 * from a company with no active revision, and all three are a 409. The engine
 * answers those with a `code`, and the screen branches on it.
 */
export class RestError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string;
  readonly hint: string;
  /**
   * The engine's own sentence for the code — the envelope's `message`, which
   * every refusal it writes carries — or "" for an answer the engine did not
   * write. What a person is shown when a screen has nothing more specific to
   * say: the sign-in surface's one uniform refusal is exactly this sentence,
   * and a screen that wrote its own would be a second copy of the engine's
   * wording, the one that goes stale.
   */
  readonly sentence: string;
  /** Everything else the body carried, for a caller that needs a field. */
  readonly body: Record<string, unknown>;
  /**
   * The `Retry-After` the answer carried, in whole seconds — either form the
   * RFC allows ([retryAfterSeconds]) — or null where it named no wait.
   *
   * A `429` always carries one and says how long the sign-in curve makes the
   * next attempt wait. A `503` the engine wrote carries one where waiting can
   * clear the cause and NONE where it cannot (a node with no keyring, a full
   * log), which is the difference a loader retries on — see [retryHint],
   * which also refuses to read a proxy's header as the engine's.
   */
  readonly retryAfterSeconds: number | null;
  /**
   * Whether the answer carried the engine's mark ([markedByEngine]): the one
   * thing in it nothing in front of the engine writes. FALSE UNLESS SAID, so
   * an error built anywhere that forgot to say reads as not the engine's
   * (an outcome nobody knows), never as a refusal the engine made; [request]
   * records what the response said.
   */
  readonly marked: boolean;

  constructor(
    status: number,
    body: Record<string, unknown>,
    retryAfter: number | null = null,
    marked = false,
  ) {
    const code = typeof body.error === "string" ? body.error : "";
    const detail = typeof body.detail === "string" ? body.detail : "";
    super(detail || code || `HTTP ${status}`);
    this.name = "RestError";
    this.status = status;
    this.code = code;
    this.detail = detail;
    this.hint = typeof body.hint === "string" ? body.hint : "";
    this.sentence = typeof body.message === "string" ? body.message : "";
    this.body = body;
    this.retryAfterSeconds = retryAfter;
    this.marked = marked;
  }

  /**
   * Whether the engine refused on AUTHORITY rather than on the request.
   *
   * Both statuses, because a screen locks the same way for either: 401 is
   * "nobody is signed in" and 403 is "the credential you presented does not
   * carry this". Only a 401 sends the reader to sign in (see [noteSession]): a
   * person holding a perfectly good session meets 403 the moment they open a
   * screen outside their grants, which is the ordinary case.
   */
  get unauthorized(): boolean {
    return this.status === 401 || this.status === 403;
  }

  /** The grants a refusal on authority named — see [refusedGrants]. */
  get grants(): string[] {
    return refusedGrants(this.body);
  }

  /** Where the limit that caused the refusal is changed, or null ([remedyOf]). */
  get remedy(): Remedy | null {
    return remedyOf(this.body.remedy);
  }

  /**
   * This refusal in the shape `QueryState` renders from — the one the
   * socket's error frame carries, under the same keys and on the same terms:
   * a `403 unauthorized`'s rule and the grants that would have admitted the
   * caller, and a 503's state-log code, its own words and whether waiting
   * changes it (no `Retry-After` is the engine saying it will not). Null for
   * anything else, a 401 included: nobody being signed in is not a rule's
   * refusal, and there are no grants to name to nobody.
   *
   * ONLY `unauthorized` IS A RULE'S REFUSAL. Every 403 used to be read as one,
   * with its code standing in for the rule, so a read refused
   * `seat_unavailable` or `csrf_origin` reached the banner as a refusal naming
   * no grant — which says "no grant would change that: the rule asks a
   * relation the org chart does not hold", a claim about neither.
   */
  get refusal(): QueryRefusal | LogRefusal | null {
    if (
      this.status === 403 &&
      this.code === "unauthorized" &&
      typeof this.body.reason === "string"
    ) {
      return { reason: this.body.reason, grants: this.grants };
    }
    const hint = this.retryHint;
    if (hint === null) return null;
    return {
      code: typeof this.body.refusal === "string" ? this.body.refusal : null,
      detail: this.detail || null,
      retryAfter: hint,
    };
  }

  /**
   * When the ENGINE said to ask again, in whole seconds, or null where it said
   * nothing: a `503` it wrote says its `Retry-After`, and ZERO where it sent
   * none — its statement that waiting will not change the answer. Every other
   * answer, a `503` something in front of the engine wrote included, carries
   * no hint, because nobody at the engine decided one.
   */
  get retryHint(): number | null {
    return this.status === 503 && !this.unanswered ? (this.retryAfterSeconds ?? 0) : null;
  }

  /**
   * Whether this is NOT the engine's own answer ([answeredByEngine]): nothing
   * came back (status 0), an answer without the engine's mark (a gateway's
   * page or its JSON, whatever the body says), a body that could not be read
   * (`unreadable_body`, a 200 cut off part way through), or a status with no
   * engine error code in it.
   *
   * WHAT A WRITE'S CALLER NEEDS BEFORE IT READS A REFUSAL AS ONE. A refusal
   * the engine wrote means nothing was done; this means nobody here knows. A
   * reverse proxy whose read timeout is shorter than the fifty seconds the
   * engine takes to answer a node gate (`engine.GateAnswerBudget`) hands a slow
   * eviction to the browser as a 504 — and read as a refusal, its operation id
   * was dropped with it.
   */
  get unanswered(): boolean {
    return !answeredByEngine(this.status, this.code, this.marked);
  }
}

/**
 * The grants a refusal on AUTHORITY named: any ONE of which would have
 * admitted the caller. Empty when the answer named none — a 401 (nobody was
 * recognised), or a rule that asks a relation no grant replaces.
 *
 * READ FROM THE ANSWER, because the engine's envelope carries them under
 * `grants` precisely so no screen has to name a grant itself: a sentence
 * typed into a screen is a second statement of the rule, and the one that
 * goes stale the day the rule's grant moves.
 */
export function refusedGrants(body: unknown): string[] {
  if (typeof body !== "object" || body === null) return [];
  const grants = (body as Record<string, unknown>).grants;
  return Array.isArray(grants) ? grants.filter((g): g is string => typeof g === "string") : [];
}

/**
 * The codes a `401` carries when it is an answer about WHAT WAS TYPED rather
 * than about the browser's credential: a sign-in whose details were not
 * accepted, and one whose password proved itself and now wants the second
 * factor. Every other `401` means this browser holds nothing the engine
 * accepts, and the session needs a sign-in.
 */
const TYPED_REFUSALS = new Set(["sign_in_refused", "second_factor_required"]);

/**
 * What a refusal says about the browser's SESSION, noted where every screen's
 * request passes — so no screen has to recognise a lost session itself, and
 * none can forget to.
 *
 * A `401` that is not an answer about typed details is a browser signed in as
 * nobody the engine accepts: never signed in, its session ended, expired or
 * revoked. A `403 second_factor_enrolment_required` is a session that may do
 * nothing but enrol the second factor the deployment requires. Every other
 * refusal is about the REQUEST, and the session is fine.
 */
function noteSession(refusal: RestError): void {
  if (refusal.status === 401 && !TYPED_REFUSALS.has(refusal.code)) needSession("sign_in");
  if (refusal.status === 403 && refusal.code === "second_factor_enrolment_required") {
    needSession("second_factor");
  }
}

/**
 * A `Retry-After` header value as whole seconds from `now`, or null for a
 * header that is absent or says nothing usable.
 *
 * BOTH FORMS RFC 9110 ALLOWS. The engine writes delay-seconds, but a proxy in
 * front of it may answer for it with an HTTP-date, and reading that as "no
 * hint" would drop the one instruction the refusal carried. A date already
 * past is a wait of zero, never a negative one.
 */
export function retryAfterSeconds(header: string | null | undefined, now: number): number | null {
  const value = header?.trim() ?? "";
  if (value === "") return null;
  if (/^\d+$/.test(value)) return Number(value);
  // AN HTTP-DATE NAMES ITS DAY OR MONTH IN LETTERS in every form the RFC
  // admits, and `Date.parse` alone would read "-4" as a year.
  if (!/[A-Za-z]/.test(value)) return null;
  const at = Date.parse(value);
  if (Number.isNaN(at)) return null;
  return Math.max(0, Math.ceil((at - now) / 1000));
}

/** The seconds a response's `Retry-After` names, or null for none. */
function retryAfterOf(response: Response): number | null {
  return retryAfterSeconds(response.headers.get("Retry-After"), Date.now());
}

/**
 * [RestError.retryHint] for a refused response that did not come through
 * [request] — the degraded-mode snapshot (`api.ts`) and the socket's
 * plain-HTTP re-ask of a refused handshake (`socket.ts`), each of which reads
 * the response itself. Consumes the body.
 */
export async function retryHintOf(response: Response): Promise<number | null> {
  const body = (await response.json().catch(() => null)) as unknown;
  const envelope =
    body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {};
  return new RestError(
    response.status,
    envelope,
    retryAfterOf(response),
    markedByEngine(response.headers),
  ).retryHint;
}

/**
 * A refusal that never reached the engine: DNS, a dropped connection, a proxy
 * answering HTML. Status 0, so a caller testing `status === 409` cannot
 * mistake it for an answer.
 */
function offline(err: unknown): RestError {
  return new RestError(0, {
    error: "unreachable",
    detail: err instanceof Error ? err.message : "the engine could not be reached",
  });
}

/**
 * How long a request may take before it is abandoned.
 *
 * A REQUEST THAT NEVER SETTLES NEVER SETTLES, and that is not a slow spinner
 * here: every write in this UI runs behind a `busy` flag whose only reset is
 * the `finally` of its own await, and every dialog disables its own exits
 * while busy — Escape, the veil click and the Cancel button. An unresolved
 * fetch was a modal with every way out switched off and a reload as the only
 * escape.
 *
 * ANCHORED TO THE LONGEST ORDINARY PATH THIS API HAS: a setup submission
 * seals a credential in the fleet's store, patches the company document,
 * validates it whole and advances the epoch, each a round trip of its own.
 * Thirty seconds is comfortably above that and comfortably below the point at
 * which a person concludes the page is broken.
 *
 * It is the DEFAULT, not the only deadline: a call whose path is genuinely
 * longer passes [RequestOptions.timeoutMs] rather than removing the deadline.
 * Two do. A backup copies the whole store before it answers. And the node
 * gate answers a gesture within `engine.GateAnswerBudget`, and a client waits
 * the longer `GATE_REQUEST_TIMEOUT_MS` for it (`contract/gate.ts`), so thirty
 * seconds gave up on a gesture the node went on to finish, holding nothing to
 * finish it with.
 */
export const REQUEST_TIMEOUT_MS = 30_000;

/**
 * [pathSegment] is the one rule of what a path segment may be, kept beside
 * the link builder it shares the check with (`lib/segment.ts`).
 */
export { pathSegment } from "../lib/segment.ts";

/** A query-string value. `undefined` leaves the parameter out. */
export type QueryValue = string | number | boolean | undefined;

export interface RequestOptions {
  /**
   * The request body. Encoded as JSON unless `contentType` names a type that
   * is not JSON, in which case it must already be a string or a `Blob` and is
   * sent byte for byte: `PUT /secrets/{name}` takes the credential itself, and
   * an encoding step would seal JSON quotes into it; an artifact's picture is
   * its bytes.
   */
  body?: unknown;
  /**
   * What the body is. Defaults to `application/json` when there is a body.
   * `application/merge-patch+json` is JSON too, and is encoded as such.
   */
  contentType?: string;
  headers?: Record<string, string>;
  /** Appended to the path's query string; `undefined` values are skipped. */
  query?: Record<string, QueryValue>;
  /**
   * The caller's own cancellation. An aborted request rejects with the
   * signal's reason (an `AbortError` unless the caller gave another), which
   * [isAbort] recognises: a request the caller superseded is not an engine
   * that could not be reached, and reporting it as one would put an
   * "unreachable" state on a screen whose only fault was being quick.
   */
  signal?: AbortSignal;
  /**
   * How a SUCCESSFUL body is read. `json` (the default) parses it; `text`
   * hands it over as the string the engine sent, for the one kind of answer
   * that is a file rather than a document: `GET /config?format=yaml`, which a
   * JSON parse turned into an `unreadable_body` refusal. A refusal is read as
   * JSON either way, because every refusal the engine writes is one.
   */
  read?: "json" | "text";
  /**
   * How long this request may take before it is abandoned, in milliseconds —
   * [REQUEST_TIMEOUT_MS] unless the caller's path is genuinely longer and
   * says so here, rather than removing the deadline; the refusal it gets on
   * expiry names ITS deadline, not the default's. Two paths are: `POST
   * /backup`, whose copy is synchronous and bounded by the size of the store
   * rather than by anything a screen decides, and the node gate
   * (`contract/gate.ts`).
   */
  timeoutMs?: number;
}

/** What the engine answered, whole: the status and entity-tag beside the body. */
export interface RestResponse {
  status: number;
  /**
   * The parsed JSON body, or null for an empty one (a 204, a 304). With
   * `read: "text"` a success's body is the text itself, empty included.
   */
  body: unknown;
  /**
   * The `ETag` header verbatim, quoted as the engine writes it, or null. The
   * config surface tags a document with its revision id, and `If-Match` takes
   * the tag back exactly as it was given.
   */
  etag: string | null;
  /** Whether the answer carried the engine's mark ([markedByEngine]). */
  marked: boolean;
}

/** A deadline as a person would say it: seconds under two minutes, else
 *  minutes. */
function waitWords(ms: number): string {
  const seconds = Math.round(ms / 1000);
  return seconds < 120 ? `${seconds} seconds` : `${Math.round(seconds / 60)} minutes`;
}

/** Whether a rejection is the caller's own abort rather than a failure. */
export function isAbort(err: unknown): boolean {
  return (
    typeof err === "object" && err !== null && (err as { name?: unknown }).name === "AbortError"
  );
}

/** JSON is `application/json` and every `+json` type, merge patch included. */
function isJson(contentType: string): boolean {
  const essence = contentType.split(";")[0]!.trim().toLowerCase();
  return essence === "application/json" || essence.endsWith("+json");
}

/** The path with the caller's query parameters merged into its own. */
function withQuery(path: string, query: Record<string, QueryValue> | undefined): string {
  if (!query) return path;
  const at = path.indexOf("?");
  const params = new URLSearchParams(at < 0 ? "" : path.slice(at + 1));
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) params.set(key, String(value));
  }
  const qs = params.toString();
  return (at < 0 ? path : path.slice(0, at)) + (qs ? `?${qs}` : "");
}

/**
 * The route a step-up is given at. Asking to confirm the confirmation would
 * be a dialog that reopens for ever, so a refusal there is the refusal.
 */
const STEP_UP_PATH = "/auth/step-up";

/**
 * `waiting`, or the caller's own abort if that comes first. A person can sit
 * at the confirmation for as long as they like, and a screen that gave up on
 * its request meanwhile must not be held to an answer it no longer wants.
 */
function unlessAborted<T>(waiting: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (!signal) return waiting;
  if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(signal.reason);
    signal.addEventListener("abort", abort, { once: true });
    waiting.then(
      (value) => {
        signal.removeEventListener("abort", abort);
        resolve(value);
      },
      (err: unknown) => {
        signal.removeEventListener("abort", abort);
        reject(err);
      },
    );
  });
}

/** How many requests are out right now, and who is waiting for none to be. */
let inFlight = 0;
let settled: (() => void)[] = [];

/**
 * Resolves once no request this module sent is still out — at once when none
 * is.
 *
 * WHAT A SOCKET CLOSED 4401 WAITS FOR before it dials again. A step-up
 * REPLACES the session it was made from, so the engine ends the old one and
 * closes every socket it opened; the answer that sets the new cookie may
 * still be on its way, and a dial before it lands carries the cookie the
 * engine just ended and is refused — which would send a person who had just
 * proved who they are to the sign-in form.
 */
export function whenRequestsSettle(): Promise<void> {
  if (inFlight === 0) return Promise.resolve();
  return new Promise((resolve) => settled.push(resolve));
}

/** The wall-clock instant, in ms, the engine last answered a request here. */
let answeredAt = 0;

/**
 * When the engine last answered any request this module sent, in ms since
 * the epoch, or 0 for never. Any answer to a request carrying the session
 * cookie re-issues it once it is five minutes old, so a tab that has heard
 * from the engine this recently needs no keepalive (`keepalive.ts`).
 */
export function lastAnsweredAt(): number {
  return answeredAt;
}

/**
 * The one request path, answering the status and entity-tag as well as the
 * body.
 *
 * NOT CACHED, EVER. Every answer here is either a write or a guarded read of
 * something that changes under the reader (a configuration revision, the
 * sealed store's names, an integration's requirements), and a heuristic cache
 * hit on one of those is a screen showing the company as it was. A 304 still
 * reaches the caller, when the caller sent the precondition that asks for it.
 *
 * # A step-up is confirmed HERE, and the refused request sent again
 *
 * A gesture refused `403 step_up_required` is asked of the person once —
 * through whatever confirms a step-up (`signin.ts`), however many requests
 * were refused together — and then REPLAYED: the same method, path, body and
 * headers, so a form that was being saved is saved, rather than lost to a
 * refusal its screen could only report. Once: a replay refused again is the
 * refusal. Every screen gets this by sending its writes through here, and no
 * screen implements it.
 *
 * The deadline is each ATTEMPT's, not the gesture's: the time a person spends
 * typing their password is not the engine taking too long.
 */
async function request(
  method: string,
  path: string,
  options: RequestOptions = {},
): Promise<RestResponse> {
  try {
    return await counted(method, path, options);
  } catch (err) {
    const refused =
      err instanceof RestError && err.status === 403 && err.code === "step_up_required";
    if (!refused || path.split("?")[0] === STEP_UP_PATH) throw err;
    if (!(await unlessAborted(confirmStepUp(), options.signal))) throw err;
    return counted(method, path, options);
  }
}

/** One attempt, counted in flight for [whenRequestsSettle]. */
async function counted(
  method: string,
  path: string,
  options: RequestOptions,
): Promise<RestResponse> {
  inFlight++;
  try {
    return await attempt(method, path, options);
  } finally {
    inFlight--;
    if (inFlight === 0) {
      const waiting = settled;
      settled = [];
      for (const resolve of waiting) resolve();
    }
  }
}

/** One round trip — see [request] for what surrounds it. */
async function attempt(
  method: string,
  path: string,
  options: RequestOptions,
): Promise<RestResponse> {
  const {
    body,
    headers = {},
    query,
    signal,
    read = "json",
    timeoutMs = REQUEST_TIMEOUT_MS,
  } = options;
  const contentType = options.contentType ?? (body === undefined ? undefined : "application/json");
  let encoded: string | Blob | undefined;
  if (body !== undefined) {
    if (contentType && !isJson(contentType)) {
      if (typeof body !== "string" && !(body instanceof Blob)) {
        throw new TypeError(`rest.request: a ${contentType} body must be a string or a Blob`);
      }
      encoded = body;
    } else {
      encoded = JSON.stringify(body);
    }
  }

  // Refused before a round trip: a caller that already gave up has no use
  // for an answer, and sending the request anyway could still write.
  if (signal?.aborted) throw signal.reason;

  const init: RequestInit = {
    method,
    cache: "no-store",
    // Stated rather than left to the default, because it is the whole of how
    // this request is authenticated: the session cookie, sent to this origin
    // and to no other.
    credentials: "same-origin",
    headers: {
      ...(contentType ? { "Content-Type": contentType } : {}),
      ...headers,
    },
    ...(encoded === undefined ? {} : { body: encoded }),
  };

  // ABORTED RATHER THAN AWAITED FOR EVER, see [REQUEST_TIMEOUT_MS]. The
  // deadline and the caller's signal abort ONE controller, because a fetch
  // takes one signal; which of them fired decides what the rejection says.
  //
  // BOTH STAY ARMED UNTIL THE BODY HAS BEEN READ, not only until the headers
  // arrive. A fetch resolves on the status line, and an engine that sends its
  // headers and then stalls, or a connection that drops half way through a
  // body, would otherwise leave a request no deadline and no caller could end.
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);
  const forward = () => controller.abort(signal?.reason);
  signal?.addEventListener("abort", forward, { once: true });

  // Why a request ended before it had an answer, as the one rejection a
  // caller can act on: its own abort, the deadline, or an engine it never
  // fully heard from.
  const unanswered = (err: unknown): unknown => {
    if (signal?.aborted) return signal.reason;
    return timedOut
      ? new RestError(0, {
          error: "unreachable",
          detail: `the engine did not answer within ${waitWords(timeoutMs)}`,
        })
      : offline(err);
  };

  let response: Response;
  let text: string;
  try {
    try {
      response = await fetch(location.origin + withQuery(path, query), {
        ...init,
        signal: controller.signal,
      });
    } catch (err) {
      throw unanswered(err);
    }
    // A 204, a 304 and a body-less 200 are all real answers, and each reads
    // as an empty string. A body that FAILS to read is not one of them: it is
    // a connection that dropped part way through, which says nothing about
    // what the engine did, so it is status 0 like any other request that was
    // never fully answered. Reading it as an empty body turned a write whose
    // outcome is unknown into a success with no body.
    try {
      text = await response.text();
    } catch (err) {
      throw unanswered(err);
    }
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", forward);
  }

  answeredAt = Date.now();

  if (read === "text" && response.ok) {
    return {
      status: response.status,
      body: text,
      etag: response.headers.get("ETag"),
      marked: markedByEngine(response.headers),
    };
  }

  let parsed: unknown = null;
  if (text !== "") {
    try {
      parsed = JSON.parse(text);
    } catch {
      // A proxy's HTML error page, or a body cut short. On a refusal that is
      // all the detail there is; on a success it is a broken answer either
      // way, so both become an error rather than a silent null — and one
      // that says it is not the engine's answer ([RestError.unanswered]),
      // since nothing in it says what the engine did. A refusal's
      // `Retry-After` still travels: a proxy answering a 503 page for an
      // engine that is restarting says when to come back in the header,
      // whatever its body is.
      throw new RestError(
        response.ok ? 502 : response.status,
        {
          error: "unreadable_body",
          detail: response.ok
            ? "the answer was cut short, or is not the engine's JSON"
            : `a ${response.status} came back without the engine's JSON, so something in front of it answered`,
        },
        response.ok ? null : retryAfterOf(response),
        markedByEngine(response.headers),
      );
    }
  }

  // 304 IS NOT A REFUSAL. It answers a conditional read whose precondition
  // the caller wrote, and it means the representation the caller holds is
  // still current.
  if (!response.ok && response.status !== 304) {
    const body = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
    const refusal = new RestError(
      response.status,
      body,
      retryAfterOf(response),
      markedByEngine(response.headers),
    );
    noteSession(refusal);
    throw refusal;
  }
  return {
    status: response.status,
    body: parsed,
    etag: response.headers.get("ETag"),
    marked: markedByEngine(response.headers),
  };
}

/** The body alone, for the callers that need nothing else. */
async function bodyOf(method: string, path: string, options?: RequestOptions): Promise<unknown> {
  return (await request(method, path, options)).body;
}

/**
 * How long a download may take, from its request to its last byte.
 *
 * FIFTEEN MINUTES, not [REQUEST_TIMEOUT_MS]: a project's largest file is a
 * gibibyte, and that is fourteen minutes at ten megabits a second. A deadline
 * sized for a JSON answer would abandon every large download part way.
 */
export const DOWNLOAD_TIMEOUT_MS = 15 * 60_000;

/**
 * A file's bytes, fetched as a blob over the session cookie.
 *
 * NOT A LINK. A plain `<a href>` would carry the cookie too, but its answer is
 * the browser's to handle: a refusal would be saved to disk as the file — the
 * engine's JSON envelope under the file's own name — and a lost session would
 * download a 401 rather than sending the reader to sign in. Fetched here, a
 * refusal is the engine's JSON read like every other ([RestError], with its
 * `Retry-After`), and a `401` reaches [noteSession] exactly as a request's
 * does.
 *
 * NOT COUNTED IN FLIGHT ([whenRequestsSettle]): a download may run for
 * [DOWNLOAD_TIMEOUT_MS], and a socket closed 4401 waits for the requests that
 * could be carrying a new cookie — a GET of a file sets none.
 */
async function blob(path: string, signal?: AbortSignal): Promise<Blob> {
  if (signal?.aborted) throw signal.reason;
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, DOWNLOAD_TIMEOUT_MS);
  const forward = () => controller.abort(signal?.reason);
  signal?.addEventListener("abort", forward, { once: true });
  // Why the download ended before its last byte: the caller's own abort, the
  // deadline, or an engine it never fully heard from.
  const unanswered = (err: unknown): unknown => {
    if (signal?.aborted) return signal.reason;
    return timedOut
      ? new RestError(0, {
          error: "unreachable",
          detail: `the download did not finish within ${waitWords(DOWNLOAD_TIMEOUT_MS)}`,
        })
      : offline(err);
  };
  try {
    let response: Response;
    try {
      response = await fetch(location.origin + path, {
        method: "GET",
        cache: "no-store",
        // The whole of how this download is authenticated, as for [request].
        credentials: "same-origin",
        signal: controller.signal,
      });
    } catch (err) {
      throw unanswered(err);
    }
    answeredAt = Date.now();
    if (!response.ok) {
      let body: Record<string, unknown>;
      try {
        body = (await response.json()) as Record<string, unknown>;
      } catch {
        body = { error: "unreadable_body" };
      }
      const refusal = new RestError(
        response.status,
        body,
        retryAfterOf(response),
        markedByEngine(response.headers),
      );
      noteSession(refusal);
      throw refusal;
    }
    try {
      return await response.blob();
    } catch (err) {
      // CUT SHORT. The engine sends a file's length before its bytes and
      // closes the connection when it cannot finish, so a body that fails
      // to read is a download that did not complete — never a smaller file.
      throw unanswered(err);
    }
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", forward);
  }
}

/** A text stream being read: the next chunk, or null once the engine ended it. */
export interface TextStream {
  next: () => Promise<string | null>;
  /**
   * The engine answered `204 No Content`: there is nothing to read and
   * nothing will follow, the event-stream answer that says not to dial again.
   */
  noContent: boolean;
}

/**
 * A long-lived text answer, read as it arrives: the Dashboard Assistant's
 * turn stream (`assistant.ts`), whose bytes are server-sent events.
 *
 * THE SAME CREDENTIAL AND THE SAME REFUSALS as every request here: the
 * session cookie and nothing else, no browser cache in front of it, and a
 * refusal read as the engine's JSON ([RestError]), a `401` reaching
 * [noteSession] exactly as a request's does. Only the STATUS LINE has a
 * deadline ([REQUEST_TIMEOUT_MS]); the body runs for as long as the engine
 * keeps it open, and the caller decides when a quiet stream has stalled,
 * since only the caller knows how often the engine promised to speak.
 *
 * NOT COUNTED IN FLIGHT ([whenRequestsSettle]), for a download's reason: a
 * stream may run for minutes, and it sets no cookie.
 */
async function stream(
  path: string,
  options: { headers?: Record<string, string>; signal?: AbortSignal } = {},
): Promise<TextStream> {
  const { headers = {}, signal } = options;
  if (signal?.aborted) throw signal.reason;
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, REQUEST_TIMEOUT_MS);
  const forward = () => controller.abort(signal?.reason);
  signal?.addEventListener("abort", forward, { once: true });
  const unanswered = (err: unknown): unknown => {
    if (signal?.aborted) return signal.reason;
    return timedOut
      ? new RestError(0, {
          error: "unreachable",
          detail: `the engine did not answer within ${waitWords(REQUEST_TIMEOUT_MS)}`,
        })
      : offline(err);
  };
  const release = () => signal?.removeEventListener("abort", forward);
  let response: Response;
  try {
    response = await fetch(location.origin + path, {
      method: "GET",
      cache: "no-store",
      // The whole of how this stream is authenticated, as for [request].
      credentials: "same-origin",
      headers: { Accept: "text/event-stream", ...headers },
      signal: controller.signal,
    });
  } catch (err) {
    release();
    throw unanswered(err);
  } finally {
    clearTimeout(timer);
  }
  answeredAt = Date.now();
  // NO CONTENT IS AN ANSWER, not an unreadable one: a browser hands a 204
  // no body, and reading it as a failure would dial it again.
  if (response.status === 204) {
    release();
    return { next: async () => null, noContent: true };
  }
  if (!response.ok || response.body === null) {
    let body: Record<string, unknown>;
    try {
      body = (await response.json()) as Record<string, unknown>;
    } catch {
      body = { error: "unreadable_body" };
    } finally {
      release();
    }
    const refusal = new RestError(
      response.ok ? 502 : response.status,
      body,
      retryAfterOf(response),
      markedByEngine(response.headers),
    );
    noteSession(refusal);
    throw refusal;
  }
  const reader = response.body.getReader();
  // AN ABORT ENDS A READ IN FLIGHT: a body whose connection a proxy dropped
  // without a word never errors, so a caller that judged the stream stalled
  // must be able to end the read it is waiting on.
  controller.signal.addEventListener(
    "abort",
    () => {
      reader.cancel(controller.signal.reason).catch(() => {});
    },
    { once: true },
  );
  const decoder = new TextDecoder();
  return {
    noContent: false,
    async next() {
      let chunk: ReadableStreamReadResult<Uint8Array>;
      try {
        chunk = await reader.read();
      } catch (err) {
        release();
        throw unanswered(err);
      }
      if (chunk.done) {
        release();
        const tail = decoder.decode();
        return tail === "" ? null : tail;
      }
      answeredAt = Date.now();
      return decoder.decode(chunk.value, { stream: true });
    },
  };
}

export const rest = {
  /**
   * The whole answer: status, entity-tag and body. For a caller that sends a
   * precondition, reads a tag, cancels, or branches on a success status.
   */
  request,
  /** A read's body. `signal` is the loader's: `lib/useRest.ts` aborts a read
   *  it superseded, and a screen reads through that rather than calling this. */
  get: (path: string, signal?: AbortSignal) => bodyOf("GET", path, { signal }),
  post: (path: string, body?: unknown, headers?: Record<string, string>) =>
    bodyOf("POST", path, { body: body ?? {}, headers }),
  put: (path: string, body?: unknown, headers?: Record<string, string>) =>
    bodyOf("PUT", path, { body: body ?? {}, headers }),
  patch: (path: string, body?: unknown, headers?: Record<string, string>) =>
    bodyOf("PATCH", path, { body: body ?? {}, headers }),
  /**
   * THE BODY IS THE VALUE, not a document carrying one.
   *
   * `PUT /secrets/{name}` takes the credential as raw bytes, deliberately: a
   * credential is arbitrary text — a PEM key has newlines, a token can hold
   * anything — and an encoding step between the operator and the byte
   * sequence the vendor compares is a 401 nobody can explain. Sending it
   * through `put` would seal the JSON quotes into the credential.
   */
  putText: (path: string, value: string) =>
    bodyOf("PUT", path, { body: value, contentType: "text/plain; charset=utf-8" }),
  /**
   * A file's bytes as the body, under the file's own type (or
   * `application/octet-stream` where the browser knows none): an artifact's
   * picture moving through the engine's transfer route.
   */
  putBytes: (path: string, value: Blob) =>
    bodyOf("PUT", path, { body: value, contentType: value.type || "application/octet-stream" }),
  // DELETE CARRIES A BODY HERE, which is unusual and deliberate: a
  // disconnect is not one act but a family of them, and which one it is —
  // whether the accounts go too, whether to stop waiting for a third-party
  // app that will never answer — are inputs to the deletion rather than
  // separate routes. Passing them as query parameters would put a destructive
  // choice in a proxy log.
  del: (path: string, body?: unknown, headers?: Record<string, string>) =>
    bodyOf("DELETE", path, { body, headers }),
  /** A file's bytes — see [blob]. */
  blob,
  /** A long-lived text answer, read as it arrives (see [stream]). */
  stream,
};
