/**
 * WHICH ELEMENT SCROLLS — declared once, because three unrelated things have
 * to find the same one.
 *
 * The shell owns the scroller: one element between the page bar and the
 * viewport edge, with every screen rendered inside it. Nothing a screen
 * renders scrolls on its own, so "scroll to the top" and "is the reader at the
 * top" are questions about the SHELL's element rather than about whatever is
 * currently on it.
 *
 * It had two spellings. The router restored a remembered position through
 * `getElementById("screen-scroll")` and the settled-list hook asked whether
 * the reader was reading through `querySelector(".screen")` — the same node
 * today, by coincidence of markup, and the class is also the STYLING hook, so
 * a layout change that moved the padding onto an inner wrapper and left the
 * class where it was would have silently split them: the router would have
 * gone on restoring positions while the hook read `scrollTop` off an element
 * that never scrolls, decided the reader was at the top, and merged rows
 * under them — the exact failure the hook exists to prevent, reported as
 * nothing at all.
 *
 * So the id is what identifies it and the class is only what paints it: an id
 * cannot be applied to a second element to pick up a style, which is precisely
 * why a selector for identity must not double as one for appearance.
 */

/** The id the shell puts on its scroller. */
export const SCREEN_SCROLL_ID = "screen-scroll";

/**
 * The shell's scroller, or nil where there is no shell — a hook rendered
 * bare in a test, a screen mounted before the shell exists. Every caller
 * treats the absence as "nothing to scroll" rather than as a fault, because
 * neither of those is one.
 */
export function screenScroller(): HTMLElement | null {
  return document.getElementById(SCREEN_SCROLL_ID);
}

/**
 * Bring `el` on screen when it is not — and, when asked, put focus on it.
 *
 * ON SCREEN MEANS INSIDE THE SHELL'S SCROLLER, not the window: the page bar
 * sits above the scroller, so an element behind the bar is off screen however
 * the window's own box reads it. An element already in view is left exactly
 * where it is, and so is focus — a press that moved the page with nothing to
 * show would be a jump with no reason. With no shell (a screen rendered bare
 * in a test) the window is the view.
 *
 * `focus` is for a press whose only perceivable result is the thing revealed —
 * a thread chosen from a stacked list, whose detail is a screen below it — so
 * a keyboard or a screen reader lands where it opened. Leave it off where the
 * reader is still stepping through the list they pressed in, as a revision
 * picked on Configuration's Diff lens is.
 */
export function reveal(el: HTMLElement | null, { focus = false }: { focus?: boolean } = {}): void {
  if (!el) return;
  const box = el.getBoundingClientRect();
  const view = screenScroller()?.getBoundingClientRect();
  const top = view ? view.top : 0;
  const bottom = view ? view.bottom : window.innerHeight;
  if (box.top >= top && box.bottom <= bottom) return;
  el.scrollIntoView({ block: "start" });
  if (focus) el.focus({ preventScroll: true });
}
