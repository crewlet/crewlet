import { describe, expect, test } from "vitest";

import {
  benchParts,
  benchWords,
  editSummary,
  formErrors,
  formOf,
  keyBackWords,
  keyMark,
  keysKeepInlinePlaces,
  modelEntity,
  modelLine,
  nextLift,
  problemsByField,
  usesOf,
} from "./models.ts";
import type { Seat } from "./seats.ts";
import type { CredentialKeyRow, CredentialPoolRow } from "~/contract/credentials.ts";

const seat = (name: string, llm?: Record<string, string[]>) =>
  ({
    key: name,
    name,
    handle: name.toLowerCase(),
    kind: "agent",
    raw: { name, llm },
  }) as unknown as Seat;

describe("which seats run on a model", () => {
  // THE ENGINE'S CHAIN, read: first is what the phase runs on, later a fallback.
  test("is the chain each seat resolved to, first versus later", () => {
    const uses = usesOf(
      [
        seat("CEO", { execute: ["smart", "fast"], review: ["smart"] }),
        seat("SWE", { execute: ["fast"] }),
        seat("Ana"),
      ],
      "fast",
    );
    expect(uses.map((u) => [u.seat.name, u.runs, u.fallback, u.every])).toEqual([
      ["CEO", [], ["executor"], false],
      ["SWE", ["executor"], [], true],
    ]);
  });

  // THE EXECUTOR FIRST, whatever order the projection's keys arrive in.
  test("lists phases in the engine's order, not the answer's", () => {
    const [use] = usesOf([seat("CEO", { review: ["m"], judge: ["x"], execute: ["m"] })], "m");
    expect(use?.runs).toEqual(["executor", "reviewer"]);
    expect(use?.every).toBe(false);
  });
});

describe("the edit", () => {
  const entity = {
    type: "anthropic",
    model: "m1",
    api_keys: ["${A}", "__redacted__"],
    reasoning: true,
    cooldowns: { rate_limit_seconds: 600 },
  };

  // EVERYTHING THE FORM DOES NOT SHOW GOES BACK AS READ, the mask included.
  test("sends back what it did not show exactly as read", () => {
    const form = formOf(entity);
    expect(form.rateLimit).toBe("600");
    expect(modelEntity(entity, form)).toEqual(entity);
    const cleared = modelEntity(entity, { ...form, rateLimit: "", keys: [] });
    expect(cleared).toEqual({ type: "anthropic", model: "m1", reasoning: true });
  });

  // A SUMMARY NAMES VARIABLES, NEVER A VALUE, and never the mask.
  test("summarises by name, and never mentions the mask", () => {
    const before = formOf(entity);
    const after = { ...before, keys: [{ id: "x", value: "${B}" }, before.keys[1]!] };
    const summary = editSummary("smart", before, after);
    expect(summary).toBe("Edit the model smart: keys added: ${B}; keys removed: ${A}");
    expect(summary).not.toContain("__redacted__");
  });

  test("refuses a bench time outside what the engine accepts, and an empty key row", () => {
    const form = formOf(entity);
    expect(formErrors({ ...form, auth: "59" }, form).auth).toMatch(/from 60 to 86400/);
    expect(formErrors({ ...form, auth: "86400" }, form).auth).toBeUndefined();
    expect(
      formErrors({ ...form, keys: [...form.keys, { id: "y", value: " " }] }, form).keys,
    ).toBeDefined();
  });

  // AN INLINE KEY IS PUT BACK BY ITS PLACE, so a list holding one keeps its
  // length and the mask its index. [${A}, <inline>], remove key 1 and add
  // ${C}: the same length, the mask at index 0 — and the engine would restore
  // it from ${A}, swapping the inline key for another with nothing refused.
  test("refuses any list that moves an inline key off the place it was read at", () => {
    const read = formOf(entity);
    const [a, inline] = read.keys as [(typeof read.keys)[0], (typeof read.keys)[0]];
    const c = { id: "c", value: "${C}" };
    const swapped = { ...read, keys: [inline, c] };
    expect(keysKeepInlinePlaces(read.keys, swapped.keys)).toBe(false);
    expect(formErrors(swapped, read).keys).toMatch(/Key 1 is written inline/);
    // Grown or shrunk while the mask stands: refused.
    expect(keysKeepInlinePlaces(read.keys, [a, inline, c])).toBe(false);
    expect(keysKeepInlinePlaces(read.keys, [inline])).toBe(false);
    // Kept in its place, the neighbour edited: fine.
    expect(keysKeepInlinePlaces(read.keys, [c, inline])).toBe(true);
    expect(formErrors({ ...read, keys: [c, inline] }, read).keys).toBeUndefined();
    // Replaced in its place, the list may then change freely.
    expect(keysKeepInlinePlaces(read.keys, [c])).toBe(true);
    expect(keysKeepInlinePlaces(read.keys, [a, c, { id: "d", value: "${D}" }])).toBe(true);
  });

  // BY THE PROBLEM'S SEGMENTS, never its message; another model's problem is the rest.
  test("places the engine's problems beside their fields by segment", () => {
    const { fields, rest } = problemsByField(
      [
        {
          path: "",
          segments: ["providers", "llm", "smart", "api_keys", 1],
          kind: "invalid",
          message: "k",
        },
        {
          path: "",
          segments: ["providers", "llm", "smart", "cooldowns", "auth_seconds"],
          kind: "invalid",
          message: "a",
        },
        {
          path: "",
          segments: ["providers", "llm", "fast", "model"],
          kind: "invalid",
          message: "other",
        },
      ] as never,
      "smart",
      { api_keys: ["${A}", "${B}"] },
    );
    expect(fields).toEqual({ keys: "k", auth: "a" });
    expect(rest).toEqual(["other"]);
  });

  // A PROBLEM ON A KEY THIS PAGE SENT AS THE MASK is this screen's sentence,
  // not the engine's whole-document one about seats, units and MCP servers.
  test("says a key it sent as the mask in its own words", () => {
    const { fields } = problemsByField(
      [
        {
          path: "",
          segments: ["providers", "llm", "smart", "api_keys", 0],
          kind: "unknown_value",
          message: "still holds the redaction marker: a seat is matched by its handle",
        },
      ] as never,
      "smart",
      { api_keys: ["__redacted__", "${B}"] },
    );
    expect(fields.keys).toMatch(/^Key 1 is written inline/);
    expect(fields.keys).not.toMatch(/seat|unit|MCP/);
  });
});

