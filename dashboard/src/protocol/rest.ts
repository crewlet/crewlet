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

async function send(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<unknown> {
  const token = apiToken();
  const init: RequestInit = {
    method,
    headers: {
      ...(token ? { Authorization: "Bearer " + token } : {}),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      ...headers,
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  };

  let response: Response;
  try {
    response = await fetch(location.origin + path, init);
  } catch (err) {
    throw offline(err);
  }

  // A 204 and a body-less 200 are both real answers. Reading them as JSON
  // would turn a success into a parse failure.
  const text = await response.text().catch(() => "");
  let parsed: unknown = null;
  if (text !== "") {
    try {
      parsed = JSON.parse(text);
    } catch {
      // A proxy's HTML error page, or a truncated body. On a refusal that is
      // all the detail there is; on a success it is a broken answer either
      // way, so both become an error rather than a silent null.
      throw new RestError(response.ok ? 502 : response.status, {
        error: "unreadable_body",
        detail: "the engine answered something that is not JSON",
      });
    }
  }

  if (!response.ok) {
    const body = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
    throw new RestError(response.status, body);
  }
  return parsed;
}

export const rest = {
  get: (path: string) => send("GET", path),
  post: (path: string, body?: unknown, headers?: Record<string, string>) =>
    send("POST", path, body ?? {}, headers),
  put: (path: string, body?: unknown, headers?: Record<string, string>) =>
    send("PUT", path, body ?? {}, headers),
  patch: (path: string, body?: unknown, headers?: Record<string, string>) =>
    send("PATCH", path, body ?? {}, headers),
  // DELETE CARRIES A BODY HERE, which is unusual and deliberate: a
  // disconnect is not one act but a family of them, and which one it is —
  // whether the accounts go too, whether to stop waiting for a vendor that
  // will never answer — are inputs to the deletion rather than separate
  // routes. Passing them as query parameters would put a destructive choice
  // in a proxy log.
  del: (path: string, body?: unknown, headers?: Record<string, string>) =>
    send("DELETE", path, body, headers),
};
