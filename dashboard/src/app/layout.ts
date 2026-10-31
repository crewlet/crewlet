/**
 * The frame's breakpoints: READ from the kit's tokens, and the widths the
 * dashboard derives for itself: where the peek, the assistant, Settings'
 * column and Chatlet's columns each change shape.
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
 * # Where the peek becomes a column
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
 *     + 236  Settings' section column, where it stands beside the section
 *            ([SECTION_COLUMN_MIN]; below that it folds to a picker)
 *     + 444  the list
 *
 * = 1160 on a screen and 1396 beside Settings' column, at the normal density.
 *
 * The first cut of this counted only the first four terms and the list, and
 * the three it missed are each wide enough to matter: at the 1112 it derived,
 * the Worklet list beside a peek was 396px (under the floor it existed to
 * protect), and on Settings, whose column sits inside the screen, a peek at
 * 1280 left the nodes grid 328px and cut its columns off.
 *
 * # Why the SHELL decides, and not a media query
 *
 * Two of those terms are not the window's to know. The section column is a
 * property of the SCREEN (a media query has one width for every screen, so
 * one right for Worklet is 236px wrong for Settings), and the padding and the
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
import { useSyncExternalStore } from "react";
import { useMediaMatch } from "@crewlethq/ui";
import type { SectionRenderer } from "./nav.ts";

const px = (value: string): number => Number.parseFloat(value);

/** Below this the sidebar is a drawer and the sheet is the window. */
export const SHELL_BREAKPOINT = px(breakpoint.shell);

/** Below this a layout is single-pane and a grid's rows are cards. */
export const PHONE_BREAKPOINT = px(breakpoint.phone);

/**
 * A reader whose PRIMARY INPUT IS TOUCH — no hover, a coarse pointer: a phone,
 * or a tablet held as one. Not a width: a narrow window on a laptop still has
 * a keyboard, and a wide tablet still has none.
 *
 * A KEYCAP IS A HINT FOR A KEYBOARD, so the frame draws none here
 * ([useKeyHints]): the drawer's search drew "Ctrl + K" on a
 * phone, a key the reader does not have, and the keycap took the room its own
 * label needed. Asked this way round — "is it touch?" rather than "is there a
 * keyboard?" — because no media feature can see a keyboard, and a browser
 * that answers neither question keeps its hints.
 */
export const TOUCH_MEDIA = "(hover: none) and (pointer: coarse)";

/**
 * Whether the frame draws its KEYCAP hints: everywhere but where the reader's
 * primary input is touch ([TOUCH_MEDIA]), where a keycap names a key the
 * reader does not have. The keys still work for a reader who attaches a
 * keyboard; only the hint is withheld.
 */
export function useKeyHints(): boolean {
  return !useMediaMatch(TOUCH_MEDIA);
}

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
 * The narrowest TRANSCRIPT Chatlet draws beside its pinned Rooms panel or an
 * open pane: the width WCAG 2.1's Reflow criterion (1.4.10) asks every page to
 * read at without scrolling sideways. A transcript is text that wraps, and a
 * phone already draws a room about this wide; a column that would leave it
 * less lies over the conversation instead ([CHAT_PANE_FOLD], [CHAT_NARROW]).
 */
export const TRANSCRIPT_FLOOR = 320;

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
 * `.crewlet-section-frame__body`'s beside the Settings column, which takes the kit's back
 * and gives the same to the section.
 */
export const CONTENT_PADDING = px(spacing["5"]);

/** The sheet's two hairlines, one each side. */
const SHEET_HAIRLINES = 2;

/** The density a preference names, as the multiplier the kit scales by. */
export function densityScale(choice: keyof typeof density): number {
  return Number(density[choice]);
}

/** How wide the column a workspace draws beside its screens is, if it draws one. */
export function columnWidth(renderer: SectionRenderer | undefined): number {
  switch (renderer) {
    case "column":
      return px(size.shell.rail);
    default:
      return 0;
  }
}

/**
 * The narrowest a Settings section is drawn BESIDE its column: room for a
 * grid of six facts and a line of detail (the Audit log's), or a row of four
 * stat tiles, without cutting a value short. Measured: at 1024 the column
 * left a 489px section, and the audit's When, Where, What and To columns were
 * drawn 46px wide while its stat tiles read "Not s…".
 */
export const SECTION_BODY_FLOOR = 720;

/**
 * The narrowest window Settings' column stands beside its section at; below
 * it the column folds to the one-row picker the phone draws, and the section
 * takes the screen's whole width. Every width between the window's edge and
 * the section, at the normal density (a media query cannot read the
 * reader's), as [peekColumnMin] counts them: the sidebar, the sheet's inset
 * and hairlines, the scroller's gutter, the column, and the section's floor.
 * `styles/frame.css` writes it as a literal, held here by `frame.test.ts`.
 */
