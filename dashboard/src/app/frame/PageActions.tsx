/**
 * A screen's own controls, rendered into the page bar.
 *
 * A PORTAL RATHER THAN A HOOK, deliberately. The controls a screen offers are
 * conditional on what it loaded — a node's id once the fleet answers, a
 * project's key once the board does — and a hook that published them would
 * have to be called unconditionally with a value that is usually null, or be
 * called conditionally and break the rules of hooks. A portal is written where
 * the controls are decided and lands where they belong.
 *
 * # Without a frame it renders in place
 *
 * A screen mounted outside the shell — a test, a fixture, a storybook — still
 * OWNS its controls, and a portal with nowhere to land would silently drop
 * them. So the fallback is to render where they were written, which is the
 * honest answer to "there is no page bar": put them on the page.
 *
 * The slot is looked up in a LAYOUT effect rather than an ordinary one, which
 * is what keeps that fallback from flickering in the real application: the bar
 * is an ancestor in the same commit, so the lookup succeeds before the browser
 * paints and the controls never appear on the page first and jump.
 */

import { useLayoutEffect, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";

/** The id the page bar's own slot carries. */
export const PAGE_ACTIONS_SLOT = "page-actions";

export function PageActions({ children }: { children: ReactNode }) {
  const [slot, setSlot] = useState<HTMLElement | null>(null);
  const [looked, setLooked] = useState(false);
  // BEFORE THE PAINT, because the bar is rendered by an ancestor in the same
  // commit: reading the DOM during render would find nothing on the very first
  // one, and reading it after the paint would show the fallback for a frame.
  useLayoutEffect(() => {
    setSlot(document.getElementById(PAGE_ACTIONS_SLOT));
    setLooked(true);
  }, []);
  if (slot) return createPortal(children, slot);
  // Not yet looked: render nothing rather than the fallback, or every screen
  // in the application would paint its controls twice.
  if (!looked) return null;
  return <div className="row gap-1 wrap page-actions">{children}</div>;
}
