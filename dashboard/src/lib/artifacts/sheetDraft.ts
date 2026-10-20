/**
 * A new sheet before it is made: the collection the create page shapes in a
 * live grid, the rows typed or imported into it, and the engine writes it
 * becomes (`artifacts/new/SheetCreate.tsx`).
 *
 * THE DRAFT IS THE SCHEMA ITSELF. A template's collection, with every rule it
 * carries (the Review sheet's assigned-reviewer writes, a sign-up's
 * contribute policy), is held as the engine will take it, and the grid draws
 * it through the same `sheetColumns` the sheet's own page uses, so what the
 * page shows is what the sheet will be. A column the reader adds here has a
 * provisional key until the sheet is made, when it is keyed from the name the
 * reader gave it (`Due date` becomes `due_date`); a template's keys never
 * change, since its rules name them.
 *
 * AN IMPORT IS TYPED BY THE GRID'S OWN PARSER. Each CSV column takes the
 * first type every one of its values parses as (`parseText`, the parser a
 * paste goes through), so an import is held to exactly the rules the grid
 * holds a person typing to, and a value that fits no column is counted and
 * left empty rather than guessed.
 */

import {
  parseText,
  sheetLabels,
  type SheetColumn,
  type SheetEdit,
  type SheetPerson,
  type SheetValueContext,
} from "@crewlethq/ui";
import { DATA_BATCH_MAX_OPS, DATA_COLUMNS_MAX } from "~/contract/artifacts.ts";
import type {
  CollectionPreset,
  ColumnType,
  DataCollection,
  DataColumn,
  DataOp,
  DataOption,
} from "~/protocol/index.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { hasOptions, keyFromLabel, optionID, schemaProblem, withPreset } from "./schema.ts";
import { sheetColumns } from "./sheet.ts";

/** One row of a draft: an id the grid keys it by, and its values by column key. */
export interface DraftRow {
  id: string;
  values: Record<string, unknown>;
}

export interface SheetDraft {
  /** The collection, columns included, as the engine will take it. */
  collection: DataCollection;
  /** Keys of the columns made on this page, keyed again from their names when the sheet is made. */
  provisional: readonly string[];
  rows: readonly DraftRow[];
}

/** The most a column or option name holds, in bytes (`artifactdata.MaxLabelBytes`). */
export const DRAFT_LABEL_MAX_BYTES = 120;

/** How many empty rows a draft with none shows, so the grid reads as a sheet to fill in. */
const EMPTY_ROWS = 3;

let rowSerial = 0;
/** A draft row's id: the grid's key until the engine names the document. */
function draftRowID(): string {
  rowSerial += 1;
  return `draft-${rowSerial}`;
}

/** Empty rows to type into. */
export function emptyRows(count = EMPTY_ROWS): DraftRow[] {
  return Array.from({ length: count }, () => ({ id: draftRowID(), values: {} }));
}

/** A draft from a template's schema and, when kept, its example rows. */
export function draftOf(
  collections: readonly DataCollection[],
  examples: readonly Record<string, unknown>[] = [],
): SheetDraft {
  const collection = collections[0];
  if (!collection) throw new Error("a sheet template names one collection");
  const rows = examples.map((values) => ({ id: draftRowID(), values: { ...values } }));
  return { collection, provisional: [], rows: rows.length > 0 ? rows : emptyRows() };
}

/** The draft's columns as the grid draws them, every one writable: the reader is its owner. */
export function draftColumns(draft: SheetDraft): SheetColumn[] {
  return sheetColumns(draft.collection, "owner");
}

/** The context the grid's parser reads values in. */
export function valueContext(people: readonly SheetPerson[] | undefined): SheetValueContext {
  return { people, labels: sheetLabels() };
}

/** Whether a row holds nothing anybody typed. */
export function isBlankRow(row: DraftRow): boolean {
  return Object.values(row.values).every(
    (v) => v === null || v === undefined || v === "" || (Array.isArray(v) && v.length === 0),
  );
}

function columnsOf(draft: SheetDraft): DataColumn[] {
  return draft.collection.columns ?? [];
}

function withColumns(draft: SheetDraft, columns: DataColumn[]): SheetDraft {
  return { ...draft, collection: { ...draft.collection, columns } };
}

/** A sheet column's options as the datastore spells them: a status keeps its category, a choice its colour. */
function dataOptions(type: ColumnType, options: SheetColumn["options"]): DataOption[] | undefined {
  if (!hasOptions(type)) return undefined;
  return (options ?? []).map((o) => {
    const out: DataOption = { id: o.id, label: o.label };
    if (o.color) out.color = o.color;
    if (type === "status") out.category = o.category ?? "todo";
    return out;
  });
}

