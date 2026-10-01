/**
 * The application's one polite live region: where the design system's own
 * controls say what a gesture did.
 *
 * # Nothing mounted it, so nothing was said
 *
 * uilet's list and tag controls announce every change they make — "Added SRE",
 * "Moved fast to position 1 of 2" — through one module-level `announce()`, and
 * that function speaks only into a mounted `<Announcer>`. The package says to
 * mount one in the shell; this application never did. So every add, remove
 * and reorder in the org builder's node editor was silent to a screen reader,
 * and each one printed "announce() was called with no <Announcer> mounted"
 * into the console of the browser that made it.
 *
 * ONE, AND ABOVE THE FRAME. The package keeps a single sentence for the whole
 * page, so two mounted regions would each say it and a reader would hear every
 * change twice. And it sits beside the frame rather than in the shell, because
 * the frame swaps the shell for the sign-in screens and back: a region mounted
 * inside it would be torn down by the very navigation a sign-in causes, and
 * the package resets its sentence when the last region goes.
 *
 * # It follows the fullscreen element
 *
 * The org builder can take its container fullscreen, and a fullscreen element
 * renders only its own subtree — which is why the builder carries its own
 * layer host, toast outlet and live region inside that container
 * (`routes/org/builder/Builder.tsx`). Its node editor is where these
 * announcements come from, so while an element is fullscreen the region is
 * PORTALLED into it: the one region, moved to where the reader is, rather
 * than a second one inside the builder that would repeat every sentence the
 * moment it left fullscreen.
 */

import { Announcer } from "@crewlethq/ui";
import { useSyncExternalStore } from "react";
import { createPortal } from "react-dom";

/**
 * What the region is called, for a reader browsing landmarks.
 *
 * NOT THE PACKAGE'S DEFAULT. `<Announcer>` defaults to "Notifications", which
 * is also what the toast provider calls its own polite region — so a reader
 * listing the page's regions met two "Notifications" and no way to tell the
 * one that reports a write's outcome from the one that reports a control's
 * edit.
 */
export const ANNOUNCER_LABEL = "Announcements";

function subscribe(onChange: () => void): () => void {
  document.addEventListener("fullscreenchange", onChange);
  return () => document.removeEventListener("fullscreenchange", onChange);
}

/** The element the page is showing fullscreen, or null; jsdom has neither. */
function fullscreenElement(): Element | null {
  return document.fullscreenElement ?? null;
}

export function AppAnnouncer() {
  const host = useSyncExternalStore(subscribe, fullscreenElement, () => null);
  const region = <Announcer label={ANNOUNCER_LABEL} />;
  return host ? createPortal(region, host) : region;
}
