// @vitest-environment node
/**
 * An artifact's threads as the dashboard draws the engine's answer: the list
 * each is under, the words a draft is given (the engine's own), and where a
 * pin stands over a page, a slide's element and a design's board.
 */

import { describe, expect, test } from "vitest";
import type { ArtifactCommentThread } from "~/protocol/index.ts";
import type { CommentLayout } from "~/runtime/bridge.ts";
import {
  PIN_INSET,
  anchorBox,
  anchorLabel,
  boardAnchorAt,
  boardWorld,
  elementText,
  filterOf,
  filterParam,
  listed,
  pagePin,
  partPin,
  phrase,
  pinned,
  placeOf,
  stateWords,
} from "./comments.ts";

const SECTION = { kind: "html", page: "index.html", name: "pricing" } as const;

function thread(over: Partial<ArtifactCommentThread> = {}): ArtifactCommentThread {
  return {
    id: "t1",
    version: 2,
    anchor: SECTION,
    label: "Section: Pricing",
    placement: "here",
    place: SECTION,
    status: "open",
    created_by: "avery",
    created_by_kind: "human",
    created_at: "2026-11-14T10:00:00Z",
    updated_at: "2026-11-14T10:00:00Z",
    remarks: [],
    ...over,
  };
}

describe("the lists", () => {
  test("a thread is open while placed, outdated once its part is gone, and resolved either way", () => {
    expect(filterOf(thread())).toBe("open");
    expect(filterOf(thread({ placement: "moved" }))).toBe("open");
    expect(filterOf(thread({ placement: "outdated", place: undefined }))).toBe("outdated");
    expect(filterOf(thread({ status: "resolved", placement: "outdated" }))).toBe("resolved");
  });

  test("a thread made on a later version is listed under none", () => {
    const later = thread({ id: "t2", placement: "later", place: undefined });
    expect(listed([thread(), later], "open").map((t) => t.id)).toEqual(["t1"]);
  });

  test("the address names a list, Open where it names none or another word", () => {
    expect(filterParam("resolved")).toBe("resolved");
    expect(filterParam("outdated")).toBe("outdated");
    expect(filterParam("")).toBe("open");
    expect(filterParam("everything")).toBe("open");
  });

  // Mutation: pin a resolved thread under Open and the artifact is covered in checks.
  test("a pin is drawn for an open thread placed here, a resolved one only under Resolved or selected", () => {
    const resolved = thread({ status: "resolved" });
    expect(pinned(thread(), "open", null)).toBe(true);
    expect(pinned(resolved, "open", null)).toBe(false);
    expect(pinned(resolved, "resolved", null)).toBe(true);
    expect(pinned(resolved, "open", "t1")).toBe(true);
    expect(pinned(thread({ placement: "outdated", place: undefined }), "outdated", "t1")).toBe(
      false,
    );
    const whole = thread({ anchor: { kind: "version" }, place: { kind: "version" } });
    expect(placeOf(whole)).toBeNull();
    expect(pinned(whole, "open", null)).toBe(false);
  });

  test("a moved thread is pinned where the engine found it, not where it was made", () => {
    const moved = thread({
      anchor: { kind: "slides", slide: "s1", path: "0/1", tag: "h2", text: "Results" },
      placement: "moved",
      place: { kind: "slides", slide: "s1", path: "0/3", tag: "h2", text: "Results" },
    });
    expect(placeOf(moved)?.path).toBe("0/3");
  });
});

