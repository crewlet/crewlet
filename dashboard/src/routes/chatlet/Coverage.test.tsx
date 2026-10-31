/**
 * What each chat view tells the frame's state bar about its answer.
 *
 * THE RULE IS "THE ANSWER THIS SCREEN WAS DRAWN FROM" (`usePageCoverage`): the
 * rail on the bare screen, a room's transcript, each of the reader's views
 * (Activity, Threads, Saved, Drafts and sent), Browse rooms' directory, and
 * none for a search. The screen used to publish the RAIL's coverage on every
 * one of them, so an incomplete transcript or feed (a deferred record met the
 * read, and rows may be missing) was never shown, and the state bar described
 * a list the reader was not looking at. The state bar draws nothing on a
 * healthy answer, so the published fact is the only thing a suite can hold.
 */

import { cleanup, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { usePageCoverage } from "~/app/Shell.tsx";
import { GENERATION_STRIDE } from "~/contract/positions.ts";
import { mountChat } from "~/test/chat.tsx";
import { LoneRoom } from "./testkit.tsx";

import { ReadCursors } from "./cursor.ts";
import { mountScreen } from "./frameKit.tsx";

vi.mock("~/app/Shell.tsx", async () => ({
  ...(await vi.importActual<typeof import("~/app/Shell.tsx")>("~/app/Shell.tsx")),
  usePageCoverage: vi.fn(),
}));

beforeEach(() => {
  vi.mocked(usePageCoverage).mockClear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/**
 * An answer served at `seq` on the log's SECOND generation, complete or not:
 * a generation past the first, so a coverage that published the bare
 * sequence rather than the packed position reads as another point and fails.
 */
const served = (seq: number, complete = true) => ({
  read_level: "session",
  complete,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
});

/** What the state bar is handed for an answer served at `seq`: packed. */
const at = (seq: number) => GENERATION_STRIDE + seq;

const channel = { v: 1, id: "room-1", kind: "public", name: "general", created_at: "" };

/** The rail, served at 10 and complete. */
const rail = { ...served(10), read_state: true, channels: [{ channel, revision: 1, unread: 0 }] };

/** Every coverage the views published, the empty ones dropped. */
const published = () =>
  vi
    .mocked(usePageCoverage)
    .mock.calls.map(([coverage]) => coverage)
    .filter(Boolean);

test("a room publishes its transcript's coverage, an incomplete one included", async () => {
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom
      channelID="room-1"
      viewer="ada"
      nameOf={(handle: string) => handle}
      cursors={cursors}
    />,
    {
      answers: {
        chat_channel: { ...served(30), channel, revision: 1, message_seq: 0, member: true },
        chat_messages: { ...served(40, false), channel_id: "room-1", messages: [] },
      },
    },
  );
  await waitFor(() =>
    expect(published()).toContainEqual({
      read_level: "session",
      complete: false,
      applied_through: at(40),
    }),
  );
});

/** Each of the reader's views, at its address, with the answer it is drawn from. */
const VIEWS: { view: string; hash: string; answers: Record<string, unknown> }[] = [
  {
    view: "Activity",
    hash: "#/chatlet/activity",
    answers: {
      chat_activity: {
        ...served(50, false),
        items: [],
        unread: { mention: 0, reply: 0 },
        read_state: true,
      },
    },
  },
  {
    view: "Threads",
    hash: "#/chatlet/threads",
    answers: { chat_threads: { ...served(50, false), threads: [], read_state: true } },
  },
  {
    view: "Saved",
    hash: "#/chatlet/saved",
    answers: { chat_saved: { ...served(50, false), saved: [], read_state: true } },
  },
  {
    view: "Drafts and sent",
    hash: "#/chatlet/drafts",
    answers: { chat_sent: { ...served(50, false), messages: [] } },
  },
  {
    view: "Browse rooms",
    hash: "#/chatlet/browse",
    answers: { chat_rooms: { ...served(50, false), rooms: [] } },
  },
];

test.each(VIEWS)("$view publishes its own answer's coverage, not the rail's", async (c) => {
  mountScreen({ hash: c.hash, answers: { chat_channels: rail, ...c.answers } as never });
  await waitFor(() =>
    expect(published()).toContainEqual({
      read_level: "session",
      complete: false,
      applied_through: at(50),
    }),
  );
  expect(published()).not.toContainEqual(expect.objectContaining({ applied_through: at(10) }));
});

test("a search publishes none, as the work search does, not even the rail's", async () => {
  // A RANKING OVER THIS NODE'S OWN INDEX has no read level to state, and the
  // rail the screen also holds describes a list the reader is not looking at.
  mountScreen({
    hash: "#/chatlet/search?q=launch",
    answers: {
      chat_channels: rail,
      chat_search: { ...served(60), hits: [], searched: 2 },
    },
  });
  await screen.findByText(/No messages match/);
  await new Promise((r) => setTimeout(r, 20));
  expect(published()).toEqual([]);
});

test("the bare chat screen publishes the rail's", async () => {
  mountScreen({ answers: { chat_channels: rail } });
  await waitFor(() =>
    expect(published()).toContainEqual({
      read_level: "session",
      complete: true,
      applied_through: at(10),
    }),
  );
});

test("a room on the screen publishes its transcript's, and never the rail's", async () => {
  // A ROOM'S ADDRESS IS ITS UUID (`app/routes.ts`), never a name or a word.
  const id = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";
  const room = { ...channel, id };
  mountScreen({
    hash: `#/chatlet/${id}`,
    answers: {
      chat_channels: { ...rail, channels: [{ channel: room, revision: 1, unread: 0 }] },
      chat_channel: { ...served(30), channel: room, revision: 1, message_seq: 0, member: true },
      chat_messages: { ...served(40), channel_id: id, messages: [] },
    },
  });
  await waitFor(() =>
    expect(published()).toContainEqual(expect.objectContaining({ applied_through: at(40) })),
  );
  expect(published()).not.toContainEqual(expect.objectContaining({ applied_through: at(10) }));
});
