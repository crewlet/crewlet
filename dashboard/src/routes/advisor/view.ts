/**
 * What the Advisor's page draws, as one set of shapes: the company's numbers
 * over a range (its stats and charts), each finding as a row, and a run.
 *
 * THE SHAPES ARE THE ADVISOR'S OWN SNAPSHOT AND FINDING CONTRACT: a stat is a
 * number with its unit, the previous range's value and the direction that is
 * good; a chart is a kind (columns across, stacked columns, bars down) over
 * labelled x values with aligned series. The page draws only these, so its
 * figures, charts and rows read the same whichever answers fill them.
 *
 * TODAY THEY ARE FILLED HERE, by pure functions over the questions the
 * company's other screens ask (`work_flow`, `work_workload`, `turn_outcomes`,
 * the newest weekly report), so a figure is the number Worklet and Spend show
 * for the same window; and each finding's headline, stats and actions are read
 * off its facts and its fix. A figure not measured is null, never a zero.
 */

import { href } from "~/app/router.tsx";
import { TAKE_BACKUP } from "~/lib/backups.ts";
import { fmtCount } from "~/lib/format.ts";
import { wakeWords } from "~/lib/wakes.ts";
import type { WorkFlowAnswer, WorkloadAnswer } from "~/protocol/index.ts";
import type { TurnOutcomeRow } from "~/protocol/turnOutcomes.ts";
import type { AdvisorReport } from "~/protocol/advisorFindings.ts";
import type { AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";
import type { AdvisorPage } from "~/protocol/advisor.ts";
import { ranked } from "./words.ts";

/** What a stat's number counts. */
export type StatUnit = "hours" | "minutes" | "items" | "turns" | "tokens" | "percent" | "count";

/** The windows the page reads. */
export const RANGES = [7, 14, 30] as const;
export type RangeDays = (typeof RANGES)[number];

/** One figure of the strip. `value` null is a figure not measured. */
export interface ViewStat {
  key: string;
  label: string;
  value: number | null;
  unit: StatUnit;
  /** The previous range's value, null where there is none. */
  was: number | null;
  /** Which way is good, for the change's ink. */
  better: "up" | "down" | null;
  /** The window it covers where that is not the page's range ("last week"). */
  window?: string;
}

/** One chart: x values across (or down, for bars) and series aligned to them. */
export interface ViewChart {
  id: string;
  title: string;
  unit: StatUnit;
  kind: "columns" | "stacked_columns" | "bars";
  x: { kind: "day" | "seat" | "entry" | "reason" | "state"; labels: string[] };
  /** For an x of days, each label's instant (epoch ms), so a line can be drawn over time. */
  at?: number[];
  series: { id: string; label: string; values: (number | null)[] }[];
  note: string | null;
}

/** The company's numbers over one range. */
export interface ViewSnapshot {
  days: RangeDays;
  stats: ViewStat[];
  charts: ViewChart[];
}

// ---------------------------------------------------------------------------
// The numbers, from today's questions.
// ---------------------------------------------------------------------------

/** The range's started turns, summed. */
export interface Turns {
  turns: number;
  tokens: number;
  nothing: number;
}

export function turnsOf(rows: readonly TurnOutcomeRow[] | null | undefined): Turns | null {
  if (!rows) return null;
  const out: Turns = { turns: 0, tokens: 0, nothing: 0 };
  for (const r of rows) {
    out.turns += r.turns;
    out.tokens += r.tokens;
    out.nothing += r.nothing;
  }
  return out;
}

const share = (t: Turns | null) =>
  t && t.turns > 0 ? Math.round((t.nothing / t.turns) * 100) : null;
const perTurn = (t: Turns | null) => (t && t.turns > 0 ? Math.round(t.tokens / t.turns) : null);

/** A weekly report's metric, or null where the report does not carry it. */
function metric(
  r: AdvisorReport | null,
  key: string,
): { value: number; was: number | null } | null {
  const m = r?.metrics.find((x) => x.key === key);
  return m ? { value: m.value, was: m.was } : null;
}

/** The hours work stood waiting, held or blocked over the newest reported week. */
export function stuckHours(r: AdvisorReport | null): { value: number; was: number | null } | null {
  const parts = ["wait_h", "held_h", "blocked_h"].map((k) => metric(r, k));
  if (parts.every((p) => p === null)) return null;
  const value = parts.reduce((n, p) => n + (p?.value ?? 0), 0);
  const was = parts.some((p) => p?.was == null)
    ? null
    : parts.reduce((n, p) => n + (p?.was ?? 0), 0);
  return {
    value: Math.round(value * 10) / 10,
    was: was === null ? null : Math.round(was * 10) / 10,
  };
}

/**
 * The hours seats sat out a model's limit, as the capacity finding measured
 * them, or null where none is open: "not measured", never "none lost".
 */
export function capacityHours(findings: readonly AdvisorKeptFinding[]): number | null {
  const held = findings.filter((f) => f.state === "open" && f.class === "capacity.model_held");
  if (held.length === 0) return null;
  const hours = held.flatMap((f) => f.evidence.facts).filter((x) => x.unit === "hours");
  return Math.round(hours.reduce((n, x) => n + x.value, 0) * 10) / 10;
}

/** The answers the numbers are built from. */
export interface SnapshotAnswers {
  flow: WorkFlowAnswer | null;
  workload: WorkloadAnswer | null;
  outcomes: readonly TurnOutcomeRow[] | null;
  previous: readonly TurnOutcomeRow[] | null;
  report: AdvisorReport | null;
  findings: readonly AdvisorKeptFinding[];
}

/** The strip's figures. */
export function statsOf(a: SnapshotAnswers): ViewStat[] {
  const turns = turnsOf(a.outcomes);
  const before = turnsOf(a.previous);
  const open = a.flow ? a.flow.now.not_started + a.flow.now.active : null;
  const first = a.flow?.points[0];
  const stuck = stuckHours(a.report);
  return [
    {
      key: "open_items",
      label: "Open work",
      value: open,
      unit: "items",
      was: first ? first.not_started + first.active : null,
      better: "down",
    },
    {
      key: "stuck_hours",
      label: "Waiting, held or blocked",
      value: stuck?.value ?? null,
      unit: "hours",
      was: stuck?.was ?? null,
      better: "down",
      window: "last week",
    },
    {
      key: "capacity_lost_hours",
      label: "Lost to model limits",
      value: capacityHours(a.findings),
      unit: "hours",
      was: null,
      better: "down",
    },
    {
      key: "wasted_turns_percent",
      label: "Wasted wakes",
      value: share(turns),
      unit: "percent",
      was: share(before),
      better: "down",
    },
    {
      key: "tokens_per_turn",
      label: "Cost per turn",
      value: perTurn(turns),
      unit: "tokens",
      was: perTurn(before),
      better: "down",
    },
  ];
}

/** Open work and work completed, per day of the range. */
export function openWorkChart(flow: WorkFlowAnswer | null): ViewChart | null {
  if (!flow) return null;
  return {
    id: "open_work_days",
    title: "Open work",
    unit: "items",
    kind: "columns",
    x: { kind: "day", labels: flow.points.map((p) => p.window) },
    at: flow.points.map((p) => Date.parse(p.start)),
    series: [
      { id: "open", label: "Open", values: flow.points.map((p) => p.not_started + p.active) },
      { id: "completed", label: "Completed", values: flow.points.map((p) => p.completed) },
    ],
    note: null,
  };
}

/** The flow states an item in progress stands in, as a stacked chart's series. */
const FLOW_STATES = [
  { id: "working", label: "Working" },
  { id: "waiting", label: "Waiting" },
  { id: "held", label: "Held" },
  { id: "blocked", label: "Blocked" },
  { id: "stuck", label: "Stuck" },
] as const;

/** Each seat's items in progress by the flow state that holds them, the eight most loaded. */
export function flowBySeatChart(
  workload: WorkloadAnswer | null,
  nameOf: (handle: string) => string,
): ViewChart | null {
  if (!workload) return null;
  const rows = workload.rows
    .filter((r) => FLOW_STATES.some((s) => r.flow[s.id] > 0))
    .sort((a, b) => total(b) - total(a))
    .slice(0, 8);
  return {
    id: "flow_by_seat",
    title: "Work in progress, by agent",
    unit: "items",
    kind: "stacked_columns",
    x: { kind: "seat", labels: rows.map((r) => nameOf(r.handle)) },
    series: FLOW_STATES.map((s) => ({
      id: s.id,
      label: s.label,
      values: rows.map((r) => r.flow[s.id]),
    })),
    note: null,
  };
}

const total = (r: WorkloadAnswer["rows"][number]) =>
  FLOW_STATES.reduce((n, s) => n + r.flow[s.id], 0);

/**
 * The range's turns by what woke them, across every agent: how many changed
 * nothing beside how many did. BY THE WHOLE REASON (`work_item:lead_fallback`,
 * not `work_item`): the advice differs per reason, and a kind sums a reason
 * that wastes most of its turns into one that wastes none.
 */
export function wastedWakesChart(rows: readonly TurnOutcomeRow[] | null): ViewChart | null {
  if (!rows) return null;
  const by = new Map<string, { turns: number; nothing: number; nothingTokens: number }>();
  for (const r of rows) {
    const w = by.get(r.wake) ?? { turns: 0, nothing: 0, nothingTokens: 0 };
    w.turns += r.turns;
    w.nothing += r.nothing;
    w.nothingTokens += r.nothing_tokens;
    by.set(r.wake, w);
  }
  const top = [...by.entries()]
    .filter(([, w]) => w.nothing > 0)
    .sort(([, a], [, b]) => b.nothingTokens - a.nothingTokens || b.nothing - a.nothing)
    .slice(0, 6);
  return {
    id: "wasted_wakes",
    title: "Turns that changed nothing, by what woke them",
    unit: "turns",
    kind: "bars",
    x: { kind: "reason", labels: top.map(([wake]) => wakeWords(wake, "").label) },
    series: [
      { id: "nothing", label: "Changed nothing", values: top.map(([, w]) => w.nothing) },
      { id: "moved", label: "Changed something", values: top.map(([, w]) => w.turns - w.nothing) },
    ],
    note: null,
  };
}

/** Tokens a turn per agent, the costliest first, beside the company's median agent. */
export function tokensPerTurnChart(
  rows: readonly TurnOutcomeRow[] | null,
  nameOf: (handle: string) => string,
): ViewChart | null {
  if (!rows) return null;
  const by = new Map<string, { turns: number; tokens: number }>();
  for (const r of rows) {
    const s = by.get(r.seat) ?? { turns: 0, tokens: 0 };
    s.turns += r.turns;
    s.tokens += r.tokens;
    by.set(r.seat, s);
  }
  const per = [...by.entries()]
    .filter(([, s]) => s.turns > 0 && s.tokens > 0)
    .map(([seat, s]) => ({ seat, value: Math.round(s.tokens / s.turns) }))
    .sort((a, b) => b.value - a.value);
  const sorted = per.map((p) => p.value).sort((a, b) => a - b);
  const median = sorted.length ? sorted[Math.floor((sorted.length - 1) / 2)]! : null;
  const top = per.slice(0, 8);
  return {
    id: "tokens_per_turn",
    title: "Cost per turn",
    unit: "tokens",
    kind: "bars",
    x: { kind: "seat", labels: top.map((p) => nameOf(p.seat)) },
    series: [{ id: "tokens", label: "Tokens a turn", values: top.map((p) => p.value) }],
    note: median === null ? null : `The median agent spends ${fmtCount(median)} tokens a turn.`,
  };
}

/** The company's numbers over `days`, from today's answers. */
export function snapshotOf(
  a: SnapshotAnswers,
  days: RangeDays,
  nameOf: (handle: string) => string,
): ViewSnapshot {
  return {
    days,
    stats: statsOf(a),
    charts: [
      openWorkChart(a.flow),
      flowBySeatChart(a.workload, nameOf),
      wastedWakesChart(a.outcomes),
      tokensPerTurnChart(a.outcomes, nameOf),
    ].filter((c): c is ViewChart => c !== null),
  };
}

// ---------------------------------------------------------------------------
// Which finding explains a figure.
// ---------------------------------------------------------------------------

/**
 * The finding classes that explain each figure: the first open one, the most
 * costly first, is the figure's link. A figure with no open explainer has no
 * link, which is a gap in words rather than a wrong link.
 */
export const EXPLAINED_BY: Readonly<Record<string, readonly string[]>> = {
  open_items: ["work.concentrated", "work.bottleneck", "work.held_by_person", "capacity.add_seat"],
  stuck_hours: [
    "work.pickup_still_waiting",
    "work.bottleneck",
    "work.held_by_person",
    "review.idle",
  ],
  capacity_lost_hours: ["capacity.model_held", "models.credential_refused", "budget.stopped"],
  wasted_turns_percent: [
    "wake.noop_share",
    "wake.alarm_noop",
    "wake.redelivered_seen",
    "wake.agent_mentions_without_ask",
    "schedule.unproductive",
  ],
  tokens_per_turn: [
    "run.round_cap",
    "spend.auxiliary_share",
    "learning.auxiliary_model",
    "budget.stopped",
  ],
};

/** The open finding that explains a figure, or null. */
export function explainer(
  key: string,
  findings: readonly AdvisorKeptFinding[],
): AdvisorKeptFinding | null {
  const classes = EXPLAINED_BY[key] ?? [];
  return ranked(findings.filter((f) => f.state === "open" && classes.includes(f.class)))[0] ?? null;
}

// ---------------------------------------------------------------------------
// A finding as its row draws it.
// ---------------------------------------------------------------------------

/** What a finding's press does. */
export type ViewAction =
  /** The fix's option, re-checked and sent in place ([ApplyPanel]). */
  | { kind: "apply"; label: string; option: number }
  /** The place to act, in the dashboard or, `external`, outside it. */
  | { kind: "open"; label: string; href: string; external?: boolean }
  /** Ask Crewlet, with a question waiting in its box. */
  | { kind: "ask"; label: string; question: string }
  /** An atlas fix, sent with the atlas's own tools on the entries it names. */
  | { kind: "review"; label: string };

/** One finding, as its row draws it. */
export interface ViewFinding {
  finding: AdvisorKeptFinding;
  /** One line, the engine's words. */
  headline: string;
  /** One line of why. */
  why: string;
  /** The numbers behind it. */
  stats: ViewStat[];
  actions: { primary: ViewAction | null; secondary: ViewAction[] };
}

/** Where an `open_page` fix is taken, by the page the engine names and what it opens on. */
export const PAGE_FIXES: Readonly<
  Record<
    AdvisorPage,
    (target: string) => { path: string[]; query?: Record<string, string>; action: string } | null
  >
> = {
  backups: () => ({
    path: ["settings", "backups"],
    query: { take: TAKE_BACKUP },
    action: "Take a backup",
  }),
  item: (key) => (key ? { path: ["worklet", key], action: `Open ${key}` } : null),
  seat: (handle) =>
    handle ? { path: ["company", "org", "seats", handle], action: "Open the agent" } : null,
};

/**
 * What a button says for a fix the engine wrote as a sentence: its first
 * clause, the act itself ("Run Agent SWE's learning on a smaller model",
 * "Take a backup now", "Add a seat like Backend"), without what follows a
 * colon or a comma ("..., then schedule backups", ": the same job, model and
 * limits"), or an "and" past a button's length.
 */
export function actWords(sentence: string): string {
  let s = sentence.trim().replace(/\.$/, "");
  const cut = s.search(/[,:;]/);
  if (cut > 0) s = s.slice(0, cut);
  if (s.length > 44) {
    const and = s.search(/ (and|or) /);
    if (and > 0) s = s.slice(0, and);
  }
  return s;
}

/** What an `open_setting` fix's button says, by the setting it opens. */
const SETTING_ACTS: Readonly<Record<string, string>> = {
  "providers.llm": "Add a model entry",
};

/** The fix's presses: the first is the row's one button, the rest are in its menu. */
export function actionsOf(f: AdvisorKeptFinding): ViewFinding["actions"] {
  const fix = f.fix;
  const none = { primary: null, secondary: [] };
  if (f.state !== "open") return none;
  switch (fix.kind) {
    case "config_change": {
      const [first, ...rest] = fix.options.map((o, i): ViewAction => ({
        kind: "apply",
        label: o.label,
        option: i,
      }));
      return first ? { primary: first, secondary: rest } : none;
    }
    case "api_change":
      return {
        primary: { kind: "apply", label: actWords(fix.sentence), option: 0 },
        secondary: [],
      };
    case "open_page": {
      const page =
        fix.page && fix.page in PAGE_FIXES
          ? PAGE_FIXES[fix.page as AdvisorPage](fix.target ?? "")
          : null;
      return page
        ? {
            primary: { kind: "open", label: page.action, href: href(page.path, page.query) },
            secondary: [],
          }
        : none;
    }
    case "open_setting":
      return {
        primary: {
          kind: "open",
          label: SETTING_ACTS[fix.setting ?? ""] ?? "Open the setting",
          href: href(["settings", "config"]),
        },
        secondary: [],
      };
    case "external_step":
      return fix.url
        ? {
            primary: {
              kind: "open",
              label: f.subject.name ? `Open ${f.subject.name}` : "Open it",
              href: fix.url,
              external: true,
            },
            secondary: [],
          }
        : none;
    case "ask_crewlet":
      return {
        primary: {
          kind: "ask",
          label: "Ask Crewlet",
          question: fix.prompt || actWords(fix.sentence),
        },
        secondary: [],
      };
    case "atlas_act":
      return {
        primary:
          fix.act && f.entries !== null
            ? { kind: "review", label: "Review the fix" }
            : { kind: "open", label: "Open the atlas", href: href(["company", "atlas"]) },
        secondary: [],
      };
  }
  return none;
}

/** A fact's unit as a stat's: the closest the stat's set has. */
const STAT_UNITS: Readonly<Record<string, StatUnit>> = {
  hours: "hours",
  minutes: "minutes",
  items: "items",
  turns: "turns",
  tokens: "tokens",
  percent: "percent",
};

/** The first sentence of the engine's why: the row's one line. */
export function firstSentence(why: string): string {
  const at = why.search(/[.!?](\s|$)/);
  return at < 0 ? why : why.slice(0, at + 1);
}

/** A finding as its row draws it, read off its facts and its fix. */
export function findingView(f: AdvisorKeptFinding): ViewFinding {
  const stats: ViewStat[] = [];
  if (f.impact?.hours_per_week != null) {
    stats.push({
      key: "hours_per_week",
      label: "a week",
      value: f.impact.hours_per_week,
      unit: "hours",
      was: null,
      better: "down",
    });
  }
  if (f.impact?.tokens_per_week != null) {
    stats.push({
      key: "tokens_per_week",
      label: "a week",
      value: f.impact.tokens_per_week,
      unit: "tokens",
      was: null,
      better: "down",
    });
  }
  for (const fact of f.evidence.facts) {
    stats.push({
      key: fact.key,
      label: fact.label,
      value: fact.value,
      unit: STAT_UNITS[fact.unit] ?? "count",
      was: null,
      better: null,
    });
  }
  return {
    finding: f,
    headline: f.title,
    why: firstSentence(f.why),
    stats: stats.slice(0, 3),
    actions: actionsOf(f),
  };
}
