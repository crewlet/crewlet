/**
 * A collection as the design system's `SheetGrid` draws it: the engine's
 * schema (`artifacts.Column`) mapped onto sheet columns field for field, the
 * documents onto rows, and the reader's role onto what they may edit.
 *
 * THE ENGINE DECIDES EVERY CELL. What is mapped here is only what can be
 * known from the schema and the reader's role (a column whose write role is
 * above theirs, a reader below
 * contributor), so a cell the reader cannot change is drawn read only rather
 * than refused after the edit; a rule that depends on the row or the value
 * (own rows, rows assigned to the reader, a value an immutable column already
 * holds) is the applier's, and its refusal is said on the cell.
 */

import {
  isSheetColumnType,
  type SheetColumn,
  type SheetOption,
  type SheetRow,
  type SheetStatusCategory,
} from "@crewlethq/ui";
import type { ArtifactRole, DataCollection, DataColumn, DataDoc } from "~/protocol/index.ts";
import { STATUS_CATEGORIES } from "~/contract/artifacts.ts";
import { roleIncludes } from "./words.ts";

/** Whether a status option's category off the wire is one the engine defines, and the sheet draws. */
function isStatusCategory(value: string): value is SheetStatusCategory {
  return (STATUS_CATEGORIES as readonly string[]).includes(value);
}

function options(column: DataColumn): SheetOption[] | undefined {
  if (!column.options) return undefined;
  return column.options.map((option) => {
    const out: SheetOption = { id: option.id, label: option.label };
    if (option.color) out.color = option.color;
    if (option.category && isStatusCategory(option.category)) out.category = option.category;
    if (option.archived) out.archived = true;
    return out;
  });
}

/** Whether a reader at `role` may write `column` at all, as far as the schema says. */
export function columnWritable(
  column: DataColumn,
  collection: DataCollection,
  role: ArtifactRole | "",
): boolean {
  if (!roleIncludes(role, "contributor")) return false;
  if (column.write && !roleIncludes(role, column.write)) return false;
  // A COLUMN NAMING NO WRITE ROLE takes the collection's create role.
  if (!column.write && !roleIncludes(role, collection.create)) return false;
  return true;
}

/**
 * A collection's columns as sheet columns, for a reader at `role`. A hidden
 * column is not drawn; the primary column is frozen; a type this build does
 * not know (a newer engine's) is drawn as read-only text rather than dropped.
 */
export function sheetColumns(collection: DataCollection, role: ArtifactRole | ""): SheetColumn[] {
  const out: SheetColumn[] = [];
  for (const column of collection.columns ?? []) {
    if (column.hidden) continue;
    const known = isSheetColumnType(column.type);
    const mapped: SheetColumn = {
      field: column.key,
      label: column.label || column.key,
      type: known ? column.type : "text",
    };
    // AN IMMUTABLE COLUMN takes its first value and keeps it: an empty cell of
    // it is still written once, so it is not drawn read only, and the
    // applier's refusal of a second write is said on the cell. A HUMANS-ONLY
    // column binds agents alone, and the dashboard's reader is a person.
    const writable = known && columnWritable(column, collection, role);
    if (!writable) mapped.readOnly = true;
    if (column.primary) mapped.frozen = true;
    if (column.required) mapped.required = true;
    if (column.min !== undefined) mapped.min = column.min;
    if (column.max !== undefined) mapped.max = column.max;
    if (column.max_length !== undefined) mapped.maxLength = column.max_length;
    if (column.max_items !== undefined) mapped.maxItems = column.max_items;
    const choices = options(column);
    if (choices) mapped.options = choices;
    out.push(mapped);
  }
  return out;
}

/**
 * A schemaless collection's columns: every field its documents carry, as
 * text, read only, since the engine validates nothing about them and a typed
 * editor would guess.
 */
export function inferredColumns(
  docs: readonly { fields: Record<string, unknown> }[],
): SheetColumn[] {
  const fields = new Set<string>();
  for (const d of docs) for (const key of Object.keys(d.fields)) fields.add(key);
  return [...fields].sort().map((field) => ({ field, label: field, type: "text", readOnly: true }));
}

/**
 * The documents as rows, in the order they were created (then by id), so a
 * row does not move under the reader when somebody else edits it.
 */
export function sheetRows(
  docs: readonly { doc: DataDoc; fields: Record<string, unknown> }[],
): SheetRow[] {
  return [...docs]
    .sort((a, b) => {
      const at = a.doc.created_at || "￿";
      const bt = b.doc.created_at || "￿";
      return at < bt ? -1 : at > bt ? 1 : a.doc.id < b.doc.id ? -1 : a.doc.id > b.doc.id ? 1 : 0;
    })
    .map((d) => ({ id: d.doc.id, values: d.fields }));
}

/** The longest name a download is given, in characters, before `.csv`. */
const FILE_NAME_MAX = 120;

/**
 * The name a collection's CSV downloads as: the artifact's title and the
 * collection's, as the reader knows them ("Q4 launch review - Launch
 * gates.csv"), never the collection's key. Characters a file system refuses
 * are dropped, runs of space collapse, and the name is cut to a length every
 * system keeps.
 */
export function csvFileName(artifact: string, collection: string): string {
  const clean = (text: string) =>
    // eslint-disable-next-line no-control-regex
    text
      .replace(/[\\/:*?"<>|\u0000-\u001f\u007f]/g, " ")
      .replace(/\s+/g, " ")
      .trim();
  const parts = [clean(artifact), clean(collection)].filter(Boolean);
  const unique = parts.length === 2 && parts[0] === parts[1] ? [parts[0]!] : parts;
  const name = (unique.join(" - ") || "Export").slice(0, FILE_NAME_MAX).trim();
  return `${name}.csv`;
}
