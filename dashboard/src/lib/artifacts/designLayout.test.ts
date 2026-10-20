import { describe, expect, test } from "vitest";
import {
  BOARD_BRIEF_MAX_BYTES,
  BOARD_GAP,
  DESIGN_MAX_BOARDS,
  FRAME_PRESETS,
  STARTER_LAYOUTS,
  addBoard,
  boardProblems,
  draftFrom,
  draftHold,
  dropNote,
  layoutOf,
  linkProblem,
  placeNote,
  presetOf,
  removeBoard,
  reorderBoard,
  retypeNote,
  taskBrief,
  tidy,
  type DesignDraft,
} from "./designLayout.ts";

const phone = FRAME_PRESETS.find((p) => p.id === "phone")!;
const landing = () => draftFrom(STARTER_LAYOUTS.find((l) => l.id === "landing")!);

describe("starting layouts", () => {
  // A LAYOUT IS LAID SIDE BY SIDE in its order, tops aligned, an even gap
  // apart, every board its preset's size and its own id.
  test("each layout lays its boards out in order, an even gap apart", () => {
    for (const layout of STARTER_LAYOUTS) {
      const d = draftFrom(layout);
      expect(d.boards.map((b) => b.title)).toEqual(layout.boards.map((b) => b.title));
      expect(new Set(d.boards.map((b) => b.id)).size).toBe(d.boards.length);
      d.boards.forEach((b, i) => {
        expect(b.y).toBe(0);
        if (i > 0) expect(b.x).toBe(d.boards[i - 1]!.x + d.boards[i - 1]!.w + BOARD_GAP);
      });
    }
    expect(draftFrom(STARTER_LAYOUTS.find((l) => l.id === "blank")!).boards).toEqual([]);
  });

  test("a size is named by its preset, and a custom size by nothing", () => {
    expect(presetOf(390, 844)?.label).toBe("Phone");
    expect(presetOf(391, 844)).toBeUndefined();
  });
});

describe("shaping the boards", () => {
  test("a board is added after the last one, or where the Frame tool placed it, with a fresh name", () => {
    const d = landing();
    const last = d.boards.at(-1)!;
    const after = addBoard(d, phone);
    const added = after.draft.boards.at(-1)!;
    expect(after.id).toBe(added.id);
    expect(added).toMatchObject({
      x: last.x + last.w + BOARD_GAP,
      y: 0,
      w: 390,
      h: 844,
      title: "Screen 4",
    });
    const placed = addBoard(d, phone, { x: 10.6, y: -20.2 }).draft.boards.at(-1)!;
    expect(placed).toMatchObject({ x: 11, y: -20 });
  });

  test("a design holds at most the format's boards", () => {
    let d: DesignDraft = { boards: [], notes: [] };
    for (let i = 0; i < DESIGN_MAX_BOARDS; i++) d = addBoard(d, phone).draft;
    const refused = addBoard(d, phone);
    expect(refused.id).toBeUndefined();
    expect(refused.draft).toBe(d);
  });

  test("a board moves in the order, held to its ends, and is removed", () => {
    const d = landing();
    const [home, pricing] = d.boards;
    expect(reorderBoard(d, pricing!.id, -1).boards[0]!.id).toBe(pricing!.id);
    expect(reorderBoard(d, home!.id, -1)).toBe(d);
    expect(removeBoard(d, home!.id).boards.map((b) => b.title)).toEqual([
      "Pricing",
      "Home on phone",
    ]);
    // Tidy lays out what moved, in the new order.
    const tidied = tidy(reorderBoard(d, pricing!.id, -1).boards);
    expect(tidied[0]!.x).toBe(0);
    expect(tidied[0]!.title).toBe("Pricing");
  });

  test("notes are placed, retyped and removed by the canvas's own rules", () => {
    const placed = placeNote(landing(), "sticky", { x: 4, y: 5 });
    expect(placed.id).toMatch(/^[a-z0-9][a-z0-9-]*$/);
    const note = placed.draft.notes[0]!;
    expect(note).toMatchObject({ kind: "sticky", x: 4, y: 5, text: "Note", color: "yellow" });
    const retyped = retypeNote(placed.draft, { ...note, text: "Copy by Friday" });
    expect(retyped.notes[0]!.text).toBe("Copy by Friday");
    expect(dropNote(retyped, note.id).notes).toEqual([]);
  });
});

describe("what the engine would refuse", () => {
  test("a board is named, sized within the bounds, and briefed within the cap", () => {
    const b = landing().boards[0]!;
    expect(Object.values(boardProblems(b)).filter(Boolean)).toEqual([]);
    const wrong = boardProblems({
      ...b,
      title: "  ",
      w: 39,
      h: 8001,
      brief: "a".repeat(BOARD_BRIEF_MAX_BYTES + 1),
    });
    expect(wrong.title).toBe("Give the board a name.");
    expect(wrong.w).toMatch(/40 to 8000/);
    expect(wrong.h).toMatch(/40 to 8000/);
    expect(wrong.brief).toMatch(/2000/);
  });

  test("a draft is held until it has a board and every board is right", () => {
    expect(draftHold({ boards: [], notes: [] })).toBe("Add a board.");
    const d = landing();
    expect(draftHold(d)).toBeUndefined();
    expect(draftHold({ ...d, boards: [{ ...d.boards[1]!, w: 10 }] })).toBe(
      "Fix the board “Pricing”.",
    );
  });
});

describe("what is sent", () => {
  test("the layout carries each board trimmed, with a brief only where one was written", () => {
    const d = landing();
    d.boards[0] = { ...d.boards[0]!, title: "  Home ", brief: "  Hero  " };
    const layout = layoutOf(d);
    expect(layout.boards[0]).toEqual({
      title: "Home",
      x: 0,
      y: 0,
      w: 1440,
      h: 1024,
      brief: "Hero",
    });
    expect(layout.boards[1]).not.toHaveProperty("brief");
    expect(layout).not.toHaveProperty("notes");
    expect(layoutOf(placeNote(d, "title", { x: 0, y: -100 }).draft).notes).toHaveLength(1);
  });

  test("the task brief says the goal, every board in order, the references and when it is done", () => {
    const d = landing();
    d.boards[0] = { ...d.boards[0]!, brief: "Hero with the sign-up form" };
    const text = taskBrief({ brief: "A launch page.", draft: d, links: ["https://example.com"] });
    expect(text.split("\n\n")).toEqual([
      "A launch page.",
      "Boards, in order:\n1. Home (1440 by 1024, Desktop): Hero with the sign-up form\n2. Pricing (1440 by 1024, Desktop)\n3. Home on phone (390 by 844, Phone)",
      "References:\n- https://example.com",
      expect.stringMatching(/^Each board is a placeholder page.*every board is designed\.$/),
    ]);
  });

  test("a reference is a web address", () => {
    expect(linkProblem("https://figma.com/file/x")).toBeUndefined();
    expect(linkProblem("javascript:alert(1)")).toMatch(/https/);
    expect(linkProblem("figma")).toMatch(/https/);
  });
});