/** The fields a column's type gives meaning to, dropped when the type changes. */
const TYPED_FIELDS = [
  "options",
  "min",
  "max",
  "max_length",
  "max_items",
  "default",
  "mode",
] as const;

/**
 * A column changed in its settings, carried into the draft: its name, type,
 * choices, stars and whether it is required. The rules a template set on it
 * (who writes it, and on which rows) stay. When its type changes, every value
 * the rows hold in it is read again as the new type, as its text, and kept
 * where it fits: a column of numbers typed as text, then as a number, keeps
 * its numbers.
 */
export function withColumnSettings(
  draft: SheetDraft,
  key: string,
  next: SheetColumn,
  context: SheetValueContext,
): SheetDraft {
  const columns = columnsOf(draft);
  const at = columns.findIndex((c) => c.key === key);
  const was = columns[at];
  if (!was) return draft;
  const retyped = next.type !== was.type;
  const base: DataColumn = { ...was };
  if (retyped) for (const field of TYPED_FIELDS) delete base[field];
  const changed: DataColumn = { ...base, type: next.type, label: next.label };
  const options = dataOptions(next.type, next.options);
  if (options) changed.options = options;
  else delete changed.options;
  if (next.type === "rating") changed.max = next.max ?? 5;
  if (next.required && next.type !== "checkbox") changed.required = true;
  else delete changed.required;
  const updated = withColumns(
    draft,
    columns.map((c, i) => (i === at ? changed : c)),
  );
  if (!retyped && !optionsDropped(was, changed)) return updated;
  const before = sheetColumns({ ...draft.collection, columns: [was] }, "owner")[0];
  const after = sheetColumns({ ...draft.collection, columns: [changed] }, "owner")[0];
  if (!before || !after) return updated;
  return {
    ...updated,
    rows: draft.rows.map((row) => {
      const value = row.values[key];
      if (value === undefined || value === null) return row;
      const values = { ...row.values };
      const parsed = parseText({ ...after, required: false }, textOf(before, value), context);
      if (parsed.ok && parsed.value !== null) values[key] = parsed.value;
      else delete values[key];
      return { ...row, values };
    }),
  };
}

/** Whether a choice the rows may name was removed. */
function optionsDropped(was: DataColumn, now: DataColumn): boolean {
  const kept = new Set((now.options ?? []).map((o) => o.id));
  return (was.options ?? []).some((o) => !kept.has(o.id));
}

/** A value as the text a person would type for it, under its column: an option's label, a list joined. */
function textOf(column: SheetColumn, value: unknown): string {
  const label = (id: unknown) => column.options?.find((o) => o.id === id)?.label ?? String(id);
  if (Array.isArray(value)) return value.map(label).join(", ");
  if (column.type === "select" || column.type === "status") return label(value);
  return String(value);
}

/** A key no column of the draft has: `column_4`. */
function freshKey(columns: readonly DataColumn[]): string {
  const taken = new Set(columns.map((c) => c.key));
  for (let n = columns.length + 1; ; n++) {
    const key = `column_${n}`;
    if (!taken.has(key)) return key;
  }
}

/** A name no column of the draft has: `Column 4`. */
function freshLabel(columns: readonly DataColumn[]): string {
  const taken = new Set(columns.map((c) => (c.label ?? c.key).toLowerCase()));
  for (let n = columns.length + 1; ; n++) {
    const label = `Column ${n}`;
    if (!taken.has(label.toLowerCase())) return label;
  }
}

/** A text column appended to the draft, and its key, for the page to open its settings. */
export function withNewColumn(draft: SheetDraft): { draft: SheetDraft; key: string } {
  const columns = columnsOf(draft);
  const key = freshKey(columns);
  const column: DataColumn = { key, type: "text", label: freshLabel(columns) };
  return {
    draft: {
      ...withColumns(draft, [...columns, column]),
      provisional: [...draft.provisional, key],
    },
    key,
  };
}

/** The draft with a column moved one place, never before the primary column. */
export function withColumnMoved(draft: SheetDraft, key: string, step: -1 | 1): SheetDraft {
  const columns = [...columnsOf(draft)];
  const at = columns.findIndex((c) => c.key === key);
  const to = at + step;
  if (at < 0 || to < 0 || to >= columns.length || columns[at]?.primary || columns[to]?.primary) {
    return draft;
  }
  const [moved] = columns.splice(at, 1);
  columns.splice(to, 0, moved!);
  return withColumns(draft, columns);
}

