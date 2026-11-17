// @vitest-environment node
import { describe, expect, test } from "vitest";
import type { CodePrecondition } from "~/protocol/index.ts";
import { mirrorLink, placingWord, pusherRest } from "./rail.ts";

/** A live change whose readiness lists `pre`, the first met. */
const live = (...pre: Partial<CodePrecondition>[]) => ({
  state: "open",
  readiness: {
    met: 1,
    total: pre.length + 1,
    first: "Its owner merges it once a newer node says how.",
    preconditions: [
      { kind: "facts", state: "met", detail: "" },
      ...pre.map((p) => ({ kind: "check", state: "unmet", detail: "", ...p })),
    ],
  },
});

describe("a change's placing word", () => {
  test.each([
    [{ kind: "approval" }, "waits on review", undefined],
    [{ kind: "human_approval" }, "waits on review", undefined],
    [{ kind: "owners_approval" }, "waits on review", undefined],
    [{ kind: "check" }, "waits on checks", undefined],
    [{ kind: "blocker" }, "blocked by findings", undefined],
    [{ kind: "mergeable" }, "waits on a person", undefined],
    [{ kind: "merge_authority" }, "waits on a person", undefined],
    [{ kind: "queue" }, "in the queue", "info"],
    [{ kind: "facts", state: "computing" }, "being judged", undefined],
    [{ kind: "mergeable", state: "computing" }, "being judged", undefined],
    [{ kind: "a_newer_kind" }, "Its owner merges it once a newer node says how.", undefined],
  ] as const)("%o is %s", (pre, word, tone) => {
    expect(placingWord(live(pre), "jane")).toEqual(tone ? { word, tone } : { word });
  });

  test("is the reader's own where the first unmet condition waits on their seat", () => {
    const waits = { kind: "approval", waits_on: [{ handle: "jane", kind: "person" }] };
    expect(placingWord(live(waits), "jane")).toEqual({ word: "waits on you", tone: "warning" });
    expect(placingWord(live(waits), "cto")).toEqual({ word: "waits on review" });
    expect(placingWord(live(waits), "")).toEqual({ word: "waits on review" });
  });

  test("is draft for a draft and ready to merge where every condition is met", () => {
    expect(placingWord({ ...live({ kind: "check" }), state: "draft" }, "jane")).toEqual({
      word: "draft",
    });
    expect(placingWord(live(), "jane")).toEqual({ word: "ready to merge", tone: "success" });
  });
});

describe("a mirror's address", () => {
  test.each([
    ["https://github.com/nimbus/web.git", "https://github.com/nimbus/web", "github.com/nimbus/web"],
    ["https://github.com/nimbus/web", "https://github.com/nimbus/web", "github.com/nimbus/web"],
    [
      "http://forge.example.com/acme/web/",
      "http://forge.example.com/acme/web",
      "forge.example.com/acme/web",
    ],
    [
      "https://bot:s3cret@forge.example.com/web.git",
      "https://forge.example.com/web",
      "forge.example.com/web",
    ],
  ])("%s links its host's page", (url, href, text) => {
    expect(mirrorLink(url)).toEqual({ href, text });
  });

  test.each([
    ["ssh://git@forge.example.com/acme/web.git", "ssh://git@forge.example.com/acme/web.git"],
    ["ssh://git:s3cret@forge.example.com/web.git", "ssh://git@forge.example.com/web.git"],
    ["git@github.com:nimbus/web.git", "git@github.com:nimbus/web.git"],
  ])("%s is text, its password dropped", (url, text) => {
    expect(mirrorLink(url)).toEqual({ href: null, text });
  });
});

describe("the pushers past the named", () => {
  const top = [
    { kind: "agent", handle: "a", pushes: 9 },
    { kind: "gitlet", pushes: 4 },
    { kind: "agent", handle: "b", pushes: 2 },
  ];
  test("count the rows not drawn and the ones the engine left out", () => {
    const window = { from: "2026-10-17T00:00:00Z", until: "2026-11-16T00:00:00Z" };
    expect(pusherRest({ ...window, top }, 2)).toEqual({ pushers: 1, pushes: 2 });
    expect(pusherRest({ ...window, top, omitted: 3, omitted_pushes: 5 }, 2)).toEqual({
      pushers: 4,
      pushes: 7,
    });
    expect(pusherRest({ ...window, top }, 6)).toBeNull();
    expect(pusherRest(window, 6)).toBeNull();
  });
});
