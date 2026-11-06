/**
 * WHICH ELEMENT SCROLLS — declared once, because three unrelated things have
 * to find the same one.
 *
 * The shell owns the scroller: one element between the page bar and the
 * viewport edge, with every screen rendered inside it, and on every screen
 * that scrolls as a page it is what moves the screen. So "scroll to the top"
 * and "is the reader at the top" are questions about the SHELL's element
 * rather than about whatever is currently on it — which is everything this
 * module answers.
 *
 * IT IS NOT THE ONLY SCROLLER, and two others are worth knowing about before
 * reasoning about where a wheel goes or what `reveal` moves. A FULL-HEIGHT
 * screen (`useFillScreen`, whose doc in `app/fill.tsx` names the screens that
 * ask and when each withdraws) holds this element still: a canvas fills it and
 * pans rather than scrolls, and a two-column screen (Chatlet's rail and its
 * transcript among them) lets its own columns scroll — each exactly this
 * element's height, so `100cqb` is their view too, and on those screens THEY
 * are the page's scrollers. And a box inside the page may scroll its own
 * content (a running phase's transcript, a seat's thread list, a board lane, a
 * long record): none of those is the page, and each is bounded by the view and
 * hands the wheel back, at either end, to whichever of the two scrolls it (see
 * "The document does not scroll" in docs/reference/dashboard-design.md).
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
 * The element that scrolls `el` past the reader: its nearest ancestor whose
 * overflow scrolls vertically, else the shell's scroller.
 *
 * FOR A QUESTION ABOUT ONE ELEMENT'S POSITION, such as which heading is being
 * read. The shell's scroller is the page on most screens, and not on one that
 * scrolls a body of its own: an artifact's workspace scrolls its body inside
 * the shell, which holds still, so a reader measured against the shell's
 * scroller never moved and a doc's outline stayed on its first heading.
 */
export function scrollerOf(el: Element | null): HTMLElement | null {
  for (let at = el?.parentElement ?? null; at; at = at.parentElement) {
    const { overflowY } = getComputedStyle(at);
    if (overflowY === "auto" || overflowY === "scroll") return at;
  }
  return screenScroller();
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

/** How long a held scroll is re-applied while a page fills: two seconds of frames. */
const HOLD_FRAMES = 120;
/** How many frames a held scroll must stay put before it is left to the reader. */
const HELD_FRAMES = 3;
/** What a reader does that ends a hold: the page is theirs from then on. */
const TOUCHES = ["wheel", "touchstart", "pointerdown", "keydown"] as const;

/**
 * Holds `el` at the scroll `target` answers until the page holds it: re-applied
 * each frame until it has stayed put for a few frames, for at most two seconds,
 * and abandoned the moment the reader touches the page. Returns the cancel.
 *
 * ONE ASSIGNMENT LANDS SHORT on a page that fills late: the scroll is clamped
 * to the height that exists, and rows drawn above the place let the browser's
 * scroll anchoring carry it off. The target is asked each frame, so a place an
 * element names ([placeOf]) follows it as the page fills; a target answering
 * null (the element went with its screen) ends the hold.
 */
export function holdScroll(el: HTMLElement, target: () => number | null): () => void {
  let frames = 0;
  let still = 0;
  let live = true;
  const cancel = () => {
    live = false;
    for (const t of TOUCHES) el.removeEventListener(t, cancel);
  };
  const settle = () => {
    const want = live && frames++ <= HOLD_FRAMES ? target() : null;
    if (want === null) return cancel();
    el.scrollTop = want;
    still = Math.abs(el.scrollTop - want) <= 1 ? still + 1 : 0;
    if (still < HELD_FRAMES) requestAnimationFrame(settle);
    else cancel();
  };
  for (const t of TOUCHES) el.addEventListener(t, cancel, { once: true, passive: true });
  requestAnimationFrame(settle);
  return cancel;
}

/**
 * The scroll of `scroller` that puts `el` at its top, less the element's own
 * `scroll-margin-top` as `scrollIntoView` reads it; null once `el` has left
 * the page.
 */
export function placeOf(el: HTMLElement, scroller: HTMLElement): number | null {
  if (!el.isConnected) return null;
  const margin = parseFloat(getComputedStyle(el).scrollMarginTop) || 0;
  const offset = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
  return Math.max(0, scroller.scrollTop + offset - margin);
}
