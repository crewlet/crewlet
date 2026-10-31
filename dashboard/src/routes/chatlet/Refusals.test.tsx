/**
 * A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS, on every
 * chat read that can be refused.
 *
 * A chat question asked as a principal that is no person's seat is refused
 * `bad_params` (`internal/api/queries/chat.go`'s `errNoChatSeat`), the one
 * class whose sentence survives to the caller, and the remedy, a binding in
 * the identity directory, is only in the sentence. A list that dropped it drew
 * "this is the screen's bug to fix" in its place, the opposite of what is
 * true. And one asked without the Chatlet app is refused `unauthorized`, naming
 * the app, which is drawn as the access to ask for rather than as a fault.
 * The rail's own case is in `Chat.test.tsx` and a search's in
 * `Search.test.tsx`; these are the other lists the screen draws.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { QueryError } from "~/protocol/index.ts";
import { mountChat } from "~/test/chat.tsx";
import { LoneRoom } from "./testkit.tsx";

import { ReadCursors } from "./cursor.ts";
import { mountScreen } from "./frameKit.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** A room's address is its uuid (`app/routes.ts`), never a name or a word. */
const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";

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

/** How a case mounts its list: as the app mounts the screen at its address, or a room alone. */
type Answer = NonNullable<Parameters<typeof mountChat>[1]>["answer"];

/** The screen at `hash`, as the app mounts it. */
const atAddress = (hash: string) => (answer: Answer) => mountScreen({ hash, answer });

/** A room's transcript, mounted on its own. */
const loneRoom = (answer: Answer) =>
  mountChat(<LoneRoom channelID="room-1" viewer="ada" nameOf={name} cursors={cursors()} />, {
    answer,
  });

/**
 * Drafts and sent opens on what the reader has not sent, which is this tab's
 * own and never refused, so its case asks for the Sent tab.
 */
const sentTab = (answer: Answer) => {
  atAddress("#/chatlet/drafts")(answer);
  void screen.findByRole("tab", { name: /^Sent/ }).then((tab) => fireEvent.click(tab));
};

const CASES: { list: string; refused: string; mount: (answer: Answer) => unknown }[] = [
  { list: "a room's transcript", refused: "chat_messages", mount: loneRoom },
  {
    list: "a thread",
    refused: "chat_thread",
    mount: atAddress(`#/chatlet/${ROOM}?pane=thread:${ROOM}:m-1`),
  },
  { list: "Activity", refused: "chat_activity", mount: atAddress("#/chatlet/activity") },
  { list: "Threads", refused: "chat_threads", mount: atAddress("#/chatlet/threads") },
  { list: "Saved", refused: "chat_saved", mount: atAddress("#/chatlet/saved") },
  { list: "Drafts and sent", refused: "chat_sent", mount: sentTab },
  { list: "Browse rooms", refused: "chat_rooms", mount: atAddress("#/chatlet/browse") },
];

/**
 * Every read but the refused one answers as a healthy room would, and a
 * question this suite does not answer is refused as a node that serves no
 * such question refuses it: never an empty object a view would read as an
 * answer of its shape.
 */
const answering =
  (refused: string, refusal: () => never): Answer =>
  async (what) => {
    if (what === refused) refusal();
    if (what === "chat_channel") return room;
    if (what === "chat_messages") return { ...served, channel_id: "room-1", messages: [] };
    if (what === "chat_channels") return { ...served, read_state: true, channels: [] };
    throw new QueryError("unknown_query");
  };

test.each(CASES)("$list says the engine's own sentence when refused", async (c) => {
  c.mount(
    answering(c.refused, () => {
      throw new QueryError("bad_params", null, SENTENCE);
    }),
  );
  await waitFor(() => expect(screen.getByText(new RegExp(SENTENCE))).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});

test.each(CASES)("$list says which access it needs when refused on authority", async (c) => {
  c.mount(
    answering(c.refused, () => {
      throw new QueryError("unauthorized", { reason: "no_grant", grants: ["chatlet"] });
    }),
  );
  await waitFor(() => expect(screen.getByText(/needs Chatlet access/)).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});
