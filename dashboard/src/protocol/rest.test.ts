/**
 * The REST transport's own contract.
 */

import { afterEach, describe, expect, test, vi } from "vitest";

import { isAbort, REQUEST_TIMEOUT_MS, rest, RestError, retryAfterSeconds } from "./index.ts";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** What a call sent, and a fetch that answers with `respond`. */
function stub(respond: (url: string, init: RequestInit) => Response) {
  const sent: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      return respond(url, init);
    }),
  );
  return sent;
}

const json = (payload: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

// A REQUEST THAT NEVER SETTLES IS ABANDONED.
//
// There was no deadline, and an unresolved fetch is not a slow spinner here:
// every write runs behind a `busy` flag whose only reset is the `finally` of
// its own await, and every dialog disables Escape, the veil click and its own
// Cancel button while busy. The modal had every exit switched off and a
// reload as the only way out.
test("a request that never answers is abandoned rather than awaited", async () => {
  vi.useFakeTimers();
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (_url: string, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        }),
    ),
  );

  const pending = rest.get("/config");
  const settled = pending.catch((err: unknown) => err);
  await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS + 1);

  const err = await settled;
  expect(err).toBeInstanceOf(RestError);
  // STATUS 0, which is what every caller already tests for as "unreachable":
  // a request this process gave up on and one the engine never answered are
  // the same fact to somebody looking at the screen.
  expect((err as RestError).status).toBe(0);
  expect(isAbort(err)).toBe(false);
  vi.useRealTimers();
});

describe("the whole answer", () => {
  // THE TAG IS THE WRITE'S PRECONDITION. The config surface stamps a document
  // with its revision as an entity-tag, and a transport that returned only
  // bodies left every caller reading a DIFFERENT resource to learn the id an
  // If-Match needs.
  test("status, body and the entity-tag verbatim", async () => {
    stub(() => json({ name: "Acme" }, 200, { ETag: '"01JREV"' }));
    const answer = await rest.request("GET", "/config");
    expect(answer).toEqual({ status: 200, body: { name: "Acme" }, etag: '"01JREV"' });
  });

  test("a success status other than 200 is reported, not flattened", async () => {
    stub(() => json({ revision_id: "01JNEW", epoch: 7 }, 201));
    const answer = await rest.request("PUT", "/config", { body: { name: "Acme" } });
    expect(answer.status).toBe(201);
    expect(answer.etag).toBeNull();
  });

  // 304 IS NOT A REFUSAL. It is the engine saying the revision the caller
  // holds is current, in answer to a precondition the caller wrote.
  test("a 304 resolves with no body", async () => {
    const sent = stub(() => new Response(null, { status: 304, headers: { ETag: '"01JREV"' } }));
    const answer = await rest.request("GET", "/config", {
      headers: { "If-None-Match": '"01JREV"' },
    });
    expect(answer).toEqual({ status: 304, body: null, etag: '"01JREV"' });
    expect((sent[0]!.init.headers as Record<string, string>)["If-None-Match"]).toBe('"01JREV"');
  });

  test("a refusal still throws, with the engine's code and the whole body", async () => {
    stub(() => json({ error: "revision_advanced", current_revision_id: "01JB" }, 409));
    const err = await rest.request("PATCH", "/config", { body: {} }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).status).toBe(409);
    expect((err as RestError).code).toBe("revision_advanced");
    expect((err as RestError).body.current_revision_id).toBe("01JB");
  });

  // A GUARDED READ IS NEVER A CACHE HIT. A heuristic cache answering GET
  // /config from memory is a screen showing the company as it was.
  test("nothing is served from the browser's cache", async () => {
    const sent = stub(() => json({}));
    await rest.get("/secrets");
    await rest.request("GET", "/config");
    expect(sent.map((s) => s.init.cache)).toEqual(["no-store", "no-store"]);
  });
});

describe("a text answer", () => {
  // A FILE IS NOT A DOCUMENT. `GET /config?format=yaml` answers the company
  // as YAML, and parsing it as JSON turned the export into an
  // `unreadable_body` refusal.
  test("a success is handed over as the text the engine sent", async () => {
    stub(
      () =>
        new Response("name: Acme\n", {
          status: 200,
          headers: { "Content-Type": "application/yaml", ETag: '"01JREV"' },
        }),
    );
    const answer = await rest.request("GET", "/config", {
      query: { format: "yaml" },
      read: "text",
    });
    expect(answer).toEqual({ status: 200, body: "name: Acme\n", etag: '"01JREV"' });
  });

  test("a refusal is still read as the engine's JSON", async () => {
    stub(() => json({ error: "invalid_token", detail: "set a token" }, 401));
    const err = await rest.request("GET", "/config", { read: "text" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).code).toBe("invalid_token");
  });
});

describe("what a request sends", () => {
  test("query parameters are merged into the path's own", async () => {
    const sent = stub(() => json({}));
    await rest.request("PUT", "/config?format=json", {
      body: {},
      query: { dry_run: true, skipped: undefined },
    });
    expect(new URL(sent[0]!.url).pathname).toBe("/config");
    expect(new URL(sent[0]!.url).search).toBe("?format=json&dry_run=true");
  });

  test("a merge patch is JSON, sent under its own type", async () => {
    const sent = stub(() => json({}, 201));
    await rest.request("PATCH", "/config", {
      body: { name: "Acme", units: null },
      contentType: "application/merge-patch+json",
    });
    const headers = sent[0]!.init.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(sent[0]!.init.body).toBe('{"name":"Acme","units":null}');
  });

  test("a body that is not JSON is sent byte for byte", async () => {
    const sent = stub(() => new Response(null, { status: 204 }));
    await rest.putText("/secrets/GITHUB_TOKEN", 'line one\n"quoted"');
    expect(sent[0]!.init.body).toBe('line one\n"quoted"');
    // And a non-string body under a non-JSON type is a caller's bug, refused
    // before it can seal "[object Object]" into a credential.
    await expect(
      rest.request("PUT", "/secrets/X", { body: { value: 1 }, contentType: "text/plain" }),
    ).rejects.toThrow(TypeError);
  });
});

