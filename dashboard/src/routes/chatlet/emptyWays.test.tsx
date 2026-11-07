/**
 * A filter that empties a Chatlet list offers the way back as a control: the
 * search narrowed to a room or a person, and Activity's Unread only.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";

import { mountInFrame, mountScreen } from "./frameKit.tsx";
import { SearchView } from "./Search.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const ROOMS = {
  ...SERVED,
  rooms: [
    { channel: { v: 1, id: "room-1", kind: "public", name: "launch", created_at: "" }, members: 4 },
  ],
};

// Mutation: drop the action and a narrowed search with no match is a dead end
// with a sentence telling the reader which two fields to reset.
test("a narrowed search with no match offers Search every room, which widens it", async () => {
  const asked: Record<string, unknown>[] = [];
  const mounted = mountInFrame(<SearchView />, {
    hash: "#/chatlet/search?q=thursday&in=room-1",
    answer: async (what, params) => {
      if (what === "chat_search") {
        asked.push(params ?? {});
        return { hits: [], searched: 1, read_level: "session", position: SERVED.position };
      }
      if (what === "chat_rooms") return ROOMS;
      if (what === "chat_channels") return { ...SERVED, read_state: true, channels: [] };
      return {};
    },
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  const widen = await screen.findByRole("button", { name: "Search every room" });
  expect(asked.at(-1)).toMatchObject({ channel_id: "room-1" });
  fireEvent.click(widen);
  await waitFor(() => expect(asked.at(-1)?.channel_id).toBeUndefined());
});

test("Activity filtered to Unread only and empty offers Show everything", async () => {
  const asked: Record<string, unknown>[] = [];
  mountScreen({
    hash: "#/chatlet/activity",
    answer: async (what, params) => {
      if (what === "chat_activity") {
        asked.push(params ?? {});
        return { ...SERVED, items: [], unread: { mention: 0, reply: 0 }, read_state: true };
      }
      if (what === "chat_channels") {
        return {
          ...SERVED,
          read_state: true,
          channels: [
            {
              channel: { v: 1, id: "room-1", kind: "public", name: "launch", created_at: "" },
              revision: 1,
              unread: 0,
            },
          ],
        };
      }
      return {};
    },
  });
  fireEvent.click(await screen.findByRole("switch", { name: "Unread only" }));
  const all = await screen.findByRole("button", { name: "Show everything" });
  await waitFor(() => expect(asked.at(-1)).toMatchObject({ unread: true }));
  fireEvent.click(all);
  await waitFor(() => expect(asked.at(-1)?.unread).toBeUndefined());
  expect((screen.getByRole("switch", { name: "Unread only" }) as HTMLInputElement).checked).toBe(
    false,
  );
});
