/**
 * The fleet rules the page mirrors from the engine (a member's class and
 * harness, a fleet's name, its new key) and the `llm-fleets` writes: a create
 * is create-only, a change is made under the fleet's own tag, and a delete
 * moves every place that names the fleet.
 */

import { beforeEach, describe, expect, test, vi } from "vitest";
import type { Fleet } from "~/protocol/fleets.ts";
import type { ModelView } from "../data.ts";
import {
  classOf,
  createFleet,
  deleteFleet,
  keyFromName,
  misfitOf,
  nameProblem,
  onFleet,
  saveFleet,
  wordOf,
} from "./data.ts";

const calls = vi.hoisted(() => ({
  log: [] as { what: string; key: string; body?: unknown; etag?: string; headers?: unknown }[],
  entity: { kind: "entity", entity: {}, etag: '"t1"' } as Record<string, unknown>,
}));
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getEntity: async () => calls.entity,
  putEntity: async (_k: string, key: string, body: unknown, etag: string) => {
    calls.log.push({ what: "put", key, body, etag });
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
  createEntity: async (_k: string, key: string, body: unknown) => {
    calls.log.push({ what: "create", key, body });
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
}));
vi.mock("~/protocol/rest.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/rest.ts")>()),
  rest: {
    request: async (method: string, path: string, init: { body?: unknown; headers?: unknown }) => {
      calls.log.push({
        what: `${method} ${path}`,
        key: "",
        body: init?.body,
        headers: init?.headers,
      });
      if (path.endsWith("/references"))
        return {
          status: 200,
          etag: null,
          marked: true,
          body: {
            references: [{ at: { seat: "qa", job: "executor" } }, { at: { advisor: "findings" } }],
          },
        };
      return {
        status: 200,
        etag: null,
        marked: true,
        body: { revision_id: "r3", epoch: 3, warnings: [] },
      };
    },
  },
}));

beforeEach(() => {
  calls.log = [];
  calls.entity = { kind: "entity", entity: {}, etag: '"t1"' };
});

const model = (harness: string, id: string) => ({ harness, model: id }) as ModelView;
const fleet = (key: string, name: string): Fleet => ({
  key,
  name,
  is_default: key === "default-fleet",
  class: "",
  harness: "",
  strategy: "balanced",
  threshold_points: 5,
  spread: null,
  members: [],
});

