import { describe, expect, it } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import { ASSISTANT_CHAT, modelEntry, modelsDoc } from "./fixtures.ts";
import { assistantOf, modelViews } from "./data.ts";

/** Three models, Ask Crewlet running on `b`, as the engine reports it. */
function models() {
  return modelViews(
    {
      reading: "admin",
      node: "node-1",
      nodes: [{ id: "node-1", reported: true }],
      entries: [
        modelEntry({ key: "a" }),
        modelEntry({ key: "c" }),
        modelEntry({ key: "b", used_by: [ASSISTANT_CHAT] }),
      ],
      alarms: [],
      executor_harnesses: ["claude-code", "opencode"],
    },
    null,
  );
}

describe("Ask Crewlet's model", () => {
  it("reads the document's one key, and none where it names none", () => {
    const doc = {
      ...modelsDoc({ a: {}, b: {}, c: {} }),
      assistant: { model: "c" },
    } as CompanyDocument;
    expect(assistantOf(doc, models())).toEqual({ key: "c" });
    const off = { ...modelsDoc({ a: {}, b: {}, c: {} }) } as CompanyDocument;
    expect(assistantOf(off, models())).toEqual({ key: null });
  });

  it("reads a member's from where the engine says the chat runs", () => {
    expect(assistantOf(null, models())).toEqual({ key: "b" });
    expect(assistantOf(null, [])).toEqual({ key: null });
  });
});

// A MODEL CAN RUN CODING WHERE THE ENGINE SAYS ITS HARNESS RUNS AN EXECUTOR, the
// served list read rather than one compiled into the page.
describe("a model's coding", () => {
  it("follows the engine's executor_harnesses", () => {
    const views = (executors: string[]) =>
      modelViews(
        {
          reading: "admin",
          node: "node-1",
          nodes: [{ id: "node-1", reported: true }],
          entries: [modelEntry({ key: "p", harness: "pi" }), modelEntry({ key: "c" })],
          alarms: [],
          executor_harnesses: executors,
        },
        null,
      ).map((m) => [m.key, m.executor]);
    expect(views(["claude-code", "opencode"])).toEqual([
      ["p", false],
      ["c", true],
    ]);
    expect(views(["claude-code", "opencode", "pi"])).toEqual([
      ["p", true],
      ["c", true],
    ]);
  });
});
