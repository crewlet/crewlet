// @vitest-environment node
/**
 * The host end of the page bridge: every call is held to what the version
 * declared before it reaches the engine, translated into the engine's own
 * grammar, and answered in the page contract's words.
 */

import { describe, expect, test, vi } from "vitest";
import { QueryError, type LiveSocket } from "~/protocol/index.ts";
import { BRIDGE_MESSAGE, type RuntimeCapability } from "~/runtime/bridge.ts";
import type { KeyedAnswer } from "~/lib/keyedWrite.ts";
import { PageHost, bridgeOutcome, engineQuery } from "./host.ts";

describe("a page's query", () => {
  test("is the engine's grammar: operators, a list for in, one order and the cursor", () => {
    expect(
      engineQuery({
        where: [
          ["status", "eq", "done"],
          ["score", "gte", 3],
          ["score", "lte", 9],
          ["owner", "in", ["maya", "ali"]],
        ],
        order: [["score", "desc"]],
        limit: 20,
        cursor: "abc",
      }),
    ).toEqual({
      where: [
        { path: "status", op: "eq", value: "done" },
        { path: "score", op: "ge", value: 3 },
        { path: "score", op: "le", value: 9 },
        { path: "owner", op: "in", values: ["maya", "ali"] },
      ],
      order_by: { path: "score", desc: true },
      limit: 20,
      after: "abc",
    });
  });

  test("is refused for an operator, an order or a limit the contract does not take", () => {
    expect(() => engineQuery({ where: [["a", "like", "x"]] })).toThrow(/op like/);
    expect(() => engineQuery({ where: [["a", "in", "x"]] })).toThrow(/list of values/);
    expect(() =>
      engineQuery({
        order: [
          ["a", "asc"],
          ["b", "asc"],
        ],
      }),
    ).toThrow(/one/);
    expect(() => engineQuery({ limit: 1.5 })).toThrow(/whole number/);
    expect(() => engineQuery({ where: "a = 1" })).toThrow(/list/);
  });
});

describe("a write's verdict", () => {
  test("is an answer for every verdict the engine decided", () => {
    expect(bridgeOutcome({ verdict: "applied", seq: 4 } as never)).toEqual({
      verdict: "applied",
      seq: 4,
    });
    expect(bridgeOutcome({ verdict: "pending" } as never)).toEqual({ verdict: "pending" });
    expect(bridgeOutcome({ verdict: "conflict", reason: "stale" } as never)).toEqual({
      verdict: "conflict",
      reason: "stale",
    });
  });

  test("is a refusal when nothing was decided, or the transport refused it", () => {
    expect(() => bridgeOutcome({ verdict: "unknown", reason: "lost" } as never)).toThrow("lost");
    expect(() =>
      bridgeOutcome({ verdict: "refused", code: "write_budget", reason: "slow down" } as never),
    ).toThrow("slow down");
  });
});

/** A port that keeps what the host posts. */
function port() {
  const posted: Record<string, unknown>[] = [];
  return {
    posted,
    port: { postMessage: (m: Record<string, unknown>) => posted.push(m) } as unknown as MessagePort,
  };
}

function host(
  capabilities: RuntimeCapability[],
  over: {
    query?: (kind: string, params: Record<string, unknown>) => Promise<unknown>;
    write?: () => Promise<KeyedAnswer>;
    openLink?: (url: URL) => Promise<boolean>;
    print?: () => Promise<boolean>;
    readerActed?: () => boolean;
    setPresence?: (...args: unknown[]) => void;
  } = {},
) {
  const query = vi.fn(over.query ?? (async () => ({})));
  const h = new PageHost({
    artifact: "a1",
    version: 3,
    capabilities,
    socket: { query } as unknown as LiveSocket,
    store: null,
    clientID: "client-1",
    newKey: () => "key-1",
    openLink: over.openLink ?? (async () => true),
    print: over.print,
    readerActed: over.readerActed,
    setPresence: over.setPresence ?? (() => {}),
    write:
      over.write ??
      (async (): Promise<KeyedAnswer> => ({ kind: "done", pending: false, body: {} })),
  });
  return { h, query };
}

async function call(h: PageHost, method: string, params: Record<string, unknown> = {}) {
  const p = port();
  await h.receive({ type: BRIDGE_MESSAGE.call, id: 1, method, params }, p.port);
  return p.posted[0];
}

