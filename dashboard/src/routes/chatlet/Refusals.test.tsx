/**
 * A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS, on every
 * chat read that can be refused.
 *
 * A chat question asked as a principal that is no person's seat is refused
 * `bad_params` (`internal/api/queries/chat.go`'s `errNoChatSeat`) — the one
 * class whose sentence survives to the caller, and the remedy, a binding in
 * the identity directory, is only in the sentence. A list that dropped it drew
 * "this is the screen's bug to fix" in its place — the opposite of what is
 * true. And one asked without the Chatlet app is refused `unauthorized`, naming
 * the app, which is drawn as the access to ask for rather than as a fault.
 * The rail's own case is in `Chat.test.tsx`; these are the four other lists
 * the screen draws.
 */

import { cleanup, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";

import { QueryError } from "~/protocol/index.ts";
import { mountChat } from "~/test/chat.tsx";
import { LoneRoom } from "./testkit.tsx";

import { ReadCursors } from "./cursor.ts";
import { Mentions } from "./Mentions.tsx";
import { Search } from "./Search.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const SENTENCE = "bind its row in the identity directory to a human seat";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
};

const room = {
  ...served,
  channel: {
    v: 1,
    id: "room-1",
    kind: "public",
    name: "general",
    created_at: "2026-01-01T00:00:00Z",
  },
  revision: 1,
  message_seq: 1,
  member: true,
  members: [],
};

const cursors = () => new ReadCursors({ flush: async () => undefined });
const name = (handle: string) => handle;

const CASES: { list: string; hash: string; refused: string; draw: () => ReactNode }[] = [
  {
    list: "the mention feed",
    hash: "#/chatlet/mentions",
    refused: "chat_mentions",
    draw: () => <Mentions nameOf={name} />,
  },
  {
    list: "a room's transcript",
    hash: "#/chatlet/room-1",
    refused: "chat_messages",
    draw: () => <LoneRoom channelID="room-1" viewer="ada" nameOf={name} cursors={cursors()} />,
  },
  {
    list: "a thread",
    hash: "#/chatlet/room-1?thread=m-1",
    refused: "chat_thread",
    draw: () => <LoneRoom channelID="room-1" viewer="ada" nameOf={name} cursors={cursors()} />,
  },
  {
    list: "a search",
    hash: "#/chatlet/search?q=launch",
    refused: "chat_search",
    draw: () => <Search channels={[]} viewer="ada" nameOf={name} />,
  },
];

test.each(CASES)("$list says the engine's own sentence when refused", async (c) => {
  location.hash = c.hash;
  mountChat(c.draw(), {
    answer: async (what) => {
      if (what === c.refused) throw new QueryError("bad_params", null, SENTENCE);
      if (what === "chat_channel") return room;
      if (what === "chat_messages") return { ...served, channel_id: "room-1", messages: [] };
      return {};
    },
  });
  await waitFor(() => expect(screen.getByText(new RegExp(SENTENCE))).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});

test.each(CASES)("$list says which access it needs when refused on authority", async (c) => {
  location.hash = c.hash;
  mountChat(c.draw(), {
    answer: async (what) => {
      if (what === c.refused) {
        throw new QueryError("unauthorized", { reason: "no_grant", grants: ["chatlet"] });
      }
      if (what === "chat_channel") return room;
      if (what === "chat_messages") return { ...served, channel_id: "room-1", messages: [] };
      return {};
    },
  });
  await waitFor(() => expect(screen.getByText(/needs Chatlet access/)).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});