test("the next key back is the earliest cooling deadline across every model", () => {
  const row = (until: (string | null)[]) =>
    ({
      keys: until.map((u) => ({ state: u ? "cooling" : "ready", cooling_until: u })),
    }) as unknown as CredentialPoolRow;
  expect(nextLift([row([null, "2026-09-29T12:40:00Z"]), row(["2026-09-29T12:05:00Z"])])).toBe(
    "2026-09-29T12:05:00Z",
  );
  expect(nextLift([row([null])])).toBeNull();
});

// ONE CLOCK: a bench is written in the unit letters every span on the
// dashboard uses ("41m" on a cooling key), and named for what benches a key,
// never by a status code a reader has to already know.
test("a bench is named for its cause and written as every other span", () => {
  expect(benchWords(3600)).toBe("1h");
  expect(benchWords(300)).toBe("5m");
  expect(benchWords(5400)).toBe("1h 30m");
  expect(benchWords(90)).toBe("1m 30s");
  const parts = benchParts({ rate_limit_seconds: 3600, auth_seconds: 300 });
  expect(parts.map((p) => p.words)).toEqual(["rate limit 1h", "auth 5m"]);
  expect(parts.map((p) => p.words).join(" ")).not.toMatch(/429|401/);
});

// A cli-agent entry names no model, and its line is its type alone.
test("a model's line joins the id only when there is one", () => {
  expect(modelLine({ type: "cli-agent", model: "" })).toBe("cli-agent");
  expect(modelLine({ type: "anthropic", model: "m" })).toBe("anthropic · m");
});

describe("a key on the list", () => {
  const key = (over: Partial<CredentialKeyRow>): CredentialKeyRow => ({
    ref: "ACME_KEY",
    source: "reference",
    hint: "",
    state: "ready",
    cooling_until: null,
    same_as: 0,
    uses: 0,
    in_flight: 0,
    ...over,
  });

  // A KEY WITH NO NAME IS NAMED BY ITS PLACE AND WHERE IT LIVES, the way its
  // page names it — a bare "#1" beside the named marks read as a cipher.
  test("marks a named key by its variable and an inline one by its place", () => {
    expect(keyMark(key({}), 1)).toBe("ACME_KEY");
    expect(keyMark(key({ source: "default", ref: "OPENAI_API_KEY" }), 1)).toBe("OPENAI_API_KEY");
    expect(keyMark(key({ source: "inline", ref: "" }), 2)).toBe("key 2 · inline");
  });

  // A DUPLICATE IS IN THE POOL ONCE, as the key it repeats; only a key that
  // resolved to nothing, or is benched, is out of it.
  test("says a duplicate comes back as the key it repeats", () => {
    expect(keyBackWords(key({}))).toBe("Now");
    expect(keyBackWords(key({ state: "duplicate", same_as: 1 }))).toBe("As key 1");
    expect(keyBackWords(key({ state: "unresolved" }))).toBe("Not in the pool");
  });
});
