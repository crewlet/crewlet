/**
 * What the Advisor's page draws, read off the engine's answers: the
 * company's numbers over a range (`advisor_snapshot`, ADR-0166), each finding
 * as a row, and which finding explains which number.
 *
 * THE ENGINE STATES THE NUMBERS; THE PAGE CHOOSES THE STRIP. The snapshot's
 * stats are each a number with its unit, the previous range's value and the
 * direction that is good, and its charts are a kind over labelled points.
 * The strip shows four of them a row (the kit's widest row, which folds two by
 * two on a phone): open work; the hours work waited, was held or stood
 * blocked, as one figure with its parts beside it; the hours lost to model
 * limits; and the share of turns that changed nothing. A stat whose key is a
 * chart's id heads that chart instead (`tokens_per_turn`).
 *
 * A FIGURE NOT MEASURED IS SAID SO, never drawn as a zero: a stat the snapshot
 * does not carry is null.
 */

import { href } from "~/app/router.tsx";
import { TAKE_BACKUP } from "~/lib/backups.ts";
import { fmtCount } from "~/lib/format.ts";
import type {
  AdvisorAction,
  AdvisorChart,
  AdvisorKeptFinding,
  AdvisorSnapshot,
  AdvisorStat,
  AdvisorStatUnit,
} from "~/protocol/advisorFindings.ts";
import type { AdvisorPage } from "~/protocol/advisor.ts";
import { ranked } from "./words.ts";

/** What a stat's number counts. */
export type StatUnit = AdvisorStatUnit;

/** The windows the page reads. */
export const RANGES = [7, 14, 30] as const;
export type RangeDays = (typeof RANGES)[number];

/** One figure of the strip. `value` null is a figure not measured. */
export interface ViewStat extends Omit<AdvisorStat, "value"> {
  value: number | null;
  /** Its parts or context in a few words, said in place of the range it compares with. */
  detail?: string;
}

/** One chart, and for a chart over days each point's instant, so it is drawn over time. */
export interface ViewChart extends AdvisorChart {
  at?: number[];
}

/** The company's numbers over one range, as the page lays them out. */
export interface ViewSnapshot {
  days: RangeDays;
  /** The strip's figures, in order. */
  stats: ViewStat[];
  /** Figures that head a chart of their own id. */
  charted: ViewStat[];
  charts: ViewChart[];
  unread: string[];
}

/** The parts the strip's flow figure sums. */
const FLOW_PARTS = [
  { key: "wait_hours", word: "waiting" },
  { key: "held_hours", word: "held" },
  { key: "blocked_hours", word: "blocked" },
] as const;

const round = (n: number) => Math.round(n * 10) / 10;

/**
 * A number as the page says it: whole from ten up, else to a tenth, so a
 * figure and the parts beside it agree ("134 h" over "30 h waiting · 89 h
 * held · 15 h blocked") and three parts fit one line.
 */
export function amount(n: number): number {
  return Math.abs(n) >= 10 ? Math.round(n) : round(n);
}

/** A number in its unit, in a few characters: "134 h", "12 min", "62%". */
export function amountWords(n: number, unit: StatUnit): string {
  const said = fmtCount(amount(n));
  if (unit === "hours") return `${said} h`;
  if (unit === "minutes") return `${said} min`;
  if (unit === "percent") return `${said}%`;
  return said;
}

const said = (s: AdvisorStat) => amountWords(s.value, s.unit);

/** The strip's figures, from the snapshot's stats. */
export function stripOf(snapshot: AdvisorSnapshot | null): ViewStat[] {
  const by = new Map((snapshot?.stats ?? []).map((s) => [s.key, s]));
  const missing = (key: string, label: string, unit: StatUnit): ViewStat => ({
    key,
    label,
    value: null,
    unit,
    was: null,
    better: "down",
  });
  const open = by.get("open_items");
  const parts = FLOW_PARTS.flatMap((p) => {
    const stat = by.get(p.key);
    return stat ? [{ word: p.word, stat }] : [];
  });
  const pickup = by.get("pickup_wait_minutes");
  const flow: ViewStat =
    parts.length === 0
      ? missing("flow_hours", "Waiting, held or blocked", "hours")
      : {
          key: "flow_hours",
          label: "Waiting, held or blocked",
          value: round(parts.reduce((n, p) => n + p.stat.value, 0)),
          unit: "hours",
          was: parts.every((p) => p.stat.was !== null)
            ? round(parts.reduce((n, p) => n + (p.stat.was ?? 0), 0))
            : null,
          better: "down",
          detail: [
            ...parts.map((p) => `${said(p.stat)} ${p.word}`),
            ...(pickup ? [`pickup ${said(pickup)}`] : []),
          ].join(" · "),
        };
  const capacity = by.get("capacity_lost_hours");
  const wasted = by.get("wasted_turns_percent");
  const turns = by.get("turns");
  return [
    open ? { ...open, label: "Open work" } : missing("open_items", "Open work", "items"),
    flow,
    capacity
      ? { ...capacity, label: "Lost to model limits" }
      : missing("capacity_lost_hours", "Lost to model limits", "hours"),
    wasted
      ? {
          ...wasted,
          label: "Wasted wakes",
          ...(turns ? { detail: `of ${fmtCount(turns.value)} turns` } : {}),
        }
      : missing("wasted_turns_percent", "Wasted wakes", "percent"),
  ];
}

