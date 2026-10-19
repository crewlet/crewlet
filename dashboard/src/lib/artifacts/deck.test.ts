/**
 * A person's deck edit: what it changed, the publish that saves it, and the
 * merge a refused save is moved onto the newer version by.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { ArtifactDeck } from "~/protocol/index.ts";
import { slideSource, withText } from "../slides.tsx";
import {
  changesOf,
  draftOf,
  indexSource,
  mergeDeck,
  publishOf,
  resolve,
  slidePath,
  sourcesNeeded,
  type DeckDraft,
} from "./deck.ts";

const golden = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/artifacts/slideformat/testdata/deck.golden.json"),
    "utf8",
  ),
) as ArtifactDeck;
const index = readFileSync(
  join(process.cwd(), "..", "internal/artifacts/slideformat/testdata/deck/deck.json"),
  "utf8",
);

const base = draftOf(golden, 1);

/** The path of the run of text holding needle on slide id. */
function runOf(d: DeckDraft, id: string, needle: string): string {
  const find = (n: { text?: string; children?: unknown[] }, path: string): string | null => {
    if (n.text?.includes(needle)) return path;
    for (const [i, c] of (n.children ?? []).entries()) {
      const at = find(c as typeof n, path ? `${path}/${i}` : String(i));
      if (at) return at;
    }
    return null;
  };
  return find(d.slides[id]!.root, "")!;
}

/** The exit test's person: a typo fixed on slide 4, slides 6 and 7 swapped. */
function personsEdit(from: DeckDraft): DeckDraft {
  const path = runOf(from, "plan", "plcae");
  const plan = from.slides.plan!;
  const order = [...from.order];
  const [a, b] = [order.indexOf("team"), order.indexOf("timeline")];
  [order[a], order[b]] = [order[b]!, order[a]!];
  return {
    ...from,
    order,
    slides: {
      ...from.slides,
      plan: { ...plan, root: withText(plan.root, path, "Every request lands in one place.") },
    },
  };
}

describe("a person's edit", () => {
  test("a typo and a swap change one slide and the index", () => {
    const c = changesOf(base, personsEdit(base));
    expect(c).toEqual({ slides: ["plan"], removed: [], index: true });
    expect(sourcesNeeded(base, personsEdit(base))).toEqual(["slides/plan.html", "deck.json"]);
  });

  test("the save is a patch replacing each changed file whole, against the bytes it read", () => {
    const sources = { "slides/plan.html": "<section>old</section>", "deck.json": index };
    const body = publishOf(base, personsEdit(base), sources, "Fix a typo");
    expect(body.mode).toBe("patch");
    expect(body.label).toBe("Fix a typo");
    expect(
      body.edits.map((e) => [e.path, e.old_str === sources[e.path as keyof typeof sources]]),
    ).toEqual([
      ["slides/plan.html", true],
      ["deck.json", true],
    ]);
    expect(body.edits[0]!.new_str).toContain("one place.");
    const next = JSON.parse(body.edits[1]!.new_str) as {
      order: string[];
      v: number;
      sections: unknown;
    };
    expect(next.order.slice(5, 7)).toEqual(["timeline", "team"]);
    expect(next.v).toBe(4);
    expect(next.sections).toEqual(JSON.parse(index).sections);
  });

  test("a deleted slide's file is removed, and a cover or section naming it moves to the next slide", () => {
    const draft = { ...base, order: base.order.filter((id) => id !== "plan") };
    const body = publishOf(base, draft, { "deck.json": index });
    expect(body.files).toEqual([{ path: slidePath("plan"), remove: true }]);
    const next = JSON.parse(indexSource(index, base, draft)) as {
      sections: Record<string, { start: string }>;
    };
    expect(next.sections.s2!.start).toBe("numbers");
  });

  test("an unread source is a refusal before anything is sent", () => {
    expect(() => publishOf(base, personsEdit(base), {})).toThrow(/was not read/);
  });
});

describe("merging onto a newer version", () => {
  /** The concurrent agent: it changed slide 9's heading. */
  function agents(from: DeckDraft, version: number): DeckDraft {
    const chart = from.slides.chart!;
    const path = runOf(from, "chart", "Requests by week");
    return {
      ...from,
      base: version,
      slides: {
        ...from.slides,
        chart: { ...chart, root: withText(chart.root, path, "Requests each week") },
      },
    };
  }

  test("changes to different slides merge, both kept, nothing to resolve", () => {
    const live = agents(base, 2);
    const m = mergeDeck(base, personsEdit(base), live);
    expect(m.conflicts).toEqual([]);
    expect(m.theirs).toEqual(["chart"]);
    expect(m.draft.base).toBe(2);
    expect(slideSource(m.draft.slides.plan!)).toContain("one place.");
    expect(slideSource(m.draft.slides.chart!)).toContain("Requests each week");
    expect(m.draft.order.slice(5, 7)).toEqual(["timeline", "team"]);
    // AGAINST THE NEWER VERSION, the save rewrites only what the person changed.
    expect(changesOf(m.live, m.draft)).toEqual({ slides: ["plan"], removed: [], index: true });
  });

  test("one slide both changed differently is the person's to resolve", () => {
    const path = runOf(base, "plan", "plcae");
    const theirs: DeckDraft = {
      ...base,
      base: 2,
      slides: {
        ...base.slides,
        plan: {
          ...base.slides.plan!,
          root: withText(base.slides.plan!.root, path, "One place for everything."),
        },
      },
    };
    const m = mergeDeck(base, personsEdit(base), theirs);
    expect(m.conflicts).toEqual([{ kind: "slide", id: "plan", title: "The plan" }]);
    expect(slideSource(m.draft.slides.plan!)).toContain("one place.");
    const taken = resolve(m, m.conflicts[0]!, "theirs");
    expect(taken.conflicts).toEqual([]);
    expect(slideSource(taken.draft.slides.plan!)).toContain("One place for everything.");
  });

  test("a slide the newer version added stays where it put it", () => {
    const live: DeckDraft = {
      ...base,
      base: 2,
      order: [...base.order.slice(0, 2), "extra", ...base.order.slice(2)],
      slides: { ...base.slides, extra: { ...base.slides.agenda!, id: "extra" } },
    };
    const m = mergeDeck(base, personsEdit(base), live);
    expect(m.conflicts).toEqual([]);
    expect(m.draft.order.slice(0, 4)).toEqual(["cover", "agenda", "extra", "problem"]);
  });

  test("both reordering differently is the person's to resolve", () => {
    const live: DeckDraft = { ...base, base: 2, order: [...base.order].reverse() };
    const m = mergeDeck(base, personsEdit(base), live);
    expect(m.conflicts).toContainEqual({ kind: "order" });
    expect(resolve(m, { kind: "order" }, "theirs").draft.order).toEqual(live.order);
  });
});
