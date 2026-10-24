/**
 * A new sheet's draft: typed from an import by the grid's own parser, shaped
 * column by column, and sent as the schema and rows the engine takes.
 */

import { describe, expect, test } from "vitest";
import type { SheetPerson } from "@crewlethq/ui";
import { DATA_BATCH_MAX_OPS } from "~/contract/artifacts.ts";
import { SHEET_TEMPLATES, schemaProblem, trackerSheetSchema, reviewSheetSchema } from "./schema.ts";
import type { DataCollection } from "~/protocol/index.ts";
import { COLUMN_TYPE_WORDS } from "./words.ts";
import {
  BRIEF_ROWS_MAX,
  draftBrief,
  draftColumns,
  draftFromTable,
  draftOf,
  draftProblem,
  draftWrites,
  inferColumn,
  insertBatches,
  titleFromFile,
  typeFromName,
  valueContext,
  withColumnMoved,
  withColumnSettings,
  withEdits,
  withNewColumn,
  withoutColumn,
} from "./sheetDraft.ts";

const people: SheetPerson[] = [
  { id: "seat:maya", name: "Maya Ortiz", kind: "human" },
  { id: "seat:pm", name: "PM", kind: "agent" },
];
const context = valueContext(people);
const bare = valueContext(undefined);

describe("typing an imported column", () => {
  test.each([
    ["Approved", ["yes", "no", "Yes", ""], "checkbox"],
    ["Budget", ["12,500", "800", "$2,000", "0"], "number"],
    ["Flag", ["0", "1", "1", "0"], "number"],
    ["Due", ["2026-11-02", "2026-11-09"], "date"],
    ["Site", ["https://example.com/a", "http://example.com/b"], "url"],
    ["Owner", ["Maya Ortiz", "PM", "maya ortiz"], "person"],
    ["Notes", ["short", "a\nsecond line"], "longtext"],
    ["Gate", ["Security review", "Pricing page", "Load test"], "text"],
  ])("%s holding %j is %s", (label, values, type) => {
    expect(inferColumn("c", label, values, context).type).toBe(type);
  });

  test("a value that fits no stricter type keeps the column text, and the grid counts it as unfit nowhere", () => {
    expect(inferColumn("c", "Due date", ["2026-11-02", "soon"], bare).type).toBe("text");
  });

  test("repeated words become choices, each with its own hue and an id from its label", () => {
    const column = inferColumn(
      "c",
      "Tier",
      ["Gold", "Silver", "Gold", "gold", "Bronze", "Silver"],
      bare,
    );
    expect(column.type).toBe("select");
    expect(column.options).toEqual([
      { id: "gold", label: "Gold", color: "purple" },
      { id: "silver", label: "Silver", color: "cyan" },
      { id: "bronze", label: "Bronze", color: "green" },
    ]);
  });

  test("a column named for a status becomes one, its choices under categories", () => {
    const column = inferColumn("c", "Status", ["Open", "Closed", "Open"], bare);
    expect(column.type).toBe("status");
    expect(column.options?.map((o) => o.category)).toEqual(["todo", "in_progress"]);
  });

  test("lists of repeated words become a multi-select", () => {
    const column = inferColumn(
      "c",
      "Areas",
      ["security, legal", "legal", "infra; perf", "perf"],
      bare,
    );
    expect(column.type).toBe("multiselect");
    expect(column.options?.map((o) => o.label)).toEqual(["security", "legal", "infra", "perf"]);
  });

  test("a person column needs the company's people; without them, names stay words", () => {
    expect(inferColumn("c", "Owner", ["Maya Ortiz", "PM", "Maya Ortiz"], bare).type).toBe("select");
  });

  test("an empty column takes the type its name suggests", () => {
    expect(inferColumn("c", "Due date", ["", ""], bare).type).toBe("date");
    expect(inferColumn("c", "Confidence", [], bare)).toMatchObject({ type: "rating", max: 5 });
    expect(inferColumn("c", "Priority", [], bare).type).toBe("text");
  });
});

