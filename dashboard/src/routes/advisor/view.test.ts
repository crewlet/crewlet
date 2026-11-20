/**
 * What the Advisor's page draws, read off the engine's answers: the strip the
 * page chooses from the snapshot's stats, the charts over days drawn over
 * time, each press routed by its kind, and the finding that explains a figure.
 */

import { expect, test } from "vitest";
import type {
  AdvisorAction,
  AdvisorKeptFinding,
  AdvisorSnapshot,
  AdvisorStat,
} from "~/protocol/advisorFindings.ts";
import { deltaOf, figure } from "./Numbers.tsx";
import {
  actionView,
  chartColumns,
  explainer,
  findingView,
  firstSentence,
  snapshotView,
  stripOf,
} from "./view.ts";

const stat = (
  key: string,
  value: number,
  unit: AdvisorStat["unit"],
  was: number | null = null,
): AdvisorStat => ({
  key,
  label: key,
  value,
  unit,
  was,
  better: "down",
});

const snapshot = (
  stats: AdvisorStat[],
  charts: AdvisorSnapshot["charts"] = [],
): AdvisorSnapshot => ({
  as_of: "2026-11-20T06:00:00Z",
  run_id: null,
  range: { from: "2026-11-14T00:00:00Z", to: "2026-11-20T06:00:00Z", days: 7 },
  stats,
  charts,
  unread: [],
});

function finding(patch: Partial<AdvisorKeptFinding> = {}): AdvisorKeptFinding {
  return {
    id: "f-1",
    key: "k",
    class: "wake.noop_share",
    source: "rule",
    actor: "owner",
    area: "wakes",
    severity: "costs",
    subject: { kind: "seat", id: "pm", name: "Agent PM", seat: "pm", unit: "", schedule: "" },
    title: "An agent was woken for turns that changed nothing.",
    why: "Most of its turns moved no work. Each one costs tokens.",
    item: "",
    names: [],
    path: "",
    entries: null,
    explanation: null,
    state: "open",
    close_reason: null,
    ineffective: null,
    snoozed_until: null,
    opened_at: "2026-11-19T08:00:00Z",
    last_confirmed_at: "2026-11-20T04:00:00Z",
    closed_at: null,
    effect_due_at: null,
    confirmed: "fresh",
    revision: 1,
    seen_before: 0,
    impact: null,
    evidence: { window: { from: "", to: "" }, facts: [], records: [] },
    fix: { kind: "none", sentence: "Nothing to send.", options: [] },
    headline: "Agent PM wasted 124 of 185 turns.",
    stats: [],
    chart: null,
    seats: [],
    actions: { primary: null, secondary: [] },
    premise_checked_at: null,
    ...patch,
  };
}

const action = (patch: Partial<AdvisorAction>): AdvisorAction => ({
  kind: "open",
  label: "Open",
  option: null,
  items: null,
  to: null,
  page: null,
  target: null,
  setting: null,
  url: null,
  question: null,
  ...patch,
});

// THE ENGINE STATES THE NUMBERS; THE PAGE CHOOSES THE STRIP: four a row, the
// hours work stood still as one figure with its parts, and a figure the
// snapshot does not carry said as not measured, never as a zero.
test("the strip is four figures from the snapshot, flow hours summed with their parts", () => {
  const strip = stripOf(
    snapshot([
      stat("open_items", 40, "items", 30),
      stat("wait_hours", 29.2, "hours", 10),
      stat("held_hours", 4.25, "hours", 2),
      stat("blocked_hours", 2, "hours", 1),
      stat("pickup_wait_minutes", 12, "minutes"),
      stat("turns", 440, "turns"),
      stat("wasted_turns_percent", 62, "percent", 50),
      stat("tokens_per_turn", 1150, "tokens", 500),
    ]),
  );
  expect(strip.map((s) => [s.key, s.value, s.was])).toEqual([
    ["open_items", 40, 30],
    ["flow_hours", 35.5, 13],
    ["capacity_lost_hours", null, null],
    ["wasted_turns_percent", 62, 50],
  ]);
  expect(strip[1]!.detail).toBe("29 h waiting · 4.3 h held · 2 h blocked · pickup 12 min");
  expect(strip[3]!.detail).toBe("of 440 turns");
  expect(stripOf(null).map((s) => s.value)).toEqual([null, null, null, null]);
});

test("a stat whose key is a chart's id heads that chart, and a chart over days is drawn over time", () => {
  const view = snapshotView(
    snapshot(
      [stat("open_items", 40, "items"), stat("tokens_per_turn", 1150, "tokens")],
      [
        {
          id: "tokens_per_turn",
          title: "Tokens a turn",
          unit: "tokens",
          kind: "columns",
          x: { kind: "seat", keys: ["pm"], labels: ["Agent PM"] },
          series: [{ id: "tokens", label: "Tokens", values: [1150] }],
          note: null,
        },
        {
          id: "open_work_days",
          title: "Open work",
          unit: "items",
          kind: "columns",
          x: { kind: "day", keys: ["2026-11-19", "2026-11-20"], labels: ["Nov 19", "Nov 20"] },
          series: [{ id: "open", label: "Open", values: [30, 40] }],
          note: null,
        },
      ],
    ),
    7,
  );
  expect(view.charted.map((s) => s.key)).toEqual(["tokens_per_turn"]);
  expect(view.charts[0]!.at).toBeUndefined();
  expect(view.charts[1]!.at).toEqual([Date.parse("2026-11-19"), Date.parse("2026-11-20")]);
});

// NO CHART STANDS ALONE ON A ROW: the charts take as few rows of at most four
// as they fit, and share them as evenly as they go.
test("the charts are laid out in rows of at most four, as even as they go", () => {
  expect([1, 2, 3, 4, 5, 6, 7, 8, 9].map(chartColumns)).toEqual([1, 2, 3, 4, 3, 3, 4, 4, 3]);
});

