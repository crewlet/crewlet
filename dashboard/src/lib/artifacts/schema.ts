/**
 * Building and changing a datastore's schema as the schema editor and the
 * sheet templates do: option ids, collection policies, the checks the editor
 * makes before it sends, and the templates a new sheet starts from.
 *
 * THE ENGINE COMPILES A PRESET into its rules (`artifactdata.Compile`) and
 * holds every collection to the grammar again; what is checked here is what
 * the reader can be told at the field, by the contract's own patterns.
 */

import {
  COLLECTION_NAME_PATTERN,
  DATA_COLLECTIONS_MAX,
  DATA_COLUMNS_MAX,
  FIELD_NAME_PATTERN,
  RESERVED_COLLECTIONS,
} from "~/contract/artifacts.ts";
import type {
  CollectionPreset,
  ColumnType,
  DataCollection,
  DataColumn,
  DataOption,
} from "~/protocol/index.ts";

const COLLECTION = new RegExp(COLLECTION_NAME_PATTERN);
const FIELD = new RegExp(FIELD_NAME_PATTERN);

/** Why a new collection's name would be refused, or null. */
export function collectionNameProblem(name: string, taken: readonly string[]): string | null {
  if (!COLLECTION.test(name)) {
    return "A name is a lowercase letter, then lowercase letters, digits and underscores, at most 48 of them.";
  }
  if ((RESERVED_COLLECTIONS as readonly string[]).includes(name)) {
    return `“${name}” is a name the datastore's own routes take. Pick another.`;
  }
  if (taken.includes(name)) return `There is already a collection called “${name}”.`;
  return null;
}

/** Why a new column's key would be refused, or null. */
export function fieldNameProblem(key: string, taken: readonly string[]): string | null {
  if (!FIELD.test(key))
    return "A key is a letter, then letters, digits and underscores, at most 64 of them.";
  if (taken.includes(key)) return `There is already a column called “${key}”.`;
  return null;
}

/** A label with its accents taken off its letters (`Crème` reads `Creme`), so a key keeps the word whole. */
function unaccented(label: string): string {
  return label.normalize("NFKD").replace(/\p{M}+/gu, "");
}

/** A field key made from what the reader typed as a column's name: `Due date` becomes `due_date`. */
export function keyFromLabel(label: string): string {
  const base = unaccented(label)
    .replace(/[^\p{L}\p{N}]+/gu, "_")
    .replace(/[^A-Za-z0-9_]/g, "")
    .replace(/^_+|_+$/g, "")
    .toLowerCase();
  if (base === "") return "field";
  const start = /^[a-z]/.test(base) ? base : `f_${base}`;
  return start.slice(0, 64);
}

/** An option id made from its label, unique among `taken`: stable once written, so relabelling never rewrites data. */
export function optionID(label: string, taken: readonly string[]): string {
  const base =
    unaccented(label)
      .replace(/[^A-Za-z0-9_-]+/g, "_")
      .replace(/^_+|_+$/g, "")
      .toLowerCase()
      .slice(0, 56) || "option";
  if (!taken.includes(base)) return base;
  for (let n = 2; ; n++) {
    const id = `${base}_${n}`;
    if (!taken.includes(id)) return id;
  }
}

/** Whether a column type offers options. */
export function hasOptions(type: ColumnType): boolean {
  return type === "select" || type === "multiselect" || type === "status";
}

/**
 * A collection whose policy the reader picked: the preset alone, with the
 * rules it compiles to left for the engine to fill, since naming both and
 * getting one wrong is a refusal.
 */
export function withPreset(collection: DataCollection, preset: CollectionPreset): DataCollection {
  if (collection.preset === preset) return collection;
  const { read: _r, create: _c, delete: _d, ...rest } = collection;
  return { ...rest, preset } as DataCollection;
}

/** What each policy means, in the schema editor's words. */
export const PRESET_WORDS: Readonly<
  Record<CollectionPreset, { label: string; description: string }>
> = {
  shared: {
    label: "Editors change it",
    description:
      "Everyone it is shared with reads every row; people who can edit add and change rows.",
  },
  contribute: {
    label: "Everyone adds rows",
    description:
      "Everyone it is shared with reads every row, adds rows and changes their own; editors change any.",
  },
  author: {
    label: "Only the owner changes it",
    description: "Everyone reads it; only the owner, or the agent that owns it, writes.",
  },
  private: {
    label: "Each person their own",
    description: "Each person reads and writes only their own rows, even the owner.",
  },
};

