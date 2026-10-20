/**
 * The dashboards: each a board (`dashboard.json`, the engine's `dashspec`) of
 * datasets and the widgets drawn from them on the twelve-column grid, and the
 * store a `store` dataset reads. Beta health is kept by Crewlet-PM's morning
 * refresh; Sprint delivery reads the engine's own tracker and spend as the
 * viewer.
 */

import type { DashboardSpec, DataCollection } from "~/protocol/types.ts";

/** The weekly figures Crewlet-PM writes each morning from the beta's ledger exports. */
export const WEEKLY: DataCollection = {
  name: "weekly",
  title: "Weekly beta stats",
  preset: "author",
  read: "all",
  create: "editor",
  delete: "editor",
  columns: [
    { key: "week", type: "date", label: "Week of", primary: true },
    { key: "groups", type: "number", label: "Active groups" },
    { key: "expenses", type: "number", label: "Expenses recorded" },
    { key: "settle_ups", type: "number", label: "Settle-ups" },
    { key: "settled", type: "number", label: "Settled within a week" },
    { key: "payments", type: "number", label: "Payments per settle-up" },
  ],
};

const WEEKLY_FIGURES = [
  [31, 96, 22, 79, 2.4],
  [34, 118, 28, 81, 2.2],
  [36, 121, 30, 84, 2.1],
  [39, 140, 35, 85, 2.0],
  [41, 151, 37, 88, 1.8],
  [43, 162, 41, 89, 1.7],
  [45, 170, 44, 90, 1.7],
  [48, 184, 47, 92, 1.6],
] as const;

/** The weekly rows, each week's Monday counted back from the company's now, the latest last. */
export function weeklyRows(now: number): Record<string, unknown>[] {
  const today = new Date(now);
  const monday =
    Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate()) -
    ((today.getUTCDay() + 6) % 7) * 86_400_000;
  return WEEKLY_FIGURES.map(([groups, expenses, settle_ups, settled, payments], i) => ({
    week: new Date(monday - (WEEKLY_FIGURES.length - 1 - i) * 7 * 86_400_000)
      .toISOString()
      .slice(0, 10),
    groups,
    expenses,
    settle_ups,
    settled,
    payments,
  }));
}

/** Groups with an open balance older than three days, kept by the same refresh. */
export const ATTENTION: DataCollection = {
  name: "attention",
  title: "Groups needing a look",
  preset: "author",
  read: "all",
  create: "editor",
  delete: "editor",
  columns: [
    { key: "group", type: "text", label: "Group", primary: true },
    { key: "open_balance", type: "text", label: "Open balance" },
    { key: "oldest", type: "number", label: "Oldest unpaid, days" },
    { key: "members", type: "number", label: "Members" },
  ],
};

export const ATTENTION_ROWS = [
  { group: "Lisbon trip", open_balance: "€412.60", oldest: 9, members: 6 },
  { group: "Climbing club", open_balance: "€208.00", oldest: 6, members: 11 },
  { group: "Flat 4B", open_balance: "€50.15", oldest: 4, members: 3 },
];

/** Groups by size and kind: a fixed file the beta survey produced. */
export const GROUP_SIZES_CSV = `size,groups
Two people,7
Three,12
Four,13
Five,7
Six,5
Seven or more,4
`;

export const BETA_HEALTH: DashboardSpec = {
  v: 1,
  title: "Beta health",
  datasets: [
    {
      id: "weekly",
      title: "Weekly beta stats",
      source: "store",
      store: { collection: "weekly" },
    },
    {
      id: "attention",
      title: "Groups needing a look",
      source: "store",
      store: { collection: "attention" },
    },
    {
      id: "sizes",
      title: "Groups by size",
      source: "file",
      file: { path: "data/group-sizes.csv" },
      columns: [
        { key: "size", type: "text", label: "Members" },
        { key: "groups", type: "number", label: "Groups" },
      ],
      key: "size",
    },
  ],
  widgets: [
    {
      id: "groups",
      type: "stat",
      title: "Active groups",
      dataset: "weekly",
      value: "groups",
      agg: "last",
      format: "integer",
      w: 3,
    },
    {
      id: "expenses",
      type: "stat",
      title: "Expenses this week",
      dataset: "weekly",
      value: "expenses",
      agg: "last",
      format: "integer",
      w: 3,
    },
    {
      id: "settled",
      type: "stat",
      title: "Settled within a week",
      dataset: "weekly",
      value: "settled",
      agg: "last",
      format: "integer",
      unit: "%",
      w: 3,
    },
    {
      id: "payments",
      type: "stat",
      title: "Payments per settle-up",
      dataset: "weekly",
      value: "payments",
      agg: "last",
      format: "number",
      w: 3,
    },
    {
      id: "trend",
      type: "timeseries",
      title: "Expenses and settle-ups a week",
      dataset: "weekly",
      x: "week",
      y: ["expenses", "settle_ups"],
      w: 7,
    },
    {
      id: "sizes",
      type: "bars",
      title: "Groups by size",
      dataset: "sizes",
      label: "size",
      value: "groups",
      w: 5,
    },
    {
      id: "attention",
      type: "table",
      title: "Groups needing a look",
      dataset: "attention",
      columns: ["group", "open_balance", "oldest", "members"],
      w: 12,
    },
  ],
};

