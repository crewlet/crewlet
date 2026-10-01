/**
 * The frame's breakpoints: READ from the kit's tokens, and the one width the
 * dashboard derives for itself.
 *
 * # Read, never redeclared
 *
 * `breakpoint.shell` (1024) is where the sheet goes full-bleed and the
 * sidebar becomes a drawer — the kit's `AppShell` switches there on its own —
 * and `breakpoint.phone` (640) is where a layout goes single-pane and a
 * `DataGrid`'s rows become cards. Both are the kit's numbers, and a copy of
 * either here would be the second place a breakpoint lives, which is how a
 * drawer comes to open at one width while the grid folds at another. A media
 * query cannot read a custom property, so the stylesheet must write the
 * literal; `styles/frame.test.ts` holds every literal it writes against the
 * values here.
 *
 * # The one width that is ours: where the peek becomes a column
 *
 * The peek is a column of the sheet while the window can hold everything in
 * front of the list, the peek, AND a list still wide enough to read beside
 * it; below that it is a drawer over the screen. The LIST FLOOR is what was
 * measured — 444px is under the width a work row needs for a key, a title
 * and an assignee — and the threshold is arithmetic over it, counting EVERY
 * width between the window's edge and the list's own content box:
 *
 *       236  the sidebar                     (`size.shell.rail`)
 *     +   8  the sheet's inset, x density    (`size.shell.inset`)
 *     +   2  the sheet's two hairlines
 *     + 420  the peek
 *     +  10  the scroller's reserved gutter  (`scrollbar-gutter: stable`)
 *     +  40  the screen's padding, x density (`spacing.5` each side)
 *     + 236  Settings' section column, on a screen that draws one —
 *            or 256, Knowledge's tree ([TREE_COLUMN])
 *     + 444  the list
 *
 * = 1160 on a screen, 1396 beside Settings' column and 1416 beside
 * Knowledge's tree, at the normal density.
 *
 * The first cut of this counted only the first four terms and the list, and
 * the three it missed are each wide enough to matter: at the 1112 it derived,
 * the Work list beside a peek was 396px — under the floor it existed to
 * protect — and on Settings, whose column sits inside the screen, a peek at
 * 1280 left the nodes grid 328px and cut its columns off.
 *
 * # Why the SHELL decides, and not a media query
 *
 * Two of those terms are not the window's to know. The section column is a
 * property of the SCREEN — a media query has one width for every screen, so
 * one right for Work is 236px wrong for Settings — and the padding and the
 * inset scale with the reader's DENSITY, which a media query can no more read
 * than it can read a custom property. The last cut wrote its threshold at the
 * widest density for that reason, which was safe and still wrong for the
 * column. The shell knows both, so it asks `matchMedia` for the one width
 * that is right for the frame on screen now ([peekColumnMin]) and publishes
 * which shape the peek takes as ONE attribute — so a width with a column AND
 * a drawer, or neither, cannot be written at all.
 *
 * # And the list keeps its floor at every width above it
 *
 * A reader may drag the peek to 640, and a stored width outlives the window
 * it was dragged in — 640 at 1200 would give the list back 224px. So the
 * column's track is the reader's width CAPPED at what the sheet can give it
 * with the list at its floor ([listReserve], published to the stylesheet as
 * `--peek-reserve`): the threshold is where the resting width fits, and the
 * cap is what keeps a wider one from undoing it.
 */

import { breakpoint, density, size, spacing } from "@crewlethq/tokens";
import type { SectionRenderer } from "./nav.ts";

const px = (value: string): number => Number.parseFloat(value);

/** Below this the sidebar is a drawer and the sheet is the window. */
export const SHELL_BREAKPOINT = px(breakpoint.shell);

/** Below this a layout is single-pane and a grid's rows are cards. */
export const PHONE_BREAKPOINT = px(breakpoint.phone);

/** The peek's resting width, and the range a reader may drag it through. */
export const PEEK_WIDTH = 420;
export const PEEK_MAX = 640;

