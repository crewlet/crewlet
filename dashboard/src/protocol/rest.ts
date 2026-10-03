/**
 * The dashboard's one REST transport: every write, and the guarded reads the
 * socket has no question for.
 *
 * The socket remains the data channel for state. This is not a second one: it
 * carries the requests that are not questions about state at all. Writes never
 * go over the socket, deliberately: a write is judged on its `Origin` by the
 * engine's cross-site check before its handler runs, answers the three write
 * outcomes as statuses with an op id to retry by, and may be refused for a
 * step-up this module confirms and replays — none of which a frame on an open
 * socket carries. And a handful of reads exist only as REST, `GET /secrets`
 * above all, because no query in the registry answers them.
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

import { retryAfterMs, unansweredRetryMs } from "./retry.ts";
import { confirmStepUp, needSession, type StepUpWindow } from "./session.ts";
import type { QueryErrorCode } from "../contract/errors.ts";
import type { LogRefusal, QueryRefusal } from "./types.ts";

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
  /**
   * The `Retry-After` the answer carried, in whole seconds, or null for none —
   * either form the RFC allows ([retryAfterSeconds]). A `429` always carries
   * one and says how long the curve makes the next attempt wait; a `503` the
   * engine wrote carries one where waiting can clear the cause and none where
   * it cannot, which is a difference a screen has to render. A refusal
   * something in front of the engine wrote keeps its header here too, though
   * [retryHint] never reads it as the engine's.
   */
  readonly retryAfter: number | null;
  /** Everything else the body carried, for a caller that needs a field. */
  readonly body: Record<string, unknown>;

  constructor(status: number, body: Record<string, unknown>, retryAfter: number | null = null) {
    const code = typeof body.error === "string" ? body.error : "";
    const detail = typeof body.detail === "string" ? body.detail : "";
    super(detail || code || `HTTP ${status}`);
    this.name = "RestError";
    this.status = status;
    this.code = code;
    this.detail = detail;
    this.hint = typeof body.hint === "string" ? body.hint : "";
    this.sentence = typeof body.message === "string" ? body.message : "";
    this.retryAfter = retryAfter;
    this.body = body;
  }

  /**
   * Whether the engine refused on AUTHORITY rather than on the request.
   *
   * Both statuses, because a screen locks the same way for either and the
   * distinction is not one it can act on: 401 is "present a credential" and
   * 403 is "the one you presented does not carry this grant". What neither
   * is, any more, is a reason to send the reader to sign in again — a reader
   * holding a perfectly good session meets 403 the moment they open a screen
   * outside their grants, which is the ordinary case rather than the
   * exceptional one. Only a 401 says nobody is signed in (see [noteSession]).
   */
  get unauthorized(): boolean {
    return this.status === 401 || this.status === 403;
  }

  /** The grants a refusal on authority named — see [refusedGrants]. */
  get grants(): string[] {
    return refusedGrants(this.body);
  }

  /**
   * This refusal in the shape `QueryState` renders from — the one the
   * socket's error frame carries, under the same keys: a 403's rule and the
   * grants that would have admitted the caller, and a 503's state-log code,
   * its own words and whether waiting changes it (no `Retry-After` is the
   * engine saying it will not). Null for anything else, a 401 included:
   * nobody being signed in is not a rule's refusal, and there are no grants
   * to name to nobody.
   */
  get refusal(): QueryRefusal | LogRefusal | null {
    if (this.status === 403) {
      return {
        reason: typeof this.body.reason === "string" ? this.body.reason : this.code,
        grants: this.grants,
      };
    }
    const hint = this.retryHint;
    if (hint !== null) {
      return {
        code: typeof this.body.refusal === "string" ? this.body.refusal : null,
        detail: this.detail || null,
        retryAfter: hint,
      };
    }
    return null;
  }

  /**
   * When the engine said to ask again, in whole seconds, or null where it said
   * nothing: a `503` it wrote says its `Retry-After`, and ZERO where it sent
   * none — its statement that waiting will not change the answer. Every other
   * answer, a `503` something in front of the engine wrote included, carries
   * no hint, because nobody at the engine decided one.
   *
   * What a retry loop waits out (`retryAfterMs` in `retry.ts`), and what
   * [refusal] hands `QueryState` for a `503`: the rule for which `503` is the
   * engine's lives here and nowhere else.
   */
  get retryHint(): number | null {
    return this.status === 503 && !this.unanswered ? (this.retryAfter ?? 0) : null;
  }

  /**
   * Whether this is NOT the engine's own answer: nothing came back (status
   * 0), a body that could not be read (`unreadable_body` — a gateway's HTML
   * page, a 200 cut off part way through), or a status with no engine error
   * code in it. Every refusal the engine writes is JSON with an `error` code,
   * its auth guard's included and its router's own `no_route` and
   * `method_not_allowed` for a route or method it does not serve, so an answer
   * without one was written by something in front of it.
   *
   * WHAT A WRITE'S CALLER NEEDS BEFORE IT READS A REFUSAL AS ONE. A refusal
   * the engine wrote means nothing was done; this means nobody here knows. A
   * reverse proxy's default read timeout is a minute, which is exactly what
   * the engine allows a node gate past its judgement, so a slow eviction
   * reached the browser as a 504 — and read as a refusal, its operation id
   * was dropped with it.
   */
  get unanswered(): boolean {
    return this.status === 0 || this.code === "" || this.code === "unreadable_body";
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
 * goes stale the day the rule's grant moves. Takes the raw body rather than
 * only a [RestError] because the org builder reads `GET /config` through its
 * own answer type.
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

/** What [restRetryMs] needs to know beyond the failure itself. */
export interface RetryContext {
  /**
   * The screen's own cadence for the answer it holds, in milliseconds, or
   * `null` where it has none: what a failure the engine gave no hint for
   * waits, as the poll it replaces would have.
   */
  readonly cadence: number | null;
  /**
   * How many reads IN A ROW, this one included, nobody answered
   * ([RestError.unanswered]) — what the backoff is counted from. Any answer
   * the engine wrote, a refusal included, starts the count again.
   */
  readonly unanswered: number;
}

/**
 * When a REST read that failed with `err` is asked again, in milliseconds, or
 * `null` for "not on a timer" — the REST twin of the socket's
 * `unavailableRetryMs`, for a screen that reads over REST and asks again on
 * its own. Decided on the code [restFailure] draws the failure as, so the
 * banner and the timer can never disagree about which failure this is.
 *
 * - A `503` the engine wrote says when ([RestError.retryHint]), read through
 *   `retryAfterMs`: waited out exactly, bounded, and its ZERO — a `503` with
 *   no `Retry-After` — never on a timer, because the engine is saying waiting
 *   will not change the answer.
 * - A read NOBODY ANSWERED backs off (`unansweredRetryMs`), from a second to
 *   thirty. It is the one failure with nothing that would ever ask again
 *   otherwise: the live socket can be up the whole time — a request past its
 *   deadline on a slow engine, one dropped on the way — so its coming back
 *   never happens, and a screen with no poll of its own held the banner until
 *   somebody reloaded. A `Retry-After` something in front of the engine wrote
 *   — a proxy's `503` page — is not waited out: nobody at the engine decided
 *   it, and the backoff is already this tab's whole answer to a node it
 *   cannot hear.
 * - Every other failure carries no hint, since nobody at the engine decided
 *   one, and waits the screen's own `cadence`.
 */
export function restRetryMs(err: unknown, context: RetryContext): number | null {
  switch (restFailure(err).error) {
    case "unavailable":
      return retryAfterMs((err as RestError).retryHint ?? 0);
    case "unanswered":
      return unansweredRetryMs(context.unanswered);
    default:
      return context.cadence;
  }
}

/**
 * What a failed READ is drawn as: every code a socket question fails with, and
 * the one a REST read adds — `unanswered`, a request no answer from the engine
 * came back to ([RestError.unanswered]).
 *
 * NOT A MEMBER OF `QueryErrorCode`, which is the socket's vocabulary: a Go
 * test in `internal/api/stream` pins that union to the codes the engine sends
 * plus the two the socket mints itself, and a REST read is not a socket
 * question. `QueryState`'s banner table is keyed on this wider union, so a
 * code here with no sentence is a compile error exactly as one there is.
 */
export type ReadErrorCode = QueryErrorCode | "unanswered";

/** A failed REST read as the pair `QueryState` renders — see [restFailure]. */
export interface RestFailure {
  readonly error: ReadErrorCode;
  readonly refusal: QueryRefusal | LogRefusal | null;
}

/**
 * A failed REST read in `QueryState`'s terms: the code its banner is chosen
 * by, and the refusal that lets the banner say what would change the answer.
 *
 * THE REST TWIN OF `queryFailure`, for a screen that reads over REST and draws
 * its failure the way a socket question's is drawn. Each such screen used to
 * map a failure for itself, and each forgot a different case: the credential
 * listing drew an engine `503` as a fault on the node, the Integrations
 * listing drew nothing for any failure but a refusal and a `503`, and the pass
 * history drew a first read that failed as "No pass has run on this node" — an
 * answer about the integration that nobody gave.
 *
 * - A refusal on AUTHORITY (`401`, `403`) is `unauthorized`, carrying the rule
 *   and the grants it named.
 * - A `503` the engine wrote ([RestError.retryHint]) is `unavailable`, with
 *   its state-log code and hint: the banner says the screen asks again on its
 *   own, or — at zero — that asking will not change it.
 * - A read NO ANSWER FROM THE ENGINE CAME BACK TO ([RestError.unanswered]) is
 *   `unanswered`: status 0 — a request past its thirty-second deadline, one
 *   dropped on the way — or a status something in front of the engine wrote
 *   (a gateway's `502` or `504`, a body cut off part way). Nothing refused it
 *   and nothing here knows what the engine would have said, and its banner
 *   says the screen asks again on its own, which [restRetryMs]'s backoff
 *   makes true. It was `closed`, whose banner says the SOCKET went away and
 *   the screen reads again once it is back: while the socket stayed up — the
 *   ordinary case for one slow request — neither was true, and the screen
 *   read again only on a reload. And a gateway's answer was `query_failed`,
 *   "the engine tried to answer and failed", about an answer the engine never
 *   wrote.
 * - Anything else is `query_failed`, a fault on the node: a `500` it wrote.
 *
 * A `404` is the CALLER'S to read first, because what it means is the route's
 * — the credential surface unregistered on this process, or one pass nobody
 * remembers — and reading it here would say one of those about the other.
 */
export function restFailure(err: unknown): RestFailure {
  if (!(err instanceof RestError)) return { error: "query_failed", refusal: null };
  if (err.unauthorized) return { error: "unauthorized", refusal: err.refusal };
  if (err.retryHint !== null) return { error: "unavailable", refusal: err.refusal };
  if (err.unanswered) return { error: "unanswered", refusal: null };
  return { error: "query_failed", refusal: null };
}

/**
 * A `Retry-After` header value as whole seconds from `now`, or null for a
 * header that is absent or says nothing usable.
 *
 * BOTH FORMS RFC 9110 ALLOWS. The engine writes delay-seconds, but a proxy in
 * front of it may answer for it with an HTTP-date, and reading that as "no
 * hint" would drop the one instruction the refusal carried. A date already
 * past is a wait of zero, never a negative one. Which of the two WROTE it —
 * and so whether a zero means "waiting will not change it" — is not this
 * function's question: that is [RestError.retryHint]'s rule.
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
 * the response itself.
 *
 * ONE READING for both, because each needs the same three steps — the body
 * read as an envelope whatever it holds, the header read as whole seconds, and
 * the rule for which `503` is the engine's — and each spelled them out for
 * itself, which is how two copies come to disagree about a proxy's `503`.
 * Consumes the body.
 */
export async function retryHintOf(response: Response): Promise<number | null> {
  const body = (await response.json().catch(() => null)) as unknown;
  const envelope =
    body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {};
  return new RestError(response.status, envelope, retryAfterOf(response)).retryHint;
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
 * gate is allowed two minutes from its first record to its last answer
 * (`contract/gate.ts`), so thirty seconds gave up on a gesture the node went
 * on to finish, holding nothing to finish it with.
 */
export const REQUEST_TIMEOUT_MS = 30_000;

/** A query-string value. `undefined` leaves the parameter out. */
export type QueryValue = string | number | boolean | undefined;

export interface RequestOptions {
  /**
   * The request body. Encoded as JSON unless `contentType` names a type that
   * is not JSON, in which case it must already be a string and is sent
   * byte for byte: `PUT /secrets/{name}` takes the credential itself, and an
   * encoding step would seal JSON quotes into it.
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
   * How long this request may take before it is abandoned, in milliseconds —
   * [REQUEST_TIMEOUT_MS] unless the caller's path is genuinely longer and
   * says so here, rather than removing the deadline; the refusal it gets on
   * expiry names ITS deadline, not the default's. Two paths are: `POST
   * /backup`, whose copy is synchronous and bounded by the size of the store
   * rather than by anything a screen decides, and the node gate.
   */
  timeoutMs?: number;
  /**
   * How a SUCCESSFUL body is read. `json` (the default) parses it; `text`
   * hands it over as the string the engine sent, for the one kind of answer
   * that is a file rather than a document: `GET /config?format=yaml`, which a
   * JSON parse turned into an `unreadable_body` refusal. A refusal is read as
   * JSON either way, because every refusal the engine writes is one.
   */
  read?: "json" | "text";
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
}

/**
 * A deadline as a person would say it: in minutes where it is a whole number
 * of them past the first, and in seconds otherwise — EXACT either way, because
 * the sentence names the deadline that ran out, and the node gate's two
 * minutes and fifteen seconds rounded to "2 minutes" would be a deadline
 * nobody set.
 */
function waitWords(ms: number): string {
  const seconds = Math.round(ms / 1000);
  return seconds >= 120 && seconds % 60 === 0 ? `${seconds / 60} minutes` : `${seconds} seconds`;
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
 * The route a step-up is given at. It answers `step_up_required` itself when
 * the caller is a credential nobody present can confirm — and asking to
 * confirm the confirmation would be a dialog that reopens for ever.
 */
const STEP_UP_PATH = "/auth/step-up";

/** Which window a step-up refusal names, from the envelope's own key. */
function windowOf(refusal: RestError): StepUpWindow {
  const window = refusal.body.window;
  return typeof window === "string" && window !== "" ? window : "step_up";
}

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
 * through whatever confirms a step-up (`session.ts`), however many requests
 * were refused together — and then REPLAYED: the same method, path, body and
 * headers, so a form that was being saved is saved, rather than lost to a
 * refusal its screen could only report. Once: a replay refused again is the
 * refusal. Every screen gets this by sending its writes through here, and no
 * screen implements it, which is what makes it one ceremony rather than a
 * dozen that disagree.
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
    return await attempt(method, path, options);
  } catch (err) {
    const refused =
      err instanceof RestError && err.status === 403 && err.code === "step_up_required";
    if (!refused || path.split("?")[0] === STEP_UP_PATH) throw err;
    if (!(await unlessAborted(confirmStepUp(windowOf(err)), options.signal))) throw err;
    return attempt(method, path, options);
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
  let encoded: string | undefined;
  if (body !== undefined) {
    if (contentType && !isJson(contentType)) {
      if (typeof body !== "string") {
        throw new TypeError(`rest.request: a ${contentType} body must be a string`);
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

  if (read === "text" && response.ok) {
    return { status: response.status, body: text, etag: response.headers.get("ETag") };
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
      // `Retry-After` still travels on the error, as what was said — a
      // proxy answering a 503 page for an engine that is restarting says
      // when to come back in the header, whatever its body is — though
      // [RestError.retryHint] never reads it as the engine's.
      throw new RestError(
        response.ok ? 502 : response.status,
        {
          error: "unreadable_body",
          detail: response.ok
            ? "the answer was cut short, or is not the engine's JSON"
            : `a ${response.status} came back that is not the engine's JSON — something in front of it answered`,
        },
        response.ok ? null : retryAfterOf(response),
      );
    }
  }

  // 304 IS NOT A REFUSAL. It answers a conditional read whose precondition
  // the caller wrote, and it means the representation the caller holds is
  // still current.
  if (!response.ok && response.status !== 304) {
    const body = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
    const refusal = new RestError(response.status, body, retryAfterOf(response));
    noteSession(refusal);
    throw refusal;
  }
  return { status: response.status, body: parsed, etag: response.headers.get("ETag") };
}

/** The body alone, for the callers that need nothing else. */
async function bodyOf(method: string, path: string, options?: RequestOptions): Promise<unknown> {
  return (await request(method, path, options)).body;
}

export const rest = {
  /**
   * The whole answer: status, entity-tag and body. For a caller that sends a
   * precondition, reads a tag, cancels, or branches on a success status.
   */
  request,
  /**
   * A read, ended by `signal` where the caller passes one: a read whose
   * screen went, or whose answer a newer read superseded, has nobody left to
   * hand its answer to (`lib/restRead.ts`).
   */
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
  // DELETE CARRIES A BODY HERE, which is unusual and deliberate: a
  // disconnect is not one act but a family of them, and which one it is —
  // whether the accounts go too, whether to stop waiting for a third-party
  // app that will never answer — are inputs to the deletion rather than
  // separate routes. Passing them as query parameters would put a destructive
  // choice in a proxy log.
  del: (path: string, body?: unknown, headers?: Record<string, string>) =>
    bodyOf("DELETE", path, { body, headers }),
};