describe("a draft from a table", () => {
  const table = [
    ["Gate", "Budget", "", "Gate"],
    ["Security review", "12,500", "x", "a"],
    ["Pricing page", "lots", "y", "b"],
    ["", "", "", ""],
  ];

  test("names its columns from the header, uniquely, and the first stays text and primary", () => {
    const { draft, rows } = draftFromTable(table, true, bare);
    const columns = draft.collection.columns ?? [];
    expect(columns.map((c) => c.label)).toEqual(["Gate", "Budget", "Column 3", "Gate 2"]);
    expect(columns[0]).toMatchObject({ type: "text", primary: true });
    expect(rows).toBe(2);
    expect(draft.provisional).toEqual(columns.map((c) => c.key));
  });

  test("reads every value as its column, and counts the ones that fit none", () => {
    const { draft, unfit } = draftFromTable(
      [
        ["Name", "Score"],
        ["a", "1"],
        ["b", "2"],
      ],
      true,
      bare,
    );
    expect(draft.rows.map((r) => r.values)).toEqual([
      { column_1: "a", column_2: 1 },
      { column_1: "b", column_2: 2 },
    ]);
    expect(unfit).toBe(0);
    expect(draftFromTable(table, true, bare).unfit).toBe(0);
  });

  test("without a header, every row is data and the columns are numbered", () => {
    const { draft, rows } = draftFromTable([["a", "1"]], false, bare);
    expect(draft.collection.columns?.map((c) => c.label)).toEqual(["Column 1", "Column 2"]);
    expect(rows).toBe(1);
  });
});

describe("shaping a draft", () => {
  test("a column retyped keeps every value that reads as the new type", () => {
    let draft = draftFromTable(
      [
        ["Name", "Score"],
        ["a", "7"],
        ["b", "x"],
      ],
      true,
      bare,
    ).draft;
    draft = withColumnSettings(
      draft,
      "column_2",
      { field: "column_2", label: "Score", type: "text" },
      bare,
    );
    expect(draft.rows.map((r) => r.values["column_2"])).toEqual(["7", "x"]);
    draft = withColumnSettings(
      draft,
      "column_2",
      { field: "column_2", label: "Score", type: "number" },
      bare,
    );
    expect(draft.rows.map((r) => r.values["column_2"])).toEqual([7, undefined]);
  });

  test("a choice removed clears the rows that chose it; a template's write rules stay", () => {
    let draft = draftOf(reviewSheetSchema(), [
      { item: "A", status: "approved" },
      { item: "B", status: "pending" },
    ]);
    const status = draftColumns(draft).find((c) => c.field === "status")!;
    draft = withColumnSettings(
      draft,
      "status",
      { ...status, label: "Verdict", options: status.options!.filter((o) => o.id !== "approved") },
      bare,
    );
    expect(draft.rows.map((r) => r.values["status"])).toEqual([undefined, "pending"]);
    expect(draft.collection.columns?.find((c) => c.key === "status")).toMatchObject({
      label: "Verdict",
      write: "contributor",
      when: "assigned_to_viewer",
    });
  });

  test("a status keeps a category on every choice; a choice column carries none", () => {
    let draft = draftOf(trackerSheetSchema());
    const priority = draftColumns(draft).find((c) => c.field === "priority")!;
    draft = withColumnSettings(
      draft,
      "priority",
      { ...priority, type: "status", options: [{ id: "high", label: "High" }] },
      bare,
    );
    expect(draft.collection.columns?.find((c) => c.key === "priority")?.options).toEqual([
      { id: "high", label: "High", category: "todo" },
    ]);
  });

  test("a new column is provisional and named apart; the primary column neither moves nor goes", () => {
    const start = draftOf(trackerSheetSchema());
    const { draft, key } = withNewColumn(start);
    expect(draft.provisional).toEqual([key]);
    expect(draft.collection.columns?.at(-1)).toMatchObject({
      key: "column_7",
      label: "Column 7",
      type: "text",
    });
    expect(withColumnMoved(draft, "owner", -1)).toBe(draft);
    expect(withoutColumn(draft, "task")).toBe(draft);
    const moved = withColumnMoved(draft, key, -1);
    expect(moved.collection.columns?.map((c) => c.key).slice(-2)).toEqual([key, "notes"]);
    expect(withoutColumn(draft, key).provisional).toEqual([]);
  });
});

