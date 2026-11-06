/**
 * The Rooms panel slid in from Menu, over the screen at an address.
 *
 * THE INVARIANTS: the slid-in panel belongs to the place it was opened over,
 * so a move made any other way than a pick in it (Back, the app sidebar, a
 * link) closes it as a pick does; and its sections call a room a room.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { QueryError } from "~/protocol/index.ts";
import { SERVED } from "~/test/chatAccess.ts";
import { actEngine } from "~/test/chat.tsx";

import { mountScreen } from "./frameKit.tsx";
import { resetDockForTest } from "./nav.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetDockForTest();
  location.hash = "#/";
});

const GENERAL = {
  channel: {
    v: 1,
    id: "room-1",
    kind: "public",
    name: "general",
    created_at: "2026-01-01T00:00:00Z",
  },
  revision: 1,
  unread: 0,
  mentions: 0,
};

/** The screen at `hash`, its reader in one public room. */
function mountAt(hash: string) {
  actEngine();
  return mountScreen({
    hash,
    answer: async (what) => {
      switch (what) {
        case "chat_channels":
          return { ...SERVED, read_state: true, channels: [GENERAL] };
        case "chat_activity":
          return { ...SERVED, items: [], unread: { mention: 0, reply: 0 }, read_state: true };
        case "chat_rooms":
          return { ...SERVED, rooms: [] };
        default:
          throw new QueryError("unknown_query");
      }
    },
  });
}

/** Slide the panel in from Menu. */
async function openPanel(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: /^Menu/ }));
  return screen.findByRole("dialog", { name: "Rooms" });
}

// Mutation: drop the sheet's close on a change of address and this goes red,
// the panel left open and dimming the new place.
test("the slid-in panel closes when the address moves under it", async () => {
  mountAt("#/chatlet/activity");
  await openPanel();
  // AS BACK, THE APP SIDEBAR OR A LINK MOVE IT: not a pick in the panel.
  act(() => {
    location.hash = "#/chatlet/browse";
  });
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Rooms" })).toBeNull());
  expect(await screen.findByRole("heading", { level: 1, name: "Browse rooms" })).toBeTruthy();
});

// ONE WORD FOR A ROOM. Mutation: head the named rooms "Channels" again and
// this goes red.
test("the named rooms are listed under Rooms", async () => {
  mountAt("#/chatlet/activity");
  const panel = await openPanel();
  await within(panel).findByText("general");
  expect(within(panel).getByRole("button", { name: /^Rooms/ })).toBeTruthy();
  expect(within(panel).queryByText("Channels")).toBeNull();
});
