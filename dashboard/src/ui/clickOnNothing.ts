/**
 * A click on nothing: the reader putting down what they picked up.
 *
 * # Why it exists
 *
 * A node selected in the org builder, or a row or card whose peek was open,
 * stayed picked up wherever the reader clicked next: its card in the accent,
 * its row barred, the peek still beside the list. The only way to put it down
 * was to pick something else up or to find the one control that closes it.
 * Every canvas and list a reader has used lets go on a click on empty space,
 * and this is that rule, written once for every surface that holds something,
 * because two copies of "what counts as nothing" are how one of them comes to
 * forget the menus.
 *
 * # What counts as nothing
 *
 * Anything that is not a thing the reader can act on or pick up: not a control,
 * not an item that selects itself (a tree item, a chart card, a grid row), not
 * anything drawn under the pointing hand (a table row that opens a peek is a
 * plain row with a click handler, and the hand is the one thing every such
 * row shows), not inside a menu, a listbox, a dialog or a tooltip, and not
 * inside whatever the caller says holds the thing itself (the peek's own
 * rail). Without the hand, a click on a second row would move the peek and
 * then close it. And never:
 *
 * - while a modal surface is open, because the page under a modal is not the
 *   reader's to click;
 * - a click that ends a text selection, which is the reader taking words, not
 *   letting go of anything;
 * - the end of a pan, which is no click at all: the canvas swallows the one a
 *   drag would otherwise leave.
 */

import { useEffect, useRef } from "react";
import { isModalLayerOpen } from "@crewlethq/ui";

/** What a click can land on without being a click on nothing. */
const KEEPS = [
  "[role='treeitem']",
  ".crewlet-tree-canvas__card",
  "[role='row'][aria-level]",
  "button",
  "a[href]",
  "input",
  "select",
  "textarea",
  "label",
  "[contenteditable='true']",
  "[role='menu']",
  "[role='listbox']",
  "[role='dialog']",
  "[role='tooltip']",
].join(", ");

/**
 * Whether `event` is a primary click on nothing. `keep` names what else holds
 * the thing being put down, so a click inside it is not on nothing either.
 */
export function isClickOnNothing(event: MouseEvent, keep?: string): boolean {
  if (event.defaultPrevented || event.button !== 0) return false;
  if (isModalLayerOpen()) return false;
  const target = event.target instanceof Element ? event.target : null;
  if (!target || !target.isConnected) return false;
  if (target.closest(keep ? `${KEEPS}, ${keep}` : KEEPS) !== null) return false;
  if (drawnPressable(target)) return false;
  const words = window.getSelection?.();
  return !words || words.isCollapsed;
}

/** Whether `el`, or anything it sits in, is drawn under the pointing hand. */
function drawnPressable(el: Element): boolean {
  for (let at: Element | null = el; at !== null && at !== document.body; at = at.parentElement) {
    if (getComputedStyle(at).cursor === "pointer") return true;
  }
  return false;
}

/**
 * Calls `onNothing` on every click on nothing, for as long as it is given.
 *
 * NULL WHILE NOTHING IS HELD, so a page with no selection and no peek listens
 * to no click at all. Heard on the whole document rather than on a surface,
 * because the page under a short table or a narrow chart is the application's
 * rather than the surface's.
 */
export function useClickOnNothing(onNothing: (() => void) | null, keep?: string): void {
  const latest = useRef(onNothing);
  latest.current = onNothing;
  const holding = onNothing !== null;
  useEffect(() => {
    if (!holding) return;
    function onClick(event: MouseEvent) {
      if (isClickOnNothing(event, keep)) latest.current?.();
    }
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, [holding, keep]);
}
