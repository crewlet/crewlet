/**
 * A menu button: a trigger that opens a short list of actions.
 *
 * WHY IT EXISTS. A card on a chart has more actions than it has room for
 * buttons (edit, move, delete, change kind, open), and a row of icons nobody
 * can name is not an interface. The WAI-ARIA menu button pattern is the one a
 * screen reader and a keyboard already know, and it has enough rules that a
 * second hand-written copy would miss some of them.
 *
 * THE RULES IT KEEPS.
 *
 * - OPENING PUTS FOCUS ON THE FIRST ITEM (ArrowUp on the trigger: the last).
 *   Arrows move and wrap, Home and End jump, a letter moves to the next item
 *   starting with it, Enter or Space activates.
 * - IT IS A POPUP ON THE SHARED STACK (`usePopup`): Escape closes the menu
 *   before anything beneath it, and a press outside closes it without also
 *   counting as a press on a veil.
 * - FOCUS GOES BACK TO WHAT OPENED IT on Escape, on Tab (which then carries on
 *   to the next control, as the pattern says) and before an action runs. The
 *   last is what lets an action open a dialog that returns focus to the right
 *   place: the dialog captures its opener when it first renders, and by then
 *   focus is back on the trigger rather than on an item about to unmount.
 *   What opened it is whatever held focus when it opened, so a menu opened
 *   from a tree item with the ContextMenu key returns to the tree item, not to
 *   the pointer-only button that anchors it.
 * - A DISABLED ITEM STAYS FOCUSABLE and says so (`aria-disabled`), so a
 *   keyboard user learns the action exists and is unavailable rather than
 *   meeting a list that silently skips it.
 * - IN A CANVAS IT RENDERS INTO THE OVERLAY LAYER it is given, positioned from
 *   the trigger's viewport rectangle and kept inside the layer's bounds, so the
 *   zoom neither scales nor clips it. It follows the trigger through a pan
 *   (`VIEW_CHANGE_EVENT`) and closes when the trigger is panned out of view.
 *   Focus moves in only once it has been placed: until then it is drawn
 *   `visibility: hidden` so it never flashes at the layer's corner, and a
 *   browser refuses focus to a hidden element. Anywhere else it is positioned
 *   by the stylesheet under its trigger.
 * - WHAT HAPPENS IN THE MENU STAYS IN THE MENU. A key, a press or a click
 *   inside it does not reach the element it was opened from. A portal carries
 *   React events up the component tree, not the document, so without this the
 *   Enter that activates "Delete" would also reach the card's own Enter
 *   (edit), and ArrowDown would move the tree's focus as well as the menu's.
 *   Escape and Tab still travel, because the layer stack acts on them at the
 *   document, and so does a chord with Ctrl, Command or Alt, which is an
 *   application shortcut rather than a key of the menu's.
 * - A CHOICE IS A RADIO ITEM. An item given `checked` is a `menuitemradio`
 *   that says whether it is the current value (`aria-checked`) and draws a
 *   check where the others draw nothing, so a menu that picks one value (a
 *   unit's lead) reads as a choice with an answer rather than a list of
 *   actions that all look alike. Every answer of such a choice should say
 *   `checked`, true or false, so the check column lines up. The answers that
 *   stand together are nested in one `group`, as the pattern asks of radio
 *   items sharing a menu with actions, so a screen reader counts "2 of 3"
 *   among the answers rather than among every item; a separator divides the
 *   group from the actions.
 * - IT CAN BE OPENED FROM OUTSIDE (`open`, `onOpenChange`), because a tree item
 *   that holds focus opens its card's menu from the keyboard while the
 *   trigger itself is out of the tab order (`triggerTabIndex={-1}`).
 */