export const SECTION_COLUMN_MIN = Math.ceil(
  px(size.shell.rail) +
    px(size.shell.inset) +
    SHEET_HAIRLINES +
    SCROLLBAR_GUTTER +
    columnWidth("column") +
    SECTION_BODY_FLOOR,
);

/** Which frame a screen sits in, for the terms that differ. */
export interface Frame {
  /**
   * The width of the column drawn inside the screen beside the list —
   * Settings' sections, or 0 where there is none
   * ([columnWidth]).
   */
  column: number;
  /** The reader's density multiplier ([densityScale]). */
  scale: number;
  /**
   * What the assistant takes from the window while it is a COLUMN of the
   * frame ([assistantTrack]): its width and the inset it is held off the
   * window's edge by. Absent, or 0, while it is closed or stands over the
   * page, where it takes no track.
   */
  assistant?: number;
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
      listReserve(frame) +
      (frame.assistant ?? 0),
  );
}

/*
 * # The assistant: a third track of the frame
 *
 * The assistant is a second sheet on the frame, to the right of the page's,
 * mirroring the rail (the kit's `AppShell` `aside`). While it is a COLUMN the
 * window holds, at the reader's density:
 *
 *       236  the sidebar                          (`size.shell.rail`)
 *     +   8  the seam: the page sheet's end inset (`size.shell.inset`, x density)
 *     +   2  the page sheet's two hairlines
 *     + 494  the list's reserve ([listReserve]: gutter, padding, the list)
 *     + 420  the assistant
 *     +   8  the inset it is held off the window's edge by
 *
 * = 1168 on a screen and 1404 beside Settings' column, at the normal density.
 * Below that it stands OVER the page's end edge (an overlay, not a modal: the
 * page beside it stays live, with no veil and no Tab trap), and below the
 * phone breakpoint it is the whole window (a sheet, which is a modal).
 *
 * A PEEK BESIDE AN OPEN ASSISTANT BECOMES A DRAWER FIRST: the assistant's
 * track is one more term of [peekColumnMin], so both are columns only where
 * the window holds both and the list at its floor (1588 on a screen). The
 * reader opened the assistant to talk about the list, and the list keeps its
 * floor. And Settings' column folds to its picker sooner, by the same track
 * ([sectionColumnMin]).
 */

/** The assistant's resting width, and the range a reader may drag it through. */
export const ASSISTANT_WIDTH = 420;
export const ASSISTANT_MIN = 360;
export const ASSISTANT_MAX = 640;

/** Which shape the assistant takes: a column of the frame, over the page, or the whole window. */
export type AssistantShape = "column" | "overlay" | "sheet";

/**
 * The assistant width every OTHER frame threshold counts: the width the reader
 * dragged it to (kept per browser), or the resting width where they never
 * did, inside the range a drag allows.
 *
 * The thresholds (when Settings' column folds, when a peek becomes a drawer)
 * used to count the resting 420 whatever was drawn, so beside a 640 panel at
 * 1700 Settings kept its column over a 560px section, under the floor that
 * rule exists for. The width drawn is this, capped further by what leaves the
 * page's sheet its floor ([assistantMaxWidth]); the cap can only make it
 * narrower, so a threshold that counts this errs towards folding, never
 * towards a section too narrow to read.
 */
export function assistantCounted(stored: number | null): number {
  return Math.min(ASSISTANT_MAX, Math.max(ASSISTANT_MIN, stored ?? ASSISTANT_WIDTH));
}

/** What the assistant's column takes from the window: its width and its outer inset. */
export function assistantTrack(width: number, scale: number): number {
  return width + px(size.shell.inset) * scale;
}

/**
 * The narrowest the page's SHEET may be beside an assistant column, its seam
 * and hairlines included: what the kit caps the column at
 * (`AppShell` `asideReserve`), so a width dragged on a wide window never
 * takes a narrower one's list away. With a peek column in the sheet, the
 * peek's resting width is the sheet's too.
 */
export function sheetFloor(frame: Frame, peekColumn = false): number {
  return (
    px(size.shell.inset) * frame.scale +
    SHEET_HAIRLINES +
    listReserve(frame) +
    (peekColumn ? PEEK_WIDTH : 0)
  );
}

/**
 * The narrowest window the assistant is a column at, in this frame, at its
 * resting width; below it, an overlay. Rounded UP, because a column a
 * fraction too narrow is the defect.
 */