/**
 * How a chart is drawn. NAMES GO DOWN A LIST, DAYS GO ACROSS: a chart over
 * seats, entries or reasons is a list of named bars, each row its full name
 * (split by its series where there are several), because a column chart
 * names only its first, middle and last columns, as a time axis does, and
 * long names under columns collide. Over days, one series of columns is a
 * line and several are stacked columns.
 */
export function chartShape(c: AdvisorChart): "line" | "columns" | "bars" {
  if (c.kind === "bars" || c.x.kind !== "day") return "bars";
  return c.kind === "columns" ? "line" : "columns";
}

/** A chart as the page draws it: over days, each point's instant. */
export function chartView(c: AdvisorChart): ViewChart {
  return c.x.kind === "day" ? { ...c, at: c.x.keys.map((k) => Date.parse(k)) } : c;
}

/**
 * How many charts a row holds: as few rows of at most four as the charts fit,
 * shared as evenly as they go (five are three and two), so no chart stands
 * alone beside an empty row.
 */
export function chartColumns(count: number): number {
  const rows = Math.max(1, Math.ceil(count / 4));
  return Math.max(1, Math.ceil(count / rows));
}

/** The company's numbers over `days`, from the snapshot. */
export function snapshotView(snapshot: AdvisorSnapshot | null, days: RangeDays): ViewSnapshot {
  const charts = (snapshot?.charts ?? []).map(chartView);
  const ids = new Set(charts.map((c) => c.id));
  return {
    days,
    stats: stripOf(snapshot),
    charted: (snapshot?.stats ?? []).filter((s) => ids.has(s.key)),
    charts,
    unread: snapshot?.unread ?? [],
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
  flow_hours: [
    "work.pickup_still_waiting",
    "work.bottleneck",
    "work.held_by_person",
    "review.idle",
  ],
  capacity_lost_hours: [
    "capacity.model_limit",
    "capacity.model_held",
    "models.credential_refused",
    "budget.stopped",
  ],
  wasted_turns_percent: [
    "wake.reason_waste",
    "wake.noop_share",
    "wake.alarm_noop",
    "wake.redelivered_seen",
    "wake.agent_mentions_without_ask",
    "schedule.unproductive",
  ],
  tokens_per_turn: [
    "cost.turn_outlier",
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

/** What a finding's press does, as the page routes it. */
export type ViewAction =
  /** The fix's option, re-checked and sent in place ([ApplyPanel]). */
  | { kind: "apply"; label: string; option: number }
  /** The place to act, in the dashboard or, `external`, outside it. */
  | { kind: "open"; label: string; href: string; external?: boolean }
  /** Ask Crewlet, with a question waiting in its box. */
  | { kind: "ask"; label: string; question: string }
  /** An atlas fix, sent with the atlas's own tools on the entries it names. */
  | { kind: "review"; label: string }
  /**
   * Work to move to another seat: re-checked and moved in place, each item
   * through the work item's own Reassign, the suggested seat prefilled.
   */
  | { kind: "reassign"; label: string; items: string[]; to: string };

/** One finding, as its row draws it. */
export interface ViewFinding {
  finding: AdvisorKeptFinding;
  /** One line, carrying the number. */
  headline: string;
  /** One line of why. */
  why: string;
  /** The numbers behind it, the row's three. */
  stats: AdvisorStat[];
  actions: { primary: ViewAction | null; secondary: ViewAction[] };
}

/** Where an `open` press is taken, by the page the engine names and what it opens on. */
export const PAGE_FIXES: Readonly<
  Record<AdvisorPage, (target: string) => { path: string[]; query?: Record<string, string> } | null>
> = {
  backups: () => ({ path: ["settings", "backups"], query: { take: TAKE_BACKUP } }),
  item: (key) => (key ? { path: ["worklet", key] } : null),
  seat: (handle) => (handle ? { path: ["company", "org", "seats", handle] } : null),
};

/**
 * The press that opens an Apply on the work a finding moves rather than on
 * one of its fix's options: the re-check answers the items to move, and the
 * panel moves them (`apply.ts`'s moveAll).
 */
export const REASSIGN = -1;

/**
 * A press as the page routes it, or null for one this page does not route: a
 * reassignment naming no item or no seat, and an `open` naming a page this
 * build does not know.
 */
export function actionView(a: AdvisorAction, f: AdvisorKeptFinding): ViewAction | null {
  switch (a.kind) {
    case "apply":
      return a.option === null ? null : { kind: "apply", label: a.label, option: a.option };
    case "open": {
      if (a.page) {
        const page =
          a.page in PAGE_FIXES ? PAGE_FIXES[a.page as AdvisorPage](a.target ?? "") : null;
        return page ? { kind: "open", label: a.label, href: href(page.path, page.query) } : null;
      }
      return a.setting
        ? { kind: "open", label: a.label, href: href(["settings", "config"]) }
        : null;
    }
    case "step":
      return a.url ? { kind: "open", label: a.label, href: a.url, external: true } : null;
    case "ask":
      return a.question ? { kind: "ask", label: a.label, question: a.question } : null;
    case "fix":
      // AN ATLAS FIX IS SENT WITH THE ATLAS'S OWN TOOLS on the entries the
      // finding names; a reader the engine names none (no atlas app) is
      // sent to the atlas instead.
      return f.fix.kind === "atlas_act" && f.fix.act && f.entries !== null
        ? { kind: "review", label: a.label }
        : { kind: "open", label: "Open the atlas", href: href(["company", "atlas"]) };
    case "reassign":
      return a.items && a.items.length > 0 && a.to
        ? { kind: "reassign", label: a.label, items: a.items, to: a.to }
        : null;
  }
  return null;
}

const escaped = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/**
 * A row's words with its seat said once. The row's prefix names its subject,
 * so a label, the why or the headline that OPENS on the subject (by its name
 * or its seat's handle, "app-engineer tokens on 17 Nov") drops it and starts
 * on what follows ("Tokens on 17 Nov"), and a seat's handle anywhere else is
 * said by its name, never as a key. A row that names no subject keeps its
 * words.
 */
export function withoutSubject(text: string, f: AdvisorKeptFinding): string {
  const name = f.subject.name;
  if (!name) return text;
  const who = [name, f.subject.seat].filter(Boolean).map(escaped).join("|");
  const opening = new RegExp(`^(?:${who})(?:'s|’s)?(?![\\w-])[\\s:,]*`, "i");
  const rest = text.replace(opening, "");
  const said = rest === text ? text : rest.charAt(0).toUpperCase() + rest.slice(1);
  const handle = f.subject.seat;
  return handle && handle !== name
    ? said.replace(new RegExp(`(?<![\\w-])${escaped(handle)}(?![\\w-])`, "g"), name)
    : said;
}

/** The first sentence of the engine's why: the row's one line. */
export function firstSentence(why: string): string {
  const at = why.search(/[.!?](\s|$)/);
  return at < 0 ? why : why.slice(0, at + 1);
}

/** A finding as its row draws it. */
export function findingView(f: AdvisorKeptFinding): ViewFinding {
  const routed = (a: AdvisorAction | null) => (a ? actionView(a, f) : null);
  const open = f.state === "open";
  const secondary = f.actions.secondary.map(routed).filter((a): a is ViewAction => a !== null);
  // ONE PRESS A ROW, one this page can take: a primary it does not route
  // gives its place to the first secondary it does.
  const primary = routed(f.actions.primary);
  return {
    finding: f,
    headline: withoutSubject(f.headline, f),
    why: withoutSubject(firstSentence(f.why), f),
    stats: f.stats.slice(0, 3).map((s) => ({ ...s, label: withoutSubject(s.label, f) })),
    actions: !open
      ? { primary: null, secondary: [] }
      : primary
        ? { primary, secondary }
        : { primary: secondary[0] ?? null, secondary: secondary.slice(1) },
  };
}
