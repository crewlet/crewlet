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

/**
 * How many things a schema draft changes, as the editor's footer counts them:
 * a collection added or removed, a collection's own policy, and each column
 * added, removed or edited. A change none of those catch (an order, say) is
 * still one change, so a draft that differs never counts as none.
 */
export function schemaChangeCount(
  before: readonly DataCollection[],
  after: readonly DataCollection[],
): number {
  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  const was = new Map(before.map((c) => [c.name, c]));
  const now = new Map(after.map((c) => [c.name, c]));
  let count = 0;
  for (const name of new Set([...was.keys(), ...now.keys()])) {
    const a = was.get(name);
    const b = now.get(name);
    if (!a || !b) {
      count += 1;
      continue;
    }
    const { columns: aColumns = [], ...aPolicy } = a;
    const { columns: bColumns = [], ...bPolicy } = b;
    if (!same(aPolicy, bPolicy)) count += 1;
    const aByKey = new Map(aColumns.map((col) => [col.key, col]));
    const bByKey = new Map(bColumns.map((col) => [col.key, col]));
    for (const key of new Set([...aByKey.keys(), ...bByKey.keys()])) {
      if (!same(aByKey.get(key), bByKey.get(key))) count += 1;
    }
  }
  return count === 0 && !same(before, after) ? 1 : count;
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

/** One collection people who can edit change, with the columns given. */
function sharedCollection(name: string, title: string, columns: DataColumn[]): DataCollection {
  return {
    name,
    title,
    preset: "shared",
    read: "all",
    create: "editor",
    delete: "editor",
    columns,
  };
}

/**
 * The Tracker: one row per piece of work, who owns it, where it stands, how
 * urgent it is and when it is due.
 */
export function trackerSheetSchema(): DataCollection[] {
  const statuses: string[] = [];
  const priorities: string[] = [];
  return [
    sharedCollection("tasks", "Tracker", [
      { key: "task", type: "text", label: "Task", primary: true, required: true },
      { key: "owner", type: "person", label: "Owner" },
      {
        key: "status",
        type: "status",
        label: "Status",
        options: [
          status("Not started", "todo", "neutral", statuses),
          status("In progress", "in_progress", "info", statuses),
          status("Done", "done", "success", statuses),
          status("Blocked", "blocked", "danger", statuses),
        ],
        default: "not_started",
      },
      {
        key: "priority",
        type: "select",
        label: "Priority",
        options: [
          choice("High", "rose", priorities),
          choice("Medium", "amber", priorities),
          choice("Low", "blue", priorities),
        ],
      },
      { key: "due", type: "date", label: "Due" },
      { key: "notes", type: "longtext", label: "Notes" },
    ]),
  ];
}

/** The Budget: one line per cost, its category, the amount, who owns it and whether it is paid. */
export function budgetSheetSchema(): DataCollection[] {
  const categories: string[] = [];
  return [
    sharedCollection("lines", "Budget", [
      { key: "item", type: "text", label: "Item", primary: true, required: true },
      {
        key: "category",
        type: "select",
        label: "Category",
        options: [
          choice("Software", "purple", categories),
          choice("Contractors", "cyan", categories),
          choice("Travel", "amber", categories),
          choice("Events", "rose", categories),
        ],
      },
      { key: "amount", type: "number", label: "Amount", min: 0 },
      { key: "owner", type: "person", label: "Owner" },
      { key: "paid", type: "checkbox", label: "Paid" },
      { key: "notes", type: "longtext", label: "Notes" },
    ]),
  ];
}

/** The Roadmap: one row per initiative, its quarter, where it stands, who leads it and how sure the team is. */
export function roadmapSheetSchema(): DataCollection[] {
  const quarters: string[] = [];
  const statuses: string[] = [];
  return [
    sharedCollection("initiatives", "Roadmap", [
      { key: "initiative", type: "text", label: "Initiative", primary: true, required: true },
      {
        key: "quarter",
        type: "select",
        label: "Quarter",
        options: [
          choice("Q1", "cyan", quarters),
          choice("Q2", "green", quarters),
          choice("Q3", "amber", quarters),
          choice("Q4", "purple", quarters),
        ],
      },
      {
        key: "status",
        type: "status",
        label: "Status",
        options: [
          status("Exploring", "todo", "neutral", statuses),
          status("Building", "in_progress", "info", statuses),
          status("Shipped", "done", "success", statuses),
          status("Paused", "blocked", "warning", statuses),
        ],
        default: "exploring",
      },
      { key: "lead", type: "person", label: "Lead" },
      { key: "target", type: "date", label: "Target date" },
      { key: "confidence", type: "rating", label: "Confidence", max: 5 },
    ]),
  ];
}

/**
 * The Sign-up sheet: everyone it is shared with adds their own row and
 * changes only that one, so a list fills itself in without anybody's row
 * being rewritten by somebody else.
 */
export function signupSheetSchema(): DataCollection[] {
  const slots: string[] = [];
  return [
    {
      name: "signups",
      title: "Sign-up",
      preset: "contribute",
      read: "all",
      create: "contributor",
      delete: "editor",
      columns: [
        { key: "name", type: "text", label: "Name", primary: true, required: true },
        {
          key: "slot",
          type: "select",
          label: "Slot",
          options: [
            choice("Morning", "cyan", slots),
            choice("Afternoon", "amber", slots),
            choice("Evening", "purple", slots),
          ],
        },
        { key: "notes", type: "longtext", label: "Notes" },
      ],
    },
  ];
}

/** A calendar date `days` after `from`, in the reader's own calendar, as a date column holds it. */
function dayAfter(from: Date, days: number): string {
  const at = new Date(from.getFullYear(), from.getMonth(), from.getDate() + days);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

/** A template's example rows, by column key: what a new sheet starts with when the reader keeps them. */
export type ExampleRows = (today: Date) => Record<string, unknown>[];

/**
 * The ways a new sheet starts, in the order New offers them: blank, then the
 * templates. Each names its columns (`schema`) and a few example rows that
 * show what goes where (`examples`), which the reader may keep or drop.
 */
export const SHEET_TEMPLATES = [
  {
    key: "blank",
    label: "Blank",
    description: "One column to start from. Shape the rest in the grid.",
    schema: blankSheetSchema,
    examples: (() => []) as ExampleRows,
  },
  {
    key: "tracker",
    label: "Tracker",
    description: "Work with an owner, a status, a priority and a due date.",
    schema: trackerSheetSchema,
    examples: ((today) => [
      {
        task: "Draft the launch announcement",
        status: "in_progress",
        priority: "high",
        due: dayAfter(today, 3),
      },
      {
        task: "Review the pricing page",
        status: "not_started",
        priority: "medium",
        due: dayAfter(today, 7),
      },
      {
        task: "Book the customer interviews",
        status: "done",
        priority: "low",
        due: dayAfter(today, -2),
      },
    ]) as ExampleRows,
  },
  {
    key: "review",
    label: "Review",
    description:
      "Items to review, a reviewer for each, and the status, labels and notes they fill in.",
    schema: reviewSheetSchema,
    examples: (() => [
      {
        item: "Single sign-on with SAML",
        details: "Customers on the Enterprise plan sign in through their identity provider.",
        status: "pending",
        labels: ["must_have"],
      },
      {
        item: "Audit log export",
        details: "A daily export of every admin action as CSV.",
        status: "changes_requested",
        labels: ["nice_to_have", "question"],
      },
      { item: "Data residency in the EU", status: "approved", labels: ["must_have"], done: true },
    ]) as ExampleRows,
  },
  {
    key: "budget",
    label: "Budget",
    description: "Costs by category, with the amount, an owner and whether each is paid.",
    schema: budgetSheetSchema,
    examples: (() => [
      { item: "Design tool seats", category: "software", amount: 1200, paid: true },
      { item: "Security audit", category: "contractors", amount: 9000, paid: false },
      { item: "Launch event venue", category: "events", amount: 4500, paid: false },
    ]) as ExampleRows,
  },
  {
    key: "roadmap",
    label: "Roadmap",
    description: "Initiatives by quarter, with a status, a lead, a target date and a confidence.",
    schema: roadmapSheetSchema,
    examples: ((today) => [
      {
        initiative: "Self-serve onboarding",
        quarter: "q1",
        status: "building",
        target: dayAfter(today, 30),
        confidence: 4,
      },
      {
        initiative: "Usage-based billing",
        quarter: "q2",
        status: "exploring",
        target: dayAfter(today, 90),
        confidence: 3,
      },
      {
        initiative: "Mobile app",
        quarter: "q3",
        status: "paused",
        target: dayAfter(today, 180),
        confidence: 2,
      },
    ]) as ExampleRows,
  },
  {
    key: "signup",
    label: "Sign-up",
    description: "Everyone adds their own row and changes only theirs.",
    schema: signupSheetSchema,
    examples: (() => [
      { name: "Ana Lopez", slot: "morning" },
      { name: "Ben Okafor", slot: "afternoon", notes: "Happy to swap if needed." },
    ]) as ExampleRows,
  },
] as const;

export type SheetTemplate = (typeof SHEET_TEMPLATES)[number]["key"];