/** The draft without a column, and without its values. The primary column stays. */
export function withoutColumn(draft: SheetDraft, key: string): SheetDraft {
  const columns = columnsOf(draft);
  if (columns.find((c) => c.key === key)?.primary) return draft;
  return {
    ...withColumns(
      draft,
      columns.filter((c) => c.key !== key),
    ),
    provisional: draft.provisional.filter((k) => k !== key),
    rows: draft.rows.map((row) => {
      if (!(key in row.values)) return row;
      const { [key]: _gone, ...values } = row.values;
      return { ...row, values };
    }),
  };
}

/** The draft with the grid's edits written into its rows. */
export function withEdits(draft: SheetDraft, edits: readonly SheetEdit[]): SheetDraft {
  const byRow = new Map<string, SheetEdit[]>();
  for (const edit of edits) byRow.set(edit.rowId, [...(byRow.get(edit.rowId) ?? []), edit]);
  return {
    ...draft,
    rows: draft.rows.map((row) => {
      const mine = byRow.get(row.id);
      if (!mine) return row;
      const values = { ...row.values };
      for (const edit of mine) values[edit.field] = edit.value;
      return { ...row, values };
    }),
  };
}

export function withRowAdded(draft: SheetDraft): SheetDraft {
  return { ...draft, rows: [...draft.rows, { id: draftRowID(), values: {} }] };
}

export function withoutRow(draft: SheetDraft, id: string): SheetDraft {
  return { ...draft, rows: draft.rows.filter((row) => row.id !== id) };
}

/** Whether the draft can take another column. */
export function canAddColumn(draft: SheetDraft): boolean {
  return columnsOf(draft).length < DATA_COLUMNS_MAX;
}

/** Why a column's name would be refused, or undefined: empty, too long, or another column's. */
export function columnNameProblem(
  draft: SheetDraft,
  key: string,
  label: string,
): string | undefined {
  const name = label.trim();
  if (name === "") return "Give the column a name.";
  const bytes = utf8Bytes(name);
  if (bytes > DRAFT_LABEL_MAX_BYTES) {
    return `The name is ${bytes} bytes and the most is ${DRAFT_LABEL_MAX_BYTES}.`;
  }
  const clash = columnsOf(draft).find(
    (c) => c.key !== key && (c.label ?? c.key).trim().toLowerCase() === name.toLowerCase(),
  );
  return clash ? `Another column is called “${clash.label ?? clash.key}”.` : undefined;
}

/**
 * Why the draft cannot be made yet, in the sentence the create action says,
 * or undefined: the engine's bounds on the schema, a name it would refuse, and
 * a row that leaves a required column empty.
 */
export function draftProblem(draft: SheetDraft): string | undefined {
  const bounds = schemaProblem([draft.collection]);
  if (bounds) return bounds;
  for (const column of columnsOf(draft)) {
    const name = columnNameProblem(draft, column.key, column.label ?? column.key);
    if (name) return `${column.label || "A column"}: ${name}`;
    for (const option of column.options ?? []) {
      if (utf8Bytes(option.label) > DRAFT_LABEL_MAX_BYTES) {
        return `${column.label}: the choice “${option.label.slice(0, 24)}…” is longer than ${DRAFT_LABEL_MAX_BYTES} bytes.`;
      }
    }
  }
  const required = columnsOf(draft).filter((c) => c.required && c.type !== "checkbox");
  const rows = draft.rows.filter((row) => !isBlankRow(row));
  for (const column of required) {
    const at = draft.rows.findIndex((row) => {
      if (!rows.includes(row)) return false;
      const v = row.values[column.key];
      return v === null || v === undefined || v === "" || (Array.isArray(v) && v.length === 0);
    });
    if (at >= 0) return `Row ${at + 1} needs a value in ${column.label || column.key}.`;
  }
  return undefined;
}

/** What the engine is sent: the schema, and the rows by their final keys. */
export interface DraftWrites {
  collections: DataCollection[];
  /** The collection the rows are written to. */
  collection: string;
  /** Every row that holds something, keyed as the schema names its columns. */
  rows: Record<string, unknown>[];
}

/**
 * The draft as the engine's writes: each column made here keyed from its
 * name, unique among the rest; the policy the reader picked; and the rows that
 * hold something, with their values under the final keys.
 */
