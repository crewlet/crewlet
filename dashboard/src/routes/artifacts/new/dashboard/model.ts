/**
 * New dashboard's model: what each engine source offers a person (its words,
 * its settings, the figures it suggests), the ready-made boards a dashboard
 * can start from, and the one function that turns a draft into the
 * `dashboard.json` the engine publishes.
 *
 * A PERSON COMPOSES, NEVER WRITES. Every choice here is one of the board
 * format's closed sets (a catalogue source, an enum value, a widget type over
 * the source's own columns), so a draft composes a spec the engine accepts.
 * The engine still holds it to the format at publish, and a refusal names
 * the field.
 *
 * A SOURCE THIS BUILD HAS NO WORDS FOR (one the engine grew after it) is still
 * offered: its catalogue summary, a count of its rows and a table of its
 * columns. The catalogue is the engine's; this module only says it well.
 */

import type {
  DashboardColumn,
  DashboardDataset,
  DashboardEngineSource,
  DashboardSourceParam,
  DashboardSpec,
  DashboardWidget,
} from "~/protocol/index.ts";
import {
  DASHBOARD_COLUMNS,
  DASHBOARD_MAX_DATASETS,
  DASHBOARD_MAX_TABLE_COLUMNS,
  DASHBOARD_MAX_WIDGETS,
} from "~/contract/artifacts.ts";
import { wordsOf } from "~/lib/artifacts/dashboard.ts";

/** Where a version's spec lives (`dashspec.EntryPath`). */
export const SPEC_PATH = "dashboard.json";

/** The spec format this build writes (`dashspec.FormatVersion`). */
const FORMAT_VERSION = 1;

/** The most bytes a widget's or a dataset's title holds (`artifacts.MaxTitleBytes`). */
const MAX_TITLE_BYTES = 120;

// ---- the sources, in a person's words ------------------------------------ //

export interface SourceWords {
  label: string;
  /** The part of the company it reads: Work, Spend, Agents. */
  area: string;
  description: string;
}

const SOURCE_WORDS: Readonly<Record<string, SourceWords>> = {
  "tracker.items": {
    label: "Work items",
    area: "Work",
    description: "Tasks in the tracker, one row each, with status, assignee and due date.",
  },
  "tracker.workload": {
    label: "Workload",
    area: "Work",
    description: "How much open work each person and agent is carrying.",
  },
  "spend.seats": {
    label: "Spend by seat",
    area: "Spend",
    description: "Tokens each person and agent used over a window.",
  },
  "spend.daily": {
    label: "Spend by day",
    area: "Spend",
    description: "Tokens used each day, split by seat, model or team.",
  },
  "agents.status": {
    label: "Agent status",
    area: "Agents",
    description: "What every agent is doing right now.",
  },
};

