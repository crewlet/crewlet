import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryError } from "~/protocol/index.ts";
import type { ModelCheck } from "~/protocol/models.ts";
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
    {
      prefix: "/llm/providers/",
      suffix: "/check",
      call: (k) => checkModel(k, { smoke: false }, () => Promise.reject(new Error("not asked"))),
    },
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
      // A NAME OF DOTS ALONE is the route above whatever the escaping: never
      // sent, and refused as a rejection, never a throw from the call itself.
      sent.length = 0;
      let refused: Promise<unknown> | undefined;
      expect(() => {
        refused = route.call("..");
      }, route.prefix).not.toThrow();
      await expect(refused).rejects.toThrow(RangeError);
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

/**
 * A MODEL'S TEST RUNS ON ITS NODE'S OWN TIME, so the route answers it running
 * and the page asks about it (`llm_check`) until it is not, whichever node
 * answers: what it found, or a refusal saying why it found nothing. A node
 * that cannot answer yet is asked again; the caller's abort ends the wait at
 * once.
 */
describe("a model's test", () => {
  const running = {
    id: "check-1",
    entry: "opus",
    node: "node-a",
    state: "running",
    draft: false,
    smoke: true,
    started_at: "2026-11-14T12:00:00Z",
    deadline: "2026-11-14T12:03:10Z",
    ended_at: null,
    by: "jane.doe",
  } as const;
  const found = { at: "2026-11-14T12:00:40Z", node: "node-a", ok: true, lines: [] };

  /** Answers the route's POST with `started`, recording what was sent. */
  function route(started: unknown) {
    const sent: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        sent.push(`${init?.method ?? "GET"} ${new URL(url, "http://engine.example").pathname}`);
        return new Response(JSON.stringify(started), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        });
      }),
    );
    return sent;
  }

  /** Answers each question with the next of `answers`: a record, or an error to reject with. */
  function asking(answers: unknown[]) {
    const asked: string[] = [];
    const ask = async (key: string): Promise<ModelCheck> => {
      asked.push(key);
      const next = answers.shift();
      if (next instanceof Error) throw next;
      return next as ModelCheck;
    };
    return { ask, asked };
  }

  afterEach(() => {
    vi.useRealTimers();
  });

  it("asks until the test ends and answers what it found", async () => {
    vi.useFakeTimers();
    const sent = route(running);
    const { ask, asked } = asking([
      running,
      new QueryError("unavailable"),
      { ...running, state: "ended", ended_at: found.at, check: found },
    ]);
    const answer = checkModel("opus", { smoke: true }, ask);
    await vi.advanceTimersByTimeAsync(6000);
    expect(await answer).toEqual({ kind: "ok", status: 202, value: found });
    expect(sent).toEqual(["POST /llm/providers/opus/check"]);
    expect(asked).toEqual(["opus", "opus", "opus"]);
  });

  it("answers a test that found nothing, was lost, or went, as a refusal saying why", async () => {
    vi.useFakeTimers();
    for (const [polled, code, sentence] of [
      [
        { ...running, state: "ended", ended_at: found.at, failure: "the node stopped" },
        "",
        "the node stopped",
      ],
      [{ ...running, state: "lost", failure: "lost on node-a" }, "", "lost on node-a"],
      [{ ...running, id: "check-2" }, "", "Another test of this model began"],
      [new QueryError("not_found"), "not_found", "The test's outcome is gone"],
    ] as const) {
      route(running);
      const { ask } = asking([polled]);
      const answer = checkModel("opus", { smoke: true }, ask);
      await vi.advanceTimersByTimeAsync(2000);
      const got = await answer;
      expect(got.kind).toBe("refused");
      if (got.kind === "refused") {
        expect(got.code).toBe(code);
        expect(got.sentence).toContain(sentence);
      }
    }
  });

  it("stops waiting at once on the caller's abort", async () => {
    vi.useFakeTimers();
    const sent = route(running);
    const { ask, asked } = asking([]);
    const abort = new AbortController();
    const answer = checkModel("opus", { smoke: true }, ask, abort.signal);
    await vi.advanceTimersByTimeAsync(500);
    abort.abort();
    await expect(answer).rejects.toMatchObject({ name: "AbortError" });
    expect(sent).toEqual(["POST /llm/providers/opus/check"]);
    expect(asked).toEqual([]);
  });
});
