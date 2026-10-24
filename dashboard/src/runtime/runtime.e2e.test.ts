/**
 * The page runtime end to end: the committed `runtime.js` a page runs,
 * talking over a real port to the real page host and datastore, over an
 * engine held in memory. What a page is promised in
 * `docs/reference/artifact-runtime.md` is held here on the bytes the
 * artifact origin serves, rather than on either half alone.
 */

import { afterAll, beforeAll, describe, expect, test } from "vitest";
import {
  MemoryEngine,
  openPage,
  pageRuntime,
  type OpenPage,
  type PageError,
} from "~/test/artifactPage.ts";
import { PageHost } from "~/routes/artifacts/host.ts";

const engine = new MemoryEngine(
  [
    {
      name: "vendors",
      title: "Vendors",
      read: "all",
      create: "contributor",
      delete: "editor",
      columns: [
        { key: "vendor", type: "text", primary: true },
        { key: "score", type: "number" },
        { key: "owner", type: "person" },
      ],
    },
    { name: "payroll", read: "editors", create: "editor", delete: "editor" },
  ],
  { name: "maya", kind: "human" },
);
engine.seed("vendors", { v1: { vendor: "Northwind", score: 7, owner: "maya" } });
engine.unreadable.add("payroll");

let page: OpenPage;
beforeAll(async () => {
  page = await openPage({
    host: (options) => new PageHost(options),
    capabilities: ["db", "identity", "presence"],
    scriptCapabilities: "db identity presence",
    engine,
    viewer: { seat: "maya", kind: "human", displayName: "Maya Ortiz", role: "editor" },
    people: [
      { seat: "maya", kind: "human", displayName: "Maya Ortiz" },
      {
        seat: "ada",
        kind: "agent",
        displayName: "Ada",
        avatar: { character: "otter", hue: "cyan" },
      },
    ],
    roster: {
      members: [
        {
          session: "s-ada",
          seat: "ada",
          kind: "agent",
          displayName: "Ada",
          avatar: { character: "otter", hue: "cyan" },
          activity: "editing",
          focus: { collection: "vendors", doc: "v1", field: "score" },
          at: "2026-10-18T10:00:00Z",
        },
      ],
      truncated: false,
    },
  });
});
afterAll(() => page.close());

/** The next delivery a subscription makes that satisfies `ready`. */
function delivered(
  collection: string,
  ready: (docs: { id: string; data: Record<string, unknown> }[]) => boolean,
): Promise<{ id: string; data: Record<string, unknown> }[]> {
  return new Promise((resolve, reject) => {
    const stop = pageRuntime()
      .data.collection(collection)
      .subscribe(
        (docs) => {
          if (!ready(docs)) return;
          queueMicrotask(() => stop());
          resolve(docs);
        },
        (err) => reject(err),
      );
  });
}

describe("a page", () => {
  test("knows what its version declares from its first script, before the handshake", () => {
    expect(page.early).toEqual(["db", "identity", "presence"]);
  });

  test("is told what its version declared once the host has answered its hello", () => {
    expect(pageRuntime().capabilities.list().sort()).toEqual(["db", "identity", "presence"]);
    expect(pageRuntime().capabilities.has("state")).toBe(false);
  });

  test("reads a collection live and sees its own write applied", async () => {
    const first = await delivered("vendors", (docs) => docs.length === 1);
    expect(first[0]?.data).toEqual({ vendor: "Northwind", score: 7, owner: "maya" });
    const seen = delivered("vendors", (docs) => docs[0]?.data["score"] === 9);
    expect(await pageRuntime().data.collection("vendors").merge("v1", { score: 9 })).toMatchObject({
      verdict: "applied",
    });
    expect((await seen)[0]?.data["score"]).toBe(9);
  });

  test("is answered invalid, not thrown at, for a value refused before the log", async () => {
    const outcome = await pageRuntime().data.collection("vendors").merge("v1", { score: "high" });
    expect(outcome).toMatchObject({ verdict: "invalid" });
  });

  test("is told a subscription was refused rather than waiting on it forever", async () => {
    const refusal = await new Promise<PageError>((resolve, reject) => {
      pageRuntime()
        .data.collection("payroll")
        .subscribe(() => reject(new Error("a refused subscription delivered documents")), resolve);
    });
    expect(refusal.code).toBe("forbidden");
    expect(refusal).toBeInstanceOf(Error);
  });

  test("undoes and redoes its own writes, and nothing past them", async () => {
    const collection = pageRuntime().data.collection("vendors");
    await collection.merge("v1", { vendor: "Contoso" });
    expect(await pageRuntime().data.undo()).toMatchObject({ verdict: "applied" });
    expect((await delivered("vendors", () => true))[0]?.data["vendor"]).toBe("Northwind");
    expect(await pageRuntime().data.redo()).toMatchObject({ verdict: "applied" });
    expect((await delivered("vendors", () => true))[0]?.data["vendor"]).toBe("Contoso");
  });

  test("is answered conflict for an undo somebody overtook", async () => {
    await pageRuntime().data.collection("vendors").merge("v1", { score: 4 });
    engine.seed("vendors", { v1: { vendor: "Fabrikam", score: 1 } });
    expect(await pageRuntime().data.undo()).toMatchObject({ verdict: "conflict" });
  });

  test("names the people a person cell offers, and the room's members, from the chart", async () => {
    expect(await pageRuntime().identity.people()).toEqual([
      { seat: "maya", kind: "human", displayName: "Maya Ortiz" },
      {
        seat: "ada",
        kind: "agent",
        displayName: "Ada",
        avatar: { character: "otter", hue: "cyan" },
      },
    ]);
    expect(await pageRuntime().identity.me()).toMatchObject({ seat: "maya", role: "editor" });
    expect(pageRuntime().presence.roster().members).toEqual([
      expect.objectContaining({ session: "s-ada", displayName: "Ada", activity: "editing" }),
    ]);
  });

  test("reads the schema the collections were declared with", async () => {
    expect((await pageRuntime().data.schema()).map((c) => c.name)).toEqual(["vendors", "payroll"]);
  });
});
