/**
 * How a screen asks the shell to stop scrolling and hand it the height.
 *
 * FIVE SCREENS NEED THIS, four of them for part of their life. Two are
 * canvases — Edit org's visualization and the org chart — and a canvas fills
 * the box its screen gives it rather than growing the page: a wheel turned
 * over a page that scrolls lands in a canvas half off screen. Two are two
 * columns that each scroll on their own — the Inbox (a list and its pane) and
 * a task's page (a task and its rail) — which they can only do inside a box
 * of the window's height. Each withdraws the request where it stops being
 * true: the builder off its chart, and the other three below a phone's width,
 * where the chart is rows and the columns are one, and the page scrolls as any
 * other.
 *
 * Chat asks for the whole of its life, and for the same reason one level
 * worse: a transcript that scrolls inside a page that also scrolls is two
 * scrollers under one wheel, and the one that moves is whichever the pointer
 * is over — so a reader loses their place in a conversation by looking at it.
 * A phone is no exception: there the rail stacks above the room and each
 * takes its share of the window, rather than the page growing past the
 * composer.
 *
 * WHY A REQUEST RATHER THAN A STYLESHEET. This used to be a rule the shell's
 * own sheet carried, matching a class deep inside the screen
 * (`.screen-inner:has(.org-builder-body.fill)`), which meant the shell's
 * layout was decided by a selector in another package's file naming a class
 * neither of them owned. The design system asks for the answer instead
 * ([AppShell]'s `fill`), and a prop is a value somebody has to pass: the
 * screen that knows says so, the shell that draws reads it, and nothing in
 * between has to agree about a class name to keep the two joined.
 */

import { createContext, useContext, useEffect } from "react";

/** Set by the shell; a no-op wherever a screen renders without one. */
export const FillRequest = createContext<(on: boolean) => void>(() => {});

/**
 * Ask the shell for the window's remaining height while `on` is true.
 *
 * The request is withdrawn when it turns false and when the screen unmounts,
 * so a reader who leaves the canvas view, or the screen, gets the scroller
 * back. A screen that asked and never withdrew would leave every screen after
 * it unable to scroll.
 */
export function useFillScreen(on: boolean): void {
  const request = useContext(FillRequest);
  useEffect(() => {
    request(on);
    return () => request(false);
  }, [request, on]);
}
