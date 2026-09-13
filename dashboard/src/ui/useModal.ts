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
 *   back in. The one exception is focus inside a modal the stack does not
 *   know, such as the shell's token dialog: that surface owns its own keys.
 * - FOCUS GOES IN ON OPEN, unless something inside already took it (a field
 *   with `autoFocus`), and GOES BACK ON CLOSE to whatever held it when the
 *   modal first rendered. That is captured during render on purpose: by the
 *   time an effect runs, `autoFocus` has already moved focus into the modal,
 *   and a capture taken then would restore focus to an element that no longer
 *   exists.
 * - A VEIL CLOSES ONLY ITS OWN MODAL, and only when the press lands on the
 *   veil itself. The decision is taken on `pointerdown`, before any surface
 *   closes, so the press that dismisses a menu is never also read as a press
 *   on the veil beneath it.
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

/**
 * Whether focus is inside a modal this stack does not know about.
 *
 * The shell still hand-rolls two (the token dialog and the command palette),
 * and the token dialog is raised by a refused request, which can happen while
 * a drawer is open. That surface owns its keys: trapping Tab back into the
 * drawer, or closing the drawer on its Escape, would make it unusable.
 */
function inForeignModal(): boolean {
  const active = document.activeElement;
  const host = active instanceof Element ? active.closest("[aria-modal='true']") : null;
  return host !== null && !stack.some((entry) => entry.panel() === host);
}

function onKeyDown(e: KeyboardEvent): void {
  if (inForeignModal()) return;
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
    // A popup above the modal decides Tab for itself (a menu closes on it);
    // the trap only applies once focus is the modal's again.
    const popup = top("popup");
    if (popup && popup.order > modal.order) return;
    const inside = focusables(panel);
    const active = document.activeElement;
    const within = active instanceof Node && panel.contains(active);
    if (inside.length === 0) {
      e.preventDefault();
      panel.focus();
      return;
    }
    const first = inside[0]!;
    const last = inside[inside.length - 1]!;
    if (!within) {
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    } else if (e.shiftKey && (active === first || active === panel)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  }
}

function onPointerDown(e: PointerEvent): void {
  const entry = top();
  if (!entry || !(e.target instanceof Node)) return;
  const target = e.target;
  if (entry.kind === "modal") {
    if (target === entry.veil() && entry.dismissable()) entry.dismiss("outside");
    return;
  }
  const inside = [entry.panel(), ...entry.inside()];
  if (inside.some((el) => el?.contains(target))) return;
  if (entry.dismissable()) entry.dismiss("outside");
}

function register(entry: Entry): () => void {
  if (stack.length === 0) {
    document.addEventListener("keydown", onKeyDown);
    // CAPTURE, so the decision is made before any handler on the page reacts
    // to the press and before a surface that closes because of it unmounts.
    document.addEventListener("pointerdown", onPointerDown, true);
  }
  stack.push(entry);
  return () => {
    const at = stack.indexOf(entry);
    if (at >= 0) stack.splice(at, 1);
    if (stack.length === 0) {
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("pointerdown", onPointerDown, true);
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

/** Whether `entry` is the surface a key press or a press would reach now. */
function isTopmost(order: number): boolean {
  return top()?.order === order;
}

export interface ModalOptions {
  onClose: () => void;
  /** False while a request is in flight: Escape and the veil stop closing. */
  dismissable?: boolean;
}

export interface Modal {
  /** The dialog element: focus goes into it and Tab stays in it. */
  panelRef: (el: HTMLElement | null) => void;
  /** The veil behind it: a press on the veil itself closes the modal. */
  veilRef: (el: HTMLElement | null) => void;
}

/** A modal surface: a dialog or a drawer. Mount it only while it is open. */
export function useModal({ onClose, dismissable = true }: ModalOptions): Modal {
  const panel = useRef<HTMLElement | null>(null);
  const veil = useRef<HTMLElement | null>(null);
  const close = useRef(onClose);
  const canClose = useRef(dismissable);
  close.current = onClose;
  canClose.current = dismissable;
  const order = useOpeningOrder(true);
  // WHERE FOCUS CAME FROM, read during the first render. See the module doc
  // for why an effect is too late.
  const [opener] = useState<Element | null>(() =>
    typeof document === "undefined" ? null : document.activeElement,
  );

  useLayoutEffect(
    () =>
      register({
        kind: "modal",
        order,
        panel: () => panel.current,
        veil: () => veil.current,
        inside: () => [],
        dismiss: () => close.current(),
        dismissable: () => canClose.current,
      }),
    [order],
  );

  useEffect(() => {
    const root = panel.current;
    if (root && !root.contains(document.activeElement)) {
      // The first control, or the panel itself when it has none, so a screen
      // reader announces the label rather than continuing behind the veil.
      (focusables(root)[0] ?? root).focus();
    }
    return () => {
      if (opener instanceof HTMLElement && opener.isConnected && opener !== document.body) {
        opener.focus();
      }
    };
  }, [opener]);

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
