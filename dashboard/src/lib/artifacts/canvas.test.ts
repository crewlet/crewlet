import { describe, expect, test } from "vitest";
import {
  CANVAS_MAX_NOTES,
  CanvasUnreadable,
  addNote,
  boardItems,
  canvasText,
  changeNote,
  mergeCanvas,
  moveBoard,
  parseCanvas,
  publishOf,
  removeNote,
  type DesignCanvas,
} from "./canvas.ts";

const LANDING: DesignCanvas = {
  v: 1,
  title: "Launch",
  boards: {
    "hero.html": { x: 0, y: 0, w: 1440, h: 900, title: "Hero" },
    "pricing.html": { x: 0, y: 1000, w: 1440, h: 900 },
  },
  order: ["hero.html", "pricing.html"],
  notes: [{ id: "s1", kind: "sticky", x: 1600, y: 0, w: 240, h: 200, text: "Copy", color: "pink" }],
};

describe("reading and writing", () => {
  test("a canvas reads back as it was written, in the format's field order", () => {
    const text = canvasText(LANDING);
    expect(parseCanvas(text)).toEqual(LANDING);
    expect(text.indexOf('"v"')).toBeLessThan(text.indexOf('"boards"'));
    expect(text.endsWith("}\n")).toBe(true);
  });

  test("a file outside the format is refused, not guessed at", () => {
    expect(() => parseCanvas("{")).toThrow(CanvasUnreadable);
    expect(() => parseCanvas(JSON.stringify({ ...LANDING, v: 2 }))).toThrow(CanvasUnreadable);
    expect(() => parseCanvas(JSON.stringify({ v: 1, boards: {} }))).toThrow(CanvasUnreadable);
  });

  test("a board without a title is labelled by its page's file name", () => {
    expect(boardItems(LANDING).map((b) => b.title)).toEqual(["Hero", "pricing.html"]);
    expect(boardItems(LANDING)[1]).toMatchObject({ id: "pricing.html", x: 0, y: 1000 });
  });
});

describe("a person's changes", () => {
  test("a move changes that board's frame and nothing else", () => {
    const moved = moveBoard(LANDING, "pricing.html", { x: 1540, y: 0, w: 1440, h: 900 });
    expect(moved.boards["pricing.html"]).toEqual({ x: 1540, y: 0, w: 1440, h: 900 });
    expect(moved.boards["hero.html"]).toBe(LANDING.boards["hero.html"]);
    expect(moveBoard(LANDING, "nowhere.html", { x: 1, y: 1, w: 50, h: 50 })).toBe(LANDING);
  });

  test("a note keeps its id and kind through a change, and a title never takes a colour", () => {
    const changed = changeNote(LANDING, { ...LANDING.notes![0]!, text: "Final copy", x: 10 });
    expect(changed.notes![0]).toEqual({ ...LANDING.notes![0], text: "Final copy", x: 10 });
    const titled = addNote(LANDING, "title", { x: 0, y: -120.4 }, "Launch page");
    expect(titled.notes![1]).toEqual({
      id: "title-1",
      kind: "title",
      x: 0,
      y: -120,
      w: 720,
      h: 80,
      text: "Launch page",
    });
    expect(canvasText(changeNote(titled, { ...titled.notes![1]!, color: "blue" }))).not.toContain(
      '"color": "blue"',
    );
  });

  test("new notes take fresh ids, and none is added past the format's bound", () => {
    const one = addNote(LANDING, "sticky", { x: 0, y: 0 }, "a");
    const two = addNote(one, "sticky", { x: 0, y: 0 }, "b");
    expect(two.notes!.map((n) => n.id)).toEqual(["s1", "note-1", "note-2"]);
    expect(two.notes![1]!.color).toBe("yellow");
    const full: DesignCanvas = {
      ...LANDING,
      notes: Array.from({ length: CANVAS_MAX_NOTES }, (_, i) => ({
        id: `n${i}`,
        kind: "sticky" as const,
        x: 0,
        y: 0,
        w: 100,
        h: 100,
        text: "x",
      })),
    };
    expect(addNote(full, "sticky", { x: 0, y: 0 }, "one more")).toBe(full);
    expect(removeNote(two, "note-1").notes!.map((n) => n.id)).toEqual(["s1", "note-2"]);
  });

  test("a save replaces exactly the canvas it started from, and nothing else", () => {
    const base = canvasText(LANDING);
    const draft = moveBoard(LANDING, "hero.html", { x: 8, y: 0, w: 1440, h: 900 });
    expect(publishOf(base, draft, "Moved the hero")).toEqual({
      mode: "patch",
      edits: [{ path: "canvas.json", old_str: base, new_str: canvasText(draft) }],
      files: [],
      label: "Moved the hero",
    });
    expect(publishOf(base, draft, "")).not.toHaveProperty("label");
  });
});

describe("the merge onto a newer version", () => {
  test("each side's changes to different items are both kept", () => {
    const mine = moveBoard(LANDING, "hero.html", { x: 100, y: 0, w: 1440, h: 900 });
    const live = changeNote(
      moveBoard(LANDING, "pricing.html", { x: 0, y: 2000, w: 1440, h: 900 }),
      { ...LANDING.notes![0]!, text: "Theirs" },
    );
    const merged = mergeCanvas(LANDING, mine, live);
    expect(merged.draft.boards["hero.html"]!.x).toBe(100);
    expect(merged.draft.boards["pricing.html"]!.y).toBe(2000);
    expect(merged.draft.notes![0]!.text).toBe("Theirs");
    expect(merged.overlaps).toEqual([]);
    expect(merged.theirs).toBe(2);
  });

  test("an item both changed keeps the person's change, and says so", () => {
    const mine = moveBoard(LANDING, "hero.html", { x: 100, y: 0, w: 1440, h: 900 });
    const live = moveBoard(LANDING, "hero.html", { x: 0, y: 500, w: 1440, h: 900 });
    const merged = mergeCanvas(LANDING, mine, live);
    expect(merged.draft.boards["hero.html"]).toMatchObject({ x: 100, y: 0 });
    expect(merged.overlaps).toEqual(["Hero"]);
  });

  test("the boards are always the newer version's, since a board is a page", () => {
    const live: DesignCanvas = {
      ...LANDING,
      boards: { ...LANDING.boards, "faq.html": { x: 0, y: 2000, w: 800, h: 600 } },
      order: [...LANDING.order, "faq.html"],
    };
    const merged = mergeCanvas(LANDING, LANDING, live);
    expect(Object.keys(merged.draft.boards)).toEqual(["hero.html", "pricing.html", "faq.html"]);
    expect(merged.draft.order).toEqual(live.order);
  });

  test("notes added, removed and added twice under one id all land", () => {
    const mine = addNote(removeNote(LANDING, "s1"), "sticky", { x: 0, y: 0 }, "Mine");
    const live = addNote(LANDING, "sticky", { x: 50, y: 50 }, "Theirs");
    const merged = mergeCanvas(LANDING, mine, live);
    const texts = merged.draft.notes!.map((n) => `${n.id}:${n.text}`);
    expect(texts).toEqual(["note-1:Theirs", "note-2:Mine"]);
  });
});