describe("the words", () => {
  test("a draft is said as the engine says the thread it becomes", () => {
    expect(anchorLabel({ kind: "version" })).toBe("The whole version");
    expect(anchorLabel({ kind: "html", page: "index.html", name: "option-annual" })).toBe(
      "Section: Option annual",
    );
    expect(anchorLabel({ kind: "html", page: "index.html", point: { x: 0.2, y: 0.4 } })).toBe(
      "A point on the page",
    );
    const results = { kind: "slides", slide: "s2", path: "0", tag: "h1", text: "Results" } as const;
    expect(anchorLabel(results, ["s1", "s2"])).toBe("Slide 2: “Results”");
    expect(anchorLabel(results, ["s1"])).toBe("A removed slide: “Results”");
    expect(anchorLabel({ ...results, text: "" }, ["s2"])).toBe("Slide 1");
    expect(anchorLabel({ ...results, text: "x".repeat(70) }, ["s2"])).toBe(
      `Slide 1: “${"x".repeat(60)}…”`,
    );
    expect(anchorLabel({ kind: "design", board: "boards/welcome.html", x: 195, y: 760 })).toBe(
      "Welcome board, at 195, 760",
    );
    // THE ENGINE LEAVES A ZERO OUT of the wire.
    expect(anchorLabel({ kind: "design", board: "boards/plan_v2.HTM", y: 4 })).toBe(
      "Plan v2 board, at 0, 4",
    );
  });

  test("a name reads as words, its first letter a capital", () => {
    expect(phrase("plans.annual-v2")).toBe("Plans annual v2");
    expect(phrase("x__y")).toBe("X y");
  });

  test("a thread's state says the version that resolved it, or why it has no place here", () => {
    expect(stateWords(thread({ status: "resolved", resolved_version: 3 }), 4)).toBe(
      "Resolved in v3",
    );
    expect(stateWords(thread({ status: "resolved" }), 4)).toBe("Resolved");
    expect(stateWords(thread({ placement: "outdated", place: undefined }), 3)).toBe(
      "Outdated: v3 has no pricing section",
    );
    const slide = { kind: "slides", slide: "s9", path: "0", tag: "p", text: "Old" } as const;
    expect(
      stateWords(
        thread({ anchor: slide, placement: "outdated", label: "A removed slide: “Old”" }),
        3,
      ),
    ).toBe("Outdated: v3 has no such slide");
    expect(
      stateWords(thread({ anchor: slide, placement: "outdated", label: "Slide 2: “Old”" }), 3),
    ).toBe("Outdated: the slide in v3 no longer has this element");
    expect(stateWords(thread({ placement: "moved" }), 3)).toBe("Moved with its content");
    expect(stateWords(thread(), 3)).toBeNull();
  });

  test("an element's text is kept as the engine keeps it: runs joined, breaks as spaces, 200 at most", () => {
    const node = {
      tag: "p",
      children: [
        { text: "Revenue  grew" },
        { tag: "br" },
        { tag: "b", children: [{ text: "40%\n" }] },
        { text: " in Q3" },
      ],
    };
    expect(elementText(node)).toBe("Revenue grew 40% in Q3");
    expect(elementText({ tag: "p", children: [{ text: "é".repeat(300) }] })).toHaveLength(200);
    expect(elementText(null)).toBe("");
  });
});

describe("where a pin stands", () => {
  const layout: CommentLayout = {
    anchors: [{ name: "pricing", x: 40, y: 1000, w: 800, h: 400 }],
    doc: { w: 1280, h: 4000 },
    scroll: { x: 0, y: 900 },
    viewport: { w: 1280, h: 800 },
  };

  test("on a part, at its top end corner, inside it however small it is", () => {
    expect(partPin({ x: 40, y: 100, w: 800, h: 400 })).toEqual({
      x: 40 + 800 - PIN_INSET,
      y: 100 + PIN_INSET,
    });
    expect(partPin({ x: 0, y: 0, w: 20, h: 10 })).toEqual({ x: 10, y: 10 });
  });

  test("on a page, in the frame's viewport: the document's pixels less its scroll", () => {
    expect(anchorBox(layout, "pricing")).toEqual({ x: 40, y: 100, w: 800, h: 400 });
    expect(pagePin(layout, SECTION)).toEqual({ x: 40 + 800 - PIN_INSET, y: 100 + PIN_INSET });
    expect(
      pagePin(layout, { kind: "html", page: "index.html", point: { x: 0.5, y: 0.25 } }),
    ).toEqual({ x: 640, y: 100 });
    expect(pagePin(layout, { kind: "html", page: "index.html", name: "gone" })).toBeNull();
    expect(anchorBox(layout, "gone")).toBeNull();
  });

  const boards = [
    { id: "boards/home.html", x: 0, y: 0, w: 1440, h: 900 },
    { id: "boards/plan.html", x: 1600, y: 0, w: 390, h: 844 },
    { id: "boards/over.html", x: 1500, y: 100, w: 400, h: 100 },
  ];

  test("on a board, at its corner plus the anchor's point in the board's own pixels", () => {
    expect(
      boardWorld(boards, { kind: "design", board: "boards/plan.html", x: 195, y: 760 }),
    ).toEqual({ x: 1795, y: 760 });
    expect(boardWorld(boards, { kind: "design", board: "boards/home.html" })).toEqual({
      x: 0,
      y: 0,
    });
    expect(
      boardWorld(boards, { kind: "design", board: "boards/gone.html", x: 1, y: 1 }),
    ).toBeNull();
  });

  test("a click on the canvas is a point on the topmost board under it, and none off every board", () => {
    expect(boardAnchorAt(boards, { x: 100.7, y: 50.2 })).toEqual({
      kind: "design",
      board: "boards/home.html",
      x: 100,
      y: 50,
    });
    expect(boardAnchorAt(boards, { x: 1650, y: 150 })).toEqual({
      kind: "design",
      board: "boards/over.html",
      x: 150,
      y: 50,
    });
    expect(boardAnchorAt(boards, { x: 1500, y: 950 })).toBeNull();
    expect(boardAnchorAt(boards, { x: 1440, y: 10 })).toBeNull();
  });
});
