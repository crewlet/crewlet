/**
 * The REST transport's own contract.
 */

import { afterEach, describe, expect, test, vi } from "vitest";

import { GATE_REQUEST_TIMEOUT_MS } from "../contract/gate.ts";
import { lineOf, modules, parse, stringValue, walk, type Node } from "../test/source.ts";
import {
  isAbort,
  pathSegment,
  REQUEST_TIMEOUT_MS,
  rest,
  RestError,
  retryAfterSeconds,
} from "./index.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

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
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
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

// A LONGER PATH SAYS SO, AND STILL HAS A DEADLINE. `POST /backup` copies the
// whole store before it answers, so the ordinary thirty seconds would call a
// backup that is still copying a failure — while the engine goes on to write a
// perfectly good one.
test("a caller with a longer path keeps the request past the ordinary deadline", async () => {
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

  let done = false;
  const settled = rest
    .request("POST", "/backup", { timeoutMs: 30 * 60_000 })
    .catch((err: unknown) => err)
    .finally(() => {
      done = true;
    });
  await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS + 1);
  expect(done).toBe(false);

  await vi.advanceTimersByTimeAsync(30 * 60_000);
  const err = await settled;
  expect((err as RestError).status).toBe(0);
  expect((err as RestError).message).toContain("30 minutes");
  vi.useRealTimers();
});

// A CALL WITH A LONGER PATH SAYS SO, AND ITS REFUSAL NAMES ITS OWN DEADLINE.
//
// The node gate takes up to fifty seconds to answer a gesture
// (`engine.GateAnswerBudget`), and the fixed thirty seconds gave up on a
// gesture the node went on to finish. A per-call deadline replaces the default
// for that call only — it is not abandoned at the default, it IS abandoned at
// its own, and the sentence says which deadline ran out rather than the
// default's. The deadline is the gate's own (`GATE_REQUEST_TIMEOUT_MS`) rather
// than a copy of it: a literal here outlived the constant it was copied from.
test("a per-call deadline replaces the default for that call", async () => {
  expect(GATE_REQUEST_TIMEOUT_MS).toBeGreaterThan(REQUEST_TIMEOUT_MS);
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

  let done = false;
  const settled = rest
    .request("POST", "/worklet/retention/evict/node-4", {
      body: {},
      timeoutMs: GATE_REQUEST_TIMEOUT_MS,
    })
    .catch((err: unknown) => err)
    .finally(() => {
      done = true;
    });
  await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS + 1);
  expect(done).toBe(false);

  await vi.advanceTimersByTimeAsync(GATE_REQUEST_TIMEOUT_MS - REQUEST_TIMEOUT_MS);
  const err = await settled;
  expect(err).toBeInstanceOf(RestError);
  expect((err as RestError).status).toBe(0);
  expect((err as RestError).detail).toContain("within 60 seconds");
});

