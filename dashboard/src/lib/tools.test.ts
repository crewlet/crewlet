import { describe, expect, it } from "vitest";
import {
  capabilityOf,
  capabilityTone,
  hintSentence,
  hintsAdvertised,
  schemaFields,
} from "./tools.ts";
import type { ToolAnnotations, ToolRow } from "~/protocol/types.ts";

function ann(over: Partial<ToolAnnotations> = {}): ToolAnnotations {
  return {
    read_only: "unknown",
    destructive: "unknown",
    idempotent: "unknown",
    open_world: "unknown",
    ...over,
  };
}

function tool(over: Partial<ToolRow> = {}): ToolRow {
  return {
    name: "t",
    description: "",
    source: "builtin",
    annotations: ann(),
    delivers: "",
    ...over,
  };
}

describe("what a tool can do", () => {
  it("never reads an unadvertised hint as a denial", () => {
    // A fresh MCP server annotates nothing. Treating unknown as "not a
    // write" would present its every tool as a proven read on the one
    // screen an operator audits it on — which is the engine's own rule
    // (`Registry.KnownReads`), stated positively for the same reason.
    expect(capabilityOf(ann())).toBe("unknown");
    expect(capabilityOf(undefined)).toBe("unknown");
    expect(capabilityTone(capabilityOf(ann()))).toBeUndefined();
  });

  it("answers with the strongest thing the hints assert", () => {
    expect(capabilityOf(ann({ read_only: "yes" }))).toBe("reads");
    expect(capabilityOf(ann({ read_only: "no" }))).toBe("writes");
    // Both a write and irreversible: the column answers "what is the most
    // this could do", so the worse of the two wins.
    expect(capabilityOf(ann({ read_only: "no", destructive: "yes" }))).toBe("destroys");
    // And destructive alone still outranks a read-only claim that
    // contradicts it, rather than being hidden behind it.
    expect(capabilityOf(ann({ read_only: "yes", destructive: "yes" }))).toBe("destroys");
  });
});

describe("the hint sentence", () => {
  it("names what was NOT advertised, because that is what an operator acts on", () => {
    const said = hintSentence(ann({ read_only: "no", open_world: "yes" }));
    expect(said).toContain("can write");
    expect(said).toContain("reaches outside the company");
    expect(said).toContain("nothing said about destructive, idempotent");
  });

  it("says so plainly when a server advertised nothing at all", () => {
    expect(hintSentence(ann())).toBe("nothing was advertised about what this tool does");
  });

  it("distinguishes a denial from a silence", () => {
    expect(hintSentence(ann({ destructive: "no" }))).toContain("is reversible");
    expect(hintSentence(ann({ destructive: "unknown" }))).not.toContain("reversible");
  });
});

describe("how much a server said", () => {
  it("counts only the hints that assert something either way", () => {
    expect(hintsAdvertised(ann())).toBe(0);
    expect(hintsAdvertised(ann({ read_only: "no", destructive: "no" }))).toBe(2);
    expect(
      hintsAdvertised(
        ann({ read_only: "yes", destructive: "no", idempotent: "yes", open_world: "no" }),
      ),
    ).toBe(4);
  });
});

describe("a tool's arguments", () => {
  it("reads the schema's own field names, marking what is required", () => {
    const fields = schemaFields(
      tool({
        input_schema: {
          type: "object",
          properties: { key: { type: "string" }, limit: { type: "integer" } },
          required: ["key"],
        },
      }),
    );
    expect(fields).toEqual([
      { name: "key", type: "string", required: true },
      { name: "limit", type: "integer", required: false },
    ]);
  });

  it("is empty for a tool that sent no schema, and for one with no properties", () => {
    expect(schemaFields(tool())).toEqual([]);
    expect(schemaFields(tool({ input_schema: { type: "object" } }))).toEqual([]);
  });

  it("renders a union type rather than claiming one half of it", () => {
    const fields = schemaFields(
      tool({ input_schema: { properties: { note: { type: ["string", "null"] } } } }),
    );
    expect(fields[0]?.type).toBe("string | null");
  });

  it("reports an unreadable type as absent rather than as a claim", () => {
    const fields = schemaFields(tool({ input_schema: { properties: { odd: { enum: [1, 2] } } } }));
    expect(fields[0]?.type).toBe("");
  });
});