/**
 * The peek's resting width beside a CANVAS — the approved org chart's 330.
 *
 * A list beside a peek keeps its floor and reads fine at 420; a chart does
 * not have a floor, it has a SIZE, and every pixel the peek takes is a pixel
 * the whole company is shrunk into. At 420 the Nimbus chart beside a peek was
 * drawn at 71% on a 1440 window and 54% on a 1280 one, its state lines at
 * seven pixels; at the artboard's 330, with the field running to the sheet's
 * edges, it is drawn at 94% on 1440. A screen asks for it with
 * `usePeekWidth` (`app/frame/peekWidth.ts`).
 */
export const CANVAS_PEEK_WIDTH = 330;

/**
 * The narrowest a reader may drag the peek: the narrowest any screen rests it
 * at, so a width a canvas screen rests at is a width a reader can also choose
 * — and one they chose there is kept rather than refused on the next load.
 */
export const PEEK_MIN = CANVAS_PEEK_WIDTH;

/** The narrowest list a peek may stand beside: a key, a title, an assignee. */
export const LIST_FLOOR = 444;

/**
 * The width the screen's scroller keeps for its scrollbar, whether or not
 * one is drawn (`scrollbar-gutter: stable`, the kit's): the width
 * `styles/base.css` gives `::-webkit-scrollbar`, which is what Chromium and
 * Safari draw. Firefox draws its `thin` bar at 8, so this is the wider of the
 * two; a browser with overlay scrollbars keeps none, and loses nothing by a
 * threshold ten pixels early. `frame.test.ts` holds it against the sheet.
 */
export const SCROLLBAR_GUTTER = 10;

/**
 * What a screen's content keeps on EACH side before the list starts, at the
 * normal density: the kit's `.crewlet-app-shell__content` padding, and
 * `.section-body`'s beside the Settings column, which takes the kit's back
 * and gives the same to the section.
 */
export const CONTENT_PADDING = px(spacing["5"]);

/** The sheet's two hairlines, one each side. */
const SHEET_HAIRLINES = 2;

/** The density a preference names, as the multiplier the kit scales by. */
export function densityScale(choice: keyof typeof density): number {
  return Number(density[choice]);
}

/**
 * Knowledge's tree column: the artboard's 256.
 *
 * WIDER THAN SETTINGS' COLUMN, deliberately. Settings lists nine sections at
 * one level; the tree nests a space, a folder and a page under each other,
 * each level indented, with a space's key on the right — and at the sidebar's
 * 236 a third-level title had under 120px before it was cut.
 */
export const TREE_COLUMN = 256;

/** How wide the column a workspace draws beside its screens is, if it draws one. */
export function columnWidth(renderer: SectionRenderer | undefined): number {
  switch (renderer) {
    case "column":
      return px(size.shell.rail);
    case "tree":
      return TREE_COLUMN;
    default:
      return 0;
  }
}

/** Which frame a screen sits in, for the one term that differs. */
export interface Frame {
  /**
   * The width of the column drawn inside the screen beside the list —
   * Settings' sections, Knowledge's tree — or 0 where there is none
   * ([columnWidth]).
   */
  column: number;
  /** The reader's density multiplier ([densityScale]). */
  scale: number;
}

/**
 * Everything the SHEET keeps beside the peek column with the list at its
 * floor: the scroller's gutter, the screen's padding, the section column
 * where there is one, and the list. The peek's track is capped at the sheet
 * less this, which is what `--peek-reserve` carries.
 */
export function listReserve({ column, scale }: Frame): number {
  return SCROLLBAR_GUTTER + 2 * CONTENT_PADDING * scale + column + LIST_FLOOR;
}

/**
 * The narrowest window the peek is a column at, in this frame; below it the
 * peek is a drawer. Rounded UP to a whole pixel, because the density makes
 * the sum fractional and a column a fraction too narrow is the defect.
 */
export function peekColumnMin(frame: Frame): number {
  return Math.ceil(
    px(size.shell.rail) +
      px(size.shell.inset) * frame.scale +
      SHEET_HAIRLINES +
      PEEK_WIDTH +
      listReserve(frame),
  );
}
