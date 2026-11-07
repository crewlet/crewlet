/**
 * Open a DIRECT conversation with a seat in the company's own chat: an entry
 * of the seat page's and the seat panel's "More" menus.
 *
 * A DOOR, NOT THE WRITE: it opens chat's New direct message with the seat
 * already chosen (`#/chatlet?dm=`), whose Open is the `open_dm` press — and is
 * held with that press's reason, and on the reader's own seat, for the reason
 * Message is: a door onto a dialog that then refused teaches the reason one
 * press too late.
 *
 * DRAWN ONLY WHERE A ROOM CAN BE ([useDirectSeatAccess]): the company runs
 * the engine's own chat and the reader holds Chatlet. That is why it is not one
 * of `components/writes.tsx`'s controls, every one of which is drawn for every
 * reader and says why it cannot act: the viewer names the chat backend only to
 * a reader holding Chatlet, so to anybody else this screen cannot tell a company
 * with the engine's chat from one on a vendor's — where the conversation is
 * the vendor's and this engine holds no room for it — and a locked door to a
 * room that may not exist would be a claim nobody made.
 */

import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";
import { SendGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { menuHold, useDirectSeatAccess } from "~/lib/useWriteAccess.ts";

/**
 * The door as an entry of a seat's "More" menu, the seat page's and its
 * panel's: beside Message and Assign task it was a third button that pushed
 * Assign task onto a row of its own, held for most seats. None where no room
 * can be.
 */
export function useDirectMessageEntry(handle: string): PageMenuEntry[] {
  const access = useDirectSeatAccess(handle);
  const nav = useNavigator();
  if (access === null || !handle) return [];
  return [
    {
      key: "direct",
      label: "Direct message",
      icon: <SendGlyph size="sm" />,
      onSelect: () => nav.to(["chatlet"], { dm: handle }),
      ...menuHold(access),
    },
  ];
}
