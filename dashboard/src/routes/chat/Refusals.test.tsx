/**
 * A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS, on every
 * chat read that can be refused.
 *
 * Every chat question refuses a credential that is nobody's seat as
 * `bad_params`, and that class was chosen BECAUSE it is the one whose sentence
 * survives to the caller (`internal/api/queries/chat.go`'s `errNoChatSeat`):
 * the remedy is a field of company configuration, and only the sentence names
 * it. A list that dropped it drew "this is the screen's bug to fix" in its
 * place — the opposite of what is true. The rail's own case is in
 * `Chat.test.tsx`; these are the four other lists the screen draws.
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

const SENTENCE = "give a seat contact.crewlet_operator_id matching this token";

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
    hash: "#/chat/mentions",
    refused: "chat_mentions",
    draw: () => <Mentions nameOf={name} />,
  },
  {
    list: "a room's transcript",
    hash: "#/chat/room-1",
    refused: "chat_messages",
    draw: () => <LoneRoom channelID="room-1" viewer="ada" nameOf={name} cursors={cursors()} />,
  },
  {
    list: "a thread",
    hash: "#/chat/room-1?thread=m-1",
    refused: "chat_thread",
    draw: () => <LoneRoom channelID="room-1" viewer="ada" nameOf={name} cursors={cursors()} />,
  },
  {
    list: "a search",
    hash: "#/chat/search?q=launch",
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
