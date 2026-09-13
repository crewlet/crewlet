/**
 * The layer stack: which open surface a key press or a press outside belongs to.
 *
 * WHY IT EXISTS. Every dialog used to put its own Escape listener on the
 * document, so a prompt raised over another modal closed on the same keypress
 * as the modal beneath it, and an operator asking "discard these edits?" lost
 * the editor along with the question. Focus was moved in and returned, but Tab
 * walked out of the dialog into the page behind the veil. Two shells (the
 * dialog and the drawer) would each have had to get those rules right, and two
 * copies of a focus rule are how they come to disagree.
 *
 * THE RULES IT KEEPS.
 *
 * - ONE STACK, module-level, holding modals (a dialog, a drawer) and popups (a
 *   menu, a picker's list). Only the TOPMOST entry handles Escape and a press
 *   outside, so an open menu inside a drawer closes first, then the drawer.
 * - ORDER IS OPENING ORDER, taken when the surface renders open rather than
 *   when its effect runs: React runs a child's effects before its parent's, so
 *   a dialog mounted inside a drawer in the same commit would otherwise
 *   register first and end up beneath the drawer it sits on.
 * - A MODAL TRAPS TAB. Focus cycles inside the topmost modal, and Tab from
 *   anywhere outside it (focus can leave through a toast's close button) comes
 *   back in. There is no exception for a modal the stack does not know: a
 *   hand-rolled one raised over a drawer would have its Tab pulled back into
 *   the drawer and its Escape taken by the drawer beneath it, which is why
 *   every modal in the dashboard is on the stack, the shell's token dialog,
 *   search and engine panel included.
 * - FOCUS GOES IN ON OPEN, unless something inside already took it (a field
 *   with `autoFocus`), and GOES BACK ON CLOSE to whatever held it when the
 *   modal first rendered. That is captured during render on purpose: by the
 *   time an effect runs, `autoFocus` has already moved focus into the modal,
 *   and a capture taken then would restore focus to an element that no longer
 *   exists. When that element has gone because it sat in a modal that closed
 *   as this one opened, focus goes where that modal would have sent it; when
 *   it has gone from a modal that is still open, to that modal's panel, never
 *   behind its veil (`returnChain`). A modal that closes BENEATH another
 *   surface still open above it returns nothing: focus is in that surface.
 * - A VEIL CLOSES ONLY ITS OWN MODAL, and only when the press lands on the
 *   veil itself. The decision is taken on `pointerdown`, before any surface
 *   closes, so the press that dismisses a menu is never also read as a press
 *   on the veil beneath it. The CLOSE waits for that press's `click`: a veil
 *   removed on `pointerdown` is gone before a tap's compatibility mouse
 *   events are hit-tested, so the click a finger ends with would land on
 *   whatever the veil was covering, such as a Delete button behind a dialog.
 * - A CONTROL THAT CONSUMES ESCAPE KEEPS IT. A completion list that closes on
 *   Escape calls `preventDefault` or stops propagation, and the stack leaves
 *   that press alone.
 *
 * WHAT IT DOES NOT OWN: markup, copy, and whether closing is allowed right now.
 * A modal mid-write passes `dismissable: false`.
 *
 * A surface using these hooks is MOUNTED ONLY WHILE OPEN (a modal) or passes
 * `open` (a popup); both are how the callers in this tree already render.
 */

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

type Kind = "modal" | "popup";

interface Entry {
  kind: Kind;
  order: number;
  /** The surface itself: a modal's panel, a popup's list. */
  panel: () => HTMLElement | null;
  /** Where a modal hands focus back on close, first choice first (see `returnChain`). */
  returnTo: readonly Element[];
  /** A modal's veil, whose own presses close it. */
  veil: () => HTMLElement | null;
  /** Elements a press inside does not count as outside, such as a menu's trigger. */
  inside: () => (HTMLElement | null)[];
  /** Escape, or a press outside (a popup) or on the veil (a modal). */
  dismiss: (reason: "escape" | "outside") => void;
  dismissable: () => boolean;
}

const stack: Entry[] = [];
let opened = 0;

function top(kind?: Kind): Entry | undefined {
  let found: Entry | undefined;
  for (const entry of stack) {
    if (kind && entry.kind !== kind) continue;
    if (!found || entry.order > found.order) found = entry;
  }
  return found;
}

/**
 * What a keyboard user can Tab to inside `root`, in document order.
 *
 * Exported because a surface choosing where focus starts needs the same
 * answer the trap uses; two selectors would disagree about a disabled button.
 */