export const SPRINT_DELIVERY: DashboardSpec = {
  v: 1,
  title: "Sprint delivery",
  datasets: [
    {
      id: "items",
      title: "FAIR tasks",
      source: "engine",
      engine: { name: "tracker.items", params: { project: "FAIR", limit: 200 } },
    },
    {
      id: "by_group",
      title: "FAIR tasks by status",
      source: "calc",
      calc: {
        from: "items",
        steps: [
          { op: "group", by: ["status_group"], aggregates: [{ column: "tasks", agg: "count" }] },
        ],
      },
    },
    {
      id: "active",
      title: "FAIR tasks in progress",
      source: "calc",
      calc: {
        from: "items",
        steps: [
          { op: "filter", where: [{ column: "status_group", cmp: "eq", value: "active" }] },
          { op: "group", aggregates: [{ column: "tasks", agg: "count" }] },
        ],
      },
    },
    {
      id: "waiting",
      title: "FAIR tasks not started",
      source: "calc",
      calc: {
        from: "items",
        steps: [
          { op: "filter", where: [{ column: "status_group", cmp: "eq", value: "not_started" }] },
          { op: "group", aggregates: [{ column: "tasks", agg: "count" }] },
        ],
      },
    },
    {
      id: "workload",
      title: "Open work by assignee",
      source: "engine",
      engine: { name: "tracker.workload" },
    },
    {
      id: "tokens",
      title: "Tokens a day, by agent",
      source: "engine",
      engine: { name: "spend.daily", params: { days: 14 } },
    },
  ],
  widgets: [
    { id: "open", type: "stat", title: "FAIR tasks", dataset: "items", agg: "count", w: 4 },
    {
      id: "in-progress",
      type: "stat",
      title: "In progress",
      dataset: "active",
      value: "tasks",
      w: 4,
    },
    {
      id: "not-started",
      type: "stat",
      title: "Not started",
      dataset: "waiting",
      value: "tasks",
      w: 4,
    },
    {
      id: "tokens",
      type: "timeseries",
      title: "Tokens a day, by agent",
      dataset: "tokens",
      x: "day",
      y: ["total_tokens"],
      by: "band",
      w: 8,
    },
    {
      id: "by-assignee",
      type: "bars",
      title: "Open work by assignee",
      dataset: "workload",
      label: "handle",
      value: "open",
      w: 4,
    },
    {
      id: "by-status",
      type: "share",
      title: "FAIR tasks by status",
      dataset: "by_group",
      label: "status_group",
      value: "tasks",
      w: 6,
    },
    {
      id: "note",
      type: "note",
      text: "Every figure is read from Crewlet as you, so a task you cannot open is not counted for you.",
      w: 6,
    },
  ],
};

export const TEMPLATE_DASHBOARD: DashboardSpec = {
  v: 1,
  title: "KPI dashboard",
  datasets: [
    {
      id: "figures",
      title: "Figures",
      source: "store",
      store: { collection: "figures" },
    },
  ],
  widgets: [
    {
      id: "first",
      type: "stat",
      title: "A figure",
      dataset: "figures",
      value: "value",
      agg: "last",
      w: 4,
    },
    {
      id: "trend",
      type: "timeseries",
      title: "Over time",
      dataset: "figures",
      x: "period",
      y: ["value"],
      w: 8,
    },
  ],
};

export const FIGURES: DataCollection = {
  name: "figures",
  title: "Figures",
  preset: "author",
  read: "all",
  create: "editor",
  delete: "editor",
  columns: [
    { key: "period", type: "date", label: "Period", primary: true },
    { key: "value", type: "number", label: "Value" },
  ],
};
