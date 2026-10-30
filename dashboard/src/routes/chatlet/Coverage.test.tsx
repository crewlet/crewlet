/**
 * What each chat view tells the frame's state bar about its answer.
 *
 * THE RULE IS "THE ANSWER THIS SCREEN WAS DRAWN FROM" (`usePageCoverage`), and
 * chat has four: the rail, a room's transcript, the mention feed and a search.
 * The screen used to publish the RAIL's coverage on every one of them, so an
 * incomplete transcript or mention feed — a deferred record met the read, and
 * rows may be missing — was never shown, and the state bar described a list
 * the reader was not looking at. The state bar draws nothing on a healthy
 * answer, so the published fact is the only thing a suite can hold.
 */

import { cleanup, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { usePageCoverage } from "~/app/Shell.tsx";
import { GENERATION_STRIDE } from "~/contract/positions.ts";
import { mountChat } from "~/test/chat.tsx";
import { LoneRoom } from "./testkit.tsx";

import { Chat } from "./Chat.tsx";
import { ReadCursors } from "./cursor.ts";
import { Mentions } from "./Mentions.tsx";

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
 * An answer served at `seq` on the log's SECOND generation, complete or not —
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
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
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

test("the mention feed publishes its own coverage", async () => {
  mountChat(<Mentions nameOf={(handle) => handle} />, {
    answers: { chat_mentions: { ...served(50, false), handle: "ada", mentions: [] } },
  });
  await waitFor(() =>
    expect(published()).toContainEqual({
      read_level: "session",
      complete: false,
      applied_through: at(50),
    }),
  );
});

test("a search publishes none, as the work search does — not even the rail's", async () => {
  // A RANKING OVER THIS NODE'S OWN INDEX has no read level to state, and the
  // rail the screen also holds describes a list the reader is not looking at.
  location.hash = "#/chatlet/search?q=launch";
  mountChat(<Chat view="search" />, {
    answers: {
      chat_channels: rail,
      chat_search: { ...served(60), hits: [], searched: 2 },
    },
  });
  await screen.findByText(/Nothing matched/);
  await new Promise((r) => setTimeout(r, 20));
  expect(published()).toEqual([]);
});

test("the bare chat screen publishes the rail's, and a room on it never does", async () => {
  mountChat(<Chat />, { answers: { chat_channels: rail } });
  await waitFor(() =>
    expect(published()).toContainEqual({
      read_level: "session",
      complete: true,
      applied_through: at(10),
    }),
  );

  cleanup();
  vi.mocked(usePageCoverage).mockClear();
  mountChat(<Chat channel="room-1" />, {
    answers: {
      chat_channels: rail,
      chat_channel: { ...served(30), channel, revision: 1, message_seq: 0, member: true },
      chat_messages: { ...served(40), channel_id: "room-1", messages: [] },
    },
  });
  await waitFor(() =>
    expect(published()).toContainEqual(expect.objectContaining({ applied_through: at(40) })),
  );
  expect(published()).not.toContainEqual(expect.objectContaining({ applied_through: at(10) }));
});