/** A source's words: this build's own, or the catalogue's for a source it does not know. */
export function sourceWords(src: DashboardEngineSource): SourceWords {
  const known = SOURCE_WORDS[src.name];
  if (known) return known;
  const domain = src.domain.charAt(0).toUpperCase() + src.domain.slice(1);
  return { label: src.name, area: domain, description: src.summary.replace(/`/g, "") };
}

// ---- settings: the parameters a person may set --------------------------- //

/** One choice of a setting; "" is the parameter left out. */
export interface SettingOption {
  value: string;
  label: string;
}

/** One parameter of a source, as the Data panel offers it. */
export interface Setting {
  key: string;
  label: string;
  options: SettingOption[];
}

/** A source's settings as a person set them, by parameter key. */
export type SourceSettings = Readonly<Record<string, string>>;

const PARAM_LABELS: Readonly<Record<string, string>> = {
  scope: "Whose work",
  status_group: "Status",
  days: "Window",
  group: "Split by",
};

const ENUM_WORDS: Readonly<Record<string, Readonly<Record<string, string>>>> = {
  scope: {
    all: "Everyone",
    mine: "Mine",
    queue: "My queue",
    priorities: "My priorities",
    triage: "Unassigned",
    blocked: "Blocked",
    overdue: "Overdue",
  },
  group: {
    seat: "Seat",
    phase: "Phase",
    model: "Model",
    provider: "Provider",
    unit: "Team",
    worker: "Worker",
  },
};

/** The windows a `days` parameter offers, clipped to the source's bounds. */
const DAY_WINDOWS = [7, 14, 30, 90];

/** What an absent parameter reads, where the source answers absence as something. */
const ABSENT_WORDS: Readonly<Record<string, string>> = {
  status_group: "Open work",
  days: "Current window",
};

function enumLabel(p: DashboardSourceParam, value: string): string {
  if (p.key === "status_group") {
    return wordsOf({ key: p.key, type: "text", words: "tracker_status_group" }, value);
  }
  return ENUM_WORDS[p.key]?.[value] ?? value.charAt(0).toUpperCase() + value.slice(1);
}

/**
 * The settings a source offers: every enum parameter, and a window of days.
 * A free-text parameter (a project key, a team) is not offered: a person
 * picks, never types a key, and an agent can write one.
 */
export function settingsOf(src: DashboardEngineSource): Setting[] {
  const out: Setting[] = [];
  for (const p of src.params) {
    const label = PARAM_LABELS[p.key] ?? p.key;
    const absent = p.default ? [] : [{ value: "", label: ABSENT_WORDS[p.key] ?? "Any" }];
    if (p.type === "enum" && p.enum?.length) {
      out.push({
        key: p.key,
        label,
        options: [...absent, ...p.enum.map((v) => ({ value: v, label: enumLabel(p, v) }))],
      });
    } else if (p.type === "number" && p.key === "days") {
      const days = DAY_WINDOWS.filter((d) => (p.min ?? 1) <= d && d <= (p.max ?? Infinity));
      out.push({
        key: p.key,
        label,
        options: [...absent, ...days.map((d) => ({ value: String(d), label: `Last ${d} days` }))],
      });
    }
  }
  return out;
}

/** A setting's value as the person set it, else the source's default ("" for none). */
export function settingValue(src: DashboardEngineSource, set: SourceSettings, key: string): string {
  return set[key] ?? src.params.find((p) => p.key === key)?.default ?? "";
}

// ---- figures a source suggests ------------------------------------------- //

/** One figure a source suggests: a widget over it, with the parameters it pins. */
export interface Recipe {
  /** Unique within the source; with it, what a placed figure was made from. */
  key: string;
  title: string;
  /** Parameters the figure reads at fixed values, over the person's settings. */
  pins?: Readonly<Record<string, string>>;
  widget: Omit<DashboardWidget, "id" | "dataset" | "title">;
}

const RECIPES: Readonly<Record<string, readonly Recipe[]>> = {
  "tracker.items": [
    {
      key: "count",
      title: "Work items",
      widget: { type: "stat", w: 3, agg: "count", format: "integer" },
    },
    {
      key: "overdue",
      title: "Overdue",
      pins: { scope: "overdue" },
      widget: { type: "stat", w: 3, agg: "count", format: "integer" },
    },
    {
      key: "blocked",
      title: "Blocked",
      pins: { scope: "blocked" },
      widget: { type: "stat", w: 3, agg: "count", format: "integer" },
    },
    {
      key: "table",
      title: "Work items",
      widget: {
        type: "table",
        w: 12,
        columns: ["key", "title", "status", "assignee", "due"],
        limit: 10,
      },
    },
    {
      key: "overdue_table",
      title: "Overdue work",
      pins: { scope: "overdue" },
      widget: { type: "table", w: 6, columns: ["key", "title", "assignee", "due"], limit: 10 },
    },
  ],
  "tracker.workload": [
    {
      key: "open",
      title: "Open work",
      widget: { type: "stat", w: 3, value: "open", agg: "sum", format: "integer" },
    },
    {
      key: "blocked",
      title: "Blocked",
      widget: { type: "stat", w: 3, value: "blocked", agg: "sum", format: "integer" },
    },
    {
      key: "overdue",
      title: "Overdue",
      widget: { type: "stat", w: 3, value: "overdue", agg: "sum", format: "integer" },
    },
    {
      key: "by_assignee",
      title: "Open work by assignee",
      widget: { type: "bars", w: 6, label: "handle", value: "open", limit: 10, format: "integer" },
    },
    {
      key: "table",
      title: "Workload",
      widget: {
        type: "table",
        w: 6,
        columns: ["handle", "open", "points", "blocked", "overdue"],
        limit: 20,
      },
    },
  ],
  "spend.seats": [
    {
      key: "total",
      title: "Tokens used",
      widget: { type: "stat", w: 3, value: "total_tokens", agg: "sum", format: "compact" },
    },
    {
      key: "by_seat",
      title: "Tokens by seat",
      widget: {
        type: "bars",
        w: 6,
        label: "name",
        value: "total_tokens",
        limit: 10,
        format: "compact",
      },
    },
    {
      key: "share",
      title: "Share of tokens",
      widget: { type: "share", w: 6, label: "name", value: "total_tokens", format: "compact" },
    },
    {
      key: "table",
      title: "Spend by seat",
      widget: {
        type: "table",
        w: 6,
        columns: ["name", "total_tokens", "input_tokens", "output_tokens", "calls"],
        limit: 20,
      },
    },
  ],
  "spend.daily": [
    {
      key: "total",
      title: "Tokens in the window",
      widget: { type: "stat", w: 3, value: "total_tokens", agg: "sum", format: "compact" },
    },
    {
      key: "columns",
      title: "Tokens per day",
      widget: {
        type: "columns",
        w: 8,
        x: "day",
        y: ["total_tokens"],
        by: "band",
        format: "compact",
      },
    },
    {
      key: "trend",
      title: "Token trend",
      widget: {
        type: "timeseries",
        w: 8,
        x: "day",
        y: ["total_tokens"],
        by: "band",
        format: "compact",
      },
    },
  ],
  "agents.status": [
    {
      key: "count",
      title: "Agents",
      widget: { type: "stat", w: 3, agg: "count", format: "integer" },
    },
    {
      key: "table",
      title: "Agents now",
      widget: {
        type: "table",
        w: 12,
        columns: ["name", "activity", "phase", "last_turn_at", "last_outcome"],
        limit: 20,
      },
    },
  ],
};

/**
 * The figures a source suggests: this build's for a source it knows, else a
 * count of its rows and a table of its first columns.
 */
export function recipesOf(src: DashboardEngineSource): readonly Recipe[] {
  const known = RECIPES[src.name];
  if (known) return known;
  const label = sourceWords(src).label;
  return [
    {
      key: "count",
      title: label,
      widget: { type: "stat", w: 3, agg: "count", format: "integer" },
    },
    {
      key: "table",
      title: label,
      widget: {
        type: "table",
        w: 12,
        columns: src.columns.slice(0, Math.min(6, DASHBOARD_MAX_TABLE_COLUMNS)).map((c) => c.key),
        limit: 10,
      },
    },
  ];
}

/** A recipe of a source, by key. */
export function recipeOf(src: DashboardEngineSource, key: string): Recipe | undefined {
  return recipesOf(src).find((r) => r.key === key);
}

// ---- the draft ----------------------------------------------------------- //

/** One figure on the draft board. */
export interface Placed {
  /** The widget's id in the spec. */
  id: string;
  source: string;
  recipe: string;
  /** Width in board columns. */
  w: number;
}

/** What New dashboard holds while a person composes. */
export interface Draft {
  /** Each source's settings, by source name. */
  settings: Readonly<Record<string, SourceSettings>>;
  placed: readonly Placed[];
}

export const EMPTY_DRAFT: Draft = { settings: {}, placed: [] };

/** Whether the board can take another figure. */
export function hasRoom(d: Draft): boolean {
  return d.placed.length < DASHBOARD_MAX_WIDGETS;
}

/** A widget id no figure on the draft holds yet, from the recipe it is made from. */
function freshId(d: Draft, source: string, recipe: string): string {
  const stem = idStem(`${source}_${recipe}`);
  const taken = new Set(d.placed.map((p) => p.id));
  if (!taken.has(stem)) return stem;
  for (let n = 2; ; n++) {
    const id = `${stem.slice(0, 36)}_${n}`;
    if (!taken.has(id)) return id;
  }
}

/** A spec id from words: a lowercase letter, then lowercase letters, digits and `_`, at most 40. */
function idStem(text: string): string {
  const s = text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^[^a-z]+/, "")
    .replace(/_+$/, "");
  return (s || "d").slice(0, 40).replace(/_+$/, "");
}

/** The draft with one more figure at its end, and that figure's id. */
export function place(
  d: Draft,
  source: string,
  recipe: string,
  w: number,
): { draft: Draft; id: string } {
  const id = freshId(d, source, recipe);
  return { draft: { ...d, placed: [...d.placed, { id, source, recipe, w }] }, id };
}

/** The draft with one figure taken off. */
export function unplace(d: Draft, id: string): Draft {
  return { ...d, placed: d.placed.filter((p) => p.id !== id) };
}

/** The draft with one figure's width changed, held to the grid. */
export function resize(d: Draft, id: string, w: number): Draft {
  const width = Math.min(DASHBOARD_COLUMNS, Math.max(1, Math.round(w)));
  return { ...d, placed: d.placed.map((p) => (p.id === id ? { ...p, w: width } : p)) };
}

/** The draft with its figures in this order (ids it does not hold are ignored). */
export function reorder(d: Draft, order: readonly string[]): Draft {
  const byId = new Map(d.placed.map((p) => [p.id, p]));
  const next = order.flatMap((id) => {
    const p = byId.get(id);
    return p ? [p] : [];
  });
  return { ...d, placed: [...next, ...d.placed.filter((p) => !order.includes(p.id))] };
}

/** The draft with one source's setting changed. */
export function setSetting(d: Draft, source: string, key: string, value: string): Draft {
  return { ...d, settings: { ...d.settings, [source]: { ...d.settings[source], [key]: value } } };
}

// ---- datasets: what each figure reads ------------------------------------ //

/** One engine read a draft makes: a source with its arguments. */
export interface DraftDataset {
  /** Names the read: the source and its arguments, the same for the same read. */
  key: string;
  source: DashboardEngineSource;
  /** Every argument with a value, as the query string a preview sends. */
  args: Readonly<Record<string, string>>;
  title: string;
}

/**
 * The read one recipe makes over a source with a person's settings: their
 * settings, the recipe's pins over them, each argument at the source's
 * default left out so two figures reading the same rows share one dataset.
 */
export function datasetOf(
  src: DashboardEngineSource,
  set: SourceSettings,
  recipe: Recipe | undefined,
): DraftDataset {
  const args: Record<string, string> = {};
  const words: string[] = [];
  for (const p of src.params) {
    const value = recipe?.pins?.[p.key] ?? set[p.key] ?? "";
    if (value === "" || value === p.default) continue;
    args[p.key] = value;
    words.push(
      p.type === "enum"
        ? enumLabel(p, value).toLowerCase()
        : p.key === "days"
          ? `last ${value} days`
          : value,
    );
  }
  const keys = Object.keys(args).sort();
  const label = sourceWords(src).label;
  return {
    key: [src.name, ...keys.map((k) => `${k}=${args[k]}`)].join("&"),
    source: src,
    args,
    title: clip(words.length ? `${label}, ${words.join(", ")}` : label),
  };
}

/** Text cut to at most `MAX_TITLE_BYTES` of UTF-8 at a character boundary. */
function clip(text: string): string {
  const enc = new TextEncoder();
  if (enc.encode(text).length <= MAX_TITLE_BYTES) return text;
  let out = "";
  for (const ch of text) {
    if (enc.encode(out + ch + "…").length > MAX_TITLE_BYTES) break;
    out += ch;
  }
  return `${out}…`;
}

/** Everything one placed figure is drawn from. */
export interface Resolved {
  placed: Placed;
  source: DashboardEngineSource;
  recipe: Recipe;
  dataset: DraftDataset;
  widget: DashboardWidget;
}

/** The draft's figures with what each reads; a figure over a source the catalogue lost is dropped. */
export function resolve(d: Draft, catalogue: readonly DashboardEngineSource[]): Resolved[] {
  const byName = new Map(catalogue.map((s) => [s.name, s]));
  return d.placed.flatMap((p) => {
    const source = byName.get(p.source);
    const recipe = source ? recipeOf(source, p.recipe) : undefined;
    if (!source || !recipe) return [];
    const dataset = datasetOf(source, d.settings[p.source] ?? {}, recipe);
    return [
      { placed: p, source, recipe, dataset, widget: widgetOf(recipe, p.id, p.w, dataset.key) },
    ];
  });
}

/** A recipe as a widget of a board, reading the dataset named `dataset`. */
export function widgetOf(r: Recipe, id: string, w: number, dataset: string): DashboardWidget {
  return { ...r.widget, id, w, title: r.title, dataset };
}

/** Whether the draft's figures read more datasets than a board declares. */
export function tooManyDatasets(resolved: readonly Resolved[]): boolean {
  return new Set(resolved.map((r) => r.dataset.key)).size > DASHBOARD_MAX_DATASETS;
}

/**
 * The draft as the board format writes it: one engine dataset per distinct
 * read, its id from its source and arguments, and every figure in the order
 * placed. A number parameter is written as a number.
 */
export function composeSpec(
  title: string,
  d: Draft,
  catalogue: readonly DashboardEngineSource[],
): DashboardSpec {
  const resolved = resolve(d, catalogue);
  const ids = new Map<string, string>();
  const datasets: DashboardDataset[] = [];
  for (const r of resolved) {
    if (ids.has(r.dataset.key)) continue;
    const stem = idStem(
      [
        r.source.name,
        ...Object.keys(r.dataset.args)
          .sort()
          .map((k) => r.dataset.args[k]),
      ].join("_"),
    );
    let id = stem;
    for (let n = 2; [...ids.values()].includes(id); n++) id = `${stem.slice(0, 36)}_${n}`;
    ids.set(r.dataset.key, id);
    const params: Record<string, string | number> = {};
    for (const [k, v] of Object.entries(r.dataset.args)) {
      const p = r.source.params.find((x) => x.key === k);
      params[k] = p?.type === "number" ? Number(v) : v;
    }
    datasets.push({
      id,
      title: r.dataset.title,
      source: "engine",
      engine: {
        name: r.source.name,
        ...(Object.keys(params).length ? { params } : {}),
      },
    });
  }
  return {
    v: FORMAT_VERSION,
    title: title.trim(),
    datasets,
    widgets: resolved.map((r) => ({ ...r.widget, dataset: ids.get(r.dataset.key) ?? "" })),
  };
}

/** The columns a source's preview answers, for a figure drawn before any row arrives. */
export function shapeOf(src: DashboardEngineSource): { columns: DashboardColumn[]; key?: string } {
  return { columns: src.columns, ...(src.key ? { key: src.key } : {}) };
}

// ---- starting points ----------------------------------------------------- //

/** A ready-made board: figures by source and recipe, with their widths. */
export interface Starter {
  key: string;
  label: string;
  description: string;
  figures: readonly { source: string; recipe: string; w?: number }[];
}

export const STARTERS: readonly Starter[] = [
  {
    key: "workload",
    label: "Team workload",
    description: "Open, blocked and overdue work, and who is carrying it.",
    figures: [
      { source: "tracker.workload", recipe: "open", w: 4 },
      { source: "tracker.workload", recipe: "blocked", w: 4 },
      { source: "tracker.workload", recipe: "overdue", w: 4 },
      { source: "tracker.workload", recipe: "by_assignee" },
      { source: "tracker.items", recipe: "overdue_table" },
    ],
  },
  {
    key: "spend",
    label: "Token spend",
    description: "Tokens per day and who used them.",
    figures: [
      { source: "spend.daily", recipe: "total", w: 4 },
      { source: "spend.daily", recipe: "columns" },
      { source: "spend.seats", recipe: "by_seat" },
      { source: "spend.seats", recipe: "share" },
    ],
  },
  {
    key: "agents",
    label: "Agent activity",
    description: "What every agent is doing, and the work they hold.",
    figures: [
      { source: "agents.status", recipe: "count", w: 4 },
      { source: "tracker.workload", recipe: "open", w: 4 },
      { source: "spend.seats", recipe: "total", w: 4 },
      { source: "agents.status", recipe: "table" },
    ],
  },
  {
    key: "work",
    label: "Work overview",
    description: "How much work is open, overdue and blocked, and the tasks themselves.",
    figures: [
      { source: "tracker.items", recipe: "count", w: 4 },
      { source: "tracker.items", recipe: "overdue", w: 4 },
      { source: "tracker.items", recipe: "blocked", w: 4 },
      { source: "tracker.items", recipe: "table" },
    ],
  },
];

/**
 * A draft begun from a starter, over the sources this catalogue answers: a
 * figure over a source the engine does not list is left out.
 */
export function fromStarter(s: Starter, catalogue: readonly DashboardEngineSource[]): Draft {
  let d = EMPTY_DRAFT;
  for (const f of s.figures) {
    const src = catalogue.find((c) => c.name === f.source);
    const recipe = src ? recipeOf(src, f.recipe) : undefined;
    if (!src || !recipe) continue;
    d = place(d, f.source, f.recipe, f.w ?? recipe.widget.w).draft;
  }
  return d;
}

/** The sources a starter reads, in the order its figures name them. */
export function starterSources(s: Starter): string[] {
  return [...new Set(s.figures.map((f) => f.source))];
}