import {
  type ComponentType,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { Button, cx } from "./primitives.tsx";
import { VIEW_CHANGE_EVENT, placePopup } from "./viewport.ts";
import { type GlyphProps, CheckGlyph, MoreVertGlyph } from "@crewlethq/icons/glyphs";
import { usePopupLayer } from "@crewlethq/ui";

export interface MenuItem {
  kind?: "item";
  key: string;
  label: string;
  icon?: ComponentType<GlyphProps>;
  onSelect: () => void;
  disabled?: boolean;
  /** A destructive action: drawn in the critical ink. */
  danger?: boolean;
  /** A shortcut or a short note, shown at the end of the row. */
  hint?: ReactNode;
  /**
   * Makes the item one answer of a single choice: a `menuitemradio` that is
   * the current value when true. Left out, the item is an action.
   */
  checked?: boolean;
}

export interface MenuSeparator {
  kind: "separator";
  key: string;
}

export type MenuEntry = MenuItem | MenuSeparator;

/** Space between the trigger and the menu, and between the menu and a layer's edge. */
const GAP = 4;

/** Every item the arrows walk: actions and the answers of a choice alike. */
const ITEM_SELECTOR = "[role='menuitem'],[role='menuitemradio']";

/** A menu's entries as it draws them: an action, a separator, or a run of answers. */
type Run =
  | { kind: "action"; item: MenuItem }
  | { kind: "separator"; key: string }
  | { kind: "answers"; key: string; items: MenuItem[] };

/** Gathers each run of consecutive answers (items given `checked`) into one group. */
function runs(entries: readonly MenuEntry[]): Run[] {
  const out: Run[] = [];
  for (const entry of entries) {
    if (entry.kind === "separator") {
      out.push({ kind: "separator", key: entry.key });
      continue;
    }
    const last = out[out.length - 1];
    if (entry.checked === undefined) out.push({ kind: "action", item: entry });
    else if (last?.kind === "answers") last.items.push(entry);
    else out.push({ kind: "answers", key: `${entry.key}:answers`, items: [entry] });
  }
  return out;
}

export function Menu({
  label,
  items,
  icon: Glyph = MoreVertGlyph,
  children,
  layer,
  open: controlled,
  onOpenChange,
  triggerTabIndex,
  align = "start",
  size = "sm",
}: {
  /** The trigger's accessible name, such as "Actions for Software Engineer". */
  label: string;
  items: readonly MenuEntry[];
  icon?: ComponentType<GlyphProps>;
  /** Visible trigger text; without it the trigger is an icon button named by `label`. */
  children?: ReactNode;
  /** A canvas overlay layer to render into (`useCanvasOverlay`). */
  layer?: HTMLElement | null;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  /** -1 for a pointer-only trigger whose actions the keyboard reaches another way. */
  triggerTabIndex?: number;
  /** Which edge of the trigger an inline menu lines up with. */
  align?: "start" | "end";
  size?: "sm" | "md";
}) {
  const id = useId();
  const menuID = `${id}-menu`;
  const [uncontrolled, setUncontrolled] = useState(false);
  const open = controlled ?? uncontrolled;
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  const startAt = useRef<"first" | "last">("first");
  const [place, setPlace] = useState<{ left: number; top: number } | null>(null);

  // Read through refs so every callback below is stable: the layer's
  // listeners are registered once per opening, not once per parent render.
  const isControlled = useRef(controlled !== undefined);
  const report = useRef(onOpenChange);
  isControlled.current = controlled !== undefined;
  report.current = onOpenChange;

  const setOpen = useCallback((next: boolean) => {
    if (!isControlled.current) setUncontrolled(next);
    report.current?.(next);
  }, []);

  const giveFocusBack = useCallback(() => {
    const back = opener.current;
    const target = back && back.isConnected && back !== document.body ? back : trigger.current;
    target?.focus();
  }, []);

  const close = useCallback(
    (restore: boolean) => {
      if (restore) giveFocusBack();
      setOpen(false);
    },
    [giveFocusBack, setOpen],
  );

  const popup = usePopupLayer({
    open,
    onDismiss: (reason) => close(reason === "escape"),
  });

  const entries = () =>
    menu.current ? [...menu.current.querySelectorAll<HTMLElement>(ITEM_SELECTOR)] : [];

  // ON OPEN: remember what held focus.
  useLayoutEffect(() => {
    if (!open) return;
    const active = document.activeElement;
    // Not when focus is already inside: an effect that runs twice (React's
    // strict mode does, on mount) would otherwise remember a menu item.
    if (!(active instanceof Node && menu.current?.contains(active))) {
      opener.current = active instanceof HTMLElement ? active : null;
    }
  }, [open]);

  // THEN MOVE IT IN, once the menu can take it: at once under its trigger, and
  // in a layer only after it has been placed (see the module doc).
  const placed = open && (!layer || place !== null);
  useLayoutEffect(() => {
    if (!placed) return;
    const all = entries();
    (startAt.current === "last" ? all[all.length - 1] : all[0])?.focus();
    startAt.current = "first";
  }, [placed]);

  // ---- position in a layer -------------------------------------------------
  const position = useCallback(() => {
    if (!layer || !trigger.current || !menu.current) return;
    const bounds = layer.getBoundingClientRect();
    const at = trigger.current.getBoundingClientRect();
    const anchor = {
      x: at.left - bounds.left,
      y: at.top - bounds.top,
      width: at.width,
      height: at.height,
    };
    // Panned out of view: a menu for a card nobody can see is a menu for
    // nothing, so it closes rather than floating detached at the edge.
    if (
      anchor.x + anchor.width < 0 ||
      anchor.y + anchor.height < 0 ||
      anchor.x > bounds.width ||
      anchor.y > bounds.height
    ) {
      close(true);
      return;
    }
    const own = menu.current.getBoundingClientRect();
    const spot = placePopup(
      anchor,
      { width: own.width, height: own.height },
      { x: GAP, y: GAP, width: bounds.width - 2 * GAP, height: bounds.height - 2 * GAP },
      GAP,
    );
    setPlace((was) =>
      was && was.left === spot.x && was.top === spot.y ? was : { left: spot.x, top: spot.y },
    );
  }, [layer, close]);

  useLayoutEffect(() => {
    if (!open || !layer) return;
    position();
    layer.addEventListener(VIEW_CHANGE_EVENT, position);
    window.addEventListener("resize", position);
    return () => {
      layer.removeEventListener(VIEW_CHANGE_EVENT, position);
      window.removeEventListener("resize", position);
    };
  }, [open, layer, position]);

  useEffect(() => {
    if (!open) setPlace(null);
  }, [open]);

  // ---- keys ------------------------------------------------------------------
  function onTriggerKeyDown(e: KeyboardEvent<HTMLButtonElement>) {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      startAt.current = e.key === "ArrowUp" ? "last" : "first";
      setOpen(true);
    }
  }

  function onMenuKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    // The menu's own keys go no further: see the module doc.
    if (e.key !== "Escape" && e.key !== "Tab" && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.stopPropagation();
    }
    const all = entries();
    const at = all.indexOf(document.activeElement as HTMLElement);
    const go = (index: number) => all[(index + all.length) % all.length]?.focus();
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        go(at + 1);
        return;
      case "ArrowUp":
        e.preventDefault();
        go(at - 1);
        return;
      case "Home":
        e.preventDefault();
        go(0);
        return;
      case "End":
        e.preventDefault();
        go(all.length - 1);
        return;
      case "Tab":
        // Not prevented: focus goes back to the opener first, and the Tab then
        // carries on from there to the next control.
        close(true);
        return;
      default:
        if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey && e.key !== " ") {
          const letter = e.key.toLocaleLowerCase();
          for (let step = 1; step <= all.length; step++) {
            const candidate = all[(at + step) % all.length]!;
            if ((candidate.textContent ?? "").trim().toLocaleLowerCase().startsWith(letter)) {
              e.preventDefault();
              candidate.focus();
              return;
            }
          }
        }
    }
  }

  function activate(item: MenuItem) {
    if (item.disabled) return;
    close(true);
    item.onSelect();
  }

  const item = (entry: MenuItem) => (
    <button
      key={entry.key}
      type="button"
      role={entry.checked === undefined ? "menuitem" : "menuitemradio"}
      aria-checked={entry.checked}
      tabIndex={-1}
      className={cx("menu-item", entry.danger && "danger")}
      aria-disabled={entry.disabled || undefined}
      onClick={() => activate(entry)}
    >
      {entry.checked !== undefined && (
        <span className="menu-item-check" aria-hidden="true">
          {entry.checked && <CheckGlyph size="sm" />}
        </span>
      )}
      {entry.icon && <entry.icon size="sm" />}
      <span className="menu-item-label">{entry.label}</span>
      {entry.hint && <span className="menu-item-hint">{entry.hint}</span>}
    </button>
  );

  const list = open ? (
    <div
      className={cx("popover", "menu", !layer && align === "end" && "end")}
      id={menuID}
      role="menu"
      aria-label={label}
      ref={(el) => {
        menu.current = el;
        popup.panelRef(el);
      }}
      style={layer ? (place ?? { visibility: "hidden" }) : undefined}
      onKeyDown={onMenuKeyDown}
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
    >
      {runs(items).map((run) =>
        run.kind === "separator" ? (
          <div key={run.key} role="separator" className="menu-separator" />
        ) : run.kind === "answers" ? (
          <div key={run.key} role="group" className="menu-group">
            {run.items.map(item)}
          </div>
        ) : (
          item(run.item)
        ),
      )}
    </div>
  ) : null;

  return (
    <span className="menu-anchor">
      <Button
        ref={(el) => {
          trigger.current = el;
          popup.insideRef(el);
        }}
        variant="ghost"
        size={size}
        icon={Glyph}
        // An icon trigger is named by its title; one with visible text by the text.
        title={children ? undefined : label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuID : undefined}
        tabIndex={triggerTabIndex}
        onClick={() => {
          if (open) {
            close(true);
          } else {
            startAt.current = "first";
            setOpen(true);
          }
        }}
        onKeyDown={onTriggerKeyDown}
      >
        {children}
      </Button>
      {layer ? list && createPortal(list, layer) : list}
    </span>
  );
}