describe("the engine's rules, mirrored", () => {
  test("a model's class is the Claude tier or GPT-5 Codex by its id, else the id itself", () => {
    expect(classOf("claude-opus-4-5")).toBe("Claude Opus");
    expect(classOf("sonnet")).toBe("Claude Sonnet");
    expect(classOf("claude-haiku-5")).toBe("Claude Haiku");
    expect(classOf("gpt-5-codex")).toBe("GPT-5 Codex");
    expect(classOf("qwen3-coder")).toBe("qwen3-coder");
  });

  test("a member of another class, or of the same class on another harness, is refused in the engine's words", () => {
    const first = model("claude-code", "opus");
    expect(misfitOf(model("claude-code", "claude-opus-5"), first)).toBeNull();
    expect(misfitOf(model("claude-code", "sonnet"), first)).toBe(
      "Runs Claude Sonnet; this fleet runs Claude Opus.",
    );
    expect(misfitOf(model("opencode", "anthropic/claude-opus-5"), first)).toBe(
      "Runs Claude Opus on opencode; this fleet runs Claude Opus on claude-code.",
    );
  });

  test("a fleet's name is given, at most 60 characters, and its own ignoring case", () => {
    const fleets = [fleet("default-fleet", "default-fleet"), fleet("claude-opus", "Claude Opus")];
    expect(nameProblem("  ", null, fleets)).toBe("Give the fleet a name.");
    expect(nameProblem("x".repeat(61), null, fleets)).toBe("Use at most 60 characters.");
    expect(nameProblem("claude opus", null, fleets)).toBe("Another fleet is called Claude Opus.");
    // ITS OWN NAME is no clash while it is edited.
    expect(nameProblem("Claude Opus", "claude-opus", fleets)).toBeNull();
  });

  test("a new key comes from the name, DNS-safe, and takes the next free suffix on a clash", () => {
    expect(keyFromName("Claude Opus!", new Set())).toBe("claude-opus");
    expect(keyFromName("Claude Opus", new Set(["claude-opus", "claude-opus-2"]))).toBe(
      "claude-opus-3",
    );
    expect(keyFromName("***", new Set())).toBe("fleet");
    const long = keyFromName("a".repeat(70), new Set(["a".repeat(63)]));
    expect(long.length).toBeLessThanOrEqual(63);
    expect(long.endsWith("-2")).toBe(true);
  });

  test("a value is said as a fleet's name, a model's key, or Not set", () => {
    const fleets = [fleet("claude-opus", "Claude Opus")];
    expect(wordOf("fleet:claude-opus", fleets)).toBe("Fleet · Claude Opus");
    expect(wordOf("fleet:gone", fleets)).toBe("Fleet · gone");
    expect(wordOf("sonnet", fleets)).toBe("sonnet");
    expect(wordOf("", fleets)).toBe("Not set");
  });

  test("who is on a fleet is every seat or helper with any job on it", () => {
    const on = onFleet(
      [
        {
          subject: "a",
          kind: "seat",
          name: "A",
          target: { fleet: "f" },
          now: null,
          waiting_until: null,
        },
        {
          subject: "b",
          kind: "seat",
          name: "B",
          target: { pinned: "" },
          now: null,
          waiting_until: null,
          jobs: [{ job: "review", target: { fleet: "f" }, now: null, waiting_until: null }],
        },
        {
          subject: "c",
          kind: "helper",
          name: "C",
          target: { pinned: "x" },
          now: "x",
          waiting_until: null,
        },
      ],
      "f",
    );
    expect(on.map((a) => a.subject)).toEqual(["a", "b"]);
  });
});

describe("the llm-fleets writes", () => {
  test("a new fleet is created create-only, with no read of an entity that is not there", async () => {
    await createFleet("opus", { name: "Opus", strategy: "balanced", members: [] }, "Create");
    expect(calls.log).toEqual([
      { what: "create", key: "opus", body: { name: "Opus", strategy: "balanced", members: [] } },
    ]);
  });

  test("a change is written under the fleet's own tag", async () => {
    await saveFleet("opus", { name: "Opus", strategy: "sequential", members: ["a", "b"] }, "Edit");
    expect(calls.log).toEqual([
      {
        what: "put",
        key: "opus",
        body: { name: "Opus", strategy: "sequential", members: ["a", "b"] },
        etag: '"t1"',
      },
    ]);
  });

  test("a fleet deleted since the page read it is a conflict, never a create", async () => {
    calls.entity = { kind: "missing" };
    const outcome = await saveFleet(
      "gone",
      { name: "Gone", strategy: "balanced", members: [] },
      "x",
    );
    expect(outcome.kind).toBe("conflict");
    expect(calls.log).toEqual([]);
  });

  test("a delete moves every place that names the fleet to the one choice, under its tag", async () => {
    await deleteFleet("opus", "fleet:default-fleet", "Delete the Opus fleet");
    const del = calls.log.find((c) => c.what.startsWith("DELETE"))!;
    expect(del.what).toBe("DELETE /config/llm-fleets/opus");
    expect(del.body).toEqual({
      repoint: [
        { at: { seat: "qa", job: "executor" }, to: "fleet:default-fleet" },
        { at: { advisor: "findings" }, to: "fleet:default-fleet" },
      ],
    });
    expect(del.headers).toEqual({ "If-Match": '"t1"', "X-Summary": "Delete the Opus fleet" });
  });
});
