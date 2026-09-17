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
import { ButtonLink, IconButton } from "@crewlethq/ui";
import {
  ArrowOutwardGlyph,
  CloseGlyph,
  KeyboardArrowDownGlyph,
  KeyboardArrowUpGlyph,
} from "@crewlethq/icons/glyphs";
import { useKeyChords } from "~/lib/keys.ts";

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
 * A row's click, for a row that is an anchor.
 *
 * A ROW IS A REAL LINK EITHER WAY: its `href` is the object's page, so
 * middle-click and ⌘-click open a tab and the status bar says where it goes;
 * a plain left click is intercepted and peeks instead, because the list is
 * where the reader is and sending them away to read one title is the
 * navigation every tracker learned not to make.
 *
 * ONE COPY. This rule was written twice — here, where nothing called it, and
 * again in `components/work.tsx` as `openHandler`, where the whole tracker
 * did. Two spellings of "which clicks mean elsewhere" is how one of them
 * comes to forget the middle button, and the dead copy is the one that would
 * have been corrected last.
 *
 * It takes a plain callback rather than an object reference: a caller that
 * has a ref closes over it, and a caller that opens something else entirely —
 * a board card, a calendar chip — needs no ref at all.
 */
export function rowPeekHandler(open?: () => void): ((e: React.MouseEvent) => void) | undefined {
  if (!open) return undefined;
  return (e) => {
    // EVERY WAY A READER OPENS A TAB. A plain left click peeks; anything the
    // browser would treat as "open elsewhere" is left alone.
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    e.preventDefault();
    open();
  };
}

/**
 * A `DataGrid` row's `onRowActivate`, wired to open a peek.
 *
 * WHY NOT [rowPeekHandler] DIRECTLY: that one decides on `e.button`, and a
 * KeyboardEvent has none. `DataGrid` activates its cursor row from an `enter`
 * chord, so `undefined !== 0` read as "the reader meant elsewhere" and the
 * keyboard path opened nothing at all. Three screens each hit that and wrote
 * the same private adapter — `Fleet`, `Pages` and `Spend`, byte for byte —
 * which is the shape this repository has already paid for three times
 * (`textcut`, `whsec`, `httpjson`). It belongs beside the handler it wraps.
 *
 * THE KEYBOARD PATH HAS NO MODIFIERS TO READ, so it opens unconditionally:
 * a grid's `enter` is the reader saying "this row" with nothing to say
 * "elsewhere" with, and the browser will not navigate on its own behalf.
 */
export function peekRow<T>(
  open: (row: T) => void,
): (row: T, e: React.MouseEvent | React.KeyboardEvent) => void {
  return (row, e) => {
    if (!("button" in e)) {
      open(row);
      return;
    }
    rowPeekHandler(() => open(row))?.(e);
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

  useKeyChords([
    // ESCAPE CLOSES FROM INSIDE A FIELD TOO: the peek holds inputs, and a
    // reader who has focused one and wants out means the rail rather than the
    // field.
    { key: "escape", run: close, whileTyping: true },
    { key: "[", run: () => onStep?.(-1), when: Boolean(onStep) },
    { key: "]", run: () => onStep?.(1), when: Boolean(onStep) },
  ]);

  // THE DRAGGED WIDTH IS PUBLISHED ON THE ROOT, not on the rail.
  //
  // Two things read it and they are on opposite sides of this component: the
  // grid track that sizes the peek column is declared on `.app`, which is
  // this rail's ANCESTOR, and the fixed drawer's own `width` is on the rail
  // itself. A custom property inherits downward only, so the value written on
  // the aside — where it was — could never reach the track: the column stayed
  // at its 420px fallback for ever, `.peek-rail` deliberately declares no
  // width of its own, and a drag moved React state, localStorage and nothing
  // on screen. From the root both readers inherit one value, which is the
  // thing frame.css claims: the column and the panel cannot disagree.
  //
  // Removed on unmount rather than left behind, so a closed rail does not
  // leave a width pinned on the document for the next one to inherit.
  useEffect(() => {
    const root = document.documentElement;
    root.style.setProperty("--peek-w", `${width}px`);
    return () => {
      root.style.removeProperty("--peek-w");
    };
  }, [width]);

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
      <aside className="peek-rail" aria-label={`${KINDS[object.kind].label} detail`}>
        <div
          className="peek-grip"
          onMouseDown={startDrag}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize the detail panel"
        />
        <div className="peek-bar">
          {/* `IconButton` RATHER THAN A LABELLED BUTTON WITH NO LABEL: each of
              these is a glyph and a tooltip, which is the one thing it is for,
              and it makes the name a required prop rather than something a
              `title` happened to supply. */}
          {onStep && (
            <>
              <IconButton
                size="sm"
                variant="ghost"
                icon={<KeyboardArrowUpGlyph size="sm" />}
                label="Previous"
                title="Previous ([)"
                onClick={() => onStep(-1)}
              />
              <IconButton
                size="sm"
                variant="ghost"
                icon={<KeyboardArrowDownGlyph size="sm" />}
                label="Next"
                title="Next (])"
                onClick={() => onStep(1)}
              />
            </>
          )}
          <span className="spacer" />
          {/* A REAL ANCHOR WEARING THE BUTTON, which is what `.btn` on an `<a>`
              was spelling by hand — and the arrow is a glyph now rather than
              the character `↗`. NOT `external`: that withholds the opener and
              opens a tab, and this goes to the object's own page in this app. */}
          <ButtonLink
            size="small"
            variant="secondary"
            href={peekHref(object)}
            trailingIcon={<ArrowOutwardGlyph size="sm" />}
          >
            Open
          </ButtonLink>
          <IconButton
            size="sm"
            variant="ghost"
            icon={<CloseGlyph size="sm" />}
            label="Close"
            title="Close (esc)"
            onClick={close}
          />
        </div>
        <div className="peek-body">{children}</div>
      </aside>
    </>
  );
}
