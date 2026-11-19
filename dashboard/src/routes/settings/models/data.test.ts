import { describe, expect, it } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import { ASSISTANT_CHAT, modelEntry, modelsDoc } from "./fixtures.ts";
import { assistantOf, modelViews } from "./data.ts";

/** Three models, `b` answering first and `c` behind it, as the engine reports the chain. */
function models() {
  return modelViews(
    {
      reading: "admin",
      node: "node-1",
      nodes: [{ id: "node-1", reported: true }],
      entries: [
        modelEntry({ key: "a" }),
        modelEntry({ key: "c", used_by: [{ assistant: "chat", position: 1 }] }),
        modelEntry({
          key: "b",
          used_by: [ASSISTANT_CHAT],
        }),
      ],
      alarms: [],
    },
    null,
  );
}

describe("the assistant's chain", () => {
  // THE WHOLE CHAIN, never its head alone: a change of its model keeps the
  // fallbacks, so the page has to know them.
  it("reads the document's chain, the first model and the fallbacks behind it", () => {
    const doc = {
      ...modelsDoc({ a: {}, b: {}, c: {} }),
      assistant: { enabled: true, llm: ["c", "a", "b"] },
    } as CompanyDocument;
    expect(assistantOf(doc, models())).toEqual({ enabled: true, key: "c", fallbacks: ["a", "b"] });
    const one = { ...doc, assistant: { enabled: false, llm: "a" } } as CompanyDocument;
    expect(assistantOf(one, models())).toEqual({ enabled: false, key: "a", fallbacks: [] });
  });

  it("reads a member's chain from where the engine says the chat runs, in order", () => {
    expect(assistantOf(null, models())).toEqual({ enabled: true, key: "b", fallbacks: ["c"] });
    expect(assistantOf(null, [])).toEqual({ enabled: false, key: null, fallbacks: [] });
  });
});
