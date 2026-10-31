// @vitest-environment node

/**
 * Where the assistant is a column, at every density, and what its track does
 * to everything else in the frame.
 *
 * EACH THRESHOLD IS HELD AGAINST A SUM WALKED TERM BY TERM HERE, rather than
 * through the module's own helpers, so a term dropped from `layout.ts` is a
 * list under its floor in this sum: at the column's threshold the list keeps
 * at least its floor, and one pixel narrower it would not.
 */

import { describe, expect, test } from "vitest";
import { density, size, spacing } from "@crewlethq/tokens";
import {
  assistantCounted,
  ASSISTANT_MAX,
  ASSISTANT_MIN,
  ASSISTANT_WIDTH,
  CONTENT_PADDING,
  LIST_FLOOR,
  PEEK_WIDTH,
  PHONE_BREAKPOINT,
  SCROLLBAR_GUTTER,
  SECTION_COLUMN_MIN,
  assistantColumnMin,
  assistantCoverEnd,
  assistantMaxWidth,
  assistantOccupied,
  assistantShape,
  assistantTrack,
  columnWidth,
  densityScale,
  peekColumnMin,
  sectionColumnMin,
  sheetFloor,
} from "./layout.ts";

const rail = Number.parseFloat(size.shell.rail);
const inset = Number.parseFloat(size.shell.inset);
const SCALES = (Object.keys(density) as (keyof typeof density)[]).map(
  (d) => [d, densityScale(d)] as const,
);

/** What a list is left at a window `width` with the assistant a column at its resting width. */
function listBesideAssistant(width: number, scale: number, column: number): number {
  return (
    width -
    rail -
    inset * scale - // the seam: the page sheet's end inset
    2 - // the page sheet's hairlines
    ASSISTANT_WIDTH -
    inset * scale - // the inset holding the assistant off the window's edge
    SCROLLBAR_GUTTER -
    2 * CONTENT_PADDING * scale -
    column
  );
}

describe("the assistant's shape", () => {
  test("is a column from 1168px on a screen at the normal density, 1404 beside Settings' column", () => {
    expect(assistantColumnMin({ column: 0, scale: densityScale("normal") })).toBe(1168);
    expect(
      assistantColumnMin({ column: columnWidth("column"), scale: densityScale("normal") }),
    ).toBe(1404);
  });

  test("at every density, the list keeps its floor at the threshold and not a pixel under it", () => {
    for (const [name, scale] of SCALES) {
      for (const column of [0, columnWidth("column")]) {
        const at = assistantColumnMin({ column, scale });
        expect(
          listBesideAssistant(at, scale, column),
          `${name}, column ${column}`,
        ).toBeGreaterThanOrEqual(LIST_FLOOR);
        expect(
          listBesideAssistant(at - 1, scale, column),
          `${name}, column ${column}`,
        ).toBeLessThan(LIST_FLOOR);
      }
    }
  });

  test("is a column, an overlay or the whole window by the window's width alone", () => {
    const frame = { column: 0, scale: densityScale("normal") };
    expect(assistantShape(PHONE_BREAKPOINT - 1, frame)).toBe("sheet");
    expect(assistantShape(PHONE_BREAKPOINT, frame)).toBe("overlay");
    expect(assistantShape(1167, frame)).toBe("overlay");
    expect(assistantShape(1168, frame)).toBe("column");
  });
});

