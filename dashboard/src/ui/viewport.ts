/**
 * The geometry of a pannable, zoomable viewport, as pure functions.
 *
 * WHY IT IS SEPARATE FROM THE CANVAS. Every rule here is arithmetic that a
 * browser test cannot reach: jsdom has no layout, no pointer capture and no
 * `DOMMatrix`, so a zoom that drifted off the cursor or a fit that cropped the
 * root would pass any component suite. Kept pure, each rule is a function of
 * numbers with a node-environment suite behind it, and the canvas is left
 * with only the wiring.
 *
 * THE MODEL. A view is `{ x, y, k }`: a world point `p` lands on screen at
 * `p * k + (x, y)`, with the origin at the viewport's top-left corner. Every
 * function returns a new view and never mutates one.
 *
 * THE RULES IT KEEPS.
 *
 * - A ZOOM KEEPS ITS FOCUS STILL. The world point under the cursor (or the
 *   pinch midpoint, or the viewport centre for a key) stays where it was,
 *   including when the zoom is clamped at a limit.
 * - CONTENT CANNOT BE LOST. A pan leaves at least a margin of the content on
 *   screen on each axis, so nobody drags the chart away and has nothing left
 *   to drag it back by.
 * - A FIT NEVER ENLARGES past one to one, and when the content is too tall to
 *   fit even at the smallest zoom it aligns to the TOP, where a tree's root is,
 *   rather than centring the root off screen.
 * - A REVEAL MOVES AS LITTLE AS IT CAN: nothing when the target is already in
 *   view, otherwise just enough to bring it inside the padding.
 */

export interface Point {
  x: number;
  y: number;
}

export interface Size {
  width: number;
  height: number;
}

export interface Rect extends Point, Size {}

/** A world point `p` is drawn at `p * k + (x, y)`. */
export interface View {
  x: number;
  y: number;
  k: number;
}

export const IDENTITY: View = { x: 0, y: 0, k: 1 };

/**
 * The event an overlay layer receives when the content beneath it moves.
 *
 * A popup positioned from an item's viewport rectangle has to follow that item
 * through a pan or a zoom, and listening on the layer it was rendered into is
 * the whole contract: the popup never needs to know what kind of surface drew
 * the layer.
 */
export const VIEW_CHANGE_EVENT = "crewlet:viewchange";

/**
 * The smallest zoom. At a quarter size body text is about three pixels tall:
 * past reading, but the shape of the structure and where a card sits in it
 * are still legible, which is what zooming out is for. Below it a large tree
 * is a smear of rectangles that tells nobody more than a fit does.
 */
export const MIN_ZOOM = 0.25;

/**
 * The largest zoom. At double size body text is larger than the page's own
 * screen titles; past that a zoom only magnifies pixels, and a stray pinch
 * leaves a single card filling the viewport with nothing to navigate by.
 */
export const MAX_ZOOM = 2;

/** A fit shows everything at no more than actual size: a small tree is not blown up. */
export const FIT_MAX_ZOOM = 1;

/**
 * One zoom step, for a key press or a button: a fifth. Small enough that two
 * presses are a deliberate change rather than a jump, large enough that going
 * from the smallest zoom to actual size takes eight presses, not twenty.
 */
export const ZOOM_STEP = 1.2;

/**
 * Wheel zoom per pixel of wheel delta, chosen so that one notch of a mouse
 * wheel (which browsers report as about 100 pixels) is exactly one
 * `ZOOM_STEP`: a wheel notch and a key press do the same thing. A trackpad
 * pinch reports small deltas many times a second, which this turns into a
 * smooth zoom.
 */
export const WHEEL_ZOOM_PER_PIXEL = Math.log2(ZOOM_STEP) / 100;

/** What a wheel "line" is in pixels, for devices that report lines (Firefox on some platforms). */
export const WHEEL_LINE_PX = 16;

/**
 * How much content stays on screen however far it is panned: about one card
 * edge, which is enough to see where the chart went and to grab it back.
 */
export const PAN_MARGIN = 48;

