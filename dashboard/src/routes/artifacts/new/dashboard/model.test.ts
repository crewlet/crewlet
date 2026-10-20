import { describe, expect, test } from "vitest";
import type { DashboardEngineSource } from "~/protocol/index.ts";
import catalogueJson from "./testdata/catalogue.json";
import {
  EMPTY_DRAFT,
  STARTERS,
  composeSpec,
  datasetOf,
  fromStarter,
  place,
  recipeOf,
  recipesOf,
  reorder,
  resize,
  resolve,
  setSetting,
  settingsOf,
  sourceWords,
  type Draft,
} from "./model.ts";

// THE ENGINE'S OWN CATALOGUE, held equal to `dashspec.Catalogue()` by the
// engine's suite (`TestNewDashboardsCatalogueIsTheEngines`).
const CATALOGUE = catalogueJson as DashboardEngineSource[];
const source = (name: string) => CATALOGUE.find((s) => s.name === name)!;

/** A draft of every figure every source suggests, each once. */
function everyFigure(): Draft {
  let d = EMPTY_DRAFT;
  for (const src of CATALOGUE) {
    for (const r of recipesOf(src)) d = place(d, src.name, r.key, r.widget.w).draft;
  }
  return d;
}

describe("the boards New dashboard composes", () => {
  // EVERY BOARD THIS PAGE CAN COMPOSE FROM A STARTER, and one of every figure,
  // written where the engine's suite holds each to the board format
  // (`TestNewDashboardComposesBoardsTheEngineAccepts`). A recipe changed here
  // changes this file, which `vitest -u` rewrites and the engine re-checks.
  test("are written for the engine's suite to validate", async () => {
    const boards: Record<string, unknown> = {
      every_figure: composeSpec("Every figure", everyFigure(), CATALOGUE),
    };
    for (const s of STARTERS)
      boards[s.key] = composeSpec(s.label, fromStarter(s, CATALOGUE), CATALOGUE);
    await expect(`${JSON.stringify(boards, null, 2)}\n`).toMatchFileSnapshot(
      "./testdata/composed.json",
    );
  });

  test("name only columns their source answers, of the type each widget reads", () => {
    for (const r of resolve(everyFigure(), CATALOGUE)) {
      const columns = new Map(r.source.columns.map((c) => [c.key, c.type]));
      const w = r.widget;
      const named = [w.value, w.x, w.label, w.by, ...(w.y ?? []), ...(w.columns ?? [])].filter(
        (k): k is string => !!k,
      );
      for (const k of named)
        expect(columns.has(k), `${r.source.name}.${r.recipe.key}: ${k}`).toBe(true);
      if (w.x) expect(columns.get(w.x)).toBe("date");
      for (const y of w.y ?? []) expect(columns.get(y)).toBe("number");
      if (w.value && w.agg !== "count") expect(columns.get(w.value)).toBe("number");
    }
  });

  test("declare one dataset per distinct read, its pins a read of their own", () => {
    const d = fromStarter(
      STARTERS.find((s) => s.key === "work")!,
      CATALOGUE,
    );
    const spec = composeSpec("Work", d, CATALOGUE);
    expect(spec.datasets.map((x) => x.id)).toEqual([
      "tracker_items",
      "tracker_items_overdue",
      "tracker_items_blocked",
    ]);
    expect(spec.datasets[1]!.engine).toEqual({
      name: "tracker.items",
      params: { scope: "overdue" },
    });
    expect(spec.widgets.map((w) => w.dataset)).toEqual([
      "tracker_items",
      "tracker_items_overdue",
      "tracker_items_blocked",
      "tracker_items",
    ]);
    expect(spec.title).toBe("Work");
  });

  test("write a person's settings as the source's own types, and leave defaults out", () => {
    let d = place(EMPTY_DRAFT, "spend.daily", "columns", 8).draft;
    d = setSetting(d, "spend.daily", "days", "30");
    d = setSetting(d, "spend.daily", "group", "seat");
    const spec = composeSpec("Spend", d, CATALOGUE);
    expect(spec.datasets).toEqual([
      {
        id: "spend_daily_30",
        title: "Spend by day, last 30 days",
        source: "engine",
        engine: { name: "spend.daily", params: { days: 30 } },
      },
    ]);
  });

  test("drop a figure over a source the catalogue no longer lists", () => {
    const d = place(EMPTY_DRAFT, "tracker.gone", "count", 3).draft;
    expect(composeSpec("Gone", d, CATALOGUE).widgets).toEqual([]);
  });
});

describe("a draft", () => {
  test("gives every figure an id of its own", () => {
    let d = EMPTY_DRAFT;
    const ids: string[] = [];
    for (let i = 0; i < 3; i++) {
      const made = place(d, "tracker.items", "count", 3);
      d = made.draft;
      ids.push(made.id);
    }
    expect(ids).toEqual(["tracker_items_count", "tracker_items_count_2", "tracker_items_count_3"]);
  });

  test("holds a width to the grid and keeps an order it was given", () => {
    let d = place(EMPTY_DRAFT, "tracker.items", "count", 3).draft;
    d = place(d, "tracker.items", "table", 12).draft;
    expect(resize(d, "tracker_items_count", 40).placed[0]!.w).toBe(12);
    expect(resize(d, "tracker_items_count", 0).placed[0]!.w).toBe(1);
    expect(reorder(d, ["tracker_items_table"]).placed.map((p) => p.id)).toEqual([
      "tracker_items_table",
      "tracker_items_count",
    ]);
  });
});

describe("a source in a person's words", () => {
  test("offers its enum parameters and a window of days, never a key to type", () => {
    const items = settingsOf(source("tracker.items"));
    expect(items.map((s) => s.key)).toEqual(["scope", "status_group"]);
    expect(items[0]!.options[0]).toEqual({ value: "all", label: "Everyone" });
    expect(items[1]!.options[0]).toEqual({ value: "", label: "Open work" });
    const seats = settingsOf(source("spend.seats"));
    expect(seats[0]!.options.map((o) => o.value)).toEqual(["", "7", "14", "30", "90"]);
    expect(settingsOf(source("tracker.workload"))).toEqual([]);
  });

  test("names a pinned read by what it pins", () => {
    const src = source("tracker.items");
    expect(datasetOf(src, {}, recipeOf(src, "overdue")).title).toBe("Work items, overdue");
    expect(datasetOf(src, { scope: "all" }, undefined).key).toBe("tracker.items");
  });

  test("says a source this build does not know in the catalogue's words", () => {
    const novel: DashboardEngineSource = {
      ...source("agents.status"),
      name: "deploys.recent",
      domain: "deploys",
      summary: "Recent `deploys`, one row each.",
    };
    expect(sourceWords(novel)).toEqual({
      label: "deploys.recent",
      area: "Deploys",
      description: "Recent deploys, one row each.",
    });
    expect(recipesOf(novel).map((r) => r.widget.type)).toEqual(["stat", "table"]);
  });
});
