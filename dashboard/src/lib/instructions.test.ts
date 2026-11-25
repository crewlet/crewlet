import { describe, expect, it } from "vitest";
import { ASSISTANT_INSTRUCTIONS_MAX_RUNES } from "~/contract/instructions.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  companyInstructionsPlan,
  draftOf,
  layerOf,
  layersInUse,
  overCap,
  sameLayer,
} from "./instructions.ts";

const doc = (instructions?: unknown) =>
  ({ assistant: { model: "assistant", instructions } }) as unknown as CompanyDocument;

describe("an instructions layer", () => {
  it("saves only what it sets, its text trimmed", () => {
    expect(layerOf({ length: "", format: "tldr", text: "  Use British spelling.  " })).toEqual({
      format: "tldr",
      text: "Use British spelling.",
    });
    expect(layerOf(draftOf(undefined))).toEqual({});
  });

  it("counts its text in code points, as the engine does, against the cap", () => {
    const atCap = "é".repeat(ASSISTANT_INSTRUCTIONS_MAX_RUNES);
    expect(overCap({ length: "", format: "", text: atCap })).toBe(false);
    expect(overCap({ length: "", format: "", text: `${atCap}🙂` })).toBe(true);
  });

  it("is the same as another that says the same once trimmed", () => {
    expect(sameLayer({ text: "a " }, { text: "a" })).toBe(true);
    expect(sameLayer({ length: "brief" }, {})).toBe(false);
  });

  it("is in use in the drawer only where a layer sets something", () => {
    expect(layersInUse({ company: {}, mine: { text: "  " } })).toEqual({
      company: false,
      mine: false,
    });
    expect(layersInUse({ company: { length: "brief" }, mine: {} })).toEqual({
      company: true,
      mine: false,
    });
    expect(layersInUse(undefined)).toEqual({ company: false, mine: false });
  });
});

describe("the company layer's change", () => {
  it("writes the whole layer, removing what it leaves out", () => {
    const plan = companyInstructionsPlan({ length: "brief", text: "x" }, { format: "lists" });
    expect(plan(doc({ length: "brief", text: "x" }))).toEqual({
      patch: { assistant: { instructions: { length: null, format: "lists", text: null } } },
      summary: "Ask Crewlet: company instructions changed (assistant.instructions)",
    });
  });

  it("removes the block when nothing is left", () => {
    const change = companyInstructionsPlan({ length: "brief" }, {})(doc({ length: "brief" }));
    expect(change).toMatchObject({ patch: { assistant: { instructions: null } } });
  });

  it("is nothing to save when the document already says it, and a conflict when it moved", () => {
    expect(companyInstructionsPlan({}, { length: "brief" })(doc({ length: "brief" }))).toBeNull();
    expect(companyInstructionsPlan({}, { length: "brief" })(doc({ format: "tldr" }))).toBe(
      "conflict",
    );
  });
});
