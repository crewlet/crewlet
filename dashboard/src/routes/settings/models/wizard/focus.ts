/**
 * Where focus goes around the model wizard (design QA M6): to the step's
 * heading as it opens, to the first field a blocked Continue is about, to the
 * model's own heading after a save, and back to the control that opened the
 * wizard after a Cancel.
 *
 * THE WIZARD REPLACES THE PAGE THAT OPENED IT, so the control a person pressed
 * is gone by the time the wizard closes and a new one stands in its place.
 * It is remembered by what it is called, read while the opening page is still
 * on screen (the wizard's first render, before React commits it), and found
 * again by that name once the page is back.
 */

/** How the control that opened the wizard is found again: its role and its name. */
export interface Opener {
  selector: string;
  name: string;
}

const NAMED = "button, a[href], [role='button'], [role='menuitem']";

const nameOf = (el: Element) =>
  (el.getAttribute("aria-label") ?? el.textContent ?? "").replace(/\s+/g, " ").trim();

/**
 * The control focus is on now, as an Opener. A menu item closes with its
 * menu, so the menu's own trigger stands for it: that is where Escape on the
 * menu would have returned focus too.
 */
export function openerNow(): Opener | null {
  if (typeof document === "undefined") return null;
  let el: Element | null = document.activeElement;
  const menu = el?.closest("[role='menu']");
  if (menu?.id) el = document.querySelector(`[aria-controls="${CSS.escape(menu.id)}"]`) ?? el;
  if (!el || el === document.body || !el.matches(NAMED)) return null;
  const name = nameOf(el);
  return name ? { selector: NAMED, name } : null;
}

/** Calls `find` on each of the next frames until it answers, then focuses what it found. */
export function focusWhenShown(find: () => HTMLElement | null, frames = 30): void {
  let left = frames;
  const attempt = () => {
    const el = find();
    if (el) {
      el.focus();
      return;
    }
    if (--left > 0) requestAnimationFrame(attempt);
  };
  requestAnimationFrame(attempt);
}

/** The opener found again on the page that replaced the wizard, else the page's own heading. */
export function findOpener(opener: Opener | null): HTMLElement | null {
  if (opener) {
    const match = [...document.querySelectorAll<HTMLElement>(opener.selector)].find(
      (el) => nameOf(el) === opener.name,
    );
    if (match) return match;
  }
  return pageHeading();
}

/** The page's own heading, made focusable for the move rather than added to the tab order. */
export function pageHeading(): HTMLElement | null {
  const h1 = document.querySelector<HTMLElement>("main h1, h1");
  if (h1 && !h1.hasAttribute("tabindex")) h1.setAttribute("tabindex", "-1");
  return h1;
}

/** The first field inside `root` that says it is invalid. */
export function firstInvalid(root: HTMLElement | null): HTMLElement | null {
  return root?.querySelector<HTMLElement>("[aria-invalid='true']") ?? null;
}
