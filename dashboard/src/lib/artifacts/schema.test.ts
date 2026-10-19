// @vitest-environment node
/** The schema editor's checks and the templates a new sheet starts from. */

import { describe, expect, test } from "vitest";
import {
  collectionNameProblem,
  fieldNameProblem,
  hasOptions,
  keyFromLabel,
  optionID,
  reviewSheetSchema,
  schemaChangeCount,
  schemaProblem,
  SHEET_TEMPLATES,
  withPreset,
} from "./schema.ts";

describe("a collection's name", () => {
  test("is held to the engine's grammar, its reserved names and the names taken", () => {
    expect(collectionNameProblem("tasks", [])).toBeNull();
    expect(collectionNameProblem("Tasks", [])).toMatch(/lowercase/);
    expect(collectionNameProblem("1tasks", [])).toMatch(/lowercase/);
    expect(collectionNameProblem("history", [])).toMatch(/datastore's own routes/);
    expect(collectionNameProblem("tasks", ["tasks"])).toMatch(/already/);
  });
});

describe("a column's key", () => {
  test("is made from its label within the field grammar", () => {
    expect(keyFromLabel("Due date")).toBe("due_date");
    expect(keyFromLabel("  Café crème ")).toBe("cafe_creme");
    expect(keyFromLabel("2026 budget")).toBe("f_2026_budget");
    expect(keyFromLabel("!!!")).toBe("field");
    expect(fieldNameProblem(keyFromLabel("x".repeat(100)), [])).toBeNull();
  });

  test("is refused when it breaks the grammar or is taken", () => {
    expect(fieldNameProblem("_secret", [])).toMatch(/letter/);
    expect(fieldNameProblem("name", ["name"])).toMatch(/already/);
  });
});

describe("an option's id", () => {
  test("is made from its label and kept unique", () => {
    expect(optionID("In review", [])).toBe("in_review");
    expect(optionID("In review", ["in_review"])).toBe("in_review_2");
    expect(optionID("In review", ["in_review", "in_review_2"])).toBe("in_review_3");
    expect(optionID("???", [])).toBe("option");
    expect(optionID("Crème", [])).toBe("creme");
  });
});

describe("a policy picked", () => {
  test("sends the preset alone, leaving the rules it compiles to for the engine", () => {
    const [items] = reviewSheetSchema();
    if (!items) throw new Error("the review sheet has no collection");
    const changed = withPreset(items, "contribute");
    expect(changed.preset).toBe("contribute");
    expect(changed).not.toHaveProperty("read");
    expect(changed).not.toHaveProperty("create");
    expect(changed).not.toHaveProperty("delete");
    expect(withPreset(items, "shared")).toBe(items);
  });
});

describe("the templates", () => {
  test("are within the engine's bounds, with unique option ids and defaults among them", () => {
    for (const template of SHEET_TEMPLATES) {
      const collections = template.schema();
      expect(schemaProblem(collections), template.key).toBeNull();
      for (const column of collections.flatMap((c) => c.columns ?? [])) {
        if (!hasOptions(column.type)) continue;
        const ids = (column.options ?? []).map((o) => o.id);
        expect(new Set(ids).size, column.key).toBe(ids.length);
        if (column.default !== undefined) expect(ids).toContain(column.default);
      }
    }
  });

  test("the review sheet lets a reviewer fill in only the rows assigned to them", () => {
    const columns = reviewSheetSchema()[0]?.columns ?? [];
    const status = columns.find((c) => c.key === "status");
    expect(status).toMatchObject({
      write: "contributor",
      when: "assigned_to_viewer",
      assigned_field: "reviewer",
    });
    expect(columns.find((c) => c.key === "item")).toMatchObject({ write: "editor" });
  });

  test("a schema past the engine's bounds is said before it is sent", () => {
    const [items] = reviewSheetSchema();
    if (!items) throw new Error("the review sheet has no collection");
    const noChoices = {
      ...items,
      columns: [{ key: "pick", type: "select" as const, label: "Pick", options: [] }],
    };
    expect(schemaProblem([noChoices])).toMatch(/offers no choices/);
  });
});

// THE FOOTER COUNTS WHAT A READER CHANGED, one per collection, policy or
// column, so a long form says how much Save will apply before it is pressed.
describe("a schema draft's change count", () => {
  test("is one per collection added or removed, policy changed and column touched", () => {
    const before = reviewSheetSchema();
    expect(schemaChangeCount(before, before)).toBe(0);
    const [first, ...rest] = before;
    if (!first) throw new Error("the review template has a collection");
    const columns = first.columns ?? [];
    const edited = {
      ...first,
      title: "Renamed",
      columns: [
        { ...columns[0]!, label: "Changed" },
        ...columns.slice(2),
        { key: "fresh", type: "text" as const, label: "Fresh" },
      ],
    };
    // A policy, one column edited, one removed and one added.
    expect(schemaChangeCount(before, [edited, ...rest])).toBe(4);
    const added = {
      name: "extra",
      read: "all" as const,
      create: "editor" as const,
      delete: "editor" as const,
    };
    expect(schemaChangeCount(before, [...before, added])).toBe(1);
    expect(schemaChangeCount(before, rest)).toBe(1);
  });
});