/** Space kept around the content by a fit, so the outermost cards do not touch the edge. */
export const FIT_PADDING = 24;

/** Space kept around a revealed item, so focus never lands on a card cut by the edge. */
export const REVEAL_PADDING = 24;

/** How far an arrow key pans a focused viewport: roughly half a card, so a press is visible. */
export const PAN_STEP = 64;

/**
 * How far a press may travel and still be a click or a tap rather than a
 * drag. A mouse is precise; a finger rolls several pixels while it rests, and a
 * slop sized for a mouse would turn every tap into a tiny pan.
 */
export const TAP_SLOP = { mouse: 4, pen: 4, touch: 10 } as const;

export function clampZoom(k: number): number {
  return Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, k));
}

export function toScreen(view: View, p: Point): Point {
  return { x: p.x * view.k + view.x, y: p.y * view.k + view.y };
}

export function toWorld(view: View, p: Point): Point {
  return { x: (p.x - view.x) / view.k, y: (p.y - view.y) / view.k };
}

/** A world rectangle as it is drawn, in viewport coordinates. */
export function screenRect(view: View, r: Rect): Rect {
  const at = toScreen(view, r);
  return { x: at.x, y: at.y, width: r.width * view.k, height: r.height * view.k };
}

export function panBy(view: View, dx: number, dy: number): View {
  return { x: view.x + dx, y: view.y + dy, k: view.k };
}

/** Zoom by `factor` about `focus` (a viewport point), which stays still. */
export function zoomAt(view: View, factor: number, focus: Point): View {
  const k = clampZoom(view.k * factor);
  const ratio = k / view.k;
  return {
    x: focus.x - (focus.x - view.x) * ratio,
    y: focus.y - (focus.y - view.y) * ratio,
    k,
  };
}

/**
 * The same view, panned no further than leaves `margin` of the content on
 * screen on each axis. A view that already satisfies it comes back unchanged.
 */
export function clampPan(view: View, content: Rect, viewport: Size, margin = PAN_MARGIN): View {
  const axis = (offset: number, start: number, length: number, room: number): number => {
    const drawn = length * view.k;
    // Never demand more margin than the content or the viewport has.
    const m = Math.max(0, Math.min(margin, drawn, room));
    const lowest = m - (start + length) * view.k;
    const highest = room - m - start * view.k;
    return Math.min(highest, Math.max(lowest, offset));
  };
  return {
    x: axis(view.x, content.x, content.width, viewport.width),
    y: axis(view.y, content.y, content.height, viewport.height),
    k: view.k,
  };
}

/** The view that shows all of `content`, centred, at no more than actual size. */
export function fit(content: Rect, viewport: Size, padding = FIT_PADDING): View {
  if (viewport.width <= 0 || viewport.height <= 0) return IDENTITY;
  const roomX = Math.max(0, viewport.width - 2 * padding);
  const roomY = Math.max(0, viewport.height - 2 * padding);
  const scaleX = content.width > 0 ? roomX / content.width : Infinity;
  const scaleY = content.height > 0 ? roomY / content.height : Infinity;
  const k = Math.max(MIN_ZOOM, Math.min(FIT_MAX_ZOOM, scaleX, scaleY));
  const drawnHeight = content.height * k;
  const x = (viewport.width - content.width * k) / 2 - content.x * k;
  // TOP-ALIGNED when it cannot fit: see the module doc.
  const y =
    drawnHeight <= roomY
      ? (viewport.height - drawnHeight) / 2 - content.y * k
      : padding - content.y * k;
  return { x, y, k };
}

/** The smallest pan that brings world rectangle `target` inside the padded viewport. */
export function reveal(view: View, target: Rect, viewport: Size, padding = REVEAL_PADDING): View {
  const drawn = screenRect(view, target);
  const axis = (offset: number, start: number, length: number, room: number): number => {
    const low = padding;
    const high = room - padding;
    // Larger than the room: show its start, which is where its label is.
    if (length > high - low) return offset + (low - start);
    if (start < low) return offset + (low - start);
    if (start + length > high) return offset - (start + length - high);
    return offset;
  };
  return {
    x: axis(view.x, drawn.x, drawn.width, viewport.width),
    y: axis(view.y, drawn.y, drawn.height, viewport.height),
    k: view.k,
  };
}

