/**
 * Saving the company's brand: a version of the company's kit published (or a
 * first kit made) and adopted, each write keyed; a version that landed and was
 * not adopted said as exactly that; a kit that is gone replaced by a new one.
 */

import { afterEach, expect, test, vi } from "vitest";
import type { BrandDocument } from "~/protocol/index.ts";
import { newBrandSaveKeys, saveBrand, type BrandSave } from "./api.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

afterEach(() => vi.unstubAllGlobals());

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
  });
}

const DOC: BrandDocument = {
  v: 1,
  name: "FairSplit",
  logos: { primary: "logos/primary.svg" },
  colors: { primary: "#0f7b5f" },
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};

function save(kit: string | null): BrandSave {
  return {
    kit,
    head: 7,
    title: "FairSplit brand",
    document: DOC,
    files: [{ path: "logos/primary.svg", body: new Blob(["<svg/>"], { type: "image/svg+xml" }) }],
  };
}

type Answer = (url: string, init: RequestInit) => Response;

/** An engine answering each request by the first rule whose pattern its method and path match. */
function engine(rules: [RegExp, Answer][]) {
  const sent: { method: string; url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const method = init.method ?? "GET";
      sent.push({ method, url, init });
      const rule = rules.find(([re]) => re.test(`${method} ${url}`));
      return rule ? rule[1](url, init) : new Response(null, { status: 204 });
    }),
  );
  return sent;
}

const intent: Answer = () =>
  json(
    {
      upload: "u-1",
      expires_at: "",
      transfers: [
        { path: "brand.json", method: "PUT", url: "", direct: false },
        { path: "logos/primary.svg", method: "PUT", url: "", direct: false },
      ],
    },
    201,
  );

test("with no brand adopted, a first kit is made from the files and adopted", async () => {
  const sent = engine([
    [/^POST .*\/artifacts\/uploads$/, intent],
    [/^POST .*\/artifacts$/, () => json({ artifact: "k-1", number: 1 }, 201)],
    [/^POST .*\/brand\/adoptions$/, () => json({}, 200)],
  ]);
  const keys = newBrandSaveKeys();
  const answer = await saveBrand(save(null), keys);
  expect(answer).toEqual({ kind: "adopted", kit: "k-1", version: 1, pending: false });
  const create = sent.find((s) => s.method === "POST" && /\/artifacts$/.test(s.url))!;
  const body = JSON.parse(String(create.init.body));
  expect(body).toMatchObject({
    kind: "brand",
    title: "FairSplit brand",
    upload: "u-1",
    entry: "brand.json",
    shares: [{ audience: { kind: "company" }, role: "viewer" }],
  });
  expect(new Headers(create.init.headers).get("Idempotency-Key")).toBe(keys.write);
  // THE INDEX TRAVELS AS THE CANONICAL TEXT a reader of the version reads.
  const index = sent.find((s) => /files\/brand\.json$/.test(s.url))!;
  expect(JSON.parse(String(index.init.body))).toMatchObject({ v: 1, name: "FairSplit" });
  const adopt = sent.find((s) => /\/brand\/adoptions$/.test(s.url))!;
  expect(JSON.parse(String(adopt.init.body))).toEqual({ kit: "k-1", version: 1 });
  expect(new Headers(adopt.init.headers).get("Idempotency-Key")).toBe(keys.adopt);
});

test("with a brand adopted, a version of its kit is published against the head and adopted", async () => {
  const sent = engine([
    [/^POST .*\/artifacts\/k-1\/uploads$/, intent],
    [/^POST .*\/artifacts\/k-1\/versions$/, () => json({ artifact: "k-1", number: 8 }, 201)],
    [/^POST .*\/brand\/adoptions$/, () => json({}, 202)],
  ]);
  const answer = await saveBrand(save("k-1"), newBrandSaveKeys());
  expect(answer).toEqual({ kind: "adopted", kit: "k-1", version: 8, pending: true });
  const publish = sent.find((s) => /\/versions$/.test(s.url))!;
  expect(new Headers(publish.init.headers).get("If-Match")).toBe('"7"');
  expect(JSON.parse(String(publish.init.body))).toMatchObject({
    mode: "whole",
    entry: "brand.json",
    capabilities: [],
  });
});

test("a version that landed and was not adopted says so, with the engine's reason", async () => {
  engine([
    [/^POST .*\/artifacts\/k-1\/uploads$/, intent],
    [/^POST .*\/artifacts\/k-1\/versions$/, () => json({ artifact: "k-1", number: 8 }, 201)],
    [
      /^POST .*\/brand\/adoptions$/,
      () =>
        json(
          {
            error: "brand_contrast",
            message: "Text on the light ground must reach 4.5:1.",
            contrast: { foreground: "text", ground: "light_ground", ratio: 2.5, min: 4.5 },
          },
          422,
        ),
    ],
  ]);
  const answer = await saveBrand(save("k-1"), newBrandSaveKeys());
  expect(answer.kind).toBe("published");
  if (answer.kind !== "published") return;
  expect(answer.version).toBe(8);
  expect(answer.text).toMatch(/^Version 8 of the brand kit is saved and not published yet\./);
});

test("an adopted kit that is gone is replaced by a new kit holding the same files", async () => {
  const sent = engine([
    [
      /^POST .*\/artifacts\/k-1\/uploads$/,
      () => json({ error: "not_found", message: "Not found." }, 404),
    ],
    [/^POST .*\/artifacts\/uploads$/, intent],
    [/^POST .*\/artifacts$/, () => json({ artifact: "k-2", number: 1 }, 201)],
    [/^POST .*\/brand\/adoptions$/, () => json({}, 200)],
  ]);
  const keys = newBrandSaveKeys();
  const answer = await saveBrand(save("k-1"), keys);
  expect(answer).toEqual({ kind: "adopted", kit: "k-2", version: 1, pending: false });
  const create = sent.find((s) => s.method === "POST" && /\/artifacts$/.test(s.url))!;
  expect(new Headers(create.init.headers).get("Idempotency-Key")).toBe(keys.newKitWrite);
});

test("a refused publish adopts nothing", async () => {
  const sent = engine([
    [/^POST .*\/artifacts\/k-1\/uploads$/, intent],
    [
      /^POST .*\/artifacts\/k-1\/versions$/,
      () =>
        json({ error: "invalid", message: "brand.json:3: field: the colour is not #rrggbb." }, 422),
    ],
  ]);
  const answer = await saveBrand(save("k-1"), newBrandSaveKeys());
  expect(answer.kind).toBe("refused");
  expect(sent.some((s) => /\/brand\/adoptions$/.test(s.url))).toBe(false);
});

test("keys are the engine's operation ids, one per write", () => {
  const keys = newBrandSaveKeys();
  const all = Object.values(keys);
  expect(new Set(all).size).toBe(all.length);
});