describe("the whole answer", () => {
  // THE TAG IS THE WRITE'S PRECONDITION. The config surface stamps a document
  // with its revision as an entity-tag, and a transport that returned only
  // bodies left every caller reading a DIFFERENT resource to learn the id an
  // If-Match needs.
  test("status, body and the entity-tag verbatim", async () => {
    stub(() => json({ name: "Acme" }, 200, { ETag: '"01JREV"' }));
    const answer = await rest.request("GET", "/config");
    expect(answer).toEqual({ status: 200, body: { name: "Acme" }, etag: '"01JREV"', marked: true });
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
    expect(answer).toEqual({ status: 304, body: null, etag: '"01JREV"', marked: false });
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
    expect(answer).toEqual({ status: 200, body: "name: Acme\n", etag: '"01JREV"', marked: false });
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

  test("a file's bytes are sent as the Blob, under the file's own type", async () => {
    const sent = stub(() => new Response(null, { status: 204 }));
    const png = new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], { type: "image/png" });
    await rest.putBytes("/artifacts/uploads/u/files/logo.png", png);
    expect(sent[0]!.init.body).toBe(png);
    expect((sent[0]!.init.headers as Record<string, string>)["Content-Type"]).toBe("image/png");
    await rest.putBytes("/artifacts/uploads/u/files/data.bin", new Blob([new Uint8Array([1])]));
    expect((sent[1]!.init.headers as Record<string, string>)["Content-Type"]).toBe(
      "application/octet-stream",
    );
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

// ONLY AN ANSWER WITH THE ENGINE'S MARK AND AN ERROR CODE IS THE ENGINE'S
// REFUSAL. Every refusal it writes is JSON with an `error` code and carries its
// mark (`X-Crewlet-Engine`), so a gateway's page, a JSON body of a proxy's own,
// a proxy's JSON spelled like the engine's but unmarked, or a 200 cut off part
// way through is something else, and a write's caller reading one as a refusal
// dropped the operation id of a gesture the engine went on to finish.
describe("whether an answer is the engine's own", () => {
  const cases: [string, () => Response, boolean][] = [
    ["an engine refusal", () => json({ error: "eviction_refused", detail: "live" }, 409), false],
    ["the guard's own 401", () => json({ error: "invalid_token" }, 401), false],
    [
      "the router's own 404 for a route this node does not serve",
      () => json({ error: "no_route", detail: "this node serves nothing at POST /x" }, 404),
      false,
    ],
    ["the router's own 405", () => json({ error: "method_not_allowed" }, 405), false],
    [
      "a gateway timeout's page",
      () =>
        new Response("<html><h1>504 Gateway Time-out</h1></html>", {
          status: 504,
          headers: { "Content-Type": "text/html" },
        }),
      true,
    ],
    ["a proxy's own JSON", () => json({ message: "upstream closed" }, 502), true],
    // FORGED: the engine's envelope, from something that is not the engine.
    ...([403, 413] as const).map((status): [string, () => Response, boolean] => [
      `a proxy's ${status} in the engine's own shape, without its mark`,
      () =>
        new Response(JSON.stringify({ error: "forbidden", detail: "blocked by policy" }), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      true,
    ]),
    [
      "a 200 cut off part way through",
      () =>
        new Response('{"node":"node-4","op_id":"01a0', {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      true,
    ],
  ];
  for (const [name, respond, unanswered] of cases) {
    test(`${name} is ${unanswered ? "not " : ""}the engine's answer`, async () => {
      stub(respond);
      const err = await rest
        .request("POST", "/worklet/retention/evict/node-4")
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(RestError);
      expect((err as RestError).unanswered).toBe(unanswered);
    });
  }

  test("a request nothing answered is not the engine's answer", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    const err = await rest
      .request("POST", "/worklet/retention/evict/node-4")
      .catch((e: unknown) => e);
    expect((err as RestError).status).toBe(0);
    expect((err as RestError).unanswered).toBe(true);
  });
});

// A REFUSAL ON AUTHORITY IS THE RULE'S, and only `unauthorized` is one. The
// socket reads its error frame on the same terms, so the two transports say
// the same thing about the same refusal.
describe("what a 403 tells a screen", () => {
  const refusalOf = (body: Record<string, unknown>) => new RestError(403, body).refusal;

  test("a rule's refusal carries its reason and the grants that would admit", () => {
    expect(refusalOf({ error: "unauthorized", reason: "no_grant", grants: ["admin"] })).toEqual({
      reason: "no_grant",
      grants: ["admin"],
    });
  });

  // NOT A RULE: a seat the chart no longer holds, a write from another site.
  // Read as one, its banner said no access would change it, of a rule that
  // never decided — true of neither.
  test("any other 403 is no rule's refusal", () => {
    expect(refusalOf({ error: "seat_unavailable", detail: "ana" })).toBeNull();
    expect(refusalOf({ error: "csrf_origin" })).toBeNull();
  });
});

// A SEGMENT STAYS ONE SEGMENT once a URL resolves it, and a value that
// cannot be one (empty, or dots alone, which the URL standard reads as a step
// along the path however they are escaped) is refused rather than sent.
test("a path segment is one segment, whatever it holds, or refused", () => {
  for (const value of ["a/b", "a?b#c", "a..b", ".hidden", "x.", "50% off"]) {
    const path = new URL(`/base/${pathSegment(value)}/end`, "http://engine.example").pathname;
    const parts = path.split("/");
    expect(parts, value).toHaveLength(4);
    expect(decodeURIComponent(parts[2]!), value).toBe(value);
  }
  for (const value of ["", ".", "..", "..."])
    expect(() => pathSegment(value), value).toThrow(RangeError);
});

// EVERY PATH SEGMENT GOES THROUGH THE ONE RULE (`lib/segment.ts`), read off
// every module's syntax tree. `encodeURIComponent` sends `..` as `..`, and
// `/config/llm-providers/..` reaches `/config`: an id that is only dots asked
// the route above it. So no module but the rule's own names
// `encodeURIComponent`, and every value put into a request path, up to its
// query, is one segment (`pathSegment`, `pathSegments` or `linkSegment`),
// whether a `/` precedes it or not: `/x/v-${id}` is as much a step along the
// path as `/x/${id}` when the id is `../..`.
//
// A REQUEST PATH is a template or a `+` chain (read whole, every link of it)
// that is passed to `rest.*`, held as a request's `path` (an object naming a
// `method` beside it), starts with `/` and no whitespace, or starts
// with a call to a PATH BUILDER: a function, in any module, that returns one
// of those. A path a builder starts is held at the builder's own template, so
// what follows the call is held here.
describe("every path segment is one segment", () => {
  const SEGMENTERS = new Set(["pathSegment", "pathSegments", "linkSegment"]);
  /**
   * The one place a path is built from a DECLARED SHAPE rather than segment by
   * segment: the assistant's route table (`contract/assistant.ts`), each route
   * a literal whose `{id}` is filled by [pathSegment], and the export and
   * stream addresses the engine answers, refused unless they sit under that
   * table's prefix. Named by module and expression, so a new one is a decision.
   */
  const DECLARED: ReadonlySet<string> = new Set([
    "protocol/assistant.ts tail",
    "protocol/assistant.ts path.slice(ASSISTANT_PREFIX.length)",
    "protocol/assistant.ts streamUrl.slice(ASSISTANT_PREFIX.length)",
  ]);

  /** What a path spells, in order: the text written into it and the values put between. */
  type Piece = { text: string } | { value: Node };
  const piecesOf = (node: Node): Piece[] => {
    if (node.type === "BinaryExpression" && node.operator === "+")
      return [...piecesOf(node.left as Node), ...piecesOf(node.right as Node)];
    const text = stringValue(node);
    if (text !== null) return [{ text }];
    if (node.type === "TemplateLiteral") {
      const quasis = node.quasis as Node[];
      const expressions = node.expressions as Node[];
      return quasis.flatMap((quasi, i) => {
        const value = quasi.value as { cooked?: string | null; raw: string };
        const piece: Piece[] = [{ text: value.cooked ?? value.raw }];
        const expression = expressions[i];
        return expression ? [...piece, { value: expression }] : piece;
      });
    }
    return [{ value: node }];
  };
  const isPathShaped = (node: Node) =>
    node.type === "TemplateLiteral" || (node.type === "BinaryExpression" && node.operator === "+");
  const calleeName = (node: Node) =>
    node.type === "CallExpression" && (node.callee as Node).type === "Identifier"
      ? String((node.callee as Node).name)
      : null;
  const isSegment = (node: Node) => SEGMENTERS.has(calleeName(node) ?? "");
  const startsPath = (pieces: Piece[], builders: ReadonlySet<string>) => {
    const first = pieces.find((p) => !("text" in p) || p.text !== "");
    if (!first) return false;
    // A URL PATH SPELLS NO WHITESPACE: `/ ${total}` is a count read aloud, not a route.
    return "text" in first
      ? /^\/\S*$/.test(first.text)
      : builders.has(calleeName(first.value) ?? "");
  };

  /** What each function returns, keyed by the name it is bound to. */
  const returnsOf = (root: Node): Map<string, Node[]> => {
    const out = new Map<string, Node[]>();
    const add = (name: string, fn: Node) => {
      const body = fn.body as Node;
      if (body.type !== "BlockStatement") {
        out.set(name, [body]);
        return;
      }
      const returned: Node[] = [];
      walk(body, (node) => {
        if (node !== body && /Function/.test(node.type)) return false;
        if (node.type === "ReturnStatement" && node.argument) returned.push(node.argument as Node);
      });
      out.set(name, returned);
    };
    walk(root, (node) => {
      if (node.type === "FunctionDeclaration" && node.id) add(String((node.id as Node).name), node);
      if (
        node.type === "VariableDeclarator" &&
        (node.id as Node).type === "Identifier" &&
        node.init &&
        /Function/.test((node.init as Node).type)
      )
        add(String((node.id as Node).name), node.init as Node);
    });
    return out;
  };

  /** Every function that returns a path, read to a fixed point: a builder may start with another. */
  const buildersIn = (trees: Node[]): Set<string> => {
    const returns = new Map<string, Node[]>();
    for (const tree of trees)
      for (const [name, nodes] of returnsOf(tree))
        returns.set(name, [...(returns.get(name) ?? []), ...nodes]);
    const builders = new Set<string>();
    for (let grew = true; grew;) {
      grew = false;
      for (const [name, nodes] of returns) {
        if (builders.has(name)) continue;
        if (nodes.some((n) => isPathShaped(n) && startsPath(piecesOf(n), builders))) {
          builders.add(name);
          grew = true;
        }
      }
    }
    return builders;
  };

  const keyOf = (property: Node) =>
    (property.key as Node).type === "Identifier"
      ? String((property.key as Node).name)
      : stringValue(property.key as Node);
  /**
   * A `path` held in a REQUEST: an object that names a `method` beside it, as
   * a keyed write does. A draft's field path (`tabs.${i}.title`) is no request's.
   */
  const requestObjects = new WeakSet<Node>();
  const isRequestPath = (property: Node) =>
    keyOf(property) === "path" && requestObjects.has(property);

  interface Source {
    path: string;
    text: string;
    lang: "ts" | "tsx" | "dts";
  }
  /** The values in `sources`' request paths that are not one segment, and the paths read. */
  function offendersIn(sources: readonly Source[]) {
    const trees = sources.map((s) => parse(s.text, s.lang));
    const builders = buildersIn(trees);
    const offending: string[] = [];
    const declared = new Set<string>();
    let checked = 0;
    sources.forEach(({ path, text }, m) => {
      const line = lineOf(text);
      walk(trees[m]!, (node, parent, key) => {
        if (node.type === "ObjectExpression") {
          const properties = (node.properties as Node[]).filter((p) => p.type === "Property");
          if (properties.some((p) => keyOf(p) === "method"))
            for (const property of properties) requestObjects.add(property);
          return;
        }
        if (!isPathShaped(node)) return;
        // A LINK OF A CHAIN is read with the whole chain, from its top.
        if (parent?.type === "BinaryExpression" && parent.operator === "+") return;
        const pieces = piecesOf(node);
        if (pieces.every((p) => "text" in p)) return;
        const passed =
          parent?.type === "CallExpression" &&
          key === "arguments" &&
          (parent.callee as Node).type === "MemberExpression" &&
          ((parent.callee as Node).object as Node).type === "Identifier" &&
          ((parent.callee as Node).object as Node).name === "rest";
        const held = parent?.type === "Property" && key === "value" && isRequestPath(parent);
        if (!passed && !held && !startsPath(pieces, builders)) return;
        checked++;
        // THE PATH ENDS AT THE QUERY: what follows a `?` is no segment.
        let before = "";
        let inQuery = false;
        pieces.forEach((piece, i) => {
          if ("text" in piece) {
            before = piece.text;
            inQuery ||= piece.text.includes("?");
            return;
          }
          const value = piece.value;
          if (inQuery || isSegment(value)) return;
          const leading = pieces.slice(0, i).every((p) => "text" in p && p.text === "");
          if (leading && builders.has(calleeName(value) ?? "")) return;
          const named = `${path} ${text.slice(value.start, value.end)}`;
          if (DECLARED.has(named)) {
            declared.add(named);
            return;
          }
          offending.push(`${path}:${line(value.start)} after "${before}"`);
        });
      });
    });
    return { offending, declared, checked };
  }

  test("no module but the rule's own encodes a segment itself", () => {
    const offending: string[] = [];
    for (const { path, text, lang } of modules()) {
      if (path === "lib/segment.ts") continue;
      const line = lineOf(text);
      walk(parse(text, lang), (node) => {
        if (node.type === "Identifier" && node.name === "encodeURIComponent")
          offending.push(`${path}:${line(node.start)}`);
      });
    }
    expect(offending).toEqual([]);
  });

  test("every value put into a request path is one segment", () => {
    const { offending, declared, checked } = offendersIn(modules());
    // FLOOR: the walk reached the request paths it exists for.
    expect(checked).toBeGreaterThan(40);
    expect(offending).toEqual([]);
    // Every declared shape is still built where it was named: a stale one goes.
    expect([...declared].sort()).toEqual([...DECLARED].sort());
  });

  // EACH SHAPE A PATH TAKES, a raw value in it found. Every one of these went
  // unread before the guard read paths whole.
  test.each<[string, string]>([
    [
      "held as a request's path",
      'export const r = (id: string) => ({ method: "GET", path: `artifacts/${id}` });',
    ],
    [
      "started by a builder",
      "const base = (id: string) => `/a/${pathSegment(id)}`;\n" +
        "export const r = (id: string, n: string) => `${base(id)}/versions/${n}`;",
    ],
    [
      "started by a builder a builder starts",
      "function base(id: string) { return `/a/${pathSegment(id)}`; }\n" +
        "const versions = (id: string) => `${base(id)}/versions`;\n" +
        "export const r = (id: string, n: string) => `${versions(id)}/${n}`;",
    ],
    [
      "a chain past its first value",
      'export const r = (a: string, b: string) => "/a/" + pathSegment(a) + "/b/" + b;',
    ],
    ["a value inside a segment", "export const r = (id: string) => `/x/v-${id}`;"],
  ])("a raw value in a path %s is found", (_, text) => {
    const { offending } = offendersIn([{ path: "case.ts", text, lang: "ts" }]);
    expect(offending).toHaveLength(1);
  });

  test("a segmented path, a builder's own call and a query pass", () => {
    const text =
      "const base = (id: string) => `/a/${pathSegment(id)}`;\n" +
      "export const r = (id: string, n: string, q: string) =>\n" +
      '  [`${base(id)}/v-${pathSegment(n)}?q=${q}`, "/a/" + pathSegment(id) + "?q=" + q];';
    const { offending, checked } = offendersIn([{ path: "case.ts", text, lang: "ts" }]);
    expect(offending).toEqual([]);
    expect(checked).toBe(3);
  });

  test("a field path and a count read aloud are no request's path", () => {
    const text =
      "export const r = (i: number, n: number) =>\n" +
      "  [{ path: `tabs.${i}.title`, label: `Tab ${i}` }, `/ ${n}`];";
    const { offending, checked } = offendersIn([{ path: "case.ts", text, lang: "ts" }]);
    expect(offending).toEqual([]);
    expect(checked).toBe(0);
  });
});