test("a change is said in the ink of whether it is the one wanted, a share in points", () => {
  const s = { key: "k", label: "l", unit: "percent", better: "down" as const };
  expect(deltaOf({ ...s, value: 62, was: 50 })).toEqual({ value: "+12 pts", polarity: "bad" });
  expect(deltaOf({ ...s, value: 40, was: 50 })).toEqual({ value: "−10 pts", polarity: "good" });
  expect(deltaOf({ ...s, unit: "items", value: 40, was: 30 })).toEqual({
    value: "+33%",
    polarity: "bad",
  });
  expect(deltaOf({ ...s, value: 40, was: null })).toBeUndefined();
  expect(figure(1150, "tokens")).toEqual({ value: "1,150", unit: "tokens" });
  expect(figure(1, "items")).toEqual({ value: "1", unit: "item" });
});

// A FIGURE AND ITS PARTS AGREE: the tile says its hours as its detail line
// says their parts (whole from ten, else to a tenth), and a change from
// nothing says its unit.
test("a figure is whole from ten, as its parts are, and a change from nothing says its unit", () => {
  const s = { key: "k", label: "l", unit: "hours" as const, better: "down" as const };
  expect(figure(134.2, "hours")).toEqual({ value: "134", unit: "h" });
  expect(figure(4.25, "hours")).toEqual({ value: "4.3", unit: "h" });
  expect(figure(62.4, "percent")).toEqual({ value: "62%" });
  expect(deltaOf({ ...s, value: 134.2, was: 0 })).toEqual({ value: "+134 h", polarity: "bad" });
  expect(deltaOf({ ...s, unit: "minutes", value: 12, was: 0 })).toEqual({
    value: "+12 min",
    polarity: "bad",
  });
  expect(deltaOf({ ...s, unit: "percent", value: 62.44, was: 50 })).toEqual({
    value: "+12 pts",
    polarity: "bad",
  });
});

// ROUTING STAYS IN THE DASHBOARD: the engine names a page or a setting,
// never a link, and the page builds the address.
test("each press is routed by its kind, and one this page does not route is left out", () => {
  const f = finding();
  expect(actionView(action({ kind: "apply", label: "Learn on Haiku", option: 1 }), f)).toEqual({
    kind: "apply",
    label: "Learn on Haiku",
    option: 1,
  });
  expect(actionView(action({ label: "Open ENG-12", page: "item", target: "ENG-12" }), f)).toEqual({
    kind: "open",
    label: "Open ENG-12",
    href: "#/worklet/ENG-12",
  });
  expect(
    actionView(action({ label: "Add a model entry", setting: "providers.llm" }), f),
  ).toMatchObject({
    kind: "open",
    label: "Add a model entry",
  });
  expect(
    actionView(action({ kind: "step", label: "Open GitHub", url: "https://github.com/x" }), f),
  ).toEqual({
    kind: "open",
    label: "Open GitHub",
    href: "https://github.com/x",
    external: true,
  });
  expect(actionView(action({ kind: "ask", label: "Ask Crewlet", question: "Why?" }), f)).toEqual({
    kind: "ask",
    label: "Ask Crewlet",
    question: "Why?",
  });
  expect(actionView(action({ kind: "fix", label: "Review the fix" }), f)).toMatchObject({
    kind: "open",
    label: "Open the atlas",
  });
  expect(
    actionView(action({ kind: "reassign", label: "Move 6 items", items: ["A-1"], to: "x" }), f),
  ).toBeNull();
  expect(actionView(action({ label: "Open it", page: "nowhere", target: "x" }), f)).toBeNull();
});

test("a row is the engine's headline, one line of why, three numbers and its presses while open", () => {
  const f = finding({
    stats: [
      stat("a", 1, "turns"),
      stat("b", 2, "turns"),
      stat("c", 3, "turns"),
      stat("d", 4, "turns"),
    ],
    actions: {
      primary: action({ kind: "apply", label: "Learn on Haiku", option: 0 }),
      secondary: [
        action({ kind: "apply", label: "Learn on Mini", option: 1 }),
        action({ kind: "reassign" }),
      ],
    },
  });
  const view = findingView(f);
  expect(view.headline).toBe("Agent PM wasted 124 of 185 turns.");
  expect(view.why).toBe("Most of its turns moved no work.");
  expect(view.stats.map((s) => s.key)).toEqual(["a", "b", "c"]);
  expect(view.actions).toEqual({
    primary: { kind: "apply", label: "Learn on Haiku", option: 0 },
    secondary: [{ kind: "apply", label: "Learn on Mini", option: 1 }],
  });
  expect(findingView({ ...f, state: "applied" }).actions).toEqual({ primary: null, secondary: [] });
  expect(firstSentence("No full stop")).toBe("No full stop");
});

test("a primary this page does not route leaves the first press it routes leading", () => {
  const f = finding({
    actions: {
      primary: action({ kind: "reassign", label: "Move the work", items: ["ENG-1"], to: "dev-2" }),
      secondary: [action({ label: "Open it", page: "seat", target: "dev" })],
    },
  });
  expect(findingView(f).actions).toEqual({
    primary: { kind: "open", label: "Open it", href: "#/company/org/seats/dev" },
    secondary: [],
  });
});

test("a figure links the open finding that explains it, the most costly first", () => {
  const note = finding({ id: "n", severity: "note" });
  const cost = finding({ id: "c", severity: "costs" });
  expect(explainer("wasted_turns_percent", [note, cost])?.id).toBe("c");
  expect(explainer("open_items", [note, cost])).toBeNull();
});
