// @vitest-environment node
/** What the artifacts' screens say: history rows as sentences, audiences and roles. */

import { describe, expect, test } from "vitest";
import type { ArtifactHistoryEntry } from "~/protocol/index.ts";
import { actorName, audienceKey, historyWords, roleIncludes } from "./words.ts";

const names = {
  audience: (raw: string) => (raw === "seat:pm" ? "Pat Mills" : raw),
  seat: (handle: string) => (handle === "maya" ? "Maya Ortiz" : handle),
};

function entry(over: Partial<ArtifactHistoryEntry>): ArtifactHistoryEntry {
  return {
    position: 1,
    op_id: "op-1",
    op: "publish",
    outcome: "applied",
    actor: { Name: "maya", Kind: "human" },
    at: "2026-10-18T10:00:00Z",
    summary: {},
    ...over,
  };
}

describe("a history row", () => {
  test("reads as what was done", () => {
    expect(
      historyWords(entry({ op: "publish", number: 3, summary: { label: "Final" } }), names),
    ).toBe("published version 3 “Final”");
    expect(
      historyWords(entry({ op: "share", summary: { audience: "seat:pm", role: "editor" } }), names),
    ).toBe("shared it with Pat Mills: can edit");
    expect(historyWords(entry({ op: "transfer", summary: { to: "maya" } }), names)).toBe(
      "gave it to Maya Ortiz",
    );
    expect(
      historyWords(
        entry({ op: "restore_version", number: 5, summary: { restored_from: 2 } }),
        names,
      ),
    ).toBe("restored version 2 as version 5");
  });

  test("a refused row says what was tried and that it was refused", () => {
    expect(historyWords(entry({ op: "publish", number: 4, outcome: "refused" }), names)).toBe(
      "tried to publish version 4 and was refused",
    );
    expect(historyWords(entry({ op: "trash", outcome: "refused" }), names)).toBe(
      "tried to move it to the trash and was refused",
    );
  });

  test("a purge's first step reads as the deletion beginning", () => {
    expect(historyWords(entry({ op: "purge_begin" }), names)).toBe("began deleting it for good");
    expect(historyWords(entry({ op: "purge_begin", outcome: "refused" }), names)).toBe(
      "tried to begin deleting it for good and was refused",
    );
  });

  test("an op this build does not know is still said, in its own words", () => {
    expect(historyWords(entry({ op: "rename_collection" }), names)).toBe("rename collection");
  });
});

describe("an actor", () => {
  test("is the seat's name, or the engine for a system write", () => {
    expect(actorName({ Name: "maya", Kind: "human" }, names.seat)).toBe("Maya Ortiz");
    expect(actorName({ Name: "compactor", Kind: "system" }, names.seat)).toBe("the engine");
    expect(actorName(undefined, names.seat)).toBe("the engine");
  });
});

describe("an audience", () => {
  test("is spelled in a path as the engine spells it", () => {
    expect(audienceKey({ kind: "seat", id: "pm" })).toBe("seat:pm");
    expect(audienceKey({ kind: "unit", id: "ops" })).toBe("unit:ops");
    expect(audienceKey({ kind: "company" })).toBe("company");
  });
});

describe("a role", () => {
  test("includes every role below it on the ladder, and nothing is no role", () => {
    expect(roleIncludes("owner", "editor")).toBe(true);
    expect(roleIncludes("editor", "contributor")).toBe(true);
    expect(roleIncludes("viewer", "contributor")).toBe(false);
    expect(roleIncludes("", "viewer")).toBe(false);
  });
});