describe("the host", () => {
  test("refuses a call the version did not declare, before it reaches the engine", async () => {
    const { h, query } = host([]);
    expect(await call(h, "assets.list")).toMatchObject({
      ok: false,
      error: { code: "capability_not_declared" },
    });
    expect(query).not.toHaveBeenCalled();
  });

  test("ignores anything on the port that is not exactly a call", async () => {
    const { h } = host(["db"]);
    const p = port();
    await h.receive({ type: BRIDGE_MESSAGE.call, id: 1, method: "constructor" }, p.port);
    await h.receive("bridge.call", p.port);
    expect(p.posted).toEqual([]);
  });

  test("binds every read to its own artifact, whatever the page names", async () => {
    const { h, query } = host(["assets"], {
      query: async () => ({
        assets: [
          { id: "x", name: "a.png", content_type: "image/png", object: { size: 3, hash: "h" } },
        ],
      }),
    });
    expect(await call(h, "assets.list", { id: "another-artifact" })).toMatchObject({
      ok: true,
      value: [{ id: "x", name: "a.png", contentType: "image/png", size: 3 }],
    });
    expect(query).toHaveBeenCalledWith("artifact_assets", { id: "a1" });
  });

  test("answers a document that is not there as null, and a refused read as forbidden", async () => {
    const missing = host(["db"], {
      query: async () => {
        throw new QueryError("not_found");
      },
    });
    expect(await call(missing.h, "data.get", { collection: "rows", id: "r9" })).toMatchObject({
      ok: true,
      value: null,
    });
    const refused = host(["db"], {
      query: async () => {
        throw new QueryError("unauthorized");
      },
    });
    expect(await call(refused.h, "data.list", { collection: "rows" })).toMatchObject({
      ok: false,
      error: { code: "forbidden" },
    });
  });

  test("never lets a page write the engine's per-viewer state collection through the datastore", async () => {
    const { h } = host(["db"]);
    expect(
      await call(h, "data.write", { ops: [{ op: "set", collection: "_state", id: "x" }] }),
    ).toMatchObject({ ok: false, error: { code: "invalid" } });
  });

  test("words a refused state write by the status the engine answered", async () => {
    const { h } = host(["state"], {
      write: async () =>
        ({
          kind: "refused",
          text: "Your share does not allow this.",
          error: { status: 403 },
        }) as unknown as KeyedAnswer,
    });
    expect(await call(h, "state.set", { key: "k", value: 1 })).toMatchObject({
      ok: false,
      error: { code: "forbidden", message: "Your share does not allow this." },
    });
  });

  test("opens only an http or https link, and only through the reader's answer", async () => {
    const openLink = vi.fn(async () => false);
    const { h } = host([], { openLink });
    expect(await call(h, "openLink", { url: "javascript:alert(1)" })).toMatchObject({
      ok: false,
      error: { code: "invalid" },
    });
    expect(await call(h, "openLink", { url: "/relative" })).toMatchObject({ ok: false });
    expect(await call(h, "openLink", { url: "https://example.com/a" })).toMatchObject({
      ok: true,
      value: false,
    });
    expect(openLink).toHaveBeenCalledTimes(1);
  });

  test("prints only on the reader's own gesture, and only where the host prints", async () => {
    const print = vi.fn(async () => true);
    let acted = false;
    const { h } = host([], { print, readerActed: () => acted });
    expect(await call(h, "print")).toMatchObject({ ok: false, error: { code: "forbidden" } });
    expect(print).not.toHaveBeenCalled();
    acted = true;
    expect(await call(h, "print")).toMatchObject({ ok: true, value: true });
    expect(print).toHaveBeenCalledTimes(1);

    const { h: none } = host([], { readerActed: () => true });
    expect(await call(none, "print")).toMatchObject({ ok: false, error: { code: "unavailable" } });
  });

  test("refuses a link the frame protocol refuses: userinfo, past the bound, or mail", async () => {
    const openLink = vi.fn(async () => true);
    const { h } = host([], { openLink });
    for (const url of [
      "https://accounts.example.com:443@evil.example/login",
      "https://user:secret@example.com/",
      `https://example.com/${"a".repeat(2048)}`,
      "mailto:someone@example.com",
    ]) {
      expect(await call(h, "openLink", { url })).toMatchObject({
        ok: false,
        error: { code: "invalid" },
      });
    }
    expect(openLink).not.toHaveBeenCalled();
  });

  test("pins presence to the page's version and bounds what it says", async () => {
    const setPresence = vi.fn();
    const { h } = host(["presence"], { setPresence });
    await call(h, "presence.set", {
      activity: "typing",
      focus: { version: 99, doc: "r1", field: "x".repeat(300), script: "y" },
    });
    expect(setPresence).toHaveBeenCalledWith("viewing", {
      version: 3,
      doc: "r1",
      field: "x".repeat(128),
    });
  });

  test("tells an attached page its contract, capabilities and connection first", () => {
    const { h } = host(["db", "presence"]);
    const p = port();
    h.attach(p.port);
    expect(p.posted[0]).toMatchObject({
      type: BRIDGE_MESSAGE.init,
      contract: "1.0.0",
      capabilities: ["db", "presence"],
      connection: "live",
    });
    expect(p.posted[1]).toMatchObject({ type: BRIDGE_MESSAGE.event, topic: "presence" });
  });
});
