/**
 * The preview host: it tells a page it may read its datastore and nothing
 * else, names nobody, answers reads through the datastore's own questions and
 * a subscription once, and refuses every write, state change, presence,
 * identity, link and print, asking the engine nothing for them.
 */

import { describe, expect, test, vi } from "vitest";
import type { LiveSocket } from "~/protocol/index.ts";
import { BRIDGE_MESSAGE, BRIDGE_METHODS, type BridgeMethod } from "~/runtime/bridge.ts";
import { PREVIEW_SNAPSHOT_LIMIT, PreviewHost } from "./previewHost.ts";

const doc = (id: string) => ({
  id,
  version: 2,
  fields: { title: `Row ${id}` },
  deleted: false,
  created_by: { name: "maya", kind: "human" },
  updated_by: { name: "maya", kind: "human" },
  created_at: "2026-10-19T10:00:00Z",
  updated_at: "2026-10-19T10:00:00Z",
});

/** A socket that answers the datastore's read questions and has nothing else to call. */
function socket() {
  const query = vi.fn(async (what: string) => {
    switch (what) {
      case "artifact_data":
      case "artifact_data_query":
        return { docs: [doc("r1"), { ...doc("r2"), deleted: true }] };
      case "artifact_data_doc":
        return doc("r1");
      case "artifact_data_schema":
        return { collections: [{ name: "rows" }, { name: "_state" }] };
      case "artifact_assets":
        return { assets: [] };
      default:
        throw new Error(`unexpected question ${what}`);
    }
  });
  return { query } as unknown as LiveSocket & { query: typeof query };
}

function port() {
  const sent: Record<string, unknown>[] = [];
  return {
    sent,
    postMessage: (m: Record<string, unknown>) => sent.push(m),
  } as unknown as MessagePort & {
    sent: Record<string, unknown>[];
  };
}

async function call(host: PreviewHost, p: ReturnType<typeof port>, method: string, params = {}) {
  await host.receive({ type: BRIDGE_MESSAGE.call, id: 7, method, params }, p);
  return p.sent.at(-1) as { ok: boolean; value?: unknown; error?: { code: string } };
}

describe("a preview's host", () => {
  test("tells the page it may read, names nobody, and is live", () => {
    const host = new PreviewHost({ artifact: "a1", socket: socket(), granted: ["db", "assets"] });
    const p = port();
    host.attach(p);
    expect(p.sent[0]).toMatchObject({
      type: BRIDGE_MESSAGE.init,
      capabilities: ["db", "assets"],
      viewer: null,
      connection: "live",
    });
  });

  test("answers reads through the datastore's own questions, and a subscription once", async () => {
    const s = socket();
    const host = new PreviewHost({ artifact: "a1", socket: s, granted: ["db"] });
    const p = port();
    expect((await call(host, p, "data.get", { collection: "rows", id: "r1" })).value).toMatchObject(
      {
        id: "r1",
      },
    );
    expect(await call(host, p, "data.list", { collection: "rows" })).toMatchObject({
      ok: true,
      value: { docs: [{ id: "r1" }] },
    });
    expect(
      await call(host, p, "data.query", { collection: "rows", query: { where: [["a", "eq", 1]] } }),
    ).toMatchObject({ ok: true });
    expect((await call(host, p, "data.schema")).value).toEqual([{ name: "rows" }]);
    const sub = await call(host, p, "data.subscribe", { collection: "rows" });
    expect(sub.value).toMatchObject({ subscription: 1, docs: [{ id: "r1" }] });
    expect(s.query).toHaveBeenCalledWith("artifact_data", {
      id: "a1",
      collection: "rows",
      limit: PREVIEW_SNAPSHOT_LIMIT,
    });
    // EVERY QUESTION IS A READ of this artifact alone.
    for (const [what, params] of s.query.mock.calls as unknown as [string, { id: string }][]) {
      expect(what).toMatch(/^artifact_(data|data_doc|data_query|data_schema|assets)$/);
      expect(params.id).toBe("a1");
    }
  });

  test("refuses every call that is not a read, and asks the engine nothing for it", async () => {
    const s = socket();
    const host = new PreviewHost({
      artifact: "a1",
      socket: s,
      granted: ["db", "assets", "identity", "presence", "state"],
    });
    const p = port();
    const reads = new Set([
      "data.get",
      "data.list",
      "data.query",
      "data.schema",
      "data.subscribe",
      "data.unsubscribe",
      "assets.list",
    ]);
    const others = (Object.keys(BRIDGE_METHODS) as BridgeMethod[]).filter((m) => !reads.has(m));
    expect(others).toEqual(
      expect.arrayContaining(["data.write", "data.undo", "data.redo", "state.set", "presence.set"]),
    );
    for (const method of others) {
      const answer = await call(host, p, method, { ops: [], collection: "rows", url: "https://x" });
      expect(answer.ok, method).toBe(false);
      expect(["forbidden", "capability_not_declared"], method).toContain(answer.error?.code);
    }
    expect(s.query).not.toHaveBeenCalled();
  });

  test("tells a page granted no datastore nothing it may read, and answers it no read", async () => {
    const s = socket();
    const host = new PreviewHost({ artifact: "a1", socket: s, granted: ["assets"] });
    const p = port();
    host.attach(p);
    expect(p.sent[0]).toMatchObject({ capabilities: ["assets"] });
    expect((await call(host, p, "data.list", { collection: "rows" })).error?.code).toBe(
      "capability_not_declared",
    );
    expect(s.query).not.toHaveBeenCalled();
  });
});
