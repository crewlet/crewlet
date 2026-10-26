// @vitest-environment node
/** What the artifacts' screens say: history rows as sentences, audiences and roles. */

import { describe, expect, test } from "vitest";
import type { ArtifactHistoryEntry, DataHistoryEntry } from "~/protocol/index.ts";
import {
  actorName,
  audienceKey,
  cellChangeWords,
  dataChangeWords,
  dataRefusalReason,
  dataRefusalText,
  historyOpTag,
  historyRefusalWords,
  historyWords,
  KIND_LIST,
  KIND_WORDS,
  purgeAt,
  purgeSentence,
  roleIncludes,
} from "./words.ts";

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
    actor: { name: "maya", kind: "human" },
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

  // A MOVE NAMES FOLDERS BY ID ALONE, so the sentence says where the
  // artifact went and never a folder's name a reader may not be shown.
  test("a move says where it went and names no folder", () => {
    expect(historyWords(entry({ op: "move", summary: { to_folder: "f-1" } }), names)).toBe(
      "filed it in a folder",
    );
    expect(
      historyWords(entry({ op: "move", summary: { from_folder: "f-1", to_folder: "f-2" } }), names),
    ).toBe("moved it to another folder");
    expect(historyWords(entry({ op: "move", summary: { from_folder: "f-1" } }), names)).toBe(
      "moved it to the top level",
    );
    expect(
      historyWords(entry({ op: "move", outcome: "refused", summary: { to_folder: "f-1" } }), names),
    ).toBe("tried to file it in a folder and was refused");
    expect(historyOpTag("move")).toBe("move");
  });

  test("a purge's first step reads as the deletion beginning", () => {
    expect(historyWords(entry({ op: "purge_begin" }), names)).toBe("began deleting it for good");
    expect(historyWords(entry({ op: "purge_begin", outcome: "refused" }), names)).toBe(
      "tried to begin deleting it for good and was refused",
    );
  });

  // AN ADMINISTRATOR'S READ SAID IT TWICE: "opened version 1 as an
  // administrator (as an administrator)", the row's flag appended to a
  // sentence that already names it.
  test("a change made as an administrator says so once", () => {
    expect(historyWords(entry({ op: "read_elevated", number: 1, elevated: true }), names)).toBe(
      "opened version 1 as an administrator",
    );
    expect(
      historyWords(
        entry({ op: "share", elevated: true, summary: { audience: "seat:pm", role: "viewer" } }),
        names,
      ),
    ).toBe("shared it with Pat Mills: can view (as an administrator)");
  });

  test("an asset's rows are said as the asset's", () => {
    expect(historyWords(entry({ op: "asset_create", summary: { name: "logo.png" } }), names)).toBe(
      "added the asset logo.png",
    );
    expect(historyWords(entry({ op: "asset_remove", summary: { name: "logo.png" } }), names)).toBe(
      "removed the asset logo.png",
    );
  });

  test("a link names the work item it put the artifact on", () => {
    expect(historyWords(entry({ op: "link_work_item", work_item: "ENG-12" }), names)).toBe(
      "linked it to ENG-12",
    );
    expect(
      historyWords(entry({ op: "link_work_item", work_item: "ENG-12", outcome: "refused" }), names),
    ).toBe("tried to link it to ENG-12 and was refused");
  });

  test("an op this build does not know is still said, in its own words", () => {
    expect(historyWords(entry({ op: "rename_collection" }), names)).toBe("rename collection");
  });
});