export function focusables(root: HTMLElement): HTMLElement[] {
  const selector = [
    "a[href]",
    "button:not([disabled])",
    "input:not([disabled]):not([type='hidden'])",
    "select:not([disabled])",
    "textarea:not([disabled])",
    "[contenteditable='true']",
    "[tabindex]",
  ].join(",");
  return [...root.querySelectorAll<HTMLElement>(selector)].filter(
    (el) =>
      el.tabIndex >= 0 &&
      !(el as HTMLButtonElement).disabled &&
      !el.closest("[hidden],[inert]") &&
      // RENDERED, where the browser can say so. A control under
      // `display: none` is skipped by Tab, and a trap that counted it as the
      // last stop would let focus walk past the real last one and out.
      (typeof el.checkVisibility !== "function" || el.checkVisibility()),
  );
}

function onKeyDown(e: KeyboardEvent): void {
  if (e.key === "Escape") {
    if (e.defaultPrevented) return;
    const entry = top();
    if (!entry) return;
    // Handled whether or not it closes: a modal that refuses to close right
    // now still owns the key, and nothing beneath it may take it instead.
    e.preventDefault();
    if (entry.dismissable()) entry.dismiss("escape");
    return;
  }
  if (e.key === "Tab") {
    const modal = top("modal");
    const panel = modal?.panel();
    if (!modal || !panel) return;
    // A popup above the modal that still holds focus decides Tab for itself.
    // One that has just handed focus back (a menu closes on Tab and returns
    // focus to its trigger before this listener runs) is still registered
    // until React re-renders, and must not let that Tab walk out of the modal.
    const popup = top("popup");
    const active = document.activeElement;
    if (popup && popup.order > modal.order && active && popup.panel()?.contains(active)) return;
    const inside = focusables(panel);
    if (inside.length === 0) {
      e.preventDefault();
      panel.focus();
      return;
    }
    const first = inside[0]!;
    const last = inside[inside.length - 1]!;
    if (!(active instanceof Node) || !panel.contains(active)) {
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
      return;
    }
    // BY DOCUMENT POSITION, not by identity with the first or last stop.
    // Focus can rest inside on something Tab never stops at (the panel
    // itself, a roving item with `tabindex="-1"`), and from one of those past
    // the last stop the browser's own Tab would leave the modal.
    const after = (el: HTMLElement) =>
      (active.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    if (e.shiftKey && !inside.some((el) => el !== active && !after(el))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && !inside.some((el) => el !== active && after(el))) {
      e.preventDefault();
      first.focus();
    }
  }
}

/** The modal whose veil the current press began on, closed by that press's click. */
let armed: Entry | null = null;

function onPointerDown(e: PointerEvent): void {
  armed = null;
  const entry = top();
  if (!entry || !(e.target instanceof Node)) return;
  const target = e.target;
  if (entry.kind === "modal") {
    if (target === entry.veil()) armed = entry;
    return;
  }
  const inside = [entry.panel(), ...entry.inside()];
  if (inside.some((el) => el?.contains(target))) return;
  if (entry.dismissable()) entry.dismiss("outside");
}

function onClick(e: MouseEvent): void {
  const entry = armed;
  armed = null;
  // Still the topmost, and still its veil that was pressed: a surface opened
  // between the press and its click owns the press now.
  if (!entry || entry !== top() || e.target !== entry.veil()) return;
  if (entry.dismissable()) entry.dismiss("outside");
}

function register(entry: Entry): () => void {
  if (stack.length === 0) {
    document.addEventListener("keydown", onKeyDown);
    // CAPTURE, so the decision is made before any handler on the page reacts
    // to the press and before a surface that closes because of it unmounts.
    document.addEventListener("pointerdown", onPointerDown, true);
    document.addEventListener("click", onClick, true);
  }
  stack.push(entry);
  return () => {
    const at = stack.indexOf(entry);
    if (at >= 0) stack.splice(at, 1);
    if (armed === entry) armed = null;
    if (stack.length === 0) {
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("pointerdown", onPointerDown, true);
      document.removeEventListener("click", onClick, true);
    }
  };
}

/**
 * The opening order of a surface that is open now, taken during render.
 *
 * A ref written during render, which React otherwise discourages: a render
 * that is thrown away only spends a number, and the order among the renders
 * that commit is still the order the surfaces opened in.
 */
function useOpeningOrder(open: boolean): number {
  const ref = useRef<number>(0);
  if (open && ref.current === 0) ref.current = ++opened;
  if (!open) ref.current = 0;
  return ref.current;
}

/**
 * Where a modal opening now hands focus back when it closes, first choice
 * first, read during its first render.
 *
 * The opener alone is not enough when it lives inside another modal. A
 * control in one modal often closes that modal and opens the next in one
 * gesture (a status panel's "Set token" that hands over to a credential
 * dialog), so
 * by the time the new modal closes its opener has been unmounted, and focus
 * restored to a detached element lands on the page body. The chain carries on
 * from there: the host modal's panel, which is still connected only while the
 * host is still open (so focus never goes behind a veil that is still up),
 * and then wherever the host itself would have returned focus.
 */
function returnChain(opener: Element | null): Element[] {
  if (!opener) return [];
  let host: Entry | undefined;
  for (const entry of stack) {
    if (entry.kind !== "modal" || !entry.panel()?.contains(opener)) continue;
    if (!host || entry.order > host.order) host = entry;
  }
  const panel = host?.panel();
  return host && panel ? [opener, panel, ...host.returnTo] : [opener];
}

/** Whether `entry` is the surface a key press or a press would reach now. */
function isTopmost(order: number): boolean {
  return top()?.order === order;
}

export interface ModalOptions {
  onClose: () => void;
  /** False while a request is in flight: Escape and the veil stop closing. */
  dismissable?: boolean;
  /**
   * Where focus starts when nothing inside has taken it, if not the first
   * control. A drawer's first control is its Close button, and an editor
   * that opens with focus on Close has made closing the first thing it asks.
   */
  initialFocus?: () => HTMLElement | null;
}

export interface Modal {
  /** The dialog element: focus goes into it and Tab stays in it. */
  panelRef: (el: HTMLElement | null) => void;
  /** The veil behind it: a press on the veil itself closes the modal. */
  veilRef: (el: HTMLElement | null) => void;
}

/** A modal surface: a dialog or a drawer. Mount it only while it is open. */
export function useModal({ onClose, dismissable = true, initialFocus }: ModalOptions): Modal {
  const panel = useRef<HTMLElement | null>(null);
  const veil = useRef<HTMLElement | null>(null);
  const close = useRef(onClose);
  const canClose = useRef(dismissable);
  const start = useRef(initialFocus);
  close.current = onClose;
  canClose.current = dismissable;
  start.current = initialFocus;
  const order = useOpeningOrder(true);
  // WHERE FOCUS CAME FROM, read during the first render. See the module doc
  // for why an effect is too late.
  const [returnTo] = useState<Element[]>(() =>
    typeof document === "undefined" ? [] : returnChain(document.activeElement),
  );

  useLayoutEffect(
    () =>
      register({
        kind: "modal",
        order,
        panel: () => panel.current,
        returnTo,
        veil: () => veil.current,
        inside: () => [],
        dismiss: () => close.current(),
        dismissable: () => canClose.current,
      }),
    [order, returnTo],
  );

  useEffect(() => {
    const root = panel.current;
    if (root && !root.contains(document.activeElement)) {
      // The surface's chosen start, else the first control, else the panel
      // itself, so a screen reader announces the label rather than continuing
      // behind the veil.
      (start.current?.() ?? focusables(root)[0] ?? root).focus();
    }
    return () => {
      // NEVER FROM BENEATH ANOTHER SURFACE. A modal can close while one that
      // opened after it is still up (a route change or a shortcut closes it,
      // not its own Escape). Focus is inside that surface, which this one has
      // already left the stack for by now, and handing focus back here would
      // move it behind a veil that is still up.
      const holder = document.activeElement;
      if (holder && stack.some((entry) => entry.panel()?.contains(holder))) return;
      const back = returnTo.find(
        (el): el is HTMLElement =>
          el instanceof HTMLElement && el.isConnected && el !== document.body,
      );
      back?.focus();
    };
  }, [returnTo]);

  const panelRef = useCallback((el: HTMLElement | null) => {
    panel.current = el;
  }, []);
  const veilRef = useCallback((el: HTMLElement | null) => {
    veil.current = el;
  }, []);
  return { panelRef, veilRef };
}

export interface PopupOptions {
  open: boolean;
  /** Called on Escape and on a press outside the popup and its `inside` elements. */
  onDismiss: (reason: "escape" | "outside") => void;
}

export interface Popup {
  /** The popup surface. */
  panelRef: (el: HTMLElement | null) => void;
  /** An element a press on does not dismiss, such as the button that toggles it. */
  insideRef: (el: HTMLElement | null) => void;
  /** Whether this popup is the surface Escape would reach right now. */
  isTopmost: () => boolean;
}

/**
 * A non-modal popup on the same stack: a menu, a listbox. It traps nothing,
 * and it closes before anything beneath it.
 */
export function usePopup({ open, onDismiss }: PopupOptions): Popup {
  const panel = useRef<HTMLElement | null>(null);
  const inside = useRef<HTMLElement | null>(null);
  const dismiss = useRef(onDismiss);
  dismiss.current = onDismiss;
  const order = useOpeningOrder(open);

  useLayoutEffect(() => {
    if (!open) return;
    return register({
      kind: "popup",
      order,
      panel: () => panel.current,
      // A popup hands focus back itself (a menu to its trigger), and nothing
      // is ever opened from inside one: an action gives focus back first.
      returnTo: [],
      veil: () => null,
      inside: () => [inside.current],
      dismiss: (reason) => dismiss.current(reason),
      dismissable: () => true,
    });
  }, [open, order]);

  const panelRef = useCallback((el: HTMLElement | null) => {
    panel.current = el;
  }, []);
  const insideRef = useCallback((el: HTMLElement | null) => {
    inside.current = el;
  }, []);
  return { panelRef, insideRef, isTopmost: () => open && isTopmost(order) };
}