export function assistantColumnMin(frame: Frame): number {
  return Math.ceil(
    px(size.shell.rail) + sheetFloor(frame) + assistantTrack(ASSISTANT_WIDTH, frame.scale),
  );
}

/** The assistant's shape at a window `width`, in this frame. */
export function assistantShape(width: number, frame: Frame): AssistantShape {
  if (width < PHONE_BREAKPOINT) return "sheet";
  return width >= assistantColumnMin(frame) ? "column" : "overlay";
}

/**
 * The widest a reader may drag the assistant column to at a window `width`:
 * [ASSISTANT_MAX], capped at what leaves the page's sheet its floor. Never
 * under [ASSISTANT_MIN], where the column would not have been chosen.
 */
export function assistantMaxWidth(width: number, frame: Frame, peekColumn = false): number {
  const room =
    width -
    px(size.shell.rail) -
    sheetFloor(frame, peekColumn) -
    px(size.shell.inset) * frame.scale;
  return Math.max(ASSISTANT_MIN, Math.min(ASSISTANT_MAX, Math.floor(room)));
}

/**
 * What the assistant takes from the window's end edge while it is a column
 * or stands over the page, so the toast stack stands beside it rather than
 * over it: its width (an overlay's is capped at the window less a strip of
 * the page, as the kit draws it) and the inset holding it off the edge. A
 * sheet is the whole window and takes the stack's foot instead.
 */
export function assistantOccupied(
  shape: AssistantShape,
  width: number,
  viewport: number,
  scale: number,
): number {
  if (shape === "sheet") return 0;
  const inset = px(size.shell.inset) * scale;
  const drawn =
    shape === "overlay" ? Math.min(width, viewport - px(spacing["10"]) - 2 * inset) : width;
  return Math.max(0, Math.round(drawn + inset));
}

/**
 * The narrowest window Settings' column stands beside its section at, with
 * the assistant's track (if any) taken first: [SECTION_COLUMN_MIN] plus it.
 */
export function sectionColumnMin(assistant: number): number {
  return Math.ceil(SECTION_COLUMN_MIN + assistant);
}

/*
 * # Chatlet: the conversation, the pinned Rooms panel and a pane
 *
 * Chatlet draws up to three columns side by side: the Rooms panel when it is
 * pinned, the conversation, and a pane (a thread, a room's details, a
 * profile, a catch up). They follow the SCREEN'S width, never the window's,
 * because the sidebar, a peek column and the assistant's track each take a
 * share of the window that no media query can see: the stylesheet asks the
 * page container (`@container page`), and the frame measures the chat area
 * itself for the one decision a stylesheet cannot make, whether the panel may
 * be pinned at all (`routes/chatlet/Chat.tsx`).
 *
 * The conversation takes what is left, and keeps [TRANSCRIPT_FLOOR] at every
 * step:
 *
 *       1180 and over   panel 280 + pane 400   leave the transcript 500
 *       980 to 1180     panel 248 + pane 340   leave it 392 at 980
 *       760 to 980      panel 248, the pane OVER the conversation, leave 512 at 760
 *       under 760       the panel slides in and the pane covers the conversation
 *
 * so no fold leaves the transcript under its floor beside what stands next
 * to it. `styles/screenwidth.test.ts` holds the literals the stylesheet writes
 * against these.
 */

/** The pinned Rooms panel's column, on a roomy screen. */
export const CHAT_DOCK_WIDTH = 280;

/** A pane's column beside the conversation, on a roomy screen. */
export const CHAT_PANE_WIDTH = 400;

/** Below this screen width the panel and the pane take their snug widths. */
export const CHAT_ROOMY = 1180;

/** The pinned panel below [CHAT_ROOMY]. */
export const CHAT_DOCK_SNUG = 248;

/** A pane beside the conversation below [CHAT_ROOMY]. */
export const CHAT_PANE_SNUG = 340;

/** Below this screen width a pane lies over the conversation, not beside it. */
export const CHAT_PANE_FOLD = 980;

/**
 * Below this width of the chat area the Rooms panel is never pinned (it
 * slides in from Menu) and a pane covers the conversation whole.
 */
export const CHAT_NARROW = 760;

/**
 * The window's width, as a value a component re-renders on. For the one
 * figure no media query can hand back: how far a reader may drag a panel.
 */
export function useViewportWidth(): number {
  return useSyncExternalStore(onViewportResize, viewportWidth, () => 0);
}

function viewportWidth(): number {
  return typeof window === "undefined" ? 0 : window.innerWidth;
}

function onViewportResize(notify: () => void): () => void {
  window.addEventListener("resize", notify);
  return () => window.removeEventListener("resize", notify);
}
