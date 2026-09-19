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
