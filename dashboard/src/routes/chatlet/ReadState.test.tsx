/**
 * A Chatlet list that is still being read, or could not be: the room's
 * transcript, a thread and a search's results. A failure the screen does not
 * ask again on its own offers Try again, and the box under a room stays put.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { QueryError } from "~/protocol/index.ts";
import { mountChat } from "~/test/chat.tsx";

import { ReadCursors } from "./cursor.ts";
import { mountScreen } from "./frameKit.tsx";
import { askedAgainByHand } from "./ReadState.tsx";
import { LoneRoom } from "./testkit.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};
const room = { ...served, channel, revision: 1, message_seq: 1, member: true, members: [] };

const said = {
  message: {
    v: 1,
    id: "m-1",
    channel_id: ROOM,
    author: "bo",
    author_kind: "agent",
    body: "the date moved to Thursday",
    created_at: "2026-01-01T00:00:00Z",
  },
  channel_seq: 1,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
};

type Answer = NonNullable<Parameters<typeof mountChat>[1]>["answer"];

const loneRoom = (answer: Answer) =>
  mountChat(
    <LoneRoom
      channelID="room-1"
      viewer="ada"
      nameOf={(handle) => handle}
      cursors={new ReadCursors({ flush: async () => undefined })}
    />,
    { answer },
  );

const CASES: {
  list: string;
  failing: string;
  mount: (answer: Answer) => unknown;
  then: unknown;
}[] = [
  {
    list: "a room's transcript",
    failing: "chat_messages",
    mount: loneRoom,
    then: { ...served, channel_id: "room-1", messages: [said] },
  },
  {
    list: "a thread",
    failing: "chat_thread",
    mount: (answer) => mountScreen({ hash: `#/chatlet/${ROOM}?pane=thread:${ROOM}:m-1`, answer }),
    then: { ...served, channel_id: ROOM, root: said, replies: [] },
  },
  {
    list: "a search",
    failing: "chat_search",
    mount: (answer) => mountScreen({ hash: "#/chatlet/search?q=thursday", answer }),
    then: {
      ...served,
      hits: [
        {
          message_id: "m-1",
          channel_id: ROOM,
          author: "bo",
          snippet: "the date moved to Thursday",
          at: "2026-01-01T00:00:00Z",
          score: 1,
        },
      ],
      searched: 1,
    },
  },
];

/** Answers every read, and fails the case's own list until it has been asked `fails` times. */
function answering(c: (typeof CASES)[number], fails: number) {
  const asked: string[] = [];
  const answer: Answer = async (what) => {
    asked.push(what);
    if (what === c.failing) {
      if (asked.filter((w) => w === what).length <= fails) throw new QueryError("query_failed");
      return c.then;
    }
    if (what === "chat_channel") return room;
    if (what === "chat_messages") return { ...served, channel_id: "room-1", messages: [] };
    if (what === "chat_channels") return { ...served, read_state: true, channels: [] };
    throw new QueryError("unknown_query");
  };
  return { asked, answer };
}

// Mutation: drop the button and a failed read is a dead end until a reload.
test.each(CASES)("$list that failed once offers Try again, which asks again", async (c) => {
  const { asked, answer } = answering(c, 1);
  c.mount(answer);
  const again = await screen.findByRole("button", { name: "Try again" });
  expect(screen.getByText(/the engine failed to answer/i)).toBeTruthy();
  const before = asked.filter((w) => w === c.failing).length;
  fireEvent.click(again);
  await waitFor(() => expect(asked.filter((w) => w === c.failing).length).toBe(before + 1));
  await waitFor(() => expect(screen.queryByText(/the engine failed to answer/i)).toBeNull());
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
});

test("a room that could not be read keeps its message box where it is", async () => {
  const { answer } = answering(CASES[0]!, Number.POSITIVE_INFINITY);
  const { view } = loneRoom(answer);
  await screen.findByRole("button", { name: "Try again" });
  const failed = view.container.querySelector(".chat-read-failed")!;
  const foot = view.container.querySelector(".chat-room-foot")!;
  // The failure stands in the transcript's place, before the foot, as one
  // flexible box, so the foot keeps the bottom of the room.
  expect(failed.parentElement).toBe(foot.parentElement);
  expect(failed.compareDocumentPosition(foot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

test("a room whose standing is not read yet draws no empty join bar", async () => {
  // THE ROOM'S DETAIL NEVER ANSWERS: membership is unknown, not "outside".
  // Mutation: draw the bar whenever membership is not known to be true.
  const { view } = loneRoom(async (what) => {
    if (what === "chat_channel") return new Promise(() => undefined);
    if (what === "chat_messages") return { ...served, channel_id: "room-1", messages: [said] };
    if (what === "chat_channels")
      return { ...served, read_state: true, channels: [{ channel, revision: 1, unread: 0 }] };
    throw new QueryError("unknown_query");
  });
  await screen.findByText("the date moved to Thursday");
  expect(view.container.querySelector(".chat-join-bar")).toBeNull();
});

test("a search still out claims no count of messages", async () => {
  // Mutation: count the hits held while the search is out, and the tab says 0.
  mountScreen({
    hash: "#/chatlet/search?q=thursday",
    answer: async (what) => {
      if (what === "chat_search") return new Promise(() => undefined);
      if (what === "chat_channels") return { ...served, read_state: true, channels: [] };
      throw new QueryError("unknown_query");
    },
  });
  const tab = await screen.findByRole("tab", { name: /^Messages/ });
  expect(tab.textContent).toBe("Messages");
});

test("only a failure the screen does not ask again itself is the reader's to retry", () => {
  expect(askedAgainByHand("query_failed")).toBe(true);
  expect(askedAgainByHand("timeout")).toBe(true);
  expect(askedAgainByHand("a_code_from_a_newer_node")).toBe(true);
  for (const code of ["unavailable", "closed", "unauthorized", "not_found", "bad_params"]) {
    expect(askedAgainByHand(code), code).toBe(false);
  }
});