export function draftWrites(draft: SheetDraft, preset: CollectionPreset): DraftWrites {
  const columns = columnsOf(draft);
  const provisional = new Set(draft.provisional);
  const taken = new Set(columns.filter((c) => !provisional.has(c.key)).map((c) => c.key));
  const rename = new Map<string, string>();
  for (const column of columns) {
    if (!provisional.has(column.key)) continue;
    const base = keyFromLabel(column.label ?? column.key);
    let key = base;
    for (let n = 2; taken.has(key); n++) key = `${base.slice(0, 60)}_${n}`;
    taken.add(key);
    rename.set(column.key, key);
  }
  const keyed = columns.map((c) => (rename.has(c.key) ? { ...c, key: rename.get(c.key)! } : c));
  const collection = withPreset({ ...draft.collection, columns: keyed }, preset);
  const rows = draft.rows
    .filter((row) => !isBlankRow(row))
    .map((row) => {
      const out: Record<string, unknown> = {};
      for (const [key, value] of Object.entries(row.values)) {
        if (value === null || value === undefined || value === "") continue;
        if (Array.isArray(value) && value.length === 0) continue;
        out[rename.get(key) ?? key] = value;
      }
      return out;
    });
  return { collections: [collection], collection: collection.name, rows };
}

/**
 * The rows as insert batches, each within the engine's bound on one write.
 *
 * IN THE ORDER THEY WERE DRAWN. A sheet lists its rows by when they were
 * made, then by id, and a batch's rows are all made at once, so each id
 * leads with the row's place (`00004_…`) and the random part after it keeps
 * it unique: the sheet opens with its rows where the reader put them.
 */
export function insertBatches(
  collection: string,
  rows: readonly Record<string, unknown>[],
  newID: () => string,
): DataOp[][] {
  const width = String(Math.max(rows.length - 1, 0)).length;
  const batches: DataOp[][] = [];
  for (let i = 0; i < rows.length; i += DATA_BATCH_MAX_OPS) {
    batches.push(
      rows.slice(i, i + DATA_BATCH_MAX_OPS).map((fields, at) => ({
        op: "set" as const,
        collection,
        doc: `${String(i + at).padStart(width, "0")}_${newID()}`,
        fields,
      })),
    );
  }
  return batches;
}

/* ----- proposing a type ------------------------------------------------- */

/** Words in a column's name that say what it holds, checked in order. */
const NAME_TYPES: readonly [RegExp, ColumnType][] = [
  [/\b(status|state|stage)\b/i, "status"],
  [/\b(date|due|deadline|when|day|start|end|target)\b/i, "date"],
  [/\b(owner|assignee|reviewer|lead|who|person|people|author)\b/i, "person"],
  [/\b(url|link|website|site|href)\b/i, "url"],
  [/\b(rating|stars|confidence)\b/i, "rating"],
  [/\b(done|complete|completed|approved|paid|active|enabled)\b/i, "checkbox"],
  [
    /\b(amount|cost|price|budget|count|qty|quantity|total|score|estimate|points|number|hours|percent)\b/i,
    "number",
  ],
  [/\b(notes|description|details|summary|comments?)\b/i, "longtext"],
  [/\b(tags|labels)\b/i, "multiselect"],
  [/\b(priority|category|type|quarter|tier|team|kind|region|size)\b/i, "select"],
];

/** The type a column's name suggests ("Due date" a date, "Owner" a person), or undefined. */
export function typeFromName(label: string): ColumnType | undefined {
  return NAME_TYPES.find(([words]) => words.test(label))?.[1];
}

/* ----- an import ---------------------------------------------------------- */

/** The most rows an import takes here; a larger table is an agent's to load. */
export const IMPORT_ROWS_MAX = 10_000;
/** The most bytes a file read here may be. */
export const IMPORT_BYTES_MAX = 10 * 1024 * 1024;

/** The most distinct values a column of repeated words is offered as choices with. */
const CHOICES_MAX = 20;

export interface ImportedDraft {
  draft: SheetDraft;
  /** How many values fit no column's type and were left empty. */
  unfit: number;
  /** How many rows were read, the header aside. */
  rows: number;
}

/** A column's name from the header, or `Column 3` where the header gives none. */
function headerNames(header: readonly string[] | undefined, width: number): string[] {
  const names: string[] = [];
  const seen = new Set<string>();
  for (let i = 0; i < width; i++) {
    let name = (header?.[i] ?? "").trim().replace(/\s+/g, " ");
    while (utf8Bytes(name) > DRAFT_LABEL_MAX_BYTES) name = name.slice(0, -1);
    if (name === "") name = `Column ${i + 1}`;
    let unique = name;
    for (let n = 2; seen.has(unique.toLowerCase()); n++) unique = `${name} ${n}`;
    seen.add(unique.toLowerCase());
    names.push(unique);
  }
  return names;
}

