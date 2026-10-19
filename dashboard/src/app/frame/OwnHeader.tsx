/**
 * A screen that draws its own head in the frame's place: the page bar, the
 * trail and the section tabs give way to one bar the screen renders.
 *
 * ONE SCREEN DOES, AND FOR A REASON THE OTHERS DO NOT HAVE. An open artifact
 * is read the way a document is opened in its own application: the artifact
 * is the screen, and every pixel of chrome above it is a pixel it is drawn
 * smaller in. The frame's bar, an object header and a tab strip stacked over
 * it cost a deck a third of a laptop's height, so the artifact's page draws a
 * single slim bar instead (`routes/artifacts/ArtifactPage.tsx`, the kit's
 * `ArtifactBar`). Every other object keeps the frame's head, where the trail
 * and the tabs are how a reader moves between an object's readings.
 *
 * A PORTAL, for the reason `PageActions` is one: what the bar holds is decided
 * by what the screen loaded, so it is written where it is decided and lands
 * where it belongs. The frame keeps what is the frame's: the navigation
 * toggle on a narrow window is the screen's to place (it is the bar's first
 * control, `AppShell.Toggle`), and the state bar still stands under the head.
 *
 * WITHOUT A FRAME IT RENDERS IN PLACE, so a screen mounted alone (a test, a
 * fixture) still owns its head, and with one it renders nothing until the
 * frame has drawn the slot, so the bar never paints in the screen first and
 * jumps.
 */

import { createContext, useContext, useLayoutEffect, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { AppShell } from "@crewlethq/ui";

/**
 * Where the head lands: the element the frame drew for it, `null` while the
 * frame has not drawn it yet, `undefined` where there is no frame at all.
 */
export const OwnHeaderSlot = createContext<HTMLElement | null | undefined>(undefined);

/** Set by the frame: a screen says it draws its own head while it is mounted. */
export const OwnHeaderRequest = createContext<(own: boolean) => void>(() => {});

export function OwnPageHeader({ children }: { children: ReactNode }) {
  const request = useContext(OwnHeaderRequest);
  const slot = useContext(OwnHeaderSlot);
  // BEFORE THE PAINT: the frame swaps its bar for the slot in the same frame
  // the screen first draws, so the reader never sees both heads.
  useLayoutEffect(() => {
    request(true);
    return () => request(false);
  }, [request]);
  if (slot === undefined) return <>{children}</>;
  return slot ? createPortal(children, slot) : null;
}

/**
 * The frame's navigation toggle, for the screen's own head to place first:
 * the shell shows it below its breakpoint, where the sidebar is a drawer.
 * Nothing outside a frame, where there is no sidebar to open.
 */
export function FrameNavToggle() {
  return useContext(OwnHeaderSlot) === undefined ? null : <AppShell.Toggle />;
}
