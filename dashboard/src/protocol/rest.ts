/**
 * The dashboard's one REST transport: every write, and the guarded reads the
 * socket has no question for.
 *
 * The socket remains the data channel for state. This is not a second one: it
 * carries the requests that are not questions about state at all. Writes never
 * go over the socket, deliberately — its token rides the query string on the
 * handshake, and a channel whose credential appears in a proxy log is not
 * where a credential-bearing write belongs (see internal/api/auth's own note
 * on that). And a handful of reads exist only as REST, `GET /secrets` above
 * all, because no query in the registry answers them.
 *
 * ONE MODULE, for the reason `api.ts` states about itself: a screen reaching
 * for its own transport takes its client from somewhere, and the somewhere the
 * Fleet screen chose was a context field the shell never populated, so it
 * shipped dead. Everything here is a plain function over `fetch` against
 * `location.origin`, which is where the dashboard is served from and the only
 * origin the engine answers on (it writes no CORS header at all).
 *
 * Every call carries the operator bearer token. The engine guards `/config`,
 * `/secrets` and `/setup` in full, reads included, whatever the anonymous-read
 * posture is, so a call with no token is refused rather than silently served.
 */

import { apiToken } from "./authToken.ts";

/**
 * What the engine said when it refused.
 *
 * `status` alone is not enough to act on: the setup surface distinguishes a
 * revision that moved under the caller from a config slot holding a literal
 * from a fleet with no keyring, and all three are a 409 or a 503. The engine
 * answers those with a `code`, and the screen branches on it.
 */
export class RestError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string;
  readonly hint: string;
  /** Everything else the body carried, for a caller that needs a field. */
  readonly body: Record<string, unknown>;

  constructor(status: number, body: Record<string, unknown>) {
    const code = typeof body.error === "string" ? body.error : "";
    const detail = typeof body.detail === "string" ? body.detail : "";
    super(detail || code || `HTTP ${status}`);
    this.name = "RestError";
    this.status = status;
    this.code = code;
    this.detail = detail;
    this.hint = typeof body.hint === "string" ? body.hint : "";
    this.body = body;
  }

  /** Whether the engine refused the credential rather than the request. */
  get unauthorized(): boolean {
    return this.status === 401 || this.status === 403;
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
 * The node gate is the one that does — the engine allows a gesture a minute
 * from its first record to its last answer, so thirty seconds gave up on a
 * gesture the node went on to finish, holding nothing to finish it with.
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
   * How long this request may take before it is abandoned, in milliseconds.
   * Defaults to [REQUEST_TIMEOUT_MS]; a caller whose path is genuinely longer
   * says so here, and the refusal it gets on expiry names ITS deadline.
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
 * The one request path, answering the status and entity-tag as well as the
 * body.
 *
 * NOT CACHED, EVER. Every answer here is either a write or a guarded read of
 * something that changes under the reader (a configuration revision, the
 * sealed store's names, an integration's requirements), and a heuristic cache
 * hit on one of those is a screen showing the company as it was. A 304 still
 * reaches the caller, when the caller sent the precondition that asks for it.
 */
async function request(
  method: string,
  path: string,
  options: RequestOptions = {},
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

  const token = apiToken();
  const init: RequestInit = {
    method,
    cache: "no-store",
    headers: {
      ...(token ? { Authorization: "Bearer " + token } : {}),
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
          detail: `the engine did not answer within ${timeoutMs / 1000} seconds`,
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
      // since nothing in it says what the engine did.
      throw new RestError(response.ok ? 502 : response.status, {
        error: "unreadable_body",
        detail: response.ok
          ? "the answer was cut short, or is not the engine's JSON"
          : `a ${response.status} came back that is not the engine's JSON — something in front of it answered`,
      });
    }
  }

  // 304 IS NOT A REFUSAL. It answers a conditional read whose precondition
  // the caller wrote, and it means the representation the caller holds is
  // still current.
  if (!response.ok && response.status !== 304) {
    const refusal = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
    throw new RestError(response.status, refusal);
  }
  return { status: response.status, body: parsed, etag: response.headers.get("ETag") };
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
 * A file's bytes, fetched with the operator's token.
 *
 * NOT A LINK. A plain `<a href>` carries no bearer token, so on an engine that
 * guards its reads it would download the refusal instead of the file — the
 * bytes are fetched here, where the token is, and handed to the caller as a
 * blob. A refusal is read as the engine's JSON, like every other.
 */
async function blob(path: string, signal?: AbortSignal): Promise<Blob> {
  const token = apiToken();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), DOWNLOAD_TIMEOUT_MS);
  const forward = () => controller.abort(signal?.reason);
  signal?.addEventListener("abort", forward, { once: true });
  try {
    let response: Response;
    try {
      response = await fetch(location.origin + path, {
        method: "GET",
        cache: "no-store",
        headers: token ? { Authorization: "Bearer " + token } : {},
        signal: controller.signal,
      });
    } catch (err) {
      if (signal?.aborted) throw signal.reason;
      throw offline(err);
    }
    if (!response.ok) {
      let body: Record<string, unknown> = {};
      try {
        body = (await response.json()) as Record<string, unknown>;
      } catch {
        body = { error: "unreadable_body" };
      }
      throw new RestError(response.status, body);
    }
    try {
      return await response.blob();
    } catch (err) {
      // CUT SHORT. The engine sends a file's length before its bytes and
      // closes the connection when it cannot finish, so a body that fails
      // to read is a download that did not complete — never a smaller file.
      throw offline(err);
    }
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", forward);
  }
}

export const rest = {
  /**
   * The whole answer: status, entity-tag and body. For a caller that sends a
   * precondition, reads a tag, cancels, or branches on a success status.
   */
  request,
  get: (path: string) => bodyOf("GET", path),
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
  /** A file's bytes — see [blob]. */
  blob,
};