describe("what the engine is sent", () => {
  test("columns made here are keyed from their names, uniquely; template keys and the policy are kept", () => {
    let draft = draftOf(trackerSheetSchema());
    const first = withNewColumn(draft);
    draft = withColumnSettings(
      first.draft,
      first.key,
      { field: first.key, label: "Due", type: "date" },
      bare,
    );
    draft = withEdits(draft, [
      { rowId: draft.rows[0]!.id, field: "task", value: "Ship" },
      { rowId: draft.rows[0]!.id, field: first.key, value: "2026-11-02" },
    ]);
    const writes = draftWrites(draft, "contribute");
    const [collection] = writes.collections;
    expect(collection?.preset).toBe("contribute");
    expect(collection).not.toHaveProperty("read");
    expect(collection?.columns?.map((c) => c.key)).toEqual([
      "task",
      "owner",
      "status",
      "priority",
      "due",
      "notes",
      "due_2",
    ]);
    expect(writes.rows).toEqual([{ task: "Ship", due_2: "2026-11-02" }]);
    expect(schemaProblem(writes.collections)).toBeNull();
  });

  test("a row that leaves a required column empty holds Create, and says which", () => {
    const draft = withEdits(draftOf(trackerSheetSchema()), []);
    const typed = withEdits(draft, [
      { rowId: draft.rows[1]!.id, field: "notes", value: "Only notes" },
    ]);
    expect(draftProblem(draft)).toBeUndefined();
    expect(draftProblem(typed)).toBe("Row 2 needs a value in Task.");
  });

  test("two columns of one name hold Create", () => {
    const { draft, key } = withNewColumn(draftOf(trackerSheetSchema()));
    const clash = withColumnSettings(
      draft,
      key,
      { field: key, label: "owner", type: "text" },
      bare,
    );
    expect(draftProblem(clash)).toBe("Owner: Another column is called “owner”.");
  });

  test("rows go in batches within the engine's bound, with ids that keep their order", () => {
    const rows = Array.from({ length: 250 }, (_, i) => ({ n: i }));
    let serial = 0;
    const batches = insertBatches("rows", rows, () => `r${(serial++ % 7).toString()}`);
    expect(batches.map((b) => b.length)).toEqual([DATA_BATCH_MAX_OPS, DATA_BATCH_MAX_OPS, 50]);
    const ids = batches.flat().map((op) => op.doc);
    expect([...ids].sort()).toEqual(ids);
    expect(batches[0]![0]).toEqual({
      op: "set",
      collection: "rows",
      doc: "000_r0",
      fields: { n: 0 },
    });
  });

  test("every template's examples name only its columns and read as their types", () => {
    for (const template of SHEET_TEMPLATES) {
      const draft = draftOf(template.schema(), template.examples(new Date(2026, 9, 19)));
      const keys = new Set((draft.collection.columns ?? []).map((c) => c.key));
      for (const row of draft.rows)
        for (const key of Object.keys(row.values)) expect(keys, template.key).toContain(key);
      expect(draftProblem(draft), template.key).toBeUndefined();
    }
  });
});

test("a column's name suggests its type", () => {
  expect(typeFromName("Due date")).toBe("date");
  expect(typeFromName("Owner")).toBe("person");
  expect(typeFromName("Estimated hours")).toBe("number");
  expect(typeFromName("Gate")).toBeUndefined();
});

test("a file's name reads as a title", () => {
  expect(titleFromFile("launch-gates.csv")).toBe("Launch gates");
  expect(titleFromFile("Q4_budget  final.TSV")).toBe("Q4 budget final");
});

describe("what an agent is handed", () => {
  const collection: DataCollection = {
    name: "items",
    read: "all",
    create: "editor",
    delete: "editor",
    columns: [
      { key: "task", type: "text" as const, label: "Task", primary: true },
      {
        key: "stage",
        type: "status" as const,
        label: "Stage",
        options: [
          { id: "todo", label: "To do" },
          { id: "done", label: "Done" },
          { id: "old", label: "Old", archived: true },
        ],
      },
      { key: "secret", type: "text" as const, label: "Secret", hidden: true },
    ],
  };

  test("every visible column with its type, its choices and whether it is required", () => {
    const text = draftBrief(draftOf([collection]), COLUMN_TYPE_WORDS);
    expect(text).toBe(
      [
        "Start from these columns:",
        "- Task (Text, required)",
        "- Stage (Status): To do, Done",
      ].join("\n"),
    );
  });

  test("the rows typed so far, choices by their labels, blank rows left out", () => {
    const draft = draftOf([collection], [{ task: "Write the plan", stage: "done" }, {}]);
    const text = draftBrief(draft, COLUMN_TYPE_WORDS);
    expect(text).toContain("And this first row:\n- Task: Write the plan; Stage: Done");
  });

  test("past the cap the rest are counted rather than spelled out", () => {
    const rows = Array.from({ length: BRIEF_ROWS_MAX + 3 }, (_, i) => ({ task: `Row ${i}` }));
    const text = draftBrief(draftOf([collection], rows), COLUMN_TYPE_WORDS);
    expect(text.split("\n").filter((l) => l.startsWith("- Task: Row"))).toHaveLength(
      BRIEF_ROWS_MAX,
    );
    expect(text).toContain("- and 3 more rows like these");
  });
});
