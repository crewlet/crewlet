/**
 * A bounded, pannable and zoomable viewport over positioned content.
 *
 * WHY IT EXISTS. A hierarchy drawn as a tree grows wider than any screen, and
 * the page's own scroller is the wrong instrument for it: `.screen` is the one
 * element on this dashboard allowed to scroll, and a chart that scrolled it
 * sideways would fight the router's scroll memory and every other screen's
 * layout. So the chart lives in its own viewport, and the geometry of moving
 * around it is `viewport.ts`; this component is the wiring from pointers,
 * wheels and keys to that geometry.
 *
 * THE RULES IT KEEPS.
 *
 * - IT FILLS THE HEIGHT ITS CONTAINER GIVES IT and never grows the page. A
 *   screen that wants a full-height canvas gives it a full-height box.
 * - THE PAGE STILL SCROLLS. A plain wheel pans the canvas only while focus is
 *   inside it; otherwise the wheel belongs to the page, so a reader scrolling
 *   down is never caught by a chart passing under the pointer. Ctrl or Command
 *   with the wheel zooms toward the cursor wherever the pointer is, which is
 *   also what a trackpad pinch sends.
 * - TOUCH IS OPT IN. Until the canvas is tapped, one finger scrolls the page
 *   (`touch-action: pan-y`); a tap activates it, one finger then pans, and a
 *   visible Done control hands the finger back to the page. Two fingers pinch
 *   toward their midpoint at any time, because two fingers are never a scroll.
 * - ZOOM KEYS BELONG TO THE VIEWPORT ITSELF: `+`, `-` and `0` (and Ctrl or
 *   Command with `=`, `-` and `0`) act only while the viewport element holds
 *   focus, and the arrow keys pan it. Focus on an item inside leaves every key
 *   to the item, so a tree's type-ahead and arrow navigation are never eaten.
 * - A FOCUSED ITEM IS NEVER SCROLLED INTO VIEW BEHIND THE TRANSFORM'S BACK.
 *   The viewport clips rather than scrolls, but a browser still scrolls a
 *   clipping box to show a focused descendant; that scroll is converted into
 *   a pan and reset, so the transform stays the one source of truth.
 * - AN OVERLAY LAYER, untransformed, sits over the viewport. Menus and pickers
 *   opened from an item render there (see `useCanvasOverlay`), positioned from
 *   the item's viewport rectangle, so they are neither scaled by the zoom nor
 *   clipped inside a card. The layer receives `VIEW_CHANGE_EVENT` whenever the
 *   content moves beneath it.
 * - FIT HAPPENS ONCE, on the first layout that has both a measured viewport
 *   and content bounds, and again only on request. A data push that changes
 *   the content never moves the operator's view, and nothing is shown before
 *   that first fit.
 * - MOTION ONLY ON REQUEST. A fit, a zoom button or a reveal eases; a drag, a
 *   pinch, a wheel and a relayout anchor never do, and under reduced motion
 *   nothing does.
 *
 * Pointer capture is used where the browser has it and the drag is tracked on
 * the window either way, so a drag that leaves the viewport keeps panning.
 * Fullscreen is not this component's concern: a surface that goes fullscreen
 * must take its dialogs and toasts with it, so the screen that owns those
 * decides.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
  type Ref,
} from "react";
import { Button } from "./primitives.tsx";
import {
  IDENTITY,
  PAN_STEP,
  VIEW_CHANGE_EVENT,
  ZOOM_STEP,
  anchor,
  beyondSlop,
  clampPan,
  fit,
  panBy,
  pinch,
  reveal,
  screenRect,
  wheelPixels,
  wheelZoomFactor,
  zoomAt,
  type PinchStart,
  type Point,
  type Rect,
  type Size,
  type View,
} from "./viewport.ts";

/** What a screen can ask of a canvas it holds a ref to. */
export interface CanvasHandle {
  /** Show all of the content. Eases, unless reduced motion is set. */
  fit(): void;
  /** Zoom about the viewport centre. */
  zoomBy(factor: number): void;
  /** Pan the least distance that brings a world rectangle into view. */
  reveal(target: Rect): void;
  /** Keep a node still on screen across a relayout: see `viewport.anchor`. */
  anchor(before: Point, after: Point): void;
  /** A world rectangle in viewport coordinates, for positioning an overlay. */
  screenRect(target: Rect): Rect;
  /** The current view. */
  view(): View;
}

const OverlayContext = createContext<HTMLElement | null>(null);

/**
 * The untransformed overlay layer of the canvas this is rendered inside, or
 * null outside a canvas (and before the layer has mounted).
 */
export function useCanvasOverlay(): HTMLElement | null {
  return useContext(OverlayContext);
}

