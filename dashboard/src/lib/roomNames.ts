/**
 * The company's rooms this reader may read, by name, read ONCE for every
 * surface in the frame, so a room written in prose (`#launch`) is drawn as the
 * room it is (`components/InlineToken.tsx`) and the room finder can offer one
 * nobody has spoken in (`routes/chatlet/Browse.tsx`).
 *
 * WHY A ROOM NEEDS ITS NAME. A room's name is any `[a-z0-9][a-z0-9_-]{0,63}`,
 * so `#e2e`, `#b2b` and `#2fa` are spelled exactly as a colour and `#2026` and
 * `#911` as an issue's number. Prose names rooms that way, a seat's and a
 * model's alike, and only the company's own rooms can say which a sentence
 * means: a name this holds is a room, and every other `#word` is read by its
 * shape, as it would be in a company with no chat of its own.
 *
 * WHAT IT READS: `chat_room_names`, every named room the reader may read,
 * joined or not (a public room and a unit's room are any seat's), the archived
 * ones included, since an archive closes a room to new messages and not to
 * being named. A room created since that answer is added from the live frames
 * this tab has seen (`state.chat.rooms`), which carry a room's name for every
 * room the reader may read. The answer is bounded (`chatlet.MaxRoomNames`,
 * every live room the company can hold, then the most recently archived); a
 * name past it reads by its shape.
 *
 * ONLY WHERE CHAT IS THE ENGINE'S AND THIS READER READS IT AS A SEAT, as every
 * Chatlet read in the frame is gated: one without the app reads no room, a
 * company on a vendor's chat or none has no room of the engine's to name, and
 * nothing is asked before the engine has said the company runs Chatlet
 * (`lib/services.ts`), and a credential acting as no seat is refused every
 * chat question. None outside the frame, for the reason `lib/linkHosts.ts`
 * gives.
 *
 * NOT POLLED, and asked again when the reader comes back to the tab: a room is
 * a rare, deliberate act, the frames carry the ones made meanwhile, and the
 * moment the set matters to prose is the next time somebody reads it.
 */

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import type { StoreState } from "~/protocol/index.ts";
import { useServicesAsk } from "./services.ts";
import { useSlice } from "./store-hooks.ts";
import { useQuery } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";

/** Every room this reader may read: its id, by its name in lower case. */
export type RoomNames = ReadonlyMap<string, string>;

const NONE: RoomNames = new Map();

const Rooms = createContext<RoomNames>(NONE);

/**
 * The named rooms this tab has seen a frame for, as one string: a frame in a
 * room already seen changes nothing here, so the set, and every run of prose
 * drawn against it, is rebuilt only when a room this tab had not seen appears.
 */
function sighted(state: StoreState): string {
  const seen: string[] = [];
  for (const [id, live] of Object.entries(state.chat.rooms)) {
    const name = live.changes[0]?.channel_name;
    if (name) seen.push(`${name.toLowerCase()}\u0001${id}`);
  }
  return seen.sort().join("\u0000");
}

export function RoomNamesProvider({ children }: { children: ReactNode }) {
  const viewer = useViewer();
  const ask = useServicesAsk();
  const enabled = ask.chat && viewer.chatlet && !viewer.unbound && !viewer.asking;
  const { data } = useQuery("chat_room_names", {}, { enabled, refetchOnFocus: true });
  const seen = useSlice(["chat"], sighted);
  const rooms = useMemo(() => {
    if (!enabled) return NONE;
    const out = new Map<string, string>();
    for (const room of data?.rooms ?? []) out.set(room.name.toLowerCase(), room.id);
    for (const entry of seen === "" ? [] : seen.split("\u0000")) {
      const [name = "", id = ""] = entry.split("\u0001");
      if (!out.has(name)) out.set(name, id);
    }
    return out.size === 0 ? NONE : out;
  }, [enabled, data, seen]);
  return createElement(Rooms.Provider, { value: rooms }, children);
}

/** The rooms this reader may read by name, or none outside the frame. */
export function useRoomNames(): RoomNames {
  return useContext(Rooms);
}