describe("the assistant's track, as one more term of every other width", () => {
  const scale = densityScale("normal");
  const track = assistantTrack(ASSISTANT_WIDTH, scale);

  test("a peek beside an open assistant becomes a drawer first: both are columns only from 1588", () => {
    expect(peekColumnMin({ column: 0, scale })).toBe(1160);
    expect(peekColumnMin({ column: 0, scale, assistant: track })).toBe(1588);
    // At 1440 the assistant is a column and the peek a drawer.
    expect(1440 >= assistantColumnMin({ column: 0, scale })).toBe(true);
    expect(1440 >= peekColumnMin({ column: 0, scale, assistant: track })).toBe(false);
  });

  test("Settings' column folds to its picker sooner while the assistant is a column", () => {
    expect(sectionColumnMin(0)).toBe(SECTION_COLUMN_MIN);
    expect(sectionColumnMin(track)).toBe(SECTION_COLUMN_MIN + ASSISTANT_WIDTH + inset);
  });

  test("every threshold counts the width the reader dragged, inside the drag's range", () => {
    expect(assistantCounted(null)).toBe(ASSISTANT_WIDTH);
    expect(assistantCounted(640)).toBe(640);
    expect(assistantCounted(9999)).toBe(ASSISTANT_MAX);
    expect(assistantCounted(10)).toBe(ASSISTANT_MIN);
    // Beside a 640 panel at 1700, Settings folds: counting the resting 420
    // kept the column over a section narrower than its floor.
    const dragged = assistantTrack(assistantCounted(640), scale);
    expect(1700 >= sectionColumnMin(track)).toBe(true);
    expect(1700 >= sectionColumnMin(dragged)).toBe(false);
  });

  test("the page's sheet keeps the list, and a peek column where there is one", () => {
    const frame = { column: 0, scale };
    expect(sheetFloor(frame)).toBe(inset + 2 + SCROLLBAR_GUTTER + 2 * CONTENT_PADDING + LIST_FLOOR);
    expect(sheetFloor(frame, true) - sheetFloor(frame)).toBe(PEEK_WIDTH);
  });
});

describe("how far a reader may drag it", () => {
  const frame = { column: 0, scale: densityScale("normal") };

  test("to the widest step on a wide window", () => {
    expect(assistantMaxWidth(1920, frame)).toBe(ASSISTANT_MAX);
  });

  test("only as far as leaves the page's sheet its floor", () => {
    expect(assistantMaxWidth(1300, frame)).toBe(1300 - rail - sheetFloor(frame) - inset);
    expect(assistantMaxWidth(1300, frame)).toBeLessThan(ASSISTANT_MAX);
  });

  test("never under the narrowest, where a column would not have been chosen", () => {
    expect(assistantMaxWidth(900, frame)).toBe(ASSISTANT_MIN);
  });

  test("the resting width sits inside the range a reader may drag through", () => {
    expect(ASSISTANT_MIN).toBeLessThanOrEqual(ASSISTANT_WIDTH);
    expect(ASSISTANT_WIDTH).toBeLessThanOrEqual(ASSISTANT_MAX);
    expect([ASSISTANT_MIN, ASSISTANT_WIDTH, ASSISTANT_MAX]).toEqual([360, 420, 640]);
  });
});

describe("what the assistant takes from the window's end edge, for the toast stack", () => {
  const scale = densityScale("normal");

  test("a column or an overlay takes its width and its inset; a sheet takes none", () => {
    expect(assistantOccupied("column", 420, 1440, scale)).toBe(420 + inset);
    expect(assistantOccupied("overlay", 420, 1100, scale)).toBe(420 + inset);
    expect(assistantOccupied("sheet", 420, 390, scale)).toBe(0);
  });

  test("an overlay on a narrow window is drawn narrower than its width, and takes only that", () => {
    // The kit leaves a strip of the page (`spacing.10`) and both insets in view.
    const strip = Number.parseFloat(spacing["10"]);
    expect(assistantOccupied("overlay", 640, 700, scale)).toBe(700 - strip - 2 * inset + inset);
  });
});

// A SCREEN'S END EDGE UNDER THE OVERLAY: what Chatlet's pane keeps clear of.
// A column pushes the page narrower and a sheet is modal, so neither covers
// any of it.
describe("how much of the page's end the assistant stands over", () => {
  const scale = densityScale("normal");

  test("an overlay covers what it occupies from the window's edge; a column and a sheet none", () => {
    expect(assistantCoverEnd("overlay", 420, 1100, scale)).toBe(420 + inset);
    expect(assistantCoverEnd("column", 420, 1440, scale)).toBe(0);
    expect(assistantCoverEnd("sheet", 420, 390, scale)).toBe(0);
  });

  test("an overlay drawn narrower on a narrow window covers only what it draws", () => {
    // THE SAME PANEL THE TOAST STACK STANDS BESIDE, measured the same way.
    expect(assistantCoverEnd("overlay", 640, 700, scale)).toBe(
      assistantOccupied("overlay", 640, 700, scale),
    );
  });
});
