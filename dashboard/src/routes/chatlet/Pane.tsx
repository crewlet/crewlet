/**
 * The pane beside the conversation: a thread, a room's details, a seat's
 * profile, or a catch up. One at a time, because the pane is one address
 * parameter (`nav.tsx`).
 *
 * # Each pane is the kit's SidePanel
 *
 * Every pane draws through `SidePanel`, which already does what a pane owes a
 * keyboard reader: opening moves focus to its heading (so a screen reader
 * announces what opened), Escape from anywhere inside closes it unless a
 * control inside answered the key first, and closing with focus inside hands
 * focus back to whatever held it before. This module adds the two things a
 * panel cannot know about the screen it sits on.
 *
 * THE OPENER OUTLIVES A PANE OVER A PANE. A member's profile opened from the
 * Members tab replaces the details pane, so the profile's own "before" is a
 * row in a pane that is gone by the time the profile closes. The host keeps
 * the control that opened the FIRST pane (the last one focused outside every
 * pane, menu and dialog: a menu item that opened a pane is gone once the menu
 * closes, its trigger is not) and returns focus to it when the last pane
 * closes and focus has nowhere else to be.
 *
 * ESCAPE IS THE PANEL'S, from inside it. Focus moves into the pane when it
 * opens, so the key a reader presses next is heard there; a key heard at the
 * window would be a binding outside the dashboard's one keymap
 * (`app/keymap.ts`), which nothing else may add.
 */

import { useEffect, useLayoutEffect, useRef } from "react";

// THE PANES', DIALOGS' AND VIEWS' OWN SHEET, arriving with the chat chunk this
// module is in (`styles/classes.test.ts`' LAZY_SHEETS).
import "~/styles/chatlet-panes.css";
import type { ChatPane } from "./nav.tsx";
import { CatchUpPane } from "./CatchUp.tsx";
import { DetailsPane } from "./Details.tsx";
import { ProfilePane } from "./Profile.tsx";
import { ThreadPane } from "./Thread.tsx";

/** The box every pane is drawn in. */
const HOST_CLASS = "chat-pane-host";

/**
 * Layers whose controls are gone once they close: focus in one is never the
 * pane's opener, the trigger that opened the layer is.
 */
const LAYER = '[role="menu"], [role="dialog"], [role="alertdialog"], [role="listbox"]';

/** A pane's identity: a new one is a new panel, a new tab of the same is not. */
function paneKey(pane: ChatPane): string {
  switch (pane.kind) {
    case "thread":
      return `thread:${pane.room}:${pane.root}`;
    case "details":
      return `details:${pane.room}`;
    case "profile":
      return `profile:${pane.handle}`;
    case "catchup":
      return `${pane.mode}:${pane.room}`;
  }
}

export function ChatPaneHost({ pane, onClose }: { pane: ChatPane | null; onClose: () => void }) {
  useOpenerReturn(pane !== null);
  if (!pane) return null;
  const key = paneKey(pane);
  return (
    // DRAWS NO BOX of its own (`display: contents`), so the panel inside is
    // the frame's grid item; it is here so focus inside a pane is told apart
    // from focus anywhere else.
    <div className={HOST_CLASS}>
      {pane.kind === "thread" ? (
        <ThreadPane key={key} room={pane.room} root={pane.root} onClose={onClose} />
      ) : pane.kind === "details" ? (
        <DetailsPane key={key} room={pane.room} tab={pane.tab} onClose={onClose} />
      ) : pane.kind === "profile" ? (
        <ProfilePane key={key} handle={pane.handle} onClose={onClose} />
      ) : (
        <CatchUpPane key={key} room={pane.room} mode={pane.mode} onClose={onClose} />
      )}
    </div>
  );
}

/**
 * Keep the control that opened the first pane, and give focus back to it when
 * the last pane closes with focus left nowhere.
 */
function useOpenerReturn(open: boolean): void {
  const lastOutside = useRef<HTMLElement | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  const was = useRef(open);

  useEffect(() => {
    const onFocus = (event: FocusEvent) => {
      const target = event.target;
      if (!(target instanceof HTMLElement)) return;
      // BY ITS CLASS, never by a ref: a panel focuses its heading in its own
      // layout effect, which runs before the host's ref is attached.
      if (target.closest(`.${HOST_CLASS}`) || target.closest(LAYER)) return;
      lastOutside.current = target;
    };
    // HEARD IN THE CAPTURE PHASE of `focus`, which every focus raises, rather
    // than `focusin`, which a focus moved by script does not raise everywhere.
    document.addEventListener("focus", onFocus, true);
    return () => document.removeEventListener("focus", onFocus, true);
  }, []);

  // A LAYOUT EFFECT, after the panel's own cleanup has run on the commit
  // that closed it: by then the panel has handed focus back if it could.
  useLayoutEffect(() => {
    const before = was.current;
    was.current = open;
    if (open && !before) {
      opener.current = lastOutside.current;
      return;
    }
    if (!open && before) {
      const active = document.activeElement;
      const lost = active === null || active === document.body;
      if (lost && opener.current?.isConnected) opener.current.focus({ preventScroll: true });
      opener.current = null;
    }
  }, [open]);
}
