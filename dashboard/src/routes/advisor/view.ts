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

/** A chart as the page draws it: over days, each point's instant. */
export function chartView(c: AdvisorChart): ViewChart {
  return c.x.kind === "day" ? { ...c, at: c.x.keys.map((k) => Date.parse(k)) } : c;
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
  | { kind: "review"; label: string };

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
 * A press as the page routes it, or null for one this page does not route: a
 * reassignment (sent through the worklet route once the engine offers one),
 * and an `open` naming a page this build does not know.
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
  }
  return null;
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
  return {
    finding: f,
    headline: f.headline,
    why: firstSentence(f.why),
    stats: f.stats.slice(0, 3),
    actions: {
      primary: open ? routed(f.actions.primary) : null,
      secondary: open
        ? f.actions.secondary.map(routed).filter((a): a is ViewAction => a !== null)
        : [],
    },
  };
}