describe("an actor", () => {
  test("is the seat's name, or the engine for a system write", () => {
    expect(actorName({ name: "maya", kind: "human" }, names.seat)).toBe("Maya Ortiz");
    expect(actorName({ name: "compactor", kind: "system" }, names.seat)).toBe("the engine");
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

describe("a datastore write in words", () => {
  const entry = (over: Partial<DataHistoryEntry>): DataHistoryEntry => ({
    seq: 1,
    op_id: "op",
    op: "write",
    verdict: "applied",
    actor: { name: "miles", kind: "human" },
    at: "2026-10-18T10:00:00Z",
    changes: [],
    digest: "",
    ...over,
  });
  const label = (_: string, field: string) => ({ notes: "Notes", gate: "Gate" })[field] ?? field;

  test("names the columns it changed by their labels", () => {
    expect(
      dataChangeWords(
        entry({ changes: [{ c: "gates", d: "r7", op: "update", f: [{ f: "notes" }] }] }),
        label,
      ),
    ).toBe("changed Notes in 1 row");
  });

  test("a refused write says what it tried, and never the applier's own reason", () => {
    const refused = entry({
      verdict: "invalid",
      reason:
        "op 0: notes is long text, written whole only guarded: name the if_version you edited, or append",
      changes: [{ c: "gates", d: "r7", op: "update", f: [{ f: "notes" }] }],
    });
    const said = dataChangeWords(refused, label);
    expect(said).toBe("tried to change Notes in 1 row");
    expect(said).not.toMatch(/op 0|if_version/);
    expect(dataRefusalReason("conflict")).toBe("It changed since it was read.");
    expect(dataRefusalText("invalid")).toBe("Not saved: the value does not fit this column.");
  });

  test("a refused write with nothing recorded says it tried to make a change", () => {
    expect(dataChangeWords(entry({ verdict: "conflict" }), label)).toBe("tried to make a change");
  });
});

describe("a cell's change in its history", () => {
  test("says what it was set to, never “empty to”", () => {
    expect(cellChangeWords({ f: "gate", a: "Settlement algorithm" })).toBe(
      "Set to “Settlement algorithm”",
    );
    expect(cellChangeWords({ f: "gate", b: "a", a: "b" })).toBe("Changed from “a” to “b”");
    expect(cellChangeWords({ f: "gate", b: "a" })).toBe("Cleared “a”");
    expect(cellChangeWords({ f: "tags", b: [], a: ["x", "y"] })).toBe("Set to “x, y”");
    expect(cellChangeWords({ f: "notes", bh: "d1", ah: "d2" })).toBe("Changed the text");
  });
});

describe("the trash", () => {
  test("names the day an artifact is deleted for good, from the engine's retention", () => {
    expect(purgeAt("2026-10-18T10:00:00Z")).toBe("2026-11-17T10:00:00.000Z");
    expect(purgeSentence("2026-10-18T10:00:00Z")).toMatch(/^It is deleted for good on .+ unless/);
    expect(purgeSentence("2026-10-18T10:00:00Z")).not.toMatch(/30 days/);
  });

  test("an unknown trash time says the retention rather than a wrong date", () => {
    expect(purgeAt("0001-01-01T00:00:00Z")).toBeNull();
    expect(purgeSentence(undefined)).toMatch(/30 days after it was trashed/);
  });
});

describe("the words other screens say an artifact's facts in", () => {
  test("the kind list names every kind the engine has, in its order", () => {
    expect(KIND_LIST).toBe("HTML pages, docs, slides, dashboards, sheets, designs and brand kits");
  });

  // "PAGE" IS DOCLET'S WORD: the html kind beside Doclet's Pages named
  // two things one way.
  test("an html artifact is called HTML, and no kind is called a page", () => {
    expect(KIND_WORDS.html).toBe("HTML");
    expect(Object.values(KIND_WORDS)).not.toContain("Page");
  });

  test("an op is a word a reader knows, and an unknown one keeps its name", () => {
    expect(historyOpTag("read_elevated")).toBe("admin read");
    expect(historyOpTag("patch")).toBe("edit");
    expect(historyOpTag("restore")).toBe("untrash");
    expect(historyOpTag("link_work_item")).toBe("link work item");
    expect(historyOpTag("asset_create")).toBe("add asset");
    expect(historyOpTag("some_new_op")).toBe("some new op");
  });

  test("a refusal reason is a clause, never the log's code", () => {
    expect(historyRefusalWords("head_moved")).toBe("a newer version was published first");
    expect(historyRefusalWords("quota_bytes")).toBe("the company has reached its storage limit");
    expect(historyRefusalWords("some_new_reason")).toBe("some new reason");
  });
});
