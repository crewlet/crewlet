/**
 * The engine's charter route (`GET` and `PUT /config/charter`) as a suite
 * holds it: what the company's charter says, its tag, every write the page
 * sent, and a refusal to answer the next write with.
 *
 * It keeps the route's own contract (`internal/api/configapi/charter.go`),
 * because a suite that answered more kindly than the engine would certify an
 * editor the engine refuses: a write is conditional on the charter's tag and
 * refused `412 version_moved` with the charter as it now is when the tag is
 * stale, a write that says what the charter already says is `200
 * changed:false` whatever its tag, and a stored write moves the tag. Every
 * other request answers 404, as no route at all.
 *
 * [fakeCharterEngine] installs the route as the global `fetch`; a suite that
 * answers other routes beside it (the org builder's configuration) asks
 * [charterRoute] for the handler and calls it for the charter's path.
 */

import { vi } from "vitest";
import type { Charter } from "~/contract/config.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

/** The fake engine: its state, which a suite reads and changes between steps. */
export interface CharterEngine {
  charter: Charter;
  /** Moved by every stored write; the ETag is `"charter-<tag>"`. */
  tag: number;
  /** Every PUT the page sent: its body, its precondition and its summary. */
  puts: { body: Charter; ifMatch: string | null; summary: string | null }[];
  /** How many times the page read the charter. */
  gets: number;
  /** Answers the next PUT instead of judging it. */
  refuse?: { status: number; body: unknown } | undefined;
  /** Fails the next PUT as a request that never reached the engine. */
  fail?: Error | undefined;
}

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
  });

const same = (a: Charter, b: Charter) =>
  a.mission === b.mission &&
  a.vision === b.vision &&
  a.policies.length === b.policies.length &&
  a.policies.every((p, i) => p === b.policies[i]);

/** A charter route's state over `initial`, at its first tag, with nothing sent. */
export function charterEngine(initial: Charter): CharterEngine {
  return { charter: { ...initial, policies: [...initial.policies] }, tag: 1, puts: [], gets: 0 };
}

/** The route as a `fetch` handler over `engine`, which it reads and moves. */
export function charterRoute(engine: CharterEngine) {
  return async (url: string | URL | Request, init: RequestInit = {}): Promise<Response> => {
    if (!String(url).includes("/config/charter")) return json({}, 404);
    const etag = `"charter-${engine.tag}"`;
    if ((init.method ?? "GET") === "GET") {
      engine.gets++;
      return json(engine.charter, 200, { ETag: etag });
    }
    const headers = new Headers(init.headers);
    const body = JSON.parse(String(init.body)) as Charter;
    engine.puts.push({
      body,
      ifMatch: headers.get("If-Match"),
      summary: headers.get("X-Summary"),
    });
    if (engine.fail) {
      const failure = engine.fail;
      engine.fail = undefined;
      throw failure;
    }
    if (engine.refuse) {
      const { status, body: refusal } = engine.refuse;
      engine.refuse = undefined;
      return json(refusal, status);
    }
    if (same(body, engine.charter)) {
      return json({ charter: body, changed: false, revision_id: "r" }, 200, { ETag: etag });
    }
    if (headers.get("If-Match") !== etag) {
      return json({ error: "version_moved", current: engine.charter, etag }, 412, { ETag: etag });
    }
    engine.charter = body;
    engine.tag++;
    return json({ charter: body, changed: true, revision_id: "r", epoch: 5 }, 200, {
      ETag: `"charter-${engine.tag}"`,
    });
  };
}

/**
 * A charter route over `initial`, installed as the global `fetch` (undo with
 * `vi.unstubAllGlobals()`). The returned engine is live: change it between
 * steps to play a colleague's write.
 */
export function fakeCharterEngine(initial: Charter): CharterEngine {
  const engine = charterEngine(initial);
  vi.stubGlobal("fetch", vi.fn(charterRoute(engine)));
  return engine;
}
