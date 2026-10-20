/**
 * A new deck's plan: its rows as the create page edits them, the plan it
 * sends, and the slides it previews, drawn from the engine's own answer.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import type { SlidesStartersAnswer } from "~/protocol/index.ts";
import { titleOf } from "../slides.tsx";
import {
  addAfter,
  duplicate,
  planHold,
  planOf,
  previewSlide,
  remove,
  reorder,
  rowsOf,
  update,
} from "./deckPlan.ts";

const starters = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/api/artifactsapi/testdata/slides-starters.golden.json"),
    "utf8",
  ),
) as SlidesStartersAnswer;
const layouts = new Map(starters.layouts.map((l) => [l.id, l]));
const theme = starters.themes.find((t) => t.id === "paper")!;

test("the rows open on the engine's default plan, each keyed", () => {
  const rows = rowsOf(starters.default);
  expect(rows.map((r) => r.layout)).toEqual(["cover", "agenda", "columns", "closing"]);
  expect(new Set(rows.map((r) => r.key)).size).toBe(4);
});

test("a slide is added after the selected one, duplicated after itself, removed and reordered", () => {
  let rows = rowsOf(starters.default);
  rows = addAfter(rows, rows[1]!.key, "number");
  expect(rows.map((r) => r.layout)).toEqual(["cover", "agenda", "number", "columns", "closing"]);
  rows = update(rows, rows[2]!.key, { title: "Uptime" });
  rows = duplicate(rows, rows[2]!.key);
  expect(rows.slice(2, 4).map((r) => [r.layout, r.title])).toEqual([
    ["number", "Uptime"],
    ["number", "Uptime"],
  ]);
  expect(new Set(rows.map((r) => r.key)).size).toBe(rows.length);
  const gone = remove(rows, rows[rows.length - 1]!.key);
  expect(gone.rows).toHaveLength(5);
  expect(gone.select).toBe(gone.rows[4]!.key);
  const flipped = reorder(
    gone.rows,
    [...gone.rows].reverse().map((r) => r.key),
  );
  expect(flipped[0]!.layout).toBe("columns");
});

test("the plan sends each layout, and a title only where one was typed", () => {
  const rows = update(rowsOf(starters.default), "r2", { title: "  The plan  " });
  expect(planOf("grove", rows)).toEqual({
    theme: "grove",
    slides: [
      { layout: "cover" },
      { layout: "agenda", title: "The plan" },
      { layout: "columns" },
      { layout: "closing" },
    ],
  });
});

test("a plan with no slide, too many, or a title too long is held, naming why", () => {
  expect(planHold([])).toBe("Add at least one slide.");
  const many = Array.from({ length: 61 }, (_, i) => ({
    key: `r${i}`,
    layout: "points",
    title: "",
  }));
  expect(planHold(many)).toMatch(/at most 60 slides/);
  expect(planHold([{ key: "r1", layout: "points", title: "x".repeat(201) }])).toBe(
    "Slide 1's title is too long.",
  );
  expect(planHold(rowsOf(starters.default))).toBeUndefined();
});

test("a row previews as its layout's sample in the theme, with the title it will be written with", () => {
  const [cover, agenda] = rowsOf(starters.default);
  const drawn = previewSlide(theme, cover!, "Board review", layouts.get("cover"));
  expect(drawn?.id).toBe(cover!.key);
  expect(titleOf(drawn!.root)).toBe("Board review");
  expect(titleOf(previewSlide(theme, cover!, "", layouts.get("cover"))!.root)).toBe(
    "Untitled deck",
  );
  const titled = previewSlide(
    theme,
    { ...agenda!, title: "Today" },
    "Deck",
    layouts.get("agenda"),
  )!;
  expect(titleOf(titled.root)).toBe("Today");
  expect(titleOf(previewSlide(theme, agenda!, "Deck", layouts.get("agenda"))!.root)).toBe("Agenda");
  // A quote's title is its first paragraph: the quote itself.
  const quote = previewSlide(
    theme,
    { key: "q", layout: "quote", title: "Ship it." },
    "Deck",
    layouts.get("quote"),
  )!;
  expect(titleOf(quote.root)).toBe("Ship it.");
});

test("every theme previews every layout", () => {
  for (const t of starters.themes)
    for (const l of starters.layouts)
      expect(
        previewSlide(t, { key: "k", layout: l.id, title: "" }, "Deck", l),
        `${t.id}/${l.id}`,
      ).toBeDefined();
});
