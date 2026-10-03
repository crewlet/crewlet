/**
 * How a screen asks the shell to stop scrolling and hand it the height.
 *
 * THREE SCREENS NEED THIS, each for part of its life. Edit org's
 * visualization is a canvas, and a canvas fills the box its screen gives it
 * rather than growing the page — a wheel turned over a page that scrolls
 * lands in a canvas half off screen. The Inbox and a task's page are two
 * columns that each scroll on their own (a list and its pane; a task and its
 * rail), which they can only do inside a box of the window's height. Each
 * withdraws the request where it stops being true: the builder off its chart,
 * the other two below a phone's width, where they are one column and the page
 * scrolls as any other.
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