/** Whether a schema is within the engine's bounds, and why not. */
export function schemaProblem(collections: readonly DataCollection[]): string | null {
  if (collections.length > DATA_COLLECTIONS_MAX) {
    return `A sheet keeps at most ${DATA_COLLECTIONS_MAX} collections.`;
  }
  for (const c of collections) {
    if ((c.columns?.length ?? 0) > DATA_COLUMNS_MAX) {
      return `A collection has at most ${DATA_COLUMNS_MAX} columns, and ${c.title || c.name} has ${c.columns?.length}.`;
    }
    for (const col of c.columns ?? []) {
      if (hasOptions(col.type) && (col.options ?? []).length === 0) {
        return `${col.label || col.key} offers no choices yet: add at least one.`;
      }
    }
  }
  return null;
}

/** A status option, with its category and the tone it is drawn in. */
function status(label: string, category: string, color: string, taken: string[]): DataOption {
  const id = optionID(label, taken);
  taken.push(id);
  return { id, label, category, color };
}

/** A select option in one of the node hues an author picks for a category. */
function choice(label: string, color: string, taken: string[]): DataOption {
  const id = optionID(label, taken);
  taken.push(id);
  return { id, label, color };
}

/**
 * The Review sheet (template X1): one row per thing to review, the reviewer
 * a person or an agent, a status every reader can set on rows assigned to
 * them, chips, a done box and notes. The item and its details are the
 * owner's, so a reviewer fills in the verdict without rewriting what is under
 * review; the status, done box and notes are written by a contributor on the
 * rows assigned to them, and by editors on any.
 */
export function reviewSheetSchema(): DataCollection[] {
  const statuses: string[] = [];
  const labels: string[] = [];
  const reviewer: DataColumn = {
    key: "reviewer",
    type: "person",
    label: "Reviewer",
    write: "editor",
  };
  const assigned = (column: DataColumn): DataColumn => ({
    ...column,
    write: "contributor",
    when: "assigned_to_viewer",
    assigned_field: "reviewer",
  });
  return [
    {
      name: "items",
      title: "Review",
      preset: "shared",
      read: "all",
      create: "editor",
      delete: "editor",
      columns: [
        {
          key: "item",
          type: "text",
          label: "Item",
          primary: true,
          required: true,
          write: "editor",
        },
        { key: "details", type: "longtext", label: "Details", write: "editor" },
        reviewer,
        assigned({
          key: "status",
          type: "status",
          label: "Status",
          options: [
            status("Pending", "todo", "neutral", statuses),
            status("Approved", "done", "success", statuses),
            status("Changes requested", "in_progress", "warning", statuses),
            status("Blocked", "blocked", "danger", statuses),
          ],
          default: "pending",
        }),
        assigned({
          key: "labels",
          type: "multiselect",
          label: "Labels",
          options: [
            choice("Must have", "rose", labels),
            choice("Nice to have", "blue", labels),
            choice("Question", "amber", labels),
          ],
        }),
        assigned({ key: "done", type: "checkbox", label: "Done" }),
        assigned({ key: "notes", type: "longtext", label: "Notes" }),
        {
          key: "work_item",
          type: "text",
          label: "Linked work item",
          max_length: 64,
          write: "editor",
        },
      ],
    },
  ];
}

/** A blank sheet: one collection with one text column, ready to grow. */
export function blankSheetSchema(): DataCollection[] {
  return [
    {
      name: "rows",
      title: "Sheet",
      preset: "shared",
      read: "all",
      create: "editor",
      delete: "editor",
      columns: [{ key: "name", type: "text", label: "Name", primary: true }],
    },
  ];
}

/** The templates a new sheet starts from, in the order New offers them. */
export const SHEET_TEMPLATES = [
  {
    key: "review",
    label: "Review sheet",
    description:
      "One row per item to review, a reviewer for each, and a status, labels and notes they fill in.",
    schema: reviewSheetSchema,
  },
  {
    key: "blank",
    label: "Blank sheet",
    description: "One column to start from. Add columns on its Data tab.",
    schema: blankSheetSchema,
  },
] as const;

export type SheetTemplate = (typeof SHEET_TEMPLATES)[number]["key"];
