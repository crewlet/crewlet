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

/**
 * The id of the page bar's LENS slot: beside the breadcrumb, before the
 * controls.
 */
export const PAGE_LENSES_SLOT = "page-lenses";

export function PageActions({ children }: { children: ReactNode }) {
  return (
    <PageSlot id={PAGE_ACTIONS_SLOT} fallback="row gap-2 wrap page-actions">
      {children}
    </PageSlot>
  );
}

/**
 * An object page's LENSES, rendered into the page bar beside its name.
 *
 * A lens is a different question about the one object the trail ends on — a
 * project's Items, About and History — so it belongs WITH the name it is a
 * lens on: `Work › ENG Core · Items | About | History`. Drawn as a row of its
 * own under the bar it cost the first screenful a line (46px, measured
 * against the approved Board, whose lanes start straight under two toolbar
 * rows) and read as a third toolbar over the work.
 *
 * Not the workspace's SECTION tabs, which are addresses of their own under
 * the bar ([SectionTabs]): a lens is a key on this object's address
 * (`lens=`), and the bar never draws both — sections belong to a section's
 * own page, lenses to an object's.
 */
export function PageLenses({ children }: { children: ReactNode }) {
  return (
    <PageSlot id={PAGE_LENSES_SLOT} fallback="page-lenses">
      {children}
    </PageSlot>
  );
}

/**
 * One slot of the page bar, found by id and portalled into; without a frame,
 * rendered in place inside `fallback`'s classes.
 */
function PageSlot({
  id,
  fallback,
  children,
}: {
  id: string;
  fallback: string;
  children: ReactNode;
}) {
  const [slot, setSlot] = useState<HTMLElement | null>(null);
  const [looked, setLooked] = useState(false);
  // BEFORE THE PAINT, because the bar is rendered by an ancestor in the same
  // commit: reading the DOM during render would find nothing on the very first
  // one, and reading it after the paint would show the fallback for a frame.
  useLayoutEffect(() => {
    setSlot(document.getElementById(id));
    setLooked(true);
  }, [id]);
  if (slot) return createPortal(children, slot);
  // Not yet looked: render nothing rather than the fallback, or every screen
  // in the application would paint its controls twice.
  if (!looked) return null;
  return <div className={fallback}>{children}</div>;
}
