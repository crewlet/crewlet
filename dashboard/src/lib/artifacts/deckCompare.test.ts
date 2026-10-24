/**
 * Two versions of a deck paired slide by slide: an edit is one row, a slide
 * one version lacks sits where it was, and a move is what the order changed.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { ArtifactDeck, DeckSlide } from "~/protocol/index.ts";
import { withText } from "../slides.tsx";
import { compareDecks } from "./deckCompare.ts";

const golden = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/artifacts/slideformat/testdata/deck.golden.json"),
    "utf8",
  ),
) as ArtifactDeck;
const slides = golden.slides!;
const byId = (id: string) => slides.find((s) => s.id === id)!;
const deck = (list: DeckSlide[]): ArtifactDeck => ({ ...golden, slides: list });
const summary = (older: ArtifactDeck, newer: ArtifactDeck) =>
  compareDecks(older, newer).map((r) => `${r.id}:${r.change}${r.moved ? ":moved" : ""}`);

describe("comparing two decks", () => {
  test("the exit test's edit is one changed slide and one move", () => {
    const plan = byId("plan");
    const edited = {
      ...plan,
      root: withText(plan.root, "1/0/1/0", "Every request lands in one place."),
    };
    const order = slides.map((s) => (s.id === "plan" ? edited : s));
    [order[5], order[6]] = [order[6]!, order[5]!];
    const rows = compareDecks(golden, deck(order));
    const changed = rows.filter((r) => r.change !== "unchanged" || r.moved);
    expect(changed.map((r) => `${r.id}:${r.change}:${r.moved}`)).toEqual([
      "plan:changed:false",
      "team:unchanged:true",
    ]);
    expect(rows.find((r) => r.id === "team")).toMatchObject({ fromPosition: 6, toPosition: 7 });
  });

  test("an added slide and a removed one sit where they are, and nothing moved", () => {
    const extra = { ...byId("agenda"), id: "extra", title: "Extra" };
    const newer = [slides[0]!, extra, ...slides.slice(2)];
    expect(summary(golden, deck(newer)).slice(0, 4)).toEqual([
      "cover:unchanged",
      "agenda:removed",
      "extra:added",
      "problem:unchanged",
    ]);
  });

  test("a slide moved to the end moves alone", () => {
    const newer = [...slides.filter((s) => s.id !== "agenda"), byId("agenda")];
    expect(summary(golden, deck(newer)).filter((r) => r.includes("moved"))).toEqual([
      "agenda:unchanged:moved",
    ]);
  });

  test("a change to the notes is a change to the slide", () => {
    const newer = slides.map((s) => (s.id === "cover" ? { ...s, notes: "New notes." } : s));
    expect(summary(golden, deck(newer))[0]).toBe("cover:changed");
  });
});
