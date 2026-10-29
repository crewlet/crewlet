import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cancelDeviceSignIn,
  checkHostLogin,
  checkModel,
  referencesOf,
  removeModel,
  retryModel,
  startDeviceSignIn,
  storeCredential,
  testCredential,
} from "./api.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

/**
 * A NAME IS ONE SEGMENT, on every route that carries one: what the request
 * actually sent, after the URL resolved, read back as the name it was given.
 * A key or an id is the engine's, but a `/` would reach a different route, a
 * `?` would start the query, and `..` would climb out of the model's path.
 */
describe("every model route", () => {
  const NAMES = ["a/b", "what?x=1#y", "a..b", "x.", "50% off"];
  const ROUTES: { prefix: string; suffix: string; call: (name: string) => Promise<unknown> }[] = [
    {
      prefix: "/llm/providers/",
      suffix: "/credential",
      call: (k) => testCredential(k, { values: {}, createOnly: false }),
    },
    {
      prefix: "/llm/providers/",
      suffix: "/credential",
      call: (k) => storeCredential(k, { values: {}, createOnly: false }, "e1"),
    },
    { prefix: "/llm/providers/", suffix: "/check", call: (k) => checkModel(k, { smoke: false }) },
    { prefix: "/llm/providers/", suffix: "/retry", call: (k) => retryModel(k, "probe") },
    { prefix: "/llm/providers/", suffix: "/host-login/check", call: (k) => checkHostLogin(k) },
    { prefix: "/llm/providers/", suffix: "/device-sign-in", call: (k) => startDeviceSignIn(k) },
    { prefix: "/llm/device-sign-ins/", suffix: "", call: (id) => cancelDeviceSignIn(id) },
    {
      prefix: "/config/llm-providers/",
      suffix: "",
      call: (k) =>
        removeModel(
          k,
          { repoint: [], assistantOff: false },
          { etag: "e1", summary: "", dryRun: true },
          new AbortController().signal,
        ),
    },
  ];

  it("sends a model's key or a sign-in's id as exactly one path segment", async () => {
    const sent: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        sent.push(new URL(url, "http://engine.example").pathname);
        return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
      }),
    );
    for (const route of ROUTES) {
      for (const name of NAMES) {
        sent.length = 0;
        await route.call(name);
        expect(sent, `${route.prefix}${name}${route.suffix}`).toHaveLength(1);
        const path = sent[0]!;
        expect(path.startsWith(route.prefix), path).toBe(true);
        expect(path.endsWith(route.suffix), path).toBe(true);
        const segment = path.slice(route.prefix.length, path.length - route.suffix.length);
        expect(segment, path).not.toContain("/");
        expect(decodeURIComponent(segment), path).toBe(name);
      }
      // A NAME OF DOTS ALONE is the route above whatever the escaping: never sent.
      sent.length = 0;
      await expect(async () => route.call("..")).rejects.toThrow(RangeError);
      expect(sent, route.prefix).toEqual([]);
    }
  });
});

describe("referencesOf", () => {
  // THE REMOVE FLOW DRAWS ITS REPOINTS FROM THE REFUSAL, so the references are
  // read off the engine's body as written, and nothing else passes as one.
  it("reads the references a removal refusal carries", () => {
    const body = {
      error: "validation_error",
      references: [
        { at: { seat: "pm", job: "review" }, segments: ["roles", 0, "llm_review"], chain: ["big"] },
        { at: { assistant: "chat" }, segments: ["assistant", "llm"], chain: ["big"] },
      ],
    };
    expect(referencesOf(body)).toHaveLength(2);
    expect(referencesOf(body)[1]?.at).toEqual({ assistant: "chat" });
  });

  it("reads none from a body without them, or with something else in their place", () => {
    expect(referencesOf(null)).toEqual([]);
    expect(referencesOf({ error: "conflict" })).toEqual([]);
    expect(referencesOf({ references: "big" })).toEqual([]);
    expect(referencesOf({ references: [{ at: "pm" }, 3] })).toEqual([]);
  });
});