describe("cancellation", () => {
  // A SUPERSEDED REQUEST IS NOT AN UNREACHABLE ENGINE. A dry run aborted
  // because the draft moved on would otherwise paint "could not reach the
  // engine" over a screen whose only fault was being quick.
  test("the caller's abort rejects as an abort, not as status 0", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        (_url: string, init?: RequestInit) =>
          new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener("abort", () =>
              reject(new DOMException("aborted", "AbortError")),
            );
          }),
      ),
    );
    const controller = new AbortController();
    const pending = rest.request("PUT", "/config", { body: {}, signal: controller.signal });
    controller.abort();
    const err = await pending.catch((e: unknown) => e);
    expect(isAbort(err)).toBe(true);
    expect(err).not.toBeInstanceOf(RestError);
  });

  test("an already aborted signal sends nothing at all", async () => {
    const sent = stub(() => json({}));
    const controller = new AbortController();
    controller.abort();
    const err = await rest
      .request("PATCH", "/config", { body: {}, signal: controller.signal })
      .catch((e: unknown) => e);
    expect(isAbort(err)).toBe(true);
    expect(sent).toEqual([]);
  });
});

// THE HEADERS ARE NOT THE ANSWER. A fetch resolves on the status line, and the
// body is read after it; everything that ends a request has to reach that
// second half too, or a body that stalls or breaks is a request nothing ends
// and nothing reports.
describe("a body that never arrives whole", () => {
  /**
   * A fetch that answers `status` at once and then streams a body that
   * `stall` decides the fate of. The stream errors when the request's own
   * signal aborts, as a browser's does.
   */
  function headersThen(status: number, stall: (body: ReadableStreamDefaultController) => void) {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async (_url: string, init?: RequestInit) =>
          new Response(
            new ReadableStream({
              start(body) {
                body.enqueue(new TextEncoder().encode('{"revision_id":'));
                init?.signal?.addEventListener("abort", () =>
                  body.error(new DOMException("aborted", "AbortError")),
                );
                stall(body);
              },
            }),
            { status },
          ),
      ),
    );
  }

  // A WRITE WHOSE BODY BROKE IS A WRITE WHOSE OUTCOME IS UNKNOWN. Read as an
  // empty body it was a 201 with nothing in it, which a caller takes as done.
  test("a connection that drops part way through is status 0, not an empty success", async () => {
    headersThen(201, (body) => body.error(new TypeError("network error")));
    const err = await rest
      .request("PUT", "/config", { body: {} })
      .catch((e: unknown) => e as unknown);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).status).toBe(0);
    expect((err as RestError).code).toBe("unreachable");
  });

  test("the caller's abort still ends a request whose body is slow", async () => {
    headersThen(200, () => {});
    const controller = new AbortController();
    const pending = rest
      .request("GET", "/config", { signal: controller.signal })
      .catch((e: unknown) => e);
    // Let the headers land, so the abort arrives while the body is read.
    await new Promise((resolve) => setTimeout(resolve, 0));
    controller.abort();
    expect(isAbort(await pending)).toBe(true);
  });

  test("the deadline still ends a request whose body stalls", async () => {
    vi.useFakeTimers();
    headersThen(200, () => {});
    const settled = rest.get("/config").catch((e: unknown) => e);
    await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS + 1);
    const err = await settled;
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).status).toBe(0);
    expect((err as RestError).detail).toContain("did not answer");
  });
});

// THE REFUSAL CARRIES THE ENGINE'S WAIT.
//
// A 503 is two different answers — a node catching up or draining, which a
// wait clears and which says how long, and a node with no keyring, which no
// wait clears and which says nothing — and the header is the only thing that
// tells a loader which of the two it holds.
describe("Retry-After", () => {
  test("a 503 with Retry-After carries the wait", async () => {
    stub(() => json({ error: "unavailable" }, 503, { "Retry-After": "7" }));
    const err = await rest.get("/secrets").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestError);
    expect((err as RestError).retryAfterSeconds).toBe(7);
  });

  test("a 503 without one carries none", async () => {
    stub(() => json({ error: "no_keyring" }, 503));
    const err = await rest.get("/secrets").catch((e: unknown) => e);
    expect((err as RestError).retryAfterSeconds).toBeNull();
  });

  test("a proxy's HTML 503 still carries its wait", async () => {
    stub(() => new Response("<html>down</html>", { status: 503, headers: { "Retry-After": "3" } }));
    const err = await rest.get("/secrets").catch((e: unknown) => e);
    expect((err as RestError).code).toBe("unreadable_body");
    expect((err as RestError).retryAfterSeconds).toBe(3);
  });

  test.each([
    ["12", 12],
    [" 0 ", 0],
    ["", null],
    [null, null],
    ["soon", null],
    ["-4", null],
    ["Thu, 01 Jan 2026 00:00:30 GMT", 30],
    ["Wed, 31 Dec 2025 23:59:00 GMT", 0],
  ] as const)("the header %j reads as %j seconds", (header, want) => {
    expect(retryAfterSeconds(header, Date.parse("2026-01-01T00:00:00Z"))).toBe(want);
  });
});