/** The distinct trimmed values, in first-seen order, case kept from the first. */
function distinct(values: readonly string[]): string[] {
  const seen = new Map<string, string>();
  for (const v of values) {
    const t = v.trim();
    if (t !== "" && !seen.has(t.toLowerCase())) seen.set(t.toLowerCase(), t);
  }
  return [...seen.values()];
}

/** Choices made from a column's words, each with the next hue. */
function choicesFrom(words: readonly string[]): DataOption[] {
  const hues = ["purple", "cyan", "green", "amber", "rose", "blue"];
  const taken: string[] = [];
  return words.map((label, i) => {
    const id = optionID(label, taken);
    taken.push(id);
    return { id, label: label.slice(0, 60), color: hues[i % hues.length]! };
  });
}

/** Whether every value parses as the column, and so the column is that type. */
function allParse(
  column: SheetColumn,
  values: readonly string[],
  context: SheetValueContext,
): boolean {
  return values.every((v) => v.trim() === "" || parseText(column, v, context).ok);
}

/**
 * The type a column of imported text is: the first that every value parses as,
 * from the most particular (a yes or no, a number, a date, a link, a person the
 * company has) to repeated words (choices, or lists of them), then long text
 * and text. A column whose name suggests a type and holds nothing takes it.
 */
export function inferColumn(
  key: string,
  label: string,
  values: readonly string[],
  context: SheetValueContext,
): DataColumn {
  const filled = values.filter((v) => v.trim() !== "");
  const named = typeFromName(label);
  const column = (type: ColumnType, extra: Partial<DataColumn> = {}): DataColumn => ({
    key,
    type,
    label,
    ...extra,
  });
  if (filled.length === 0) {
    return named && !hasOptions(named)
      ? column(named, named === "rating" ? { max: 5 } : {})
      : column("text");
  }
  const probe = (type: ColumnType, extra: Partial<SheetColumn> = {}): SheetColumn => ({
    field: key,
    label,
    type: type as SheetColumn["type"],
    ...extra,
  });
  // A column of only 0 and 1 is numbers, not a yes or no.
  const wordy = filled.some((v) => !/^[01]$/.test(v.trim()));
  if (wordy && allParse(probe("checkbox"), filled, context)) return column("checkbox");
  if (allParse(probe("number"), filled, context)) {
    const ints = filled.every((v) => /^\s*[1-9]\d*\s*$/.test(v));
    const most = Math.max(...filled.map((v) => Number(v)));
    if (named === "rating" && ints && most <= 10)
      return column("rating", { max: Math.max(5, most) });
    return column("number");
  }
  if (allParse(probe("date"), filled, context)) return column("date");
  if (
    filled.every((v) => /^https?:\/\//i.test(v.trim())) &&
    allParse(probe("url"), filled, context)
  ) {
    return column("url");
  }
  if (context.people && context.people.length > 0 && allParse(probe("person"), filled, context)) {
    return column("person");
  }
  const long = filled.some((v) => v.length > 120 || /[\r\n]/.test(v));
  if (!long) {
    const words = distinct(filled);
    // REPEATED: some word comes back, and there are few enough of them to
    // read as a list to pick from, even in a short file.
    const repeated =
      words.length < filled.length && words.length <= Math.max(3, Math.floor(filled.length / 2));
    if (repeated && words.length <= CHOICES_MAX && words.every((w) => w.length <= 40)) {
      const type: ColumnType = named === "status" ? "status" : "select";
      const options = choicesFrom(words).map((o, i) =>
        type === "status"
          ? { id: o.id, label: o.label, category: i === 0 ? "todo" : "in_progress" }
          : o,
      );
      return column(type, { options });
    }
    const listed = filled.some((v) => /[,;]/.test(v));
    if (listed) {
      const parts = distinct(filled.flatMap((v) => v.split(/[,;]/)));
      const listedCount = filled.reduce(
        (n, v) => n + v.split(/[,;]/).filter((p) => p.trim()).length,
        0,
      );
      if (
        parts.length <= CHOICES_MAX &&
        parts.length < listedCount &&
        parts.every((w) => w.length <= 40)
      ) {
        return column("multiselect", { options: choicesFrom(parts) });
      }
    }
  }
  return column(long ? "longtext" : "text");
}

/**
 * A draft from a parsed table: the first row its column names when `header`,
 * each column typed by its values ([inferColumn]) and every value read as its
 * column. The first column names each row, so it stays text.
 */
export function draftFromTable(
  table: readonly (readonly string[])[],
  header: boolean,
  context: SheetValueContext,
): ImportedDraft {
  const body = (header ? table.slice(1) : table).filter((r) => r.some((v) => v.trim() !== ""));
  const width = Math.min(
    Math.max(header ? (table[0]?.length ?? 0) : 0, ...body.map((r) => r.length), 1),
    DATA_COLUMNS_MAX,
  );
  const names = headerNames(header ? table[0] : undefined, width);
  const keys = names.map((_, i) => `column_${i + 1}`);
  const columns = names.map((label, i) => {
    const values = body.map((r) => r[i] ?? "");
    if (i === 0) {
      const long = values.some((v) => v.length > 120 || /[\r\n]/.test(v));
      return {
        key: keys[i]!,
        type: long ? "longtext" : "text",
        label,
        primary: true,
      } as DataColumn;
    }
    return inferColumn(keys[i]!, label, values, context);
  });
  const collection: DataCollection = {
    name: "rows",
    title: "Sheet",
    preset: "shared",
    read: "all",
    create: "editor",
    delete: "editor",
    columns,
  };
  const grid = sheetColumns(collection, "owner");
  let unfit = 0;
  const rows = body.map((r) => {
    const values: Record<string, unknown> = {};
    grid.forEach((column, i) => {
      const text = r[i] ?? "";
      if (text.trim() === "") return;
      const parsed = parseText({ ...column, required: false }, text, context);
      if (parsed.ok && parsed.value !== null) values[column.field] = parsed.value;
      else unfit += 1;
    });
    return { id: draftRowID(), values };
  });
  return {
    draft: { collection, provisional: keys, rows: rows.length > 0 ? rows : emptyRows() },
    unfit,
    rows: body.length,
  };
}

/** A file's name as a title: `launch-gates.csv` reads `Launch gates`. */
export function titleFromFile(name: string): string {
  const words = name
    .replace(/\.(csv|tsv|txt)$/i, "")
    .replace(/[-_]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** The most rows a hand-off to an agent spells out; the rest are counted. */
export const BRIEF_ROWS_MAX = 50;

/**
 * The draft as an agent is handed it with a brief: every column with its type,
 * its choices and whether it is required, then the rows typed so far, one line
 * each in the columns' order, choices by their labels. Said in words rather
 * than as the schema, since the agent reads it in a task's description and
 * builds the sheet from it with its own tools.
 */
export function draftBrief(draft: SheetDraft, types: Readonly<Record<ColumnType, string>>): string {
  const columns = (draft.collection.columns ?? []).filter((c) => !c.hidden);
  if (columns.length === 0) return "";
  const lines = ["Start from these columns:"];
  for (const c of columns) {
    const facts = [types[c.type] ?? c.type];
    if (c.required || c.primary) facts.push("required");
    const options = (c.options ?? []).filter((o) => !o.archived).map((o) => o.label);
    lines.push(
      `- ${c.label || c.key} (${facts.join(", ")})${options.length > 0 ? `: ${options.join(", ")}` : ""}`,
    );
  }
  const rows = draft.rows.filter((row) => !isBlankRow(row));
  if (rows.length > 0) {
    const said = (c: DataColumn, value: unknown): string => {
      const label = (id: unknown) => c.options?.find((o) => o.id === id)?.label ?? String(id);
      if (value === null || value === undefined || value === "") return "";
      if (Array.isArray(value)) return value.map(label).join(", ");
      if (hasOptions(c.type)) return label(value);
      return typeof value === "object" ? JSON.stringify(value) : String(value);
    };
    lines.push("", rows.length === 1 ? "And this first row:" : "And these first rows:");
    for (const row of rows.slice(0, BRIEF_ROWS_MAX)) {
      const cells = columns
        .map((c) => [c.label || c.key, said(c, row.values[c.key])] as const)
        .filter(([, v]) => v !== "")
        .map(([k, v]) => `${k}: ${v}`);
      lines.push(`- ${cells.join("; ")}`);
    }
    if (rows.length > BRIEF_ROWS_MAX) {
      lines.push(`- and ${(rows.length - BRIEF_ROWS_MAX).toLocaleString()} more rows like these`);
    }
  }
  return lines.join("\n");
}
