import { describe, expect, it } from "vitest";
import { referencesOf } from "./api.ts";

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