/**
 * The view that draws world point `after` where `before` was drawn.
 *
 * What keeps a node still on screen when a relayout moves it in the world: a
 * card grows, its siblings shift, and the one the operator just acted on stays
 * under their eyes instead of jumping away.
 */
export function anchor(view: View, before: Point, after: Point): View {
  return panBy(view, (before.x - after.x) * view.k, (before.y - after.y) * view.k);
}

/** Where a two-pointer gesture began. */
export interface PinchStart {
  view: View;
  a: Point;
  b: Point;
}

/**
 * The view for a pinch whose pointers have moved from `start` to `a` and `b`.
 *
 * The spread scales the zoom, and the world point under the starting midpoint
 * follows the current midpoint, so one gesture zooms and pans together the way
 * every touch map does.
 */
export function pinch(start: PinchStart, a: Point, b: Point): View {
  const spread = (p: Point, q: Point) => Math.hypot(p.x - q.x, p.y - q.y);
  const was = spread(start.a, start.b);
  // Two pointers on one spot have no spread to scale by.
  const factor = was < 1 ? 1 : spread(a, b) / was;
  const k = clampZoom(start.view.k * factor);
  const from = { x: (start.a.x + start.b.x) / 2, y: (start.a.y + start.b.y) / 2 };
  const to = { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
  const world = toWorld(start.view, from);
  return { x: to.x - world.x * k, y: to.y - world.y * k, k };
}

/** The minimal slice of a WheelEvent these functions read. */
export interface WheelLike {
  deltaX: number;
  deltaY: number;
  /** 0 pixels, 1 lines, 2 pages. */
  deltaMode: number;
  shiftKey?: boolean;
}

/**
 * A wheel event's movement in pixels.
 *
 * Normalised across delta modes, and with Shift turning a vertical wheel into
 * a horizontal pan on platforms that do not already do that themselves.
 */
export function wheelPixels(e: WheelLike, pageHeight: number): Point {
  const unit = e.deltaMode === 1 ? WHEEL_LINE_PX : e.deltaMode === 2 ? pageHeight : 1;
  const dx = e.deltaX * unit;
  const dy = e.deltaY * unit;
  if (e.shiftKey && dx === 0) return { x: dy, y: 0 };
  return { x: dx, y: dy };
}

/** The zoom factor for a vertical wheel movement of `dy` pixels (up zooms in). */
export function wheelZoomFactor(dy: number): number {
  return Math.pow(2, -dy * WHEEL_ZOOM_PER_PIXEL);
}

/** Whether a press that went from `a` to `b` has become a drag. */
export function beyondSlop(a: Point, b: Point, pointerType: string): boolean {
  const slop = TAP_SLOP[pointerType as keyof typeof TAP_SLOP] ?? TAP_SLOP.mouse;
  return Math.hypot(b.x - a.x, b.y - a.y) > slop;
}

/**
 * Where a popup of `size` goes beside `anchor`, inside `bounds`.
 *
 * Below the anchor and aligned to its start by default; above it when there is
 * not room below and there is more room above; and slid back inside the bounds
 * horizontally, so a menu opened from a card at the edge of a canvas is never
 * cut off by the edge it opened near.
 */
export function placePopup(anchorRect: Rect, size: Size, bounds: Rect, gap: number): Point {
  const below = anchorRect.y + anchorRect.height + gap;
  const above = anchorRect.y - gap - size.height;
  const roomBelow = bounds.y + bounds.height - below;
  const roomAbove = anchorRect.y - gap - bounds.y;
  const y = roomBelow >= size.height || roomBelow >= roomAbove ? below : above;
  const maxX = bounds.x + bounds.width - size.width;
  const x = Math.max(bounds.x, Math.min(anchorRect.x, maxX));
  return { x, y };
}