/** Controls that take a press themselves, so a press on one never starts a pan. */
const INTERACTIVE = "button, a[href], input, select, textarea, [contenteditable='true'], label";

interface Press {
  kind: "press";
  id: number;
  type: string;
  start: Point;
  view: View;
  dragging: boolean;
}

interface Pinching {
  kind: "pinch";
  start: PinchStart;
  ids: [number, number];
}

export function Canvas({
  label,
  content,
  children,
  overlay,
  controls = true,
  onViewChange,
  ref,
}: {
  /** The accessible name of the viewport, such as "Organization chart". */
  label: string;
  /** The laid-out content's bounds in world units, or null until it has been measured. */
  content: Rect | null;
  /** The positioned content: the layer that is panned and zoomed. */
  children: ReactNode;
  /** Anything to draw over the viewport without the transform. */
  overlay?: ReactNode;
  /** The zoom out, zoom in and fit buttons. */
  controls?: boolean;
  onViewChange?: (view: View) => void;
  ref?: Ref<CanvasHandle>;
}) {
  const helpID = useId();
  const viewport = useRef<HTMLDivElement>(null);
  const root = useRef<HTMLDivElement>(null);
  const [layer, setLayer] = useState<HTMLElement | null>(null);

  const [view, setView] = useState<View>(IDENTITY);
  const [animate, setAnimate] = useState(false);
  const [ready, setReady] = useState(false);
  const [touchActive, setTouchActive] = useState(false);
  const [panning, setPanning] = useState(false);
  const [size, setSize] = useState<Size>({ width: 0, height: 0 });

  // Mirrors of state for handlers registered once. A pan runs at pointer
  // frequency, and re-registering window listeners per frame would drop moves.
  const viewNow = useRef(view);
  const sizeNow = useRef(size);
  const contentNow = useRef(content);
  const touchNow = useRef(touchActive);
  contentNow.current = content;
  touchNow.current = touchActive;
  sizeNow.current = size;

  const apply = useCallback((next: View, eased: boolean) => {
    const bounds = contentNow.current;
    const clamped = bounds ? clampPan(next, bounds, sizeNow.current) : next;
    setAnimate(eased);
    const was = viewNow.current;
    // Unchanged is not a change: a resize or a data push that leaves the view
    // where it was must not tell the overlay that anything moved.
    if (clamped.x === was.x && clamped.y === was.y && clamped.k === was.k) return;
    viewNow.current = clamped;
    setView(clamped);
  }, []);

  // ---- the viewport's own size -------------------------------------------
  useLayoutEffect(() => {
    const el = viewport.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver((entries) => {
      const box = entries[entries.length - 1]?.contentRect;
      if (box) setSize({ width: box.width, height: box.height });
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // ---- fit once, on the first layout that can be fitted --------------------
  useLayoutEffect(() => {
    if (ready || !content || size.width <= 0 || size.height <= 0) return;
    const first = fit(content, size);
    viewNow.current = first;
    setView(first);
    setReady(true);
  }, [ready, content, size]);

  // A resize, or content that changed shape, keeps the view and only makes
  // sure the content is still reachable: a data push never refits.
  useLayoutEffect(() => {
    if (ready) apply(viewNow.current, false);
  }, [ready, size, content, apply]);

  // ---- tell the overlay and the owner that the content moved ---------------
  const notify = useRef(onViewChange);
  notify.current = onViewChange;
  useLayoutEffect(() => {
    layer?.dispatchEvent(new CustomEvent(VIEW_CHANGE_EVENT, { detail: view }));
    notify.current?.(view);
  }, [view, layer]);

  useImperativeHandle(
    ref,
    () => ({
      fit: () => {
        const bounds = contentNow.current;
        if (bounds) apply(fit(bounds, sizeNow.current), true);
      },
      zoomBy: (factor) => {
        const { width, height } = sizeNow.current;
        apply(zoomAt(viewNow.current, factor, { x: width / 2, y: height / 2 }), true);
      },
      reveal: (target) => {
        const next = reveal(viewNow.current, target, sizeNow.current);
        if (next.x !== viewNow.current.x || next.y !== viewNow.current.y) apply(next, true);
      },
      anchor: (before, after) => apply(anchor(viewNow.current, before, after), false),
      screenRect: (target) => screenRect(viewNow.current, target),
      view: () => viewNow.current,
    }),
    [apply],
  );

  // ---- the wheel ------------------------------------------------------------
  // A native, non-passive listener: React's wheel handler is passive and
  // cannot stop the page from zooming or scrolling.
  useEffect(() => {
    const el = viewport.current;
    if (!el) return;
    function onWheel(e: WheelEvent) {
      if (!contentNow.current || !el) return;
      const rect = el.getBoundingClientRect();
      const moved = wheelPixels(e, sizeNow.current.height);
      if (e.ctrlKey || e.metaKey) {
        e.preventDefault();
        const focus = { x: e.clientX - rect.left, y: e.clientY - rect.top };
        apply(zoomAt(viewNow.current, wheelZoomFactor(moved.y), focus), false);
        return;
      }
      if (!el.contains(document.activeElement)) return;
      e.preventDefault();
      apply(panBy(viewNow.current, -moved.x, -moved.y), false);
    }
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [apply]);

  // ---- pointers ---------------------------------------------------------------
  const pointers = useRef(new Map<number, Point>());
  const gesture = useRef<Press | Pinching | null>(null);

  const local = useCallback((e: { clientX: number; clientY: number }): Point => {
    const rect = viewport.current?.getBoundingClientRect();
    return { x: e.clientX - (rect?.left ?? 0), y: e.clientY - (rect?.top ?? 0) };
  }, []);

  const endGesture = useCallback(() => {
    gesture.current = null;
    setPanning(false);
  }, []);

  // CAPTURED ONLY ONCE IT IS A GESTURE. A browser dispatches the click that
  // ends a captured press at the capturing element, so capturing every press
  // on arrival would turn a plain click on an item into a click on the
  // viewport and the item would never hear it.
  const capture = useCallback((id: number) => {
    const el = viewport.current;
    if (!el || typeof el.setPointerCapture !== "function") return;
    try {
      el.setPointerCapture(id);
    } catch {
      // A pointer the browser has already released cannot be captured; the
      // window listeners still follow it.
    }
  }, []);

  useEffect(() => {
    function onMove(e: PointerEvent) {
      if (!pointers.current.has(e.pointerId)) return;
      const at = local(e);
      pointers.current.set(e.pointerId, at);
      const g = gesture.current;
      if (!g) return;
      if (g.kind === "pinch") {
        const a = pointers.current.get(g.ids[0]);
        const b = pointers.current.get(g.ids[1]);
        if (a && b) apply(pinch(g.start, a, b), false);
        return;
      }
      if (g.id !== e.pointerId) return;
      if (!g.dragging) {
        if (!beyondSlop(g.start, at, g.type)) return;
        // An inactive canvas does not take a finger: the page is scrolling.
        if (g.type === "touch" && !touchNow.current) {
          endGesture();
          return;
        }
        g.dragging = true;
        setPanning(true);
        capture(g.id);
        // A drag that began on a card's text has started a selection; the
        // chart is moving, so nothing is being selected.
        window.getSelection?.()?.removeAllRanges();
      }
      apply(panBy(g.view, at.x - g.start.x, at.y - g.start.y), false);
    }

    function onUp(e: PointerEvent) {
      if (!pointers.current.delete(e.pointerId)) return;
      const g = gesture.current;
      if (g?.kind === "press" && g.id === e.pointerId) {
        if (g.dragging) {
          if (viewport.current) swallowNextClick(viewport.current);
        } else if (g.type === "touch" && !touchNow.current && e.type === "pointerup") {
          setTouchActive(true);
        }
        endGesture();
      } else if (g?.kind === "pinch") {
        // One finger lifting ends the pinch rather than turning the other into
        // a pan, which would jump by however far the midpoint had travelled.
        endGesture();
      }
    }

    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
  }, [apply, capture, endGesture, local]);

  function onPointerDown(e: ReactPointerEvent<HTMLDivElement>) {
    if (!ready) return;
    if (e.pointerType === "mouse" && e.button !== 0) return;
    const target = e.target instanceof Element ? e.target : null;
    if (target?.closest(INTERACTIVE)) return;
    const at = local(e);
    pointers.current.set(e.pointerId, at);
    const touches = [...pointers.current.entries()];
    if (e.pointerType === "touch" && touches.length >= 2) {
      const [a, b] = touches.slice(-2) as [[number, Point], [number, Point]];
      capture(a[0]);
      capture(b[0]);
      gesture.current = {
        kind: "pinch",
        ids: [a[0], b[0]],
        start: { view: viewNow.current, a: a[1], b: b[1] },
      };
      setPanning(true);
      return;
    }
    gesture.current = {
      kind: "press",
      id: e.pointerId,
      type: e.pointerType || "mouse",
      start: at,
      view: viewNow.current,
      dragging: false,
    };
  }

  // A tap outside hands a touch-activated canvas back to the page.
  useEffect(() => {
    if (!touchActive) return;
    function onOutside(e: PointerEvent) {
      if (e.target instanceof Node && !root.current?.contains(e.target)) setTouchActive(false);
    }
    document.addEventListener("pointerdown", onOutside, true);
    return () => document.removeEventListener("pointerdown", onOutside, true);
  }, [touchActive]);

  // ---- keys ---------------------------------------------------------------------
  function onKeyDown(e: ReactKeyboardEvent<HTMLDivElement>) {
    if (e.target !== e.currentTarget || !ready) return;
    const mod = e.ctrlKey || e.metaKey;
    const { width, height } = sizeNow.current;
    const centre = { x: width / 2, y: height / 2 };
    let handled = true;
    if (e.key === "+" || (mod && e.key === "=")) {
      apply(zoomAt(viewNow.current, ZOOM_STEP, centre), true);
    } else if (e.key === "-") {
      apply(zoomAt(viewNow.current, 1 / ZOOM_STEP, centre), true);
    } else if (e.key === "0") {
      if (content) apply(fit(content, sizeNow.current), true);
    } else if (!mod && !e.altKey && e.key.startsWith("Arrow")) {
      const step = {
        ArrowLeft: [PAN_STEP, 0],
        ArrowRight: [-PAN_STEP, 0],
        ArrowUp: [0, PAN_STEP],
        ArrowDown: [0, -PAN_STEP],
      }[e.key];
      if (step) apply(panBy(viewNow.current, step[0]!, step[1]!), false);
      else handled = false;
    } else {
      handled = false;
    }
    if (handled) e.preventDefault();
  }

  // ---- a browser's scroll-into-view becomes a pan ------------------------------
  function onScroll() {
    const el = viewport.current;
    if (!el || (el.scrollLeft === 0 && el.scrollTop === 0)) return;
    const dx = el.scrollLeft;
    const dy = el.scrollTop;
    el.scrollLeft = 0;
    el.scrollTop = 0;
    apply(panBy(viewNow.current, -dx, -dy), false);
  }

  const centreZoom = (factor: number) => {
    const { width, height } = sizeNow.current;
    apply(zoomAt(viewNow.current, factor, { x: width / 2, y: height / 2 }), true);
  };

  return (
    <div
      className="canvas"
      ref={root}
      data-ready={ready}
      data-animate={animate}
      data-panning={panning}
      data-touch-active={touchActive}
    >
      <div
        className="canvas-viewport"
        ref={viewport}
        tabIndex={0}
        role="group"
        aria-roledescription="canvas"
        aria-label={label}
        aria-describedby={helpID}
        onPointerDown={onPointerDown}
        onKeyDown={onKeyDown}
        onScroll={onScroll}
      >
        <div
          className="canvas-world"
          style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}
        >
          <OverlayContext.Provider value={layer}>{children}</OverlayContext.Provider>
        </div>
      </div>
      <div className="canvas-overlay" ref={setLayer}>
        {overlay}
      </div>
      {(controls || touchActive) && (
        <div className="canvas-controls">
          {touchActive && (
            <Button size="sm" onClick={() => setTouchActive(false)}>
              Done
            </Button>
          )}
          {controls && (
            <>
              <Button
                size="sm"
                variant="ghost"
                icon="zoomOut"
                title="Zoom out"
                onClick={() => centreZoom(1 / ZOOM_STEP)}
                disabled={!ready}
              />
              <Button
                size="sm"
                variant="ghost"
                icon="zoomIn"
                title="Zoom in"
                onClick={() => centreZoom(ZOOM_STEP)}
                disabled={!ready}
              />
              <Button
                size="sm"
                variant="ghost"
                icon="maximize"
                title="Fit to view"
                onClick={() => content && apply(fit(content, sizeNow.current), true)}
                disabled={!ready}
              />
            </>
          )}
        </div>
      )}
      <span className="sr-only" id={helpID}>
        While this canvas has focus, plus and minus zoom, zero fits everything in view, and the
        arrow keys pan.
      </span>
    </div>
  );
}

/**
 * Swallows the click a drag ends with.
 *
 * A drag that started on an item ends in a click on that item, and opening an
 * editor because somebody moved the chart is the canvas lying about what they
 * did. Only a click inside the viewport is swallowed, and the listener goes
 * after one turn of the event loop whether or not a click came, so a later,
 * real click is never eaten.
 */
function swallowNextClick(within: HTMLElement): void {
  const swallow = (e: MouseEvent) => {
    if (!(e.target instanceof Node) || !within.contains(e.target)) return;
    e.preventDefault();
    e.stopPropagation();
  };
  window.addEventListener("click", swallow, { capture: true, once: true });
  setTimeout(() => window.removeEventListener("click", swallow, { capture: true }), 0);
}
