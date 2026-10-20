/**
 * A new deck's plan as a person lays it out on the create page: the theme it
 * is drawn in and the slides it opens with, each a layout and its title, and
 * the slides the page previews it with.
 *
 * THE ENGINE COMPOSES THE DECK (`slideformat.Compose`), so the page sends the
 * plan, never markup; and THE PREVIEW IS THE ENGINE'S OWN DRAWING. Every
 * theme's sample deck (`GET /artifacts/starters/slides`) holds one slide of
 * each layout as the canonical tree the format's parser produced, and a row is
 * previewed as its layout's sample slide with the title replaced, so what the
 * page shows is what the engine writes, with no parsing here.
 *
 * PURE: no fetch and no React.
 */

import type { ArtifactDeck, DeckLayout, DeckPlan, DeckSlide, DeckTheme } from "~/protocol/index.ts";
import { titlePath, withText } from "../slides.tsx";
import { utf8Bytes } from "../format.ts";
import { DECK_PLAN_MAX_SLIDES, DECK_SLIDE_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";

/** The layout whose title is the deck's own (`slideformat.LayoutCover`). */
export const COVER_LAYOUT = "cover";

/** One slide of the plan being laid out: a key of its own, its layout and its title. */
export interface PlanRow {
  /** Stable across moves and edits; never sent. */
  key: string;
  layout: string;
  /** "" is the layout's own heading (a cover's is the deck's title). */
  title: string;
}

/** The plan's rows from the engine's default plan, keyed from 1. */
export function rowsOf(plan: DeckPlan): PlanRow[] {
  return (plan.slides ?? []).map((s, i) => ({
    key: `r${i + 1}`,
    layout: s.layout,
    title: s.title ?? "",
  }));
}

/** A key no row holds yet. */
export function nextKey(rows: readonly PlanRow[]): string {
  let n = rows.length + 1;
  while (rows.some((r) => r.key === `r${n}`)) n++;
  return `r${n}`;
}

/** rows with a new slide of layout after the row keyed after (at the end when it is not found). */
export function addAfter(rows: readonly PlanRow[], after: string, layout: string): PlanRow[] {
  const row: PlanRow = { key: nextKey(rows), layout, title: "" };
  const at = rows.findIndex((r) => r.key === after);
  return at < 0 ? [...rows, row] : [...rows.slice(0, at + 1), row, ...rows.slice(at + 1)];
}

/** rows with a copy of the row keyed key right after it. */
export function duplicate(rows: readonly PlanRow[], key: string): PlanRow[] {
  const at = rows.findIndex((r) => r.key === key);
  if (at < 0) return [...rows];
  const copy: PlanRow = { ...rows[at]!, key: nextKey(rows) };
  return [...rows.slice(0, at + 1), copy, ...rows.slice(at + 1)];
}

/** rows without the row keyed key, and the key to select after it: the next row, else the one before. */
export function remove(rows: readonly PlanRow[], key: string): { rows: PlanRow[]; select: string } {
  const at = rows.findIndex((r) => r.key === key);
  const rest = rows.filter((r) => r.key !== key);
  return { rows: rest, select: rest[Math.min(Math.max(at, 0), rest.length - 1)]?.key ?? "" };
}

/** rows in the order of keys. */
export function reorder(rows: readonly PlanRow[], keys: readonly string[]): PlanRow[] {
  const byKey = new Map(rows.map((r) => [r.key, r]));
  return keys.map((k) => byKey.get(k)).filter((r): r is PlanRow => r !== undefined);
}

/** rows with the row keyed key set to a layout or a title. */
export function update(
  rows: readonly PlanRow[],
  key: string,
  patch: Partial<Omit<PlanRow, "key">>,
): PlanRow[] {
  return rows.map((r) => (r.key === key ? { ...r, ...patch } : r));
}

/** The plan the create sends: the theme, and each row's layout and title (left out when empty). */
export function planOf(theme: string, rows: readonly PlanRow[]): DeckPlan {
  return {
    theme,
    slides: rows.map((r) => {
      const title = r.title.trim();
      return title ? { layout: r.layout, title } : { layout: r.layout };
    }),
  };
}

/** What is wrong with a slide's title, in the field's sentence, or undefined. */
export function slideTitleProblem(title: string): string | undefined {
  const bytes = utf8Bytes(title.trim());
  return bytes > DECK_SLIDE_TITLE_MAX_BYTES
    ? `The title is ${bytes} bytes and the most is ${DECK_SLIDE_TITLE_MAX_BYTES}.`
    : undefined;
}

/** Why the plan cannot be sent, or undefined when the engine will take it. */
export function planHold(rows: readonly PlanRow[]): string | undefined {
  if (rows.length === 0) return "Add at least one slide.";
  if (rows.length > DECK_PLAN_MAX_SLIDES)
    return `A new deck opens with at most ${DECK_PLAN_MAX_SLIDES} slides. Ask an agent for a longer one.`;
  const bad = rows.findIndex((r) => slideTitleProblem(r.title) !== undefined);
  return bad < 0 ? undefined : `Slide ${bad + 1}'s title is too long.`;
}

/** The title a row is drawn with: its own, else the deck's for a cover, else its layout's heading. */
export function shownTitle(
  row: PlanRow,
  deckTitle: string,
  layout: DeckLayout | undefined,
): string {
  const own = row.title.trim();
  if (own) return own;
  if (row.layout === COVER_LAYOUT) return deckTitle.trim() || "Untitled deck";
  return layout?.heading ?? "";
}

/** A theme's sample slide of a layout: the sample's slide ids are its layouts' (`slideformat.Sample`). */
export function sampleSlide(sample: ArtifactDeck, layout: string): DeckSlide | undefined {
  return (sample.slides ?? []).find((s) => s.id === layout);
}

/**
 * A row drawn in theme: its layout's sample slide with its shown title, under
 * the row's key, so a filmstrip keys it by the row; undefined when the theme
 * has no slide of the row's layout.
 */
export function previewSlide(
  theme: DeckTheme,
  row: PlanRow,
  deckTitle: string,
  layout: DeckLayout | undefined,
): DeckSlide | undefined {
  const sample = sampleSlide(theme.sample, row.layout);
  if (!sample) return undefined;
  const title = shownTitle(row, deckTitle, layout);
  const at = titlePath(sample.root);
  return {
    ...sample,
    id: row.key,
    title,
    root: at ? withText(sample.root, at, title) : sample.root,
  };
}
