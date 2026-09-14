/**
 * The peek: one object, beside whatever list you found it in.
 *
 * # Why the frame owns it
 *
 * The tracker grew its own — `item=` on the board, a rail rendered by the
 * board, a peek component that only the board could open. So a board could
 * peek a task and nothing else in the product could peek anything: a seat, a
 * node, a turn and a page each cost a navigation and a way back.
 *
 * Here it is one component reading one query key, so any list can open any
 * kind. The rail asks its OWN query for the object rather than being handed a
 * row by the screen: a row is whatever the list happened to select, and a peek
 * that rendered from it would show a different set of facts depending on which
 * list you opened it from.
 *
 * # The history rule
 *
 * Opening the rail PUSHES — Back closes it, which is what a reader means by
 * Back with a panel open. Moving it — `[` and `]` stepping through the list —
 * REPLACES: four objects walked through one open rail are one place the reader
 * has been, exactly as four ticked chips are one screen.
 *
 * # Width
 *
 * 420 px, dragged between 360 and 640, remembered per viewer. Under 1180 px it
 * is a drawer over the content instead: the rail plus a 236 px sidebar plus a
 * readable list does not fit, and the sidebar is the one that collapses first
 * because it is one keystroke away.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useNavigator, useRoute, parseHash, buildHash } from "../router.tsx";
import { KINDS, parseRef, pathOf, refToken, type ObjectRef } from "./objects.ts";
import { href } from "../router.tsx";
import { Button } from "~/ui/primitives.tsx";

const WIDTH_KEY = "crewlet.peek.width";
const MIN = 360;
const MAX = 640;
const DEFAULT = 420;

function storedWidth(): number {
  try {
    const raw = Number(localStorage.getItem(WIDTH_KEY));
    if (Number.isFinite(raw) && raw >= MIN && raw <= MAX) return raw;
  } catch {
    // An unreadable preference is not a reason to render no rail.
  }
  return DEFAULT;
}

/** The object the rail is open on, or null. */
export function usePeek(): ObjectRef | null {
  const route = useRoute();
  return parseRef(route.query.get("peek"));
}

/**
 * Open, move and close the rail.
 *
 * `open` pushes and `move` replaces — see the history rule above. Both are
 * written here rather than at each call site, because a screen that opened a
 * peek with `filter` would make Back leave the list entirely.
 */
export function usePeekControls(): {
  open: (ref: ObjectRef) => void;
  move: (ref: ObjectRef) => void;
  close: () => void;
} {
  const nav = useNavigator();
  return {
    open: useCallback((ref: ObjectRef) => nav.section("peek", refToken(ref)), [nav]),
    move: useCallback((ref: ObjectRef) => nav.filter({ peek: refToken(ref) }), [nav]),
    close: useCallback(() => nav.filter({ peek: null }), [nav]),
  };
}

/** The href a row carries, so a modifier-click opens the object's page. */
export function peekHref(ref: ObjectRef): string {
  return href(pathOf(ref));
}

/**
 * The href that OPENS the peek, for a row that is an anchor.
 *
 * A row is a real link either way: its `href` is the object's page so
 * middle-click and ⌘-click work, and the plain click is intercepted to peek.
 */
export function rowPeekHandler(
  ref: ObjectRef,
  open: (ref: ObjectRef) => void,
): (e: React.MouseEvent) => void {
  return (e) => {
    // EVERY WAY A READER OPENS A TAB. A plain left click peeks; anything the
    // browser would treat as "open elsewhere" is left alone.
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    e.preventDefault();
    open(ref);
  };
}

export function DetailRail({
  ref: object,
  children,
  onStep,
}: {
  ref: ObjectRef;
  children: ReactNode;
  /** Step to the previous/next object in the list this was opened from. */
  onStep?: (delta: -1 | 1) => void;
}) {
  const { close } = usePeekControls();
  const [width, setWidth] = useState(storedWidth);
  const dragging = useRef(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent): void {
      const el = document.activeElement;
      const typing =
        el instanceof HTMLElement &&
        (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      if (typing) return;
      if (e.key === "Escape") {
        e.preventDefault();
        close();
      }
      if (onStep && e.key === "[") {
        e.preventDefault();
        onStep(-1);
      }
      if (onStep && e.key === "]") {
        e.preventDefault();
        onStep(1);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [close, onStep]);

  const startDrag = useCallback(() => {
    dragging.current = true;
    function onMove(e: MouseEvent): void {
      if (!dragging.current) return;
      const next = Math.min(MAX, Math.max(MIN, window.innerWidth - e.clientX));
      setWidth(next);
    }
    function onUp(): void {
      dragging.current = false;
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      setWidth((w) => {
        try {
          localStorage.setItem(WIDTH_KEY, String(w));
        } catch {
          // The drag still applies for this session.
        }
        return w;
      });
    }
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }, []);

  return (
    <>
      <div className="peek-veil" onClick={close} role="presentation" />
      <aside
        className="peek-rail"
        style={{ "--peek-w": `${width}px` } as never}
        aria-label={`${KINDS[object.kind].label} detail`}
      >
        <div
          className="peek-grip"
          onMouseDown={startDrag}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize the detail panel"
        />
        <div className="peek-bar">
          {onStep && (
            <>
              <Button
                size="sm"
                variant="ghost"
                icon="chevronUp"
                title="Previous ([)"
                onClick={() => onStep(-1)}
              />
              <Button
                size="sm"
                variant="ghost"
                icon="chevronDown"
                title="Next (])"
                onClick={() => onStep(1)}
              />
            </>
          )}
          <span className="spacer" />
          <a className="btn sm" href={peekHref(object)}>
            Open ↗
          </a>
          <Button size="sm" variant="ghost" icon="x" title="Close (esc)" onClick={close} />
        </div>
        <div className="peek-body">{children}</div>
      </aside>
    </>
  );
}

/** The current hash with a different peek, for a link that moves the rail. */
export function hashWithPeek(ref: ObjectRef | null): string {
  const current = parseHash(location.hash);
  const query = new URLSearchParams(current.query);
  if (ref) query.set("peek", refToken(ref));
  else query.delete("peek");
  return buildHash(current.path, query);
}
